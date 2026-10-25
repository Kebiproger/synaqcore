package eventstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"synaq/internal/store"
)

func names(days []time.Time) []string {
	out := make([]string, len(days))
	for i, d := range days {
		out[i] = partitionName(d)
	}
	return out
}

func TestWantedDaysAroundMidnightUTC(t *testing.T) {
	cases := []struct {
		now         time.Time
		first, last string
	}{
		{time.Date(2026, 10, 22, 23, 59, 59, 0, time.UTC), "tracking_events_p20261015", "tracking_events_p20261025"},
		{time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), "tracking_events_p20261016", "tracking_events_p20261026"},
		// 02:30 в UTC+3 — это ещё 22 октября по UTC.
		{time.Date(2026, 10, 23, 2, 30, 0, 0, time.FixedZone("MSK", 3*3600)), "tracking_events_p20261015", "tracking_events_p20261025"},
		// Переход через границу года.
		{time.Date(2027, 1, 2, 12, 0, 0, 0, time.UTC), "tracking_events_p20261226", "tracking_events_p20270105"},
	}
	for _, c := range cases {
		got := names(wantedDays(c.now, 3))
		if len(got) != pastDays+3+1 || got[0] != c.first || got[len(got)-1] != c.last {
			t.Errorf("now=%s: получено %v", c.now, got)
		}
	}
}

func TestDaysToCreateSkipsExisting(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	existing := []string{"tracking_events_p20261022", "tracking_events_p20261015", "tracking_events_p20200101", "other"}
	got := names(daysToCreate(existing, now, 1))
	want := []string{
		"tracking_events_p20261016", "tracking_events_p20261017", "tracking_events_p20261018",
		"tracking_events_p20261019", "tracking_events_p20261020", "tracking_events_p20261021",
		"tracking_events_p20261023",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("daysToCreate = %v, want %v", got, want)
	}
}

func TestPartitionsToDrop(t *testing.T) {
	existing := []string{
		"tracking_events_p20260723", // сегодня−91
		"tracking_events_p20260724", // сегодня−90: граница, остаётся
		"tracking_events_p20260722",
		"tracking_events_p20261022",
		// Чужие и некорректные имена не удаляются никогда.
		"tracking_events_p2026072",
		"tracking_events_p202607221",
		"tracking_events_p20261399",
		"tracking_events_backup",
		"xtracking_events_p20200101",
		"tracking_events_p20200101_old",
		"Tracking_events_p20200101",
		"tracking_events",
	}
	cases := []struct {
		now  time.Time
		want []string
	}{
		{time.Date(2026, 10, 22, 23, 59, 59, 0, time.UTC), []string{"tracking_events_p20260722", "tracking_events_p20260723"}},
		// Через секунду наступают новые сутки UTC: граница сдвигается на день.
		{time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), []string{"tracking_events_p20260722", "tracking_events_p20260723", "tracking_events_p20260724"}},
		// 01:00 UTC+3 = 22:00 UTC 22 октября.
		{time.Date(2026, 10, 23, 1, 0, 0, 0, time.FixedZone("MSK", 3*3600)), []string{"tracking_events_p20260722", "tracking_events_p20260723"}},
	}
	for _, c := range cases {
		got := partitionsToDrop(existing, c.now, 90)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("now=%s: partitionsToDrop = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestParsePartitionName(t *testing.T) {
	day, ok := parsePartitionName("tracking_events_p20240229")
	if !ok || !day.Equal(time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("20240229: %v %v", day, ok)
	}
	for _, bad := range []string{
		"tracking_events_p20230229", "tracking_events_p20261301", "tracking_events_p00000000",
		"tracking_events_p2026-10-2", "tracking_events_p２０２６１０２２", "tracking_events_p20261022\n",
		" tracking_events_p20261022", "public.tracking_events_p20261022",
	} {
		if _, ok := parsePartitionName(bad); ok {
			t.Errorf("%q принято как имя секции", bad)
		}
	}
	if n := partitionName(time.Date(2026, 1, 2, 23, 0, 0, 0, time.FixedZone("X", -5*3600))); n != "tracking_events_p20260103" {
		t.Errorf("partitionName = %s", n)
	}
}

func TestNewClampsOptions(t *testing.T) {
	m := New(nil, Options{RetentionDays: 1, AheadDays: 100})
	if m.opts.RetentionDays != minRetentionDays || m.opts.AheadDays != maxAheadDays || m.opts.Now == nil {
		t.Errorf("opts = %+v", m.opts)
	}
	if m := New(nil, Options{RetentionDays: 90}); m.opts.AheadDays != minAheadDays {
		t.Errorf("AheadDays = %d", m.opts.AheadDays)
	}
}

// --- Интеграционные тесты ---
// Нужен PostgreSQL: DSN суперпользователя в SYNAQ_TEST_DATABASE_URL (как в internal/store).
// Каждый тест создаёт отдельную временную базу.

var dbSeq atomic.Int64

func adminURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("SYNAQ_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SYNAQ_TEST_DATABASE_URL не задан — интеграционный тест пропущен")
	}
	return base
}

func adminExec(t *testing.T, base string, sqls ...string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("подключение к тестовому PostgreSQL: %v", err)
	}
	defer c.Close(ctx)
	for _, s := range sqls {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// newTestDB создаёт пустую базу и возвращает её имя и DSN (того же пользователя).
func newTestDB(t *testing.T) (string, string) {
	t.Helper()
	base := adminURL(t)
	name := fmt.Sprintf("synaq_eventstore_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	ident := pgx.Identifier{name}.Sanitize()
	adminExec(t, base, "CREATE DATABASE "+ident)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("SYNAQ_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return name, u.String()
}

func migratedPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

func attachedPartitions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'events.tracking_events'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", "events."+name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// insertSQL — батчевая вставка из плана (такую же использует sink/postgres).
const insertSQL = `
INSERT INTO events.tracking_events (event_date, ts, event_id, user_id, event_name, experiment_name, variant, event_data)
SELECT (t.ts AT TIME ZONE 'UTC')::date, t.ts, t.event_id, t.user_id, t.event_name, t.experiment_name, t.variant, t.event_data::jsonb
FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
     AS t(ts, event_id, user_id, event_name, experiment_name, variant, event_data)
ON CONFLICT DO NOTHING`

func insertEvent(ctx context.Context, pool *pgxpool.Pool, ts time.Time, eventID string) (int64, error) {
	tag, err := pool.Exec(ctx, insertSQL,
		[]time.Time{ts}, []string{eventID}, []string{"user_test_1"}, []string{"purchase"},
		[]string{"exp_test"}, []string{"A"}, []string{`{"amount": 10}`})
	return tag.RowsAffected(), err
}

func TestIntegrationEnsureCreateDropIdempotent(t *testing.T) {
	_, dsn := newTestDB(t)
	pool := migratedPool(t, dsn)
	ctx := context.Background()

	// Секции «прошлого»: 2026-07-01 .. 2026-07-11.
	past := New(pool, Options{RetentionDays: 90, AheadDays: 3, Now: fixedClock(time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC))})
	if err := past.EnsureNow(ctx); err != nil {
		t.Fatalf("EnsureNow (прошлое): %v", err)
	}
	// Чужие таблицы в схеме events: не секции и секция с неверным именем.
	if _, err := pool.Exec(ctx, `
CREATE TABLE events.tracking_events_backup (id int);
CREATE TABLE events.tracking_events_p20200101_old (id int);
CREATE TABLE events.tracking_events_p20200102 (LIKE events.tracking_events);`); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 22, 0, 30, 0, 0, time.UTC)
	m := New(pool, Options{RetentionDays: 90, AheadDays: 3, Now: fixedClock(now)})
	for i := 0; i < 2; i++ { // второй прогон — ничего не меняет и не падает
		if err := m.EnsureNow(ctx); err != nil {
			t.Fatalf("EnsureNow #%d: %v", i+1, err)
		}
	}
	got := attachedPartitions(t, pool)
	// Отсечка: дни < 2026-07-24 удаляются; из «прошлых» не осталось ни одной.
	want := names(wantedDays(now, 3))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("секции = %v, want %v", got, want)
	}
	for _, n := range []string{"tracking_events_backup", "tracking_events_p20200101_old", "tracking_events_p20200102"} {
		if !tableExists(t, pool, n) {
			t.Errorf("чужая таблица %s удалена", n)
		}
	}
	if tableExists(t, pool, "tracking_events_p20260701") {
		t.Error("старая секция осталась отдельной таблицей после DETACH")
	}
}

func TestIntegrationInsertNeedsPartitionAndDedups(t *testing.T) {
	_, dsn := newTestDB(t)
	pool := migratedPool(t, dsn)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC)

	_, err := insertEvent(ctx, pool, now, "evt-1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("вставка без секции: err = %v, ожидался SQLSTATE 23514", err)
	}

	if err := New(pool, Options{RetentionDays: 90, AheadDays: 3, Now: fixedClock(now)}).EnsureNow(ctx); err != nil {
		t.Fatalf("EnsureNow: %v", err)
	}
	if n, err := insertEvent(ctx, pool, now, "evt-1"); err != nil || n != 1 {
		t.Fatalf("вставка: n=%d err=%v", n, err)
	}
	// Тот же ключ в тот же UTC-день (другое время) — дубль, остаётся первая строка.
	if n, err := insertEvent(ctx, pool, now.Add(30*time.Minute-time.Second), "evt-1"); err != nil || n != 0 {
		t.Fatalf("дубль: n=%d err=%v", n, err)
	}
	// Тот же ключ на следующий UTC-день — отдельная строка.
	if n, err := insertEvent(ctx, pool, now.Add(90*time.Minute), "evt-1"); err != nil || n != 1 {
		t.Fatalf("другой день: n=%d err=%v", n, err)
	}
	var rows int
	var firstTS time.Time
	if err := pool.QueryRow(ctx, "SELECT count(*), min(ts) FROM events.tracking_events WHERE event_id = 'evt-1'").Scan(&rows, &firstTS); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || !firstTS.Equal(now) {
		t.Errorf("строк = %d, min(ts) = %s", rows, firstTS)
	}
}

func TestIntegrationAdvisoryLockSkips(t *testing.T) {
	_, dsn := newTestDB(t)
	pool := migratedPool(t, dsn)
	ctx := context.Background()

	other, err := pgx.Connect(ctx, dsn) // «другая реплика» держит блокировку
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		t.Fatal(err)
	}
	m := New(pool, Options{RetentionDays: 90, AheadDays: 3, Now: fixedClock(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))})
	if err := m.EnsureNow(ctx); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("EnsureNow при занятой блокировке: err = %v, ожидалась ErrLockBusy", err)
	}
	if got := attachedPartitions(t, pool); len(got) != 0 {
		t.Fatalf("при занятой блокировке созданы секции: %v", got)
	}
	if _, err := other.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := attachedPartitions(t, pool); len(got) != pastDays+3+1 {
		t.Fatalf("секции = %v", got)
	}
	// Блокировка снята после EnsureNow: другая сессия может её взять.
	var ok bool
	if err := other.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&ok); err != nil || !ok {
		t.Fatalf("блокировка не снята: ok=%v err=%v", ok, err)
	}
}

// Миграция и обслуживание секций от роли без суперпользователя, как synaq_app в
// docker-compose: владелец схем public и events, CONNECT/TEMP/CREATE на базу.
func TestIntegrationNonSuperuserRole(t *testing.T) {
	base := adminURL(t)
	dbName, _ := newTestDB(t)
	role := fmt.Sprintf("synaq_app_test_%d_%d", os.Getpid(), dbSeq.Add(1))
	roleID, dbID := pgx.Identifier{role}.Sanitize(), pgx.Identifier{dbName}.Sanitize()
	const pw = "synthetic-test-role-pw"
	adminExec(t, base, fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '%s'", roleID, pw))
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		// База удаляется cleanup'ом newTestDB позже (LIFO), поэтому сначала она.
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbID+" WITH (FORCE)")
		_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+roleID)
	})
	adminExec(t, base, fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM PUBLIC", dbID),
		fmt.Sprintf("GRANT CONNECT, TEMP, CREATE ON DATABASE %s TO %s", dbID, roleID))
	u, _ := url.Parse(base)
	u.Path = "/" + dbName
	adminExec(t, u.String(),
		"ALTER SCHEMA public OWNER TO "+roleID,
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		"CREATE SCHEMA IF NOT EXISTS events AUTHORIZATION "+roleID)
	u.User = url.UserPassword(role, pw)

	pool := migratedPool(t, u.String())
	ctx := context.Background()
	var super bool
	if err := pool.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil || super {
		t.Fatalf("ожидалась роль без суперпользователя: super=%v err=%v", super, err)
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	m := New(pool, Options{RetentionDays: 8, AheadDays: 1, Now: fixedClock(now.AddDate(0, 0, -30))})
	if err := m.EnsureNow(ctx); err != nil {
		t.Fatalf("EnsureNow (прошлое): %v", err)
	}
	m.opts.Now = fixedClock(now)
	if err := m.EnsureNow(ctx); err != nil {
		t.Fatalf("EnsureNow: %v", err)
	}
	if got, want := attachedPartitions(t, pool), names(wantedDays(now, 1)); !reflect.DeepEqual(got, want) {
		t.Errorf("секции = %v, want %v", got, want)
	}
	if n, err := insertEvent(ctx, pool, now, "evt-role"); err != nil || n != 1 {
		t.Fatalf("вставка от роли приложения: n=%d err=%v", n, err)
	}
}

func TestIntegrationRunStopsOnCancel(t *testing.T) {
	_, dsn := newTestDB(t)
	pool := migratedPool(t, dsn)
	m := New(pool, Options{RetentionDays: 90, AheadDays: 2})
	m.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(attachedPartitions(t, pool)) != pastDays+2+1 {
		if time.Now().After(deadline) {
			t.Fatal("Run не создал секции")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены ctx")
	}
}
