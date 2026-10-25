package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"synaq/internal/sink/postgres"
)

// schemaDDL — копия таблицы из миграции 00003_events.sql (в тесте создаётся напрямую).
const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS events;
CREATE TABLE events.tracking_events (
    event_date      date        NOT NULL,
    ts              timestamptz NOT NULL,
    event_id        text        NOT NULL,
    user_id         text        NOT NULL,
    event_name      text        NOT NULL,
    experiment_name text        NOT NULL DEFAULT '',
    variant         text        NOT NULL DEFAULT '',
    event_data      jsonb,
    CONSTRAINT tracking_events_pk PRIMARY KEY (event_id, user_id, event_name, event_date),
    CONSTRAINT tracking_events_day_chk CHECK (event_date = (ts AT TIME ZONE 'UTC')::date)
) PARTITION BY RANGE (event_date);`

var dbSeq atomic.Int64

// newTestDB создаёт пустую базу (как в internal/store) и возвращает её DSN.
func newTestDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("SYNAQ_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SYNAQ_TEST_DATABASE_URL не задан — интеграционный тест пропущен")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("подключение к тестовому PostgreSQL: %v", err)
	}
	defer admin.Close(ctx)

	name := fmt.Sprintf("synaq_events_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("SYNAQ_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func createPartition(ctx context.Context, pool *pgxpool.Pool, day time.Time) error {
	day = day.UTC().Truncate(24 * time.Hour)
	name := pgx.Identifier{"events", "tracking_events_p" + day.Format("20060102")}.Sanitize()
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF events.tracking_events FOR VALUES FROM ('%s') TO ('%s')",
		name, day.Format(time.DateOnly), day.AddDate(0, 0, 1).Format(time.DateOnly)))
	return err
}

type setup struct {
	pool     *pgxpool.Pool // отдельный пул для проверок
	producer *postgres.Producer
	ensures  atomic.Int32
}

// newSetup: база со схемой; партиция только для today. ensure создаёт партицию для extraDay.
func newSetup(t *testing.T, today, extraDay time.Time, flushInterval ...time.Duration) *setup {
	t.Helper()
	ctx := context.Background()
	dsn := newTestDB(t)
	check, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(check.Close)
	if _, err := check.Exec(ctx, schemaDDL); err != nil {
		t.Fatalf("DDL: %v", err)
	}
	if err := createPartition(ctx, check, today); err != nil {
		t.Fatalf("партиция: %v", err)
	}

	pool, err := postgres.OpenPool(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	s := &setup{pool: check}
	interval := time.Hour // батч уходит только при Close
	if len(flushInterval) > 0 {
		interval = flushInterval[0]
	}
	s.producer, err = postgres.New(pool, postgres.Config{
		BatchSize:      100,
		FlushInterval:  interval,
		MaxAttempts:    3,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  2 * time.Millisecond,
		CloseTimeout:   5 * time.Second,
		Ensure: func(ctx context.Context) error {
			s.ensures.Add(1)
			return createPartition(ctx, pool, extraDay)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.producer.Close() })
	return s
}

func ev(t *testing.T, id, name string, ts time.Time, data string) []byte {
	t.Helper()
	m := map[string]any{
		"event_id": id, "user_id": "user_test_1", "event_name": name,
		"timestamp": ts.UnixMilli(), "experiment_name": "exp", "variant": "B",
	}
	if data != "" {
		m["event_data"] = json.RawMessage(data)
	} else {
		m["event_data"] = nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// publishAll ставит события в очередь, закрывает продюсер (дозапись) и возвращает ошибки onDone.
func publishAll(t *testing.T, p *postgres.Producer, payloads ...[]byte) []error {
	t.Helper()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var errs []error
	for _, pl := range payloads {
		wg.Add(1)
		if err := p.PublishAsync(context.Background(), "", nil, pl, func(err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
			wg.Done()
		}); err != nil {
			t.Fatalf("PublishAsync: %v", err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	return errs
}

func count(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM events.tracking_events WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func noErrors(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		if err != nil {
			t.Errorf("onDone: %v", err)
		}
	}
}

func TestIntegrationDedupAndNormalisation(t *testing.T) {
	now := time.Now().UTC()
	s := newSetup(t, now, now)

	noErrors(t, publishAll(t, s.producer,
		ev(t, "e1", "purchase", now, `{"amount":10}`),
		ev(t, "e1", "purchase", now.Add(time.Millisecond), `{"amount":99}`), // дубликат в том же батче
		ev(t, "e1", "view", now, ""),                                        // другое событие — не дубликат
		ev(t, "e2", "purchase", now, `"{\"amount\":5}"`),                    // JSON в строке
		ev(t, "e3", "purchase", now, `"a\u0000b"`),                          // NUL
	))

	if n := count(t, s.pool, "true"); n != 4 {
		t.Fatalf("строк %d, want 4", n)
	}
	var amount string
	if err := s.pool.QueryRow(context.Background(),
		"SELECT event_data->>'amount' FROM events.tracking_events WHERE event_id='e1' AND event_name='purchase'").Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != "10" {
		t.Errorf("сохранена не первая строка: amount=%s", amount)
	}
	if n := count(t, s.pool, "event_id='e1' AND event_name='view' AND event_data IS NULL"); n != 1 {
		t.Errorf("event_data null должен стать SQL NULL")
	}
	if n := count(t, s.pool, "event_id='e2' AND (event_data->>'amount')::int = 5"); n != 1 {
		t.Errorf("JSON в строке не развёрнут")
	}
	if n := count(t, s.pool, "event_id='e3' AND event_data = '\"ab\"'::jsonb"); n != 1 {
		t.Errorf("NUL не удалён из event_data")
	}
	if n := count(t, s.pool, "event_date = (ts AT TIME ZONE 'UTC')::date"); n != 4 {
		t.Errorf("event_date не совпадает с UTC-днём ts")
	}
}

func TestIntegrationDedupAcrossBatches(t *testing.T) {
	now := time.Now().UTC()
	s := newSetup(t, now, now, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 2 {
		if err := s.producer.Publish(ctx, "", nil, ev(t, "dup", "purchase", now, `{"a":1}`)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	_ = s.producer.Close()
	if n := count(t, s.pool, "event_id='dup'"); n != 1 {
		t.Errorf("строк %d, want 1", n)
	}
}

func TestIntegrationBadRowIsolation(t *testing.T) {
	now := time.Now().UTC()
	s := newSetup(t, now, now)

	errs := publishAll(t, s.producer,
		ev(t, "ok1", "purchase", now, `{"amount":1}`),
		ev(t, "bad", "purchase", now, `{"n":1e1000000}`), // jsonb: numeric overflow (класс 22)
		ev(t, "ok2", "purchase", now, `{"amount":2}`),
	)
	noErrors(t, errs)
	if n := count(t, s.pool, "true"); n != 2 {
		t.Fatalf("строк %d, want 2 (отброшена только плохая)", n)
	}
	if n := count(t, s.pool, "event_id='bad'"); n != 0 {
		t.Errorf("плохая строка записана")
	}
}

func TestIntegrationMissingPartitionEnsure(t *testing.T) {
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)
	s := newSetup(t, now, yesterday) // партиции на вчера нет, её создаст ensure

	noErrors(t, publishAll(t, s.producer,
		ev(t, "today", "purchase", now, ""),
		ev(t, "yesterday", "purchase", yesterday, ""),
	))
	if n := s.ensures.Load(); n != 1 {
		t.Errorf("ensure вызван %d раз, want 1", n)
	}
	if n := count(t, s.pool, "true"); n != 2 {
		t.Errorf("строк %d, want 2 после создания партиции", n)
	}
}

func TestIntegrationPing(t *testing.T) {
	now := time.Now().UTC()
	s := newSetup(t, now, now)
	if err := s.producer.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
	var app, timeout, syncCommit string
	// Параметры сессии проверяются в отдельном пуле, открытом OpenPool по тому же DSN.
	_ = s.producer.Close()
	pool, err := postgres.OpenPool(context.Background(), s.pool.Config().ConnString(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('application_name'), current_setting('statement_timeout'), current_setting('synchronous_commit')").
		Scan(&app, &timeout, &syncCommit); err != nil {
		t.Fatal(err)
	}
	if app != "synaq-events" || timeout != "10s" || syncCommit != "off" {
		t.Errorf("параметры сессии: %s, %s, %s", app, timeout, syncCommit)
	}
}
