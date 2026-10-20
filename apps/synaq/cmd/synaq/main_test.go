package main

import (
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
