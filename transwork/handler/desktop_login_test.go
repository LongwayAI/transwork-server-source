package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/desktop-login/start", StartDesktopLogin)
	return r
}

func TestStartDesktopLoginRejectsBadPort(t *testing.T) {
	r := setupRouter()
	body, _ := common.Marshal(map[string]int{"loopbackPort": 70000})
	req := httptest.NewRequest(http.MethodPost, "/api/desktop-login/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestStartDesktopLoginRejectsWhenOidcDisabled(t *testing.T) {
	origOIDC := *system_setting.GetOIDCSettings()
	origAddr := system_setting.ServerAddress
	t.Cleanup(func() {
		*system_setting.GetOIDCSettings() = origOIDC
		system_setting.ServerAddress = origAddr
	})
	*system_setting.GetOIDCSettings() = system_setting.OIDCSettings{Enabled: false}
	r := setupRouter()
	body, _ := common.Marshal(map[string]int{"loopbackPort": 5511})
	req := httptest.NewRequest(http.MethodPost, "/api/desktop-login/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestStartDesktopLoginReturnsAuthUrlAndState(t *testing.T) {
	origOIDC := *system_setting.GetOIDCSettings()
	origAddr := system_setting.ServerAddress
	t.Cleanup(func() {
		*system_setting.GetOIDCSettings() = origOIDC
		system_setting.ServerAddress = origAddr
	})
	*system_setting.GetOIDCSettings() = system_setting.OIDCSettings{
		Enabled:               true,
		ClientId:              "client-abc",
		AuthorizationEndpoint: "https://logto.example/oidc/auth",
	}
	system_setting.ServerAddress = "https://transwork.example"

	r := setupRouter()
	body, _ := common.Marshal(map[string]int{"loopbackPort": 5511})
	req := httptest.NewRequest(http.MethodPost, "/api/desktop-login/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		AuthURL string `json:"authUrl"`
		State   string `json:"state"`
	}
	if err := common.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State == "" {
		t.Fatalf("state empty")
	}
	if !contains(resp.AuthURL, "client_id=client-abc") ||
		!contains(resp.AuthURL, "redirect_uri=") ||
		!contains(resp.AuthURL, "state="+resp.State) ||
		!contains(resp.AuthURL, "scope=") ||
		!contains(resp.AuthURL, "response_type=code") ||
		!contains(resp.AuthURL, "prompt=login") {
		t.Fatalf("auth url missing params: %s", resp.AuthURL)
	}

	// State must be persisted with the right loopback URL.
	entry, ok := desktopLoginStates.consume(resp.State)
	if !ok {
		t.Fatalf("expected state to be stored")
	}
	if entry.LoopbackURL != "http://127.0.0.1:5511/desktop-login/callback" {
		t.Fatalf("unexpected loopback: %q", entry.LoopbackURL)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func TestCompleteDesktopLoginRejectsUnknownState(t *testing.T) {
	r := gin.New()
	r.GET("/api/desktop-login/callback", CompleteDesktopLogin)
	req := httptest.NewRequest(http.MethodGet, "/api/desktop-login/callback?code=abc&state=unknown", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown state, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestCompleteDesktopLoginPropagatesIdPError(t *testing.T) {
	desktopLoginStates.put("s1", loginStateEntry{LoopbackURL: "http://127.0.0.1:5511/desktop-login/callback"})
	r := gin.New()
	r.GET("/api/desktop-login/callback", CompleteDesktopLogin)
	req := httptest.NewRequest(http.MethodGet, "/api/desktop-login/callback?state=s1&error=access_denied", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" || !contains(loc, "error=access_denied") || !contains(loc, "state=s1") {
		t.Fatalf("redirect missing error or state: %s", loc)
	}
}

func TestCompleteDesktopLoginRedirectsOnMissingCode(t *testing.T) {
	desktopLoginStates.put("s2", loginStateEntry{LoopbackURL: "http://127.0.0.1:5512/desktop-login/callback"})
	r := gin.New()
	r.GET("/api/desktop-login/callback", CompleteDesktopLogin)
	req := httptest.NewRequest(http.MethodGet, "/api/desktop-login/callback?state=s2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !contains(loc, "error=missing_code") || !contains(loc, "state=s2") {
		t.Fatalf("redirect missing fields: %s", loc)
	}
}

func TestExchangeDesktopLoginRejectsUnknownCode(t *testing.T) {
	r := gin.New()
	r.POST("/api/desktop-login/exchange", ExchangeDesktopLoginBootstrap)
	body, _ := common.Marshal(map[string]string{"code": "nope"})
	req := httptest.NewRequest(http.MethodPost, "/api/desktop-login/exchange", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestExchangeDesktopLoginRequiresCode(t *testing.T) {
	r := gin.New()
	r.POST("/api/desktop-login/exchange", ExchangeDesktopLoginBootstrap)
	body, _ := common.Marshal(map[string]string{})
	req := httptest.NewRequest(http.MethodPost, "/api/desktop-login/exchange", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestBuildDesktopEndSessionURL(t *testing.T) {
	orig := *system_setting.GetOIDCSettings()
	t.Cleanup(func() { *system_setting.GetOIDCSettings() = orig })

	cases := []struct {
		name    string
		idToken string
		authEp  string
		wantSub []string // substrings the result must contain (or empty to require empty string)
		wantEmpty bool
	}{
		{
			name:      "empty id token returns empty",
			idToken:   "",
			authEp:    "http://localhost:3001/oidc/auth",
			wantEmpty: true,
		},
		{
			name:    "logto-shaped endpoint derives session/end",
			idToken: "abc.def.ghi",
			authEp:  "http://localhost:3001/oidc/auth",
			wantSub: []string{
				"http://localhost:3001/oidc/session/end?",
				"id_token_hint=abc.def.ghi",
				"client_id=cid",
			},
		},
		{
			name:    "trailing slash is tolerated",
			idToken: "tok",
			authEp:  "http://localhost:3001/oidc/auth/",
			wantSub: []string{"http://localhost:3001/oidc/session/end?", "id_token_hint=tok"},
		},
		{
			name:      "non-Logto endpoint shape returns empty (we won't guess)",
			idToken:   "tok",
			authEp:    "https://example.com/authorize",
			wantEmpty: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*system_setting.GetOIDCSettings() = system_setting.OIDCSettings{
				Enabled:               true,
				ClientId:              "cid",
				AuthorizationEndpoint: tc.authEp,
			}
			got := buildDesktopEndSessionURL(tc.idToken)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("expected empty, got %q", got)
				}
				return
			}
			for _, sub := range tc.wantSub {
				if !contains(got, sub) {
					t.Fatalf("missing %q in %q", sub, got)
				}
			}
		})
	}
}

func TestExchangeDesktopLoginRejectsMalformedJSON(t *testing.T) {
	r := gin.New()
	r.POST("/api/desktop-login/exchange", ExchangeDesktopLoginBootstrap)
	req := httptest.NewRequest(http.MethodPost, "/api/desktop-login/exchange",
		bytes.NewBufferString("{not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}
