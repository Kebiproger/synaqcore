package auth

// Вспомогательные функции для тестов OIDC/JWT (порт tests/_jwt_utils.py).
// Все ключи и токены синтетические, генерируются в памяти.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer        = "https://auth.example.com"
	testAudience      = "100000000000000001" // синтетический project_id
	testKID           = "test_kid_1"
	genericRolesClaim = "urn:zitadel:iam:org:project:roles"
)

func projectRolesClaim(projectID string) string {
	return "urn:zitadel:iam:org:project:" + projectID + ":roles"
}

var rolesClaim = projectRolesClaim(testAudience)

// ─────────── Ключи ───────────

var (
	keysMu   sync.Mutex
	keyCache = map[string]*rsa.PrivateKey{}
)

// rsaKey — кэшированный по имени синтетический RSA-ключ.
func rsaKey(name string) *rsa.PrivateKey {
	keysMu.Lock()
	defer keysMu.Unlock()
	if k, ok := keyCache[name]; ok {
		return k
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	keyCache[name] = k
	return k
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// publicJWK — JWK публичного ключа с kid и дополнительными полями (перезаписывают базовые).
func publicJWK(key *rsa.PrivateKey, kid string, extra map[string]any) map[string]any {
	jwk := map[string]any{
		"kty": "RSA",
		"n":   b64(key.N.Bytes()),
		"e":   b64(big.NewInt(int64(key.E)).Bytes()),
		"kid": kid,
	}
	for k, v := range extra {
		jwk[k] = v
	}
	return jwk
}

func jwksDoc(keys ...map[string]any) any {
	list := make([]any, 0, len(keys))
	for _, k := range keys {
		list = append(list, k)
	}
	return map[string]any{"keys": list}
}

func publicPEM(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// ─────────── Часы ───────────

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(time.Now().Unix(), 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ─────────── Токены ───────────

var jtiSeq atomic.Int64

type tokenSpec struct {
	roles    []string // nil — без проектного claim
	key      *rsa.PrivateKey
	kid      *string // nil — без kid
	sub      any
	issuer   any
	audience any
	lifetime time.Duration
	drop     []string
	extra    map[string]any
	header   map[string]any
}

type tokenOpt func(*tokenSpec)

func withRoles(roles ...string) tokenOpt {
	return func(s *tokenSpec) {
		if roles == nil {
			roles = []string{}
		}
		s.roles = roles
	}
}
func withKey(k *rsa.PrivateKey) tokenOpt    { return func(s *tokenSpec) { s.key = k } }
func withKID(kid string) tokenOpt           { return func(s *tokenSpec) { s.kid = &kid } }
func withoutKID() tokenOpt                  { return func(s *tokenSpec) { s.kid = nil } }
func withSub(sub any) tokenOpt              { return func(s *tokenSpec) { s.sub = sub } }
func withIssuer(iss any) tokenOpt           { return func(s *tokenSpec) { s.issuer = iss } }
func withAudience(aud any) tokenOpt         { return func(s *tokenSpec) { s.audience = aud } }
func withLifetime(d time.Duration) tokenOpt { return func(s *tokenSpec) { s.lifetime = d } }
func withDrop(claims ...string) tokenOpt {
	return func(s *tokenSpec) { s.drop = append(s.drop, claims...) }
}
func withHeader(k string, v any) tokenOpt   { return func(s *tokenSpec) { s.header[k] = v } }
func withClaim(name string, v any) tokenOpt { return func(s *tokenSpec) { s.extra[name] = v } }

// claimsAt — claims access token в стиле Zitadel (с jti) на момент now.
func claimsAt(now time.Time, opts ...tokenOpt) (jwt.MapClaims, *tokenSpec) {
	kid := testKID
	s := &tokenSpec{
		kid: &kid, sub: "test_user_1", issuer: testIssuer, audience: testAudience,
		lifetime: 300 * time.Second, extra: map[string]any{}, header: map[string]any{},
	}
	for _, o := range opts {
		o(s)
	}
	c := jwt.MapClaims{
		"sub": s.sub,
		"iss": s.issuer,
		"aud": s.audience,
		"iat": now.Unix(),
		"exp": now.Add(s.lifetime).Unix(),
		"jti": fmt.Sprintf("test_jti_%d", jtiSeq.Add(1)),
	}
	if s.roles != nil {
		pid := testAudience
		if a, ok := s.audience.(string); ok {
			pid = a
		}
		roles := map[string]any{}
		for _, r := range s.roles {
			roles[r] = map[string]any{"100000000000000002": "test.example.com"}
		}
		c[projectRolesClaim(pid)] = roles
	}
	for k, v := range s.extra {
		c[k] = v
	}
	for _, d := range s.drop {
		delete(c, d)
	}
	return c, s
}

// makeToken — подписанный RS256 access token.
func makeToken(t testing.TB, now time.Time, opts ...tokenOpt) string {
	t.Helper()
	c, s := claimsAt(now, opts...)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	if s.kid != nil {
		tok.Header["kid"] = *s.kid
	}
	for k, v := range s.header {
		tok.Header[k] = v
	}
	key := s.key
	if key == nil {
		key = rsaKey("main")
	}
	out, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// hs256WithPublicPEM — атака algorithm confusion: HMAC-подпись публичным PEM как секретом.
func hs256WithPublicPEM(now time.Time, headerAlg string, opts ...tokenOpt) string {
	c, _ := claimsAt(now, opts...)
	header := map[string]any{"alg": headerAlg, "typ": "JWT", "kid": testKID}
	input := b64(mustJSON(header)) + "." + b64(mustJSON(c))
	mac := hmac.New(sha256.New, publicPEM(rsaKey("main")))
	mac.Write([]byte(input))
	return input + "." + b64(mac.Sum(nil))
}

func noneAlgToken(now time.Time, opts ...tokenOpt) string {
	c, _ := claimsAt(now, opts...)
	header := map[string]any{"alg": "none", "typ": "JWT", "kid": testKID}
	return b64(mustJSON(header)) + "." + b64(mustJSON(c)) + "."
}

// ─────────── JWKS-сервер ───────────

// jwksServer — httptest-сервер JWKS с управляемым ответом и счётчиком запросов.
type jwksServer struct {
	*httptest.Server
	mu       sync.Mutex
	body     []byte
	status   int
	delay    time.Duration
	hosts    []string
	accepts  []string
	requests atomic.Int64
}

func newJWKSServer(t testing.TB) *jwksServer {
	s := &jwksServer{status: http.StatusOK}
	s.setDoc(jwksDoc(publicJWK(rsaKey("main"), testKID, nil)))
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.mu.Lock()
		body, status, delay := s.body, s.status, s.delay
		s.hosts = append(s.hosts, r.Host)
		s.accepts = append(s.accepts, r.Header.Get("Accept"))
		s.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) setDoc(doc any) {
	s.mu.Lock()
	s.body, s.status = mustJSON(doc), http.StatusOK
	s.mu.Unlock()
}

// fail — JWKS недоступен (аналог ConnectionError в Python-тестах).
func (s *jwksServer) fail() {
	s.mu.Lock()
	s.body, s.status = []byte(`{"error":"down"}`), http.StatusInternalServerError
	s.mu.Unlock()
}

func (s *jwksServer) setDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

func (s *jwksServer) count() int { return int(s.requests.Load()) }

// ─────────── Тестовое окружение и роутер ───────────

type env struct {
	t     *testing.T
	srv   *jwksServer
	clock *fakeClock
	v     *Verifier
	r     *gin.Engine
}

// newEnv — OIDC настроен через env-поля, JWKS отдаёт один ключ testKID.
// mutate позволяет поменять Config до создания Verifier.
func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	srv := newJWKSServer(t)
	clock := newFakeClock()
	cfg := Config{Issuer: testIssuer, Audience: testAudience, JWKSURL: srv.URL, Now: clock.Now}
	for _, m := range mutate {
		m(&cfg)
	}
	v := NewVerifier(cfg)
	return &env{t: t, srv: srv, clock: clock, v: v, r: newRouter(v)}
}

// unconfiguredEnv — OIDC_* пустые (как conftest.py).
func unconfiguredEnv(t *testing.T) *env {
	return newEnv(t, func(c *Config) { *c = Config{Now: c.Now} })
}

const experiments = "/api/v1/experiments/"

// newRouter повторяет зависимости admin-роутера: список — admin|analyst, запись — admin.
func newRouter(v *Verifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ok := func(status int) gin.HandlerFunc {
		return func(c *gin.Context) {
			p, found := PrincipalFrom(c)
			if !found {
				c.JSON(http.StatusInternalServerError, gin.H{"detail": "no principal"})
				return
			}
			c.JSON(status, gin.H{"sub": p.Subject})
		}
	}
	read := RequireRoles(v, RoleAdmin, RoleAnalyst)
	write := RequireRoles(v, RoleAdmin)
	r.GET(experiments, read, ok(http.StatusOK))
	r.POST(experiments, write, ok(http.StatusCreated))
	r.DELETE(experiments+":name", write, ok(http.StatusNoContent))
	r.PATCH(experiments+":name", write, ok(http.StatusOK))
	return r
}

func (e *env) token(opts ...tokenOpt) string {
	e.t.Helper()
	return makeToken(e.t, e.clock.Now(), opts...)
}

func (e *env) admin() string { return e.token(withRoles(RoleAdmin)) }

func (e *env) do(method, path, authz string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func (e *env) get(token string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodGet, experiments, "Bearer "+token)
}

func (e *env) post(token string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodPost, experiments, "Bearer "+token)
}

func detailOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %q", w.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("ожидалось ровно {\"detail\": ...}, получено %s", w.Body.String())
	}
	d, _ := body["detail"].(string)
	return d
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("статус %d, ожидался %d; тело %s", w.Code, want, w.Body.String())
	}
}

func assert401(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assertStatus(t, w, http.StatusUnauthorized)
	if d := detailOf(t, w); d != detailUnauthorized {
		t.Fatalf("detail %q", d)
	}
	if h := w.Header().Get("WWW-Authenticate"); h != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", h)
	}
}

func assert503(t *testing.T, w *httptest.ResponseRecorder, detail string) {
	t.Helper()
	assertStatus(t, w, http.StatusServiceUnavailable)
	if d := detailOf(t, w); d != detail {
		t.Fatalf("detail %q, ожидался %q", d, detail)
	}
	if h := w.Header().Get("WWW-Authenticate"); h != "" {
		t.Fatalf("WWW-Authenticate не ожидался на 503: %q", h)
	}
}

func assertFetches(t *testing.T, s *jwksServer, want int) {
	t.Helper()
	if got := s.count(); got != want {
		t.Fatalf("запросов JWKS %d, ожидалось %d", got, want)
	}
}
