package clickhouse_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"synaq/internal/sink"
	"synaq/internal/sink/clickhouse"
	"synaq/internal/track"
)

// Таймаут ожидания в тестах; при нормальной работе ожидания укладываются в миллисекунды.
const waitTimeout = 5 * time.Second

type request struct {
	method string
	path   string
	query  map[string][]string
	header http.Header
	body   []byte
}

// fakeCH — тестовый HTTP-сервер, имитирующий ClickHouse. statuses задаёт коды
// ответов на INSERT по порядку; после их исчерпания отвечает 200.
type fakeCH struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []request
	statuses []int

	// block, если не nil, задерживает ответ на INSERT до закрытия канала или отмены запроса.
	block    chan struct{}
	received chan struct{} // сигнал о каждом полученном INSERT
}

func newFakeCH(t *testing.T, statuses ...int) *fakeCH {
	t.Helper()
	f := &fakeCH{statuses: statuses, received: make(chan struct{}, 100)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCH) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ping" {
		f.mu.Lock()
		status := http.StatusOK
		if len(f.statuses) > 0 {
			status = f.statuses[0]
		}
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "Ok.\n")
		return
	}

	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, request{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		header: r.Header.Clone(),
		body:   body,
	})
	status := http.StatusOK
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
	}
	block := f.block
	f.mu.Unlock()

	select {
	case f.received <- struct{}{}:
	default:
	}
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(status)
	if status != http.StatusOK {
		_, _ = io.WriteString(w, "Code: 241. DB::Exception: тестовая ошибка")
	}
}

func (f *fakeCH) snapshot() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func (f *fakeCH) waitReceived(t *testing.T) {
	t.Helper()
	select {
	case <-f.received:
	case <-time.After(waitTimeout):
		t.Fatal("сервер не получил INSERT")
	}
}

// baseConfig — конфигурация с быстрыми повторами; таймер по умолчанию практически
// отключён, чтобы батчи формировались только там, где тест этого ждёт.
func baseConfig(url string) clickhouse.Config {
	return clickhouse.Config{
		URL:            url,
		BufferSize:     100,
		BatchSize:      100,
		FlushInterval:  time.Hour,
		HTTPTimeout:    2 * time.Second,
		MaxAttempts:    3,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  2 * time.Millisecond,
		CloseTimeout:   waitTimeout,
	}
}

func newProducer(t *testing.T, cfg clickhouse.Config) *clickhouse.Producer {
	t.Helper()
	p, err := clickhouse.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// results собирает результаты onDone.
type results struct {
	mu   sync.Mutex
	wg   sync.WaitGroup
	errs []error
}

func (r *results) callback() func(error) {
	r.wg.Add(1)
	return func(err error) {
		r.mu.Lock()
		r.errs = append(r.errs, err)
		r.mu.Unlock()
		r.wg.Done()
	}
}

func (r *results) wait(t *testing.T) []error {
	t.Helper()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("onDone вызван не для всех событий")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errs...)
}

func payload(t *testing.T, i int) []byte {
	t.Helper()
	ev := track.TrackEvent{
		EventID:        "e-" + string(rune('a'+i)),
		UserID:         "u-1",
		EventName:      "purchase",
		Timestamp:      1700000000123,
		ExperimentName: "exp",
		Variant:        "B",
		EventData:      json.RawMessage(`{"amount":42}`),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lines(body []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func publish(t *testing.T, p *clickhouse.Producer, n int, r *results) {
	t.Helper()
	for i := range n {
		if err := p.PublishAsync(context.Background(), "ignored", nil, payload(t, i), r.callback()); err != nil {
			t.Fatalf("PublishAsync #%d: %v", i, err)
		}
	}
}

func TestBatchBySize(t *testing.T) {
	ch := newFakeCH(t)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 3
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 6, &r)
	for i, err := range r.wait(t) {
		if err != nil {
			t.Errorf("onDone #%d: %v", i, err)
		}
	}

	reqs := ch.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("INSERT-запросов %d, want 2", len(reqs))
	}
	for i, req := range reqs {
		if n := len(lines(req.body)); n != 3 {
			t.Errorf("запрос #%d содержит %d строк, want 3", i, n)
		}
	}
}

func TestFlushByInterval(t *testing.T) {
	ch := newFakeCH(t)
	cfg := baseConfig(ch.srv.URL)
	cfg.FlushInterval = 20 * time.Millisecond
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 2, &r)
	for _, err := range r.wait(t) {
		if err != nil {
			t.Errorf("onDone: %v", err)
		}
	}

	reqs := ch.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("INSERT-запросов %d, want 1", len(reqs))
	}
	if n := len(lines(reqs[0].body)); n != 2 {
		t.Errorf("запрос содержит %d строк, want 2", n)
	}
}

func TestRequestFormat(t *testing.T) {
	ch := newFakeCH(t)
	cfg := baseConfig(ch.srv.URL + "/") // завершающий слэш допустим
	cfg.BatchSize = 2
	cfg.User = "tracker"
	cfg.Password = "test-password"
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 2, &r)
	r.wait(t)

	reqs := ch.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("INSERT-запросов %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost || req.path != "/" {
		t.Errorf("запрос %s %s, want POST /", req.method, req.path)
	}

	wantQuery := map[string]string{
		"query":                           "INSERT INTO tracking_events FORMAT JSONEachRow",
		"async_insert":                    "1",
		"wait_for_async_insert":           "1",
		"input_format_null_as_default":    "1",
		"input_format_allow_errors_num":   "10",
		"input_format_allow_errors_ratio": "0.01",
	}
	for k, want := range wantQuery {
		if got := req.query[k]; len(got) != 1 || got[0] != want {
			t.Errorf("параметр %s = %v, want %q", k, got, want)
		}
	}
	if len(req.query) != len(wantQuery) {
		t.Errorf("лишние параметры запроса: %v", req.query)
	}
	if got := req.header.Get("X-ClickHouse-User"); got != "tracker" {
		t.Errorf("X-ClickHouse-User = %q", got)
	}
	if got := req.header.Get("X-ClickHouse-Key"); got != "test-password" {
		t.Errorf("X-ClickHouse-Key = %q", got)
	}

	// Тело — JSONEachRow: одна JSON-строка на событие, с именами колонок tracking_events.
	if !bytes.HasSuffix(req.body, []byte("\n")) {
		t.Error("тело должно заканчиваться переводом строки")
	}
	rows := lines(req.body)
	if len(rows) != 2 {
		t.Fatalf("строк %d, want 2", len(rows))
	}
	wantKeys := []string{"event_id", "user_id", "event_name", "timestamp", "experiment_name", "variant", "event_data"}
	for i, row := range rows {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(row), &obj); err != nil {
			t.Fatalf("строка #%d не JSON: %v: %s", i, err, row)
		}
		if len(obj) != len(wantKeys) {
			t.Errorf("строка #%d: %d ключей, want %d: %s", i, len(obj), len(wantKeys), row)
		}
		for _, k := range wantKeys {
			if _, ok := obj[k]; !ok {
				t.Errorf("строка #%d: нет ключа %q", i, k)
			}
		}
		if string(obj["timestamp"]) != "1700000000123" {
			t.Errorf("timestamp = %s, want миллисекунды 1700000000123", obj["timestamp"])
		}
	}
}

func TestNoAuthHeadersWhenEmpty(t *testing.T) {
	ch := newFakeCH(t)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 1
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 1, &r)
	r.wait(t)

	req := ch.snapshot()[0]
	for _, h := range []string{"X-ClickHouse-User", "X-ClickHouse-Key"} {
		if _, ok := req.header[http.CanonicalHeaderKey(h)]; ok {
			t.Errorf("заголовок %s не должен передаваться", h)
		}
	}
}

func TestRetryThenSuccess(t *testing.T) {
	ch := newFakeCH(t, http.StatusInternalServerError, http.StatusServiceUnavailable)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 2
	cfg.MaxAttempts = 5
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 2, &r)
	for _, err := range r.wait(t) {
		if err != nil {
			t.Errorf("onDone: %v, want nil после повтора", err)
		}
	}
	reqs := ch.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("INSERT-запросов %d, want 3 (2 ошибки + успех)", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		if !bytes.Equal(reqs[i].body, reqs[0].body) {
			t.Errorf("повтор #%d отправил другое тело", i)
		}
	}
}

func TestFinalFailureCallsOnDone(t *testing.T) {
	ch := newFakeCH(t, 500, 500, 500, 500, 500)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 3
	cfg.MaxAttempts = 3
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 3, &r)
	errs := r.wait(t)
	if len(errs) != 3 {
		t.Fatalf("onDone вызван %d раз, want 3", len(errs))
	}
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("onDone #%d: %v, want ошибку со статусом 500", i, err)
		}
	}
	if n := len(ch.snapshot()); n != 3 {
		t.Errorf("INSERT-запросов %d, want 3 (MaxAttempts)", n)
	}
}

func TestClientErrorNotRetried(t *testing.T) {
	ch := newFakeCH(t, http.StatusBadRequest)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 1
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 1, &r)
	if errs := r.wait(t); errs[0] == nil {
		t.Error("onDone: nil, want ошибку")
	}
	if n := len(ch.snapshot()); n != 1 {
		t.Errorf("INSERT-запросов %d, want 1: 4xx не повторяется", n)
	}
}

func TestBufferFull(t *testing.T) {
	ch := newFakeCH(t)
	ch.block = make(chan struct{})
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 1
	cfg.BufferSize = 2
	p := newProducer(t, cfg)

	var r results
	// Первое событие забирает фоновый горутин и зависает на HTTP-запросе.
	publish(t, p, 1, &r)
	ch.waitReceived(t)

	// Два события заполняют буфер, третье отклоняется сразу, без блокировки.
	publish(t, p, 2, &r)
	start := time.Now()
	err := p.PublishAsync(context.Background(), "", nil, payload(t, 9), func(error) {
		t.Error("onDone не должен вызываться для отклонённого события")
	})
	if !errors.Is(err, sink.ErrBufferFull) {
		t.Fatalf("PublishAsync = %v, want sink.ErrBufferFull", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("PublishAsync заблокировался на %v", d)
	}

	close(ch.block)
	for _, err := range r.wait(t) {
		if err != nil {
			t.Errorf("onDone: %v", err)
		}
	}
}

func TestBufferBytesLimit(t *testing.T) {
	ch := newFakeCH(t)
	ch.block = make(chan struct{})
	ev := payload(t, 0)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 1
	cfg.BufferSize = 100
	// Лимит байт вмещает ровно два события, хотя очередь рассчитана на 100.
	cfg.MaxBufferedBytes = 2 * len(ev)
	p := newProducer(t, cfg)

	var r results
	// Первое событие зависает на HTTP-запросе и продолжает занимать лимит до конца записи.
	publish(t, p, 1, &r)
	ch.waitReceived(t)
	publish(t, p, 1, &r)

	err := p.PublishAsync(context.Background(), "", nil, ev, func(error) {
		t.Error("onDone не должен вызываться для отклонённого события")
	})
	if !errors.Is(err, sink.ErrBufferFull) {
		t.Fatalf("PublishAsync сверх лимита байт = %v, want sink.ErrBufferFull", err)
	}

	close(ch.block)
	for _, err := range r.wait(t) {
		if err != nil {
			t.Errorf("onDone: %v", err)
		}
	}
	// После записи место освобождается.
	var r2 results
	publish(t, p, 1, &r2)
	r2.wait(t)
}

func TestCloseFlushesPending(t *testing.T) {
	ch := newFakeCH(t)
	p := newProducer(t, baseConfig(ch.srv.URL)) // батч 100, таймер 1 ч: сам батч не отправится

	var r results
	publish(t, p, 3, &r)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, err := range r.wait(t) {
		if err != nil {
			t.Errorf("onDone: %v", err)
		}
	}
	reqs := ch.snapshot()
	if len(reqs) != 1 || len(lines(reqs[0].body)) != 3 {
		t.Fatalf("после Close ожидался один INSERT с 3 строками, получено %d запросов", len(reqs))
	}

	if err := p.PublishAsync(context.Background(), "", nil, payload(t, 0), nil); !errors.Is(err, sink.ErrClosed) {
		t.Errorf("PublishAsync после Close = %v, want sink.ErrClosed", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("повторный Close: %v", err)
	}
}

func TestCloseDeadline(t *testing.T) {
	ch := newFakeCH(t)
	ch.block = make(chan struct{}) // сервер не отвечает, пока запрос не отменён
	cfg := baseConfig(ch.srv.URL)
	cfg.CloseTimeout = 50 * time.Millisecond
	p := newProducer(t, cfg)

	var r results
	publish(t, p, 2, &r)
	if err := p.Close(); err == nil {
		t.Error("Close: nil, want ошибку по таймауту")
	}
	for _, err := range r.wait(t) {
		if err == nil {
			t.Error("onDone: nil, want ошибку для неотправленного события")
		}
	}
}

func TestPublishSync(t *testing.T) {
	ch := newFakeCH(t, http.StatusBadRequest)
	cfg := baseConfig(ch.srv.URL)
	cfg.BatchSize = 1
	p := newProducer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := p.Publish(ctx, "", nil, payload(t, 0)); err == nil {
		t.Error("Publish: nil, want ошибку 400")
	}
	if err := p.Publish(ctx, "", nil, payload(t, 1)); err != nil {
		t.Errorf("Publish: %v", err)
	}
}

func TestPing(t *testing.T) {
	ok := newFakeCH(t)
	p := newProducer(t, baseConfig(ok.srv.URL))
	if err := p.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}

	bad := newFakeCH(t, http.StatusInternalServerError)
	p = newProducer(t, baseConfig(bad.srv.URL))
	if err := p.Ping(context.Background()); err == nil {
		t.Error("Ping: nil, want ошибку при статусе 500")
	}

	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	p = newProducer(t, baseConfig(url))
	if err := p.Ping(context.Background()); err == nil {
		t.Error("Ping: nil, want ошибку для недоступного сервера")
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{
		"",
		"clickhouse:8123",
		"ftp://clickhouse:8123",
		"http://user:secret@clickhouse:8123",
		"http://user:p%zzsecret@clickhouse:8123", // url.Parse падает, ошибка не должна содержать адрес
		"http://clickhouse:8123/?database=x",
	} {
		if p, err := clickhouse.New(clickhouse.Config{URL: u}); err == nil {
			_ = p.Close()
			t.Errorf("New(%q): nil, want ошибку", u)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("New(%q): пароль попал в текст ошибки: %v", u, err)
		}
	}
}
