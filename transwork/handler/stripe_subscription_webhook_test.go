package handler

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	twmodel "github.com/QuantumNous/new-api/transwork/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMapRWMutex.Unlock()
	os.Exit(m.Run())
}

// --- test helpers ------------------------------------------------------------

// setupDB gives each test a fresh in-memory SQLite bound to model.DB. A fresh DB
// per test avoids cross-test leakage and, crucially, sidesteps sibling tests in
// this package (elevenlabs) that open their own DB and nil out model.DB in their
// cleanup — so we (re)establish it at the start of every subscription test.
func setupDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&model.Option{},
		&model.SubscriptionOrder{},
		&model.SubscriptionPlan{},
		&model.UserSubscription{},
		&twmodel.StripeSubscriptionLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	prev := model.DB
	prevSQLite := common.UsingSQLite
	model.DB = db
	common.UsingSQLite = true
	t.Cleanup(func() {
		model.DB = prev
		common.UsingSQLite = prevSQLite
	})
}

func setOption(t *testing.T, key, value string) {
	t.Helper()
	common.OptionMapRWMutex.Lock()
	common.OptionMap[key] = value
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		delete(common.OptionMap, key)
		common.OptionMapRWMutex.Unlock()
	})
}

func event(id string, typ stripe.EventType, obj map[string]interface{}) stripe.Event {
	return stripe.Event{ID: id, Type: typ, Data: &stripe.EventData{Object: obj}}
}

func countLinks(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := model.DB.Model(&twmodel.StripeSubscriptionLink{}).Count(&n).Error; err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

func getLink(t *testing.T, subId string) *twmodel.StripeSubscriptionLink {
	t.Helper()
	var link twmodel.StripeSubscriptionLink
	if err := model.DB.Where("stripe_subscription_id = ?", subId).First(&link).Error; err != nil {
		t.Fatalf("get link %s: %v", subId, err)
	}
	return &link
}

func getSub(t *testing.T, id int) *model.UserSubscription {
	t.Helper()
	var sub model.UserSubscription
	if err := model.DB.Where("id = ?", id).First(&sub).Error; err != nil {
		t.Fatalf("get sub %d: %v", id, err)
	}
	return &sub
}

type mockCanceler struct {
	calls []string
	err   error
}

func (m *mockCanceler) Cancel(id string) error {
	m.calls = append(m.calls, id)
	return m.err
}

func swapCanceler(t *testing.T, m stripeSubscriptionCanceler) {
	t.Helper()
	orig := subscriptionCanceler
	subscriptionCanceler = m
	t.Cleanup(func() { subscriptionCanceler = orig })
}

func newWebhookContext(body []byte, sig string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/transwork/stripe/subscription-webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	c.Request = req
	return c, w
}

func signedHeader(secret string, payload []byte) string {
	ts := time.Now().Unix()
	mac := webhook.ComputeSignature(time.Unix(ts, 0), payload, secret)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac))
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
func (errReader) Close() error             { return nil }

// --- WS-1.2: signature / transport contract ---------------------------------

// The signature check is the whole trust boundary: an attacker who could forge an
// event with a bad signature and still mutate state could grant themselves an
// endless subscription. A bad signature must abort with 400 and write nothing.
func TestWebhookBadSignatureReturns400AndMutatesNothing(t *testing.T) {
	setupDB(t)
	setOption(t, OptionKeyStripeWebhookSecret, "whsec_test")

	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"mode":"subscription","subscription":"sub_x"}}}`)
	c, w := newWebhookContext(body, "t=1,v1=deadbeef")
	StripeSubscriptionWebhook(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad signature, got %d", w.Code)
	}
	if n := countLinks(t); n != 0 {
		t.Fatalf("bad signature must not write rows, found %d", n)
	}
}

// A body we cannot even read is an infrastructure failure, not a client error;
// 503 tells Stripe to retry later rather than treating the event as consumed.
func TestWebhookBodyReadFailureReturns503(t *testing.T) {
	setupDB(t)
	setOption(t, OptionKeyStripeWebhookSecret, "whsec_test")

	c, w := newWebhookContext(nil, "t=1,v1=deadbeef")
	c.Request.Body = errReader{}
	StripeSubscriptionWebhook(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on body-read failure, got %d", w.Code)
	}
}

// An unhandled but validly-signed event must return 200 so Stripe stops
// retrying — we intentionally subscribe to more than we act on.
func TestWebhookUnknownEventReturns200(t *testing.T) {
	setupDB(t)
	secret := "whsec_test"
	setOption(t, OptionKeyStripeWebhookSecret, secret)

	body := []byte(`{"id":"evt_unknown","type":"customer.updated","data":{"object":{}}}`)
	c, w := newWebhookContext(body, signedHeader(secret, body))
	StripeSubscriptionWebhook(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on unknown event, got %d", w.Code)
	}
}

// --- WS-1.3: event handling --------------------------------------------------

func TestCheckoutCompletedUpsertsLinkAndResolvesUserPlan(t *testing.T) {
	setupDB(t)
	order := &model.SubscriptionOrder{UserId: 7, PlanId: 3, TradeNo: "ref_abc", Status: common.TopUpStatusPending}
	if err := order.Insert(); err != nil {
		t.Fatalf("insert order: %v", err)
	}

	e := event("evt_c1", stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"mode":                "subscription",
		"subscription":        "sub_1",
		"customer":            "cus_1",
		"client_reference_id": "ref_abc",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	link := getLink(t, "sub_1")
	if link.UserId != 7 || link.PlanId != 3 {
		t.Fatalf("expected user/plan resolved from order, got user=%d plan=%d", link.UserId, link.PlanId)
	}
	if link.StripeCustomerId != "cus_1" {
		t.Fatalf("expected customer captured, got %q", link.StripeCustomerId)
	}
	if link.Status != twmodel.LinkStatusActive {
		t.Fatalf("expected active link, got %q", link.Status)
	}
	// UserSubscriptionId=0 is tolerated here (O2 capture race).
	if link.UserSubscriptionId != 0 {
		t.Fatalf("expected UserSubscriptionId to remain 0 at capture, got %d", link.UserSubscriptionId)
	}
}

func TestCheckoutCompletedNonSubscriptionModeIgnored(t *testing.T) {
	setupDB(t)
	e := event("evt_pay", stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"mode":         "payment",
		"subscription": "sub_should_ignore",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if n := countLinks(t); n != 0 {
		t.Fatalf("one-time payment mode must not create a subscription link, found %d", n)
	}
}

// Translide guard: a user re-subscribing while an old Stripe subscription is
// still live must have the old one canceled, or Stripe double-bills for access
// the app only grants once. This is the exact failure Translide shipped.
func TestCheckoutCompletedCancelsSupersededOldSubscription_TranslideGuard(t *testing.T) {
	setupDB(t)
	order := &model.SubscriptionOrder{UserId: 42, PlanId: 5, TradeNo: "ref_new", Status: common.TopUpStatusPending}
	if err := order.Insert(); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	old := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_old", UserId: 42, PlanId: 5, Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(old).Error; err != nil {
		t.Fatalf("seed old link: %v", err)
	}
	mock := &mockCanceler{}
	swapCanceler(t, mock)

	e := event("evt_c2", stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"mode":                "subscription",
		"subscription":        "sub_new",
		"customer":            "cus_42",
		"client_reference_id": "ref_new",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(mock.calls) != 1 || mock.calls[0] != "sub_old" {
		t.Fatalf("expected Stripe cancel of sub_old, got calls=%v", mock.calls)
	}
	if got := getLink(t, "sub_old"); got.Status != twmodel.LinkStatusCanceled {
		t.Fatalf("expected old link canceled, got %q", got.Status)
	}
	if got := getLink(t, "sub_new"); got.Status != twmodel.LinkStatusActive {
		t.Fatalf("expected new link active, got %q", got.Status)
	}
}

// O7: cancelling the superseded sub calls Stripe's API, which can fail
// (bad/expired key, network). That failure must not wedge event processing:
// the link is already canceled locally and the webhook still returns success.
func TestCheckoutCompletedCancelFailureIsNonFatal_O7(t *testing.T) {
	setupDB(t)
	old := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_old", UserId: 9, PlanId: 1, Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(old).Error; err != nil {
		t.Fatalf("seed old link: %v", err)
	}
	order := &model.SubscriptionOrder{UserId: 9, PlanId: 1, TradeNo: "ref_9", Status: common.TopUpStatusPending}
	if err := order.Insert(); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	swapCanceler(t, &mockCanceler{err: errors.New("stripe down")})

	e := event("evt_c3", stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"mode":                "subscription",
		"subscription":        "sub_new9",
		"client_reference_id": "ref_9",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("cancel failure must be non-fatal, got err %v", err)
	}
	if got := getLink(t, "sub_old"); got.Status != twmodel.LinkStatusCanceled {
		t.Fatalf("old link should still be canceled in DB despite Stripe failure, got %q", got.Status)
	}
}

func seedLinkedSub(t *testing.T, subId string, userId, planId int, endTime int64, amountUsed int64, durationUnit string, durationValue int) (*model.UserSubscription, *twmodel.StripeSubscriptionLink) {
	t.Helper()
	plan := &model.SubscriptionPlan{DurationUnit: durationUnit, DurationValue: durationValue, Enabled: true}
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	sub := &model.UserSubscription{
		UserId: userId, PlanId: plan.Id, EndTime: endTime, AmountUsed: amountUsed,
		AmountTotal: 1000, Status: "active", StartTime: endTime - 100,
	}
	if err := model.DB.Create(sub).Error; err != nil {
		t.Fatalf("create sub: %v", err)
	}
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: subId, UserId: userId, PlanId: plan.Id,
		UserSubscriptionId: sub.Id, Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("create link: %v", err)
	}
	return sub, link
}

// A renewal payment (subscription_cycle) must extend EndTime by one interval and
// refill quota (AmountUsed=0). Without this the user pays month 2 but loses
// access after month 1 — the core bug this feature fixes.
func TestInvoiceSucceededCycleExtendsAndRefills(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_r1", 1, 0, future, 500, model.SubscriptionDurationMonth, 1)
	wantEnd := time.Unix(future, 0).AddDate(0, 1, 0).Unix()

	e := event("evt_i1", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_1",
		"subscription":   "sub_r1",
		"billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got := getSub(t, sub.Id)
	if got.EndTime != wantEnd {
		t.Fatalf("expected EndTime extended to %d, got %d", wantEnd, got.EndTime)
	}
	if got.AmountUsed != 0 {
		t.Fatalf("expected quota refilled (AmountUsed=0), got %d", got.AmountUsed)
	}
}

// subscription_create is the first payment, already fulfilled by the upstream
// endpoint. Acting on it here would double-extend the very first period.
func TestInvoiceSucceededCreateReasonIsIgnored(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_r2", 1, 0, future, 300, model.SubscriptionDurationMonth, 1)

	e := event("evt_i2", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_2",
		"subscription":   "sub_r2",
		"billing_reason": "subscription_create",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got := getSub(t, sub.Id)
	if got.EndTime != future || got.AmountUsed != 300 {
		t.Fatalf("subscription_create must not touch the sub, got end=%d used=%d", got.EndTime, got.AmountUsed)
	}
	if link := getLink(t, "sub_r2"); link.LastInvoiceId != "" {
		t.Fatalf("subscription_create must not record an invoice id, got %q", link.LastInvoiceId)
	}
}

// deleted lapses future renewal but, by default (O1), does not yank access the
// user already paid for; the expiry sweep handles that at period end.
func TestSubscriptionDeletedCancelsLinkWithoutForcingEndTime(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_d1", 1, 0, future, 0, model.SubscriptionDurationMonth, 1)

	e := event("evt_d1", stripe.EventTypeCustomerSubscriptionDeleted, map[string]interface{}{
		"id": "sub_d1",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if link := getLink(t, "sub_d1"); link.Status != twmodel.LinkStatusCanceled {
		t.Fatalf("expected link canceled, got %q", link.Status)
	}
	if got := getSub(t, sub.Id); got.EndTime != future {
		t.Fatalf("O1: EndTime must not be forced to now, got %d want %d", got.EndTime, future)
	}
}

// updated is display-only: it keeps the auto-renew flag and period end honest but
// must never move access dates or quota, or a Portal toggle would grant/revoke
// service without a payment event.
func TestSubscriptionUpdatedSyncsAutoRenewOnly(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_u1", 1, 0, future, 500, model.SubscriptionDurationMonth, 1)
	newPeriodEnd := float64(future + 3600)

	e := event("evt_u1", stripe.EventTypeCustomerSubscriptionUpdated, map[string]interface{}{
		"id":                   "sub_u1",
		"cancel_at_period_end": true,
		"current_period_end":   newPeriodEnd,
		"status":               "active",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	link := getLink(t, "sub_u1")
	if link.AutoRenew {
		t.Fatalf("cancel_at_period_end=true should set AutoRenew=false")
	}
	if link.CurrentPeriodEnd != int64(newPeriodEnd) {
		t.Fatalf("expected CurrentPeriodEnd synced to %d, got %d", int64(newPeriodEnd), link.CurrentPeriodEnd)
	}
	if got := getSub(t, sub.Id); got.EndTime != future || got.AmountUsed != 500 {
		t.Fatalf("updated must not change access/quota, got end=%d used=%d", got.EndTime, got.AmountUsed)
	}
}

func TestInvoicePaymentFailedMarksPastDue(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_f1", 1, 0, future, 0, model.SubscriptionDurationMonth, 1)

	e := event("evt_f1", stripe.EventTypeInvoicePaymentFailed, map[string]interface{}{
		"id":           "in_f1",
		"subscription": "sub_f1",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if link := getLink(t, "sub_f1"); link.Status != twmodel.LinkStatusPastDue {
		t.Fatalf("expected past_due, got %q", link.Status)
	}
	// No access change: a single decline must not cut off service (dunning owns it).
	if got := getSub(t, sub.Id); got.EndTime != future {
		t.Fatalf("payment_failed must not change EndTime, got %d", got.EndTime)
	}
}

// When UserSubscriptionId is unresolved and a user holds two subs of the same
// plan (O3), the link must extend the one minted by ITS checkout — identified by
// the StartTime nearest the link's CreatedAt — not an unrelated concurrent sub.
func TestLazyResolutionPicksSubNearestLinkCreatedAt_O3(t *testing.T) {
	setupDB(t)
	plan := &model.SubscriptionPlan{DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
	if err := model.DB.Create(plan).Error; err != nil {
		t.Fatalf("plan: %v", err)
	}
	now := time.Now().Unix()
	// Older sub (start far before the link) and the newer sub minted by checkout.
	oldStart := now - 100000
	newStart := now
	oldSub := &model.UserSubscription{UserId: 3, PlanId: plan.Id, StartTime: oldStart, EndTime: now + 5000, AmountUsed: 100, AmountTotal: 1000, Status: "active"}
	newSub := &model.UserSubscription{UserId: 3, PlanId: plan.Id, StartTime: newStart, EndTime: now + 4000, AmountUsed: 200, AmountTotal: 1000, Status: "active"}
	if err := model.DB.Create(oldSub).Error; err != nil {
		t.Fatalf("oldSub: %v", err)
	}
	if err := model.DB.Create(newSub).Error; err != nil {
		t.Fatalf("newSub: %v", err)
	}
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_lazy", UserId: 3, PlanId: plan.Id,
		UserSubscriptionId: 0, Status: twmodel.LinkStatusActive, CreatedAt: newStart,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("link: %v", err)
	}

	e := event("evt_lz", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_lz",
		"subscription":   "sub_lazy",
		"billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got := getLink(t, "sub_lazy")
	if got.UserSubscriptionId != newSub.Id {
		t.Fatalf("expected lazy resolution to pick newSub (id=%d, nearest StartTime), got %d", newSub.Id, got.UserSubscriptionId)
	}
	if extended := getSub(t, newSub.Id); extended.AmountUsed != 0 {
		t.Fatalf("expected the resolved sub to be refilled, got AmountUsed=%d", extended.AmountUsed)
	}
	if untouched := getSub(t, oldSub.Id); untouched.AmountUsed != 100 {
		t.Fatalf("the unrelated concurrent sub must be untouched, got AmountUsed=%d", untouched.AmountUsed)
	}
}

// --- WS-1.4: idempotency -----------------------------------------------------

// Stripe delivers at-least-once: the same subscription_cycle invoice can arrive
// twice. It must extend EndTime by exactly one interval, never two, or a single
// month's payment silently buys two.
func TestReplayedInvoiceExtendsExactlyOnce(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_idem", 1, 0, future, 400, model.SubscriptionDurationMonth, 1)
	wantEnd := time.Unix(future, 0).AddDate(0, 1, 0).Unix()

	e := event("evt_idem_a", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_idem",
		"subscription":   "sub_idem",
		"billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// Same invoice id, different event id (a genuine Stripe redelivery).
	e2 := event("evt_idem_b", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_idem",
		"subscription":   "sub_idem",
		"billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(),e2); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got := getSub(t, sub.Id); got.EndTime != wantEnd {
		t.Fatalf("replayed invoice must extend exactly once: want %d got %d", wantEnd, got.EndTime)
	}
}

// A redelivery of the identical event id must short-circuit even before the
// invoice guard — the cheapest dedup and a defense against partial replays.
func TestReplayedEventIdShortCircuits(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_ev", 1, 0, future, 0, model.SubscriptionDurationMonth, 1)
	wantEnd := time.Unix(future, 0).AddDate(0, 1, 0).Unix()

	e := event("evt_same", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_ev",
		"subscription":   "sub_ev",
		"billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := handleSubscriptionEvent(context.Background(),e); err != nil {
		t.Fatalf("replay same event id: %v", err)
	}
	if got := getSub(t, sub.Id); got.EndTime != wantEnd {
		t.Fatalf("same event id replay must not double-extend: want %d got %d", wantEnd, got.EndTime)
	}
}

// --- SEC-1 / SEC-5: fail-closed on an unset signing secret -------------------

// With no signing secret configured, webhook.ConstructEventWithOptions would
// verify against an EMPTY HMAC key — which any attacker can compute — so an unset
// secret must FAIL CLOSED. A body signed with the empty key (the exact forgery an
// attacker would send) must be rejected with 503 and write zero rows. Without the
// SEC-1 gate this body verifies successfully and mutates state.
func TestWebhookEmptySecretFailsClosedAndMutatesNothing(t *testing.T) {
	setupDB(t)
	// Secret deliberately unset (setupDB does not seed it; ensure it's absent).
	common.OptionMapRWMutex.Lock()
	delete(common.OptionMap, OptionKeyStripeWebhookSecret)
	common.OptionMapRWMutex.Unlock()

	body := []byte(`{"id":"evt_forge","type":"checkout.session.completed","data":{"object":{"mode":"subscription","subscription":"sub_forge","client_reference_id":"ref_forge"}}}`)
	// Forge a valid-looking signature using the empty key — the publicly computable
	// HMAC an attacker would craft.
	c, w := newWebhookContext(body, signedHeader("", body))
	StripeSubscriptionWebhook(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unset secret must fail closed with 503, got %d", w.Code)
	}
	if n := countLinks(t); n != 0 {
		t.Fatalf("empty-secret request must not write rows, found %d", n)
	}
}

// --- CR-1 / SEC-2: unexpected processing errors must 5xx, not ACK-drop -------

// A validly-signed renewal whose UserSubscription cannot be resolved (endpoint A
// race, design O2) is a RETRYABLE failure: returning 200 would tell Stripe the
// paid renewal was consumed and lose it forever. It must return 5xx so Stripe
// redelivers. Without the fix the handler logs and returns 200.
func TestWebhookUnexpectedErrorReturns500(t *testing.T) {
	setupDB(t)
	secret := "whsec_test"
	setOption(t, OptionKeyStripeWebhookSecret, secret)
	// Link exists but there is NO matching UserSubscription → userSubId stays 0.
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_norace", UserId: 88, PlanId: 4, Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}

	body := []byte(`{"id":"evt_unexp","type":"invoice.payment_succeeded","data":{"object":{"id":"in_unexp","subscription":"sub_norace","billing_reason":"subscription_cycle"}}}`)
	c, w := newWebhookContext(body, signedHeader(secret, body))
	StripeSubscriptionWebhook(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unresolved renewal must return 500 so Stripe retries, got %d", w.Code)
	}
	// CR-2: no idempotency marker may be persisted, or the redelivery is suppressed.
	got := getLink(t, "sub_norace")
	if got.LastInvoiceId != "" || got.LastEventId != "" {
		t.Fatalf("unextended renewal must persist NO markers, got invoice=%q event=%q", got.LastInvoiceId, got.LastEventId)
	}
}

// --- CR-2: unresolved renewal is retryable and writes no markers (unit) ------

// Direct-handler view of CR-2: an unresolved renewal returns a non-nil (retryable)
// error AND leaves LastInvoiceId/LastEventId empty so a later redelivery — once
// endpoint A has created the UserSubscription — is NOT rejected by the idempotency
// guard. Without the fix the handler swallowed this as nil and stamped the markers.
func TestInvoiceUnresolvedRenewalIsRetryableAndPersistsNoMarkers(t *testing.T) {
	setupDB(t)
	link := &twmodel.StripeSubscriptionLink{
		StripeSubscriptionId: "sub_ur", UserId: 5, PlanId: 9, Status: twmodel.LinkStatusActive,
	}
	if err := model.DB.Create(link).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}

	e := event("evt_ur", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id":             "in_ur",
		"subscription":   "sub_ur",
		"billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(), e); err == nil {
		t.Fatalf("unresolved renewal must return a retryable error, got nil")
	}
	got := getLink(t, "sub_ur")
	if got.LastInvoiceId != "" || got.LastEventId != "" {
		t.Fatalf("no marker may be persisted when nothing extended, got invoice=%q event=%q", got.LastInvoiceId, got.LastEventId)
	}
}

// --- SEC-3 / CR-7: a replayed checkout must not cancel the current sub -------

// The Translide-via-replay hazard: a user subscribes (sub_A), then re-subscribes
// (sub_B, which cancels sub_A). If Stripe redelivers the OLD sub_A checkout, a
// handler without the LastEventId guard re-activates sub_A and its supersede query
// cancels the user's now-current, live sub_B — killing a paying subscription. The
// replay must be a no-op.
func TestCheckoutReplayIsNoOpAndDoesNotCancelCurrentSub_SEC3(t *testing.T) {
	setupDB(t)
	orderA := &model.SubscriptionOrder{UserId: 50, PlanId: 5, TradeNo: "ref_A", Status: common.TopUpStatusPending}
	if err := orderA.Insert(); err != nil {
		t.Fatalf("insert orderA: %v", err)
	}
	orderB := &model.SubscriptionOrder{UserId: 50, PlanId: 5, TradeNo: "ref_B", Status: common.TopUpStatusPending}
	if err := orderB.Insert(); err != nil {
		t.Fatalf("insert orderB: %v", err)
	}
	mock := &mockCanceler{}
	swapCanceler(t, mock)

	evtA := event("evt_A", stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"mode": "subscription", "subscription": "sub_A", "client_reference_id": "ref_A",
	})
	if err := handleSubscriptionEvent(context.Background(), evtA); err != nil {
		t.Fatalf("checkout A: %v", err)
	}
	evtB := event("evt_B", stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"mode": "subscription", "subscription": "sub_B", "client_reference_id": "ref_B",
	})
	if err := handleSubscriptionEvent(context.Background(), evtB); err != nil {
		t.Fatalf("checkout B: %v", err)
	}
	// After B: sub_A canceled, sub_B active, canceler called once for sub_A.
	if len(mock.calls) != 1 || mock.calls[0] != "sub_A" {
		t.Fatalf("expected exactly one cancel of sub_A after re-subscribe, got %v", mock.calls)
	}

	// Stripe redelivers the ORIGINAL sub_A checkout.
	if err := handleSubscriptionEvent(context.Background(), evtA); err != nil {
		t.Fatalf("replayed checkout A: %v", err)
	}
	if len(mock.calls) != 1 {
		t.Fatalf("replayed checkout must NOT cancel the current sub_B, canceler calls=%v", mock.calls)
	}
	if got := getLink(t, "sub_B"); got.Status != twmodel.LinkStatusActive {
		t.Fatalf("current sub_B must stay active after replay, got %q", got.Status)
	}
	if got := getLink(t, "sub_A"); got.Status != twmodel.LinkStatusCanceled {
		t.Fatalf("superseded sub_A must stay canceled after replay, got %q", got.Status)
	}
}

// --- CR-3: reactivation of an expired sub + extend-from-now ------------------

// When a renewal arrives after the sub already lapsed (EndTime in the past, Status
// "expired"), access must extend from NOW — not pastEnd+interval, which would land
// in the past and grant nothing — and the sub must flip back to "active". This is
// why calcExtendedEndTime bases on max(EndTime, now). All other renewal tests seed
// a FUTURE EndTime, so this reactivation branch is otherwise unexercised.
func TestInvoiceCycleReactivatesExpiredAndExtendsFromNow_CR3(t *testing.T) {
	setupDB(t)
	past := time.Now().Add(-240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_exp", 1, 0, past, 700, model.SubscriptionDurationMonth, 1)
	if err := model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Update("status", "expired").Error; err != nil {
		t.Fatalf("mark expired: %v", err)
	}
	pastPlusInterval := time.Unix(past, 0).AddDate(0, 1, 0).Unix()

	before := model.GetDBTimestamp()
	e := event("evt_exp", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id": "in_exp", "subscription": "sub_exp", "billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(), e); err != nil {
		t.Fatalf("handle: %v", err)
	}
	after := model.GetDBTimestamp()

	got := getSub(t, sub.Id)
	if got.Status != "active" {
		t.Fatalf("expired sub must reactivate to active on renewal, got %q", got.Status)
	}
	if got.EndTime == pastPlusInterval {
		t.Fatalf("must extend from now, not pastEnd+interval (%d)", pastPlusInterval)
	}
	lo := time.Unix(before, 0).AddDate(0, 1, 0).Unix()
	hi := time.Unix(after, 0).AddDate(0, 1, 0).Unix()
	if got.EndTime < lo || got.EndTime > hi {
		t.Fatalf("extend-from-now EndTime %d outside [%d,%d]", got.EndTime, lo, hi)
	}
	if got.AmountUsed != 0 {
		t.Fatalf("expected quota refilled, got %d", got.AmountUsed)
	}
}

// --- CR-4: annual cadence + leap-year AddDate semantics ----------------------

// Success criterion #2 (annual plan) has zero coverage otherwise. A yearly-cadence
// renewal must advance EndTime by exactly one year via AddDate(1,0,0).
func TestInvoiceCycleAnnualCadence_CR4(t *testing.T) {
	setupDB(t)
	future := time.Now().Add(240 * time.Hour).Unix()
	sub, _ := seedLinkedSub(t, "sub_yr", 1, 0, future, 500, model.SubscriptionDurationYear, 1)
	wantEnd := time.Unix(future, 0).AddDate(1, 0, 0).Unix()

	e := event("evt_yr", stripe.EventTypeInvoicePaymentSucceeded, map[string]interface{}{
		"id": "in_yr", "subscription": "sub_yr", "billing_reason": "subscription_cycle",
	})
	if err := handleSubscriptionEvent(context.Background(), e); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := getSub(t, sub.Id); got.EndTime != wantEnd {
		t.Fatalf("annual renewal must extend by one year: want %d got %d", wantEnd, got.EndTime)
	}
}

// --- CR-5: calcExtendedEndTime parity vectors (incl. month/leap normalization) -

// Pins the calendar-normalization semantics inherited from Go's AddDate — the WHY
// behind using AddDate rather than fixed-second arithmetic — and the upstream
// DurationValue<=0 guard. Times are built in the machine's local zone so both the
// input and the expected value round-trip through the implementation's
// time.Unix(base,0) consistently, regardless of test-host timezone.
func TestCalcExtendedEndTimeVectors_CR5(t *testing.T) {
	unix := func(y int, m time.Month, d int) int64 {
		return time.Date(y, m, d, 12, 0, 0, 0, time.Local).Unix()
	}
	monthPlan := &model.SubscriptionPlan{DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1}
	yearPlan := &model.SubscriptionPlan{DurationUnit: model.SubscriptionDurationYear, DurationValue: 1}
	badPlan := &model.SubscriptionPlan{DurationUnit: model.SubscriptionDurationMonth, DurationValue: 0}

	cases := []struct {
		name    string
		end     int64
		now     int64
		plan    *model.SubscriptionPlan
		want    int64
		wantErr bool
	}{
		// Jan 31 + 1 month overflows Feb → Go normalizes to Mar 3 (non-leap 2027).
		{"jan31_plus_month_normalizes", unix(2027, time.January, 31), 0, monthPlan, unix(2027, time.March, 3), false},
		// Leap day + 1 year: Feb 29 2028 has no counterpart in 2029 → Mar 1.
		{"leap_feb29_plus_year", unix(2028, time.February, 29), 0, yearPlan, unix(2029, time.March, 1), false},
		// Non-leap Feb 28 + 1 year stays Feb 28.
		{"feb28_plus_year", unix(2027, time.February, 28), 0, yearPlan, unix(2028, time.February, 28), false},
		// now > existing end → extend from now, not from the stale past end.
		{"extend_from_now", unix(2020, time.January, 1), unix(2030, time.June, 15), monthPlan, unix(2030, time.July, 15), false},
		// Non-positive duration on a non-custom plan is a misconfig (upstream parity).
		{"nonpositive_duration_errors", unix(2027, time.January, 1), 0, badPlan, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := calcExtendedEndTime(tc.end, tc.now, tc.plan)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %d)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %d got %d", tc.want, got)
			}
		})
	}
}

// --- WS-1.5: config options --------------------------------------------------

// The portal return URL must fall back to the branded gressio.ai page when unset,
// so no admin action is needed for the default happy path (O8, Option A).
func TestPortalReturnURLFallsBackToDefault(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	delete(common.OptionMap, OptionKeyPortalReturnURL)
	common.OptionMapRWMutex.Unlock()

	if got := PortalReturnURL(); got != DefaultPortalReturnURL {
		t.Fatalf("expected default %q, got %q", DefaultPortalReturnURL, got)
	}

	setOption(t, OptionKeyPortalReturnURL, "https://example.com/return")
	if got := PortalReturnURL(); got != "https://example.com/return" {
		t.Fatalf("expected override honored, got %q", got)
	}
}

// The overlay endpoint must read its OWN signing secret, never upstream's
// StripeWebhookSecret, so the two Stripe endpoints stay independently rotatable.
func TestSubscriptionWebhookSecretReadsOverlayOption(t *testing.T) {
	setOption(t, OptionKeyStripeWebhookSecret, "whsec_overlay")
	if got := SubscriptionWebhookSecret(); got != "whsec_overlay" {
		t.Fatalf("expected overlay secret, got %q", got)
	}
}
