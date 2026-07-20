package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/transwork/credits"
	twmodel "github.com/QuantumNous/new-api/transwork/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// errFreeCreditAlreadyGranted is a sentinel returned from the claim transaction
// when the user has already received or spent quota, letting the handler map it
// to 400 (already granted) instead of 500 (real DB error).
var errFreeCreditAlreadyGranted = errors.New("free credit already granted")

// inviteOptionalKey is the admin option (Payment settings) that makes the
// desktop invite code optional. When "true", a brand-new user may skip the
// invite code and claim a one-time free starter credit instead. Any non-"true"
// value (including missing) is fail-closed false, preserving the invite-required
// behavior. Default is seeded "false" (see transwork/init.go).
const inviteOptionalKey = "InviteCodeOptional"

// freeCreditAmountKey is the admin option holding the one-time free starter
// credit (in Gressio credits, matching the rest of the UI) granted to a new user
// who continues without an invite code. Read live at claim time so a later admin
// change applies to every subsequent new registration. Default is seeded "100"
// (see transwork/init.go).
const freeCreditAmountKey = "FreeCreditForNewUser"

// defaultFreeCreditCredits is the fallback grant (in Gressio credits) when the
// option is unset or unparseable.
const defaultFreeCreditCredits = 100

// InviteOptional reports whether the desktop invite code is optional (admin
// Payment setting). Fail-closed: any non-"true" value is false.
func InviteOptional() bool {
	common.OptionMapRWMutex.RLock()
	enabled := common.OptionMap[inviteOptionalKey] == "true"
	common.OptionMapRWMutex.RUnlock()
	return enabled
}

// freeCreditAmount returns the one-time free grant in raw quota units. The admin
// option FreeCreditForNewUser is configured in Gressio credits (matching the
// rest of the UI); this converts it to quota via credits.ToQuota so the grant
// lands as the intended number of displayed credits. Falls back to
// defaultFreeCreditCredits when the option is unset or unparseable.
func freeCreditAmount() int {
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[freeCreditAmountKey]
	common.OptionMapRWMutex.RUnlock()
	c := defaultFreeCreditCredits
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		c = n
	}
	return credits.ToQuota(float64(c))
}

// ClaimFreeCredit grants the one-time free starter credit to the authenticated
// user when the invite code is optional. Guarded so only a fresh user (0 quota
// AND 0 used) can claim, which prevents repeat farming. Must sit behind
// middleware.TokenAuth().
func ClaimFreeCredit(c *gin.Context) {
	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user in context"})
		return
	}

	if !InviteOptional() {
		c.JSON(http.StatusForbidden, gin.H{"error": "invite code is required"})
		return
	}

	amount := freeCreditAmount()

	// Guard against concurrent double-claims. Two simultaneous requests would
	// both read quota==0 && used==0 and both grant, farming multiple starter
	// credits. Lock the user row FOR UPDATE, re-check the guards, and increment
	// inside a single transaction so the second request observes the first grant.
	// Matches the codebase's cross-DB lock idiom (model/user.go).
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Set("gorm:query_option", "FOR UPDATE").
			Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		// Durable once-only guard: a FreeCreditClaim row means this user has
		// already received the free credit at some point, independent of their
		// current quota/usage (which can be reset or spent down). The row's
		// UserId primary key also makes a concurrent double-claim impossible even
		// if two requests slipped past the row lock.
		var claim twmodel.FreeCreditClaim
		claimErr := tx.Where("user_id = ?", userID).First(&claim).Error
		if claimErr == nil {
			return errFreeCreditAlreadyGranted
		}
		if !errors.Is(claimErr, gorm.ErrRecordNotFound) {
			return claimErr
		}
		// Fresh-user gate: only a user who has neither received nor spent any
		// quota qualifies for the starter credit.
		if user.Quota != 0 || user.UsedQuota != 0 {
			return errFreeCreditAlreadyGranted
		}
		if err := tx.Create(&twmodel.FreeCreditClaim{UserId: userID, Amount: amount}).Error; err != nil {
			return err
		}
		return tx.Model(&model.User{}).Where("id = ?", userID).
			Update("quota", gorm.Expr("quota + ?", amount)).Error
	})
	if errors.Is(err, errFreeCreditAlreadyGranted) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "free credit already granted"})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// The quota changed directly in the DB (bypassing IncreaseUserQuota so the
	// grant stays inside the locked transaction), so the cached user is now
	// stale — drop it so the next read reloads. Mirrors upstream's post-mutation
	// invalidation for user state changes.
	if err := model.InvalidateUserCache(userID); err != nil {
		logger.LogError(c, "failed to invalidate user cache after free credit grant: "+err.Error())
	}

	model.RecordLog(userID, model.LogTypeSystem, fmt.Sprintf("新用户免邀请码赠送 %s", logger.LogQuota(amount)))

	c.JSON(http.StatusOK, gin.H{"quotaAdded": amount})
}
