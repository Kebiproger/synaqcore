package apierr

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestInstall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Install(r)
	r.GET("/items/", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/fail", func(c *gin.Context) { AbortValidation(c, []string{"x"}) })

	tests := []struct {
		method, path string
		status       int
		body         string
	}{
		{http.MethodGet, "/nope", http.StatusNotFound, `{"detail":"Not Found"}`},
		{http.MethodDelete, "/items/", http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`},
		{http.MethodPost, "/fail", http.StatusUnprocessableEntity, `{"detail":["x"]}`},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
		if w.Code != tt.status || w.Body.String() != tt.body {
			t.Errorf("%s %s = %d %s, want %d %s", tt.method, tt.path, w.Code, w.Body, tt.status, tt.body)
		}
		if tt.status == http.StatusMethodNotAllowed && w.Header().Get("Allow") == "" {
			t.Errorf("405 без заголовка Allow")
		}
	}
}
