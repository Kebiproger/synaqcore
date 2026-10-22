package configcache

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"synaq/internal/model"
)

// Значения Options по умолчанию.
const (
	DefaultInterval        = 30 * time.Second
	DefaultDebounce        = 50 * time.Millisecond
	DefaultBackoffMin      = 250 * time.Millisecond
	DefaultBackoffMax      = 30 * time.Second
	DefaultReloadTimeout   = 5 * time.Second
	DefaultInitialAttempts = 5
	DefaultInitialDelay    = 2 * time.Second
)

// Options — настройки кэша. Нулевые значения заменяются значениями по умолчанию.
type Options struct {
	// Listen открывает выделенное соединение, подписанное на канал уведомлений
	// (см. PgListener). nil — без LISTEN, только периодическое перечитывание.
	Listen func(ctx context.Context) (Listener, error)
	// Interval — период полного перечитывания (по умолчанию 30 с).
	Interval time.Duration
	// Debounce — окно склейки уведомлений перед Reload (по умолчанию 50 мс).
	Debounce time.Duration
	// BackoffMin/BackoffMax — границы экспоненциальной задержки переподключения
	// слушателя (по умолчанию 250 мс и 30 с).
	BackoffMin time.Duration
	BackoffMax time.Duration
	// ReloadTimeout — таймаут одного обращения к репозиторию (по умолчанию 5 с).
	// Загрузка не зависит от контекста конкретного вызывающего: её результат
	// разделяют все ожидающие.
	ReloadTimeout time.Duration
	// InitialAttempts/InitialDelay — попытки LoadInitial (по умолчанию 5 × 2 с).
	InitialAttempts int
	InitialDelay    time.Duration
	// Logf — логгер (по умолчанию log.Printf).
	Logf func(format string, args ...any)
}

func (o *Options) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Debounce <= 0 {
		o.Debounce = DefaultDebounce
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = DefaultBackoffMin
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = DefaultBackoffMax
	}
	if o.BackoffMax < o.BackoffMin {
		o.BackoffMax = o.BackoffMin
	}
	if o.ReloadTimeout <= 0 {
		o.ReloadTimeout = DefaultReloadTimeout
	}
	if o.InitialAttempts <= 0 {
		o.InitialAttempts = DefaultInitialAttempts
	}
	if o.InitialDelay <= 0 {
		o.InitialDelay = DefaultInitialDelay
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// flight — одна загрузка из репозитория; её результат получают все ожидающие.
type flight struct {
	done chan struct{}
	err  error
}

// Cache — реализация model.Cache.
type Cache struct {
	repo model.Repository
	opts Options

	snap atomic.Pointer[snapshot]

	mu      sync.Mutex
	running *flight // загрузка, которая уже читает репозиторий
	pending *flight // следующая загрузка, к которой присоединяются новые вызовы
}

var _ model.Cache = (*Cache)(nil)

// New создаёт кэш. До первой успешной загрузки Snapshot() пуст, а Ready() == false.
func New(repo model.Repository, opts Options) *Cache {
	opts.withDefaults()
	return &Cache{repo: repo, opts: opts}
}

// Snapshot возвращает текущий снимок: один atomic.Load, без блокировок.
func (c *Cache) Snapshot() model.Snapshot {
	if s := c.snap.Load(); s != nil {
		return s
	}
	return emptySnapshot
}

// Ready сообщает, загружен ли хотя бы один снимок (для /readyz).
func (c *Cache) Ready() bool {
	return c.snap.Load() != nil
}

// Reload перечитывает все эксперименты и атомарно заменяет снимок.
//
// Конкурентные вызовы склеиваются, но с гарантией свежести: если загрузка уже
// идёт, её List мог стартовать раньше коммита вызывающего, поэтому вызов
// присоединяется к СЛЕДУЮЩЕЙ загрузке, общей для всех, кто пришёл во время
// текущей. Итого N конкурентных вызовов дают не больше двух обращений к
// репозиторию, а вернувшийся без ошибки Reload гарантирует, что снимок
// прочитан после его начала (read-your-writes для админских ручек).
//
// При ошибке снимок не меняется (продолжает раздаваться устаревший).
// Отмена ctx прекращает только ожидание, но не саму загрузку.
func (c *Cache) Reload(ctx context.Context) error {
	c.mu.Lock()
	var f *flight
	switch {
	case c.running == nil:
		f = &flight{done: make(chan struct{})}
		c.running = f
		go c.fly(f)
	case c.pending != nil:
		f = c.pending
	default:
		f = &flight{done: make(chan struct{})}
		c.pending = f
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fly выполняет загрузку f, а затем, пока есть отложенные, — следующие.
func (c *Cache) fly(f *flight) {
	for f != nil {
		f.err = c.load()
		close(f.done)

		c.mu.Lock()
		f = c.pending
		c.pending = nil
		c.running = f
		c.mu.Unlock()
	}
}

func (c *Cache) load() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.ReloadTimeout)
	defer cancel()
	exps, err := c.repo.List(ctx)
	if err != nil {
		c.opts.Logf("configcache: не удалось перечитать эксперименты (раздаётся прежний снимок): %v", err)
		return fmt.Errorf("configcache: перечитывание: %w", err)
	}
	c.snap.Store(newSnapshot(exps))
	return nil
}

// LoadInitial выполняет первую загрузку с повторами (Options.InitialAttempts
// попыток с паузой Options.InitialDelay). Ошибка означает, что сервис не может
// стартовать (main завершается).
func (c *Cache) LoadInitial(ctx context.Context) error {
	var err error
	for attempt := 1; attempt <= c.opts.InitialAttempts; attempt++ {
		if err = c.Reload(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == c.opts.InitialAttempts {
			break
		}
		c.opts.Logf("configcache: начальная загрузка, попытка %d/%d не удалась, повтор через %s",
			attempt, c.opts.InitialAttempts, c.opts.InitialDelay)
		t := time.NewTimer(c.opts.InitialDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return fmt.Errorf("configcache: начальная загрузка не удалась после %d попыток: %w",
		c.opts.InitialAttempts, err)
}

// Run — фоновый цикл обновления снимка; блокируется до отмены ctx.
// Перечитывает снимок по уведомлениям (с дебаунсом), периодически и после
// каждого (пере)подключения слушателя. Ошибки только логируются.
func (c *Cache) Run(ctx context.Context) {
	notify := make(chan struct{}, 1)
	var wg sync.WaitGroup
	if c.opts.Listen != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.listenLoop(ctx, notify)
		}()
	}
	defer wg.Wait()

	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()

	// Дебаунс: первое уведомление взводит таймер, последующие в пределах окна
	// склеиваются; окно не продлевается, чтобы задержка была ограничена.
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Reload(ctx)
		case <-notify:
			if debounceC == nil {
				if debounce == nil {
					debounce = time.NewTimer(c.opts.Debounce)
				} else {
					debounce.Reset(c.opts.Debounce)
				}
				debounceC = debounce.C
			}
		case <-debounceC:
			debounceC = nil
			_ = c.Reload(ctx)
		}
	}
}

// signal неблокирующе ставит отметку «нужно перечитать».
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// listenLoop держит подписку LISTEN: при ошибке закрывает соединение и
// переподключается с экспоненциальной задержкой; после каждого успешного
// подключения запрашивает полное перечитывание (уведомления, пришедшие пока
// соединения не было, потеряны).
func (c *Cache) listenLoop(ctx context.Context, notify chan<- struct{}) {
	backoff := c.opts.BackoffMin
	wait := func() bool {
		t := time.NewTimer(backoff)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
		backoff = min(backoff*2, c.opts.BackoffMax)
		return true
	}

	for ctx.Err() == nil {
		l, err := c.opts.Listen(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.opts.Logf("configcache: не удалось подписаться на уведомления, повтор через %s: %v", backoff, err)
			if !wait() {
				return
			}
			continue
		}
		backoff = c.opts.BackoffMin
		signal(notify)

		for {
			_, err = l.WaitForNotification(ctx)
			if err != nil {
				break
			}
			signal(notify)
		}
		closeListener(l)
		if ctx.Err() != nil {
			return
		}
		c.opts.Logf("configcache: соединение LISTEN потеряно, переподключение через %s: %v", backoff, err)
		if !wait() {
			return
		}
	}
}

func closeListener(l Listener) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.Close(ctx)
}
