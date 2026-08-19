package handler

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/transwork/credits"
	twmodel "github.com/QuantumNous/new-api/transwork/model"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/billingportal/session"
	"gorm.io/gorm"
)

// stripePortalCreator abstracts the single external Stripe call the portal
// endpoint makes (billingportal/session.New) so tests can inject a mock and
// assert exactly which customer id is passed — the O9 security invariant.
type stripePortalCreator interface {
	Create(customerId, returnURL string) (string, error)
}

type liveStripePortalCreator struct{}

func (liveStripePortalCreator) Create(customerId, returnURL string) (string, error) {
	stripe.Key = setting.StripeApiSecret
	s, err := session.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerId),
		ReturnURL: stripe.String(returnURL),
	})
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

// portalCreator is a package var so tests can swap in a mock.
var portalCreator stripePortalCreator = liveStripePortalCreator{}

// findRecurringLink returns the user's live (active/past_due) Stripe subscription
// link, if any. A canceled link is not "recurring": it no longer bills, so it
// neither drives the manage button (B6) nor grants portal access. The lookup is
// keyed strictly on the passed userId — never on a request parameter (O9).
func findRecurringLink(userId int) (*twmodel.StripeSubscriptionLink, bool) {
	if userId <= 0 {
		return nil, false
	}
	var link twmodel.StripeSubscriptionLink
	err := model.DB.Where("user_id = ? AND status IN ?", userId,
		[]string{twmodel.LinkStatusActive, twmodel.LinkStatusPastDue}).
		Order("id desc").First(&link).Error
	if err != nil {
		return nil, false
	}
	return &link, true
}

// desktopSubscriptionStatus is the shape the desktop profile pop-up reads (B6/F3)
// so it never has to merge the upstream /subscription/self payload with link rows.
type desktopSubscriptionStatus struct {
	Plan             string `json:"plan"`
	EndTime          int64  `json:"end_time"`
	AutoRenew        bool   `json:"auto_renew"`
	CurrentPeriodEnd int64  `json:"current_period_end"`
	IsRecurring      bool   `json:"is_recurring"`
	// DurationUnit is the plan's billing cycle (month/year/…), surfaced so the
	// desktop profile can label the plan with a "monthly"/"yearly" badge. Empty
	// when no plan is resolved; the client hides the badge in that case.
	DurationUnit string `json:"duration_unit"`
	// Subscription-bucket allowance, surfaced so the profile can show the monthly
	// (resetting) credit alongside the permanent wallet. Populated only when the
	// user's most-recent subscription is currently active. Amounts are in Gressio
	// credits (same unit as auth-check's walletCredits), so the two buckets sum.
	HasAllowance     bool `json:"has_allowance"`     // an active subscription bucket exists
	Unlimited        bool `json:"unlimited"`         // plan grants unlimited credit (AmountTotal==0)
	MonthlyRemaining int  `json:"monthly_remaining"` // credits left in the bucket this period
	MonthlyTotal     int  `json:"monthly_total"`     // credits granted per period
}

// GetDesktopSubscriptionStatus reports the caller's current plan / renewal state
// (design B6). It resolves the user strictly from c.GetInt("id") (set by
// TokenAuth) and joins their most-recent UserSubscription to their overlay link.
// is_recurring is false (and the desktop hides the manage button) when the user
// has no live recurring link.
func GetDesktopSubscriptionStatus(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"message": "no user in context"})
		return
	}

	var resp desktopSubscriptionStatus
	summaries, err := model.GetAllUserSubscriptions(id)
	if err != nil {
		// GORM Find returns (nil, nil) for "no rows", so a non-nil error here is a
		// genuine DB failure, not the legitimate "no subscriptions" case. Fail loud
		// rather than silently returning HTTP 200 with an empty plan.
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅状态查询失败 user_id=%d error=%q", id, err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"message": "failed to load subscription status"})
		return
	}
	if len(summaries) > 0 {
		// GetAllUserSubscriptions orders by end_time desc, id desc — [0] is the
		// most-recent subscription, which is what the pop-up displays.
		if sub := summaries[0].Subscription; sub != nil {
			resp.EndTime = sub.EndTime
			// Read the plan title with a direct PK lookup rather than the cached
			// model.GetSubscriptionPlanById: this status display should reflect the
			// current title, not a value cached for up to the plan-cache TTL.
			var plan model.SubscriptionPlan
			if err := model.DB.Where("id = ?", sub.PlanId).First(&plan).Error; err == nil {
				resp.Plan = plan.Title
				resp.DurationUnit = plan.DurationUnit
			}
			// Only surface the allowance when the bucket is live: an expired or
			// cancelled subscription grants nothing this period, so showing its
			// old number would overstate the user's available credit.
			if sub.Status == "active" && sub.EndTime > common.GetTimestamp() {
				resp.HasAllowance = true
				if sub.AmountTotal == 0 {
					resp.Unlimited = true // AmountTotal==0 is the plan's "unlimited" sentinel
				} else {
					remaining := sub.AmountTotal - sub.AmountUsed
					if remaining < 0 {
						remaining = 0
					}
					resp.MonthlyTotal = int(math.Floor(credits.FromQuota(int(sub.AmountTotal))))
					resp.MonthlyRemaining = int(math.Floor(credits.FromQuota(int(remaining))))
				}
			}
		}
	}
	if link, ok := findRecurringLink(id); ok {
		resp.IsRecurring = true
		resp.AutoRenew = link.AutoRenew
		resp.CurrentPeriodEnd = link.CurrentPeriodEnd
	}
	c.JSON(http.StatusOK, resp)
}

// CreateSubscriptionPortalSession opens Stripe's hosted Customer Portal for the
// caller (design B6). SECURITY-CRITICAL (O9): the Stripe customer id is derived
// STRICTLY from the authenticated user id set by TokenAuth. We deliberately never
// read a user_id / customer_id / subscription_id from the request body or query —
// accepting one would let any authenticated caller open another user's billing
// portal (view card, cancel sub). Returns 404 (not 500) when the user has no
// live recurring Stripe subscription.
func CreateSubscriptionPortalSession(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"message": "no user in context"})
		return
	}

	link, ok := findRecurringLink(id)
	if !ok || link.StripeCustomerId == "" {
		c.JSON(http.StatusNotFound, gin.H{"message": "no recurring subscription"})
		return
	}

	url, err := portalCreator.Create(link.StripeCustomerId, PortalReturnURL())
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅门户创建失败 user_id=%d error=%q", id, err.Error()))
		c.JSON(http.StatusBadGateway, gin.H{"message": "failed to create portal session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}

type cancelStripeSubscriptionRequest struct {
	UserSubscriptionId   int    `json:"user_subscription_id"`
	StripeSubscriptionId string `json:"stripe_subscription_id"`
}

// CancelStripeSubscription is the admin-side propagation of a local lapse into
// Stripe (design B7). Without it, invalidating a UserSubscription locally would
// leave Stripe billing a user who no longer has access — the Translide failure
// from the admin side. It cancels the Stripe subscription and marks the overlay
// link canceled; the resulting customer.subscription.deleted flows through B3
// idempotently. Already-canceled links are a no-op returning success.
func CancelStripeSubscription(c *gin.Context) {
	var req cancelStripeSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数错误")
		return
	}
	if req.UserSubscriptionId <= 0 && req.StripeSubscriptionId == "" {
		common.ApiErrorMsg(c, "参数错误")
		return
	}

	var link twmodel.StripeSubscriptionLink
	q := model.DB
	if req.StripeSubscriptionId != "" {
		q = q.Where("stripe_subscription_id = ?", req.StripeSubscriptionId)
	} else {
		q = q.Where("user_subscription_id = ?", req.UserSubscriptionId)
	}
	if err := q.First(&link).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorMsg(c, "未找到订阅映射")
			return
		}
		common.ApiError(c, err)
		return
	}

	// Idempotent: an already-canceled link needs no Stripe call and no write.
	if link.Status == twmodel.LinkStatusCanceled {
		common.ApiSuccess(c, nil)
		return
	}

	// Unlike the webhook's superseded-sub cancel (O7, non-fatal), an admin action
	// is synchronous: if Stripe cancel fails we must NOT mark the link canceled
	// locally, or the app and Stripe diverge into "access removed, still billing".
	// Surface the error so the operator retries.
	if err := subscriptionCanceler.Cancel(link.StripeSubscriptionId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DB.Model(&twmodel.StripeSubscriptionLink{}).
		Where("id = ?", link.Id).
		Updates(map[string]any{
			"status":     twmodel.LinkStatusCanceled,
			"auto_renew": false,
			"updated_at": common.GetTimestamp(),
		}).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// ListStripeSubscriptionLinks returns the overlay link rows for a user (design
// F2). The admin per-user modal merges them client-side by user_subscription_id,
// keeping the upstream subscriptions endpoint untouched and the join in the
// overlay.
func ListStripeSubscriptionLinks(c *gin.Context) {
	userId, _ := strconv.Atoi(c.Query("user_id"))
	if userId <= 0 {
		common.ApiErrorMsg(c, "无效的用户ID")
		return
	}
	var links []twmodel.StripeSubscriptionLink
	if err := model.DB.Where("user_id = ?", userId).
		Order("id desc").Find(&links).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	// Best-effort lazy backfill (F2): a link captured at checkout has
	// user_subscription_id=0 until the first renewal resolves it, so the per-user
	// modal cannot merge it against the upstream subscription rows. Resolve and
	// persist it now so a fresh purchase merges immediately. A resolve/persist
	// error must NOT fail the whole list response — log and keep the row as-is.
	for i := range links {
		if links[i].UserSubscriptionId != 0 {
			continue
		}
		resolved, err := resolveUserSubscriptionId(model.DB, &links[i])
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅链接补全 user_subscription_id 失败 link_id=%d error=%q", links[i].Id, err.Error()))
			continue
		}
		if resolved == 0 {
			continue
		}
		if err := model.DB.Model(&twmodel.StripeSubscriptionLink{}).
			Where("id = ?", links[i].Id).
			Update("user_subscription_id", resolved).Error; err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅链接持久化 user_subscription_id 失败 link_id=%d error=%q", links[i].Id, err.Error()))
			continue
		}
		links[i].UserSubscriptionId = resolved
	}
	common.ApiSuccess(c, links)
}
