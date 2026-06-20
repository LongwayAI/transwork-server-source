package transwork

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/setting"
	twhandler "github.com/QuantumNous/new-api/transwork/handler"
	"github.com/gin-gonic/gin"
)

func TestDesktopTopupRoutes_RequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api"))

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/desktop/topup/info"},
		{http.MethodPost, "/api/desktop/topup/amount"},
		{http.MethodPost, "/api/desktop/topup/pay"},
		{http.MethodPost, "/api/desktop/topup/stripe/pay"},
		{http.MethodPost, "/api/desktop/topup/creem/pay"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 without token, got %d", tc.method, tc.path, w.Code)
		}
	}
}

// TestRechargeGate verifies the server-side gate: requests are blocked with 403
// when recharge is disabled by design (the shipped default), and pass through
// once both gates are on. The enabled case guards against a future refactor
// that breaks the gate into an always-403 (or always-open) state.
func TestRechargeGate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	call := func() int {
		r := gin.New()
		r.POST("/x", rechargeGate(), func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		return w.Code
	}

	// Disabled by design (master switch off) -> 403, downstream handler skipped.
	restore := twhandler.SetRechargeMasterSwitchForTest(false)
	if code := call(); code != http.StatusForbidden {
		restore()
		t.Fatalf("expected 403 when recharge disabled, got %d", code)
	}
	restore()

	// Both gates on (master switch + a configured Stripe provider) -> passes through.
	restore = twhandler.SetRechargeMasterSwitchForTest(true)
	defer restore()
	prevKey, prevHook, prevPrice := setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId
	setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = "sk_test", "whsec_test", "price_test"
	defer func() {
		setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = prevKey, prevHook, prevPrice
	}()
	if code := call(); code != http.StatusOK {
		t.Fatalf("expected 200 when recharge enabled, got %d", code)
	}
}
