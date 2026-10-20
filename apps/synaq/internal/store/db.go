// Package store — реализация model.Repository поверх PostgreSQL (таблица experiments):
// пул pgx, миграции goose (встроены в бинарник) и запросы, сгенерированные sqlc
// (internal/store/sqlcdb, конфиг apps/synaq/sqlc.yaml).
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// NotifyChannel — канал LISTEN/NOTIFY, в который триггер experiments_notify
// публикует {"op": TG_OP, "name": <имя эксперимента>} при любом изменении строки.
const NotifyChannel = "experiments_changed"

// Переменные окружения те же, что у apps/experiment_api (src/core/config.py).
var dsnEnvVars = []string{"POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB"}

// DSNFromEnv собирает DSN PostgreSQL из POSTGRES_HOST/PORT/USER/PASSWORD/DB.
// Все переменные обязательны. Ошибка называет только отсутствующие переменные,
// значение пароля в неё не попадает; сам DSN содержит пароль — не логировать.
func DSNFromEnv() (string, error) {
	vals := make(map[string]string, len(dsnEnvVars))
	var missing []string
	for _, k := range dsnEnvVars {
		v, ok := os.LookupEnv(k)
		if !ok || v == "" {
			missing = append(missing, k)
			continue
		}
		vals[k] = v
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("не заданы переменные окружения PostgreSQL: %s", strings.Join(missing, ", "))
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(vals["POSTGRES_USER"], vals["POSTGRES_PASSWORD"]),
		Host:   net.JoinHostPort(vals["POSTGRES_HOST"], vals["POSTGRES_PORT"]),
		Path:   "/" + vals["POSTGRES_DB"],
	}
	return u.String(), nil
}

// Open создаёт пул соединений и проверяет доступность БД (Ping).
// pgx маскирует пароль в ошибках разбора DSN.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("пул PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL недоступен: %w", err)
	}
	return pool, nil
}

// Migrate применяет встроенные миграции goose. Advisory-lock на уровне сессии
// не даёт нескольким инстансам мигрировать одновременно. Идемпотентна: на базе,
// где таблицу уже создал SQLAlchemy (без версии goose), 00001 ничего не меняет,
// а 00002 добавляет триггер NOTIFY.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	p, db, err := newProvider(pool)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	return nil
}

// newProvider создаёт goose.Provider поверх пула. Возвращённый *sql.DB нужно закрыть
// (пул при этом не закрывается).
func newProvider(pool *pgxpool.Pool) (*goose.Provider, *sql.DB, error) {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, nil, err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, nil, fmt.Errorf("goose locker: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("goose provider: %w", err)
	}
	return p, db, nil
}
