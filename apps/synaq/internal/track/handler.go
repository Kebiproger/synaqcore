package track

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"synaq/internal/sink"

	"github.com/gin-gonic/gin"
)

// Лимиты входных данных.
const (
	MaxBodyBytes      = 64 << 10 // максимальный размер тела запроса, 64 КБ
	MaxFieldLen       = 256      // максимальная длина строковых полей, в символах
	MaxEventDataBytes = 16 << 10 // максимальный размер event_data, 16 КБ

	// Допустимое окно timestamp относительно серверного времени. Без ограничения
	// анонимный клиент может прислать события за сотни месяцев: одна вставка задевает
	// больше max_partitions_per_insert_block партиций (PARTITION BY toYYYYMM),
	// вставка батча отклоняется целиком и вместе с ним теряются чужие события.
	MaxEventAge   = 7 * 24 * time.Hour // насколько событие может быть в прошлом
	MaxFutureSkew = 5 * time.Minute    // допуск на расхождение часов клиента
)

// TrackEvent — формат события. JSON-теги задают формат строки, записываемой в ClickHouse,
// менять их нельзя без синхронного изменения ClickHouse-таблиц, MV и SDK.
type TrackEvent struct {
	EventID        string `json:"event_id"`
	UserID         string `json:"user_id" binding:"required"`
	EventName      string `json:"event_name" binding:"required"`
	Timestamp      int64  `json:"timestamp"` // миллисекунды Unix
	ExperimentName string `json:"experiment_name"`
	// ExperimentID надо в дальнейшем добавить для оптимизации запроса БД и сети.
	Variant   string          `json:"variant"`
	EventData json.RawMessage `json:"event_data"`
}

// Validate проверяет размеры полей и timestamp события. now — текущее серверное время.
// Timestamp == 0 допустим: сервер подставит своё время.
func (e *TrackEvent) Validate(now time.Time) error {
	fields := []struct {
		name  string
		value string
	}{
		{"event_id", e.EventID},
		{"user_id", e.UserID},
		{"event_name", e.EventName},
		{"experiment_name", e.ExperimentName},
		{"variant", e.Variant},
	}
	for _, f := range fields {
		if utf8.RuneCountInString(f.value) > MaxFieldLen {
			return fmt.Errorf("поле %s длиннее %d символов", f.name, MaxFieldLen)
		}
	}
	if len(e.EventData) > MaxEventDataBytes {
		return fmt.Errorf("поле event_data больше %d байт", MaxEventDataBytes)
	}
	if e.Timestamp != 0 {
		minTS := now.Add(-MaxEventAge).UnixMilli()
		maxTS := now.Add(MaxFutureSkew).UnixMilli()
		if e.Timestamp < minTS || e.Timestamp > maxTS {
			return errors.New("поле timestamp вне допустимого окна: ожидаются миллисекунды Unix " +
				"не старше 7 дней и не более чем на 5 минут в будущем относительно времени сервера")
		}
	}
	return nil
}

// newUUIDv4 возвращает случайный UUID версии 4 (RFC 9562) в каноническом текстовом виде.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 9562
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

const (
	deliveryLogInterval = time.Second
	healthPingTimeout   = 2 * time.Second
)

type Handler struct {
	producer sink.Producer
	topic    string
	// now — источник текущего времени; подменяется в тестах.
	now func() time.Time

	logMu      sync.Mutex
	lastLog    time.Time
	suppressed int
}

func NewHandler(producer sink.Producer, topic string) *Handler {
	return &Handler{producer: producer, topic: topic, now: time.Now}
}

// WithClock подменяет источник времени (для тестов) и возвращает тот же обработчик.
func (h *Handler) WithClock(now func() time.Time) *Handler {
	h.now = now
	return h
}

func (h *Handler) HandleTrack(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)

	var event TrackEvent
	if err := c.ShouldBindJSON(&event); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "тело запроса слишком большое"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "неправильный ввод"})
		return
	}

	now := h.now()
	if err := event.Validate(now); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if event.Timestamp == 0 {
		event.Timestamp = now.UnixMilli()
	}

	// event_id входит в ORDER BY ReplacingMergeTree (event_name, user_id, event_id, toDate(timestamp)).
	// С пустым event_id все одноимённые события пользователя за день схлопнулись бы в одно
	// (например, две покупки), поэтому без идентификатора от клиента сервер генерирует UUIDv4.
	if event.EventID == "" {
		id, err := newUUIDv4()
		if err != nil {
			log.Printf("Не удалось сгенерировать event_id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "внутренняя ошибка сервера"})
			return
		}
		event.EventID = id
	}

	payload, err := json.Marshal(event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ошибка сериализации события"})
		return
	}

	err = h.producer.PublishAsync(context.Background(), h.topic, []byte(event.UserID), payload, h.onDelivery)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"status": "OK"})
	case errors.Is(err, sink.ErrBufferFull):
		// Приёмник не успевает: просим клиента повторить запрос позже.
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "очередь событий переполнена, повторите запрос позже"})
	default:
		log.Printf("Не удалось поставить событие в очередь: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "сервис временно недоступен"})
	}
}

// onDelivery логирует ошибки доставки. При недоступном приёмнике ошибка приходит
// на каждое событие батча, поэтому в лог пишется не больше одной строки в секунду
// с числом пропущенных ошибок.
func (h *Handler) onDelivery(err error) {
	if err == nil {
		return
	}
	h.logMu.Lock()
	defer h.logMu.Unlock()
	now := time.Now()
	if now.Sub(h.lastLog) < deliveryLogInterval {
		h.suppressed++
		return
	}
	if h.suppressed > 0 {
		log.Printf("Ошибка доставки события: %v (ещё %d ошибок с прошлой записи в лог)", err, h.suppressed)
	} else {
		log.Printf("Ошибка доставки события: %v", err)
	}
	h.lastLog = now
	h.suppressed = 0
}

// HandleHealth отвечает 200, если приёмник событий доступен. Приёмники без Ping
// (не реализующие sink.Pinger) считаются доступными.
func (h *Handler) HandleHealth(c *gin.Context) {
	if pinger, ok := h.producer.(sink.Pinger); ok {
		ctx, cancel := context.WithTimeout(c.Request.Context(), healthPingTimeout)
		defer cancel()
		if err := pinger.Ping(ctx); err != nil {
			log.Printf("Проверка здоровья: приёмник событий недоступен: %v", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "OK"})
}
