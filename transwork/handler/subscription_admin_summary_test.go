package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

func TestAdminSubscriptionUsersSummaryReturnsPrimaryInPeriodPerUser(t *testing.T) {
	setupDB(t)

	// Plans.
	for _, p := range []*model.SubscriptionPlan{
		{Id: 1, Title: "Pro", QuotaResetPeriod: model.SubscriptionResetMonthly},
		{Id: 2, Title: "Team", QuotaResetPeriod: model.SubscriptionResetMonthly},
	} {
		if err := model.DB.Create(p).Error; err != nil {
			t.Fatalf("seed plan: %v", err)
		}
	}

	now := common.GetTimestamp()
	// User 5: single active in-period sub.
	// User 6: cancelled-but-in-period sub (must still be shown, status reported as cancelled).
	// User 7: fully-expired sub (must be excluded).
	// User 8: two in-period subs -> the most-recently-started (start_time desc) wins.
	// User 9: in-period sub referencing a since-deleted plan (id 3, never seeded)
	//         -> still shown, plan_title empty, request must not fail.
	// User 10: in-period sub with a non-positive PlanId (0, legacy/corrupt) ->
	//          still shown, plan_title empty, request must not fail (a lookup of
	//          id<=0 errors with a non-NotFound error and would fail the batch).
	for _, s := range []*model.UserSubscription{
		{UserId: 5, PlanId: 1, AmountTotal: 1000000, AmountUsed: 180000, Status: "active", StartTime: now - 100, EndTime: now + 10000, NextResetTime: now + 5000},
		{UserId: 6, PlanId: 2, AmountTotal: 5000000, AmountUsed: 800000, Status: "cancelled", StartTime: now - 100, EndTime: now + 10000, NextResetTime: 0},
		{UserId: 7, PlanId: 1, AmountTotal: 1000000, AmountUsed: 999999, Status: "expired", StartTime: now - 20000, EndTime: now - 100, NextResetTime: 0},
		{UserId: 8, PlanId: 1, AmountTotal: 1000000, AmountUsed: 0, Status: "active", StartTime: now - 500, EndTime: now + 10000, NextResetTime: now + 6000},
		{UserId: 8, PlanId: 2, AmountTotal: 2000000, AmountUsed: 10, Status: "active", StartTime: now - 50, EndTime: now + 10000, NextResetTime: now + 7000},
		{UserId: 9, PlanId: 3, AmountTotal: 1000000, AmountUsed: 0, Status: "active", StartTime: now - 100, EndTime: now + 10000, NextResetTime: now + 5000},
		{UserId: 10, PlanId: 0, AmountTotal: 1000000, AmountUsed: 0, Status: "active", StartTime: now - 100, EndTime: now + 10000, NextResetTime: now + 5000},
	} {
		if err := model.DB.Create(s).Error; err != nil {
			t.Fatalf("seed sub: %v", err)
		}
	}

	body, _ := json.Marshal(map[string][]int{"user_ids": {5, 6, 7, 8, 9, 10, 999}})
	c, w := jsonContext(http.MethodPost, "/api/subscription/admin/users/summary", body)
	AdminSubscriptionUsersSummary(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if resp["success"] != true {
		t.Fatalf("expected success true, got %v: %s", resp["success"], w.Body.String())
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %T", resp["data"])
	}

	// User 7 (expired) and 999 (no sub) must be absent.
	if _, present := data["7"]; present {
		t.Fatalf("expected expired user 7 to be omitted")
	}
	if _, present := data["999"]; present {
		t.Fatalf("expected unknown user 999 to be omitted")
	}
	// User 5: active Pro.
	u5, ok := data["5"].(map[string]any)
	if !ok {
		t.Fatalf("expected user 5 summary, got %T", data["5"])
	}
	if u5["plan_title"] != "Pro" {
		t.Fatalf("user 5 plan_title = %v, want Pro", u5["plan_title"])
	}
	if u5["status"] != "active" {
		t.Fatalf("user 5 status = %v, want active", u5["status"])
	}
	if u5["amount_total"].(float64) != 1000000 || u5["amount_used"].(float64) != 180000 {
		t.Fatalf("user 5 amounts wrong: %v / %v", u5["amount_total"], u5["amount_used"])
	}
	// User 6: cancelled-but-in-period is still shown, status = cancelled, next_reset_time = 0.
	u6 := data["6"].(map[string]any)
	if u6["status"] != "cancelled" {
		t.Fatalf("user 6 status = %v, want cancelled", u6["status"])
	}
	if u6["next_reset_time"].(float64) != 0 {
		t.Fatalf("user 6 next_reset_time = %v, want 0", u6["next_reset_time"])
	}
	// User 8: primary = most-recently-started (PlanId 2, Team).
	u8 := data["8"].(map[string]any)
	if u8["plan_title"] != "Team" {
		t.Fatalf("user 8 plan_title = %v, want Team (most recently started)", u8["plan_title"])
	}
	// User 9: sub references a deleted plan -> still present, empty title, no error.
	u9, ok := data["9"].(map[string]any)
	if !ok {
		t.Fatalf("expected user 9 summary (deleted plan must not drop the row), got %T", data["9"])
	}
	if u9["plan_title"] != "" {
		t.Fatalf("user 9 plan_title = %v, want empty (plan deleted)", u9["plan_title"])
	}
	// User 10: sub has a non-positive PlanId -> still present, empty title, no error
	// (a plan lookup of id<=0 errors, and must not fail the whole batch).
	u10, ok := data["10"].(map[string]any)
	if !ok {
		t.Fatalf("expected user 10 summary (PlanId 0 must not fail the batch), got %T", data["10"])
	}
	if u10["plan_title"] != "" {
		t.Fatalf("user 10 plan_title = %v, want empty (invalid PlanId)", u10["plan_title"])
	}
}

// A batch over maxSummaryUserIds must be rejected outright, not silently
// processed — the cap exists to bound the DB query, so exceeding it is an error.
func TestAdminSubscriptionUsersSummaryRejectsOversizedBatch(t *testing.T) {
	setupDB(t)

	ids := make([]int, maxSummaryUserIds+1)
	for i := range ids {
		ids[i] = i + 1
	}
	body, _ := json.Marshal(map[string][]int{"user_ids": ids})
	c, w := jsonContext(http.MethodPost, "/api/subscription/admin/users/summary", body)
	AdminSubscriptionUsersSummary(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 envelope, got %d", w.Code)
	}
	resp := decodeAPI(t, w)
	if resp["success"] != false {
		t.Fatalf("expected success=false for oversized batch (%d ids), got %v: %s",
			len(ids), resp["success"], w.Body.String())
	}
}
