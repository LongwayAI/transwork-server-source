package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAuthLogoutRejectsMissingContextFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// No id/token_id set: simulates an unauth or buggy upstream wiring.
	r.POST("/api/auth/logout", AuthLogout)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}
