package kafka

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"track_api_go/internal/sink"
)

// Брокер на закрытом порту: сообщения копятся в буфере и не уходят, что имитирует
// недоступность Kafka без сети.
const unreachableBroker = "127.0.0.1:1"

func TestPublishAsync_BufferLimit(t *testing.T) {
	const limit = 3
	c, err := newClient(Config{Brokers: []string{unreachableBroker}, MaxBuffered: limit})
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		results []error
		wg      sync.WaitGroup
	)
	onDone := func(err error) {
		mu.Lock()
		results = append(results, err)
		mu.Unlock()
		wg.Done()
	}

	for i := 0; i < limit; i++ {
		wg.Add(1)
		if err := c.PublishAsync(context.Background(), "t", []byte("k"), []byte("v"), onDone); err != nil {
			t.Fatalf("сообщение %d: PublishAsync = %v, want nil", i, err)
		}
	}

	// Сверх лимита — сразу ErrBufferFull, без блокировки и без вызова onDone.
	done := make(chan error, 1)
	go func() {
		done <- c.PublishAsync(context.Background(), "t", []byte("k"), []byte("v"), func(error) {
			t.Error("onDone не должен вызываться для отклонённого сообщения")
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, sink.ErrBufferFull) {
			t.Fatalf("PublishAsync сверх лимита = %v, want ErrBufferFull", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PublishAsync заблокировался при заполненном буфере")
	}

	// После Close новые сообщения отклоняются с ErrClosed. Закрываем клиента без Flush,
	// чтобы не ждать недоступный брокер: franz-go завершит буферизованные записи ошибкой.
	c.closed.Store(true)
	if err := c.PublishAsync(context.Background(), "t", nil, nil, nil); !errors.Is(err, sink.ErrClosed) {
		t.Fatalf("PublishAsync после закрытия = %v, want ErrClosed", err)
	}
	c.client.Close()

	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone не вызван для буферизованных сообщений после закрытия клиента")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != limit {
		t.Fatalf("onDone вызван %d раз, want %d", len(results), limit)
	}
	for _, err := range results {
		if err == nil {
			t.Error("ожидалась ошибка доставки, получено nil")
		}
	}
	if records, bytes := c.bufferUsage(); records != 0 || bytes != 0 {
		t.Errorf("буфер = %d сообщений / %d байт после завершения всех записей, want 0", records, bytes)
	}
}

func TestNewClient_DefaultMaxBuffered(t *testing.T) {
	c, err := newClient(Config{Brokers: []string{unreachableBroker}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.client.Close()
	if c.maxBuffered != DefaultMaxBuffered {
		t.Errorf("maxBuffered = %d, want %d", c.maxBuffered, DefaultMaxBuffered)
	}
	if c.maxBufferedBytes != DefaultMaxBufferedBytes {
		t.Errorf("maxBufferedBytes = %d, want %d", c.maxBufferedBytes, DefaultMaxBufferedBytes)
	}
}

func TestPublishAsync_ByteLimit(t *testing.T) {
	// Лимит в 10 байт: два сообщения по 4 байта (ключ 1 + значение 3) помещаются, третье — нет.
	c, err := newClient(Config{Brokers: []string{unreachableBroker}, MaxBuffered: 100, MaxBufferedBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	onDone := func(error) { wg.Done() }
	for i := 0; i < 2; i++ {
		wg.Add(1)
		if err := c.PublishAsync(context.Background(), "t", []byte("k"), []byte("abc"), onDone); err != nil {
			t.Fatalf("сообщение %d: PublishAsync = %v, want nil", i, err)
		}
	}
	err = c.PublishAsync(context.Background(), "t", []byte("k"), []byte("abc"), func(error) {
		t.Error("onDone не должен вызываться для отклонённого сообщения")
	})
	if !errors.Is(err, sink.ErrBufferFull) {
		t.Fatalf("PublishAsync сверх лимита байт = %v, want ErrBufferFull", err)
	}
	if _, bytes := c.bufferUsage(); bytes != 8 {
		t.Errorf("байт в буфере = %d, want 8", bytes)
	}

	c.closed.Store(true)
	c.client.Close()
	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone не вызван для буферизованных сообщений после закрытия клиента")
	}
	if records, bytes := c.bufferUsage(); records != 0 || bytes != 0 {
		t.Errorf("буфер = %d сообщений / %d байт после закрытия, want 0", records, bytes)
	}
}
