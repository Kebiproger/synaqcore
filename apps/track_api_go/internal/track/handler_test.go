package track

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"track_api_go/internal/sink"

	"github.com/gin-gonic/gin"
)

type published struct {
	topic   string
	key     []byte
	payload []byte
}

// fakeProducer — тестовая реализация sink.Producer, запоминающая сообщения.
// enqueueErr имитирует отказ постановки в очередь (например, sink.ErrBufferFull).
type fakeProducer struct {
	mu         sync.Mutex
	msgs       []published
	enqueueErr error
}

func (f *fakeProducer) Publish(_ context.Context, topic string, key, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, published{topic, key, payload})
	return nil
}

func (f *fakeProducer) PublishAsync(ctx context.Context, topic string, key, payload []byte, onDone func(error)) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	err := f.Publish(ctx, topic, key, payload)
	if onDone != nil {
		onDone(err)
	}
	return nil
}

func (f *fakeProducer) Close() error { return nil }

const testTopic = "test_topic"

// testNowMs — фиксированное «текущее» время сервера в тестах (мс Unix).
const testNowMs int64 = 1700000000000

var testNow = time.UnixMilli(testNowMs)

func fixedClock() time.Time { return testNow }

func newTestRouter(p *fakeProducer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/tracker/events", NewHandler(p, testTopic).WithClock(fixedClock).HandleTrack)
	return r
}

func doRequest(r *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tracker/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleTrack_Valid(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantTimestamp int64 // 0 — ожидается текущее время сервера (testNowMs)
	}{
		{
			name: "все поля",
			body: `{"event_id":"e-1","user_id":"u-1","event_name":"purchase","timestamp":1700000000000,` +
				`"experiment_name":"exp","variant":"B","event_data":{"amount":42}}`,
			wantTimestamp: 1700000000000,
		},
		{
			name: "только обязательные поля",
			body: `{"user_id":"u-1","event_name":"purchase"}`,
		},
		{
			name:          "поля на границе лимита",
			body:          `{"user_id":"` + strings.Repeat("я", MaxFieldLen) + `","event_name":"purchase","timestamp":1699999999000}`,
			wantTimestamp: 1699999999000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProducer{}
			w := doRequest(newTestRouter(p), tt.body)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if len(p.msgs) != 1 {
				t.Fatalf("producer получил %d сообщений, want 1", len(p.msgs))
			}
			msg := p.msgs[0]
			if msg.topic != testTopic {
				t.Errorf("topic = %q, want %q", msg.topic, testTopic)
			}

			var in, out map[string]any
			if err := json.Unmarshal([]byte(tt.body), &in); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(msg.payload, &out); err != nil {
				t.Fatalf("payload не JSON: %v", err)
			}
			if string(msg.key) != in["user_id"] {
				t.Errorf("key = %q, want user_id %q", msg.key, in["user_id"])
			}

			// Формат сообщения должен совпадать с ожидаемым ClickHouse.
			wantKeys := []string{"event_id", "user_id", "event_name", "timestamp", "experiment_name", "variant", "event_data"}
			if len(out) != len(wantKeys) {
				t.Errorf("payload содержит %d ключей, want %d: %s", len(out), len(wantKeys), msg.payload)
			}
			for _, k := range wantKeys {
				if _, ok := out[k]; !ok {
					t.Errorf("в payload нет ключа %q", k)
				}
			}
			for _, k := range []string{"event_id", "user_id", "event_name", "experiment_name", "variant"} {
				want, _ := in[k].(string)
				if k == "event_id" && want == "" {
					// Без event_id от клиента сервер генерирует UUIDv4 (см. TestHandleTrack_EventID).
					continue
				}
				if out[k] != want {
					t.Errorf("%s = %v, want %q", k, out[k], want)
				}
			}

			ts, _ := out["timestamp"].(float64)
			if tt.wantTimestamp != 0 && int64(ts) != tt.wantTimestamp {
				t.Errorf("timestamp = %v, want %d", ts, tt.wantTimestamp)
			}
			if tt.wantTimestamp == 0 && int64(ts) != testNowMs {
				t.Errorf("timestamp = %v, ожидалось время сервера %d", ts, testNowMs)
			}

			if want, ok := in["event_data"]; ok {
				gotData, _ := json.Marshal(out["event_data"])
				wantData, _ := json.Marshal(want)
				if string(gotData) != string(wantData) {
					t.Errorf("event_data = %s, want %s", gotData, wantData)
				}
			}
		})
	}
}

func TestHandleTrack_Invalid(t *testing.T) {
	long := strings.Repeat("a", MaxFieldLen+1)
	field := func(name string) string {
		return `{"user_id":"u","event_name":"e","` + name + `":"` + long + `"}`
	}

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"битый JSON", `{"user_id":"u",`, http.StatusBadRequest},
		{"не JSON", `hello`, http.StatusBadRequest},
		{"пустое тело", ``, http.StatusBadRequest},
		{"нет user_id", `{"event_name":"e"}`, http.StatusBadRequest},
		{"нет event_name", `{"user_id":"u"}`, http.StatusBadRequest},
		{"пустой user_id", `{"user_id":"","event_name":"e"}`, http.StatusBadRequest},
		{"неверный тип timestamp", `{"user_id":"u","event_name":"e","timestamp":"now"}`, http.StatusBadRequest},
		{"длинный user_id", `{"user_id":"` + long + `","event_name":"e"}`, http.StatusBadRequest},
		{"длинный event_name", `{"user_id":"u","event_name":"` + long + `"}`, http.StatusBadRequest},
		{"длинный event_id", field("event_id"), http.StatusBadRequest},
		{"длинный experiment_name", field("experiment_name"), http.StatusBadRequest},
		{"длинный variant", field("variant"), http.StatusBadRequest},
		{
			"большой event_data",
			`{"user_id":"u","event_name":"e","event_data":{"x":"` + strings.Repeat("a", MaxEventDataBytes) + `"}}`,
			http.StatusBadRequest,
		},
		{
			"слишком большое тело",
			`{"user_id":"u","event_name":"e","event_data":{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}}`,
			http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProducer{}
			w := doRequest(newTestRouter(p), tt.body)

			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tt.wantCode, w.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("ответ не JSON: %v", err)
			}
			if _, ok := resp["error"]; !ok {
				t.Errorf("в ответе нет поля error: %s", w.Body.String())
			}
			if len(p.msgs) != 0 {
				t.Errorf("producer получил %d сообщений, want 0", len(p.msgs))
			}
		})
	}
}

func TestValidateTimestamp(t *testing.T) {
	minTS := testNow.Add(-MaxEventAge).UnixMilli()
	maxTS := testNow.Add(MaxFutureSkew).UnixMilli()

	tests := []struct {
		name    string
		ts      int64
		wantErr bool
	}{
		{"ноль — время сервера", 0, false},
		{"текущее время", testNowMs, false},
		{"нижняя граница (ровно 7 дней назад)", minTS, false},
		{"на 1 мс старше нижней границы", minTS - 1, true},
		{"верхняя граница (ровно +5 минут)", maxTS, false},
		{"на 1 мс дальше верхней границы", maxTS + 1, true},
		{"слишком старое (год назад)", testNow.AddDate(-1, 0, 0).UnixMilli(), true},
		{"слишком далёкое будущее (через год)", testNow.AddDate(1, 0, 0).UnixMilli(), true},
		{"отрицательное", -1, true},
		{"минимальное int64", -1 << 63, true},
		{"максимальное int64", 1<<63 - 1, true},
		{"секунды вместо миллисекунд", testNowMs / 1000, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := TrackEvent{UserID: "u", EventName: "e", Timestamp: tt.ts}
			err := e.Validate(testNow)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHandleTrack_TimestampWindow(t *testing.T) {
	tests := []struct {
		name     string
		ts       int64
		wantCode int
	}{
		{"в окне", testNowMs - 1000, http.StatusOK},
		{"нижняя граница", testNow.Add(-MaxEventAge).UnixMilli(), http.StatusOK},
		{"верхняя граница", testNow.Add(MaxFutureSkew).UnixMilli(), http.StatusOK},
		{"слишком старое", testNow.Add(-MaxEventAge).UnixMilli() - 1, http.StatusBadRequest},
		{"слишком далёкое будущее", testNow.Add(MaxFutureSkew).UnixMilli() + 1, http.StatusBadRequest},
		{"отрицательное", -1, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProducer{}
			body := `{"user_id":"u","event_name":"e","timestamp":` + strconv.FormatInt(tt.ts, 10) + `}`
			w := doRequest(newTestRouter(p), body)
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tt.wantCode, w.Body.String())
			}
			wantMsgs := 0
			if tt.wantCode == http.StatusOK {
				wantMsgs = 1
			}
			if len(p.msgs) != wantMsgs {
				t.Errorf("producer получил %d сообщений, want %d", len(p.msgs), wantMsgs)
			}
		})
	}
}

func TestHandleTrack_EnqueueError(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantRetryAfter string
	}{
		{"буфер переполнен", sink.ErrBufferFull, "1"},
		{"обёрнутый ErrBufferFull", fmt.Errorf("обёртка: %w", sink.ErrBufferFull), "1"},
		{"приёмник закрыт", sink.ErrClosed, ""},
		{"прочая ошибка", errors.New("сбой"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProducer{enqueueErr: tt.err}
			w := doRequest(newTestRouter(p), `{"user_id":"u","event_name":"e"}`)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("ответ не JSON: %v", err)
			}
			if msg, _ := resp["error"].(string); msg == "" {
				t.Errorf("в ответе нет поля error: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "сбой") {
				t.Errorf("текст внутренней ошибки попал в ответ: %s", w.Body.String())
			}
		})
	}
}

// pingProducer — fakeProducer, реализующий sink.Pinger.
type pingProducer struct {
	fakeProducer
	pingErr error
}

func (p *pingProducer) Ping(context.Context) error { return p.pingErr }

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name     string
		producer sink.Producer
		wantCode int
	}{
		{"без Pinger", &fakeProducer{}, http.StatusOK},
		{"Ping успешен", &pingProducer{}, http.StatusOK},
		{"Ping с ошибкой", &pingProducer{pingErr: errors.New("нет связи")}, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.GET("/health", NewHandler(tt.producer, testTopic).HandleHealth)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tt.wantCode, w.Body.String())
			}
		})
	}
}

// uuidV4Re — канонический UUID версии 4 с вариантом RFC 9562, в нижнем регистре.
var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func publishedEventID(t *testing.T, payload []byte) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("payload не JSON: %v", err)
	}
	id, _ := out["event_id"].(string)
	return id
}

func TestHandleTrack_EventID(t *testing.T) {
	t.Run("пустой event_id генерируется сервером, уникален для каждого события", func(t *testing.T) {
		p := &fakeProducer{}
		r := newTestRouter(p)
		bodies := []string{
			`{"user_id":"u-1","event_name":"purchase"}`,
			`{"event_id":"","user_id":"u-1","event_name":"purchase"}`,
			`{"user_id":"u-1","event_name":"purchase"}`,
		}
		for _, body := range bodies {
			if w := doRequest(r, body); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
		}
		seen := map[string]bool{}
		for _, msg := range p.msgs {
			id := publishedEventID(t, msg.payload)
			if !uuidV4Re.MatchString(id) {
				t.Errorf("event_id = %q, ожидался UUIDv4", id)
			}
			if seen[id] {
				t.Errorf("event_id %q повторился", id)
			}
			seen[id] = true
		}
		if len(seen) != len(bodies) {
			t.Errorf("уникальных event_id %d, want %d", len(seen), len(bodies))
		}
	})

	t.Run("event_id клиента сохраняется", func(t *testing.T) {
		p := &fakeProducer{}
		w := doRequest(newTestRouter(p), `{"event_id":"client-id-1","user_id":"u-1","event_name":"purchase"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		if got := publishedEventID(t, p.msgs[0].payload); got != "client-id-1" {
			t.Errorf("event_id = %q, want %q", got, "client-id-1")
		}
	})
}

func TestNewUUIDv4(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		if !uuidV4Re.MatchString(id) {
			t.Fatalf("newUUIDv4() = %q, ожидался UUIDv4", id)
		}
		if seen[id] {
			t.Fatalf("newUUIDv4() повторил значение %q", id)
		}
		seen[id] = true
	}
}
