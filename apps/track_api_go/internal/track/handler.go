package track

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"track_api_go/internal/kafka"

	"github.com/gin-gonic/gin"
	"github.com/twmb/franz-go/pkg/kgo"
)

type TrackEvent struct{
	UserId string `json:"user_id" binding:"required"`
	EventName string `json:"event_name" binding:"required"`
    Timestamp    int64  `json:"timestamp"`     
	ExperimentName string `json:"experiment_name"`
	// ExperimentId надо в дальнейшем добавить для оптимизации запроса БД и сети
	Variant string `json:"variant"`
	EventData json.RawMessage `json:"event_data"`
}

type Handler struct{
	producer kafka.Producer
	topic string
}
				// почему указатель
func NewHandler(producer kafka.Producer, topic string) *Handler{
	return &Handler{producer: producer, topic: topic,} 
}

func (h *Handler) HandleTrack(c *gin.Context){
	var event TrackEvent
	// Сперва обработки ошибки + по памяти
	if err:= c.ShouldBindJSON(&event); err != nil{
		c.JSON(400, gin.H{"error":"неправильный ввод"})
		return // почему пустой return
	}

	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}
	payload, err := json.Marshal(event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error":"ошибка сериализации event"})
		return
	}

	h.producer.PublishAsync(context.Background(), h.topic,[]byte(event.UserId), payload , func(r *kgo.Record, err error){
	if err != nil{
		log.Printf("Ошибка доставки сообщении в Kafka %v", err)
	}
	})
	
	// Если нет ошибок то:
	log.Printf("Обработана действие:%s Пользователем:%s",event.EventName, event.UserId)
	c.JSON(200, gin.H{"status":"OK"})
}