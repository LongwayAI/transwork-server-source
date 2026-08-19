package handler

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
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
	// RedisEnabled, BatchUpdateEnabled, IsMasterNode and UsingSQLite are set
	// once in TestMain and deliberately not touched here: background goroutines
	// spawned by earlier tests are still reading them. See TestMain.

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

// stubPostAudioConsumeQuota replaces the settlement call for the duration of a
// test and reports how many times the handler reached it.
func stubPostAudioConsumeQuota(t *testing.T, err error) *int {
	t.Helper()
	prev := postAudioConsumeQuotaFn
	t.Cleanup(func() { postAudioConsumeQuotaFn = prev })
	calls := 0
	postAudioConsumeQuotaFn = func(*gin.Context, *relaycommon.RelayInfo, *dto.Usage, string) error {
		calls++
		return err
	}
	return &calls
}

// TestReportElevenLabsUsage_TransientBillingFailureLeavesReservationRetryable is
// the defect: a settlement that failed used to be indistinguishable from one
// that succeeded, so the handler carried on and wrote the terminal "settled"
// status over a row whose quota delta had never been applied. The client got
// 200, the money was never charged, and the row could never be retried by
// anything afterwards.
//
// "Retryable" is asserted by actually retrying rather than by inspecting the
// status string: the second call goes through with the settlement healed and
// must produce a real, fully-settled 200. A status assertion alone would still
// pass if some later change made a "reserved" row unreachable for other
// reasons; driving the retry to completion cannot.
func TestReportElevenLabsUsage_TransientBillingFailureLeavesReservationRetryable(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4027
	const tokenID = 5027
	const initialUserQuota = 1_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-27", initialUserQuota, 500_000, "default", realtimeAsrModel)

	const reservedQuota = 1500
	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-27",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   reservedQuota,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, reservedQuota, true))

	// A transient infrastructure failure — no funding source was touched.
	failing := stubPostAudioConsumeQuota(t, errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))

	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":60}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-27", body)

	require.Equal(t, 1, *failing)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"a settlement that did not apply must not be answered with success; body=%s", w.Body.String())
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())

	var errResp struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &errResp))
	assert.True(t, errResp.Retryable, "nothing was applied, so the client must be told to retry")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusReserved, got.Status,
		"a row that was never billed must not be marked terminal")
	assert.Zero(t, got.SettledQuota, "settled_quota must not record a charge that never landed")
	assert.Zero(t, got.SettledAt)
	assert.Equal(t, initialUserQuota-reservedQuota, getUserQuota(t, userID),
		"only the original reserve should be held; the failed settle must not have moved anything")

	// Now the retry, with settlement working again. This is the property the
	// issue names: the row is still recoverable.
	healed := stubPostAudioConsumeQuota(t, nil)
	w2 := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-27", body)
	require.Equal(t, 1, *healed)
	require.Equal(t, http.StatusOK, w2.Code, "the retry must succeed; body=%s", w2.Body.String())

	settled, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, model.AsrReservationStatusSettled, settled.Status)
	assert.Equal(t, 3000, settled.SettledQuota, "60s @ ratio 3 = 3000")
}

// stubRedisEnabled points the handler's cache-liveness seam at a fixed value.
// It deliberately does not assign to common.RedisEnabled: that flag is read by
// fire-and-forget goroutines which outlive the test, so writing it races them.
func stubRedisEnabled(t *testing.T, enabled bool) {
	t.Helper()
	prev := redisEnabledFn
	t.Cleanup(func() { redisEnabledFn = prev })
	redisEnabledFn = func() bool { return enabled }
}

// TestReportElevenLabsUsage_BillingFailureRetryablePromiseTracksRedis covers
// the gap between "the database did not move" and "nothing moved".
//
// Increase/DecreaseUserQuota hand the Redis delta to a goroutine and swallow
// its error *before* attempting the synchronous database write, so a settle
// that fails at the database has still moved the cached balance. A retry then
// applies the cache delta twice against one database delta — and in the refund
// direction (a report under the 30s reserve) that double-credits the cache,
// which is what serves quota checks, so it permits spending the user does not
// have until the key expires.
//
// Deleting the key is only a best-effort repair and cannot be relied on:
// RedisHIncrBy reads the TTL and execs its HINCRBY in two separate round trips
// with no WATCH, so an in-flight goroutine can recreate the key after the
// delete. The flag is therefore asserted to track exactly the condition under
// which it is provably true — Redis off, where the cache helpers no-op — and
// not to track "we tried to clean up".
func TestReportElevenLabsUsage_BillingFailureRetryablePromiseTracksRedis(t *testing.T) {
	for _, tc := range []struct {
		name          string
		redisEnabled  bool
		wantRetryable bool
		wantStatus    string
	}{
		{"redis off: nothing outside the database could have moved", false, true, model.AsrReservationStatusReserved},
		{"redis on: the cached balance may carry a phantom delta", true, false, model.AsrReservationStatusSettling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initTestDB(t)
			seedRealtimeAsrPricing(t)

			stubRedisEnabled(t, tc.redisEnabled)

			const userID = 4030
			const tokenID = 5030
			seedUserTokenChannel(t, userID, tokenID, "tk-real-30", 1_000_000, 500_000, "default", realtimeAsrModel)

			res := &model.AsrReservation{
				UserId:          userID,
				TokenId:         tokenID,
				TokenKey:        "tk-real-30",
				ModelName:       realtimeAsrModel,
				UsingGroup:      "default",
				ReservedQuota:   1500,
				ReservedSeconds: 30,
				CreatedAt:       common.GetTimestamp() - 120,
			}
			require.NoError(t, model.CreateAsrReservation(res))

			stubPostAudioConsumeQuota(t, errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))
			invalidated := stubInvalidateUserCache(t, nil)

			// A duration under the 30s reserve, so the settle would have been a
			// refund — the direction in which a doubled cache delta hands out
			// balance the user never paid for.
			w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-30",
				`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":10}`)
			require.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())

			// The repair is attempted either way; it is just not sufficient to
			// justify the promise.
			assert.Equal(t, []int{userID}, *invalidated,
				"the cached balance must still be dropped — best-effort repair is worth doing even when it cannot be guaranteed")

			var errResp struct {
				Retryable bool `json:"retryable"`
			}
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &errResp))
			assert.Equal(t, tc.wantRetryable, errResp.Retryable,
				"retryable must track whether the cache provably did not move, not whether cleanup was attempted")

			// The flag and the row must agree. Where the premise provably holds
			// the claim is handed back so the client can genuinely retry; where
			// it does not, the row keeps the claim rather than merely advising
			// against a retry it would still admit.
			got, err := model.GetAsrReservationById(res.Id, userID)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantStatus, got.Status,
				"the status must enforce the same answer the retryable flag advertises")
		})
	}
}

// TestReportElevenLabsUsage_BillingFailureLogsUnclearableCache keeps the
// invalidation failure greppable. With Redis off the retry premise would
// otherwise hold, so a failed invalidation is exactly what withdraws it: the
// cache may still carry a delta the database never took, and one that could not
// even be cleared is the state most likely to need a human.
func TestReportElevenLabsUsage_BillingFailureLogsUnclearableCache(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4031
	const tokenID = 5031
	seedUserTokenChannel(t, userID, tokenID, "tk-real-31", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-31",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))

	stubPostAudioConsumeQuota(t, errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))
	invalidated := stubInvalidateUserCache(t, errors.New("redis: connection pool exhausted"))

	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-31",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":10}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, []int{userID}, *invalidated, "invalidation must be attempted before answering")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettling, got.Status,
		"a cache that could not be cleared leaves the retry premise unrestorable, so the claim is kept")
}

// TestReportElevenLabsUsage_PartialBillingFailureIsTerminalNotRetryable is the
// asymmetric half. When the funding source has already taken the delta and only
// the token counter failed, "retryable" would be the wrong answer: the client
// could simply re-POST the same reservation id and be charged a second time,
// because the handler's only idempotency guard is the row's status.
//
// So this branch keeps the terminal transition — and the re-POST below is the
// point of the test, not the status assertion above it. It must be refused.
func TestReportElevenLabsUsage_PartialBillingFailureIsTerminalNotRetryable(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4028
	const tokenID = 5028
	seedUserTokenChannel(t, userID, tokenID, "tk-real-28", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-28",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, 1500, true))

	partial := fmt.Errorf("%w: %v", service.ErrQuotaPartiallyApplied,
		errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))
	calls := stubPostAudioConsumeQuota(t, partial)

	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":60}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-28", body)

	require.Equal(t, 1, *calls)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())

	var errResp struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &errResp))
	assert.False(t, errResp.Retryable,
		"funding already moved, so the client must not be invited to retry")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	// Unclaimable, which is what makes it safe — but recorded under its own
	// status rather than as a settlement, so the TES2-22 monitor counts it as
	// Unknown instead of as a healthy settle and the drift is visible as a
	// number rather than only as a log line.
	assert.Equal(t, model.AsrReservationStatusTokenDesync, got.Status,
		"a half-applied settle must be terminal, or a re-POST double-charges the funding source")
	// The figures the funding side actually applied are kept on the row, so the
	// token-counter repair does not have to be reconstructed from logs.
	assert.Equal(t, 3000, got.SettledQuota, "the applied funding delta must be recorded for reconciliation")
	assert.Equal(t, 60, got.SettledSeconds)

	// The reason the row is terminal: a second report must be refused outright
	// and must never reach the settlement call again.
	again := stubPostAudioConsumeQuota(t, partial)
	w2 := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-28", body)
	assert.Equal(t, http.StatusConflict, w2.Code, "body=%s", w2.Body.String())
	assert.Zero(t, *again, "the re-POST must be rejected before any second charge is attempted")
}

// stubInvalidateUserCache replaces the cache-invalidation seam, counting the
// user ids it was called with and returning err. Needed because the handler
// suite runs without Redis, where model.InvalidateUserCache returns nil without
// doing anything — so neither the call nor its failure is observable otherwise.
func stubInvalidateUserCache(t *testing.T, err error) *[]int {
	t.Helper()
	prev := invalidateUserCacheFn
	t.Cleanup(func() { invalidateUserCacheFn = prev })
	var calls []int
	invalidateUserCacheFn = func(userId int) error {
		calls = append(calls, userId)
		return err
	}
	return &calls
}

// TestReportElevenLabsUsage_ReleaseDropsTheCachedBalance pins the premise the
// release depends on. "Nothing was applied" is true of the database and false
// of the cache: Increase/DecreaseUserQuota hand the Redis delta to a goroutine
// and swallow its error BEFORE the synchronous database write, so a settle that
// failed at the database has still moved the cached balance.
//
// Handing the row back without dropping that key lets the retry apply the cache
// delta twice against one database delta. In the refund direction — this
// endpoint's common case, since a report under the 30s reserve refunds — that
// double-credits a balance the cache is what serves quota checks from, so it
// permits spending the user never paid for.
//
// Scope: this is the Redis-OFF case, which is the only configuration where the
// premise is provable — the cache helpers no-op outright, so the failed
// database write is the only thing that could have moved anything and it did
// not. That is why the row is released and the retry advertised here.
// TestReportElevenLabsUsage_RedisEnabledKeepsTheClaim covers the other side.
//
// The invalidation is a no-op in this configuration; it is asserted anyway so
// that the call cannot be dropped without a test noticing, since it is what
// carries the repair once Redis is on.
//
// Asserted against the invalidation seam rather than against a real Redis:
// the suite runs without one, where the real call silently returns nil.
func TestReportElevenLabsUsage_ReleaseDropsTheCachedBalance(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4033
	const tokenID = 5033
	seedUserTokenChannel(t, userID, tokenID, "tk-real-33", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-33",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, 1500, true))

	cacheCalls := stubInvalidateUserCache(t, nil)
	stubPostAudioConsumeQuota(t, errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))

	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":10}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-33", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())

	// The cached balance must be dropped for the row's owner — not the
	// authenticated caller, which is the same user here but is read from a
	// different place in the handler.
	require.Equal(t, []int{userID}, *cacheCalls,
		"the reservation owner's cached balance must be invalidated before a retry is advertised")

	var errResp struct {
		Retryable bool `json:"retryable"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &errResp))
	assert.True(t, errResp.Retryable, "a successful invalidation is what the handler advertises a retry on")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusReserved, got.Status)
}

// TestReportElevenLabsUsage_RedisEnabledKeepsTheClaim is the answer to the P1
// found independently on both this PR and #20: a successful `InvalidateUserCache`
// is NOT sufficient to make the claim safely retryable.
//
// `RedisHIncrBy` reads the TTL and runs its HINCRBY in two round trips with no
// WATCH, so a goroutine that read a positive TTL just before the delete still
// execs after it — HINCRBY recreates the key and the pipelined Expire re-arms
// it, leaving a hash holding only the phantom delta. The delete cannot be made
// to win from the caller's side.
//
// So under Redis the row is NOT handed back. Answering `retryable: false` while
// releasing would leave the retry advertised-against but not prevented: the
// status is this design's only real idempotency guard and the response flag is
// advice, so a client that ignores it could re-enter billing and apply the
// resurrected delta a second time. The status has to carry it.
//
// Asserted through the seam because the suite has no Redis; the flag is flipped
// only around the request itself so the fixtures' own quota writes do not touch
// a nil client.
func TestReportElevenLabsUsage_RedisEnabledKeepsTheClaim(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4035
	const tokenID = 5035
	seedUserTokenChannel(t, userID, tokenID, "tk-real-35", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-35",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, 1500, true))

	// Invalidation SUCCEEDS here. That is the whole point: success is still not
	// enough, so this must not be mistaken for the cache-failure branch.
	cacheCalls := stubInvalidateUserCache(t, nil)
	billingCalls := stubPostAudioConsumeQuota(t,
		errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))

	stubRedisEnabled(t, true)

	// An under-reserve report: the refund direction, where a second application
	// of the cached delta creates spendable quota rather than merely
	// over-charging.
	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":10}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-35", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())

	// The repair is still attempted — it is best-effort, not abandoned.
	assert.Equal(t, []int{userID}, *cacheCalls, "the cached balance is still dropped as best-effort repair")

	var errResp struct {
		Retryable bool `json:"retryable"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &errResp))
	assert.False(t, errResp.Retryable,
		"under Redis the retry premise is unprovable, so the promise must not be made")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettling, got.Status,
		"the claim must be kept, not merely advertised against")

	// The status, not the flag, is what actually stops it: a client ignoring
	// `retryable` gets 409 and never re-enters billing.
	require.Equal(t, 1, *billingCalls)
	w2 := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-35", body)
	assert.Equal(t, http.StatusConflict, w2.Code, "body=%s", w2.Body.String())
	assert.Equal(t, 1, *billingCalls,
		"a client that ignores retryable:false must still be unable to re-apply the cached delta")
}

// TestReportElevenLabsUsage_StaleCacheKeepsTheClaim covers the branch where the
// repair itself fails. If the key cannot be dropped, the cache may still carry
// a delta the database never took, so the premise behind a retry is not
// restorable — and a claimable row whose cached balance is wrong is the
// expensive direction of the trade. The claim stays, and the client is told so.
func TestReportElevenLabsUsage_StaleCacheKeepsTheClaim(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4034
	const tokenID = 5034
	seedUserTokenChannel(t, userID, tokenID, "tk-real-34", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-34",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, 1500, true))

	stubInvalidateUserCache(t, errors.New("dial tcp 10.0.0.6:6379: connect: connection refused"))
	billErr := errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer")
	billingCalls := stubPostAudioConsumeQuota(t, billErr)

	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":10}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-34", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())

	var errResp struct {
		Retryable bool `json:"retryable"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &errResp))
	assert.False(t, errResp.Retryable,
		"an unrepairable cached balance must not be advertised as retryable")

	// The row is what actually enforces it: still claimed, so a retry cannot
	// re-enter billing no matter what the client does with the response.
	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettling, got.Status,
		"the claim must be kept when the cached balance cannot be repaired")

	require.Equal(t, 1, *billingCalls)
	w2 := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-34", body)
	assert.Equal(t, http.StatusConflict, w2.Code, "body=%s", w2.Body.String())
	assert.Equal(t, 1, *billingCalls, "the retry must not re-enter billing against a stale cached balance")
}

// TestReportElevenLabsUsage_PartialBillingWithFailedTransitionStaysUnchargeable
// closes the gap the previous test's terminal status depends on, and is the
// evidence for removing the bounded retry that used to guard it.
//
// The retry existed because the closing transition ran from "reserved": if that
// single UPDATE failed, the row still read "reserved", the status guard admitted
// a second report, and the already-committed funding delta was charged twice.
// The two failures are correlated, not independent — the database trouble that
// stopped the settle halfway is exactly what would stop that write — so one
// attempt was not enough.
//
// Claiming the row before billing removes the dependency entirely. By the time
// any money moves the row is at "settling", which no client request can claim,
// so the closing write is bookkeeping rather than the protection itself.
//
// This test therefore fails that write on EVERY attempt — a strictly harsher
// condition than the retry was ever able to survive — and asserts the funding
// delta still cannot be charged a second time.
func TestReportElevenLabsUsage_PartialBillingWithFailedTransitionStaysUnchargeable(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4029
	const tokenID = 5029
	seedUserTokenChannel(t, userID, tokenID, "tk-real-29", 1_000_000, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-29",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, 1500, true))

	partial := fmt.Errorf("%w: %v", service.ErrQuotaPartiallyApplied,
		errors.New("dial tcp 10.0.0.5:5432: connect: connection reset by peer"))
	stubPostAudioConsumeQuota(t, partial)

	// The closing transition never succeeds — the same blip that halved the
	// settle, persisting for the rest of the request.
	prevDesync := markAsrReservationTokenDesyncFn
	t.Cleanup(func() { markAsrReservationTokenDesyncFn = prevDesync })
	desyncCalls := 0
	markAsrReservationTokenDesyncFn = func(id, quota, seconds int) (int64, error) {
		desyncCalls++
		return 0, errors.New("database is locked")
	}

	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":60}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-29", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, 1, desyncCalls, "the closing write is not retried; the claim is what protects the delta")

	// The row never reached its intended status, and that is survivable
	// precisely because "settling" is already unclaimable.
	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettling, got.Status,
		"a failed closing write must leave the row claimed, not back in circulation")

	// The point of all of it: a second report is refused and cannot re-charge
	// the funding delta that already went through — even though the status the
	// handler meant to write never landed.
	again := stubPostAudioConsumeQuota(t, partial)
	w2 := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-29", body)
	assert.Equal(t, http.StatusConflict, w2.Code, "body=%s", w2.Body.String())
	assert.Zero(t, *again, "the re-POST must be rejected before any second charge is attempted")
}

// TestReportElevenLabsUsage_FailedSettleTransitionStaysUnchargeable is the same
// property on the success branch: billing fully applied, but the write that
// records it never lands. The row must stay claimed rather than returning to
// circulation, or the client could report again and be charged a second time
// for a session that was already billed.
func TestReportElevenLabsUsage_FailedSettleTransitionStaysUnchargeable(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4032
	const tokenID = 5032
	const initialUserQuota = 1_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-32", initialUserQuota, 500_000, "default", realtimeAsrModel)

	const reservedQuota = 1500
	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-32",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   reservedQuota,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	require.NoError(t, model.DecreaseUserQuota(userID, reservedQuota, true))

	billingCalls := countingPostAudioConsumeQuota(t)

	prevMark := markAsrReservationSettledFn
	t.Cleanup(func() { markAsrReservationSettledFn = prevMark })
	markAsrReservationSettledFn = func(id, quota, seconds int) (int64, error) {
		return 0, errors.New("database is locked")
	}

	body := `{"reservation_id":` + itoa(res.Id) + `,"duration_seconds":60}`
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-32", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"a settle whose record never landed must not answer 200; body=%s", w.Body.String())

	// 60s @ ratio 3 = 3000, against a 1500 reserve.
	assert.Equal(t, initialUserQuota-3000, getUserQuota(t, userID), "the settle itself did apply")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettling, got.Status)

	w2 := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-32", body)
	assert.Equal(t, http.StatusConflict, w2.Code, "body=%s", w2.Body.String())
	assert.Equal(t, 1, billingCalls(), "the re-POST must not reach billing a second time")
	assert.Equal(t, initialUserQuota-3000, getUserQuota(t, userID),
		"a stranded row must never be billed twice")
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

// countingPostAudioConsumeQuota counts entries into the billing path while
// still performing the real settlement, and returns an accessor for the count.
//
// Distinct from stubPostAudioConsumeQuota, which replaces billing with a fixed
// result, for two reasons. First, the concurrency regression needs the real
// quota writes to happen — the wallet balance is its strongest assertion, and a
// stub would make it vacuous. Second, that helper's counter is a bare int
// incremented from the request goroutine; under genuinely concurrent requests
// that is a data race, and the count it produced could not be trusted.
func countingPostAudioConsumeQuota(t *testing.T) func() int {
	t.Helper()
	prev := postAudioConsumeQuotaFn
	t.Cleanup(func() { postAudioConsumeQuotaFn = prev })

	var mu sync.Mutex
	calls := 0
	postAudioConsumeQuotaFn = func(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, extra string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return prev(c, info, usage, extra)
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// newSettleEntryBarrier returns a replacement for asrSettleEntryGateFn that
// holds every settle request at the entry to the handler until `n` of them have
// arrived, then releases them together.
//
// The barrier is what makes this regression deterministic instead of hopeful.
// Started as bare goroutines, two HTTP requests almost never overlap inside the
// few microseconds between reading the reservation and billing it, so an
// unsynchronised test would pass against the racy implementation on nearly
// every run and be worthless as a regression. Releasing both from a standstill
// at the same instant, with the reservation read as their very next operation,
// produces the interleaving the bug needs.
//
// Times out rather than deadlocking if fewer than n requests arrive — a hung
// test reports nothing useful.
func newSettleEntryBarrier(t *testing.T, n int) func() {
	t.Helper()
	var mu sync.Mutex
	arrivals := 0
	release := make(chan struct{})

	return func() {
		mu.Lock()
		arrivals++
		if arrivals == n {
			close(release)
		}
		mu.Unlock()

		select {
		case <-release:
		case <-time.After(10 * time.Second):
			// Errorf, not Fatalf: this runs on a request goroutine.
			t.Errorf("settle entry barrier timed out waiting for %d concurrent requests", n)
		}
	}
}

// TestReportElevenLabsUsage_ConcurrentSettleAppliesOnce is the TES2-27
// regression.
//
// Two /api/elevenlabs/usage reports for one reservation are driven concurrently
// and held at the claim boundary so both are in flight at once. Exactly one may
// bill.
//
// A zero-duration report is used because that is the direction that mints
// quota: the 30-second reserve (1500 quota) is pre-consumed at mint time and
// settlement applies a *delta* against it, so settling at zero duration refunds
// the whole 1500. Before the claim, both requests passed the status check, both
// billed, and both refunds landed — while only one reserve had ever been taken.
// The user therefore ended richer than they started, repeatably, from an
// ordinary authenticated session.
//
// The wallet assertion is the load-bearing one. Status codes and row state
// could both be made to look right by a fix that still double-applied the
// money; ending at exactly the starting balance could not.
func TestReportElevenLabsUsage_ConcurrentSettleAppliesOnce(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4030
	const tokenID = 5030
	const initialUserQuota = 1_000_000
	const initialTokenQuota = 500_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-30", initialUserQuota, initialTokenQuota, "default", realtimeAsrModel)

	const reservedQuota = 1500 // 30s reserve at ratio 3
	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-30",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   reservedQuota,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))
	// Apply the reserve to the wallet, as PreConsumeBilling does at mint time.
	require.NoError(t, model.DecreaseUserQuota(userID, reservedQuota, true))
	require.Equal(t, initialUserQuota-reservedQuota, getUserQuota(t, userID))

	billingCalls := countingPostAudioConsumeQuota(t)

	const concurrent = 2
	prevGate := asrSettleEntryGateFn
	t.Cleanup(func() { asrSettleEntryGateFn = prevGate })
	asrSettleEntryGateFn = newSettleEntryBarrier(t, concurrent)

	codes := make([]int, concurrent)
	var wg sync.WaitGroup
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func(idx int) {
			defer wg.Done()
			w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-30",
				`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":0}`)
			codes[idx] = w.Code
		}(i)
	}
	wg.Wait()

	// The primary assertion: how many requests entered the billing path at all.
	// Final row state cannot express this — both racing requests can bill and
	// still leave one correct-looking "settled" row behind, which is precisely
	// the bug. Only the entry count separates "settled once" from "billed twice
	// and recorded once".
	assert.Equal(t, 1, billingCalls(),
		"exactly one concurrent settle may reach the billing path")

	ok, conflict := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	assert.Equal(t, 1, ok, "exactly one request may settle successfully, got codes %v", codes)
	assert.Equal(t, 1, conflict, "the losing request must be rejected with 409, got codes %v", codes)

	// The money. One zero-duration settle refunds the one reserve that was
	// taken, so the wallet must land back exactly where it started. Every extra
	// settlement would add another 1500 of quota the user never paid for.
	assert.Equal(t, initialUserQuota, getUserQuota(t, userID),
		"concurrent settles must refund the reserve exactly once")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettled, got.Status)
}

// TestReportElevenLabsUsage_AlreadyClaimedRowBillsNothing isolates the property
// the wallet assertion above depends on but cannot separate from the winner's
// own settlement: a settle for a reservation someone else owns must move no
// quota at all. With no winner in flight to net against, any spending shows up
// directly in the balance.
//
// Scope, stated honestly: the claim here is already visible when the request
// reads the row, so it is the status pre-check that rejects it, not the claim
// CAS. That is a real path worth pinning — it is what makes a *sequential*
// re-report free — but this test does not exercise losing the CAS itself. That
// case is covered by TestReportElevenLabsUsage_ConcurrentSettleAppliesOnce,
// where the loser may lose at either gate, and by
// TestClaimAsrReservationForSettle_ConcurrentSingleWinner in model/.
func TestReportElevenLabsUsage_AlreadyClaimedRowBillsNothing(t *testing.T) {
	initTestDB(t)
	seedRealtimeAsrPricing(t)

	const userID = 4031
	const tokenID = 5031
	const initialUserQuota = 1_000_000
	seedUserTokenChannel(t, userID, tokenID, "tk-real-31", initialUserQuota, 500_000, "default", realtimeAsrModel)

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        "tk-real-31",
		ModelName:       realtimeAsrModel,
		UsingGroup:      "default",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
		CreatedAt:       common.GetTimestamp() - 120,
	}
	require.NoError(t, model.CreateAsrReservation(res))

	// Someone else already owns the settle.
	claimed, err := model.ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	require.True(t, claimed)

	quotaBefore := getUserQuota(t, userID)
	w := postAuthed(t, "/api/elevenlabs/usage", userID, tokenID, "tk-real-31",
		`{"reservation_id":`+itoa(res.Id)+`,"duration_seconds":0}`)
	assert.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, quotaBefore, getUserQuota(t, userID), "a request that loses the claim must bill nothing")

	got, err := model.GetAsrReservationById(res.Id, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.AsrReservationStatusSettling, got.Status, "a refused settle must not move the row")
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
//
// The gin mode is deliberately NOT set here. gin.SetMode writes two package
// globals, and the concurrency test calls this helper from several goroutines
// at once, so setting it per request races those siblings (caught by -race).
// TestMain sets it once before m.Run, which is the only write the whole binary
// needs.
func postAuthed(t *testing.T, path string, userID, tokenID int, tokenKey string, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
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

