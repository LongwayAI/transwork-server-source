package handler

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initTestDB opens an in-memory SQLite database, migrates the tables the
// realtime ASR settle path touches, and primes the cross-DB column-name
// variables in the model package via initCol (which runs as a deferred side
// effect of chooseDB in production but is unreachable from outside model/ in
// tests). We trigger it indirectly by routing through model.InitDB with a
// per-test SQLite path.
func initTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prevSqlitePath := common.SQLitePath
	common.SQLitePath = filepath.Join(dir, "test.db")
	prevRedis := common.RedisEnabled
	prevBatch := common.BatchUpdateEnabled
	prevSQLite := common.UsingSQLite
	prevMaster := common.IsMasterNode
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	// migrateDB only runs on the master node; tests need it to run.
	common.IsMasterNode = true

	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	// model.RecordConsumeLog writes to LOG_DB; in production InitLogDB
	// either opens a separate DSN or points it at DB. For tests we share DB.
	model.LOG_DB = model.DB
	t.Cleanup(func() {
		if model.DB != nil {
			_ = model.CloseDB()
			model.DB = nil
			model.LOG_DB = nil
		}
		common.SQLitePath = prevSqlitePath
		common.RedisEnabled = prevRedis
		common.BatchUpdateEnabled = prevBatch
		common.UsingSQLite = prevSQLite
		common.IsMasterNode = prevMaster
		_ = os.Unsetenv("SQL_DSN")
	})
}

func TestClampReportedDuration_NegativeBecomesZero(t *testing.T) {
	// reserve floor stands: -5s should produce 0s of additional work.
	got := clampReportedDuration(-5, 100, 200)
	assert.Equal(t, 0.0, got)
}

func TestClampReportedDuration_HardCapAtSixHours(t *testing.T) {
	// Wall-clock is huge; the 6h hard cap dominates.
	createdAt := int64(0)
	now := createdAt + 24*60*60 // 24h since mint
	got := clampReportedDuration(30000, createdAt, now)
	assert.Equal(t, float64(asrRealtimeMaxSeconds), got, "must clamp to 6h hard cap")
}

func TestClampReportedDuration_ClampedByWallClock(t *testing.T) {
	// Wall-clock dominates: only 5s have elapsed since mint, so even a
	// 1000s report can't bill more than 5s.
	createdAt := int64(1000)
	now := createdAt + 5
	got := clampReportedDuration(1000, createdAt, now)
	assert.Equal(t, 5.0, got)
}

func TestClampReportedDuration_PassesThroughValidValue(t *testing.T) {
	// 60s reported, 120s of wall-clock available, below the 6h cap.
	got := clampReportedDuration(60, 0, 120)
	assert.Equal(t, 60.0, got)
}

// TestCreateElevenLabsTempToken_ReservesQuotaAndPersistsRow verifies the
// reserve→mint→persist sequence:
//  1. user quota drops by the reserve amount (ratio × 500 virtual tokens);
//  2. AsrReservation row is created with the matching reserved_quota;
//  3. response includes a non-zero reservation_id and reserve_seconds=30.
func TestCreateElevenLabsTempToken_ReservesQuotaAndPersistsRow(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	prevAPIFn := callElevenLabsTokenAPIFn
	t.Cleanup(func() { callElevenLabsTokenAPIFn = prevAPIFn })
	callElevenLabsTokenAPIFn = func(apiKey string) (string, error) {
		return "fake-temp-token", nil
	}

	const userID = 4011
	const tokenID = 5011
	const initialUserQuota = 1_000_000
	const initialTokenQuota = 500_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-1", initialUserQuota, initialTokenQuota, "default", realtimeAsrModel)

	w := postAuthed(t, "/api/elevenlabs/token", userID, tokenID, "tk-real-1", `{"model_name":"`+realtimeAsrModel+`"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp ElevenLabsTokenResponse
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.Equal(t, "fake-temp-token", resp.Token)
	assert.Equal(t, asrRealtimeReserveSeconds, resp.ReserveSeconds)
	assert.NotZero(t, resp.ReservationId, "response must include reservation_id")

	// The expected reserve: 30s × ratio 3 / 60 × 1000 virtual tokens × group ratio 1
	// = ceil(30/60*1000) * 3 = 500 * 3 = 1500.
	const expectedReserve = 1500
	gotUser := getUserQuota(t, userID)
	assert.Equal(t, initialUserQuota-expectedReserve, gotUser, "user quota should drop by reserve")

	res, err := model.GetAsrReservationById(resp.ReservationId, userID)
	require.NoError(t, err)
	require.NotNil(t, res, "AsrReservation row must exist after successful mint")
	assert.Equal(t, expectedReserve, res.ReservedQuota)
	assert.Equal(t, asrRealtimeReserveSeconds, res.ReservedSeconds)
	assert.Equal(t, model.AsrReservationStatusReserved, res.Status)
	assert.Equal(t, "tk-real-1", res.TokenKey, "TokenKey must be persisted for settle-time use")
}

// TestCreateElevenLabsTempToken_RejectsWhenInsufficient verifies that when a
// user has zero quota, the mint fails with 403, no AsrReservation row is
// created, and the (mocked) ElevenLabs API is never called.
func TestCreateElevenLabsTempToken_RejectsWhenInsufficient(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	prevAPIFn := callElevenLabsTokenAPIFn
	t.Cleanup(func() { callElevenLabsTokenAPIFn = prevAPIFn })
	called := false
	callElevenLabsTokenAPIFn = func(apiKey string) (string, error) {
		called = true
		return "should-not-be-returned", nil
	}

	const userID = 4012
	const tokenID = 5012
	// Zero user quota — billing must reject up-front.
	seedUserTokenChannel(t, userID, tokenID, "tk-real-2", 0, 100_000, "default", realtimeAsrModel)

	w := postAuthed(t, "/api/elevenlabs/token", userID, tokenID, "tk-real-2", `{"model_name":"`+realtimeAsrModel+`"}`)
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.False(t, called, "ElevenLabs token API must NOT be called when reserve fails")

	// No reservation row created.
	var count int64
	require.NoError(t, model.DB.Model(&model.AsrReservation{}).
		Where("user_id = ?", userID).Count(&count).Error)
	assert.Equal(t, int64(0), count, "no AsrReservation row on insufficient quota")
}

// TestCreateElevenLabsTempToken_RefundsOnElevenLabsError verifies that when
// the reserve succeeds but the ElevenLabs mint fails, the user quota is
// refunded (via Billing.Refund) and no AsrReservation row is left behind.
func TestCreateElevenLabsTempToken_RefundsOnElevenLabsError(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	prevAPIFn := callElevenLabsTokenAPIFn
	t.Cleanup(func() { callElevenLabsTokenAPIFn = prevAPIFn })
	callElevenLabsTokenAPIFn = func(apiKey string) (string, error) {
		return "", errors.New("simulated upstream failure")
	}

	const userID = 4013
	const tokenID = 5013
	const initialUserQuota = 1_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-3", initialUserQuota, 500_000, "default", realtimeAsrModel)

	w := postAuthed(t, "/api/elevenlabs/token", userID, tokenID, "tk-real-3", `{"model_name":"`+realtimeAsrModel+`"}`)
	assert.Equal(t, http.StatusBadGateway, w.Code, "body=%s", w.Body.String())

	// Billing.Refund is async (gopool.Go). Poll briefly for the refund.
	require.Eventually(t, func() bool {
		return getUserQuota(t, userID) == initialUserQuota
	}, 2*time.Second, 20*time.Millisecond, "user quota must be refunded after mint failure")

	var count int64
	require.NoError(t, model.DB.Model(&model.AsrReservation{}).
		Where("user_id = ?", userID).Count(&count).Error)
	assert.Equal(t, int64(0), count, "no AsrReservation row on mint failure")
}

// TestReportElevenLabsUsage_SettlesAdditive verifies that reporting a longer
// duration than the reserve (60s vs 30s) charges the user the additional
// delta (an extra 500 virtual tokens worth of quota).
func TestReportElevenLabsUsage_SettlesAdditive(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4021
	const tokenID = 5021
	const initialUserQuota = 1_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-21", initialUserQuota, 500_000, "default", realtimeAsrModel)

	const reservedQuota = 1500 // 30s reserve at ratio 3
	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-21",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   reservedQuota,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120, // 2 minutes ago — well over 60s
	}
	require.NoError(t, model.CreateAsrReservation(res))

	// Apply the reserve to the user wallet up-front (CreateAsrReservation does
	// not, since in production PreConsumeBilling has already done so).
	require.NoError(t, model.DecreaseUserQuota(userID, reservedQuota, true))

	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-21",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":60}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp ElevenLabsUsageResponse
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &resp))
	// 60s @ ratio 3 → ceil(60/60*1000) × 3 × 1 = 3000 quota total
	const expectedActual = 3000
	assert.Equal(t, expectedActual, resp.ActualQuota)
	assert.Equal(t, expectedActual-reservedQuota, resp.DeltaApplied)

	gotUser := getUserQuota(t, userID)
	assert.Equal(t, initialUserQuota-expectedActual, gotUser, "user quota must reflect additive settle")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettled, got.Status)
	assert.Equal(t, expectedActual, got.SettledQuota)
}

// TestReportElevenLabsUsage_RefundsUnderReserve verifies that reporting less
// than the reserve duration refunds the user for the difference.
func TestReportElevenLabsUsage_RefundsUnderReserve(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4022
	const tokenID = 5022
	const initialUserQuota = 1_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-22", initialUserQuota, 500_000, "default", realtimeAsrModel)

	const reservedQuota = 1500
	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-22",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   reservedQuota,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, reservedQuota, true))

	// Report 10s — under the 30s reserve.
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-22",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":10}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	// 10s → ceil(10/60*1000) = 167 virtual tokens × ratio 3 = 501 quota.
	const expectedActual = 501
	gotUser := getUserQuota(t, userID)
	assert.Equal(t, initialUserQuota-expectedActual, gotUser, "user quota must reflect refund")
}

func TestReportElevenLabsUsage_ClampedAtSixHours(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4023
	const tokenID = 5023
	const initialUserQuota = 100_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-23", initialUserQuota, 100_000_000, "default", realtimeAsrModel)

	const reservedQuota = 1500
	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-23",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   reservedQuota,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 24*60*60, // a day ago
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, reservedQuota, true))

	// Report 30,000s (well over the 6h hard cap).
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-23",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":30000}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	// 6h = 21600s → ceil(21600/60*1000) = 360_000 virtual tokens × ratio 3 = 1_080_000.
	const expectedActual = 1_080_000
	gotUser := getUserQuota(t, userID)
	assert.Equal(t, initialUserQuota-expectedActual, gotUser, "report must be clamped to 6h")
}

func TestReportElevenLabsUsage_RejectsAlreadySettled(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4024
	const tokenID = 5024
	seedUserTokenChannel(t, userID, tokenID, "tk-real-24", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-24",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
		Status:          model.AsrReservationStatusSettled,
	}
	require.NoError(t, model.CreateAsrReservation(res))

	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-24",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":60}`)
	assert.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
}

func TestReportElevenLabsUsage_RejectsOtherUserReservation(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	// Owner creates the reservation.
	seedUserTokenChannel(t, 4025, 5025, "tk-real-25", 1_000_000, 500_000, "default", realtimeAsrModel)
	res := &model.AsrReservation{
		UserId:          4025,
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))

	// Different user authenticated.
	seedUserTokenChannel(t, 4026, 5026, "tk-real-26", 1_000_000, 500_000, "default", realtimeAsrModel)
	w := postAuthed(t, "/api/elevenlabs/usage", 4026, 5026, "tk-real-26",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":60}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func seedRealtimeAsrPricing(t *testing.T) {
	t.Helper()
	// Hydrate the ratio maps from the in-source defaults (scribe_v2_realtime
	// is in defaultModelRatio at ratio 3, registered in Task 1).
	ratio_setting.InitRatioSettings()
}

func seedUserTokenChannel(t *testing.T, userID, tokenID int, tokenKey string, userQuota, tokenQuota int, group, modelName string) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.User{
		Id:       userID,
		Username: fmt.Sprintf("user_%d", userID),
		Status:   common.UserStatusEnabled,
		Quota:    userQuota,
		Group:    group,
		AffCode:  fmt.Sprintf("aff-%d", userID),
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         tokenKey,
		Status:      1,
		Name:        fmt.Sprintf("token_%d", tokenID),
		RemainQuota: tokenQuota,
		Group:       group,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:     9000 + tokenID,
		Name:   "elevenlabs-test",
		Type:   1,
		Key:    "fake-channel-key",
		Status: 1,
		Models: modelName,
		Group:  group,
	}).Error)
	priority := int64(0)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     group,
		Model:     modelName,
		ChannelId: 9000 + tokenID,
		Enabled:   true,
		Priority:  &priority,
	}).Error)
}

func getUserQuota(t *testing.T, userID int) int {
	t.Helper()
	var q int
	require.NoError(t, model.DB.Model(&model.User{}).
		Where("id = ?", userID).Select("quota").Find(&q).Error)
	return q
}

// postAuthed simulates middleware.TokenAuth's context fields and routes the
// request through a fresh gin engine wired only to the elevenlabs handlers.
func postAuthed(t *testing.T, path string, userID, tokenID int, tokenKey string, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authStub := func(c *gin.Context) {
		c.Set("id", userID)
		c.Set("token_id", tokenID)
		c.Set("token_quota", 500_000)
		c.Set("token_unlimited_quota", false)
		c.Set(string(constant.ContextKeyTokenKey), tokenKey)
		c.Set(string(constant.ContextKeyUsingGroup), "default")
		c.Set(string(constant.ContextKeyTokenGroup), "default")
		c.Set(string(constant.ContextKeyUserId), userID)
		c.Set(string(constant.ContextKeyUserGroup), "default")
		c.Next()
	}
	r.POST("/api/elevenlabs/token", authStub, CreateElevenLabsTempToken)
	r.POST("/api/elevenlabs/usage", authStub, ReportElevenLabsUsage)

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(jsonBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

