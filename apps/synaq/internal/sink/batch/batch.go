// Package batch — общая механика батчевой записи событий для приёмников sink:
// ограниченная очередь (по числу событий и по байтам), отправка батча по размеру
// или по таймеру, повторы с экспоненциальной паузой и Close с ожиданием дозаписи.
//
// Конкретный приёмник (ClickHouse, PostgreSQL) передаёт только Writer — функцию,
// которая записывает один батч и сообщает, имеет ли смысл повторять запись.
package batch

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"synaq/internal/sink"
)

// Значения по умолчанию.
const (
	DefaultBufferSize = 10000
	// DefaultMaxBufferedBytes — лимит байт в очереди и записываемом батче, 64 МиБ.
	DefaultMaxBufferedBytes = 64 << 20
	DefaultBatchSize        = 1000
	DefaultFlushInterval    = time.Second
	DefaultMaxAttempts      = 5
	DefaultRetryBaseDelay   = 200 * time.Millisecond
	DefaultRetryMaxDelay    = 3 * time.Second
	DefaultCloseTimeout     = 5 * time.Second
)

// Writer записывает один батч сериализованных событий (в порядке поступления).
// ctx отменяется, если Close не уложился в CloseTimeout. retryable сообщает,
// имеет ли смысл повторить запись того же батча; при err == nil игнорируется.
// Writer вызывается из одного фонового горутина, параллельных вызовов нет.
type Writer func(ctx context.Context, payloads [][]byte) (retryable bool, err error)

// Config — настройки батчера. Нулевые значения заменяются значениями по умолчанию.
type Config struct {
	// Name — имя хранилища для логов и ошибок, например "ClickHouse". Данные событий
	// в логи не пишутся.
	Name string

	BufferSize       int           // ёмкость очереди событий
	MaxBufferedBytes int           // лимит байт в очереди и ещё не записанных батчах
	BatchSize        int           // максимальное число событий в одном батче
	FlushInterval    time.Duration // максимальное время ожидания неполного батча

	MaxAttempts    int           // число попыток записи батча (включая первую)
	RetryBaseDelay time.Duration // пауза перед второй попыткой, далее удваивается
	RetryMaxDelay  time.Duration // верхняя граница паузы между попытками
	CloseTimeout   time.Duration // сколько Close ждёт записи оставшихся событий
}

func (c *Config) setDefaults() {
	if c.Name == "" {
		c.Name = "хранилище"
	}
	if c.BufferSize <= 0 {
		c.BufferSize = DefaultBufferSize
	}
	if c.MaxBufferedBytes <= 0 {
		c.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if c.RetryMaxDelay < c.RetryBaseDelay {
		c.RetryMaxDelay = max(DefaultRetryMaxDelay, c.RetryBaseDelay)
	}
	if c.CloseTimeout <= 0 {
		c.CloseTimeout = DefaultCloseTimeout
	}
}

type item struct {
	payload []byte
	onDone  func(error)
}

// Batcher копит события и передаёт их Writer батчами из фонового горутина.
type Batcher struct {
	cfg   Config
	write Writer

	mu     sync.RWMutex // защищает closed и закрытие queue
	closed bool
	queue  chan item

	// bufferedBytes — размер событий, принятых Enqueue и ещё не записанных
	// (в очереди или в записываемом батче).
	bufferedBytes atomic.Int64

	// stopCtx отменяется, если Close не уложился в CloseTimeout: прерывает запись и повторы.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	done       chan struct{} // закрывается, когда фоновый горутин завершился
	closeOnce  sync.Once
	closeErr   error
}

// New создаёт батчер и запускает фоновую запись.
func New(cfg Config, write Writer) *Batcher {
	cfg.setDefaults()
	stopCtx, stopCancel := context.WithCancel(context.Background())
	b := &Batcher{
		cfg:        cfg,
		write:      write,
		queue:      make(chan item, cfg.BufferSize),
		stopCtx:    stopCtx,
		stopCancel: stopCancel,
		done:       make(chan struct{}),
	}
	go b.run()
	return b
}

// Enqueue ставит событие в очередь и никогда не блокируется: при заполненном
// буфере (по числу событий или по байтам) сразу возвращает sink.ErrBufferFull,
// после Close — sink.ErrClosed; в этих случаях onDone не вызывается. Иначе onDone
// (если не nil) вызывается ровно один раз — после записи батча или при окончательной ошибке.
func (b *Batcher) Enqueue(payload []byte, onDone func(error)) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return sink.ErrClosed
	}
	if !b.reserve(int64(len(payload))) {
		return sink.ErrBufferFull
	}
	select {
	case b.queue <- item{payload: payload, onDone: onDone}:
		return nil
	default:
		b.bufferedBytes.Add(-int64(len(payload)))
		return sink.ErrBufferFull
	}
}

// reserve учитывает size байт в лимите MaxBufferedBytes; false, если лимит исчерпан.
func (b *Batcher) reserve(size int64) bool {
	limit := int64(b.cfg.MaxBufferedBytes)
	for {
		cur := b.bufferedBytes.Load()
		if cur+size > limit {
			return false
		}
		if b.bufferedBytes.CompareAndSwap(cur, cur+size) {
			return true
		}
	}
}

// Close перестаёт принимать события, записывает оставшиеся и ждёт завершения
// не дольше CloseTimeout. Если время вышло, запись и повторы прерываются, а события,
// которые не удалось записать, получают ошибку в onDone. Повторный вызов безопасен.
func (b *Batcher) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		close(b.queue)
		b.mu.Unlock()

		timer := time.NewTimer(b.cfg.CloseTimeout)
		defer timer.Stop()
		select {
		case <-b.done:
		case <-timer.C:
			b.stopCancel()
			<-b.done
			b.closeErr = fmt.Errorf("не удалось отправить все события в %s за %v", b.cfg.Name, b.cfg.CloseTimeout)
		}
		b.stopCancel()
	})
	return b.closeErr
}

// run — фоновый цикл: копит батч и записывает его по размеру или по таймеру.
func (b *Batcher) run() {
	defer close(b.done)

	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]item, 0, b.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.flush(batch)
		batch = make([]item, 0, b.cfg.BatchSize)
	}

	for {
		select {
		case it, ok := <-b.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, it)
			if len(batch) >= b.cfg.BatchSize {
				flush()
				ticker.Reset(b.cfg.FlushInterval)
			}
		case <-ticker.C:
			flush()
		}
	}
}

// flush записывает батч с повторами и сообщает результат каждому событию.
func (b *Batcher) flush(batch []item) {
	payloads := make([][]byte, len(batch))
	var size int64
	for i, it := range batch {
		payloads[i] = it.payload
		size += int64(len(it.payload))
	}

	err := b.writeWithRetry(payloads)
	// Место в лимите освобождается после записи батча (успешной или окончательно
	// неудачной) и строго ДО вызова onDone: всё, что наблюдает завершение батча,
	// должно видеть уже свободный лимит, иначе Enqueue сразу после колбэка получит
	// ложный ErrBufferFull.
	b.bufferedBytes.Add(-size)
	if err != nil {
		log.Printf("Не удалось записать в %s батч из %d событий: %v", b.cfg.Name, len(batch), err)
	}
	for _, it := range batch {
		if it.onDone != nil {
			it.onDone(err)
		}
	}
}

func (b *Batcher) writeWithRetry(payloads [][]byte) error {
	delay := b.cfg.RetryBaseDelay
	var err error
	for attempt := 1; attempt <= b.cfg.MaxAttempts; attempt++ {
		var retryable bool
		retryable, err = b.write(b.stopCtx, payloads)
		if err == nil {
			return nil
		}
		if !retryable || attempt == b.cfg.MaxAttempts {
			break
		}
		log.Printf("Ошибка записи в %s (попытка %d из %d), повтор через %v: %v",
			b.cfg.Name, attempt, b.cfg.MaxAttempts, delay, err)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-b.stopCtx.Done():
			timer.Stop()
			return fmt.Errorf("отправка прервана при остановке: %w", err)
		}
		delay = min(delay*2, b.cfg.RetryMaxDelay)
	}
	return err
}
