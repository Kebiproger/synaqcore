package admin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"synaq/internal/auth"
	"synaq/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Все ключи, токены и данные синтетические.
const (
	testIssuer   = "https://auth.example.com"
	testAudience = "100000000000000001"
	testKID      = "admin_test_kid"
)

var (
	testKey  *rsa.PrivateKey
	keyOnce  sync.Once
	testNow  = time.Unix(1_800_000_000, 0)
	errBoom  = errors.New("синтетический сбой")
	longBody = `{"name":"big","variants":{"A":1},"config":{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}}`
)

func key() *rsa.PrivateKey {
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func newVerifier() *auth.Verifier {
	b64 := base64.RawURLEncoding.EncodeToString
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": testKID,
		"n": b64(key().N.Bytes()), "e": b64(big.NewInt(int64(key().E)).Bytes()),
	}}})
	return auth.NewVerifier(auth.Config{
		Issuer: testIssuer, Audience: testAudience, JWKSURL: "http://jwks.invalid",
		Now:     func() time.Time { return testNow },
		Fetcher: func(context.Context) ([]byte, error) { return jwks, nil },
	})
}

// token — access token Zitadel с ролями проекта.
func token(t *testing.T, roles ...string) string {
	t.Helper()
	rm := map[string]any{}
	for _, r := range roles {
		rm[r] = map[string]any{"100000000000000002": "test.example.com"}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "test_user_1", "iss": testIssuer, "aud": testAudience, "jti": "test_jti",
		"iat": testNow.Unix(), "exp": testNow.Add(5 * time.Minute).Unix(),
		"urn:zitadel:iam:org:project:" + testAudience + ":roles": rm,
	})
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(key())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ─────────── фейки ───────────

type fakeRepo struct {
	mu   sync.Mutex
	exps map[string]model.Experiment
	err  error // принудительная ошибка для всех методов
}

func (r *fakeRepo) List(context.Context) ([]model.Experiment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	out := make([]model.Experiment, 0, len(r.exps))
	for _, e := range r.exps {
		out = append(out, e)
	}
	return out, nil
}

func (r *fakeRepo) Get(_ context.Context, name string) (model.Experiment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.exps[name]
	if !ok {
		return model.Experiment{}, model.ErrNotFound
	}
	return e, nil
}

func (r *fakeRepo) Create(_ context.Context, e model.Experiment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if _, ok := r.exps[e.Name]; ok {
		return model.ErrExists
	}
	r.exps[e.Name] = e
	return nil
}

func (r *fakeRepo) Update(_ context.Context, name string, p model.ExperimentPatch) (model.Experiment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.exps[name]
	if !ok {
		return model.Experiment{}, model.ErrNotFound
	}
	active := e.IsActive
	if p.IsActive != nil {
		active = *p.IsActive
	}
	if p.Variants != nil && active {
		return model.Experiment{}, model.ErrActiveVariantsChange
	}
	e.IsActive = active
	if p.Variants != nil {
		e.Variants = *p.Variants
	}
	if p.ConfigSet {
		e.Config = nil
		if p.Config != nil {
			e.Config = *p.Config
		}
	}
	if p.URLSet {
		e.URL = p.URL
	}
	r.exps[name] = e
	return e, nil
}

func (r *fakeRepo) Delete(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.exps[name]; !ok {
		return model.ErrNotFound
	}
	delete(r.exps, name)
	return nil
}

type fakeCache struct {
	mu      sync.Mutex
	reloads int
	err     error
}

func (c *fakeCache) Snapshot() model.Snapshot { return nil }

func (c *fakeCache) Reload(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloads++
	return c.err
}

func (c *fakeCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reloads
}

// ─────────── окружение ───────────

type env struct {
	t     *testing.T
	repo  *fakeRepo
	cache *fakeCache
	r     *gin.Engine
}

func newEnv(t *testing.T, exps ...model.Experiment) *env {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &fakeRepo{exps: map[string]model.Experiment{}}
	for _, e := range exps {
		repo.exps[e.Name] = e
	}
	cache := &fakeCache{}
	r := gin.New()
	Register(r, Deps{Repo: repo, Cache: cache, Auth: newVerifier()})
	return &env{t: t, repo: repo, cache: cache, r: r}
}

func (e *env) do(method, path, tok, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("статус %d, ожидался %d; тело %s", w.Code, status, w.Body.String())
	}
	if body == "" {
		return
	}
	var got, want any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("тело не JSON: %q", w.Body.String())
	}
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatalf("ожидаемое тело не JSON: %v", err)
	}
	g, _ := json.Marshal(got)
	wnt, _ := json.Marshal(want)
	if string(g) != string(wnt) {
		t.Fatalf("тело\n%s\nожидалось\n%s", g, wnt)
	}
}

func inactive(name string) model.Experiment {
	u := "/landing"
	return model.Experiment{Name: name, Variants: map[string]int64{"A": 30, "B": 70},
		Config: json.RawMessage(`{"A":{"color":"red"}}`), URL: &u, IsActive: false}
}

func active(name string) model.Experiment {
	return model.Experiment{Name: name, Variants: map[string]int64{"A": 1}, IsActive: true}
}

const (
	list = "/api/v1/experiments/"
	item = "/api/v1/experiments/"
)

// ─────────── тесты ───────────

func TestAuthBeforeValidation(t *testing.T) {
	e := newEnv(t)
	// Без токена — 401 даже с невалидным телом.
	w := e.do(http.MethodPost, list, "", `{bad`)
	expect(t, w, http.StatusUnauthorized, `{"detail":"Требуется аутентификация"}`)
	// Аналитик не может создавать: 403 раньше 422.
	w = e.do(http.MethodPost, list, token(t, auth.RoleAnalyst), `{bad`)
	expect(t, w, http.StatusForbidden, `{"detail":"Недостаточно прав"}`)
	for _, m := range []string{http.MethodPatch, http.MethodDelete} {
		w = e.do(m, item+"x", token(t, auth.RoleAnalyst), `{}`)
		expect(t, w, http.StatusForbidden, "")
	}
	w = e.do(http.MethodGet, list, "", "")
	expect(t, w, http.StatusUnauthorized, "")
	if e.cache.count() != 0 {
		t.Fatal("кэш не должен перечитываться при отказе в доступе")
	}
}

func TestCreate(t *testing.T) {
	e := newEnv(t)
	admin := token(t, auth.RoleAdmin)
	w := e.do(http.MethodPost, list, admin, `{"name":"exp_1","variants":{"A":50,"B":50}}`)
	expect(t, w, http.StatusCreated, `{"message":"Эксперимент успешно создан","name":"exp_1"}`)
	if got := e.repo.exps["exp_1"]; !got.IsActive || got.Variants["B"] != 50 {
		t.Fatalf("в репозитории %+v", got)
	}
	if e.cache.count() != 1 {
		t.Fatalf("Reload вызван %d раз, ожидался 1", e.cache.count())
	}

	w = e.do(http.MethodPost, list, admin, `{"name":"exp_1","variants":{"A":1}}`)
	expect(t, w, http.StatusBadRequest, `{"detail":"Эксперимент 'exp_1' уже существует!"}`)

	w = e.do(http.MethodPost, list, admin, `{"name":"exp_2"}`)
	expect(t, w, http.StatusUnprocessableEntity, "")

	w = e.do(http.MethodPost, list, admin, longBody)
	expect(t, w, http.StatusRequestEntityTooLarge, `{"detail":"Request Entity Too Large"}`)

	if e.cache.count() != 1 {
		t.Fatalf("Reload после неуспешных запросов: %d", e.cache.count())
	}
}

func TestCreateReloadErrorStillSucceeds(t *testing.T) {
	e := newEnv(t)
	e.cache.err = errBoom
	w := e.do(http.MethodPost, list, token(t, auth.RoleAdmin), `{"name":"exp_1","variants":{"A":1}}`)
	expect(t, w, http.StatusCreated, "")
}

func TestRepoErrorIs500WithoutDetails(t *testing.T) {
	e := newEnv(t)
	e.repo.err = errBoom
	w := e.do(http.MethodGet, list, token(t, auth.RoleAdmin), "")
	expect(t, w, http.StatusInternalServerError, `{"detail":"Internal Server Error"}`)
}

func TestListAnalyst(t *testing.T) {
	e := newEnv(t, inactive("exp_1"))
	w := e.do(http.MethodGet, list, token(t, auth.RoleAnalyst), "")
	expect(t, w, http.StatusOK, `[{"name":"exp_1","variants":{"A":30,"B":70},
		"config":{"A":{"color":"red"}},"url":"/landing","is_active":false,"total_weight":100}]`)

	// NULL config/url → null; пустой список → [].
	e = newEnv(t, active("exp_2"))
	w = e.do(http.MethodGet, list, token(t, auth.RoleAnalyst), "")
	expect(t, w, http.StatusOK, `[{"name":"exp_2","variants":{"A":1},"config":null,"url":null,"is_active":true,"total_weight":1}]`)
	e = newEnv(t)
	w = e.do(http.MethodGet, list, token(t, auth.RoleAdmin), "")
	expect(t, w, http.StatusOK, `[]`)
}

func TestPatch(t *testing.T) {
	e := newEnv(t, inactive("off"), active("on"))
	admin := token(t, auth.RoleAdmin)

	w := e.do(http.MethodPatch, item+"off", admin, `{}`)
	expect(t, w, http.StatusBadRequest, `{"detail":"Нет полей для обновления"}`)

	w = e.do(http.MethodPatch, item+"missing", admin, `{"is_active":true}`)
	expect(t, w, http.StatusNotFound, `{"detail":"Эксперимент не найден"}`)

	w = e.do(http.MethodPatch, item+"on", admin, `{"variants":{"A":1,"B":1}}`)
	expect(t, w, http.StatusConflict, `{"detail":"Нельзя менять варианты активного эксперимента: пользователи будут перераспределены. Сначала выключите эксперимент (можно в том же запросе: is_active=false)."}`)

	w = e.do(http.MethodPatch, item+"on", admin, `{"is_active":"yes"}`)
	expect(t, w, http.StatusUnprocessableEntity, "")

	if e.cache.count() != 0 {
		t.Fatalf("Reload после неуспешных PATCH: %d", e.cache.count())
	}

	// Выключить и поменять варианты в одном запросе; config: null очищает.
	w = e.do(http.MethodPatch, item+"on", admin, `{"is_active":false,"variants":{"A":1,"B":3},"config":null}`)
	expect(t, w, http.StatusOK, `{"name":"on","variants":{"A":1,"B":3},"config":null,"url":null,"is_active":false,"total_weight":4}`)
	if e.cache.count() != 1 {
		t.Fatalf("Reload вызван %d раз, ожидался 1", e.cache.count())
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t, active("exp_1"))
	admin := token(t, auth.RoleAdmin)

	w := e.do(http.MethodDelete, item+"missing", admin, "")
	expect(t, w, http.StatusNotFound, `{"detail":"Эксперимент не найден"}`)
	if e.cache.count() != 0 {
		t.Fatal("Reload после 404")
	}

	w = e.do(http.MethodDelete, item+"exp_1", admin, "")
	expect(t, w, http.StatusOK, `{"message":"Эксперимент 'exp_1' удалён"}`)
	if _, ok := e.repo.exps["exp_1"]; ok {
		t.Fatal("эксперимент не удалён")
	}
	if e.cache.count() != 1 {
		t.Fatalf("Reload вызван %d раз, ожидался 1", e.cache.count())
	}
}
