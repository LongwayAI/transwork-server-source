package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

func TestAuthRedeemRejectsMissingCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/auth/redeem", func(c *gin.Context) {
		c.Set("id", 42)
		AuthRedeem(c)
	})
	body, _ := common.Marshal(map[string]string{})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/redeem", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthRedeemRejectsMissingUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/auth/redeem", AuthRedeem)
	body, _ := common.Marshal(map[string]string{"code": "ABC"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/redeem", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}
