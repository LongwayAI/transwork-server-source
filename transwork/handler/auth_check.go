package handler

import (
	"math"
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/transwork/credits"
	"github.com/gin-gonic/gin"
)

type authCheckResponse struct {
	UserID             int    `json:"userId"`
	Email              string `json:"email"`
	DisplayName        string `json:"displayName"`
	Group              string `json:"group"`
	UserQuota          int    `json:"userQuota"`     // per-user quota; gate invite-redemption UI when 0
	WalletCredits      int    `json:"walletCredits"` // wallet quota in Gressio credits; the "purchased" bucket
	DisplayQuota       string `json:"displayQuota"`  // pre-formatted Gressio credits display
	RemainQuota        *int   `json:"remainQuota"`   // per-token cap; nil → no cap (use userQuota instead)
	UnlimitedQuota     bool   `json:"unlimitedQuota"`
	TokenExpiresAt     int64  `json:"tokenExpiresAt"`     // unix seconds; -1 means "never expires" or "unknown"
	EnableRecharge     bool   `json:"enableRecharge"`     // gate2 master switch AND a configured provider
	EnableSubscription bool   `json:"enableSubscription"` // gate2 master switch AND a configured Stripe secret
	InviteOptional     bool   `json:"inviteOptional"`     // admin Payment setting: invite code optional → offer free-credit Continue
}

// AuthCheck reports the current token's identity + entitlement state. Must be
// wired behind middleware.TokenAuth() so the standard quota/expiry/IP checks
// have already run.
func AuthCheck(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user in context"})
		return
	}

	resp := authCheckResponse{
		UserID:         id,
		UnlimitedQuota: c.GetBool("token_unlimited_quota"),
		TokenExpiresAt: -1, // sentinel: "unknown or never-expires" — overwritten by enrichWithToken on success
	}
	if !resp.UnlimitedQuota {
		q := c.GetInt("token_quota")
		resp.RemainQuota = &q
	}

	enrichWithUser(id, &resp)
	enrichWithToken(c.GetInt("token_id"), &resp)
	resp.EnableRecharge = RechargeEnabled()
	resp.EnableSubscription = SubscriptionEnabled()
	resp.InviteOptional = InviteOptional()
	c.JSON(http.StatusOK, resp)
}

func enrichWithUser(id int, resp *authCheckResponse) {
	if model.DB == nil {
		return
	}
	user, err := model.GetUserById(id, false)
	if err != nil || user.Id == 0 {
		return
	}
	resp.Email = user.Email
	resp.DisplayName = user.DisplayName
	resp.Group = user.Group
	resp.UserQuota = user.Quota
	resp.WalletCredits = int(math.Floor(credits.FromQuota(user.Quota)))
	resp.DisplayQuota = formatDisplayQuota(user.Quota)
}

func enrichWithToken(tokenID int, resp *authCheckResponse) {
	if model.DB == nil || tokenID == 0 {
		return
	}
	tok, err := model.GetTokenById(tokenID)
	if err != nil || tok.Id == 0 {
		return
	}
	resp.TokenExpiresAt = tok.ExpiredTime
}

func formatDisplayQuota(quota int) string {
	return credits.FormatQuota(quota)
}
