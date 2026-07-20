package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	twmodel "github.com/QuantumNous/new-api/transwork/model"

	"github.com/gin-gonic/gin"
)

// --- helpers -----------------------------------------------------------------

type mockPortalCreator struct {
	customerId string
	returnURL  string
	url        string
	err        error
}

func (m *mockPortalCreator) Create(customerId, returnURL string) (string, error) {
	m.customerId = customerId
	m.returnURL = returnURL
	if m.url == "" {
		m.url = "https://billing.stripe.test/session/xyz"
	}
	return m.url, m.err
}

func swapPortalCreator(t *testing.T, m stripePortalCreator) {
	t.Helper()
	orig := portalCreator
	portalCreator = m
	t.Cleanup(func() { portalCreator = orig })
}

// jsonContext builds a gin test context carrying a JSON body and query string,
// so handlers that read c.ShouldBindJSON / c.Query can be exercised directly.
func jsonContext(method, target string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var r *bytes.Reader
	if body == nil {
		r = bytes.NewReader(nil)
	} else {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, w
}

func decodeAPI(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return m
}

// --- WS-1.6 / test 14 (O9): portal customer id comes ONLY from the auth user ---

// SECURITY (O9): the portal endpoint must derive the Stripe customer id solely
// from the authenticated user (c.GetInt("id")). If it honored a request-supplied
// user_id/customer_id, any logged-in user could open ANOTHER user's Stripe
// billing portal — view their card, cancel their subscription. This test hands
// the handler an authenticated user (7) while smuggling a victim's ids (99,
// cus_victim) through BOTH the query string and the JSON body, and asserts the
// Stripe call receives the AUTHENTICATED user's customer id, never the injected
// one.
func TestPortalSessionDerivesCustomerFromAuthUserIgnoringRequestIds_O9(t *testing.T) {
	setupDB(t)
	authed := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_authed", StripeCustomerId: "cus_authed",
		UserId: 7, Status: twmodel.LinkStatusActive,
	}
	victim := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_victim", StripeCustomerId: "cus_victim",
		UserId: 99, Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(authed).Error; err != nil {
		t.Fatalf("seed authed link: %v", err)
	}
	if err := model.DB.Create(victim).Error; err != nil {
		t.Fatalf("seed victim link: %v", err)
	}
	mock := &mockPortalCreator{}
	swapPortalCreator(t, mock)

	// Adversarial: the victim's ids are supplied in both query and body.
	body := []byte(`{"user_id":99,"customer_id":"cus_victim","subscription_id":"sub_victim"}`)
	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/portal?user_id=99&customer_id=cus_victim", body)
	c.Set("id", 7) // the authenticated user, as TokenAuth would set it

	CreateSubscriptionPortalSession(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a user with a recurring sub, got %d body=%s", w.Code, w.Body.String())
	}
	if mock.customerId != "cus_authed" {
		t.Fatalf("O9 BREACH: portal opened for customer %q; must be the authed user's cus_authed, never the request-supplied cus_victim", mock.customerId)
	}
	resp := decodeAPI(t, w)
	if resp["url"] != mock.url {
		t.Fatalf("expected portal url %q returned, got %v", mock.url, resp["url"])
	}
}

// SECURITY (O9): the desktop status endpoint must derive the reported plan /
// auto_renew / current_period_end SOLELY from the authenticated user
// (c.GetInt("id")), never from a request-supplied user_id. If it honored one, any
// logged-in user could read ANOTHER user's subscription state. This seeds two
// fully distinct users (self=7, victim=99) — each with their own UserSubscription
// and link — smuggles the victim's id through BOTH the query string and the JSON
// body, authenticates as user 7, and asserts the response reflects ONLY user 7's
// plan. It regression-locks the IDOR invariant: it FAILS the moment the handler
// starts reading c.Query("user_id") (or the body id) instead of the auth id.
func TestDesktopStatusDerivesUserFromAuthUserIgnoringRequestIds_O9(t *testing.T) {
	setupDB(t)

	selfPlan := &model.SubscriptionPlan{Title: "Self Plan", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	victimPlan := &model.SubscriptionPlan{Title: "Victim Plan", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	if err := model.DB.Create(selfPlan).Error; err != nil {
		t.Fatalf("create self plan: %v", err)
	}
	if err := model.DB.Create(victimPlan).Error; err != nil {
		t.Fatalf("create victim plan: %v", err)
	}

	selfEnd := time.Now().Add(240 * time.Hour).Unix()
	victimEnd := time.Now().Add(480 * time.Hour).Unix()
	selfSub := &model.UserSubscription{UserId: 7, PlanId: selfPlan.Id, EndTime: selfEnd, Status: "active", AmountTotal: 1000}
	victimSub := &model.UserSubscription{UserId: 99, PlanId: victimPlan.Id, EndTime: victimEnd, Status: "active", AmountTotal: 2000}
	if err := model.DB.Create(selfSub).Error; err != nil {
		t.Fatalf("create self sub: %v", err)
	}
	if err := model.DB.Create(victimSub).Error; err != nil {
		t.Fatalf("create victim sub: %v", err)
	}

	selfPeriodEnd := selfEnd + 3600
	victimPeriodEnd := victimEnd + 7200
	selfLink := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_self", StripeCustomerId: "cus_self", UserId: 7, PlanId: selfPlan.Id,
		UserSubscriptionId: selfSub.Id, Status: twmodel.LinkStatusActive, AutoRenew: true, CurrentPeriodEnd: selfPeriodEnd,
	}
	victimLink := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_victim", StripeCustomerId: "cus_victim", UserId: 99, PlanId: victimPlan.Id,
		UserSubscriptionId: victimSub.Id, Status: twmodel.LinkStatusActive, AutoRenew: false, CurrentPeriodEnd: victimPeriodEnd,
	}
	if err := model.DB.Create(selfLink).Error; err != nil {
		t.Fatalf("create self link: %v", err)
	}
	if err := model.DB.Create(victimLink).Error; err != nil {
		t.Fatalf("create victim link: %v", err)
	}

	// Adversarial: the victim's id is supplied in both query and body.
	body := []byte(`{"user_id":99}`)
	c, w := jsonContext(http.MethodGet, "/api/desktop/subscription/self?user_id=99", body)
	c.Set("id", 7) // the authenticated user, as TokenAuth would set it

	GetDesktopSubscriptionStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeAPI(t, w)
	if resp["plan"] != "Self Plan" {
		t.Fatalf("O9 BREACH: reported plan %q; must be the authed user 7's \"Self Plan\", never the request-supplied victim's \"Victim Plan\"", resp["plan"])
	}
	if int64(resp["end_time"].(float64)) != selfEnd {
		t.Fatalf("O9 BREACH: reported end_time %v; must be user 7's %d, never the victim's %d", resp["end_time"], selfEnd, victimEnd)
	}
	if resp["auto_renew"] != true {
		t.Fatalf("O9 BREACH: reported auto_renew %v; must reflect user 7's link (true), never the victim's (false)", resp["auto_renew"])
	}
	if int64(resp["current_period_end"].(float64)) != selfPeriodEnd {
		t.Fatalf("O9 BREACH: reported current_period_end %v; must be user 7's %d, never the victim's %d", resp["current_period_end"], selfPeriodEnd, victimPeriodEnd)
	}
}

// A user with no live recurring Stripe subscription must get a clean 404 (not a
// 500), so the desktop app can hide the manage button rather than surface an
// error. Also proves the endpoint never fabricates a portal from thin air.
func TestPortalSession404WhenNoRecurringSub(t *testing.T) {
	setupDB(t)
	mock := &mockPortalCreator{}
	swapPortalCreator(t, mock)

	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/portal", nil)
	c.Set("id", 8) // authenticated, but has no link at all

	CreateSubscriptionPortalSession(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no recurring sub, got %d", w.Code)
	}
	if mock.customerId != "" {
		t.Fatalf("Stripe must not be called when the user has no recurring sub, got customer %q", mock.customerId)
	}
}

// A canceled link no longer bills, so it is not "recurring": the portal endpoint
// must treat it as no-subscription (404), not open a portal for a dead sub.
func TestPortalSessionIgnoresCanceledLink(t *testing.T) {
	setupDB(t)
	dead := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_dead", StripeCustomerId: "cus_dead",
		UserId: 12, Status: twmodel.LinkStatusCanceled,
	}
	if err := model.DB.Create(dead).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}
	mock := &mockPortalCreator{}
	swapPortalCreator(t, mock)

	c, w := jsonContext(http.MethodPost, "/api/desktop/subscription/portal", nil)
	c.Set("id", 12)
	CreateSubscriptionPortalSession(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a canceled-only link, got %d", w.Code)
	}
}

// --- WS-1.6 / test 15: desktop status fields ---------------------------------

// The pop-up reads plan / end_time / auto_renew / current_period_end /
// is_recurring from one authed call (F3). For a user with a live recurring link,
// is_recurring must be true and the display fields must come from the link, while
// plan/end_time come from the joined UserSubscription.
func TestDesktopStatusReturnsFieldsForRecurringUser(t *testing.T) {
	setupDB(t)
	plan := &model.SubscriptionPlan{Title: "Pro Monthly", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	end := time.Now().Add(240 * time.Hour).Unix()
	// Amounts are in raw quota; QuotaPerUnit is large, so use realistic plan-sized
	// values that survive the quota→credits conversion (a tiny amount floors to 0).
	sub := &model.UserSubscription{UserId: 7, PlanId: plan.Id, EndTime: end, Status: "active", AmountTotal: 10_000_000, AmountUsed: 4_000_000}
	if err := model.DB.Create(sub).Error; err != nil {
		t.Fatalf("create sub: %v", err)
	}
	periodEnd := end + 3600
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_s1", StripeCustomerId: "cus_s1", UserId: 7, PlanId: plan.Id,
		UserSubscriptionId: sub.Id, Status: twmodel.LinkStatusActive, AutoRenew: true, CurrentPeriodEnd: periodEnd,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}

	c, w := jsonContext(http.MethodGet, "/api/desktop/subscription/self", nil)
	c.Set("id", 7)
	GetDesktopSubscriptionStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if resp["plan"] != "Pro Monthly" {
		t.Fatalf("expected plan title, got %v", resp["plan"])
	}
	if int64(resp["end_time"].(float64)) != end {
		t.Fatalf("expected end_time %d, got %v", end, resp["end_time"])
	}
	if resp["is_recurring"] != true {
		t.Fatalf("expected is_recurring true, got %v", resp["is_recurring"])
	}
	if resp["auto_renew"] != true {
		t.Fatalf("expected auto_renew true, got %v", resp["auto_renew"])
	}
	if int64(resp["current_period_end"].(float64)) != periodEnd {
		t.Fatalf("expected current_period_end %d, got %v", periodEnd, resp["current_period_end"])
	}
	// The active bucket's allowance must surface so the profile can show the
	// monthly (resetting) credit next to the permanent wallet.
	if resp["has_allowance"] != true {
		t.Fatalf("expected has_allowance true for an active bucket, got %v", resp["has_allowance"])
	}
	if resp["unlimited"] != false {
		t.Fatalf("expected unlimited false for a finite plan, got %v", resp["unlimited"])
	}
	if resp["monthly_total"].(float64) <= 0 {
		t.Fatalf("expected positive monthly_total, got %v", resp["monthly_total"])
	}
	// 4M of 10M quota is spent, so remaining must be positive and strictly below
	// total — the whole point of surfacing the resetting bucket's live balance.
	if resp["monthly_remaining"].(float64) <= 0 {
		t.Fatalf("expected positive monthly_remaining, got %v", resp["monthly_remaining"])
	}
	if resp["monthly_remaining"].(float64) >= resp["monthly_total"].(float64) {
		t.Fatalf("expected monthly_remaining < monthly_total after usage, got %v vs %v", resp["monthly_remaining"], resp["monthly_total"])
	}
}

// An unlimited plan (AmountTotal==0) must report unlimited=true rather than a
// zero allowance, so the profile shows "Unlimited" instead of "0 monthly".
func TestDesktopStatusUnlimitedAllowance(t *testing.T) {
	setupDB(t)
	plan := &model.SubscriptionPlan{Title: "Unlimited", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	end := time.Now().Add(240 * time.Hour).Unix()
	sub := &model.UserSubscription{UserId: 11, PlanId: plan.Id, EndTime: end, Status: "active", AmountTotal: 0}
	if err := model.DB.Create(sub).Error; err != nil {
		t.Fatalf("create sub: %v", err)
	}

	c, w := jsonContext(http.MethodGet, "/api/desktop/subscription/self", nil)
	c.Set("id", 11)
	GetDesktopSubscriptionStatus(c)

	resp := decodeAPI(t, w)
	if resp["has_allowance"] != true {
		t.Fatalf("expected has_allowance true, got %v", resp["has_allowance"])
	}
	if resp["unlimited"] != true {
		t.Fatalf("expected unlimited true for AmountTotal==0, got %v", resp["unlimited"])
	}
}

// An expired subscription grants nothing this period: surfacing its old amount
// would overstate available credit, so has_allowance must be false.
func TestDesktopStatusExpiredHasNoAllowance(t *testing.T) {
	setupDB(t)
	plan := &model.SubscriptionPlan{Title: "Lapsed", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	end := time.Now().Add(-240 * time.Hour).Unix() // ended 10 days ago
	sub := &model.UserSubscription{UserId: 12, PlanId: plan.Id, EndTime: end, Status: "expired", AmountTotal: 1000}
	if err := model.DB.Create(sub).Error; err != nil {
		t.Fatalf("create sub: %v", err)
	}

	c, w := jsonContext(http.MethodGet, "/api/desktop/subscription/self", nil)
	c.Set("id", 12)
	GetDesktopSubscriptionStatus(c)

	resp := decodeAPI(t, w)
	if resp["has_allowance"] != false {
		t.Fatalf("expected has_allowance false for an expired sub, got %v", resp["has_allowance"])
	}
}

// A user with a subscription but no Stripe link is not recurring: the desktop app
// must NOT render a "Manage subscription" button that would 404, so is_recurring
// is false while plan/end_time still populate.
func TestDesktopStatusNotRecurringWithoutLink(t *testing.T) {
	setupDB(t)
	plan := &model.SubscriptionPlan{Title: "One-off", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	end := time.Now().Add(240 * time.Hour).Unix()
	sub := &model.UserSubscription{UserId: 4, PlanId: plan.Id, EndTime: end, Status: "active", AmountTotal: 1000}
	if err := model.DB.Create(sub).Error; err != nil {
		t.Fatalf("create sub: %v", err)
	}

	c, w := jsonContext(http.MethodGet, "/api/desktop/subscription/self", nil)
	c.Set("id", 4)
	GetDesktopSubscriptionStatus(c)

	resp := decodeAPI(t, w)
	if resp["is_recurring"] != false {
		t.Fatalf("expected is_recurring false without a link, got %v", resp["is_recurring"])
	}
	if resp["plan"] != "One-off" {
		t.Fatalf("expected plan still populated, got %v", resp["plan"])
	}
}

// --- WS-1.7 / test 16: admin cancel propagates to Stripe, idempotent ----------

// An admin lapsing a member must also cancel their Stripe subscription, or Stripe
// keeps billing a user who lost access (the Translide failure, admin side). The
// resulting customer.subscription.deleted is redelivered, so a repeat cancel must
// be a no-op returning success — never a second Stripe call.
func TestAdminCancelStripeMarksLinkAndIsIdempotent(t *testing.T) {
	setupDB(t)
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_ac1", UserId: 3, PlanId: 2, UserSubscriptionId: 55,
		Status: twmodel.LinkStatusActive, AutoRenew: true,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}
	mock := &mockCanceler{}
	swapCanceler(t, mock)

	body := []byte(`{"stripe_subscription_id":"sub_ac1"}`)
	c, w := jsonContext(http.MethodPost, "/api/transwork/subscription/admin/cancel-stripe", body)
	CancelStripeSubscription(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if resp := decodeAPI(t, w); resp["success"] != true {
		t.Fatalf("expected success true, got %v", resp["success"])
	}
	if len(mock.calls) != 1 || mock.calls[0] != "sub_ac1" {
		t.Fatalf("expected one Stripe cancel of sub_ac1, got %v", mock.calls)
	}
	got := getLink(t, "sub_ac1")
	if got.Status != twmodel.LinkStatusCanceled {
		t.Fatalf("expected link canceled, got %q", got.Status)
	}
	if got.AutoRenew {
		t.Fatalf("expected auto_renew cleared on cancel")
	}

	// Second call: already canceled → no-op, no additional Stripe call.
	body2 := []byte(`{"stripe_subscription_id":"sub_ac1"}`)
	c2, w2 := jsonContext(http.MethodPost, "/api/transwork/subscription/admin/cancel-stripe", body2)
	CancelStripeSubscription(c2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second call expected 200, got %d", w2.Code)
	}
	if resp := decodeAPI(t, w2); resp["success"] != true {
		t.Fatalf("second call expected success true, got %v", resp["success"])
	}
	if len(mock.calls) != 1 {
		t.Fatalf("already-canceled link must not call Stripe again, calls=%v", mock.calls)
	}
}

// The admin action also accepts a local user_subscription_id (the modal knows the
// local row, not always the sub_… id), and must resolve the same link.
func TestAdminCancelStripeByUserSubscriptionId(t *testing.T) {
	setupDB(t)
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_ac2", UserId: 3, PlanId: 2, UserSubscriptionId: 77,
		Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}
	mock := &mockCanceler{}
	swapCanceler(t, mock)

	body := []byte(`{"user_subscription_id":77}`)
	c, w := jsonContext(http.MethodPost, "/api/transwork/subscription/admin/cancel-stripe", body)
	CancelStripeSubscription(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(mock.calls) != 1 || mock.calls[0] != "sub_ac2" {
		t.Fatalf("expected cancel of sub_ac2 resolved via user_subscription_id, got %v", mock.calls)
	}
}

// --- WS-1.8 / test 17: admin links endpoint (F2) -----------------------------

// The per-user admin modal merges overlay link rows into the upstream
// subscription rows by user_subscription_id. The endpoint must return only the
// requested user's links and expose user_subscription_id so the merge is keyable.
func TestAdminListLinksReturnsUserRowsMergeableById(t *testing.T) {
	setupDB(t)
	for _, l := range []*twmodel.StripeSubscriptionLink{
		{StripeSubscriptionId: "sub_u5a", UserId: 5, PlanId: 1, UserSubscriptionId: 11, Status: twmodel.LinkStatusActive},
		{StripeSubscriptionId: "sub_u5b", UserId: 5, PlanId: 2, UserSubscriptionId: 12, Status: twmodel.LinkStatusCanceled},
		{StripeSubscriptionId: "sub_u6", UserId: 6, PlanId: 1, UserSubscriptionId: 13, Status: twmodel.LinkStatusActive},
	} {
		if err := model.DB.Create(l).Error; err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}

	c, w := jsonContext(http.MethodGet, "/api/transwork/subscription/admin/links?user_id=5", nil)
	ListStripeSubscriptionLinks(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if resp["success"] != true {
		t.Fatalf("expected success true, got %v", resp["success"])
	}
	rows, ok := resp["data"].([]any)
	if !ok {
		t.Fatalf("expected data array, got %T", resp["data"])
	}
	if len(rows) != 2 {
		t.Fatalf("expected only user 5's 2 links, got %d", len(rows))
	}
	seen := map[float64]bool{}
	for _, r := range rows {
		row := r.(map[string]any)
		usid, present := row["user_subscription_id"]
		if !present {
			t.Fatalf("row missing user_subscription_id (merge key), row=%v", row)
		}
		seen[usid.(float64)] = true
	}
	if !seen[11] || !seen[12] {
		t.Fatalf("expected user_subscription_ids 11 and 12 present, got %v", seen)
	}
}
