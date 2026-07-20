package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	twmodel "github.com/QuantumNous/new-api/transwork/model"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/subscription"
	"github.com/stripe/stripe-go/v81/webhook"
	"gorm.io/gorm"
)

// Overlay-owned DB options (design B5). These are distinct from the upstream
// setting.StripeWebhookSecret so the second (recurring) Stripe endpoint has its
// own signing secret and its own return URL.
const (
	OptionKeyStripeWebhookSecret = "transwork_subscription.stripe_webhook_secret"
	OptionKeyPortalReturnURL     = "transwork_subscription.portal_return_url"

	// DefaultPortalReturnURL is the branded gressio.ai page Stripe bounces the
	// browser back to after the Customer Portal (design B5 / O8, Option A).
	DefaultPortalReturnURL = "https://gressio.ai/subscription-return"
)

// errUnresolvedRenewal and errMissingSubscriptionId are RETRYABLE sentinels. The
// top-level handler maps ANY non-nil handler error to HTTP 5xx so Stripe
// redelivers, which is deliberately distinct from the EXPECTED-terminal outcomes
// (ignored event / duplicate / mode!=subscription / malformed-but-parseable) that
// return nil → 200 (design B2, CR-1/SEC-2). Using typed sentinels — not error
// string matching — keeps that expected/unexpected split explicit.
var (
	errUnresolvedRenewal     = errors.New("gressio subscription: renewal target unresolved (retryable)")
	errMissingSubscriptionId = errors.New("gressio subscription: invoice missing subscription id (retryable)")
)

// missingSecretWarnOnce throttles the "signing secret not configured" warning to a
// single line for the process lifetime (CR-8): once SEC-1 rejects with 503, Stripe
// retries would otherwise flood the log with an identical warning per delivery.
var missingSecretWarnOnce sync.Once

// stripeSubscriptionCanceler abstracts the single external Stripe call the
// webhook makes (subscription.Cancel) so tests can inject a mock and the O7
// "cancel failure is non-fatal" path is exercised without a live Stripe.
type stripeSubscriptionCanceler interface {
	Cancel(subscriptionId string) error
}

type liveStripeCanceler struct{}

func (liveStripeCanceler) Cancel(subscriptionId string) error {
	stripe.Key = setting.StripeApiSecret
	_, err := subscription.Cancel(subscriptionId, nil)
	return err
}

// subscriptionCanceler is a package var so tests can swap in a mock.
var subscriptionCanceler stripeSubscriptionCanceler = liveStripeCanceler{}

func readOption(key string) string {
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()
	if common.OptionMap == nil {
		return ""
	}
	return common.OptionMap[key]
}

// SubscriptionWebhookSecret returns the overlay endpoint's signing secret.
func SubscriptionWebhookSecret() string {
	return readOption(OptionKeyStripeWebhookSecret)
}

// PortalReturnURL returns the configured Customer Portal return URL, falling back
// to the branded default when the admin has not overridden it (design B5).
func PortalReturnURL() string {
	if v := readOption(OptionKeyPortalReturnURL); v != "" {
		return v
	}
	return DefaultPortalReturnURL
}

// StripeSubscriptionWebhook is the overlay's parallel Stripe endpoint (design
// B2). It verifies the signature, then routes the five recurring-lifecycle
// events. Return codes: 503 when the signing secret is unset (fail closed, SEC-1)
// or the body cannot be read; 400 on signature-verify failure; 500 on an
// unexpected/retryable processing error so Stripe redelivers (CR-1/SEC-2); and 200
// for every handled/ignored/duplicate event so Stripe stops retrying (B4
// idempotency makes the 200-on-success contract safe).
func StripeSubscriptionWebhook(c *gin.Context) {
	ctx := c.Request.Context()

	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("Gressio 订阅 webhook 读取请求体失败 client_ip=%s error=%q", c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	secret := SubscriptionWebhookSecret()
	if secret == "" {
		// SEC-1: FAIL CLOSED. webhook.ConstructEventWithOptions does NOT reject an
		// empty secret — it computes an HMAC-SHA256 with an empty key, which is
		// publicly forgeable, so verifying against it would accept attacker-forged
		// events on this unauthenticated route. Mirror the topup webhook's
		// fail-closed gate (controller/topup_stripe.go isStripeWebhookEnabled):
		// reject before verifying. 503 (not 403) so a misconfigured deploy makes
		// Stripe retry rather than silently ACK-drop, and the admin can still seed
		// the secret via the option (init.go).
		missingSecretWarnOnce.Do(func() {
			logger.LogWarn(ctx, fmt.Sprintf("Gressio 订阅 webhook 未配置签名密钥（%s），已拒绝所有事件（503）——请在后台填入 endpoint B 的签名密钥 client_ip=%s", OptionKeyStripeWebhookSecret, c.ClientIP()))
		})
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	signature := c.GetHeader("Stripe-Signature")
	event, err := webhook.ConstructEventWithOptions(payload, signature, secret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("Gressio 订阅 webhook 验签失败 client_ip=%s error=%q", c.ClientIP(), err.Error()))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if err := handleSubscriptionEvent(ctx, event); err != nil {
		// CR-1/SEC-2: an unexpected/retryable error (DB failure, lock contention,
		// unresolved renewal target, API-version field relocation) must NOT be
		// ACKed as 200 — that silently drops a paid renewal. Return 5xx so Stripe's
		// built-in retry (~3 days) recovers. Expected-terminal outcomes already
		// returned nil above and fall through to 200.
		logger.LogError(ctx, fmt.Sprintf("Gressio 订阅 webhook 处理事件失败（返回 5xx 触发 Stripe 重试）event_id=%s event_type=%s error=%q", event.ID, string(event.Type), err.Error()))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusOK)
}

// handleSubscriptionEvent routes the five recurring-lifecycle events (design B3).
// It is exported to the package for direct unit testing with fabricated events,
// bypassing the HTTP/signature layer.
func handleSubscriptionEvent(ctx context.Context, event stripe.Event) error {
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		return handleCheckoutCompleted(ctx, event)
	case stripe.EventTypeInvoicePaymentSucceeded:
		return handleInvoicePaymentSucceeded(ctx, event)
	case stripe.EventTypeInvoicePaymentFailed:
		return handleInvoicePaymentFailed(ctx, event)
	case stripe.EventTypeCustomerSubscriptionDeleted:
		return handleSubscriptionDeleted(ctx, event)
	case stripe.EventTypeCustomerSubscriptionUpdated:
		return handleSubscriptionUpdated(ctx, event)
	default:
		logger.LogInfo(ctx, fmt.Sprintf("Gressio 订阅 webhook 忽略事件 event_type=%s", string(event.Type)))
		return nil
	}
}

// lockLinkBySubId re-reads a link FOR UPDATE (upstream idempotency pattern,
// model/subscription.go:526). Returns gorm.ErrRecordNotFound when absent.
func lockLinkBySubId(tx *gorm.DB, subId string) (*twmodel.StripeSubscriptionLink, error) {
	var link twmodel.StripeSubscriptionLink
	if err := tx.Set("gorm:query_option", "FOR UPDATE").
		Where("stripe_subscription_id = ?", subId).First(&link).Error; err != nil {
		return nil, err
	}
	return &link, nil
}

// handleCheckoutCompleted records the sub_… → user/plan mapping and cancels any
// superseded live subscription for the same user (design B3, the Translide
// re-subscribe guard).
func handleCheckoutCompleted(ctx context.Context, event stripe.Event) error {
	if event.GetObjectValue("mode") != "subscription" {
		// One-time payments are fulfilled by the upstream endpoint A; ignore.
		logger.LogInfo(ctx, "Gressio 订阅 webhook 忽略非订阅 checkout.session.completed")
		return nil
	}
	subId := event.GetObjectValue("subscription")
	if subId == "" {
		logger.LogWarn(ctx, "Gressio 订阅 webhook checkout.session.completed 缺少 subscription id")
		return nil
	}
	customerId := event.GetObjectValue("customer")
	referenceId := event.GetObjectValue("client_reference_id")

	userId, planId := 0, 0
	if referenceId != "" {
		if order := model.GetSubscriptionOrderByTradeNo(referenceId); order != nil {
			userId = order.UserId
			planId = order.PlanId
		} else {
			logger.LogWarn(ctx, fmt.Sprintf("Gressio 订阅 webhook 未找到订单 trade_no=%s sub=%s", referenceId, subId))
		}
	}

	var supersededSubIds []string
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		link, err := lockLinkBySubId(tx, subId)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if link == nil {
			link = &twmodel.StripeSubscriptionLink{StripeSubscriptionId: subId}
		}
		// SEC-3/CR-7: a replayed checkout must be a no-op like the other four arms.
		// Without this, Stripe redelivering an OLD checkout.session.completed
		// re-runs the supersede query below and cancels the user's CURRENT (now
		// different) live subscription. Short-circuit on the already-processed
		// event id.
		if event.ID != "" && link.LastEventId == event.ID {
			return nil
		}
		if customerId != "" {
			link.StripeCustomerId = customerId
		}
		if userId != 0 {
			link.UserId = userId
		}
		if planId != 0 {
			link.PlanId = planId
		}
		if link.Status == "" {
			link.Status = twmodel.LinkStatusActive
		}
		if event.ID != "" {
			link.LastEventId = event.ID
		}
		if link.Id == 0 {
			if err := tx.Create(link).Error; err != nil {
				return err
			}
		} else if err := tx.Save(link).Error; err != nil {
			return err
		}

		// Cancel any other still-live subscription for this user: a re-subscribe
		// while an old sub is active would otherwise double-bill (Translide bug).
		// This supersede is intentionally CROSS-PLAN — it cancels every other active
		// link for the user regardless of plan, because an upgrade/downgrade is also
		// a re-subscribe and must not leave two Stripe subs billing at once (B3).
		if link.UserId != 0 {
			var olds []twmodel.StripeSubscriptionLink
			if err := tx.Where("user_id = ? AND stripe_subscription_id <> ? AND status IN ?",
				link.UserId, subId, []string{twmodel.LinkStatusActive, twmodel.LinkStatusPastDue}).
				Find(&olds).Error; err != nil {
				return err
			}
			for _, old := range olds {
				supersededSubIds = append(supersededSubIds, old.StripeSubscriptionId)
				if err := tx.Model(&twmodel.StripeSubscriptionLink{}).
					Where("id = ?", old.Id).
					Updates(map[string]any{
						"status":     twmodel.LinkStatusCanceled,
						"auto_renew": false,
						"updated_at": common.GetTimestamp(),
					}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// O7: the actual Stripe cancel is non-fatal. The link is already marked
	// canceled in the DB; a transient Stripe/API-key failure must not wedge event
	// processing, so we log loudly for retry and still return success (200).
	for _, oldSubId := range supersededSubIds {
		if cerr := subscriptionCanceler.Cancel(oldSubId); cerr != nil {
			logger.LogError(ctx, fmt.Sprintf("Gressio 订阅 webhook 取消旧订阅失败（非致命，需人工重试）old_sub=%s new_sub=%s error=%q", oldSubId, subId, cerr.Error()))
		} else {
			logger.LogInfo(ctx, fmt.Sprintf("Gressio 订阅 webhook 已取消被替代的旧订阅 old_sub=%s new_sub=%s", oldSubId, subId))
		}
	}
	return nil
}

// handleInvoicePaymentSucceeded extends access and refills quota on a real
// renewal (billing_reason == subscription_cycle). subscription_create is ignored
// because endpoint A already fulfilled the first period (design B3). Idempotency
// (B4) makes the extension exactly-once per invoice.
func handleInvoicePaymentSucceeded(ctx context.Context, event stripe.Event) error {
	if reason := event.GetObjectValue("billing_reason"); reason != "subscription_cycle" {
		logger.LogInfo(ctx, fmt.Sprintf("Gressio 订阅 webhook 忽略非续订发票 billing_reason=%s", reason))
		return nil
	}
	invoiceId := event.GetObjectValue("id")
	subId := invoiceSubscriptionId(event.Data.Object)
	if subId == "" {
		// CR-10: recent Stripe API versions relocated the invoice→subscription
		// pointer (top-level `subscription` → parent.subscription_details); we probe
		// both above. IgnoreAPIVersionMismatch suppresses the version check but does
		// NOT reshape the payload, so a still-empty id on a subscription_cycle
		// invoice is almost certainly a field relocation, not a subscription-less
		// invoice. Log LOUDLY and treat as retryable (5xx) rather than silently
		// no-op'ing away a paid renewal. Must be confirmed against the account's
		// real API version in the clock-advance E2E (spec testing strategy).
		logger.LogError(ctx, fmt.Sprintf("Gressio 订阅 webhook 续订发票无法解析 subscription id（疑似 Stripe API 版本字段迁移，需 E2E 核对）invoice=%s", invoiceId))
		return errMissingSubscriptionId
	}

	// Resolve "now" before opening the transaction: GetDBTimestamp issues its own
	// DB query, which would deadlock against a single-connection pool while the
	// transaction holds that connection.
	now := model.GetDBTimestamp()

	return model.DB.Transaction(func(tx *gorm.DB) error {
		link, err := lockLinkBySubId(tx, subId)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				logger.LogWarn(ctx, fmt.Sprintf("Gressio 订阅 webhook 续订发票无对应订阅映射 sub=%s invoice=%s", subId, invoiceId))
				return nil
			}
			return err
		}
		// B4 idempotency: identical event redelivery, or an already-applied invoice.
		if event.ID != "" && link.LastEventId == event.ID {
			return nil
		}
		if invoiceId != "" && link.LastInvoiceId == invoiceId {
			return nil
		}

		userSubId := link.UserSubscriptionId
		if userSubId == 0 {
			userSubId, err = resolveUserSubscriptionId(tx, link)
			if err != nil {
				return err
			}
		}
		if userSubId == 0 {
			// CR-2: nothing was extended — endpoint A has not created the
			// UserSubscription yet (capture race, design O2). Persisting
			// LastInvoiceId/LastEventId here would make the eventual redelivery a
			// no-op (the invoice guard would reject it) and strand the paid renewal
			// permanently. Persist NO marker and return a retryable error so the
			// webhook 5xxs and Stripe redelivers once the sub exists.
			logger.LogWarn(ctx, fmt.Sprintf("Gressio 订阅 webhook 续订时未能解析本地订阅（可重试，未落幂等标记）sub=%s user_id=%d plan_id=%d invoice=%s", subId, link.UserId, link.PlanId, invoiceId))
			return errUnresolvedRenewal
		}

		var sub model.UserSubscription
		if err := tx.Set("gorm:query_option", "FOR UPDATE").
			Where("id = ?", userSubId).First(&sub).Error; err != nil {
			return err
		}
		var plan model.SubscriptionPlan
		if err := tx.Where("id = ?", sub.PlanId).First(&plan).Error; err != nil {
			return err
		}
		newEnd, err := calcExtendedEndTime(sub.EndTime, now, &plan)
		if err != nil {
			return err
		}
		sub.EndTime = newEnd
		sub.AmountUsed = 0 // billing-driven refill (design decision 1)
		if sub.Status == "expired" {
			sub.Status = "active"
		}
		if err := tx.Save(&sub).Error; err != nil {
			return err
		}

		link.UserSubscriptionId = userSubId
		link.Status = twmodel.LinkStatusActive
		if invoiceId != "" {
			link.LastInvoiceId = invoiceId
		}
		if event.ID != "" {
			link.LastEventId = event.ID
		}
		return tx.Save(link).Error
	})
}

// handleSubscriptionDeleted lapses the link but, by default (O1), leaves EndTime
// untouched so the user keeps the period already paid for; the upstream expiry
// sweep flips them to expired when end_time <= now.
func handleSubscriptionDeleted(ctx context.Context, event stripe.Event) error {
	subId := event.GetObjectValue("id")
	if subId == "" {
		logger.LogWarn(ctx, "Gressio 订阅 webhook customer.subscription.deleted 缺少 subscription id")
		return nil
	}
	return model.DB.Transaction(func(tx *gorm.DB) error {
		link, err := lockLinkBySubId(tx, subId)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if event.ID != "" && link.LastEventId == event.ID {
			return nil
		}
		link.Status = twmodel.LinkStatusCanceled
		link.AutoRenew = false
		if event.ID != "" {
			link.LastEventId = event.ID
		}
		return tx.Save(link).Error
	})
}

// handleSubscriptionUpdated syncs display-only state: auto-renew and current
// period end, mirroring Stripe status. No EndTime/quota change (design B3).
func handleSubscriptionUpdated(ctx context.Context, event stripe.Event) error {
	subId := event.GetObjectValue("id")
	if subId == "" {
		logger.LogWarn(ctx, "Gressio 订阅 webhook customer.subscription.updated 缺少 subscription id")
		return nil
	}
	autoRenew := event.GetObjectValue("cancel_at_period_end") != "true"
	currentPeriodEnd := parseEpoch(subscriptionCurrentPeriodEnd(event.Data.Object))
	stripeStatus := event.GetObjectValue("status")

	return model.DB.Transaction(func(tx *gorm.DB) error {
		link, err := lockLinkBySubId(tx, subId)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if event.ID != "" && link.LastEventId == event.ID {
			return nil
		}
		link.AutoRenew = autoRenew
		if currentPeriodEnd > 0 {
			link.CurrentPeriodEnd = currentPeriodEnd
		}
		if mapped := mapStripeStatus(stripeStatus); mapped != "" {
			link.Status = mapped
		}
		if event.ID != "" {
			link.LastEventId = event.ID
		}
		return tx.Save(link).Error
	})
}

// handleInvoicePaymentFailed marks the link past_due for admin visibility; access
// is not revoked here — Stripe dunning eventually fires deleted (design B3).
func handleInvoicePaymentFailed(ctx context.Context, event stripe.Event) error {
	subId := event.GetObjectValue("subscription")
	if subId == "" {
		logger.LogWarn(ctx, "Gressio 订阅 webhook invoice.payment_failed 缺少 subscription id")
		return nil
	}
	return model.DB.Transaction(func(tx *gorm.DB) error {
		link, err := lockLinkBySubId(tx, subId)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if event.ID != "" && link.LastEventId == event.ID {
			return nil
		}
		link.Status = twmodel.LinkStatusPastDue
		if event.ID != "" {
			link.LastEventId = event.ID
		}
		return tx.Save(link).Error
	})
}

// resolveUserSubscriptionId lazily maps the link to the correct local
// UserSubscription for its plan. O3 tiebreak: when a user holds more than one sub
// of the same plan, prefer the one whose StartTime is nearest the link's
// CreatedAt (the sub minted by this checkout).
func resolveUserSubscriptionId(tx *gorm.DB, link *twmodel.StripeSubscriptionLink) (int, error) {
	if link.UserId == 0 || link.PlanId == 0 {
		return 0, nil
	}
	var subs []model.UserSubscription
	if err := tx.Where("user_id = ? AND plan_id = ?", link.UserId, link.PlanId).
		Order("end_time desc, id desc").Find(&subs).Error; err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, nil
	}
	best := subs[0]
	bestDelta := absInt64(best.StartTime - link.CreatedAt)
	for _, candidate := range subs[1:] {
		if d := absInt64(candidate.StartTime - link.CreatedAt); d < bestDelta {
			best = candidate
			bestDelta = d
		}
	}
	return best.Id, nil
}

// invoiceSubscriptionId extracts the sub_… an invoice belongs to, tolerating the
// Stripe API-version field relocation (CR-10): older versions expose it top-level
// as `subscription`, recent versions nest it under
// parent.subscription_details.subscription. Returns "" only when neither shape has
// it (an anomaly the caller treats as retryable rather than a silent no-op).
func invoiceSubscriptionId(obj map[string]interface{}) string {
	if v := objectString(obj, "subscription"); v != "" {
		return v
	}
	return objectString(obj, "parent", "subscription_details", "subscription")
}

// subscriptionCurrentPeriodEnd extracts current_period_end, tolerating the same
// API-version relocation (CR-10): recent versions moved it onto the first
// subscription item (items.data[0].current_period_end). Display-only, so a missing
// value is non-fatal (the caller simply keeps the last-known value).
func subscriptionCurrentPeriodEnd(obj map[string]interface{}) string {
	if v := objectString(obj, "current_period_end"); v != "" {
		return v
	}
	return objectString(obj, "items", "data", "0", "current_period_end")
}

// objectString safely walks a nested key path over a decoded Stripe event object
// and returns the leaf rendered as a string, or "" if any segment is
// missing/mismatched. Unlike stripe.Event.GetObjectValue it never panics when an
// intermediate node is nil or the wrong type, so it is safe to probe alternative
// payload shapes across Stripe API versions. Numeric path segments index slices
// (e.g. "data", "0").
func objectString(obj map[string]interface{}, keys ...string) string {
	var node interface{} = obj
	for _, key := range keys {
		switch n := node.(type) {
		case map[string]interface{}:
			node = n[key]
		case []interface{}:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(n) {
				return ""
			}
			node = n[idx]
		default:
			return ""
		}
	}
	if node == nil {
		return ""
	}
	return fmt.Sprintf("%v", node)
}

// calcExtendedEndTime mirrors upstream model.calcPlanEndTime (unexported, so it
// cannot be called from the overlay). It extends from the later of the current
// period end or now (design B3) so a delayed webhook never shortens the period.
func calcExtendedEndTime(existingEndTime int64, now int64, plan *model.SubscriptionPlan) (int64, error) {
	if plan == nil {
		return 0, errors.New("plan is nil")
	}
	// CR-5: parity with upstream calcPlanEndTime (model/subscription.go) — a
	// non-positive duration on a non-custom plan is a misconfiguration, not a
	// zero-length extension.
	if plan.DurationValue <= 0 && plan.DurationUnit != model.SubscriptionDurationCustom {
		return 0, errors.New("duration_value must be > 0")
	}
	base := existingEndTime
	if now > base {
		base = now
	}
	start := time.Unix(base, 0)
	switch plan.DurationUnit {
	case model.SubscriptionDurationYear:
		return start.AddDate(plan.DurationValue, 0, 0).Unix(), nil
	case model.SubscriptionDurationMonth:
		return start.AddDate(0, plan.DurationValue, 0).Unix(), nil
	case model.SubscriptionDurationDay:
		return start.Add(time.Duration(plan.DurationValue) * 24 * time.Hour).Unix(), nil
	case model.SubscriptionDurationHour:
		return start.Add(time.Duration(plan.DurationValue) * time.Hour).Unix(), nil
	case model.SubscriptionDurationCustom:
		if plan.CustomSeconds <= 0 {
			return 0, errors.New("custom_seconds must be > 0")
		}
		return start.Add(time.Duration(plan.CustomSeconds) * time.Second).Unix(), nil
	default:
		return 0, fmt.Errorf("invalid duration_unit: %s", plan.DurationUnit)
	}
}

// mapStripeStatus reduces Stripe subscription statuses to the overlay-local
// vocabulary. Unknown statuses return "" so the existing link status is kept.
func mapStripeStatus(stripeStatus string) string {
	switch stripeStatus {
	case "active", "trialing":
		return twmodel.LinkStatusActive
	case "past_due", "unpaid":
		return twmodel.LinkStatusPastDue
	case "canceled":
		return twmodel.LinkStatusCanceled
	default:
		return ""
	}
}

// parseEpoch parses a Stripe numeric field. Stripe's event Object bag decodes JSON
// numbers as float64, so GetObjectValue renders them like "1.7e+09"; ParseFloat
// (not ParseInt) is required, matching upstream's amount_total handling.
func parseEpoch(v string) int64 {
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
