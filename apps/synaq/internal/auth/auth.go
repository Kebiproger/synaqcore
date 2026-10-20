// Package auth — проверка JWT (RS256) от Zitadel и авторизация по ролям проекта.
// Контракт совпадает с src/core/auth.py в experiment_api: fail closed (503, если
// OIDC не настроен или JWKS недоступен), 401 для невалидного токена, 403 без нужной роли.
package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Роли проекта Zitadel.
const (
	RoleAdmin   = "admin"
	RoleAnalyst = "analyst"
)

// Principal — аутентифицированный субъект запроса.
type Principal struct {
	Subject string
	Roles   map[string]struct{}
}

// Verifier проверяет JWT по JWKS Zitadel.
//
// TODO: реализовать (загрузка oidc.json, кеш JWKS, проверка iss/aud/exp/jti).
type Verifier struct{}

// RequireRoles возвращает middleware, пропускающий запрос, только если у субъекта
// есть хотя бы одна из ролей roles.
//
// TODO: реализовать; пока заглушка всегда отвечает 503 (fail closed).
func RequireRoles(v *Verifier, roles ...string) gin.HandlerFunc {
	_, _ = v, roles
	return func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"detail": "Аутентификация не настроена"})
	}
}
