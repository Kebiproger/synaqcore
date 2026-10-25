package main

import (
	"context"
	"errors"
	"testing"
	"time"

	chsink "synaq/internal/sink/clickhouse"
)

func TestEventSinkFromEnv(t *testing.T) {
	cases := map[string]struct {
		val     string
		want    string
		wantErr bool
	}{
		"default":    {"", "postgres", false},
		"postgres":   {"postgres", "postgres", false},
		"clickhouse": {"clickhouse", "clickhouse", false},
		"kafka":      {"kafka", "", true},
		"case":       {"ClickHouse", "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("EVENT_SINK", c.val)
			got, err := eventSinkFromEnv()
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("got (%q, %v), want (%q, err=%v)", got, err, c.want, c.wantErr)
			}
		})
	}
}

var eventsEnvKeys = []string{
	"PG_EVENTS_MAX_CONNS", "EVENTS_BUFFER_SIZE", "EVENTS_MAX_BUFFERED_BYTES", "EVENTS_BATCH_SIZE",
	"EVENTS_FLUSH_INTERVAL", "EVENTS_RETENTION_DAYS", "EVENTS_PARTITIONS_AHEAD",
}

func clearEventsEnv(t *testing.T) {
	t.Helper()
	for _, k := range eventsEnvKeys {
		t.Setenv(k, "")
	}
}

func TestPostgresEventsFromEnv_Defaults(t *testing.T) {
	clearEventsEnv(t)
	s, err := postgresEventsFromEnv()
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if s.MaxConns != 2 || s.Sink.BufferSize != 10000 || s.Sink.MaxBufferedBytes != 32<<20 ||
		s.Sink.BatchSize != 500 || s.Sink.FlushInterval != time.Second ||
		s.RetentionDays != 90 || s.AheadDays != 3 {
		t.Errorf("значения по умолчанию не применены: %+v", s)
	}
}

func TestPostgresEventsFromEnv_Overrides(t *testing.T) {
	clearEventsEnv(t)
	t.Setenv("PG_EVENTS_MAX_CONNS", "4")
	t.Setenv("EVENTS_FLUSH_INTERVAL", "250ms")
	t.Setenv("EVENTS_RETENTION_DAYS", "8")
	t.Setenv("EVENTS_PARTITIONS_AHEAD", "30")
	s, err := postgresEventsFromEnv()
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if s.MaxConns != 4 || s.Sink.FlushInterval != 250*time.Millisecond || s.RetentionDays != 8 || s.AheadDays != 30 {
		t.Errorf("переопределения не применены: %+v", s)
	}
}

func TestPostgresEventsFromEnv_Invalid(t *testing.T) {
	cases := map[string][2]string{
		"conns zero":      {"PG_EVENTS_MAX_CONNS", "0"},
		"buffer":          {"EVENTS_BUFFER_SIZE", "-1"},
		"bytes":           {"EVENTS_MAX_BUFFERED_BYTES", "x"},
		"batch":           {"EVENTS_BATCH_SIZE", "0"},
		"interval":        {"EVENTS_FLUSH_INTERVAL", "10"},
		"retention < 8":   {"EVENTS_RETENTION_DAYS", "7"},
		"retention text":  {"EVENTS_RETENTION_DAYS", "abc"},
		"ahead zero":      {"EVENTS_PARTITIONS_AHEAD", "0"},
		"ahead too large": {"EVENTS_PARTITIONS_AHEAD", "31"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			clearEventsEnv(t)
			t.Setenv(c[0], c[1])
			if _, err := postgresEventsFromEnv(); err == nil {
				t.Error("ожидалась ошибка")
			}
		})
	}
}

func setCHEnv(t *testing.T, url, buf, bytes, batch, flush string) {
	t.Helper()
	t.Setenv("CLICKHOUSE_URL", url)
	t.Setenv("CH_BUFFER_SIZE", buf)
	t.Setenv("CH_MAX_BUFFERED_BYTES", bytes)
	t.Setenv("CH_BATCH_SIZE", batch)
	t.Setenv("CH_FLUSH_INTERVAL", flush)
}

func TestClickhouseConfigFromEnv_Defaults(t *testing.T) {
	setCHEnv(t, "", "", "", "", "")
	cfg, err := clickhouseConfigFromEnv()
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.URL != "http://clickhouse:8123" {
		t.Errorf("URL = %q", cfg.URL)
	}
	if cfg.BufferSize != chsink.DefaultBufferSize || cfg.MaxBufferedBytes != chsink.DefaultMaxBufferedBytes ||
		cfg.BatchSize != chsink.DefaultBatchSize || cfg.FlushInterval != chsink.DefaultFlushInterval {
		t.Errorf("значения по умолчанию не применены: %+v", cfg)
	}
}

func TestClickhouseConfigFromEnv_Overrides(t *testing.T) {
	setCHEnv(t, "http://ch:8123", "500", "1048576", "100", "250ms")
	cfg, err := clickhouseConfigFromEnv()
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.URL != "http://ch:8123" || cfg.BufferSize != 500 || cfg.MaxBufferedBytes != 1<<20 ||
		cfg.BatchSize != 100 || cfg.FlushInterval != 250*time.Millisecond {
		t.Errorf("переопределения не применены: %+v", cfg)
	}
}

func TestClickhouseConfigFromEnv_Invalid(t *testing.T) {
	cases := map[string][5]string{
		"buffer":   {"", "0", "", "", ""},
		"bytes":    {"", "", "-5", "", ""},
		"batch":    {"", "", "", "x", ""},
		"interval": {"", "", "", "", "10"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			setCHEnv(t, c[0], c[1], c[2], c[3], c[4])
			if _, err := clickhouseConfigFromEnv(); err == nil {
				t.Error("ожидалась ошибка")
			}
		})
	}
}

// blockingPinger ждёт отмены контекста, как зависший ClickHouse.
type blockingPinger struct{}

func (blockingPinger) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type errPinger struct{ err error }

func (p errPinger) Ping(context.Context) error { return p.err }

func TestCheckClickHouse_ErrorIsReturnedNotFatal(t *testing.T) {
	want := errors.New("connection refused")
	if err := checkClickHouse(context.Background(), errPinger{want}, time.Second); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if err := checkClickHouse(context.Background(), errPinger{}, time.Second); err != nil {
		t.Fatalf("доступный ClickHouse: err = %v", err)
	}
}

func TestCheckClickHouse_Timeout(t *testing.T) {
	start := time.Now()
	err := checkClickHouse(context.Background(), blockingPinger{}, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("проверка заняла %v, таймаут не соблюдён", d)
	}
}

func TestCheckClickHouse_ShutdownInterrupts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGTERM пришёл во время старта
	start := time.Now()
	err := checkClickHouse(ctx, blockingPinger{}, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("проверка заняла %v, отмена контекста не учтена", d)
	}
}
