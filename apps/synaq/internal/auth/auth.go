// Package auth — проверка JWT (RS256) от Zitadel и авторизация по ролям проекта.
// Контракт совпадает с apps/analytics_api/src/core/auth.py: fail closed (503, если
// OIDC не настроен или JWKS недоступен), 401 для невалидного токена, 403 без нужной роли.
//
// Как это работает:
//  1. Клиент присылает `Authorization: Bearer <JWT>` (access token от Zitadel).
//  2. Публичные ключи берём из JWKS (OIDC_JWKS_URL) и кэшируем в памяти с TTL.
//     Zitadel выбирает инстанс по заголовку Host, поэтому при запросе по внутреннему
//     адресу отправляем Host = OIDC_JWKS_HOST.
//  3. Проверяем подпись (только RS256), exp/nbf/iat, iss, aud и наличие jti
//     (у access token Zitadel jti есть, у ID token — нет, поэтому ID token
//     как bearer-токен не принимается).
//  4. Роли берём ТОЛЬКО из проектного claim `urn:zitadel:iam:org:project:<project_id>:roles`,
//     где project_id = audience. Общий `urn:zitadel:iam:org:project:roles` игнорируется:
//     он может содержать роли другого проекта (cross-project role confusion).
package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Роли проекта Zitadel.
const (
	RoleAdmin   = "admin"
	RoleAnalyst = "analyst"
)

// Тексты ошибок — те же, что в Python-версии (клиенты и тесты на них опираются).
const (
	detailUnauthorized  = "Требуется аутентификация"
	detailNotConfigured = "Аутентификация не настроена"
	detailUnavailable   = "Сервис аутентификации недоступен"
	detailForbidden     = "Недостаточно прав"
)

// principalKey — ключ Principal в gin.Context.
const principalKey = "synaq.auth.principal"

// Principal — аутентифицированный субъект запроса.
type Principal struct {
	Subject string
	Roles   map[string]struct{}
}

// HasAnyRole сообщает, есть ли у субъекта хотя бы одна из ролей.
func (p Principal) HasAnyRole(roles ...string) bool {
	for _, r := range roles {
		if _, ok := p.Roles[r]; ok {
			return true
		}
	}
	return false
}

// Error — ошибка аутентификации/авторизации с HTTP-статусом и телом {"detail": ...}.
type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return e.Detail }

// Сентинельные ошибки (сравниваются через errors.Is по указателю).
var (
	// ErrUnauthorized — нет токена или он невалиден (401 + WWW-Authenticate: Bearer).
	ErrUnauthorized = &Error{Status: http.StatusUnauthorized, Detail: detailUnauthorized}
	// ErrNotConfigured — OIDC не настроен (нет issuer / audience / JWKS URL) → 503.
	ErrNotConfigured = &Error{Status: http.StatusServiceUnavailable, Detail: detailNotConfigured}
	// ErrUnavailable — JWKS недоступен и годных ключей нет (или они старше суток) → 503.
	ErrUnavailable = &Error{Status: http.StatusServiceUnavailable, Detail: detailUnavailable}
	// ErrForbidden — у субъекта нет ни одной из требуемых ролей → 403.
	ErrForbidden = &Error{Status: http.StatusForbidden, Detail: detailForbidden}
)

// abortWith отвечает ошибкой; токен и его содержимое в ответ никогда не попадают.
func abortWith(c *gin.Context, e *Error) {
	if e.Status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", "Bearer")
	}
	c.AbortWithStatusJSON(e.Status, gin.H{"detail": e.Detail})
}

// bearerToken извлекает токен так же, как FastAPI HTTPBearer:
// схема до первого пробела (без учёта регистра), остаток — без пробелов по краям.
func bearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	scheme, param, _ := strings.Cut(header, " ")
	param = strings.TrimSpace(param)
	if scheme == "" || param == "" {
		return "", false
	}
	if strings.ToLower(scheme) != "bearer" {
		return "", false
	}
	return param, true
}

// RequireRoles возвращает middleware, пропускающий запрос, только если у субъекта
// есть хотя бы одна из ролей roles. Principal кладётся в контекст (см. PrincipalFrom).
//
// Без ролей — паника при сборке роутера (ошибка программиста, как ValueError в Python).
func RequireRoles(v *Verifier, roles ...string) gin.HandlerFunc {
	if len(roles) == 0 {
		panic("auth.RequireRoles: нужно указать хотя бы одну роль")
	}
	required := append([]string(nil), roles...)
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			abortWith(c, ErrUnauthorized)
			return
		}
		principal, err := v.Verify(c.Request.Context(), token)
		if err != nil {
			abortWith(c, asAuthError(err))
			return
		}
		if !principal.HasAnyRole(required...) {
			abortWith(c, ErrForbidden)
			return
		}
		c.Set(principalKey, principal)
		c.Next()
	}
}

// PrincipalFrom возвращает Principal, сохранённый RequireRoles.
func PrincipalFrom(c *gin.Context) (Principal, bool) {
	if c == nil {
		return Principal{}, false
	}
	v, ok := c.Get(principalKey)
	if !ok {
		return Principal{}, false
	}
	p, ok := v.(Principal)
	return p, ok
}
