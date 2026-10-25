// Package clickhouse реализует sink.Producer с прямой записью событий в ClickHouse
// по HTTP-интерфейсу (без брокера сообщений).
//
// События копятся в ограниченном буфере и отправляются фоновым горутином батчами
// (по размеру или по таймеру; общая механика — пакет internal/sink/batch) запросом INSERT ... FORMAT JSONEachRow. Строка батча —
// JSON-сериализация track.TrackEvent, её поля совпадают со столбцами tracking_events.
package clickhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"synaq/internal/sink"
	"synaq/internal/sink/batch"
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
	DefaultMaxAttempts      = batch.DefaultMaxAttempts
	DefaultRetryBaseDelay   = batch.DefaultRetryBaseDelay
	DefaultRetryMaxDelay    = batch.DefaultRetryMaxDelay
	DefaultCloseTimeout     = batch.DefaultCloseTimeout
)

// Максимум байт тела ответа, который читается (ping) или разбирается в поисках кода ошибки.
const maxErrorBody = 1 << 10

// errorCodePrefixLen — сколько байт начала тела ответа читается ради префикса "Code: NNN.".
const errorCodePrefixLen = 32

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

// Producer пишет события в ClickHouse батчами (очередь и повторы — в пакете batch).
type Producer struct {
	cfg       Config
	client    *http.Client
	insertURL string
	pingURL   string
	batcher   *batch.Batcher
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

	p := &Producer{
		cfg:       cfg,
		client:    client,
		insertURL: base.String() + "/?" + params.Encode(),
		pingURL:   base.String() + "/ping",
	}
	p.batcher = batch.New(batch.Config{
		Name:             "ClickHouse",
		BufferSize:       cfg.BufferSize,
		MaxBufferedBytes: cfg.MaxBufferedBytes,
		BatchSize:        cfg.BatchSize,
		FlushInterval:    cfg.FlushInterval,
		MaxAttempts:      cfg.MaxAttempts,
		RetryBaseDelay:   cfg.RetryBaseDelay,
		RetryMaxDelay:    cfg.RetryMaxDelay,
		CloseTimeout:     cfg.CloseTimeout,
	}, p.write)
	return p, nil
}

// PublishAsync ставит событие в очередь и никогда не блокируется:
// при заполненном буфере сразу возвращает sink.ErrBufferFull.
// Аргументы ctx, topic и key не используются. payload должен быть одной JSON-строкой
// без переводов строк (результат json.Marshal).
func (p *Producer) PublishAsync(_ context.Context, _ string, _, payload []byte, onDone func(error)) error {
	return p.batcher.Enqueue(payload, onDone)
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
	return p.batcher.Close()
}

// write — batch.Writer: собирает тело JSONEachRow (одна строка на событие) и выполняет INSERT.
func (p *Producer) write(ctx context.Context, payloads [][]byte) (bool, error) {
	var body bytes.Buffer
	for _, pl := range payloads {
		body.Write(pl)
		body.WriteByte('\n')
	}
	return p.send(ctx, body.Bytes())
}

// send выполняет один INSERT. retryable сообщает, имеет ли смысл повторять запрос:
// сетевые ошибки, 5xx и 429 повторяются, остальные 4xx (ошибка данных или прав) — нет.
func (p *Producer) send(ctx context.Context, body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.insertURL, bytes.NewReader(body))
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

	// Текст ошибки ClickHouse может цитировать данные событий (PII, переводы строк для
	// подделки логов), поэтому в лог идут только статус и числовой код ошибки.
	prefix, _ := io.ReadAll(io.LimitReader(resp.Body, errorCodePrefixLen))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	err = fmt.Errorf("ClickHouse вернул статус %d (код ошибки ClickHouse: %s)", resp.StatusCode,
		errorCode(resp.Header.Get("X-ClickHouse-Exception-Code"), prefix))
	retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	return retryable, err
}

// errorCode возвращает числовой код ошибки ClickHouse: из заголовка
// X-ClickHouse-Exception-Code, иначе из префикса тела "Code: NNN.". Возвращаются
// только цифры (не больше 6), остальной текст отбрасывается; без кода — "неизвестен".
func errorCode(header string, body []byte) string {
	if code := leadingDigits(strings.TrimSpace(header)); code != "" {
		return code
	}
	if rest, ok := strings.CutPrefix(string(body), "Code: "); ok {
		if code := leadingDigits(rest); code != "" {
			return code
		}
	}
	return "неизвестен"
}

// leadingDigits возвращает ведущие ASCII-цифры s (не больше 6).
func leadingDigits(s string) string {
	n := 0
	for n < len(s) && n < 6 && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return s[:n]
}
