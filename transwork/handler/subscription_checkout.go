package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/thanhpk/randstr"
	"gorm.io/gorm"
)

// desktopSubscriptionPlan is the slim plan shape the desktop paywall reads. It
// deliberately exposes ONLY the display fields the client needs — never the
// StripePriceId or internal purchase caps — so the raw price id stays server-side.
type desktopSubscriptionPlan struct {
	Id            int     `json:"id"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle"`
	PriceAmount   float64 `json:"price_amount"`
	Currency      string  `json:"currency"`
	DurationUnit  string  `json:"duration_unit"`
	DurationValue int     `json:"duration_value"`
}

// ListDesktopSubscriptionPlans returns the purchasable subscription plans for the
// desktop paywall: enabled plans that carry a StripePriceId (a plan without one
// cannot be checked out via Stripe, so listing it would only produce a dead
// button). It mirrors controller.GetSubscriptionPlans' enabled+sort query, then
// filters to Stripe-payable plans and projects the display-only fields.
func ListDesktopSubscriptionPlans(c *gin.Context) {
	var plans []model.SubscriptionPlan
	if err := model.DB.Where("enabled = ?", true).
		Order("sort_order desc, id desc").Find(&plans).Error; err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅套餐列表查询失败 error=%q", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"message": "failed to load subscription plans"})
		return
	}
	result := make([]desktopSubscriptionPlan, 0, len(plans))
	for _, p := range plans {
		if p.StripePriceId == "" {
			continue
		}
		result = append(result, desktopSubscriptionPlan{
			Id:            p.Id,
			Title:         p.Title,
			Subtitle:      p.Subtitle,
			PriceAmount:   p.PriceAmount,
			Currency:      p.Currency,
			DurationUnit:  p.DurationUnit,
			DurationValue: p.DurationValue,
		})
	}
	c.JSON(http.StatusOK, gin.H{"plans": result})
}

// stripeCheckoutCreator abstracts the single external Stripe call the checkout
// endpoint makes (checkout/session.New) so tests can inject a mock and assert
// exactly which customer/price/reference is passed — mirroring portalCreator.
type stripeCheckoutCreator interface {
	Create(referenceId, customerId, email, priceId, returnURL string) (string, error)
}

type liveStripeCheckoutCreator struct{}

func (liveStripeCheckoutCreator) Create(referenceId, customerId, email, priceId, returnURL string) (string, error) {
	stripe.Key = setting.StripeApiSecret

	params := &stripe.CheckoutSessionParams{
		ClientReferenceID: stripe.String(referenceId),
		// CRITICAL (vs upstream genStripeSubscriptionLink): the desktop flow must NOT
		// bounce the browser back to the web console (system_setting.ServerAddress +
		// /console/topup). It returns to the branded gressio.ai page instead so the
		// desktop user never lands on the operator dashboard.
		SuccessURL: stripe.String(withStatusParam(returnURL, "success")),
		CancelURL:  stripe.String(withStatusParam(returnURL, "cancel")),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceId),
				Quantity: stripe.Int64(1),
			},
		},
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
	}

	if "" == customerId {
		if "" != email {
			params.CustomerEmail = stripe.String(email)
		}
		// Do NOT set CustomerCreation here. Unlike the one-time recharge flow
		// (controller/topup_stripe.go), this session is mode=subscription, and
		// Stripe rejects customer_creation outside payment mode ("`customer_creation`
		// can only be used in `payment` mode."). In subscription mode Stripe always
		// creates the Customer automatically, so the param is both illegal and moot.
	} else {
		params.Customer = stripe.String(customerId)
	}

	result, err := session.New(params)
	if err != nil {
		return "", err
	}
	return result.URL, nil
}

// checkoutCreator is a package var so tests can swap in a mock.
var checkoutCreator stripeCheckoutCreator = liveStripeCheckoutCreator{}

// withStatusParam appends a status marker to the return URL so the gressio.ai
// page can tell a completed checkout from an abandoned one, tolerating a URL that
// already carries a query string.
func withStatusParam(base, status string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "status=" + status
}

type desktopSubscriptionCheckoutRequest struct {
	PlanId int `json:"plan_id"`
}

// CreateDesktopSubscriptionCheckout opens a Stripe subscription Checkout Session
// for the caller (mirrors upstream SubscriptionRequestStripePay). SECURITY-CRITICAL
// (O9): the user id is derived STRICTLY from c.GetInt("id") set by TokenAuth — the
// only body field is plan_id. Accepting a user_id from the body would let any
// authenticated caller create a checkout billed against another user. On success it
// returns raw {"url": ...}; validation/processing failures return a non-2xx with a
// {"message": ...} body, matching the desktop handlers in subscription_portal.go.
func CreateDesktopSubscriptionCheckout(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"message": "no user in context"})
		return
	}

	var req desktopSubscriptionCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.PlanId <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"message": "invalid plan_id"})
		return
	}

	plan, err := model.GetSubscriptionPlanById(req.PlanId)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"message": "plan not found"})
			return
		}
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅结账套餐查询失败 plan_id=%d error=%q", req.PlanId, err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"message": "failed to load plan"})
		return
	}
	if !plan.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"message": "plan is not enabled"})
		return
	}
	if plan.StripePriceId == "" {
		c.JSON(http.StatusBadRequest, gin.H{"message": "plan has no Stripe price"})
		return
	}
	if !strings.HasPrefix(setting.StripeApiSecret, "sk_") && !strings.HasPrefix(setting.StripeApiSecret, "rk_") {
		c.JSON(http.StatusServiceUnavailable, gin.H{"message": "Stripe is not configured"})
		return
	}
	if setting.StripeWebhookSecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"message": "Stripe webhook is not configured"})
		return
	}

	user, err := model.GetUserById(id, false)
	if err != nil || user == nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅结账用户查询失败 user_id=%d error=%v", id, err))
		c.JSON(http.StatusInternalServerError, gin.H{"message": "failed to load user"})
		return
	}

	if plan.MaxPurchasePerUser > 0 {
		count, err := model.CountUserSubscriptionsByPlan(id, plan.Id)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅结账购买次数查询失败 user_id=%d plan_id=%d error=%q", id, plan.Id, err.Error()))
			c.JSON(http.StatusInternalServerError, gin.H{"message": "failed to check purchase limit"})
			return
		}
		if count >= int64(plan.MaxPurchasePerUser) {
			c.JSON(http.StatusConflict, gin.H{"message": "purchase limit reached for this plan"})
			return
		}
	}

	// Reference id is generated exactly like upstream so the webhook's
	// client_reference_id → SubscriptionOrder lookup resolves user/plan (design B3).
	reference := fmt.Sprintf("sub-stripe-ref-%d-%d-%s", user.Id, time.Now().UnixMilli(), randstr.String(4))
	referenceId := "sub_ref_" + common.Sha1([]byte(reference))

	order := &model.SubscriptionOrder{
		UserId:          id,
		PlanId:          plan.Id,
		Money:           plan.PriceAmount,
		TradeNo:         referenceId,
		PaymentMethod:   model.PaymentMethodStripe,
		PaymentProvider: model.PaymentProviderStripe,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := order.Insert(); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅结账创建订单失败 user_id=%d plan_id=%d error=%q", id, plan.Id, err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"message": "failed to create order"})
		return
	}

	url, err := checkoutCreator.Create(referenceId, user.StripeCustomer, user.Email, plan.StripePriceId, PortalReturnURL())
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Gressio 订阅结账支付链接创建失败 trade_no=%s plan_id=%d error=%q", referenceId, plan.Id, err.Error()))
		c.JSON(http.StatusBadGateway, gin.H{"message": "failed to create checkout session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}
