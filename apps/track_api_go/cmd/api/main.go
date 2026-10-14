package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"track_api_go/internal/kafka"
	"track_api_go/internal/track"

	"github.com/gin-gonic/gin"
)

func getEnv(key , defaultValue string) string{
	if value := os.Getenv(key); value != ""{
		return value
	}
	return defaultValue
}

func main(){
	rawBrokers := getEnv("KAFKA_BROKERS","localhost:9092")
	TopicABEvents := getEnv("TOPIC","tracking_events")
	
	var brokers []string

	for _, b := range strings.Split(rawBrokers,","){
		if trimmed := strings.TrimSpace(b); trimmed!=""{
			brokers = append(brokers, trimmed)
		}
	}
	
	cfg := kafka.Config{
		Brokers: brokers,
		ClientTimeOut: 3* time.Second,
	}
	producer, err := kafka.NewProducer(cfg)
	if err != nil {
		log.Fatalf("Ошибка запуска Kafka Producer: %v", err)
	}
	
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	
	router := gin.Default()
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "OK"})
	})
	api := router.Group("api/v1/tracker")
	                // применяем функцию моего track package? Который возвращает структуру
	trackHandler := track.NewHandler(producer, TopicABEvents)

									// Используем метод этой структуры?
	api.POST("/events",trackHandler.HandleTrack)

	srv := &http.Server{
		Addr: ":8000",
		Handler: router,
	}

	go func(){
		log.Printf("HTTP сервер запущен на %v", srv.Addr)
		if err:= srv.ListenAndServe(); err != nil &&  !errors.Is(err, http.ErrServerClosed){
			log.Fatalf("Ошибка HTTP сервера: %v", err)
		}
	}()
	<-shutdownCtx.Done() // Вызов graceful shutdown, вызывается тогда когда получает сигнал SIGTERM
	log.Println("Получен сигнал завершения работы, останавливаем сервис...")

	stopHttpCtx, cancelHttp := context.WithTimeout(context.Background(), 5*time.Second)
	
	defer cancelHttp()
	if err := srv.Shutdown(stopHttpCtx); err != nil{
		log.Printf("Ошибка при остановке HTTP-сервера: %v", err)
	}
	
	if err := producer.Close(); err != nil {
		log.Printf("Ошибка при закрытии Kafka продюсера: %v", err)
	} else {
		log.Println("Kafka продюсер успешно остановлен")
	}
	
	log.Println("Сервис успешно выключен")
}