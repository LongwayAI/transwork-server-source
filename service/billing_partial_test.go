package service

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// swapDBWithTables points model.DB (and LOG_DB) at a throwaway in-memory
// database containing only the listed tables, restoring the package-wide one
// from TestMain afterwards.
//
// Omitting a table is the only way to make a settlement step fail on purpose.
// Both halves of PostConsumeQuota are single-statement GORM updates, and an
// UPDATE that matches no rows is a success on all three supported databases —
// so a fixture of missing *rows* produces no error at all and would let these
// tests pass against an implementation that never reports failure. A missing
// *table* is a real driver error raised at exactly the step under test, which
// is what distinguishes "the funding step failed" from "the token step failed".
func swapDBWithTables(t *testing.T, tables ...interface{}) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	if len(tables) > 0 {
		require.NoError(t, db.AutoMigrate(tables...))
	}

	prevDB, prevLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB = prevDB, prevLogDB
		_ = sqlDB.Close()
	})
}

func readUserQuota(t *testing.T, userId int) int {
	t.Helper()
	var q int
	require.NoError(t, model.DB.Model(&model.User{}).
		Where("id = ?", userId).Select("quota").Find(&q).Error)
	return q
}

// TestPostConsumeQuota_TokenStepFailureIsMarkedPartial pins the *reason* the
// sentinel exists: when the wallet has already been debited and only the
// token-quota counter fails, the caller must be able to tell that money moved.
// A caller that mistakes this for "nothing happened" and retries charges the
// user twice, so the assertion below is deliberately two-sided — the error
// carries the marker AND the wallet really was debited, which is what makes the
// marker a true statement rather than a label.
func TestPostConsumeQuota_TokenStepFailureIsMarkedPartial(t *testing.T) {
	// `users` present, `tokens` absent: the wallet step commits, the token step
	// that follows it fails.
	swapDBWithTables(t, &model.User{})

	const userId = 7101
	const startQuota = 10_000
	require.NoError(t, model.DB.Create(&model.User{
		Id: userId, Username: "partial_user", Quota: startQuota,
		Status: common.UserStatusEnabled, AffCode: "aff-7101",
	}).Error)

	info := &relaycommon.RelayInfo{UserId: userId, TokenId: 7201, TokenKey: "tk-7201"}
	err := PostConsumeQuota(info, 250, 0, false)

	require.Error(t, err, "a failing token-quota step must not be reported as success")
	assert.True(t, errors.Is(err, ErrQuotaPartiallyApplied),
		"funding was already committed, so the error must be distinguishable from a clean failure; got %v", err)
	assert.Equal(t, startQuota-250, readUserQuota(t, userId),
		"the marker must describe reality: the wallet debit did land")
}

// TestPostConsumeQuota_FundingStepFailureIsNotMarkedPartial is the other half of
// the contract, and the one that stops the sentinel from degrading into "an
// error occurred". If the funding step itself fails nothing was applied, the
// caller is free to retry, and marking it partial would strand a perfectly
// recoverable settlement forever.
func TestPostConsumeQuota_FundingStepFailureIsNotMarkedPartial(t *testing.T) {
	// `tokens` present, `users` absent: the wallet step fails first and the
	// token step is never reached.
	swapDBWithTables(t, &model.Token{})

	const tokenId = 7202
	require.NoError(t, model.DB.Create(&model.Token{
		Id: tokenId, UserId: 7102, Key: "tk-7202", Name: "t", Status: 1, RemainQuota: 10_000,
	}).Error)

	info := &relaycommon.RelayInfo{UserId: 7102, TokenId: tokenId, TokenKey: "tk-7202"}
	err := PostConsumeQuota(info, 250, 0, false)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrQuotaPartiallyApplied),
		"nothing was applied, so this must stay retryable; got %v", err)

	var remain int
	require.NoError(t, model.DB.Model(&model.Token{}).
		Where("id = ?", tokenId).Select("remain_quota").Find(&remain).Error)
	assert.Equal(t, 10_000, remain, "token quota must be untouched when funding failed first")
}

// TestPostAudioConsumeQuota_ReturnsSettleFailure covers the defect itself:
// PostAudioConsumeQuota used to log a failing SettleBilling and return nothing,
// so every caller — including the ASR settle handler that then marks its
// reservation terminal — was told the charge had landed.
//
// Neither `users` nor `tokens` exists here, so the settle fails at its first
// step and no quota moves anywhere; only the return value distinguishes that
// from a successful settle.
func TestPostAudioConsumeQuota_ReturnsSettleFailure(t *testing.T) {
	swapDBWithTables(t, &model.Log{}, &model.Channel{})

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		UserId:          7103,
		TokenId:         7203,
		TokenKey:        "tk-7203",
		OriginModelName: "scribe_v2_realtime",
		UsingGroup:      "default",
		// Billing nil routes SettleBilling down its legacy fallback, which is the
		// path the realtime ASR settle actually takes across the mint→settle gap.
		Billing: nil,
		// A non-zero pre-consume with zero actual usage yields a non-zero (here,
		// refund) delta, so the settle has real work to do and cannot succeed
		// trivially by short-circuiting on delta == 0.
		FinalPreConsumedQuota: 500,
		StartTime:             time.Now(),
		ChannelMeta:           &relaycommon.ChannelMeta{ChannelId: 0},
		PriceData:             types.PriceData{ModelRatio: 1, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
	}
	usage := &dto.Usage{}

	err := PostAudioConsumeQuota(c, info, usage, "ASR realtime 0.00s")
	require.Error(t, err, "a failed settlement must be reported to the caller, not only logged")
}
