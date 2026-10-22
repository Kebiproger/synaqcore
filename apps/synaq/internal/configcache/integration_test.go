package configcache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"synaq/internal/model"
)

// Интеграционный тест LISTEN/NOTIFY на настоящем PostgreSQL. Запускается, только
// если задан SYNAQ_TEST_DATABASE_URL (одноразовая БД: таблица пересоздаётся).

const itSchema = `
DROP TABLE IF EXISTS experiments;
CREATE TABLE experiments (
	name       TEXT PRIMARY KEY,
	variants   JSONB NOT NULL,
	config     JSONB,
	url        TEXT,
	is_active  BOOLEAN NOT NULL DEFAULT FALSE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE OR REPLACE FUNCTION experiments_notify() RETURNS trigger AS $$
BEGIN
	PERFORM pg_notify('experiments_changed',
		json_build_object('op', TG_OP, 'name', COALESCE(NEW.name, OLD.name))::text);
	RETURN NULL;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER experiments_notify AFTER INSERT OR UPDATE OR DELETE ON experiments
	FOR EACH ROW EXECUTE FUNCTION experiments_notify();
`

// pgRepo — минимальный Repository для теста: только List.
type pgRepo struct {
	model.Repository
	dsn string
}

func (r *pgRepo) List(ctx context.Context) ([]model.Experiment, error) {
	conn, err := pgx.Connect(ctx, r.dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx,
		`SELECT name, variants, config, url, is_active, created_at
		   FROM experiments ORDER BY created_at DESC, name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Experiment, error) {
		var e model.Experiment
		err := row.Scan(&e.Name, &e.Variants, &e.Config, &e.URL, &e.IsActive, &e.CreatedAt)
		return e, err
	})
}

func TestIntegrationListenNotify(t *testing.T) {
	dsn := os.Getenv("SYNAQ_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SYNAQ_TEST_DATABASE_URL не задан")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, itSchema); err != nil {
		t.Fatal(err)
	}

	connected := make(chan struct{}, 4)
	pgl := PgListener(dsn)
	opts := Options{
		Listen: func(ctx context.Context) (Listener, error) {
			l, err := pgl(ctx)
			if err == nil {
				connected <- struct{}{}
			}
			return l, err
		},
		Interval: time.Hour, // обновление только по уведомлению
		Logf:     t.Logf,
	}
	c := New(&pgRepo{dsn: dsn}, opts)
	if err := c.LoadInitial(ctx); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	defer func() { stop(); <-done }()

	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("LISTEN не подключился")
	}
	time.Sleep(100 * time.Millisecond) // перечитывание после подключения

	start := time.Now()
	if _, err := admin.Exec(ctx,
		`INSERT INTO experiments (name, variants, is_active) VALUES ('it_exp', '{"a":50,"b":50}', true)`); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { _, ok := c.Snapshot().Get("it_exp"); return ok },
		"INSERT виден в снимке менее чем за 1 с")
	t.Logf("INSERT виден через %s", time.Since(start))

	e, _ := c.Snapshot().Get("it_exp")
	if e.Variants["a"] != 50 || !e.IsActive {
		t.Fatalf("неожиданный эксперимент: %+v", e)
	}

	if _, err := admin.Exec(ctx, `DELETE FROM experiments WHERE name = 'it_exp'`); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { _, ok := c.Snapshot().Get("it_exp"); return !ok },
		"DELETE виден в снимке менее чем за 1 с")
}
