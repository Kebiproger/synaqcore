package variants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"synaq/internal/allocation"
	"synaq/internal/api/apierr"
	"synaq/internal/model"

	"github.com/gin-gonic/gin"
)

// fakeSnapshot/fakeCache — снимок в памяти; список хранится в порядке created_at DESC.
type fakeSnapshot []model.Experiment

func (s fakeSnapshot) Get(name string) (model.Experiment, bool) {
	for _, e := range s {
		if e.Name == name {
			return e, true
		}
	}
	return model.Experiment{}, false
}

func (s fakeSnapshot) All() []model.Experiment { return append([]model.Experiment(nil), s...) }

type fakeCache struct{ snap fakeSnapshot }

func (c *fakeCache) Snapshot() model.Snapshot     { return c.snap }
func (c *fakeCache) Reload(context.Context) error { return nil }

func ptr[T any](v T) *T { return &v }

func newRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	cache := &fakeCache{snap: fakeSnapshot{
		{Name: "new_exp", Variants: map[string]int64{"treatment": 1, "B": 1, "A": 1}, Config: json.RawMessage(`{"A":{"k":1}}`),
			URL: ptr("/promo"), IsActive: true, CreatedAt: now},
		{Name: "off", Variants: map[string]int64{"A": 1, "B": 1}, IsActive: false, CreatedAt: now.Add(-time.Hour)},
		{Name: "old_exp", Variants: map[string]int64{"A": 50, "B": 50}, IsActive: true, CreatedAt: now.Add(-2 * time.Hour)},
	}}
	r := gin.New()
	apierr.Install(r)
	Register(r, Deps{Cache: cache})
	return r
}

func do(t *testing.T, r http.Handler, method, target, body string) (int, string) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// sameJSON сравнивает JSON без учёта порядка ключей объектов.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("невалидный JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("невалидный ожидаемый JSON: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("тело:\n got  %s\n want %s", gb, wb)
	}
}

func TestVariant(t *testing.T) {
	r := newRouter()
	wantA := allocation.Allocate("u1", "old_exp", map[string]int64{"A": 50, "B": 50})
	cases := []struct {
		name, target string
		code         int
		body         string
	}{
		{"path", "/api/v1/variants/old_exp?user_id=u1", 200,
			`{"experiment_name":"old_exp","user_id":"u1","variant":"` + wantA + `"}`},
		{"query", "/api/v1/variants/?experiment_name=old_exp&user_id=u1", 200,
			`{"experiment_name":"old_exp","user_id":"u1","variant":"` + wantA + `"}`},
		{"последнее значение побеждает", "/api/v1/variants/?experiment_name=x&experiment_name=old_exp&user_id=z&user_id=u1", 200,
			`{"experiment_name":"old_exp","user_id":"u1","variant":"` + wantA + `"}`},
		{"неактивный → control", "/api/v1/variants/off?user_id=u1", 200,
			`{"experiment_name":"off","user_id":"u1","variant":"control"}`},
		{"неизвестный → control", "/api/v1/variants/nope?user_id=u1", 200,
			`{"experiment_name":"nope","user_id":"u1","variant":"control"}`},
		{"batch как имя (GET)", "/api/v1/variants/batch?user_id=u1", 200,
			`{"experiment_name":"batch","user_id":"u1","variant":"control"}`},
		{"нет user_id", "/api/v1/variants/old_exp", 400, `{"detail":"experiment_name и user_id обязательны"}`},
		{"пустой user_id", "/api/v1/variants/old_exp?user_id=", 400, `{"detail":"experiment_name и user_id обязательны"}`},
		{"нет имени", "/api/v1/variants/?user_id=u1", 400, `{"detail":"experiment_name и user_id обязательны"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, r, http.MethodGet, tc.target, "")
			if code != tc.code {
				t.Fatalf("код %d, ожидался %d (%s)", code, tc.code, body)
			}
			sameJSON(t, body, tc.body)
		})
	}
}

func TestConfig(t *testing.T) {
	r := newRouter()
	code, body := do(t, r, http.MethodGet, "/api/v1/variants/config/off", "")
	if code != 200 {
		t.Fatalf("код %d", code)
	}
	sameJSON(t, body, `{"name":"off","variants":{"A":1,"B":1},"config":{},"url":null,"is_active":false}`)

	code, body = do(t, r, http.MethodGet, "/api/v1/variants/config/nope", "")
	if code != 404 {
		t.Fatalf("код %d", code)
	}
	sameJSON(t, body, `{"detail":"Эксперимент не найден"}`)
}

func TestActiveAndEvalConfigs(t *testing.T) {
	r := newRouter()
	code, body := do(t, r, http.MethodGet, "/api/v1/variants/active", "")
	if code != 200 {
		t.Fatalf("код %d", code)
	}
	// Порядок экспериментов — created_at DESC; ключи вариантов — в порядке JSONB.
	want := `[{"name":"new_exp","url":"/promo","variants":["A","B","treatment"]},` +
		`{"name":"old_exp","url":null,"variants":["A","B"]}]`
	if body != want {
		t.Errorf("active:\n got  %s\n want %s", body, want)
	}

	code, body = do(t, r, http.MethodGet, "/api/v1/variants/eval-configs", "")
	if code != 200 {
		t.Fatalf("код %d", code)
	}
	sameJSON(t, body, `{"experiments":{
		"new_exp":{"name":"new_exp","variants":{"A":1,"B":1,"treatment":1},"config":{"A":{"k":1}},"url":"/promo"},
		"old_exp":{"name":"old_exp","variants":{"A":50,"B":50},"config":{},"url":null}}}`)
}

func TestBatch(t *testing.T) {
	r := newRouter()
	wantA := allocation.Allocate("u1", "old_exp", map[string]int64{"A": 50, "B": 50})
	code, body := do(t, r, http.MethodPost, "/api/v1/variants/batch",
		`{"user_id":"u1","experiments":["old_exp","off","nope"]}`)
	if code != 200 {
		t.Fatalf("код %d (%s)", code, body)
	}
	sameJSON(t, body, `{"user_id":"u1","variants":{"old_exp":"`+wantA+`","off":"control","nope":"control"}}`)

	code, body = do(t, r, http.MethodPost, "/api/v1/variants/batch", `{"user_id":""}`)
	if code != 422 || !strings.Contains(body, `"detail":[`) {
		t.Errorf("невалидное тело: код %d, тело %s", code, body)
	}

	code, _ = do(t, r, http.MethodPost, "/api/v1/variants/batch",
		`{"user_id":"u1","experiments":["`+strings.Repeat("a", maxBatchBody)+`"]}`)
	if code != 413 {
		t.Errorf("огромное тело: код %d, ожидался 413", code)
	}
}
