// Package admin — управление экспериментами (/api/v1/experiments/...), требует JWT.
package admin

import (
	"synaq/internal/auth"
	"synaq/internal/model"

	"github.com/gin-gonic/gin"
)

// Deps — зависимости обработчиков управления экспериментами.
type Deps struct {
	Repo  model.Repository
	Cache model.Cache
	Auth  *auth.Verifier
}

// Register регистрирует маршруты управления экспериментами в r.
//
// TODO: реализовать маршруты, совместимые с experiment_api.
func Register(r gin.IRouter, d Deps) {
	_, _ = r, d
}
