// Package apierr — единый формат ошибок HTTP API, совместимый с FastAPI:
// тело {"detail": ...}, где detail — строка или список ошибок валидации.
package apierr

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Abort прерывает обработку запроса и отвечает {"detail": detail} с кодом status.
func Abort(c *gin.Context, status int, detail any) {
	c.AbortWithStatusJSON(status, gin.H{"detail": detail})
}

// AbortValidation отвечает 422 {"detail": items}; items — список ошибок
// валидации (например, []validate.ErrorItem).
func AbortValidation(c *gin.Context, items any) {
	Abort(c, http.StatusUnprocessableEntity, items)
}

// NoRoute — обработчик для engine.NoRoute: 404 {"detail":"Not Found"}.
func NoRoute(c *gin.Context) {
	Abort(c, http.StatusNotFound, "Not Found")
}

// NoMethod — обработчик для engine.NoMethod: 405 {"detail":"Method Not Allowed"}.
// Заголовок Allow выставляет сам gin.
func NoMethod(c *gin.Context) {
	Abort(c, http.StatusMethodNotAllowed, "Method Not Allowed")
}

// Install включает ответы 404/405 в формате FastAPI: HandleMethodNotAllowed
// (иначе gin отвечает 404 и на неподдерживаемый метод), NoRoute и NoMethod.
// Редиректы gin по слэшу в конце пути (RedirectTrailingSlash) не трогаются.
func Install(engine *gin.Engine) {
	engine.HandleMethodNotAllowed = true
	engine.NoRoute(NoRoute)
	engine.NoMethod(NoMethod)
}
