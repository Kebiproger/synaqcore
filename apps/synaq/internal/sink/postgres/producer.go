// Package postgres реализует sink.Producer с записью событий в PostgreSQL
// (партиционированная таблица events.tracking_events, миграция 00003_events.sql).
//
// События копятся в ограниченном буфере (пакет internal/sink/batch) и пишутся батчами
// одним INSERT ... SELECT FROM unnest(...) ON CONFLICT DO NOTHING: дубликаты по
// первичному ключу (event_id, user_id, event_name, event_date) отбрасываются,
// сохраняется первая строка. Пул соединений отдельный от пула экспериментов, чтобы
// запись событий не отнимала соединения у выдачи вариантов.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"synaq/internal/sink"
	"synaq/internal/sink/batch"
)

// Проверка на этапе компиляции: Producer реализует sink.Producer и sink.Pinger.
var (
	_ sink.Producer = (*Producer)(nil)
	_ sink.Pinger   = (*Producer)(nil)
)

// insertQuery фиксирован; все значения передаются параметрами-массивами.
// event_date вычисляется из ts в UTC и совпадает с CHECK tracking_events_day_chk.
const insertQuery = `INSERT INTO events.tracking_events (event_date, ts, event_id, user_id, event_name, experiment_name, variant, event_data)
SELECT (t.ts AT TIME ZONE 'UTC')::date, t.ts, t.event_id, t.user_id, t.event_name, t.experiment_name, t.variant, t.event_data::jsonb
FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
     AS t(ts, event_id, user_id, event_name, experiment_name, variant, event_data)
ON CONFLICT DO NOTHING`

// Значения по умолчанию (контракт переменных EVENTS_* и PG_EVENTS_MAX_CONNS).
const (
	DefaultMaxConns         = 2                // PG_EVENTS_MAX_CONNS
	DefaultBufferSize       = 10000            // EVENTS_BUFFER_SIZE
	DefaultMaxBufferedBytes = 32 << 20         // EVENTS_MAX_BUFFERED_BYTES, 32 МиБ
	DefaultBatchSize        = 500              // EVENTS_BATCH_SIZE
	DefaultFlushInterval    = time.Second      // EVENTS_FLUSH_INTERVAL
	DefaultWriteTimeout     = 15 * time.Second // больше statement_timeout пула (10 с)
)

// Config — настройки продюсера. Нулевые значения заменяются значениями по умолчанию.
type Config struct {
	BufferSize       int           // ёмкость очереди событий (EVENTS_BUFFER_SIZE)
	MaxBufferedBytes int           // лимит байт в очереди и незаписанных батчах (EVENTS_MAX_BUFFERED_BYTES)
	BatchSize        int           // максимум событий в одном INSERT (EVENTS_BATCH_SIZE)
	FlushInterval    time.Duration // максимальное ожидание неполного батча (EVENTS_FLUSH_INTERVAL)

	// WriteTimeout ограничивает одну вставку батча (или всю построчную вставку),
	// включая ожидание соединения из пула.
	WriteTimeout   time.Duration
	MaxAttempts    int           // число попыток записи батча (включая первую)
	RetryBaseDelay time.Duration // пауза перед второй попыткой, далее удваивается
	RetryMaxDelay  time.Duration // верхняя граница паузы между попытками
	CloseTimeout   time.Duration // сколько Close ждёт записи оставшихся событий

	// Ensure создаёт недостающие партиции (eventstore.Maintainer.EnsureNow).
	// Вызывается не больше одного раза на попытку, если INSERT вернул 23514
	// (нет партиции для строки). Любая ошибка Ensure (в том числе
	// eventstore.ErrLockBusy — обслуживание занято другой репликой) повторяема:
	// батч повторяется целиком. nil — партиции не создаются.
	Ensure func(ctx context.Context) error
}

func (c *Config) setDefaults() {
	if c.BufferSize <= 0 {
		c.BufferSize = DefaultBufferSize
	}
	if c.MaxBufferedBytes <= 0 {
		c.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
}

// OpenPool создаёт отдельный пул для записи событий: application_name=synaq-events,
// statement_timeout=10s, synchronous_commit=off (при сбое сервера можно потерять
// последние доли секунды событий, зато коммит не ждёт fsync). maxConns <= 0 —
// DefaultMaxConns. Доступность проверяется Ping. DSN содержит пароль — не логировать.
func OpenPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx маскирует пароль в ошибках разбора DSN.
		return nil, fmt.Errorf("пул PostgreSQL для событий: %w", err)
	}
	if maxConns <= 0 {
		maxConns = DefaultMaxConns
	}
	cfg.MaxConns = maxConns
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "synaq-events"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "10s"
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("пул PostgreSQL для событий: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL (события) недоступен: %w", err)
	}
	return pool, nil
}

// execer — часть *pgxpool.Pool, нужная для вставки событий.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Producer пишет события в PostgreSQL батчами. Владеет пулом: Close закрывает его.
type Producer struct {
	cfg     Config
	pool    *pgxpool.Pool
	exec    execer // вставка батча; в New — pool, в тестах подменяется
	batcher *batch.Batcher
}

// New создаёт продюсер поверх пула (обычно из OpenPool) и запускает фоновую запись.
func New(pool *pgxpool.Pool, cfg Config) (*Producer, error) {
	if pool == nil {
		return nil, errors.New("не задан пул PostgreSQL для событий")
	}
	cfg.setDefaults()
	p := &Producer{cfg: cfg, pool: pool, exec: pool}
	p.batcher = batch.New(batch.Config{
		Name:             "PostgreSQL",
		BufferSize:       cfg.BufferSize,
		MaxBufferedBytes: cfg.MaxBufferedBytes,
		BatchSize:        cfg.BatchSize,
		FlushInterval:    cfg.FlushInterval,
		MaxAttempts:      cfg.MaxAttempts,
		RetryBaseDelay:   cfg.RetryBaseDelay,
		RetryMaxDelay:    cfg.RetryMaxDelay,
		CloseTimeout:     cfg.CloseTimeout,
	}, p.write)
	return p, nil
}

// PublishAsync ставит событие в очередь и никогда не блокируется:
// при заполненном буфере сразу возвращает sink.ErrBufferFull.
// Аргументы ctx, topic и key не используются; payload — json.Marshal(track.TrackEvent).
func (p *Producer) PublishAsync(_ context.Context, _ string, _, payload []byte, onDone func(error)) error {
	return p.batcher.Enqueue(payload, onDone)
}

// Publish ставит событие в очередь и ждёт результата записи его батча.
func (p *Producer) Publish(ctx context.Context, topic string, key, payload []byte) error {
	result := make(chan error, 1)
	if err := p.PublishAsync(ctx, topic, key, payload, func(err error) { result <- err }); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ping проверяет доступность PostgreSQL через пул событий.
func (p *Producer) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("PostgreSQL (события) недоступен: %w", err)
	}
	return nil
}

// Close дописывает накопленные события (не дольше CloseTimeout) и закрывает пул.
// Повторный вызов безопасен.
func (p *Producer) Close() error {
	err := p.batcher.Close()
	p.pool.Close()
	return err
}

// event — строка батча; поля и JSON-теги совпадают с track.TrackEvent.
type event struct {
	EventID        string          `json:"event_id"`
	UserID         string          `json:"user_id"`
	EventName      string          `json:"event_name"`
	Timestamp      int64           `json:"timestamp"` // миллисекунды Unix
	ExperimentName string          `json:"experiment_name"`
	Variant        string          `json:"variant"`
	EventData      json.RawMessage `json:"event_data"`
}

// row — событие, подготовленное к вставке.
type row struct {
	ts                                                  time.Time
	eventID, userID, eventName, experimentName, variant string
	eventData                                           *string // nil — SQL NULL
}

// Допустимый диапазон timestamp, мс: 1970-01-01 .. 9999-12-31 UTC.
const (
	minTimestampMs = 0
	maxTimestampMs = 253402300799999
)

// decodeRows разбирает payload'ы; нераспознанные пропускаются (возвращается их число).
func decodeRows(payloads [][]byte) (rows []row, bad int) {
	rows = make([]row, 0, len(payloads))
	for _, pl := range payloads {
		var ev event
		// Время вне диапазона timestamptz не закодировать (трекер и так ограничивает его окном).
		if err := json.Unmarshal(pl, &ev); err != nil || ev.Timestamp < minTimestampMs || ev.Timestamp > maxTimestampMs {
			bad++
			continue
		}
		r := row{
			ts:             time.UnixMilli(ev.Timestamp).UTC(),
			eventID:        stripNUL(ev.EventID),
			userID:         stripNUL(ev.UserID),
			eventName:      stripNUL(ev.EventName),
			experimentName: stripNUL(ev.ExperimentName),
			variant:        stripNUL(ev.Variant),
		}
		if data, ok := normalizeEventData(ev.EventData); ok {
			r.eventData = &data
		}
		rows = append(rows, r)
	}
	return rows, bad
}

// write — batch.Writer. Порядок: вставка батча; при 23514 (нет партиции) — один вызов
// Ensure и повтор (ошибка Ensure, в т.ч. занятый lock, — повтор батча; батч
// отбрасывается, только если после успешного Ensure партиции всё ещё нет); при
// ошибке данных (классы 22/23) — построчная вставка, в которой отбрасываются
// только плохие строки. Данные событий в логи и ошибки не попадают.
func (p *Producer) write(ctx context.Context, payloads [][]byte) (bool, error) {
	rows, bad := decodeRows(payloads)
	if bad > 0 {
		log.Printf("PostgreSQL: отброшено %d событий с нераспознанным форматом", bad)
	}
	if len(rows) == 0 {
		return false, nil
	}

	err := p.insert(ctx, rows)
	if err == nil {
		return false, nil
	}
	kind := classify(ctx, err)
	if kind == errNoPartition && p.cfg.Ensure != nil {
		if eerr := p.cfg.Ensure(ctx); eerr != nil {
			return ctx.Err() == nil, fmt.Errorf("создание партиций событий: %w", eerr)
		}
		if err = p.insert(ctx, rows); err == nil {
			return false, nil
		}
		kind = classify(ctx, err)
	}

	switch kind {
	case errRetryable:
		return true, safeError(err)
	case errData, errNoPartition:
		return p.insertRowByRow(ctx, rows)
	default:
		return false, safeError(err)
	}
}

// insert выполняет один INSERT для всех строк.
func (p *Producer) insert(ctx context.Context, rows []row) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WriteTimeout)
	defer cancel()
	_, err := p.exec.Exec(ctx, insertQuery, columns(rows)...)
	return err
}

// insertRowByRow вставляет строки по одной: строки с ошибкой данных отбрасываются
// (в лог — только их число). Повторяемая ошибка прерывает вставку, и батч повторяется
// целиком: уже вставленные строки отсеет ON CONFLICT DO NOTHING.
func (p *Producer) insertRowByRow(ctx context.Context, rows []row) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.WriteTimeout)
	defer cancel()
	dropped := 0
	for i := range rows {
		_, err := p.exec.Exec(ctx, insertQuery, columns(rows[i:i+1])...)
		if err == nil {
			continue
		}
		switch classify(ctx, err) {
		case errData, errNoPartition:
			dropped++
		case errRetryable:
			return true, safeError(err)
		default:
			return false, safeError(err)
		}
	}
	if dropped > 0 {
		log.Printf("PostgreSQL: отброшено %d из %d событий батча с некорректными данными", dropped, len(rows))
	}
	return false, nil
}

// columns раскладывает строки в параметры-массивы $1..$7 запроса insertQuery.
func columns(rows []row) []any {
	ts := make([]time.Time, len(rows))
	ids := make([]string, len(rows))
	users := make([]string, len(rows))
	names := make([]string, len(rows))
	exps := make([]string, len(rows))
	variants := make([]string, len(rows))
	data := make([]*string, len(rows))
	for i, r := range rows {
		ts[i], ids[i], users[i], names[i] = r.ts, r.eventID, r.userID, r.eventName
		exps[i], variants[i], data[i] = r.experimentName, r.variant, r.eventData
	}
	return []any{ts, ids, users, names, exps, variants, data}
}

type errKind int

const (
	errFatal       errKind = iota // повтор не поможет (права, схема, отмена при остановке)
	errRetryable                  // соединение, сериализация, ресурсы, перезапуск сервера
	errData                       // ошибка данных конкретной строки (классы 22, 23)
	errNoPartition                // 23514: нет партиции для строки
)

// classify относит ошибку к одной из категорий. ctx — контекст остановки батчера:
// если он отменён, повторять бессмысленно.
func classify(ctx context.Context, err error) errKind {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return errFatal
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		// Сетевые ошибки, обрыв соединения, таймаут ожидания пула или WriteTimeout.
		return errRetryable
	}
	code := pgErr.Code
	switch {
	case code == "23514":
		return errNoPartition
	case strings.HasPrefix(code, "08"), // connection exception
		strings.HasPrefix(code, "40"),   // transaction rollback (serialization, deadlock)
		strings.HasPrefix(code, "53"),   // insufficient resources
		strings.HasPrefix(code, "57P0"), // admin/crash shutdown, cannot connect now
		code == "57014":                 // statement_timeout: батч повторяется, а не теряется
		return errRetryable
	case strings.HasPrefix(code, "22"), strings.HasPrefix(code, "23"):
		return errData
	default:
		return errFatal
	}
}

// safeError убирает из ошибки PostgreSQL текст, который может цитировать данные
// событий (Detail, а для классов 22/23 и Message): остаются SQLSTATE и, для прочих
// классов, сообщение сервера.
func safeError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23") {
		return fmt.Errorf("PostgreSQL вернул ошибку данных (SQLSTATE %s)", pgErr.Code)
	}
	return fmt.Errorf("PostgreSQL: %s (SQLSTATE %s)", pgErr.Message, pgErr.Code)
}
