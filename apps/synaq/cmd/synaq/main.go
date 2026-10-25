package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"synaq/internal/api"
	"synaq/internal/auth"
	"synaq/internal/config"
	"synaq/internal/configcache"
	"synaq/internal/eventstore"
	"synaq/internal/sink"
	chsink "synaq/internal/sink/clickhouse"
	pgsink "synaq/internal/sink/postgres"
	"synaq/internal/store"
	"synaq/internal/track"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ginMode возвращает режим Gin из GIN_MODE (по умолчанию release).
func ginMode() string {
	switch mode := config.String("GIN_MODE", gin.ReleaseMode); mode {
	case gin.DebugMode, gin.ReleaseMode, gin.TestMode:
		return mode
	default:
		log.Printf("Неизвестное значение GIN_MODE=%q, используем release", mode)
		return gin.ReleaseMode
	}
}

// eventsTopic — имя топика, передаваемое в sink.Producer; приёмники событий его
// игнорируют и всегда пишут в свою таблицу tracking_events.
const eventsTopic = "tracking_events"

// Допустимые значения EVENT_SINK.
const (
	sinkPostgres   = "postgres"
	sinkClickHouse = "clickhouse"
)

// Границы EVENTS_RETENTION_DAYS и EVENTS_PARTITIONS_AHEAD.
const (
	defaultRetentionDays = 90
	minRetentionDays     = 8 // окно приёма событий (7 дней) + запас на опоздавшие батчи
	defaultPartitionsAhd = 3
	minPartitionsAhead   = 1
	maxPartitionsAhead   = 30
)

// eventSinkFromEnv возвращает режим приёмника событий из EVENT_SINK
// (postgres по умолчанию). Любое другое значение — ошибка.
func eventSinkFromEnv() (string, error) {
	switch mode := config.String("EVENT_SINK", sinkPostgres); mode {
	case sinkPostgres, sinkClickHouse:
		return mode, nil
	default:
		return "", fmt.Errorf("неподдерживаемое значение EVENT_SINK=%q: допустимо postgres или clickhouse", mode)
	}
}

// postgresEventsSettings — настройки PostgreSQL-приёмника событий и обслуживания секций.
type postgresEventsSettings struct {
	Sink          pgsink.Config
	MaxConns      int32
	RetentionDays int
	AheadDays     int
}

// intInRange читает целое из key и проверяет диапазон [lo, hi].
func intInRange(key string, def, lo, hi int) (int, error) {
	v, err := config.Int(key, def)
	if err != nil {
		return 0, err
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("некорректное значение %s=%d: допустимо от %d до %d", key, v, lo, hi)
	}
	return v, nil
}

// postgresEventsFromEnv собирает настройки PostgreSQL-приёмника из PG_EVENTS_MAX_CONNS и EVENTS_*.
func postgresEventsFromEnv() (postgresEventsSettings, error) {
	var s postgresEventsSettings
	var err error
	maxConns, err := intInRange("PG_EVENTS_MAX_CONNS", pgsink.DefaultMaxConns, 1, 100)
	if err != nil {
		return s, err
	}
	s.MaxConns = int32(maxConns)
	if s.Sink.BufferSize, err = config.Int("EVENTS_BUFFER_SIZE", pgsink.DefaultBufferSize); err != nil {
		return s, err
	}
	if s.Sink.MaxBufferedBytes, err = config.Int("EVENTS_MAX_BUFFERED_BYTES", pgsink.DefaultMaxBufferedBytes); err != nil {
		return s, err
	}
	if s.Sink.BatchSize, err = config.Int("EVENTS_BATCH_SIZE", pgsink.DefaultBatchSize); err != nil {
		return s, err
	}
	if s.Sink.FlushInterval, err = config.Duration("EVENTS_FLUSH_INTERVAL", pgsink.DefaultFlushInterval); err != nil {
		return s, err
	}
	if s.RetentionDays, err = intInRange("EVENTS_RETENTION_DAYS", defaultRetentionDays, minRetentionDays, math.MaxInt32); err != nil {
		return s, err
	}
	if s.AheadDays, err = intInRange("EVENTS_PARTITIONS_AHEAD", defaultPartitionsAhd, minPartitionsAhead, maxPartitionsAhead); err != nil {
		return s, err
	}
	return s, nil
}

// clickhouseConfigFromEnv собирает настройки ClickHouse-приёмника из переменных
// окружения CLICKHOUSE_URL/_USER/_PASSWORD и CH_*.
func clickhouseConfigFromEnv() (chsink.Config, error) {
	cfg := chsink.Config{
		URL:      config.String("CLICKHOUSE_URL", "http://clickhouse:8123"),
		User:     os.Getenv("CLICKHOUSE_USER"),
		Password: os.Getenv("CLICKHOUSE_PASSWORD"),
	}
	var err error
	if cfg.BufferSize, err = config.Int("CH_BUFFER_SIZE", chsink.DefaultBufferSize); err != nil {
		return cfg, err
	}
	if cfg.MaxBufferedBytes, err = config.Int("CH_MAX_BUFFERED_BYTES", chsink.DefaultMaxBufferedBytes); err != nil {
		return cfg, err
	}
	if cfg.BatchSize, err = config.Int("CH_BATCH_SIZE", chsink.DefaultBatchSize); err != nil {
		return cfg, err
	}
	if cfg.FlushInterval, err = config.Duration("CH_FLUSH_INTERVAL", chsink.DefaultFlushInterval); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// clickhousePingTimeout ограничивает проверку ClickHouse при старте.
const clickhousePingTimeout = 3 * time.Second

// pinger — часть sink.Producer, нужная для стартовой проверки (выделено для тестов).
type pinger interface {
	Ping(ctx context.Context) error
}

// checkClickHouse делает одну best-effort проверку ClickHouse при старте. Ошибка
// не фатальна: выдача вариантов и admin API от ClickHouse не зависят, приёмник сам
// повторяет запись и отвечает 503, когда буфер заполнен, а /health/track показывает
// состояние приёмника. Ожидание ограничено таймаутом и прерывается по ctx (SIGTERM).
func checkClickHouse(ctx context.Context, p pinger, timeout time.Duration) error {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return p.Ping(pingCtx)
}

// newClickHouseProducer создаёт продюсер lite-режима (прямая запись в ClickHouse).
// Недоступность ClickHouse при старте только логируется — сервис стартует.
func newClickHouseProducer(ctx context.Context) sink.Producer {
	cfg, err := clickhouseConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	producer, err := chsink.New(cfg)
	if err != nil {
		log.Fatalf("Ошибка настройки ClickHouse-приёмника: %v", err)
	}
	if err := checkClickHouse(ctx, producer, clickhousePingTimeout); err != nil {
		log.Printf("ВНИМАНИЕ: ClickHouse недоступен при старте, продолжаем без него "+
			"(события буферизуются и повторяются, при заполнении буфера — 503): %v", err)
	}
	log.Printf("Приёмник событий: ClickHouse (%s)", cfg.URL)
	return producer
}

// newPostgresProducer создаёт PostgreSQL-приёмник: отдельный пул событий, первичное
// создание секций (best-effort, дальше Maintainer повторяет раз в минуту) и фоновое
// обслуживание секций. Возвращает продюсера (Close закрывает пул событий) и функцию
// остановки Maintainer, которая отменяет его контекст и ждёт завершения.
func newPostgresProducer(ctx context.Context, dsn string, settings postgresEventsSettings) (sink.Producer, func()) {
	openCtx, cancel := context.WithTimeout(ctx, dbStartupTimeout)
	defer cancel()
	eventsPool, err := pgsink.OpenPool(openCtx, dsn, settings.MaxConns)
	if err != nil {
		log.Fatalf("Ошибка подключения к PostgreSQL для событий: %v", err)
	}

	m := eventstore.New(eventsPool, eventstore.Options{
		RetentionDays: settings.RetentionDays,
		AheadDays:     settings.AheadDays,
	})
	ensureCtx, cancelEnsure := context.WithTimeout(ctx, partitionsTimeout)
	if err := m.EnsureNow(ensureCtx); err != nil {
		log.Printf("ВНИМАНИЕ: не удалось подготовить секции событий при старте, продолжаем "+
			"(обслуживание повторится через минуту): %v", err)
	}
	cancelEnsure()

	settings.Sink.Ensure = m.EnsureNow
	producer, err := pgsink.New(eventsPool, settings.Sink)
	if err != nil {
		eventsPool.Close()
		log.Fatalf("Ошибка настройки PostgreSQL-приёмника: %v", err)
	}

	maintCtx, cancelMaint := context.WithCancel(context.Background())
	maintDone := make(chan struct{})
	go func() {
		defer close(maintDone)
		m.Run(maintCtx)
	}()
	stopMaintainer := func() {
		cancelMaint()
		<-maintDone
	}
	log.Printf("Приёмник событий: PostgreSQL (схема events, хранение %d дн., секций вперёд %d)",
		settings.RetentionDays, settings.AheadDays)
	return producer, stopMaintainer
}

// warnIfSuperuser предупреждает, если сервис подключён к PostgreSQL суперпользователем.
// Только предупреждение: ошибка запроса и роль-суперпользователь не фатальны.
func warnIfSuperuser(ctx context.Context, pool *pgxpool.Pool) {
	var super bool
	if err := pool.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		log.Printf("Не удалось проверить привилегии роли PostgreSQL: %v", err)
		return
	}
	if super {
		log.Println("ВНИМАНИЕ: synaq подключён к PostgreSQL суперпользователем; " +
			"запускайте сервис от роли с минимальными правами (synaq_app)")
	}
}

// Таймауты старта и остановки.
const (
	dbStartupTimeout  = 30 * time.Second
	partitionsTimeout = 10 * time.Second
	httpStopTimeout   = 5 * time.Second
)

func main() {
	sinkMode, err := eventSinkFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	// Настройки приёмника проверяются до подключения к БД: ошибка конфигурации — сразу fatal.
	var pgEvents postgresEventsSettings
	if sinkMode == sinkPostgres {
		if pgEvents, err = postgresEventsFromEnv(); err != nil {
			log.Fatal(err)
		}
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// PostgreSQL: DSN содержит пароль — не логируем ни его, ни конфигурацию подключения.
	dsn, err := store.DSNFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	startCtx, cancelStart := context.WithTimeout(shutdownCtx, dbStartupTimeout)
	pool, err := store.Open(startCtx, dsn)
	if err != nil {
		cancelStart()
		log.Fatalf("Ошибка подключения к PostgreSQL: %v", err)
	}
	warnIfSuperuser(startCtx, pool)
	// Миграции (включая схему events из 00003) применяются до открытия пула событий.
	if err := store.Migrate(startCtx, pool); err != nil {
		cancelStart()
		pool.Close()
		log.Fatalf("Ошибка миграций PostgreSQL: %v", err)
	}
	cancelStart()
	log.Println("PostgreSQL: подключение установлено, миграции применены")
	repo := store.NewRepository(pool)

	// Кэш конфигураций: первая загрузка обязательна, дальше — LISTEN/NOTIFY и периодическое перечитывание.
	cache := configcache.New(repo, configcache.Options{Listen: configcache.PgListener(dsn)})
	if err := cache.LoadInitial(shutdownCtx); err != nil {
		pool.Close()
		log.Fatalf("Ошибка загрузки конфигураций экспериментов: %v", err)
	}
	cacheCtx, cancelCache := context.WithCancel(context.Background())
	cacheDone := make(chan struct{})
	go func() {
		defer close(cacheDone)
		cache.Run(cacheCtx)
	}()

	verifier := auth.NewVerifier(auth.ConfigFromEnv())

	// Приёмник событий: пул событий и секции (postgres) либо ClickHouse.
	var (
		producer       sink.Producer
		stopMaintainer = func() {}
	)
	if sinkMode == sinkPostgres {
		producer, stopMaintainer = newPostgresProducer(shutdownCtx, dsn, pgEvents)
	} else {
		producer = newClickHouseProducer(shutdownCtx)
	}

	gin.SetMode(ginMode())
	router := api.NewRouter(api.Deps{
		Tracker: track.NewHandler(producer, eventsTopic),
		Repo:    repo,
		Cache:   cache,
		Auth:    verifier,
		Ready:   cache.Ready,
	})

	srv := &http.Server{
		Addr:              ":8000",
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		// Полные таймауты защищают от медленных клиентов (slowloris), держащих соединения и горутины.
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("HTTP-сервер запущен на %v", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Ошибка HTTP-сервера: %v", err)
		}
	}()

	// Graceful shutdown: ждём SIGINT/SIGTERM.
	<-shutdownCtx.Done()
	log.Println("Получен сигнал завершения работы, останавливаем сервис...")

	// Порядок: HTTP → приёмник событий (для postgres Close закрывает и пул событий) →
	// обслуживание секций → кэш конфигураций → пул PostgreSQL.
	stopHTTPCtx, cancelHTTP := context.WithTimeout(context.Background(), httpStopTimeout)
	defer cancelHTTP()
	if err := srv.Shutdown(stopHTTPCtx); err != nil {
		log.Printf("Ошибка при остановке HTTP-сервера: %v", err)
	}

	// Продюсер закрывается после остановки HTTP-сервера, чтобы не потерять принятые события.
	if err := producer.Close(); err != nil {
		log.Printf("Ошибка при закрытии приёмника событий: %v", err)
	} else {
		log.Println("Приёмник событий успешно остановлен")
	}

	stopMaintainer()
	cancelCache()
	<-cacheDone
	pool.Close()

	log.Println("Сервис успешно выключен")
}
