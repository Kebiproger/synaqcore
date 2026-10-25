// Package eventstore обслуживает хранилище событий в PostgreSQL: суточные секции
// events.tracking_events_pYYYYMMDD (без pg_partman). Maintainer заранее создаёт
// секции на ближайшие дни и удаляет секции старше срока хранения.
package eventstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Schema и Table — родительская секционированная таблица (миграция 00003_events.sql).
	Schema = "events"
	Table  = "tracking_events"

	// pastDays — сколько прошедших дней держать с секциями (события с ts до now−7d
	// трекер отклоняет, но опоздавшие батчи и ретраи укладываются в окно).
	pastDays = 7
	// Границы опций; конфиг проверяет их сам, здесь — защита от неверных значений.
	minRetentionDays = pastDays + 1
	minAheadDays     = 1
	maxAheadDays     = 30

	// advisoryLockKey — ключ pg_try_advisory_lock: обслуживание секций в один момент
	// времени выполняет только одна реплика. Константа: "synq_evp" в ASCII.
	advisoryLockKey int64 = 0x73796e715f657670

	// lockTimeout — SET LOCAL lock_timeout для каждой DDL: ATTACH/DETACH не должны
	// надолго вставать в очередь блокировок перед вставками событий.
	lockTimeout = "2s"

	defaultInterval      = time.Hour
	defaultRetryInterval = time.Minute
	unlockTimeout        = 5 * time.Second
	dayLayout            = "20060102"
)

// partitionNameRe — строгий формат имени секции. Таблицы с другими именами
// Maintainer никогда не трогает.
var partitionNameRe = regexp.MustCompile(`^tracking_events_p(\d{8})$`)

// ErrLockBusy — advisory-lock обслуживания секций занят другой репликой или
// Maintainer'ом. Секции в этот момент могут быть ещё не созданы, поэтому это
// повторяемая ошибка, а не успех: приёмник событий повторяет батч позже.
var ErrLockBusy = errors.New("обслуживание секций занято другой репликой (advisory-lock)")

// Options — параметры обслуживания секций.
type Options struct {
	// RetentionDays — срок хранения: удаляются секции дней < сегодня−RetentionDays (UTC).
	// Минимум 8 (меньшие значения поднимаются до 8).
	RetentionDays int
	// AheadDays — на сколько дней вперёд создавать секции (1..30, значения вне
	// диапазона приводятся к границе).
	AheadDays int
	// Now — часы (по умолчанию time.Now); для тестов.
	Now func() time.Time
}

// Maintainer создаёт и удаляет суточные секции events.tracking_events.
type Maintainer struct {
	pool          *pgxpool.Pool
	opts          Options
	interval      time.Duration
	retryInterval time.Duration
}

// New создаёт Maintainer. Подключения берутся из pool.
func New(pool *pgxpool.Pool, o Options) *Maintainer {
	if o.RetentionDays < minRetentionDays {
		o.RetentionDays = minRetentionDays
	}
	if o.AheadDays < minAheadDays {
		o.AheadDays = minAheadDays
	}
	if o.AheadDays > maxAheadDays {
		o.AheadDays = maxAheadDays
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Maintainer{
		pool:          pool,
		opts:          o,
		interval:      defaultInterval,
		retryInterval: defaultRetryInterval,
	}
}

// Run обслуживает секции до отмены ctx: сразу при запуске, затем раз в час,
// после ошибки — повтор через минуту. Блокирует вызывающего.
func (m *Maintainer) Run(ctx context.Context) {
	for {
		delay := m.interval
		if err := m.EnsureNow(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("eventstore: обслуживание секций: %v (повтор через %s)", err, m.retryInterval)
			delay = m.retryInterval
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// EnsureNow один раз приводит секции в порядок: создаёт недостающие на дни
// сегодня−7 .. сегодня+AheadDays (UTC) и удаляет секции старше RetentionDays.
// Если обслуживание уже выполняет другая реплика (advisory-lock занят), ничего
// не делает и возвращает ErrLockBusy: секции могут быть ещё не созданы, вызывающий
// должен повторить позже. Ошибки отдельных секций не прерывают остальные:
// возвращается их объединение.
func (m *Maintainer) EnsureNow(ctx context.Context) error {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("соединение: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("advisory-lock: %w", err)
	}
	if !locked {
		return ErrLockBusy
	}
	defer unlock(conn)

	now := m.opts.Now()
	parts, err := listPartitions(ctx, conn)
	if err != nil {
		return err
	}
	attached := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.attached {
			attached = append(attached, p.name)
		}
	}

	var errs []error
	// Сначала создание: без секции на сегодня вставки событий падают.
	for _, day := range daysToCreate(attached, now, m.opts.AheadDays) {
		if err := createPartition(ctx, conn, day); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range partitionsToDrop(attached, now, m.opts.RetentionDays) {
		if err := dropPartition(ctx, conn, name); err != nil {
			errs = append(errs, err)
		}
	}
	// Отсоединённая, но не удалённая таблица (сбой между DETACH и DROP или ручной
	// DETACH): автоматически не удаляем — это уже не секция, только предупреждаем.
	var detached []string
	for _, p := range parts {
		if !p.attached {
			detached = append(detached, p.name)
		}
	}
	for _, name := range partitionsToDrop(detached, now, m.opts.RetentionDays) {
		log.Printf("eventstore: таблица %s.%s старше срока хранения, но не присоединена к %s — удалите вручную", Schema, name, Table)
	}
	return errors.Join(errs...)
}

// unlock снимает session-level advisory-lock. Если это не удалось, соединение
// закрывается: иначе блокировка осталась бы на соединении в пуле и обслуживание
// остановилось бы на всех репликах.
func unlock(conn *pgxpool.Conn) {
	if conn.Conn().IsClosed() { // сессия завершена (например, отмена ctx) — блокировка снята сервером
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey).Scan(&ok); err != nil || !ok {
		log.Printf("eventstore: не удалось снять advisory-lock (ok=%v, err=%v), соединение закрывается", ok, err)
		_ = conn.Conn().Close(ctx) // пул уничтожит закрытое соединение при Release
	}
}

type partition struct {
	name     string
	attached bool
}

// listPartitions возвращает таблицы схемы events со строгим именем секции:
// attached=true — секции events.tracking_events по pg_inherits.
func listPartitions(ctx context.Context, conn *pgxpool.Conn) ([]partition, error) {
	rows, err := conn.Query(ctx, `
SELECT c.relname,
       EXISTS (
           SELECT 1 FROM pg_inherits i
           JOIN pg_class p ON p.oid = i.inhparent
           JOIN pg_namespace pn ON pn.oid = p.relnamespace
           WHERE i.inhrelid = c.oid AND pn.nspname = $1 AND p.relname = $2
       )
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'r'`, Schema, Table)
	if err != nil {
		return nil, fmt.Errorf("список секций: %w", err)
	}
	var out []partition
	for rows.Next() {
		var p partition
		if err := rows.Scan(&p.name, &p.attached); err != nil {
			rows.Close()
			return nil, fmt.Errorf("список секций: %w", err)
		}
		if _, ok := parsePartitionName(p.name); ok {
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("список секций: %w", err)
	}
	return out, nil
}

// createPartition создаёт таблицу секции и присоединяет её. Два отдельных шага,
// каждый в своей транзакции: CREATE ... IF NOT EXISTS делает повтор после сбоя
// между ними безопасным. ATTACH берёт SHARE UPDATE EXCLUSIVE на родителя и не
// блокирует вставки (в отличие от CREATE TABLE ... PARTITION OF).
func createPartition(ctx context.Context, conn *pgxpool.Conn, day time.Time) error {
	name := partitionName(day)
	part := pgx.Identifier{Schema, name}.Sanitize()
	parent := pgx.Identifier{Schema, Table}.Sanitize()
	// Границы — даты, отформатированные из time.Time (только цифры и '-').
	from, to := day.Format(time.DateOnly), day.AddDate(0, 0, 1).Format(time.DateOnly)

	create := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS)", part, parent)
	if err := execDDL(ctx, conn, create); err != nil {
		return fmt.Errorf("создание секции %s: %w", name, err)
	}
	attach := fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')", parent, part, from, to)
	if err := execDDL(ctx, conn, attach); err != nil {
		return fmt.Errorf("присоединение секции %s: %w", name, err)
	}
	log.Printf("eventstore: создана секция %s.%s", Schema, name)
	return nil
}

// dropPartition отсоединяет и удаляет секцию (каждый шаг — своя транзакция).
// Имя уже прошло строгую проверку parsePartitionName.
func dropPartition(ctx context.Context, conn *pgxpool.Conn, name string) error {
	part := pgx.Identifier{Schema, name}.Sanitize()
	parent := pgx.Identifier{Schema, Table}.Sanitize()
	if err := execDDL(ctx, conn, fmt.Sprintf("ALTER TABLE %s DETACH PARTITION %s", parent, part)); err != nil {
		return fmt.Errorf("отсоединение секции %s: %w", name, err)
	}
	if err := execDDL(ctx, conn, "DROP TABLE "+part); err != nil {
		return fmt.Errorf("удаление секции %s: %w", name, err)
	}
	log.Printf("eventstore: удалена секция %s.%s (старше срока хранения)", Schema, name)
	return nil
}

// execDDL выполняет одну DDL в отдельной транзакции с SET LOCAL lock_timeout.
func execDDL(ctx context.Context, conn *pgxpool.Conn, sql string) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+lockTimeout+"'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql)
		return err
	})
}

// --- Чистая логика дат (без БД) ---

// utcDay — начало UTC-суток, содержащих t.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// partitionName — имя секции дня: tracking_events_pYYYYMMDD.
func partitionName(day time.Time) string {
	return Table + "_p" + day.UTC().Format(dayLayout)
}

// parsePartitionName разбирает имя секции. Принимает только точный формат
// tracking_events_pYYYYMMDD с существующей датой (20261399 отклоняется).
func parsePartitionName(name string) (time.Time, bool) {
	m := partitionNameRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation(dayLayout, m[1], time.UTC)
	if err != nil || day.Format(dayLayout) != m[1] {
		return time.Time{}, false
	}
	return day, true
}

// wantedDays — дни (UTC), для которых должны существовать секции:
// сегодня−7 .. сегодня+ahead включительно.
func wantedDays(now time.Time, ahead int) []time.Time {
	today := utcDay(now)
	days := make([]time.Time, 0, pastDays+ahead+1)
	for d := -pastDays; d <= ahead; d++ {
		days = append(days, today.AddDate(0, 0, d))
	}
	return days
}

// daysToCreate — дни из wantedDays, для которых нет присоединённой секции.
func daysToCreate(existing []string, now time.Time, ahead int) []time.Time {
	have := make(map[string]bool, len(existing))
	for _, n := range existing {
		have[n] = true
	}
	var out []time.Time
	for _, d := range wantedDays(now, ahead) {
		if !have[partitionName(d)] {
			out = append(out, d)
		}
	}
	return out
}

// partitionsToDrop — секции с днём < сегодня−retention (UTC), по возрастанию дня.
// Имена не строгого формата пропускаются.
func partitionsToDrop(existing []string, now time.Time, retention int) []string {
	cutoff := utcDay(now).AddDate(0, 0, -retention)
	var out []string
	for _, n := range existing {
		day, ok := parsePartitionName(n)
		if ok && day.Before(cutoff) {
			out = append(out, n)
		}
	}
	sort.Strings(out) // фиксированная дата в имени: лексикографический порядок = хронологический
	return out
}
