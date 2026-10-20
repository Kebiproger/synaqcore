// Package variants — публичная раздача вариантов (/api/v1/variants/...).
package variants

import (
	"synaq/internal/model"

	"github.com/gin-gonic/gin"
)

// Deps — зависимости обработчиков раздачи вариантов.
type Deps struct {
	Cache model.Cache
}

// Register регистрирует маршруты раздачи вариантов в r.
//
// TODO: реализовать маршруты, совместимые с experiment_api.
func Register(r gin.IRouter, d Deps) {
	_, _ = r, d
}
