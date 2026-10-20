package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"synaq/internal/model"
	"synaq/internal/track"

	"github.com/gin-gonic/gin"
)

type emptySnapshot struct{}

func (emptySnapshot) Get(string) (model.Experiment, bool) { return model.Experiment{}, false }
func (emptySnapshot) All() []model.Experiment             { return nil }

type emptyCache struct{}

func (emptyCache) Snapshot() model.Snapshot     { return emptySnapshot{} }
func (emptyCache) Reload(context.Context) error { return nil }

// nopProducer — приёмник событий без Ping (считается доступным).
type nopProducer struct{}

func (nopProducer) Publish(context.Context, string, []byte, []byte) error { return nil }
func (nopProducer) PublishAsync(context.Context, string, []byte, []byte, func(error)) error {
	return nil
}
func (nopProducer) Close() error { return nil }

func newTestRouter(ready bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return NewRouter(Deps{
		Tracker: track.NewHandler(nopProducer{}, "tracking_events"),
		Cache:   emptyCache{},
		Ready:   func() bool { return ready },
	})
}

func TestRouterService(t *testing.T) {
	cases := []struct {
		name, method, path string
		ready              bool
		code               int
		body               string
	}{
		{"root", "GET", "/", true, 200, `{"message":"Hello, World!"}`},
		{"health", "GET", "/health", false, 200, `{"status":"healthy"}`},
		{"readyz готов", "GET", "/readyz", true, 200, `{"status":"ready"}`},
		{"readyz не готов", "GET", "/readyz", false, 503, `{"status":"not ready"}`},
		{"404", "GET", "/nope", true, 404, `{"detail":"Not Found"}`},
		{"405", "DELETE", "/health", true, 405, `{"detail":"Method Not Allowed"}`},
		{"405 variants", "POST", "/api/v1/variants/x", true, 405, `{"detail":"Method Not Allowed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			newTestRouter(tc.ready).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.code || w.Body.String() != tc.body {
				t.Errorf("%s %s: %d %s, ожидалось %d %s", tc.method, tc.path, w.Code, w.Body.String(), tc.code, tc.body)
			}
		})
	}
}
