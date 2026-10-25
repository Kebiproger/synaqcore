// Package sink описывает абстракцию приёмника событий трекинга.
// Реализации: PostgreSQL (internal/sink/postgres, по умолчанию) и прямая запись
// в ClickHouse по HTTP (internal/sink/clickhouse). Общая механика буфера, батчей
// и повторов — internal/sink/batch.
package sink

import (
	"context"
	"errors"
)

var (
	// ErrBufferFull возвращается PublishAsync, если внутренний буфер переполнен.
	// Вызывающий код должен ответить клиенту 503 и предложить повторить запрос.
	ErrBufferFull = errors.New("буфер событий переполнен")
	// ErrClosed возвращается при попытке отправить событие после Close.
	ErrClosed = errors.New("приёмник событий закрыт")
)

// Producer публикует сериализованные события в хранилище.
type Producer interface {
	// Publish синхронно отправляет сообщение и ждёт подтверждения доставки.
	Publish(ctx context.Context, topic string, key, payload []byte) error
	// PublishAsync ставит сообщение в очередь на отправку и не должен надолго блокироваться.
	// Ошибка возвращается, только если сообщение не принято в очередь (например,
	// ErrBufferFull или ErrClosed); в этом случае onDone не вызывается.
	// Если сообщение принято, onDone вызывается после доставки (err == nil)
	// или при окончательной ошибке. onDone может быть nil.
	PublishAsync(ctx context.Context, topic string, key, payload []byte, onDone func(err error)) error
	// Close отправляет накопленные сообщения и освобождает ресурсы.
	Close() error
}

// Pinger — необязательный интерфейс проверки доступности приёмника.
// Используется в /health через приведение типа.
type Pinger interface {
	Ping(ctx context.Context) error
}
