package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"synaq/internal/eventstore"
)

// fakeExec всегда отвечает 23514 (нет партиции) и считает вставки.
type fakeExec struct{ calls int }

func (f *fakeExec) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.calls++
	return pgconn.CommandTag{}, &pgconn.PgError{Code: "23514"}
}

// Занятый advisory-lock в Ensure — повторяемая ошибка: батч не отбрасывается.
// Партиции нет и после успешного Ensure — батч отбрасывается без повтора.
func TestWriteEnsureLockBusyIsRetryable(t *testing.T) {
	payloads := [][]byte{[]byte(`{"event_id":"e1","user_id":"u1","event_name":"n","timestamp":1700000000000}`)}

	busy := &Producer{exec: &fakeExec{}, cfg: Config{WriteTimeout: time.Second, Ensure: func(context.Context) error {
		return eventstore.ErrLockBusy
	}}}
	retryable, err := busy.write(context.Background(), payloads)
	if !retryable || !errors.Is(err, eventstore.ErrLockBusy) {
		t.Fatalf("retryable=%v err=%v, ожидался повтор с ErrLockBusy", retryable, err)
	}

	ok := &Producer{exec: &fakeExec{}, cfg: Config{WriteTimeout: time.Second, Ensure: func(context.Context) error { return nil }}}
	retryable, err = ok.write(context.Background(), payloads)
	if retryable || err != nil {
		t.Fatalf("retryable=%v err=%v, ожидалось отбрасывание без повтора", retryable, err)
	}
}
