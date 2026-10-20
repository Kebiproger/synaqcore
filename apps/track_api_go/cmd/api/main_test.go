package main

import (
	"reflect"
	"testing"

	"track_api_go/internal/kafka"
)

func TestKafkaConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "")
	t.Setenv("KAFKA_MAX_BUFFERED", "")
	t.Setenv("KAFKA_MAX_BUFFERED_BYTES", "")

	cfg := kafkaConfigFromEnv()
	if !reflect.DeepEqual(cfg.Brokers, []string{"localhost:9092"}) {
		t.Errorf("Brokers = %v", cfg.Brokers)
	}
	if cfg.MaxBuffered != kafka.DefaultMaxBuffered {
		t.Errorf("MaxBuffered = %d, want %d", cfg.MaxBuffered, kafka.DefaultMaxBuffered)
	}
	if cfg.MaxBufferedBytes != kafka.DefaultMaxBufferedBytes {
		t.Errorf("MaxBufferedBytes = %d, want %d", cfg.MaxBufferedBytes, kafka.DefaultMaxBufferedBytes)
	}
}

func TestKafkaConfigFromEnv_Overrides(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", " redpanda:9092, ,redpanda-2:9092 ")
	t.Setenv("KAFKA_MAX_BUFFERED", "500")
	t.Setenv("KAFKA_MAX_BUFFERED_BYTES", "1048576")

	cfg := kafkaConfigFromEnv()
	if !reflect.DeepEqual(cfg.Brokers, []string{"redpanda:9092", "redpanda-2:9092"}) {
		t.Errorf("Brokers = %v", cfg.Brokers)
	}
	if cfg.MaxBuffered != 500 {
		t.Errorf("MaxBuffered = %d, want 500", cfg.MaxBuffered)
	}
	if cfg.MaxBufferedBytes != 1<<20 {
		t.Errorf("MaxBufferedBytes = %d, want %d", cfg.MaxBufferedBytes, 1<<20)
	}
}
