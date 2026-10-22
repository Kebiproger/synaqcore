package main

import (
	"context"
	"errors"
	"testing"
	"time"

	chsink "synaq/internal/sink/clickhouse"
)

func TestCheckEventSink(t *testing.T) {
	for _, mode := range []string{"", "clickhouse"} {
		t.Setenv("EVENT_SINK", mode)
		if err := checkEventSink(); err != nil {
			t.Errorf("EVENT_SINK=%q: неожиданная ошибка %v", mode, err)
		}
	}
	for _, mode := range []string{"kafka", "ClickHouse", "other"} {
		t.Setenv("EVENT_SINK", mode)
		if err := checkEventSink(); err == nil {
			t.Errorf("EVENT_SINK=%q: ожидалась ошибка", mode)
		}
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
