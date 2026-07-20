package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	twmodel "github.com/QuantumNous/new-api/transwork/model"
)

// --- helpers -----------------------------------------------------------------

type mockCheckoutCreator struct {
	called                                             bool
	referenceId, customerId, email, priceId, returnURL string
	url                                                string
	err                                                error
}

func (m *mockCheckoutCreator) Create(referenceId, customerId, email, priceId, returnURL string) (string, error) {
	m.called = true
	m.referenceId = referenceId
	m.customerId = customerId
	m.email = email
	m.priceId = priceId
	m.returnURL = returnURL
	if m.url == "" {
		m.url = "https://checkout.stripe.test/c/session_abc"
	}
	return m.url, m.err
}

func swapCheckoutCreator(t *testing.T, m stripeCheckoutCreator) {
	t.Helper()
	orig := checkoutCreator
	checkoutCreator = m
	t.Cleanup(func() { checkoutCreator = orig })
}

// setStripeConfig sets the upstream setting vars the checkout guard reads and
// restores them after the test, so a "Stripe configured" test can't leak into a
// "Stripe unconfigured" one.
func setStripeConfig(t *testing.T, apiSecret, webhookSecret string) {
	t.Helper()
	origA, origW := setting.StripeApiSecret, setting.StripeWebhookSecret
	setting.StripeApiSecret = apiSecret
	setting.StripeWebhookSecret = webhookSecret
	t.Cleanup(func() {
		setting.StripeApiSecret = origA
		setting.StripeWebhookSecret = origW
	})
}

// seedUser migrates the users table onto the per-test in-memory DB and inserts a
// user. GetUserById reads StripeCustomer/Email off this row.
func seedUser(t *testing.T, id int, customer, email string) {
	t.Helper()
	if err := model.DB.AutoMigrate(&model.User{}); err != nil {
		t.Fatalf("migrate user: %v", err)
	}
	u := &model.User{Id: id, Username: fmt.Sprintf("u%d", id), StripeCustomer: customer, Email: email}
	if err := model.DB.Create(u).Error; err != nil {
		t.Fatalf("seed user %d: %v", id, err)
	}
}

// createPlan inserts a plan and evicts it from the process-global plan cache, so a
// stale entry keyed by the same (fresh-DB-recycled) id from a prior test cannot be
// returned by GetSubscriptionPlanById.
func createPlan(t *testing.T, plan *model.SubscriptionPlan) *model.SubscriptionPlan {
	t.Helper()
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	model.InvalidateSubscriptionPlanCache(plan.Id)
	return plan
}

// disablePlan forces enabled=false on an existing plan. A plain Create with
// Enabled:false is silently overridden to true by GORM's `default:true` tag (a
// false bool is a zero value), so a disabled plan must be written explicitly.
func disablePlan(t *testing.T, plan *model.SubscriptionPlan) {
	t.Helper()
	if err := model.DB.Model(&model.SubscriptionPlan{}).Where("id = ?", plan.Id).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("disable plan %d: %v", plan.Id, err)
	}
	model.InvalidateSubscriptionPlanCache(plan.Id)
}

// --- withStatusParam ---------------------------------------------------------

// The gressio.ai return page distinguishes a completed checkout from an abandoned
// one by the status query param, and must stay a valid URL whether or not the
// configured return URL already carries a query string.
func TestWithStatusParamAppendsCorrectSeparator(t *testing.T) {
	if got := withStatusParam("https://gressio.ai/return", "success"); got != "https://gressio.ai/return?status=success" {
		t.Fatalf("bare URL: got %q", got)
	}
	if got := withStatusParam("https://gressio.ai/return?ref=1", "cancel"); got != "https://gressio.ai/return?ref=1&status=cancel" {
		t.Fatalf("URL with query: got %q", got)
	}
}

// --- ListDesktopSubscriptionPlans --------------------------------------------

// The desktop paywall must see only plans it can actually check out: enabled AND
// carrying a StripePriceId. A disabled plan or one without a price id would render
// a dead button. The raw StripePriceId must never appear in the payload.
func TestListDesktopPlansFiltersToEnabledStripePayable(t *testing.T) {
	setupDB(t)
	payable := createPlan(t, &model.SubscriptionPlan{
		Title: "Pro Monthly", Subtitle: "everything", PriceAmount: 7.0, Currency: "USD",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
		Enabled: true, StripePriceId: "price_live_1",
	})
	createPlan(t, &model.SubscriptionPlan{
		Title: "No Price", Enabled: true, StripePriceId: "",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
	})
	disabled := createPlan(t, &model.SubscriptionPlan{
		Title: "Disabled", StripePriceId: "price_live_2",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
	})
	disablePlan(t, disabled)

	c, w := jsonContext(http.MethodGet, "/api/desktop/subscription/plans", nil)
	c.Set("id", 3)
	ListDesktopSubscriptionPlans(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	plans, ok := resp["plans"].([]any)
	if !ok {
		t.Fatalf("expected plans array, got %T", resp["plans"])
	}
	if len(plans) != 1 {
		t.Fatalf("expected only the enabled Stripe-payable plan, got %d", len(plans))
	}
	row := plans[0].(map[string]any)
	if row["title"] != "Pro Monthly" || row["price_amount"].(float64) != 7.0 || row["currency"] != "USD" {
		t.Fatalf("unexpected plan projection: %v", row)
	}
	if int(row["id"].(float64)) != payable.Id || row["duration_unit"] != "month" || int(row["duration_value"].(float64)) != 1 {
		t.Fatalf("unexpected plan projection: %v", row)
	}
	if _, leaked := row["stripe_price_id"]; leaked {
		t.Fatalf("StripePriceId must not be exposed to the desktop client: %v", row)
	}
}

// --- CreateDesktopSubscriptionCheckout ---------------------------------------

// SECURITY (O9) + success path: the order and the Stripe customer must come SOLELY
// from the authenticated user (c.GetInt("id")). This authenticates user 7 (whose
// Stripe customer is cus_authed) while smuggling a victim's user_id=99 in the body,
// and asserts (a) the created order is billed to user 7, (b) Stripe is called with
// cus_authed — never a body-derived id, (c) the reference id ties order↔checkout,
// and (d) the response is raw {"url":...}.
func TestCreateCheckoutDerivesUserFromAuthIgnoringBody_O9(t *testing.T) {
	setupDB(t)
	seedUser(t, 7, "cus_authed", "seven@example.com")
	setStripeConfig(t, "sk_test_123", "whsec_test")
	plan := createPlan(t, &model.SubscriptionPlan{
		Title: "Pro", PriceAmount: 7.0, Currency: "USD",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
		Enabled: true, StripePriceId: "price_authed",
	})
	mock := &mockCheckoutCreator{}
	swapCheckoutCreator(t, mock)

	// Adversarial: a victim user_id is smuggled in the body alongside the real plan.
	body := []byte(fmt.Sprintf(`{"plan_id":%d,"user_id":99}`, plan.Id))
	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/pay", body)
	c.Set("id", 7) // the authenticated user, as TokenAuth would set it

	CreateDesktopSubscriptionCheckout(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if resp["url"] != mock.url {
		t.Fatalf("expected checkout url %q, got %v", mock.url, resp["url"])
	}
	if !mock.called {
		t.Fatalf("Stripe checkout must be created")
	}
	if mock.customerId != "cus_authed" {
		t.Fatalf("O9 BREACH: checkout opened for customer %q; must be the authed user's cus_authed, never a body-derived id", mock.customerId)
	}
	if mock.priceId != "price_authed" {
		t.Fatalf("expected plan's StripePriceId passed, got %q", mock.priceId)
	}
	if mock.returnURL != PortalReturnURL() {
		t.Fatalf("expected return URL %q, got %q", PortalReturnURL(), mock.returnURL)
	}

	var order model.SubscriptionOrder
	if err := model.DB.Where("trade_no = ?", mock.referenceId).First(&order).Error; err != nil {
		t.Fatalf("expected an order keyed by the checkout reference id: %v", err)
	}
	if order.UserId != 7 {
		t.Fatalf("O9 BREACH: order billed to user %d; must be the authed user 7, never the body-supplied 99", order.UserId)
	}
	if order.PlanId != plan.Id || order.Money != 7.0 {
		t.Fatalf("unexpected order plan/money: plan=%d money=%v", order.PlanId, order.Money)
	}
	if order.PaymentMethod != model.PaymentMethodStripe || order.PaymentProvider != model.PaymentProviderStripe {
		t.Fatalf("unexpected order payment method/provider: %q/%q", order.PaymentMethod, order.PaymentProvider)
	}
	if order.Status != common.TopUpStatusPending {
		t.Fatalf("expected pending order, got %q", order.Status)
	}
}

// A disabled plan must be rejected (400) before any Stripe call or order write.
func TestCreateCheckoutRejectsDisabledPlan(t *testing.T) {
	setupDB(t)
	plan := createPlan(t, &model.SubscriptionPlan{
		Title: "Off", StripePriceId: "price_x",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
	})
	disablePlan(t, plan)
	mock := &mockCheckoutCreator{}
	swapCheckoutCreator(t, mock)

	body := []byte(fmt.Sprintf(`{"plan_id":%d}`, plan.Id))
	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/pay", body)
	c.Set("id", 7)
	CreateDesktopSubscriptionCheckout(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for disabled plan, got %d", w.Code)
	}
	if mock.called {
		t.Fatalf("Stripe must not be called for a disabled plan")
	}
}

// A plan without a StripePriceId cannot be checked out and must be rejected (400).
func TestCreateCheckoutRejectsMissingStripePrice(t *testing.T) {
	setupDB(t)
	plan := createPlan(t, &model.SubscriptionPlan{
		Title: "NoPrice", Enabled: true, StripePriceId: "",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
	})
	mock := &mockCheckoutCreator{}
	swapCheckoutCreator(t, mock)

	body := []byte(fmt.Sprintf(`{"plan_id":%d}`, plan.Id))
	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/pay", body)
	c.Set("id", 7)
	CreateDesktopSubscriptionCheckout(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing StripePriceId, got %d", w.Code)
	}
	if mock.called {
		t.Fatalf("Stripe must not be called when the plan has no price id")
	}
}

// When Stripe is not configured (no sk_/rk_ key), checkout must fail closed (503)
// rather than attempt a call that would error opaquely — and write no order.
func TestCreateCheckoutRejectsUnconfiguredStripe(t *testing.T) {
	setupDB(t)
	setStripeConfig(t, "", "") // explicitly unconfigured
	plan := createPlan(t, &model.SubscriptionPlan{
		Title: "Pro", Enabled: true, StripePriceId: "price_ok",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
	})
	mock := &mockCheckoutCreator{}
	swapCheckoutCreator(t, mock)

	body := []byte(fmt.Sprintf(`{"plan_id":%d}`, plan.Id))
	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/pay", body)
	c.Set("id", 7)
	CreateDesktopSubscriptionCheckout(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Stripe is unconfigured, got %d", w.Code)
	}
	if mock.called {
		t.Fatalf("Stripe must not be called when unconfigured")
	}
	var n int64
	model.DB.Model(&model.SubscriptionOrder{}).Count(&n)
	if n != 0 {
		t.Fatalf("no order may be written when Stripe is unconfigured, found %d", n)
	}
}

// The MaxPurchasePerUser cap is enforced exactly like upstream: a user at the cap
// gets 409 and no new order/checkout.
func TestCreateCheckoutEnforcesMaxPurchasePerUser(t *testing.T) {
	setupDB(t)
	seedUser(t, 7, "cus_authed", "seven@example.com")
	setStripeConfig(t, "sk_test_123", "whsec_test")
	plan := createPlan(t, &model.SubscriptionPlan{
		Title: "Once", Enabled: true, StripePriceId: "price_once",
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
		MaxPurchasePerUser: 1,
	})
	// User already owns one subscription of this plan → at the cap.
	if err := model.DB.Create(&model.UserSubscription{UserId: 7, PlanId: plan.Id, Status: "active"}).Error; err != nil {
		t.Fatalf("seed existing sub: %v", err)
	}
	mock := &mockCheckoutCreator{}
	swapCheckoutCreator(t, mock)

	body := []byte(fmt.Sprintf(`{"plan_id":%d}`, plan.Id))
	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/pay", body)
	c.Set("id", 7)
	CreateDesktopSubscriptionCheckout(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 at purchase cap, got %d", w.Code)
	}
	if mock.called {
		t.Fatalf("Stripe must not be called once the purchase cap is hit")
	}
}

// --- Task 3: admin links backfill user_subscription_id -----------------------

// A link captured at checkout has user_subscription_id=0 until the first renewal
// resolves it, so the admin per-user modal cannot merge it against the upstream
// subscription rows right after a purchase. Listing must resolve it lazily and
// PERSIST the id, so the merge works immediately and stays fixed on the next load.
func TestAdminListLinksResolvesAndPersistsUserSubscriptionId(t *testing.T) {
	setupDB(t)
	now := time.Now().Unix()
	sub := &model.UserSubscription{UserId: 5, PlanId: 1, StartTime: now, EndTime: now + 10000, Status: "active"}
	if err := model.DB.Create(sub).Error; err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_fresh", UserId: 5, PlanId: 1,
		UserSubscriptionId: 0, Status: twmodel.LinkStatusActive, CreatedAt: now,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}

	c, w := jsonContext(http.MethodGet, "/api/transwork/subscription/admin/links?user_id=5", nil)
	ListStripeSubscriptionLinks(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	rows := resp["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 link, got %d", len(rows))
	}
	if got := int(rows[0].(map[string]any)["user_subscription_id"].(float64)); got != sub.Id {
		t.Fatalf("response must carry the resolved user_subscription_id %d, got %d", sub.Id, got)
	}
	// Persisted: a re-read of the row (not the response) shows the backfilled id.
	var reread twmodel.StripeSubscriptionLink
	if err := model.DB.Where("stripe_subscription_id = ?", "sub_fresh").First(&reread).Error; err != nil {
		t.Fatalf("re-read link: %v", err)
	}
	if reread.UserSubscriptionId != sub.Id {
		t.Fatalf("resolved user_subscription_id must be PERSISTED, got %d want %d", reread.UserSubscriptionId, sub.Id)
	}
}
