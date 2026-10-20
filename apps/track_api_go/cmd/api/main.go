package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"track_api_go/internal/kafka"
	"track_api_go/internal/sink"
	chsink "track_api_go/internal/sink/clickhouse"
	"track_api_go/internal/track"

	"github.com/gin-gonic/gin"
)

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// ginMode возвращает режим Gin из GIN_MODE (по умолчанию release).
func ginMode() string {
	switch mode := getEnv("GIN_MODE", gin.ReleaseMode); mode {
	case gin.DebugMode, gin.ReleaseMode, gin.TestMode:
		return mode
	default:
		log.Printf("Неизвестное значение GIN_MODE=%q, используем release", mode)
		return gin.ReleaseMode
	}
}

// envInt читает положительное целое из переменной окружения; некорректное значение — фатальная ошибка.
func envInt(key string, defaultValue int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		log.Fatalf("Некорректное значение %s=%q: ожидается положительное целое число", key, raw)
	}
	return v
}

// envDuration читает положительную длительность (например, 500ms, 1s); некорректное значение — фатальная ошибка.
func envDuration(key string, defaultValue time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		log.Fatalf("Некорректное значение %s=%q: ожидается положительная длительность, например 1s", key, raw)
	}
	return v
}

// kafkaConfigFromEnv собирает настройки Kafka-продюсера из переменных окружения:
// KAFKA_BROKERS (через запятую), KAFKA_MAX_BUFFERED (сообщений) и KAFKA_MAX_BUFFERED_BYTES.
func kafkaConfigFromEnv() kafka.Config {
	rawBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")

	var brokers []string
	for _, b := range strings.Split(rawBrokers, ",") {
		if trimmed := strings.TrimSpace(b); trimmed != "" {
			brokers = append(brokers, trimmed)
		}
	}

	return kafka.Config{
		Brokers:          brokers,
		ClientTimeOut:    3 * time.Second,
		MaxBuffered:      envInt("KAFKA_MAX_BUFFERED", kafka.DefaultMaxBuffered),
		MaxBufferedBytes: envInt("KAFKA_MAX_BUFFERED_BYTES", kafka.DefaultMaxBufferedBytes),
	}
}

// newKafkaProducer создаёт продюсер для полного режима (Kafka/Redpanda).
func newKafkaProducer() sink.Producer {
	cfg := kafkaConfigFromEnv()
	producer, err := kafka.NewProducer(cfg)
	if err != nil {
		log.Fatalf("Ошибка запуска Kafka Producer: %v", err)
	}
	log.Printf("Приёмник событий: Kafka (%s), буфер до %d сообщений / %d байт",
		strings.Join(cfg.Brokers, ","), cfg.MaxBuffered, cfg.MaxBufferedBytes)
	return producer
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
	chURL := getEnv("CLICKHOUSE_URL", "http://clickhouse:8123")
	producer, err := chsink.New(chsink.Config{
		URL:              chURL,
		User:             os.Getenv("CLICKHOUSE_USER"),
		Password:         os.Getenv("CLICKHOUSE_PASSWORD"),
		BufferSize:       envInt("CH_BUFFER_SIZE", chsink.DefaultBufferSize),
		MaxBufferedBytes: envInt("CH_MAX_BUFFERED_BYTES", chsink.DefaultMaxBufferedBytes),
		BatchSize:        envInt("CH_BATCH_SIZE", chsink.DefaultBatchSize),
		FlushInterval:    envDuration("CH_FLUSH_INTERVAL", chsink.DefaultFlushInterval),
	})
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
	log.Printf("Приёмник событий: ClickHouse (%s)", chURL)
	return producer
}

func main() {
	topic := getEnv("TOPIC", "tracking_events")

	var producer sink.Producer
	switch mode := getEnv("EVENT_SINK", "kafka"); mode {
	case "kafka":
		producer = newKafkaProducer()
	case "clickhouse":
		producer = newClickHouseProducer()
	default:
		log.Fatalf("Неизвестное значение EVENT_SINK=%q: допустимы kafka или clickhouse", mode)
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gin.SetMode(ginMode())
	router := gin.New()
	router.Use(gin.Recovery())
	if gin.Mode() != gin.ReleaseMode {
		router.Use(gin.Logger())
	}

	trackHandler := track.NewHandler(producer, topic)
	router.GET("/health", trackHandler.HandleHealth)
	api := router.Group("api/v1/tracker")
	api.POST("/events", trackHandler.HandleTrack)

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

	stopHTTPCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
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

	log.Println("Сервис успешно выключен")
}
