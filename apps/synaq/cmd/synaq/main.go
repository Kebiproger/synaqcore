package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"synaq/internal/api"
	"synaq/internal/auth"
	"synaq/internal/config"
	"synaq/internal/configcache"
	"synaq/internal/sink"
	chsink "synaq/internal/sink/clickhouse"
	"synaq/internal/store"
	"synaq/internal/track"

	"github.com/gin-gonic/gin"
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

// eventsTopic — имя топика, передаваемое в sink.Producer; ClickHouse-приёмник его
// игнорирует и всегда пишет в таблицу tracking_events.
const eventsTopic = "tracking_events"

// checkEventSink допускает только EVENT_SINK=clickhouse (или пустое значение).
func checkEventSink() error {
	if mode := config.String("EVENT_SINK", "clickhouse"); mode != "clickhouse" {
		return fmt.Errorf("неподдерживаемое значение EVENT_SINK=%q: допустимо только clickhouse", mode)
	}
	return nil
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

// Параметры проверки ClickHouse при старте.
const (
	clickhouseStartupAttempts = 5
	clickhouseStartupDelay    = 2 * time.Second
	clickhousePingTimeout     = 3 * time.Second
)

// newClickHouseProducer создаёт продюсер lite-режима (прямая запись в ClickHouse)
// и ждёт доступности ClickHouse; если он так и не ответил — фатальная ошибка.
func newClickHouseProducer() sink.Producer {
	cfg, err := clickhouseConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	producer, err := chsink.New(cfg)
	if err != nil {
		log.Fatalf("Ошибка настройки ClickHouse-приёмника: %v", err)
	}

	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), clickhousePingTimeout)
		err = producer.Ping(ctx)
		cancel()
		if err == nil {
			break
		}
		if attempt == clickhouseStartupAttempts {
			_ = producer.Close()
			log.Fatalf("ClickHouse недоступен после %d попыток: %v", attempt, err)
		}
		log.Printf("ClickHouse недоступен (попытка %d из %d), повтор через %v: %v",
			attempt, clickhouseStartupAttempts, clickhouseStartupDelay, err)
		time.Sleep(clickhouseStartupDelay)
	}
	log.Printf("Приёмник событий: ClickHouse (%s)", cfg.URL)
	return producer
}

// Таймауты старта и остановки.
const (
	dbStartupTimeout = 30 * time.Second
	httpStopTimeout  = 5 * time.Second
)

func main() {
	if err := checkEventSink(); err != nil {
		log.Fatal(err)
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
	producer := newClickHouseProducer()

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

	// Порядок: HTTP → приёмник событий → кэш конфигураций → пул PostgreSQL.
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

	cancelCache()
	<-cacheDone
	pool.Close()

	log.Println("Сервис успешно выключен")
}
