package handler

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// maxSummaryUserIds caps the per-request batch size so a huge user_ids list
// cannot blow past SQL IN-clause parameter limits or hammer the DB. The admin
// User Management table pages well under this.
const maxSummaryUserIds = 500

type adminSubscriptionUsersSummaryRequest struct {
	UserIds []int `json:"user_ids"`
}

// adminUserSubscriptionSummary is the per-user row returned to the admin table.
type adminUserSubscriptionSummary struct {
	PlanTitle     string `json:"plan_title"`
	AmountTotal   int64  `json:"amount_total"`
	AmountUsed    int64  `json:"amount_used"`
	Status        string `json:"status"`
	NextResetTime int64  `json:"next_reset_time"`
}

// AdminSubscriptionUsersSummary returns, keyed by user id, each user's primary
// in-period subscription. Users with no in-period subscription are omitted.
// POST /api/subscription/admin/users/summary  body: {"user_ids":[...]}
func AdminSubscriptionUsersSummary(c *gin.Context) {
	var req adminSubscriptionUsersSummaryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数错误")
		return
	}
	if len(req.UserIds) > maxSummaryUserIds {
		common.ApiErrorMsg(c, "单次查询用户数量不能超过 500")
		return
	}

	subs, err := model.GetPrimaryInPeriodSubscriptionsByUserIds(req.UserIds)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	// Resolve plan titles via the hybrid plan cache (GetSubscriptionPlanById),
	// which is invalidated on plan create/update, so a batch usually avoids the
	// DB entirely. Dedup first so each unique plan is looked up once.
	planIds := make([]int, 0, len(subs))
	seen := make(map[int]bool)
	for _, s := range subs {
		// Skip non-positive PlanIds (legacy/corrupt rows): GetSubscriptionPlanById
		// rejects id <= 0 with a non-NotFound error, which would otherwise fail the
		// whole batch. Such rows just get an empty title, as the prior IN-query did.
		if s.PlanId > 0 && !seen[s.PlanId] {
			seen[s.PlanId] = true
			planIds = append(planIds, s.PlanId)
		}
	}
	titles := make(map[int]string)
	for _, planId := range planIds {
		plan, err := model.GetSubscriptionPlanById(planId)
		if err != nil {
			// A subscription may reference a since-deleted plan; leave its title
			// empty (as the prior IN-query did by simply omitting it) instead of
			// failing the whole request. Propagate only real DB errors.
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			common.ApiError(c, err)
			return
		}
		titles[planId] = plan.Title
	}

	// map[int] keys marshal to JSON string keys ("5"), which the frontend reads by row id.
	result := make(map[int]adminUserSubscriptionSummary, len(subs))
	for userId, s := range subs {
		result[userId] = adminUserSubscriptionSummary{
			PlanTitle:     titles[s.PlanId],
			AmountTotal:   s.AmountTotal,
			AmountUsed:    s.AmountUsed,
			Status:        s.Status,
			NextResetTime: s.NextResetTime,
		}
	}
	common.ApiSuccess(c, result)
}
