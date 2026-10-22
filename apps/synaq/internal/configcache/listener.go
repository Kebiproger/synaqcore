package configcache

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Channel — канал PostgreSQL NOTIFY, в который триггер на experiments
// публикует изменения (payload: {"op": TG_OP, "name": ...}).
const Channel = "experiments_changed"

// Notification — полученное уведомление. Payload информативен: кэш на любое
// уведомление делает полное перечитывание.
type Notification struct {
	Channel string
	Payload string
}

// Listener — подписка на уведомления на выделенном соединении.
type Listener interface {
	// WaitForNotification блокируется до уведомления, ошибки соединения или
	// отмены ctx.
	WaitForNotification(ctx context.Context) (Notification, error)
	// Close закрывает соединение.
	Close(ctx context.Context) error
}

// PgListener возвращает функцию для Options.Listen: открывает отдельное
// pgx-соединение (не из пула: LISTEN привязан к сессии) и выполняет
// LISTEN experiments_changed.
func PgListener(dsn string) func(ctx context.Context) (Listener, error) {
	return func(ctx context.Context) (Listener, error) {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return nil, fmt.Errorf("подключение LISTEN: %w", err)
		}
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
			_ = conn.Close(context.Background())
			return nil, fmt.Errorf("LISTEN %s: %w", Channel, err)
		}
		return &pgListener{conn: conn}, nil
	}
}

type pgListener struct {
	conn *pgx.Conn
}

func (l *pgListener) WaitForNotification(ctx context.Context) (Notification, error) {
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return Notification{}, err
	}
	return Notification{Channel: n.Channel, Payload: n.Payload}, nil
}

func (l *pgListener) Close(ctx context.Context) error {
	return l.conn.Close(ctx)
}
