package configcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"synaq/internal/model"
)

// ---------- фейки ----------

// fakeRepo — model.Repository, у которого используется только List.
type fakeRepo struct {
	model.Repository // остальные методы не нужны (паника при вызове)

	mu    sync.Mutex
	exps  []model.Experiment
	err   error
	gate  chan struct{} // если не nil, каждый List ждёт значение из gate
	calls atomic.Int32
	// started получает сигнал в начале каждого List (если не nil).
	started chan struct{}
}

func (r *fakeRepo) set(exps []model.Experiment, err error) {
	r.mu.Lock()
	r.exps, r.err = exps, err
	r.mu.Unlock()
}

func (r *fakeRepo) List(ctx context.Context) ([]model.Experiment, error) {
	r.calls.Add(1)
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	// Отдаём копию среза, как настоящий репозиторий.
	return append([]model.Experiment(nil), r.exps...), nil
}

// fakeListener управляется тестом через каналы.
type fakeListener struct {
	notes  chan Notification
	errs   chan error
	closed atomic.Bool
}

func (l *fakeListener) WaitForNotification(ctx context.Context) (Notification, error) {
	select {
	case n := <-l.notes:
		return n, nil
	case err := <-l.errs:
		return Notification{}, err
	case <-ctx.Done():
		return Notification{}, ctx.Err()
	}
}

func (l *fakeListener) Close(context.Context) error {
	l.closed.Store(true)
	return nil
}

// fakeDialer — фабрика слушателей: первые failFirst подключений неуспешны.
type fakeDialer struct {
	mu        sync.Mutex
	failFirst int
	attempts  int
	listeners []*fakeListener
	connected chan *fakeListener
}

func newFakeDialer(failFirst int) *fakeDialer {
	return &fakeDialer{failFirst: failFirst, connected: make(chan *fakeListener, 16)}
}

func (d *fakeDialer) listen(context.Context) (Listener, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts++
	if d.attempts <= d.failFirst {
		return nil, errors.New("connection refused")
	}
	l := &fakeListener{notes: make(chan Notification, 64), errs: make(chan error, 1)}
	d.listeners = append(d.listeners, l)
	d.connected <- l
	return l, nil
}

func (d *fakeDialer) attemptsN() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

// ---------- помощники ----------

func quietOpts() Options {
	return Options{Logf: func(string, ...any) {}}
}

func exp(name string, w int64) model.Experiment {
	u := "https://example.com/" + name
	return model.Experiment{
		Name:      name,
		Variants:  map[string]int64{"control": w, "test": w},
		Config:    json.RawMessage(`{"k":1}`),
		URL:       &u,
		IsActive:  w%2 == 0,
		CreatedAt: time.Unix(1_700_000_000, 0),
	}
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("не дождались за %s: %s", timeout, msg)
}

func names(exps []model.Experiment) []string {
	out := make([]string, len(exps))
	for i, e := range exps {
		out[i] = e.Name
	}
	return out
}

// ---------- тесты ----------

func TestNotReadyBeforeLoad(t *testing.T) {
	c := New(&fakeRepo{}, quietOpts())
	if c.Ready() {
		t.Fatal("Ready() до загрузки должен быть false")
	}
	s := c.Snapshot()
	if s == nil || len(s.All()) != 0 {
		t.Fatal("до загрузки ожидается пустой (не nil) снимок")
	}
	if _, ok := s.Get("x"); ok {
		t.Fatal("в пустом снимке ничего нет")
	}
}

func TestOrderPreservedAndInactiveIncluded(t *testing.T) {
	repo := &fakeRepo{}
	repo.set([]model.Experiment{exp("zeta", 1), exp("alpha", 2), exp("mid", 3)}, nil)
	c := New(repo, quietOpts())
	if err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Ready() {
		t.Fatal("после загрузки Ready() == true")
	}
	got := names(c.Snapshot().All())
	want := []string{"zeta", "alpha", "mid"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("порядок: got %v, want %v", got, want)
	}
	e, ok := c.Snapshot().Get("zeta") // неактивный (вес 1 → IsActive=false)
	if !ok || e.IsActive {
		t.Fatalf("неактивный эксперимент должен быть в снимке: %+v %v", e, ok)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	repo := &fakeRepo{}
	src := []model.Experiment{exp("a", 2), exp("b", 4)}
	repo.set(src, nil)
	c := New(repo, quietOpts())
	if err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Изменение исходных данных репозитория не влияет на снимок (глубокая копия).
	src[0].Variants["control"] = 999
	src[0].Config[2] = 'X'
	*src[0].URL = "mutated"
	e, _ := c.Snapshot().Get("a")
	if e.Variants["control"] != 2 || string(e.Config) != `{"k":1}` || *e.URL != "https://example.com/a" {
		t.Fatalf("снимок разделяет память с репозиторием: %+v", e)
	}
	// Изменение среза из All() не влияет на снимок.
	all := c.Snapshot().All()
	all[0] = model.Experiment{Name: "hacked"}
	if got := names(c.Snapshot().All()); got[0] != "a" {
		t.Fatalf("All() отдал разделяемый срез: %v", got)
	}
}

func TestSnapshotSwapConsistentUnderRace(t *testing.T) {
	const n = 20
	repo := &fakeRepo{}
	gen := func(g int64) []model.Experiment {
		out := make([]model.Experiment, n)
		for i := range out {
			out[i] = exp(fmt.Sprintf("e%02d", i), g)
		}
		return out
	}
	repo.set(gen(0), nil)
	c := New(repo, quietOpts())
	if err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				s := c.Snapshot()
				all := s.All()
				if len(all) != n {
					bad.Add(1)
					return
				}
				g := all[0].Variants["control"]
				for _, e := range all {
					got, ok := s.Get(e.Name)
					if e.Variants["control"] != g || !ok || got.Variants["control"] != g {
						bad.Add(1)
						return
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for g := int64(1); ctx.Err() == nil; g++ {
			repo.set(gen(g), nil)
			_ = c.Reload(ctx)
		}
	}()
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatal("читатель увидел несогласованный снимок")
	}
	if repo.calls.Load() < 3 {
		t.Fatalf("слишком мало перезагрузок: %d", repo.calls.Load())
	}
}

func TestReloadCoalescing(t *testing.T) {
	repo := &fakeRepo{gate: make(chan struct{}), started: make(chan struct{}, 8)}
	repo.set([]model.Experiment{exp("a", 2)}, nil)
	c := New(repo, quietOpts())

	// Первая загрузка стартует и зависает внутри List.
	first := make(chan error, 1)
	go func() { first <- c.Reload(context.Background()) }()
	<-repo.started

	// N конкурентных Reload во время идущей загрузки → одна общая следующая.
	const n = 50
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { errs <- c.Reload(context.Background()) }()
	}
	eventually(t, time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.pending != nil
	}, "отложенная загрузка")
	time.Sleep(50 * time.Millisecond) // даём всем горутинам войти в Reload

	repo.gate <- struct{}{} // отпускаем первую
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	<-repo.started
	repo.gate <- struct{}{} // отпускаем общую вторую
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("ожидалось 2 вызова List (идущий + один общий), получено %d", got)
	}
}

// Вызов, пришедший во время идущей загрузки, видит данные, записанные до его начала.
func TestReloadReadYourWrites(t *testing.T) {
	repo := &fakeRepo{gate: make(chan struct{}), started: make(chan struct{}, 8)}
	repo.set([]model.Experiment{exp("old", 2)}, nil)
	c := New(repo, quietOpts())
	go func() { _ = c.Reload(context.Background()) }()
	<-repo.started // идущая загрузка уже «прочитала» старое состояние

	// Эмулируем коммит админа и последующий Reload.
	done := make(chan error, 1)
	go func() {
		repo.set([]model.Experiment{exp("new", 2)}, nil)
		done <- c.Reload(context.Background())
	}()
	repo.gate <- struct{}{}
	<-repo.started
	repo.gate <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Snapshot().Get("new"); !ok {
		t.Fatal("Reload после коммита должен вернуть свежий снимок")
	}
}

func TestReloadCallerContextCancel(t *testing.T) {
	repo := &fakeRepo{gate: make(chan struct{}), started: make(chan struct{}, 8)}
	c := New(repo, quietOpts())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Reload(ctx) }()
	<-repo.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидался context.Canceled, получено %v", err)
	}
	// Сама загрузка завершается и обновляет снимок.
	repo.gate <- struct{}{}
	eventually(t, time.Second, c.Ready, "загрузка после отмены ожидания")
}

func TestStaleServedOnError(t *testing.T) {
	repo := &fakeRepo{}
	repo.set([]model.Experiment{exp("a", 2)}, nil)
	var logged atomic.Int32
	opts := Options{Logf: func(string, ...any) { logged.Add(1) }}
	c := New(repo, opts)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.set(nil, errors.New("db down"))
	if err := c.Reload(context.Background()); err == nil {
		t.Fatal("ожидалась ошибка Reload")
	}
	if _, ok := c.Snapshot().Get("a"); !ok || !c.Ready() {
		t.Fatal("при ошибке должен раздаваться прежний снимок")
	}
	if logged.Load() == 0 {
		t.Fatal("ошибка перечитывания должна логироваться")
	}
}

func TestLoadInitialRetries(t *testing.T) {
	t.Run("успех после ошибок", func(t *testing.T) {
		repo := &fakeRepo{}
		repo.set(nil, errors.New("db down"))
		opts := quietOpts()
		opts.InitialAttempts, opts.InitialDelay = 5, 20*time.Millisecond
		c := New(repo, opts)
		go func() {
			eventually(t, time.Second, func() bool { return repo.calls.Load() >= 2 }, "2 попытки")
			repo.set([]model.Experiment{exp("a", 2)}, nil)
		}()
		if err := c.LoadInitial(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !c.Ready() {
			t.Fatal("после LoadInitial Ready() == true")
		}
	})
	t.Run("все попытки неуспешны", func(t *testing.T) {
		repo := &fakeRepo{}
		repo.set(nil, errors.New("db down"))
		opts := quietOpts()
		opts.InitialAttempts, opts.InitialDelay = 3, 5*time.Millisecond
		c := New(repo, opts)
		if err := c.LoadInitial(context.Background()); err == nil {
			t.Fatal("ожидалась ошибка")
		}
		if got := repo.calls.Load(); got != 3 {
			t.Fatalf("ожидалось 3 попытки, получено %d", got)
		}
		if c.Ready() {
			t.Fatal("Ready() должен остаться false")
		}
	})
}

func TestRunPeriodicReload(t *testing.T) {
	repo := &fakeRepo{}
	repo.set([]model.Experiment{exp("a", 2)}, nil)
	opts := quietOpts()
	opts.Interval = 15 * time.Millisecond
	c := New(repo, opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	eventually(t, 2*time.Second, func() bool { return repo.calls.Load() >= 3 }, "3 периодические перезагрузки")
	repo.set([]model.Experiment{exp("b", 2)}, nil)
	eventually(t, 2*time.Second, func() bool { _, ok := c.Snapshot().Get("b"); return ok }, "новые данные по таймеру")
	cancel()
	<-done
}

func TestRunDebounce(t *testing.T) {
	repo := &fakeRepo{}
	repo.set([]model.Experiment{exp("a", 2)}, nil)
	d := newFakeDialer(0)
	opts := quietOpts()
	opts.Listen = d.listen
	opts.Interval = time.Hour
	opts.Debounce = 50 * time.Millisecond
	c := New(repo, opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	l := <-d.connected
	// После подключения — одно полное перечитывание.
	eventually(t, time.Second, func() bool { return repo.calls.Load() == 1 }, "перечитывание после подключения")

	// Пачка уведомлений склеивается в одно перечитывание.
	repo.set([]model.Experiment{exp("b", 2)}, nil)
	for i := 0; i < 20; i++ {
		l.notes <- Notification{Channel: Channel, Payload: `{"op":"INSERT","name":"b"}`}
	}
	eventually(t, time.Second, func() bool { _, ok := c.Snapshot().Get("b"); return ok }, "перечитывание по уведомлению")
	time.Sleep(150 * time.Millisecond)
	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("20 уведомлений должны дать одно перечитывание, всего List=%d (ожидалось 2)", got)
	}
}

func TestRunReconnectAndReload(t *testing.T) {
	repo := &fakeRepo{}
	repo.set([]model.Experiment{exp("a", 2)}, nil)
	d := newFakeDialer(2) // первые две попытки подключения неуспешны
	opts := quietOpts()
	opts.Listen = d.listen
	opts.Interval = time.Hour
	opts.Debounce = 5 * time.Millisecond
	opts.BackoffMin = 5 * time.Millisecond
	opts.BackoffMax = 20 * time.Millisecond
	c := New(repo, opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	l1 := <-d.connected
	if d.attemptsN() != 3 {
		t.Fatalf("ожидалось 3 попытки подключения, было %d", d.attemptsN())
	}
	eventually(t, time.Second, func() bool { return repo.calls.Load() == 1 }, "перечитывание после подключения")

	// Обрыв соединения: слушатель закрывается, переподключение, полное перечитывание.
	repo.set([]model.Experiment{exp("b", 2)}, nil)
	l1.errs <- errors.New("conn reset")
	l2 := <-d.connected
	if l2 == l1 {
		t.Fatal("ожидался новый слушатель")
	}
	eventually(t, time.Second, l1.closed.Load, "закрытие старого слушателя")
	eventually(t, time.Second, func() bool { _, ok := c.Snapshot().Get("b"); return ok }, "перечитывание после переподключения")

	cancel()
	<-done
	if !l2.closed.Load() {
		t.Fatal("при остановке Run слушатель должен закрываться")
	}
}

func TestBackoffCapped(t *testing.T) {
	repo := &fakeRepo{}
	d := newFakeDialer(1 << 30) // никогда не подключается
	opts := quietOpts()
	opts.Listen = d.listen
	opts.Interval = time.Hour
	opts.BackoffMin = time.Millisecond
	opts.BackoffMax = 4 * time.Millisecond
	c := New(repo, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	// Без ограничения сверху за 200 мс было бы ~8 попыток (1+2+4+...+128).
	if got := d.attemptsN(); got < 20 {
		t.Fatalf("задержка не ограничена BackoffMax: всего %d попыток", got)
	}
}
