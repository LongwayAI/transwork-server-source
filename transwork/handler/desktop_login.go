package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

type desktopLoginStartRequest struct {
	LoopbackPort int `json:"loopbackPort"`
}

type desktopLoginStartResponse struct {
	AuthURL string `json:"authUrl"`
	State   string `json:"state"`
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// StartDesktopLogin issues an OIDC authorization URL for the desktop client.
// The client opens it in the system browser; Logto redirects back to
// /api/desktop-login/callback with the auth code.
func StartDesktopLogin(c *gin.Context) {
	var req desktopLoginStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	// Reject privileged ports (<1024) and out-of-range values; loopback-only enforcement is the OS's responsibility.
	if req.LoopbackPort < 1024 || req.LoopbackPort > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "loopbackPort out of range"})
		return
	}

	settings := system_setting.GetOIDCSettings()
	if !settings.Enabled || settings.ClientId == "" || settings.AuthorizationEndpoint == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "OIDC not configured"})
		return
	}

	state, err := randomHex(16)
	if err != nil {
		common.SysLog("desktop-login: state gen: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "state gen"})
		return
	}

	loopback := fmt.Sprintf("http://127.0.0.1:%d/desktop-login/callback", req.LoopbackPort)
	desktopLoginStates.put(state, loginStateEntry{LoopbackURL: loopback})

	redirectURI := system_setting.ServerAddress + "/api/desktop-login/callback"
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", settings.ClientId)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	// Force the IdP to re-authenticate on every desktop sign-in so the user
	// can land on a different account after signing out. We use `prompt=login`
	// (forced re-auth) rather than `prompt=select_account`: Logto, built on
	// node-oidc-provider, ships with `prompts: ['login', 'none', 'consent']`
	// by default and rejects `select_account` with `invalid_request`. If your
	// Logto is configured to federate to Google, this forces a fresh Logto
	// session but Google may still auto-pick its sole signed-in account —
	// signing out of Google in the browser (or using a private window) is
	// the only way to also reset that upstream session.
	q.Set("prompt", "login")

	base := strings.TrimRight(settings.AuthorizationEndpoint, "?")
	authURL := base + "?" + q.Encode()

	c.JSON(http.StatusOK, desktopLoginStartResponse{AuthURL: authURL, State: state})
}

// CompleteDesktopLogin handles the OIDC redirect from Logto. Validates state,
// exchanges the auth code for user info, finds-or-creates the local user, and
// redirects the browser to the desktop loopback with a single-use bootstrap code.
func CompleteDesktopLogin(c *gin.Context) {
	state := c.Query("state")
	code := c.Query("code")
	oidcErr := c.Query("error")

	if state == "" {
		c.String(http.StatusBadRequest, "missing state")
		return
	}

	entry, ok := desktopLoginStates.consume(state)
	if !ok {
		c.String(http.StatusBadRequest, "unknown or expired state")
		return
	}

	// IdP-side error (e.g., user cancelled): forward to loopback so the desktop app can handle it.
	if oidcErr != "" {
		redirectErrToLoopback(c, entry.LoopbackURL, state, oidcErr)
		return
	}

	if code == "" {
		redirectErrToLoopback(c, entry.LoopbackURL, state, "missing_code")
		return
	}

	provider := oauth.GetProvider("oidc")
	if provider == nil || !provider.IsEnabled() {
		redirectErrToLoopback(c, entry.LoopbackURL, state, "oidc_not_enabled")
		return
	}

	accessToken, idToken, err := exchangeOidcCodeForDesktop(c.Request.Context(), code)
	if err != nil {
		common.SysLog("desktop-login exchange failed: " + err.Error())
		redirectErrToLoopback(c, entry.LoopbackURL, state, "exchange_failed")
		return
	}
	info, err := provider.GetUserInfo(c.Request.Context(), &oauth.OAuthToken{AccessToken: accessToken})
	if err != nil {
		common.SysLog("desktop-login userinfo failed: " + err.Error())
		redirectErrToLoopback(c, entry.LoopbackURL, state, "userinfo_failed")
		return
	}

	user, err := findOrCreateOidcUserForDesktop(info.ProviderUserID, info.Email, info.DisplayName)
	if err != nil {
		common.SysLog("desktop-login user resolve failed: " + err.Error())
		redirectErrToLoopback(c, entry.LoopbackURL, state, "server_error")
		return
	}
	if user.Status != common.UserStatusEnabled {
		redirectErrToLoopback(c, entry.LoopbackURL, state, "user_disabled")
		return
	}

	bootstrap, err := randomHex(32)
	if err != nil {
		common.SysLog("desktop-login bootstrap gen failed: " + err.Error())
		redirectErrToLoopback(c, entry.LoopbackURL, state, "server_error")
		return
	}
	bootstrapCodes.put(bootstrap, bootstrapEntry{UserID: user.Id, IdToken: idToken})

	q := url.Values{}
	q.Set("code", bootstrap)
	q.Set("state", state)
	redirect := entry.LoopbackURL + "?" + q.Encode()
	c.Redirect(http.StatusFound, redirect)
}

// exchangeOidcCodeForDesktop performs the OAuth 2.0 authorization_code → access_token
// exchange for the desktop sign-in flow. We cannot reuse upstream
// oauth.OIDCProvider.ExchangeToken because it hardcodes redirect_uri to
// "<ServerAddress>/oauth/oidc" (the web sign-in path); the desktop flow uses
// "<ServerAddress>/api/desktop-login/callback" during /authorize, and OAuth 2.0
// (RFC 6749 §4.1.3) requires the redirect_uri at token-exchange time to match
// the value sent at authorization time exactly.
//
// Returns (accessToken, idToken, error). The id_token is captured for use as
// id_token_hint when ending the IdP session during sign-out. It may be empty
// if the IdP omits it (e.g. when the "openid" scope is dropped); callers must
// handle the empty-string case.
func exchangeOidcCodeForDesktop(ctx context.Context, code string) (string, string, error) {
	settings := system_setting.GetOIDCSettings()
	if settings.TokenEndpoint == "" {
		return "", "", errors.New("token endpoint not configured")
	}
	values := url.Values{}
	values.Set("client_id", settings.ClientId)
	values.Set("client_secret", settings.ClientSecret)
	values.Set("code", code)
	values.Set("grant_type", "authorization_code")
	values.Set("redirect_uri", system_setting.ServerAddress+"/api/desktop-login/callback")

	req, err := http.NewRequestWithContext(ctx, "POST", settings.TokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("token endpoint returned %d: %s", res.StatusCode, string(body))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := common.Unmarshal(body, &tr); err != nil {
		return "", "", err
	}
	if tr.AccessToken == "" {
		return "", "", errors.New("empty access_token in token endpoint response")
	}
	return tr.AccessToken, tr.IDToken, nil
}

func findOrCreateOidcUserForDesktop(oidcID, email, displayName string) (*model.User, error) {
	if oidcID == "" {
		return nil, errors.New("oidc id empty")
	}
	if model.IsOidcIdAlreadyTaken(oidcID) {
		user := &model.User{OidcId: oidcID}
		if err := user.FillUserByOidcId(); err != nil {
			return nil, err
		}
		if user.Id == 0 {
			return nil, errors.New("oidc account was deleted")
		}
		return user, nil
	}
	if !common.RegisterEnabled {
		return nil, errors.New("registration disabled")
	}
	newUser := &model.User{
		OidcId:      oidcID,
		Email:       email,
		Username:    "oidc_" + randomShortSuffix(),
		DisplayName: displayName,
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
	}
	if newUser.DisplayName == "" {
		// DisplayName has validate:"max=20"; truncate by rune to stay safe with non-ASCII (SMTPUTF8) emails.
		runes := []rune(email)
		if len(runes) > 20 {
			newUser.DisplayName = string(runes[:20])
		} else {
			newUser.DisplayName = email
		}
	}
	if err := newUser.Insert(0); err != nil {
		return nil, err
	}
	return newUser, nil
}

// randomShortSuffix returns 14 hex chars (56 bits of entropy). Combined with
// the "oidc_" prefix this stays within Username's validate:"max=20" limit
// (5 + 14 = 19). Collisions are vanishingly improbable and the unique index
// on Username catches the impossible case. We deliberately do not depend on
// model.GetMaxUserId() to avoid a race between two concurrent registrations.
func randomShortSuffix() string {
	s, err := randomHex(7)
	if err != nil {
		// Fallback to a unix-nanos-based suffix; collision still highly unlikely.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return s
}

// buildDesktopEndSessionURL returns the IdP's RP-initiated logout URL with
// id_token_hint set, or empty string if no end-session endpoint can be
// determined. The endpoint is derived from the configured
// AuthorizationEndpoint by replacing a trailing "/auth" with "/session/end"
// (Logto's standard layout — e.g. ".../oidc/auth" → ".../oidc/session/end").
// If the AuthorizationEndpoint doesn't end with "/auth", we return "" rather
// than guess; in that case the desktop client just clears its local token and
// the IdP session lives on until it expires naturally.
func buildDesktopEndSessionURL(idToken string) string {
	if idToken == "" {
		return ""
	}
	settings := system_setting.GetOIDCSettings()
	authEndpoint := strings.TrimRight(settings.AuthorizationEndpoint, "/")
	if !strings.HasSuffix(authEndpoint, "/auth") {
		return ""
	}
	endSession := strings.TrimSuffix(authEndpoint, "/auth") + "/session/end"
	q := url.Values{}
	q.Set("id_token_hint", idToken)
	if settings.ClientId != "" {
		q.Set("client_id", settings.ClientId)
	}
	return endSession + "?" + q.Encode()
}

func redirectErrToLoopback(c *gin.Context, loopback, state, errCode string) {
	q := url.Values{}
	q.Set("error", errCode)
	q.Set("state", state)
	c.Redirect(http.StatusFound, loopback+"?"+q.Encode())
}

type exchangeRequest struct {
	Code string `json:"code"`
}

type exchangeResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
	UserID    int    `json:"userId"`
}

const desktopTokenLifetimeSeconds int64 = 90 * 24 * 60 * 60 // 90 days

// ExchangeDesktopLoginBootstrap turns a single-use bootstrap code into a
// long-lived bearer token row in the existing Token model.
func ExchangeDesktopLoginBootstrap(c *gin.Context) {
	var req exchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
		return
	}
	entry, ok := bootstrapCodes.consume(req.Code)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired code"})
		return
	}
	user, err := model.GetUserById(entry.UserID, false)
	if err != nil {
		common.SysLog("desktop-login user lookup failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user lookup failed"})
		return
	}
	if user.Id == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "user not found"})
		return
	}
	if user.Status != common.UserStatusEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "user disabled"})
		return
	}
	model.UpdateUserLastLoginAt(user.Id)

	key, err := randomHex(24) // 24 bytes → 48 hex chars; crypto/rand-backed; no '-' so TokenAuth keeps it intact
	if err != nil {
		common.SysLog("desktop-login key gen failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "key gen failed"})
		return
	}
	now := time.Now().Unix()
	tok := &model.Token{
		UserId:         user.Id,
		Key:            key,
		Status:         common.TokenStatusEnabled,
		Name:           "Desktop Login",
		CreatedTime:    now,
		AccessedTime:   now,
		ExpiredTime:    now + desktopTokenLifetimeSeconds,
		RemainQuota:    0,
		UnlimitedQuota: true, // per-user quota lives in User.Quota; this token has no per-token cap
		Group:          "",   // mirror upstream OAuth: leave group empty for programmatic tokens
	}
	if err = tok.Insert(); err != nil {
		common.SysLog("desktop-login token insert failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token insert failed"})
		return
	}
	if entry.IdToken != "" {
		idTokenByTokenID.Store(tok.Id, entry.IdToken)
	}
	c.JSON(http.StatusOK, exchangeResponse{
		Token:     tok.Key,
		ExpiresAt: tok.ExpiredTime,
		UserID:    user.Id,
	})
}
