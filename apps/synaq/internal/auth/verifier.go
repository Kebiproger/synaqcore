package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
)

// ─────────── Параметры (как в Python) ───────────
const (
	algRS256               = "RS256" // none / HS* отклоняются
	jwksTTL                = 600 * time.Second
	jwksMinRefreshInterval = 30 * time.Second // не чаще одного перезапроса JWKS (защита от DoS неизвестными kid)
	jwksEmptyRetryInterval = 5 * time.Second  // повтор, если ключей ещё нет совсем (Zitadel только стартует)
	jwksMaxStale           = 24 * time.Hour   // дольше устаревшие ключи не используем → 503
	jwksFetchTimeout       = 3 * time.Second
	leeway                 = 30 * time.Second // допуск рассинхрона часов
	maxTokenLength         = 8192

	maxJWKSBodyBytes   = 1 << 20  // защита от огромного ответа JWKS
	maxConfigFileBytes = 64 << 10 // oidc.json маленький

	projectRolesPrefix = "urn:zitadel:iam:org:project:"
	projectRolesSuffix = ":roles"
)

// JWKSFetcher загружает сырое тело JWKS. Подменяется в тестах.
type JWKSFetcher func(ctx context.Context) ([]byte, error)

// Config — настройки OIDC. Пустые Issuer/Audience берутся из ConfigFile (issuer/project_id).
type Config struct {
	Issuer     string // OIDC_ISSUER
	Audience   string // OIDC_AUDIENCE (переопределяет project_id из файла)
	JWKSURL    string // OIDC_JWKS_URL
	JWKSHost   string // OIDC_JWKS_HOST: Host-заголовок для JWKS-запроса по внутреннему адресу
	ConfigFile string // OIDC_CONFIG_FILE: JSON {issuer, project_id, client_id} от bootstrap-контейнера

	// Now — часы (по умолчанию time.Now). Для тестов.
	Now func() time.Time
	// Fetcher — загрузка JWKS (по умолчанию HTTP GET JWKSURL). Для тестов.
	Fetcher JWKSFetcher
}

// ConfigFromEnv читает OIDC_* из окружения. Пустые значения → защищённые эндпоинты отвечают 503.
func ConfigFromEnv() Config {
	return Config{
		Issuer:     os.Getenv("OIDC_ISSUER"),
		Audience:   os.Getenv("OIDC_AUDIENCE"),
		JWKSURL:    os.Getenv("OIDC_JWKS_URL"),
		JWKSHost:   os.Getenv("OIDC_JWKS_HOST"),
		ConfigFile: os.Getenv("OIDC_CONFIG_FILE"),
	}
}

// fileConfig — валидный OIDC_CONFIG_FILE.
type fileConfig struct {
	Issuer    string
	ProjectID string
}

// Verifier проверяет JWT по JWKS Zitadel. Безопасен для конкурентного использования.
type Verifier struct {
	cfg   Config
	now   func() time.Time
	fetch JWKSFetcher

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time     // нулевое значение — попыток ещё не было
	inflight    chan struct{} // закрывается по завершении текущей загрузки JWKS

	fileMu  sync.Mutex
	fileCfg *fileConfig // кэшируется, как только файл валиден

	warned sync.Map
}

// NewVerifier создаёт Verifier. Ключи JWKS и OIDC_CONFIG_FILE читаются лениво.
func NewVerifier(cfg Config) *Verifier {
	v := &Verifier{cfg: cfg, now: cfg.Now, fetch: cfg.Fetcher}
	if v.now == nil {
		v.now = time.Now
	}
	if v.fetch == nil {
		v.fetch = newHTTPFetcher(cfg.JWKSURL, cfg.JWKSHost)
	}
	return v
}

func (v *Verifier) warnOnce(key, format string, args ...any) {
	if _, loaded := v.warned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Printf(format, args...)
}

// asAuthError приводит произвольную ошибку к *Error (неизвестное → 503, fail closed).
func asAuthError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return ErrUnavailable
}

// ─────────── Конфигурация OIDC ───────────

// loadFileConfig читает OIDC_CONFIG_FILE (пишется bootstrap-контейнером после старта).
// Файла может ещё не быть — читаем лениво и кэшируем, как только он валиден.
func (v *Verifier) loadFileConfig() *fileConfig {
	v.fileMu.Lock()
	defer v.fileMu.Unlock()
	if v.fileCfg != nil {
		return v.fileCfg
	}
	path := v.cfg.ConfigFile
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		if err == nil || errors.Is(err, os.ErrNotExist) {
			v.warnOnce("config_file_missing", "Файл OIDC-конфига пока отсутствует: %s", path)
		} else {
			v.warnOnce("config_file_invalid", "Не удалось прочитать OIDC-конфиг %s: %T", path, err)
		}
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		v.warnOnce("config_file_invalid", "Не удалось прочитать OIDC-конфиг %s: %T", path, err)
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigFileBytes+1))
	_ = f.Close()
	if err != nil || len(raw) > maxConfigFileBytes || !utf8.Valid(raw) {
		v.warnOnce("config_file_invalid", "Не удалось прочитать OIDC-конфиг %s", path)
		return nil
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		v.warnOnce("config_file_invalid", "Не удалось прочитать OIDC-конфиг %s: %T", path, err)
		return nil
	}
	obj, ok := data.(map[string]any)
	if !ok {
		v.warnOnce("config_file_invalid", "OIDC-конфиг %s имеет неверный формат", path)
		return nil
	}
	issuer, _ := obj["issuer"].(string)
	projectID, _ := obj["project_id"].(string)
	if projectID == "" {
		v.warnOnce("config_file_invalid", "В OIDC-конфиге %s нет project_id", path)
		return nil
	}
	v.fileCfg = &fileConfig{Issuer: issuer, ProjectID: projectID}
	log.Printf("OIDC-конфиг загружен из %s", path)
	return v.fileCfg
}

// resolveOIDC возвращает (issuer, audience) или ErrNotConfigured.
func (v *Verifier) resolveOIDC() (string, string, error) {
	issuer, audience := v.cfg.Issuer, v.cfg.Audience
	if issuer == "" || audience == "" {
		if fc := v.loadFileConfig(); fc != nil {
			if issuer == "" {
				issuer = fc.Issuer
			}
			if audience == "" {
				audience = fc.ProjectID
			}
		}
	}
	if issuer == "" || audience == "" || v.cfg.JWKSURL == "" {
		v.warnOnce("not_configured",
			"OIDC не настроен (issuer=%t, audience=%t, jwks_url=%t) — защищённые эндпоинты отвечают 503",
			issuer != "", audience != "", v.cfg.JWKSURL != "")
		return "", "", ErrNotConfigured
	}
	return issuer, audience, nil
}

// ─────────── Проверка токена ───────────

// unverifiedHeader повторяет jwt.get_unverified_header из PyJWT: сегменты делятся
// по последней и первой точке, заголовок — JSON-объект, подпись — валидный base64url,
// kid (если есть) — строка, crit не поддерживается.
func unverifiedHeader(token string) (map[string]any, bool) {
	dot := strings.LastIndexByte(token, '.')
	if dot < 0 {
		return nil, false
	}
	signingInput, sig := token[:dot], token[dot+1:]
	headerSeg, _, ok := strings.Cut(signingInput, ".")
	if !ok {
		return nil, false
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(headerSeg)
	if err != nil {
		return nil, false
	}
	if _, err := base64.RawURLEncoding.DecodeString(sig); err != nil {
		return nil, false
	}
	var header map[string]any
	if err := json.Unmarshal(rawHeader, &header); err != nil || header == nil {
		return nil, false
	}
	if kid, present := header["kid"]; present {
		if _, isStr := kid.(string); !isStr {
			return nil, false
		}
	}
	if _, present := header["crit"]; present {
		return nil, false
	}
	return header, true
}

// Verify проверяет JWT и возвращает Principal.
// Ошибки: ErrUnauthorized (401), ErrNotConfigured / ErrUnavailable (503).
//
// Заголовок токена проверяем до чтения настроек: мусор вместо токена — всегда 401.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	if len(token) > maxTokenLength {
		return Principal{}, ErrUnauthorized
	}
	header, ok := unverifiedHeader(token)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	if alg, _ := header["alg"].(string); alg != algRS256 {
		return Principal{}, ErrUnauthorized
	}
	kid, _ := header["kid"].(string)
	if kid == "" {
		return Principal{}, ErrUnauthorized
	}
	if v == nil {
		return Principal{}, ErrNotConfigured
	}

	issuer, audience, err := v.resolveOIDC()
	if err != nil {
		return Principal{}, err
	}
	key, err := v.signingKey(ctx, kid)
	if err != nil {
		return Principal{}, err
	}
	if key == nil {
		return Principal{}, ErrUnauthorized
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{algRS256}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithLeeway(leeway),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	claims := jwt.MapClaims{}
	if _, err := parser.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
		return key, nil
	}); err != nil {
		return Principal{}, ErrUnauthorized
	}
	// Обязательные claims, которые golang-jwt сам не требует (PyJWT: require=[..., "iat", "sub", "jti"]).
	if claims["iat"] == nil || claims["jti"] == nil {
		return Principal{}, ErrUnauthorized
	}
	if _, isStr := claims["jti"].(string); !isStr {
		return Principal{}, ErrUnauthorized
	}
	// PyJWT отклоняет nbf: null (int(None)), golang-jwt считает его отсутствующим.
	if nbf, present := claims["nbf"]; present && nbf == nil {
		return Principal{}, ErrUnauthorized
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Principal{}, ErrUnauthorized
	}
	return Principal{Subject: sub, Roles: extractRoles(claims, audience)}, nil
}

// extractRoles — роли только из claim своего проекта.
// Форматы значения: {"role": {...}} или список таких объектов [{"role": {...}}, ...].
func extractRoles(claims jwt.MapClaims, projectID string) map[string]struct{} {
	roles := map[string]struct{}{}
	value := claims[projectRolesPrefix+projectID+projectRolesSuffix]
	items, isList := value.([]any)
	if !isList {
		items = []any{value}
	}
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for r := range obj {
			if r != "" {
				roles[r] = struct{}{}
			}
		}
	}
	return roles
}
