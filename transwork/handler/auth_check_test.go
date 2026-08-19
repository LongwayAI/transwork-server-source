package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAuthCheckEchoesContextFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Simulate what TokenAuth() does upstream.
	r.GET("/api/auth/check", func(c *gin.Context) {
		c.Set("id", 42)
		c.Set("token_quota", 1000)
		c.Set("token_unlimited_quota", false)
		AuthCheck(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// userQuota is enriched only when DB is available; zero value is always present in JSON.
	for _, want := range []string{`"userId":42`, `"remainQuota":1000`, `"unlimitedQuota":false`, `"userQuota":0`} {
		if !contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestAuthCheckRejectsMissingUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/auth/check", AuthCheck)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthCheckOmitsRemainQuotaForUnlimitedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/auth/check", func(c *gin.Context) {
		c.Set("id", 42)
		c.Set("token_unlimited_quota", true)
		AuthCheck(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, `"remainQuota":null`) || !contains(body, `"unlimitedQuota":true`) {
		t.Fatalf("body missing fields: %s", body)
	}
}
