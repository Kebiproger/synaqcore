// Package variants — публичная раздача вариантов (/api/v1/variants/...).
//
// Совместима с apps/experiment_api/src/api/v1/client.py: те же пути, JSON,
// коды ответов и тексты detail. Читает только снимок кэша конфигураций
// (model.Cache), в БД не ходит.
package variants

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"synaq/internal/allocation"
	"synaq/internal/api/apierr"
	"synaq/internal/api/validate"
	"synaq/internal/model"

	"github.com/gin-gonic/gin"
)

// Prefix — общий префикс маршрутов раздачи вариантов.
const Prefix = "/api/v1/variants"

// Тексты detail, совпадающие с experiment_api.
const (
	detailNotFound = "Эксперимент не найден"
	detailRequired = "experiment_name и user_id обязательны"
)

// maxBatchBody — предел тела POST /batch: 50 имён по 50 символов и user_id
// укладываются с большим запасом; защищает от чтения огромных тел в память.
const maxBatchBody = 64 << 10

// emptyObject — значение "config", когда конфиг не задан (Python: config or {}).
var emptyObject = json.RawMessage("{}")

// Deps — зависимости обработчиков раздачи вариантов.
type Deps struct {
	Cache model.Cache
}

type handlers struct {
	cache model.Cache
}

// Register регистрирует маршруты раздачи вариантов в r. Статические сегменты
// (active, eval-configs, config/..., batch) сосуществуют с /:experiment_name,
// как в FastAPI (GET /batch при этом попадает в /:experiment_name).
func Register(r gin.IRouter, d Deps) {
	h := &handlers{cache: d.Cache}
	g := r.Group(Prefix)
	g.GET("/config/:experiment_name", h.config)
	g.GET("/active", h.active)
	g.GET("/eval-configs", h.evalConfigs)
	g.POST("/batch", h.batch)
	g.GET("/:experiment_name", h.variant)
	g.GET("/", h.variant)
}

// configOrEmpty повторяет Python `config or {}`: NULL и пустой объект → {}.
func configOrEmpty(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return emptyObject
	}
	return raw
}

// jsonbKeyOrder возвращает ключи вариантов в порядке, в котором их отдаёт
// PostgreSQL JSONB (а значит и Python `list(variants.keys())`): сначала по
// длине в байтах, затем побайтово.
func jsonbKeyOrder(variants map[string]int64) []string {
	keys := make([]string, 0, len(variants))
	for k := range variants {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) < len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// variantsOrEmpty гарантирует {} вместо null для пустой карты.
func variantsOrEmpty(v map[string]int64) map[string]int64 {
	if v == nil {
		return map[string]int64{}
	}
	return v
}

// lastQuery — последнее значение query-параметра (как Starlette при повторах).
func lastQuery(c *gin.Context, key string) string {
	vals := c.QueryArray(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

// pick — вариант пользователя; для отсутствующего или неактивного эксперимента — "control".
func pick(snap model.Snapshot, userID, name string) string {
	e, ok := snap.Get(name)
	if !ok || !e.IsActive {
		return allocation.DefaultVariant
	}
	return allocation.Allocate(userID, name, e.Variants)
}

// GET /config/:experiment_name — конфиг эксперимента (в т.ч. неактивного).
func (h *handlers) config(c *gin.Context) {
	e, ok := h.cache.Snapshot().Get(c.Param("experiment_name"))
	if !ok {
		apierr.Abort(c, http.StatusNotFound, detailNotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":      e.Name,
		"variants":  variantsOrEmpty(e.Variants),
		"config":    configOrEmpty(e.Config),
		"url":       e.URL,
		"is_active": e.IsActive,
	})
}

type activeItem struct {
	Name     string   `json:"name"`
	URL      *string  `json:"url"`
	Variants []string `json:"variants"`
}

// GET /active — активные эксперименты в порядке created_at DESC.
func (h *handlers) active(c *gin.Context) {
	out := make([]activeItem, 0)
	for _, e := range h.cache.Snapshot().All() {
		if !e.IsActive {
			continue
		}
		out = append(out, activeItem{Name: e.Name, URL: e.URL, Variants: jsonbKeyOrder(e.Variants)})
	}
	c.JSON(http.StatusOK, out)
}

type evalConfig struct {
	Name     string           `json:"name"`
	Variants map[string]int64 `json:"variants"`
	Config   json.RawMessage  `json:"config"`
	URL      *string          `json:"url"`
}

// GET /eval-configs — карта активных экспериментов для Local Evaluation в Server SDK.
func (h *handlers) evalConfigs(c *gin.Context) {
	exps := make(map[string]evalConfig)
	for _, e := range h.cache.Snapshot().All() {
		if !e.IsActive {
			continue
		}
		exps[e.Name] = evalConfig{
			Name:     e.Name,
			Variants: variantsOrEmpty(e.Variants),
			Config:   configOrEmpty(e.Config),
			URL:      e.URL,
		}
	}
	c.JSON(http.StatusOK, gin.H{"experiments": exps})
}

// POST /batch — варианты пользователя для нескольких экспериментов.
func (h *handlers) batch(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBatchBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.Abort(c, http.StatusRequestEntityTooLarge, "Request Entity Too Large")
		} else {
			apierr.Abort(c, http.StatusBadRequest, "Bad Request")
		}
		return
	}
	req, verr := validate.Batch(c.GetHeader("Content-Type"), body)
	if verr != nil {
		apierr.Abort(c, verr.Status, verr.Detail)
		return
	}
	snap := h.cache.Snapshot()
	result := make(map[string]string, len(req.Experiments))
	for _, name := range req.Experiments {
		result[name] = pick(snap, req.UserID, name)
	}
	c.JSON(http.StatusOK, gin.H{"user_id": req.UserID, "variants": result})
}

// GET /:experiment_name?user_id= и GET /?experiment_name=&user_id= — вариант пользователя.
func (h *handlers) variant(c *gin.Context) {
	name := c.Param("experiment_name")
	if c.FullPath() == Prefix+"/" {
		name = lastQuery(c, "experiment_name")
	}
	userID := lastQuery(c, "user_id")
	if name == "" || userID == "" {
		apierr.Abort(c, http.StatusBadRequest, detailRequired)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"experiment_name": name,
		"user_id":         userID,
		"variant":         pick(h.cache.Snapshot(), userID, name),
	})
}
