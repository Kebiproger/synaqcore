package batch_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"synaq/internal/sink"
	"synaq/internal/sink/batch"
)

const waitTimeout = 5 * time.Second

// fastConfig — быстрые повторы; таймер практически отключён, чтобы батчи
// формировались только по размеру или при Close.
func fastConfig() batch.Config {
	return batch.Config{
		Name:           "тест",
		BufferSize:     100,
		BatchSize:      100,
		FlushInterval:  time.Hour,
		MaxAttempts:    3,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  2 * time.Millisecond,
		CloseTimeout:   waitTimeout,
	}
}

// counter считает вызовы onDone по каждому событию.
type counter struct {
	mu    sync.Mutex
	calls map[int]int
	errs  map[int]error
	wg    sync.WaitGroup
}

func newCounter() *counter { return &counter{calls: map[int]int{}, errs: map[int]error{}} }

func (c *counter) cb(i int) func(error) {
	c.wg.Add(1)
	return func(err error) {
		c.mu.Lock()
		c.calls[i]++
		c.errs[i] = err
		first := c.calls[i] == 1
		c.mu.Unlock()
		if first {
			c.wg.Done()
		}
	}
}

func (c *counter) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("onDone вызван не для всех событий")
	}
}

func TestFullByCount(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	cfg := fastConfig()
	cfg.BatchSize = 1
	cfg.BufferSize = 2
	b := batch.New(cfg, func(ctx context.Context, _ [][]byte) (bool, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return false, nil
	})
	defer b.Close()

	c := newCounter()
	// Первое событие забирает фоновый горутин и зависает в Writer.
	if err := b.Enqueue([]byte("a"), c.cb(0)); err != nil {
		t.Fatal(err)
	}
	<-started
	for i := 1; i <= 2; i++ {
		if err := b.Enqueue([]byte("b"), c.cb(i)); err != nil {
			t.Fatalf("Enqueue #%d: %v", i, err)
		}
	}
	if err := b.Enqueue([]byte("c"), func(error) { t.Error("onDone для отклонённого события") }); !errors.Is(err, sink.ErrBufferFull) {
		t.Fatalf("Enqueue = %v, want sink.ErrBufferFull", err)
	}
	close(release)
	c.wait(t)
}

func TestFullByBytes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	cfg := fastConfig()
	cfg.BatchSize = 1
	cfg.MaxBufferedBytes = 10
	b := batch.New(cfg, func(ctx context.Context, _ [][]byte) (bool, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return false, nil
	})
	defer b.Close()

	c := newCounter()
	// Записываемый батч продолжает занимать лимит байт до конца записи.
	if err := b.Enqueue([]byte("12345"), c.cb(0)); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := b.Enqueue([]byte("12345"), c.cb(1)); err != nil {
		t.Fatal(err)
	}
	if err := b.Enqueue([]byte("1"), nil); !errors.Is(err, sink.ErrBufferFull) {
		t.Fatalf("Enqueue сверх лимита байт = %v, want sink.ErrBufferFull", err)
	}
	close(release)
	c.wait(t)
	// После записи место освобождается.
	c2 := newCounter()
	if err := b.Enqueue([]byte("1234567890"), c2.cb(0)); err != nil {
		t.Fatalf("Enqueue после освобождения: %v", err)
	}
	c2.wait(t)
}

func TestCloseFlushesPending(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	b := batch.New(fastConfig(), func(_ context.Context, p [][]byte) (bool, error) {
		mu.Lock()
		got = append(got, p...)
		mu.Unlock()
		return false, nil
	})
	c := newCounter()
	for i := range 3 {
		if err := b.Enqueue([]byte{byte('a' + i)}, c.cb(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.wait(t)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || string(got[0]) != "a" || string(got[2]) != "c" {
		t.Fatalf("записано %q, want [a b c] по порядку", got)
	}
	if err := b.Enqueue([]byte("x"), nil); !errors.Is(err, sink.ErrClosed) {
		t.Errorf("Enqueue после Close = %v, want sink.ErrClosed", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("повторный Close: %v", err)
	}
}

func TestRetryGivesUpAndOnDoneOnce(t *testing.T) {
	var attempts atomic.Int32
	wantErr := errors.New("временная ошибка")
	cfg := fastConfig()
	cfg.BatchSize = 2
	cfg.MaxAttempts = 4
	b := batch.New(cfg, func(context.Context, [][]byte) (bool, error) {
		attempts.Add(1)
		return true, wantErr
	})
	c := newCounter()
	for i := range 2 {
		if err := b.Enqueue([]byte("x"), c.cb(i)); err != nil {
			t.Fatal(err)
		}
	}
	c.wait(t)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := attempts.Load(); n != 4 {
		t.Errorf("попыток %d, want 4 (MaxAttempts)", n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range 2 {
		if c.calls[i] != 1 {
			t.Errorf("onDone #%d вызван %d раз, want 1", i, c.calls[i])
		}
		if !errors.Is(c.errs[i], wantErr) {
			t.Errorf("onDone #%d: %v, want %v", i, c.errs[i], wantErr)
		}
	}
}

func TestNonRetryableNotRepeated(t *testing.T) {
	var attempts atomic.Int32
	cfg := fastConfig()
	cfg.BatchSize = 1
	b := batch.New(cfg, func(context.Context, [][]byte) (bool, error) {
		attempts.Add(1)
		return false, errors.New("ошибка данных")
	})
	c := newCounter()
	if err := b.Enqueue([]byte("x"), c.cb(0)); err != nil {
		t.Fatal(err)
	}
	c.wait(t)
	_ = b.Close()
	if n := attempts.Load(); n != 1 {
		t.Errorf("попыток %d, want 1", n)
	}
}

func TestCloseDeadlineCancelsWriter(t *testing.T) {
	cfg := fastConfig()
	cfg.CloseTimeout = 50 * time.Millisecond
	b := batch.New(cfg, func(ctx context.Context, _ [][]byte) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	c := newCounter()
	if err := b.Enqueue([]byte("x"), c.cb(0)); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err == nil {
		t.Error("Close: nil, want ошибку по таймауту")
	}
	c.wait(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errs[0] == nil || c.calls[0] != 1 {
		t.Errorf("onDone: %v (вызовов %d), want одну ошибку", c.errs[0], c.calls[0])
	}
}

// Резервирование должно освобождаться до вызова onDone: Enqueue прямо из колбэка
// не должен видеть переполненный буфер (успех, окончательная ошибка и Close).
func TestReleaseBeforeOnDone(t *testing.T) {
	cases := []struct {
		name     string
		writeErr error
		viaClose bool
	}{
		{"успех", nil, false},
		{"окончательная ошибка", errors.New("запись не удалась"), false},
		{"Close", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fastConfig()
			cfg.MaxBufferedBytes = 10
			if !tc.viaClose {
				cfg.BatchSize = 1
			}
			b := batch.New(cfg, func(context.Context, [][]byte) (bool, error) {
				return false, tc.writeErr
			})
			defer b.Close()

			res := make(chan error, 1)
			onDone := func(error) {
				// Весь лимит байт должен быть уже свободен.
				res <- b.Enqueue([]byte("1234567890"), nil)
			}
			if err := b.Enqueue([]byte("1234567890"), onDone); err != nil {
				t.Fatal(err)
			}
			if tc.viaClose {
				// Close закрывает приём, поэтому Enqueue из колбэка вернёт ErrClosed —
				// для этого случая проверяем лишь порядок: лимит уже свободен (не ErrBufferFull).
				go b.Close()
			}
			select {
			case err := <-res:
				if errors.Is(err, sink.ErrBufferFull) {
					t.Fatalf("в onDone буфер ещё занят: %v", err)
				}
			case <-time.After(waitTimeout):
				t.Fatal("onDone не вызван")
			}
		})
	}
}
