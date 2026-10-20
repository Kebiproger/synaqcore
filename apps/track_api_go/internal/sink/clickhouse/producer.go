// Package clickhouse реализует sink.Producer с прямой записью событий в ClickHouse
// по HTTP-интерфейсу (lite-режим без Kafka/Redpanda).
//
// События копятся в ограниченном буфере и отправляются фоновым горутином батчами
// (по размеру или по таймеру) запросом INSERT ... FORMAT JSONEachRow. Формат строки
// совпадает с сообщением, которое в полном режиме уходит в Kafka, поэтому таблица
// tracking_events получает одинаковые данные в обоих режимах.
package clickhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"track_api_go/internal/sink"
)

// Проверка на этапе компиляции: Producer реализует sink.Producer и sink.Pinger.
var (
	_ sink.Producer = (*Producer)(nil)
	_ sink.Pinger   = (*Producer)(nil)
)

// insertQuery фиксирован: имя таблицы не берётся из конфигурации, чтобы исключить подстановку SQL.
const insertQuery = "INSERT INTO tracking_events FORMAT JSONEachRow"

// Значения по умолчанию.
const (
	DefaultBufferSize = 10000
	// DefaultMaxBufferedBytes — лимит байт в очереди и отправляемом батче, 64 МиБ. Без него
	// 10000 событий по ~17 КБ (максимальный размер после валидации) заняли бы ~170 МБ.
	DefaultMaxBufferedBytes = 64 << 20
	DefaultBatchSize        = 1000
	DefaultFlushInterval    = time.Second
	DefaultHTTPTimeout      = 10 * time.Second
	DefaultMaxAttempts      = 5
	DefaultRetryBaseDelay   = 200 * time.Millisecond
	DefaultRetryMaxDelay    = 3 * time.Second
	DefaultCloseTimeout     = 5 * time.Second
)

// Максимум байт тела ответа с ошибкой, который попадает в лог.
const maxErrorBody = 1 << 10

// Config — настройки продюсера. Нулевые значения заменяются значениями по умолчанию.
type Config struct {
	URL      string // базовый адрес HTTP-интерфейса ClickHouse, например http://clickhouse:8123
	User     string // передаётся в X-ClickHouse-User, если не пустой
	Password string // передаётся в X-ClickHouse-Key, если не пустой

	BufferSize       int           // ёмкость очереди событий
	MaxBufferedBytes int           // лимит байт в очереди и ещё не записанных батчах
	BatchSize        int           // максимальное число событий в одном INSERT
	FlushInterval    time.Duration // максимальное время ожидания неполного батча

	HTTPTimeout    time.Duration // таймаут одного HTTP-запроса
	MaxAttempts    int           // число попыток отправки батча (включая первую)
	RetryBaseDelay time.Duration // пауза перед второй попыткой, далее удваивается
	RetryMaxDelay  time.Duration // верхняя граница паузы между попытками
	CloseTimeout   time.Duration // сколько Close ждёт отправки оставшихся событий

	HTTPClient *http.Client // необязательно; по умолчанию создаётся с HTTPTimeout
}

func (c *Config) setDefaults() {
	if c.BufferSize <= 0 {
		c.BufferSize = DefaultBufferSize
	}
	if c.MaxBufferedBytes <= 0 {
		c.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = DefaultHTTPTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if c.RetryMaxDelay < c.RetryBaseDelay {
		c.RetryMaxDelay = max(DefaultRetryMaxDelay, c.RetryBaseDelay)
	}
	if c.CloseTimeout <= 0 {
		c.CloseTimeout = DefaultCloseTimeout
	}
}

type item struct {
	payload []byte
	onDone  func(error)
}

// Producer пишет события в ClickHouse батчами.
type Producer struct {
	cfg       Config
	client    *http.Client
	insertURL string
	pingURL   string

	mu     sync.RWMutex // защищает closed и закрытие queue
	closed bool
	queue  chan item

	// bufferedBytes — размер событий, принятых PublishAsync и ещё не записанных
	// (в очереди или в отправляемом батче).
	bufferedBytes atomic.Int64

	// stopCtx отменяется, если Close не уложился в CloseTimeout: прерывает повторы.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	done       chan struct{} // закрывается, когда фоновый горутин завершился
	closeOnce  sync.Once
	closeErr   error
}

// New создаёт продюсер и запускает фоновую отправку. Доступность ClickHouse
// не проверяется: для этого есть Ping.
func New(cfg Config) (*Producer, error) {
	cfg.setDefaults()

	base, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil {
		// Ошибку url.Parse не оборачиваем: она содержит исходный адрес вместе с паролем, если он там есть.
		return nil, errors.New("некорректный адрес ClickHouse (CLICKHOUSE_URL)")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("адрес ClickHouse должен начинаться с http:// или https://")
	}
	if base.Host == "" {
		return nil, fmt.Errorf("в адресе ClickHouse не указан хост")
	}
	// Учётные данные в URL попали бы в тексты ошибок и логи.
	if base.User != nil {
		return nil, fmt.Errorf("не указывайте логин и пароль в адресе ClickHouse, используйте CLICKHOUSE_USER и CLICKHOUSE_PASSWORD")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("адрес ClickHouse не должен содержать параметры запроса")
	}

	params := url.Values{}
	params.Set("query", insertQuery)
	// Асинхронная вставка на стороне ClickHouse с ожиданием записи: ответ 200 означает,
	// что данные записаны, а мелкие батчи нескольких реплик трекера объединяются сервером.
	params.Set("async_insert", "1")
	params.Set("wait_for_async_insert", "1")
	// null (например, отсутствующий event_data) превращается в значение по умолчанию ('').
	params.Set("input_format_null_as_default", "1")
	// Одна строка, которую ClickHouse не смог разобрать, иначе отклонит весь батч (4xx без повторов),
	// и потеряются все события батча. Допускаем немного битых строк: они пропускаются, остальные пишутся.
	params.Set("input_format_allow_errors_num", "10")
	params.Set("input_format_allow_errors_ratio", "0.01")

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.HTTPTimeout}
	}

	stopCtx, stopCancel := context.WithCancel(context.Background())
	p := &Producer{
		cfg:        cfg,
		client:     client,
		insertURL:  base.String() + "/?" + params.Encode(),
		pingURL:    base.String() + "/ping",
		queue:      make(chan item, cfg.BufferSize),
		stopCtx:    stopCtx,
		stopCancel: stopCancel,
		done:       make(chan struct{}),
	}
	go p.run()
	return p, nil
}

// PublishAsync ставит событие в очередь и никогда не блокируется:
// при заполненном буфере сразу возвращает sink.ErrBufferFull.
// Аргументы ctx, topic и key не используются. payload должен быть одной JSON-строкой
// без переводов строк (результат json.Marshal).
func (p *Producer) PublishAsync(_ context.Context, _ string, _, payload []byte, onDone func(error)) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return sink.ErrClosed
	}
	if !p.reserve(int64(len(payload))) {
		return sink.ErrBufferFull
	}
	select {
	case p.queue <- item{payload: payload, onDone: onDone}:
		return nil
	default:
		p.bufferedBytes.Add(-int64(len(payload)))
		return sink.ErrBufferFull
	}
}

// reserve учитывает size байт в лимите MaxBufferedBytes; false, если лимит исчерпан.
func (p *Producer) reserve(size int64) bool {
	limit := int64(p.cfg.MaxBufferedBytes)
	for {
		cur := p.bufferedBytes.Load()
		if cur+size > limit {
			return false
		}
		if p.bufferedBytes.CompareAndSwap(cur, cur+size) {
			return true
		}
	}
}

// Publish ставит событие в очередь и ждёт результата записи его батча.
func (p *Producer) Publish(ctx context.Context, topic string, key, payload []byte) error {
	result := make(chan error, 1)
	if err := p.PublishAsync(ctx, topic, key, payload, func(err error) { result <- err }); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ping проверяет доступность ClickHouse через GET /ping.
func (p *Producer) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.pingURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("ClickHouse недоступен: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ClickHouse /ping вернул статус %d", resp.StatusCode)
	}
	return nil
}

// Close перестаёт принимать события, отправляет оставшиеся и ждёт завершения
// не дольше CloseTimeout. Если время вышло, повторы прерываются, а события,
// которые не удалось записать, получают ошибку в onDone. Повторный вызов безопасен.
func (p *Producer) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.queue)
		p.mu.Unlock()

		timer := time.NewTimer(p.cfg.CloseTimeout)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			p.stopCancel()
			<-p.done
			p.closeErr = fmt.Errorf("не удалось отправить все события в ClickHouse за %v", p.cfg.CloseTimeout)
		}
		p.stopCancel()
	})
	return p.closeErr
}

// run — фоновый цикл: копит батч и отправляет его по размеру или по таймеру.
func (p *Producer) run() {
	defer close(p.done)

	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]item, 0, p.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.flush(batch)
		batch = make([]item, 0, p.cfg.BatchSize)
	}

	for {
		select {
		case it, ok := <-p.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, it)
			if len(batch) >= p.cfg.BatchSize {
				flush()
				ticker.Reset(p.cfg.FlushInterval)
			}
		case <-ticker.C:
			flush()
		}
	}
}

// flush отправляет батч с повторами и сообщает результат каждому событию.
func (p *Producer) flush(batch []item) {
	var body bytes.Buffer
	var size int64
	for _, it := range batch {
		body.Write(it.payload)
		body.WriteByte('\n')
		size += int64(len(it.payload))
	}
	// Место в лимите освобождается после записи батча (успешной или окончательно неудачной).
	defer p.bufferedBytes.Add(-size)

	err := p.sendWithRetry(body.Bytes())
	if err != nil {
		log.Printf("Не удалось записать в ClickHouse батч из %d событий: %v", len(batch), err)
	}
	for _, it := range batch {
		if it.onDone != nil {
			it.onDone(err)
		}
	}
}

func (p *Producer) sendWithRetry(body []byte) error {
	delay := p.cfg.RetryBaseDelay
	var err error
	for attempt := 1; attempt <= p.cfg.MaxAttempts; attempt++ {
		var retryable bool
		retryable, err = p.send(body)
		if err == nil {
			return nil
		}
		if !retryable || attempt == p.cfg.MaxAttempts {
			break
		}
		log.Printf("Ошибка записи в ClickHouse (попытка %d из %d), повтор через %v: %v",
			attempt, p.cfg.MaxAttempts, delay, err)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-p.stopCtx.Done():
			timer.Stop()
			return fmt.Errorf("отправка прервана при остановке: %w", err)
		}
		delay = min(delay*2, p.cfg.RetryMaxDelay)
	}
	return err
}

// send выполняет один INSERT. retryable сообщает, имеет ли смысл повторять запрос:
// сетевые ошибки, 5xx и 429 повторяются, остальные 4xx (ошибка данных или прав) — нет.
func (p *Producer) send(body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(p.stopCtx, http.MethodPost, p.insertURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if p.cfg.User != "" {
		req.Header.Set("X-ClickHouse-User", p.cfg.User)
	}
	if p.cfg.Password != "" {
		req.Header.Set("X-ClickHouse-Key", p.cfg.Password)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return false, err
		}
		return true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}

	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	err = fmt.Errorf("ClickHouse вернул статус %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	return retryable, err
}
