// Package admin — управление экспериментами (/api/v1/experiments/...), требует JWT.
//
// Контракт совпадает с apps/experiment_api/src/api/v1/admin.py: те же пути, коды
// ответов и тексты ошибок. Порядок проверок как в FastAPI: сначала аутентификация
// и роль (401/403/503), затем тело (413/400/422), затем ошибки обработчика.
//
// Список читается из Repository (PostgreSQL), а не из кэша: админке нужен
// авторитетный ответ. После каждой успешной записи кэш конфигураций перечитывается
// (read-your-writes для раздачи вариантов); ошибка перечитывания только логируется —
// запись уже зафиксирована в БД, а кэш догонит её при следующем Reload.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"synaq/internal/api/apierr"
	"synaq/internal/api/validate"
	"synaq/internal/auth"
	"synaq/internal/model"

	"github.com/gin-gonic/gin"
)

// MaxBodyBytes — максимальный размер тела POST/PATCH. Совпадает с
// client_max_body_size 1m в nginx/nginx.conf, который ограничивал Python-версию;
// превышение — 413 {"detail":"Request Entity Too Large"}.
const MaxBodyBytes = 1 << 20

// Тексты ответов — дословно как в experiment_api.
const (
	msgCreated           = "Эксперимент успешно создан"
	detailNotFound       = "Эксперимент не найден"
	detailNoFields       = "Нет полей для обновления"
	detailActiveVariants = "Нельзя менять варианты активного эксперимента: пользователи будут перераспределены. Сначала выключите эксперимент (можно в том же запросе: is_active=false)."
	detailTooLarge       = "Request Entity Too Large"
	detailBodyParse      = "There was an error parsing the body"
	detailInternal       = "Internal Server Error"
	fmtExists            = "Эксперимент '%s' уже существует!"
	fmtDeleted           = "Эксперимент '%s' удалён"
)

const (
	pathList = "/api/v1/experiments/"
	pathItem = "/api/v1/experiments/:name"
)

// Deps — зависимости обработчиков управления экспериментами.
type Deps struct {
	Repo  model.Repository
	Cache model.Cache
	Auth  *auth.Verifier
}

type handler struct {
	repo  model.Repository
	cache model.Cache
}

// Register регистрирует маршруты управления экспериментами в r.
// Создание, изменение и удаление — роль admin; список — admin или analyst.
func Register(r gin.IRouter, d Deps) {
	h := &handler{repo: d.Repo, cache: d.Cache}
	write := auth.RequireRoles(d.Auth, auth.RoleAdmin)
	read := auth.RequireRoles(d.Auth, auth.RoleAdmin, auth.RoleAnalyst)

	r.POST(pathList, write, h.create)
	r.GET(pathList, read, h.list)
	r.PATCH(pathItem, write, h.update)
	r.DELETE(pathItem, write, h.delete)
}

// experimentView — элемент ответа списка и PATCH (как _experiment_to_dict в Python).
type experimentView struct {
	Name        string           `json:"name"`
	Variants    map[string]int64 `json:"variants"`
	Config      json.RawMessage  `json:"config"`
	URL         *string          `json:"url"`
	IsActive    bool             `json:"is_active"`
	TotalWeight int64            `json:"total_weight"`
}

func toView(e model.Experiment) experimentView {
	var total int64
	for _, w := range e.Variants {
		total += w
	}
	cfg := e.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage("null")
	}
	variants := e.Variants
	if variants == nil {
		variants = map[string]int64{}
	}
	return experimentView{
		Name: e.Name, Variants: variants, Config: cfg, URL: e.URL,
		IsActive: e.IsActive, TotalWeight: total,
	}
}

// readBody читает тело с ограничением размера. ok == false — ответ уже отправлен.
func readBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apierr.Abort(c, http.StatusRequestEntityTooLarge, detailTooLarge)
			return nil, false
		}
		apierr.Abort(c, http.StatusBadRequest, detailBodyParse)
		return nil, false
	}
	return body, true
}

// internalError логирует причину и отвечает 500 без подробностей.
func internalError(c *gin.Context, op string, err error) {
	log.Printf("admin: %s: %v", op, err)
	apierr.Abort(c, http.StatusInternalServerError, detailInternal)
}

// reload перечитывает кэш после записи; ошибка не влияет на ответ.
func (h *handler) reload(ctx context.Context, op, name string) {
	if err := h.cache.Reload(ctx); err != nil {
		log.Printf("admin: не удалось обновить кэш конфигураций после %s '%s': %v", op, name, err)
	}
}

func (h *handler) create(c *gin.Context) {
	body, ok := readBody(c)
	if !ok {
		return
	}
	exp, verr := validate.ExperimentCreate(c.GetHeader("Content-Type"), body)
	if verr != nil {
		apierr.Abort(c, verr.Status, verr.Detail)
		return
	}
	if err := h.repo.Create(c.Request.Context(), exp); err != nil {
		if errors.Is(err, model.ErrExists) {
			apierr.Abort(c, http.StatusBadRequest, fmt.Sprintf(fmtExists, exp.Name))
			return
		}
		internalError(c, "создание", err)
		return
	}
	h.reload(c.Request.Context(), "создания", exp.Name)
	c.JSON(http.StatusCreated, gin.H{"message": msgCreated, "name": exp.Name})
}

func (h *handler) list(c *gin.Context) {
	exps, err := h.repo.List(c.Request.Context())
	if err != nil {
		internalError(c, "список", err)
		return
	}
	out := make([]experimentView, 0, len(exps))
	for _, e := range exps {
		out = append(out, toView(e))
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) update(c *gin.Context) {
	name := c.Param("name")
	body, ok := readBody(c)
	if !ok {
		return
	}
	patch, verr := validate.ExperimentUpdate(c.GetHeader("Content-Type"), body)
	if verr != nil {
		apierr.Abort(c, verr.Status, verr.Detail)
		return
	}
	if patch.Empty() {
		apierr.Abort(c, http.StatusBadRequest, detailNoFields)
		return
	}
	updated, err := h.repo.Update(c.Request.Context(), name, patch)
	switch {
	case errors.Is(err, model.ErrActiveVariantsChange):
		apierr.Abort(c, http.StatusConflict, detailActiveVariants)
		return
	case errors.Is(err, model.ErrNotFound):
		apierr.Abort(c, http.StatusNotFound, detailNotFound)
		return
	case err != nil:
		internalError(c, "обновление", err)
		return
	}
	h.reload(c.Request.Context(), "обновления", name)
	c.JSON(http.StatusOK, toView(updated))
}

func (h *handler) delete(c *gin.Context) {
	name := c.Param("name")
	err := h.repo.Delete(c.Request.Context(), name)
	switch {
	case errors.Is(err, model.ErrNotFound):
		apierr.Abort(c, http.StatusNotFound, detailNotFound)
		return
	case err != nil:
		internalError(c, "удаление", err)
		return
	}
	h.reload(c.Request.Context(), "удаления", name)
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf(fmtDeleted, name)})
}
