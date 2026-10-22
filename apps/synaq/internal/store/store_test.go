package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"synaq/internal/model"
)

// Интеграционные тесты: нужен PostgreSQL, DSN суперпользователя-владельца в
// SYNAQ_TEST_DATABASE_URL. Каждый тест создаёт отдельную временную базу.
//
//	docker run --rm -d --name synaq-t2-pg -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:16-alpine
//	SYNAQ_TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55432/postgres go test ./internal/store/...

// sqlalchemyDDL — ровно то, что выдаёт SQLAlchemy create_all для ExperimentModel.
const sqlalchemyDDL = `CREATE TABLE experiments (
	name VARCHAR(50) NOT NULL,
	variants JSONB NOT NULL,
	config JSONB,
	url VARCHAR(200),
	is_active BOOLEAN NOT NULL,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
	PRIMARY KEY (name)
)`

var dbSeq atomic.Int64

// newTestDB создаёт пустую базу и возвращает её DSN; база удаляется по завершении теста.
func newTestDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("SYNAQ_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SYNAQ_TEST_DATABASE_URL не задан — интеграционный тест пропущен")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("подключение к тестовому PostgreSQL: %v", err)
	}
	defer admin.Close(ctx)

	name := fmt.Sprintf("synaq_store_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("SYNAQ_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newRepo — пустая база с применёнными миграциями.
func newRepo(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	pool := openPool(t, newTestDB(t))
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return NewRepository(pool), pool
}

func tableExists(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass('public.experiments') IS NOT NULL").Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func triggerExists(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_trigger WHERE tgname = 'experiments_notify' AND NOT tgisinternal").Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func ptr[T any](v T) *T { return &v }

func exp(name string, active bool) model.Experiment {
	return model.Experiment{
		Name:     name,
		Variants: map[string]int64{"A": 50, "B": 50},
		Config:   json.RawMessage(`{"A":{"color":"red"}}`),
		URL:      ptr("/landing"),
		IsActive: active,
	}
}

func mustCreate(t *testing.T, r *Repository, e model.Experiment) {
	t.Helper()
	if err := r.Create(context.Background(), e); err != nil {
		t.Fatalf("Create(%s): %v", e.Name, err)
	}
}

func TestDSNFromEnv(t *testing.T) {
	t.Setenv("POSTGRES_HOST", "db")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("POSTGRES_USER", "app")
	t.Setenv("POSTGRES_PASSWORD", "p@ss:w/rd")
	t.Setenv("POSTGRES_DB", "experiments")
	dsn, err := DSNFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Host != "db:5432" || u.User.Username() != "app" || pw != "p@ss:w/rd" || u.Path != "/experiments" {
		t.Fatalf("неверный DSN: host=%s user=%s path=%s", u.Host, u.User.Username(), u.Path)
	}

	t.Setenv("POSTGRES_PASSWORD", "")
	t.Setenv("POSTGRES_DB", "")
	_, err = DSNFromEnv()
	if err == nil || !strings.Contains(err.Error(), "POSTGRES_PASSWORD") || !strings.Contains(err.Error(), "POSTGRES_DB") {
		t.Fatalf("ожидалась ошибка с именами переменных, получено %v", err)
	}
}

func TestMigrateUpDownUp(t *testing.T) {
	pool := openPool(t, newTestDB(t))
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !tableExists(t, pool) || !triggerExists(t, pool) {
		t.Fatal("после Up должны быть таблица и триггер")
	}
	// Повторный Migrate — no-op.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("повторный Migrate: %v", err)
	}

	p, db, err := newProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("DownTo(0): %v", err)
	}
	if tableExists(t, pool) {
		t.Fatal("после Down таблица должна быть удалена")
	}
	var fn int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_proc WHERE proname = 'experiments_notify'").Scan(&fn); err != nil {
		t.Fatal(err)
	}
	if fn != 0 {
		t.Fatal("после Down функция experiments_notify должна быть удалена")
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Up после Down: %v", err)
	}
	if !tableExists(t, pool) || !triggerExists(t, pool) {
		t.Fatal("после повторного Up должны быть таблица и триггер")
	}
}

// База, созданная Python-сервисом (SQLAlchemy create_all, без goose и триггера):
// Migrate не трогает данные и добавляет триггер.
func TestMigrateOverSQLAlchemySchema(t *testing.T) {
	pool := openPool(t, newTestDB(t))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, sqlalchemyDDL); err != nil {
		t.Fatal(err)
	}
	// Python пишет config=None как jsonb 'null' (none_as_null=False).
	if _, err := pool.Exec(ctx,
		`INSERT INTO experiments (name, variants, config, url, is_active) VALUES ('legacy', '{"B": 1, "A": 3}', 'null', NULL, true)`); err != nil {
		t.Fatal(err)
	}
	if triggerExists(t, pool) {
		t.Fatal("триггера ещё не должно быть")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !triggerExists(t, pool) {
		t.Fatal("Migrate должен добавить триггер")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("повторный Migrate: %v", err)
	}

	got, err := NewRepository(pool).Get(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.Config != nil || got.URL != nil || got.Variants["A"] != 3 || got.Variants["B"] != 1 || !got.IsActive {
		t.Fatalf("неожиданная строка: %+v", got)
	}
}

func TestNotifyPayload(t *testing.T) {
	r, pool := newRepo(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		t.Fatal(err)
	}

	expect := func(op, name string) {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("ожидание NOTIFY %s: %v", op, err)
		}
		if n.Channel != NotifyChannel {
			t.Fatalf("канал %q", n.Channel)
		}
		var p struct{ Op, Name string }
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", n.Payload, err)
		}
		if p.Op != op || p.Name != name {
			t.Fatalf("payload %q, ожидалось op=%s name=%s", n.Payload, op, name)
		}
	}

	mustCreate(t, r, exp("notify_me", true))
	expect("INSERT", "notify_me")
	if _, err := r.Update(ctx, "notify_me", model.ExperimentPatch{IsActive: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	expect("UPDATE", "notify_me")
	if err := r.Delete(ctx, "notify_me"); err != nil {
		t.Fatal(err)
	}
	expect("DELETE", "notify_me")
}

func TestCreateGet(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	e := exp("checkout", true)
	mustCreate(t, r, e)

	if err := r.Create(ctx, e); !errors.Is(err, model.ErrExists) {
		t.Fatalf("дубликат: ожидался ErrExists, получено %v", err)
	}

	got, err := r.Get(ctx, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "checkout" || got.Variants["A"] != 50 || got.Variants["B"] != 50 || !got.IsActive ||
		got.URL == nil || *got.URL != "/landing" || got.CreatedAt.IsZero() {
		t.Fatalf("неожиданный эксперимент: %+v", got)
	}
	var cfg map[string]map[string]string
	if err := json.Unmarshal(got.Config, &cfg); err != nil || cfg["A"]["color"] != "red" {
		t.Fatalf("config %s: %v", got.Config, err)
	}

	// nil Config/URL -> SQL NULL.
	mustCreate(t, r, model.Experiment{Name: "bare", Variants: map[string]int64{"A": 1}})
	var cfgNull, urlNull bool
	if err := r.pool.QueryRow(ctx, "SELECT config IS NULL, url IS NULL FROM experiments WHERE name='bare'").Scan(&cfgNull, &urlNull); err != nil {
		t.Fatal(err)
	}
	if !cfgNull || !urlNull {
		t.Fatalf("ожидались SQL NULL: config=%v url=%v", cfgNull, urlNull)
	}

	if _, err := r.Get(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ожидался ErrNotFound, получено %v", err)
	}
}

func TestUpdateRules(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	newVariants := map[string]int64{"A": 10, "B": 90}

	t.Run("not found", func(t *testing.T) {
		_, err := r.Update(ctx, "missing", model.ExperimentPatch{IsActive: ptr(true)})
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("ожидался ErrNotFound, получено %v", err)
		}
	})

	t.Run("active variants change -> 409, row untouched", func(t *testing.T) {
		mustCreate(t, r, exp("u409", true))
		_, err := r.Update(ctx, "u409", model.ExperimentPatch{Variants: &newVariants, URL: ptr("/x"), URLSet: true})
		if !errors.Is(err, model.ErrActiveVariantsChange) {
			t.Fatalf("ожидался ErrActiveVariantsChange, получено %v", err)
		}
		// И когда тот же запрос оставляет is_active=true явно.
		_, err = r.Update(ctx, "u409", model.ExperimentPatch{Variants: &newVariants, IsActive: ptr(true)})
		if !errors.Is(err, model.ErrActiveVariantsChange) {
			t.Fatalf("ожидался ErrActiveVariantsChange, получено %v", err)
		}
		got, _ := r.Get(ctx, "u409")
		if got.Variants["A"] != 50 || *got.URL != "/landing" {
			t.Fatalf("строка не должна меняться: %+v", got)
		}
	})

	t.Run("deactivate in same request allows variants change", func(t *testing.T) {
		mustCreate(t, r, exp("udeact", true))
		got, err := r.Update(ctx, "udeact", model.ExperimentPatch{Variants: &newVariants, IsActive: ptr(false)})
		if err != nil {
			t.Fatal(err)
		}
		if got.IsActive || got.Variants["A"] != 10 || got.Variants["B"] != 90 {
			t.Fatalf("неожиданный результат: %+v", got)
		}
	})

	t.Run("inactive allows variants change; activating with change -> 409", func(t *testing.T) {
		mustCreate(t, r, exp("uinact", false))
		if _, err := r.Update(ctx, "uinact", model.ExperimentPatch{Variants: &newVariants}); err != nil {
			t.Fatal(err)
		}
		other := map[string]int64{"A": 1, "B": 1}
		_, err := r.Update(ctx, "uinact", model.ExperimentPatch{Variants: &other, IsActive: ptr(true)})
		if !errors.Is(err, model.ErrActiveVariantsChange) {
			t.Fatalf("ожидался ErrActiveVariantsChange, получено %v", err)
		}
	})

	t.Run("same variants on active ok", func(t *testing.T) {
		mustCreate(t, r, exp("usame", true))
		same := map[string]int64{"B": 50, "A": 50}
		got, err := r.Update(ctx, "usame", model.ExperimentPatch{Variants: &same})
		if err != nil {
			t.Fatalf("те же варианты не должны давать 409: %v", err)
		}
		if !got.IsActive || got.Variants["A"] != 50 {
			t.Fatalf("неожиданный результат: %+v", got)
		}
	})

	t.Run("explicit null clears, absent keeps", func(t *testing.T) {
		mustCreate(t, r, exp("unull", true))
		got, err := r.Update(ctx, "unull", model.ExperimentPatch{ConfigSet: true, Config: nil})
		if err != nil {
			t.Fatal(err)
		}
		if got.Config != nil || got.URL == nil || *got.URL != "/landing" {
			t.Fatalf("config должен очиститься, url остаться: %+v", got)
		}
		got, err = r.Update(ctx, "unull", model.ExperimentPatch{URLSet: true, URL: nil})
		if err != nil {
			t.Fatal(err)
		}
		if got.URL != nil || got.Config != nil || !got.IsActive || got.Variants["A"] != 50 {
			t.Fatalf("url должен очиститься: %+v", got)
		}
		var cfgNull, urlNull bool
		if err := r.pool.QueryRow(ctx, "SELECT config IS NULL, url IS NULL FROM experiments WHERE name='unull'").Scan(&cfgNull, &urlNull); err != nil {
			t.Fatal(err)
		}
		if !cfgNull || !urlNull {
			t.Fatalf("ожидались SQL NULL: config=%v url=%v", cfgNull, urlNull)
		}

		cfg := json.RawMessage(`{"B":{"x":1}}`)
		got, err = r.Update(ctx, "unull", model.ExperimentPatch{ConfigSet: true, Config: &cfg, URLSet: true, URL: ptr("/new")})
		if err != nil {
			t.Fatal(err)
		}
		if got.URL == nil || *got.URL != "/new" || !strings.Contains(string(got.Config), `"x"`) {
			t.Fatalf("новые значения не записаны: %+v", got)
		}
	})
}

// FOR UPDATE: Update ждёт чужую блокировку строки и проверяет правило 409 по
// значению после её снятия, а не по устаревшему снимку.
func TestUpdateConcurrentSerialization(t *testing.T) {
	r, pool := newRepo(t)
	ctx := context.Background()
	mustCreate(t, r, exp("race", false))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM experiments WHERE name='race' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	newVariants := map[string]int64{"A": 1, "B": 99}
	done := make(chan error, 1)
	go func() {
		_, err := r.Update(ctx, "race", model.ExperimentPatch{Variants: &newVariants})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Update должен ждать блокировку строки, завершился: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Параллельный PATCH включает эксперимент и коммитит.
	if _, err := tx.Exec(ctx, "UPDATE experiments SET is_active = true WHERE name='race'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, model.ErrActiveVariantsChange) {
			t.Fatalf("после снятия блокировки ожидался ErrActiveVariantsChange, получено %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Update не завершился после снятия блокировки")
	}
	got, _ := r.Get(ctx, "race")
	if got.Variants["A"] != 50 {
		t.Fatalf("варианты не должны измениться: %+v", got.Variants)
	}
}

func TestDelete(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	if err := r.Delete(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ожидался ErrNotFound, получено %v", err)
	}
	mustCreate(t, r, exp("gone", true))
	if err := r.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "gone"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("после удаления ожидался ErrNotFound, получено %v", err)
	}
}

func TestListOrder(t *testing.T) {
	r, pool := newRepo(t)
	ctx := context.Background()
	for _, n := range []string{"b_same", "a_same", "old", "newest"} {
		mustCreate(t, r, exp(n, true))
	}
	// Явные created_at: a_same и b_same с одинаковым временем — порядок по name.
	_, err := pool.Exec(ctx, `UPDATE experiments SET created_at = CASE name
		WHEN 'old' THEN '2020-01-01T00:00:00Z'::timestamptz
		WHEN 'a_same' THEN '2021-01-01T00:00:00Z'::timestamptz
		WHEN 'b_same' THEN '2021-01-01T00:00:00Z'::timestamptz
		WHEN 'newest' THEN '2022-01-01T00:00:00Z'::timestamptz END`)
	if err != nil {
		t.Fatal(err)
	}
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name)
	}
	if got, want := strings.Join(names, ","), "newest,a_same,b_same,old"; got != want {
		t.Fatalf("порядок %s, ожидался %s", got, want)
	}

	empty, _ := newRepo(t)
	l, err := empty.List(ctx)
	if err != nil || len(l) != 0 {
		t.Fatalf("пустой список: %v %v", l, err)
	}
}
