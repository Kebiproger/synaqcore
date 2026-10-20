// Package api собирает HTTP-маршрутизатор сервиса synaq.
package api

import (
	"net/http"

	"synaq/internal/api/admin"
	"synaq/internal/api/apierr"
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
	// Ready сообщает, загружен ли кэш конфигураций (для /readyz);
	// обычно configcache.Cache.Ready. nil — сервис считается неготовым.
	Ready func() bool
}

// NewRouter создаёт gin.Engine со всеми маршрутами. Режим Gin (gin.SetMode)
// выставляет вызывающий код до вызова NewRouter. CORS не настраивается:
// заголовки Access-Control-* выставляет только gateway (nginx).
func NewRouter(d Deps) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())
	if gin.Mode() != gin.ReleaseMode {
		router.Use(gin.Logger())
	}
	apierr.Install(router)

	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "Hello, World!"})
	})
	// /health — как у experiment_api: процесс жив, зависимости не проверяются.
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy"})
	})
	// /readyz — снимок конфигураций загружен, варианты можно раздавать.
	router.GET("/readyz", func(c *gin.Context) {
		if d.Ready == nil || !d.Ready() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	// /health/track — доступность приёмника событий (ClickHouse).
	router.GET("/health/track", d.Tracker.HandleHealth)
	router.POST("/api/v1/tracker/events", d.Tracker.HandleTrack)

	variants.Register(router, variants.Deps{Cache: d.Cache})
	admin.Register(router, admin.Deps{Repo: d.Repo, Cache: d.Cache, Auth: d.Auth})
	return router
}
