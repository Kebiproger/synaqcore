// Package apierr — единый формат ошибок HTTP API, совместимый с FastAPI:
// тело {"detail": ...}, где detail — строка или список ошибок валидации.
package apierr

import "github.com/gin-gonic/gin"

// Abort прерывает обработку запроса и отвечает {"detail": detail} с кодом status.
func Abort(c *gin.Context, status int, detail any) {
	c.AbortWithStatusJSON(status, gin.H{"detail": detail})
}
