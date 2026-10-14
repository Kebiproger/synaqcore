package kafka

import (
	"context"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)


type Producer interface{
	Publish(ctx context.Context, topic string, key, payload []byte) error // при onDone контроллер асинхронной функции поймет куда возвращаться
	PublishAsync(ctx context.Context, topic string, key ,payload []byte, onDone func(*kgo.Record, error))

	Close() error
}

type Config struct {
	Brokers []string
	ClientTimeOut time.Duration
}
type Client struct {
	client *kgo.Client
}

func NewProducer(cfg Config)(*Client, error){
	/* - Создается новый обьект Клиент (выделяется память в ОЗУ)
	 * - Обработка ошибки инициализации
	*  - Проверяется само подключение к брокеру сообщении
	*  - Обработка ошибки подключении
	 */
			// встроенный интерфейс для опции внутри Клиента
	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		// По умолчанию franz-go батчит сообщение для производительности
		kgo.ProducerBatchCompression(kgo.ZstdCompression(), kgo.GzipCompression()),
						
		kgo.ProducerLinger(5 * time.Millisecond),
		// на 5 милисекунд Продьюсер ждет дополнительных сооющении от нашего API,
		// чтобы не вызывать его каждый раз. Отправляем batch-ем
	}
	client, err := kgo.NewClient(opts...)
	if err != nil{
		return nil, fmt.Errorf("failed to initialize kafka client: %w", err)
	}

	pingCtx, cancelFunc := context.WithTimeout(context.Background(),cfg.ClientTimeOut)
	defer cancelFunc() // именно таймер выключается чтобы не мусорить память

					// Ping к самому брокеру, инициализация != брокер работает
	if err:= client.Ping(pingCtx); err != nil {
		client.Close()

		// до return выполняется defer cancelFunc
		return nil, fmt.Errorf("failed to ping kafka brokers: %w",err)
	}
	return &Client{client: client}, nil
}


func (c *Client) Publish(ctx context.Context, topic string, key, payload []byte) error {
	record := &kgo.Record{
		Topic: topic,
		Key: key,
		Value: payload,
	}

	if  err := c.client.ProduceSync(ctx, record).FirstErr() ;err != nil {
		return fmt.Errorf("ошибка отправки сообщения: %w", err)
	}

	return nil
}

func (c *Client) PublishAsync(ctx context.Context, topic string, key, payload []byte, onDone func(*kgo.Record,error)){
	record := &kgo.Record{
		Topic: topic,
		Key: key,
		Value: payload,
	}

	c.client.Produce(ctx, record, onDone)
}

func (c *Client) Close() error{
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
 						// Отправляем сообщении из буфера Kafk-и до выключение
	if err := c.client.Flush(ctx); err != nil{
		c.client.Close()
		return fmt.Errorf("failed to flush pending records: %w", err)
	}

	c.client.Close()
	return nil 
}
