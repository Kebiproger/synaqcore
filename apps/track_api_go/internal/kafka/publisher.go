package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"track_api_go/internal/sink"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Проверка на этапе компиляции: Client реализует sink.Producer и sink.Pinger.
var (
	_ sink.Producer = (*Client)(nil)
	_ sink.Pinger   = (*Client)(nil)
)

// DefaultMaxBuffered — лимит сообщений в буфере продюсера по умолчанию.
const DefaultMaxBuffered = 10000

// DefaultMaxBufferedBytes — лимит байт (ключ + значение) в буфере продюсера по умолчанию, 64 МиБ.
// Одного лимита по числу сообщений мало: при событиях по ~17 КБ 10000 сообщений — это ~170 МБ.
const DefaultMaxBufferedBytes = 64 << 20

// kgoBufferHeadroom — запас лимита franz-go сверх нашего лимита допуска. franz-go вызывает
// promise записи до того, как уменьшит свой счётчик буфера, поэтому наш счётчик освобождается
// чуть раньше. Запас гарантирует, что TryProduce не откажет с ErrMaxBuffered уже после того,
// как PublishAsync принял сообщение.
const kgoBufferHeadroom = 1024

// kgoBufferBytesHeadroom — такой же запас для лимита байт franz-go.
const kgoBufferBytesHeadroom = 8 << 20

type Config struct {
	Brokers       []string
	ClientTimeOut time.Duration
	// MaxBuffered — сколько сообщений может ждать отправки в брокер. Сверх лимита
	// PublishAsync сразу возвращает sink.ErrBufferFull. 0 — DefaultMaxBuffered.
	MaxBuffered int
	// MaxBufferedBytes — сколько байт (ключ + значение) может ждать отправки. Сверх лимита
	// PublishAsync сразу возвращает sink.ErrBufferFull. 0 — DefaultMaxBufferedBytes.
	MaxBufferedBytes int
}

type Client struct {
	client *kgo.Client

	maxBuffered      int64
	maxBufferedBytes int64

	// mu защищает счётчики буфера: проверка двух лимитов должна быть атомарной.
	mu            sync.Mutex
	buffered      int64 // принятые, но ещё не доставленные (или не отклонённые) сообщения
	bufferedBytes int64 // их суммарный размер (ключ + значение)

	closed atomic.Bool
}

// NewProducer создаёт клиента Kafka и проверяет доступность брокеров через Ping:
// успешная инициализация клиента ещё не означает, что брокер работает.
func NewProducer(cfg Config) (*Client, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.ClientTimeOut)
	defer cancel()

	if err := c.client.Ping(pingCtx); err != nil {
		c.client.Close()
		return nil, fmt.Errorf("failed to ping kafka brokers: %w", err)
	}
	return c, nil
}

// newClient создаёт клиента без проверки брокеров (используется в тестах).
func newClient(cfg Config) (*Client, error) {
	maxBuffered := cfg.MaxBuffered
	if maxBuffered <= 0 {
		maxBuffered = DefaultMaxBuffered
	}
	maxBufferedBytes := cfg.MaxBufferedBytes
	if maxBufferedBytes <= 0 {
		maxBufferedBytes = DefaultMaxBufferedBytes
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ProducerBatchCompression(kgo.ZstdCompression(), kgo.GzipCompression()),
		// Продюсер ждёт 5 мс дополнительных сообщений и отправляет их одним батчем.
		kgo.ProducerLinger(5 * time.Millisecond),
		// Явный лимит буфера: при недоступном брокере память и число ожидающих
		// сообщений ограничены, а не растут до OOM.
		kgo.MaxBufferedRecords(maxBuffered + kgoBufferHeadroom),
		kgo.MaxBufferedBytes(maxBufferedBytes + kgoBufferBytesHeadroom),
	}
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize kafka client: %w", err)
	}
	return &Client{
		client:           client,
		maxBuffered:      int64(maxBuffered),
		maxBufferedBytes: int64(maxBufferedBytes),
	}, nil
}

func (c *Client) Publish(ctx context.Context, topic string, key, payload []byte) error {
	record := &kgo.Record{
		Topic: topic,
		Key:   key,
		Value: payload,
	}

	if err := c.client.ProduceSync(ctx, record).FirstErr(); err != nil {
		return fmt.Errorf("ошибка отправки сообщения: %w", err)
	}

	return nil
}

// PublishAsync передаёт запись в буфер franz-go и никогда не блокируется.
// Если буфер заполнен (брокер недоступен или не успевает), сразу возвращает
// sink.ErrBufferFull; после Close — sink.ErrClosed. Ошибки доставки принятых
// сообщений приходят в onDone.
//
// franz-go сообщает об ErrMaxBuffered только асинхронно (через promise), поэтому
// лимит проверяется собственным счётчиком до вызова TryProduce.
func (c *Client) PublishAsync(ctx context.Context, topic string, key, payload []byte, onDone func(err error)) error {
	if c.closed.Load() {
		return sink.ErrClosed
	}
	size := int64(len(key) + len(payload))
	if !c.reserve(size) {
		return sink.ErrBufferFull
	}

	record := &kgo.Record{
		Topic: topic,
		Key:   key,
		Value: payload,
	}
	c.client.TryProduce(ctx, record, func(_ *kgo.Record, err error) {
		c.release(size)
		if onDone == nil {
			return
		}
		switch {
		case errors.Is(err, kgo.ErrMaxBuffered):
			err = fmt.Errorf("%w: %w", sink.ErrBufferFull, err)
		case errors.Is(err, kgo.ErrClientClosed):
			err = fmt.Errorf("%w: %w", sink.ErrClosed, err)
		}
		onDone(err)
	})
	return nil
}

// reserve занимает место в буфере под одно сообщение размером size байт;
// false, если исчерпан лимит сообщений или байт.
func (c *Client) reserve(size int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buffered >= c.maxBuffered || c.bufferedBytes+size > c.maxBufferedBytes {
		return false
	}
	c.buffered++
	c.bufferedBytes += size
	return true
}

// release освобождает место, занятое reserve.
func (c *Client) release(size int64) {
	c.mu.Lock()
	c.buffered--
	c.bufferedBytes -= size
	c.mu.Unlock()
}

// bufferUsage возвращает текущее число сообщений и байт в буфере.
func (c *Client) bufferUsage() (records, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffered, c.bufferedBytes
}

// Ping проверяет доступность хотя бы одного брокера.
func (c *Client) Ping(ctx context.Context) error {
	return c.client.Ping(ctx)
}

func (c *Client) Close() error {
	c.closed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Отправляем сообщения из буфера Kafka до выключения.
	if err := c.client.Flush(ctx); err != nil {
		c.client.Close()
		return fmt.Errorf("failed to flush pending records: %w", err)
	}

	c.client.Close()
	return nil
}
