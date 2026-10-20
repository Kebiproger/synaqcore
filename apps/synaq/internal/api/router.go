// Package api собирает HTTP-маршрутизатор сервиса synaq.
package api

import (
	"synaq/internal/api/admin"
	"synaq/internal/api/variants"
	"synaq/internal/auth"
	"synaq/internal/model"
	"synaq/internal/track"

	"github.com/gin-gonic/gin"
)

// Deps — зависимости всех HTTP-обработчиков.
type Deps struct {
	Tracker *track.Handler
	Repo    model.Repository
	Cache   model.Cache
	Auth    *auth.Verifier
}

// NewRouter создаёт gin.Engine со всеми маршрутами. Режим Gin (gin.SetMode)
// выставляет вызывающий код до вызова NewRouter.
func NewRouter(d Deps) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())
	if gin.Mode() != gin.ReleaseMode {
		router.Use(gin.Logger())
	}

	router.GET("/health", d.Tracker.HandleHealth)
	router.POST("/api/v1/tracker/events", d.Tracker.HandleTrack)

	variants.Register(router, variants.Deps{Cache: d.Cache})
	admin.Register(router, admin.Deps{Repo: d.Repo, Cache: d.Cache, Auth: d.Auth})
	return router
}
