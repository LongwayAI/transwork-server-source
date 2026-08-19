package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// realtimeAsrModel is the model name used for realtime ElevenLabs ASR billing.
// Distinct from batch scribe_v2 so admins can price the two tiers separately.
const realtimeAsrModel = "scribe_v2_realtime"

// asrRealtimeReserveSeconds is the floor charge applied at token-mint time.
// If the client never reports usage this becomes the permanent charge.
const asrRealtimeReserveSeconds = 30

// asrRealtimeMaxSeconds caps the duration the client can self-report.
// Combined with a wall-clock check this bounds the worst-case loss when a
// bad actor over-reports.
const asrRealtimeMaxSeconds = 6 * 60 * 60

// AsrRealtimeMaxSeconds re-exports the cap for the stale-reservation monitor in
// the parent overlay package, which must not call a session stale until this
// long has passed (plus grace for the usage report itself). Exported as an
// alias rather than renaming the unexported constant so the existing uses and
// their comments stay untouched — and so the monitor tracks the real limit
// instead of duplicating the literal, which no test could catch drifting.
const AsrRealtimeMaxSeconds = asrRealtimeMaxSeconds

// callElevenLabsTokenAPIFn is a package-level seam so handler tests can stub
// the ElevenLabs HTTP round-trip without spinning up a real channel.
var callElevenLabsTokenAPIFn = callElevenLabsTokenAPI

// postAudioConsumeQuotaFn is a package-level seam (same pattern as
// callElevenLabsTokenAPIFn) so a test can drive the settlement-failure
// branches. They are otherwise unreachable from a test: both the wallet and the
// token-quota writes are plain single-statement GORM updates that succeed even
// against rows that do not exist, so no fixture can make the real settle fail
// on purpose, and a test that only exercised the success path would pass just
// as happily against the bug this seam exists to pin.
var postAudioConsumeQuotaFn = service.PostAudioConsumeQuota

// markAsrReservationSettledFn / markAsrReservationTokenDesyncFn are
// package-level seams, same rationale as above, so a test can fail either
// closing transition and prove the row stays out of reach when it does.
//
// Both closing transitions run from "settling", which no client request can
// claim, so a failure leaves the reservation unreachable rather than
// re-chargeable. That is what removed the need to retry them.
var markAsrReservationSettledFn = model.MarkAsrReservationSettled
var markAsrReservationTokenDesyncFn = model.MarkAsrReservationTokenDesync

// invalidateUserCacheFn is a package-level seam, as above. Redis is not
// available in the handler test suite, where model.InvalidateUserCache is a
// silent no-op (model/user_cache.go returns nil when !common.RedisEnabled), so
// without it neither the call nor its failure branch could be observed at all.
var invalidateUserCacheFn = model.InvalidateUserCache

// redisEnabledFn is a package-level seam, as above, reporting whether the cache
// is live. It exists so the retryable-promise tests can drive both branches
// without assigning to common.RedisEnabled: the quota helpers hand their cache
// delta to a gopool goroutine that reads that flag and outlives the request, so
// a test writing it races those reads (caught by -race on the settle tests).
// Reading through a seam the background goroutines never touch keeps the flag
// write-once at startup, which is what production already assumes.
var redisEnabledFn = func() bool { return common.RedisEnabled }

// asrSettleEntryGateFn is a package-level seam invoked once per settle request,
// before the reservation is read and therefore before anything can have moved
// its status. It is a no-op in production and exists so the concurrency
// regression test can hold every in-flight settle here until all of them have
// arrived, then release them together.
//
// Without it the test could only start goroutines and hope they overlap. They
// usually do not — the window between reading the reservation and billing is a
// few microseconds — so an unsynchronised test would pass against the racy
// implementation on nearly every run and be worthless as a regression.
//
// It sits at the entry rather than deeper in the settle path because a request
// that loses the race returns early, so any later point is one that only the
// winner is guaranteed to reach; a barrier there waits forever for an arrival
// that will never come.
var asrSettleEntryGateFn = func() {}

type ElevenLabsTokenRequest struct {
	ModelName string `json:"model_name" binding:"required"`
}

type ElevenLabsTokenResponse struct {
	Success        bool   `json:"success"`
	Token          string `json:"token,omitempty"`
	ReservationId  int    `json:"reservation_id,omitempty"`
	ReserveSeconds int    `json:"reserve_seconds,omitempty"`
	Message        string `json:"message,omitempty"`
	Error          string `json:"error,omitempty"`
}

type ElevenLabsUsageRequest struct {
	ReservationId   int     `json:"reservation_id" binding:"required"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type ElevenLabsUsageResponse struct {
	Success      bool `json:"success"`
	ActualQuota  int  `json:"actual_quota"`
	DeltaApplied int  `json:"delta_applied"`
}

type ElevenLabsSingleUseTokenResponse struct {
	Token string `json:"token"`
}

// CreateElevenLabsTempToken maps a user token and requested model to an
// ElevenLabs channel and exchanges the channel key for a provider-issued
// single-use token. Reserves a floor charge (asrRealtimeReserveSeconds) up
// front so a session can't run for free; the matching settle handler
// (ReportElevenLabsUsage) computes the delta when the client reports duration.
//
// Order is reserve → mint → persist:
//   - reserve first so we never call ElevenLabs for an insolvent user;
//   - mint second so we have a real token to return on the happy path;
//   - persist last so a DB hiccup costs only the floor charge (not the token).
func CreateElevenLabsTempToken(c *gin.Context) {
	var req ElevenLabsTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ElevenLabsTokenResponse{
			Success: false,
			Error:   "Invalid request: " + err.Error(),
		})
		return
	}

	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, ElevenLabsTokenResponse{
			Success: false,
			Error:   "Authentication required",
		})
		return
	}

	group := effectiveUsingGroup(c)
	if group == "" {
		c.JSON(http.StatusInternalServerError, ElevenLabsTokenResponse{
			Success: false,
			Error:   "No token group available for channel selection",
		})
		return
	}

	channel, err := model.GetChannel(group, req.ModelName, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ElevenLabsTokenResponse{
			Success: false,
			Error:   "Failed to get channel: " + err.Error(),
		})
		return
	}
	if channel == nil {
		c.JSON(http.StatusNotFound, ElevenLabsTokenResponse{
			Success: false,
			Error:   fmt.Sprintf("No available channel for model: %s", req.ModelName),
		})
		return
	}
	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, ElevenLabsTokenResponse{
			Success: false,
			Error:   "Channel not configured properly",
		})
		return
	}

	info := &relaycommon.RelayInfo{
		UserId:          userID,
		OriginModelName: realtimeAsrModel,
		RequestId:       common.GetContextKeyString(c, common.RequestIdKey),
		TokenId:         c.GetInt("token_id"),
		TokenKey:        common.GetContextKeyString(c, constant.ContextKeyTokenKey),
		TokenUnlimited:  common.GetContextKeyBool(c, constant.ContextKeyTokenUnlimited),
		UsingGroup:      group,
		UserGroup:       common.GetContextKeyString(c, constant.ContextKeyUserGroup),
		StartTime:       time.Now(),
		ForcePreConsume: true,
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: channel.Id},
	}
	if us, ok := common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting); ok {
		info.UserSetting = us
	}
	reserveTokens := int(math.Ceil(float64(asrRealtimeReserveSeconds) / 60.0 * 1000.0))
	priceData, priceErr := helper.ModelPriceHelper(c, info, reserveTokens, &types.TokenCountMeta{})
	if priceErr != nil {
		c.JSON(http.StatusInternalServerError, ElevenLabsTokenResponse{
			Success: false,
			Error:   "ASR pricing not configured: " + priceErr.Error(),
		})
		return
	}
	if apiErr := service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info); apiErr != nil {
		c.JSON(apiErr.StatusCode, ElevenLabsTokenResponse{
			Success: false,
			Error:   apiErr.Error(),
		})
		return
	}

	tempToken, err := callElevenLabsTokenAPIFn(channel.Key)
	if err != nil {
		if info.Billing != nil {
			info.Billing.Refund(c)
		}
		c.JSON(http.StatusBadGateway, ElevenLabsTokenResponse{
			Success: false,
			Error:   "Failed to generate token: " + err.Error(),
		})
		return
	}

	res := &model.AsrReservation{
		UserId:          userID,
		TokenId:         info.TokenId,
		TokenKey:        info.TokenKey,
		ModelName:       info.OriginModelName,
		UsingGroup:      info.UsingGroup,
		ReservedQuota:   info.FinalPreConsumedQuota,
		ReservedSeconds: asrRealtimeReserveSeconds,
	}
	if err := model.CreateAsrReservation(res); err != nil {
		// Reserve + mint already succeeded; persistence failure means the
		// session can run without a matching settle row. We accept losing
		// the floor charge above the reserve in this rare case rather than
		// returning a confusing partial-success error to the client.
		// Logged at error level with a stable grep token so these
		// unsettleable sessions can be alerted on (they have no row at all,
		// so no sweeper can ever recover them).
		logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_PERSIST_FAILED user_id=%d token_id=%d reserved_quota=%d reserved_seconds=%d model=%s error=%q",
			userID, info.TokenId, info.FinalPreConsumedQuota, asrRealtimeReserveSeconds, info.OriginModelName, err.Error()))
	}

	c.JSON(http.StatusOK, ElevenLabsTokenResponse{
		Success:        true,
		Token:          tempToken,
		ReservationId:  res.Id,
		ReserveSeconds: asrRealtimeReserveSeconds,
		Message:        "Token generated successfully",
	})
}

func GetElevenLabsTokenStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": "ElevenLabs Token Service",
	})
}

// clampReportedDuration bounds the client-reported duration by:
//  1. zero (negative reports become 0 — the reserve floor stands);
//  2. asrRealtimeMaxSeconds hard cap;
//  3. wall-clock since the reservation was created (a bad actor can't claim
//     more time than has actually elapsed since they minted the token).
//
// Pure and DB-free so the clamping logic can be unit-tested directly.
func clampReportedDuration(reported float64, createdAt, now int64) float64 {
	if reported < 0 {
		reported = 0
	}
	wallClock := float64(now - createdAt)
	if wallClock < 0 {
		wallClock = 0
	}
	maxAllowed := math.Min(float64(asrRealtimeMaxSeconds), wallClock)
	if reported > maxAllowed {
		reported = maxAllowed
	}
	return reported
}

// ReportElevenLabsUsage settles a realtime ASR reservation against the
// client-reported session duration. Single-apply via a conditional claim on the
// AsrReservation row: the settlement is claimed ("reserved" → "settling")
// before any quota moves, so of any number of simultaneous reports exactly one
// can bill and the rest get 409 without spending anything.
//
// Single-apply is a property of the *balance*, not of the whole handler: the
// pre-check-then-409 path is unchanged for sequential repeats, but a claim that
// wins and then fails mid-billing leaves statistics rows behind it (see the
// stranded-"settling" note below).
//
// The duration is clamped to (0, min(6h, wall_clock_since_mint)) before being
// converted to virtual tokens, so a bad actor's worst case is bounded by how
// long it's been since they minted the token.
func ReportElevenLabsUsage(c *gin.Context) {
	var req ElevenLabsUsageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	asrSettleEntryGateFn()

	res, err := model.GetAsrReservationById(req.ReservationId, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to lookup reservation: " + err.Error()})
		return
	}
	if res == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Reservation not found"})
		return
	}
	if res.Status != model.AsrReservationStatusReserved {
		c.JSON(http.StatusConflict, gin.H{"error": "Reservation already settled"})
		return
	}

	duration := clampReportedDuration(req.DurationSeconds, res.CreatedAt, common.GetTimestamp())

	info := &relaycommon.RelayInfo{
		UserId:                res.UserId,
		OriginModelName:       res.ModelName,
		TokenId:               res.TokenId,
		TokenKey:              res.TokenKey,
		UsingGroup:            res.UsingGroup,
		FinalPreConsumedQuota: res.ReservedQuota,
		// Billing intentionally nil — SettleBilling's legacy fallback path
		// (service/billing.go:72) computes the delta against
		// FinalPreConsumedQuota and applies it via PostConsumeQuota, which
		// is exactly what we want across the mint→settle HTTP gap.
		Billing:   nil,
		StartTime: time.Unix(res.CreatedAt, 0),
		// UserGroup drives special group-ratio lookups in ModelPriceHelper;
		// read it from the authenticated context (the reservation is
		// owner-scoped, so it's the same user that minted it).
		UserGroup: common.GetContextKeyString(c, constant.ContextKeyUserGroup),
		// PostAudioConsumeQuota dereferences ChannelMeta.ChannelId when
		// writing the usage-log row and updating channel quota. The original
		// channel id isn't persisted on the reservation (a settle minutes
		// later may even hit a different channel), so we use 0 — the
		// channel-used-quota update is a no-op for id 0.
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 0},
	}
	if us, ok := common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting); ok {
		info.UserSetting = us
	}
	actualTokens := int(math.Ceil(duration / 60.0 * 1000.0))
	if _, err := helper.ModelPriceHelper(c, info, actualTokens, &types.TokenCountMeta{}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ASR pricing not configured: " + err.Error()})
		return
	}

	usage := &dto.Usage{
		CompletionTokens:       actualTokens,
		TotalTokens:            actualTokens,
		CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: actualTokens},
	}

	// Claim the reservation BEFORE any quota moves. The status read at the top
	// of this handler only rejects settles that are already finished when the
	// request arrives; it cannot stop two in-flight requests from both passing
	// it and both billing. Since settlement applies a delta against the
	// pre-consumed reserve, every extra racing request refunded a reserve that
	// was only ever taken once — an authenticated client could mint quota by
	// firing concurrent zero-duration reports. The conditional update below is
	// the serialisation point: exactly one caller can move the row off
	// "reserved", and only that caller is allowed to spend anything.
	//
	// Claimed here rather than earlier so that the failures above (pricing,
	// lookup, a malformed body) still leave the row at "reserved" and freely
	// re-reportable. Everything between this point and the terminal transition
	// below is the window in which a crash strands the row at "settling".
	claimed, claimErr := model.ClaimAsrReservationForSettle(req.ReservationId)
	if claimErr != nil {
		// Fail closed: an unverifiable claim must not bill. Rejecting here
		// leaves the row at "reserved", so the client can simply report again.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claim reservation: " + claimErr.Error()})
		return
	}
	if !claimed {
		// Another request owns the settle, or the row already left "reserved".
		// Same 409 the pre-check returns, and — the point of the whole change —
		// reached without having billed anything.
		c.JSON(http.StatusConflict, gin.H{"error": "Reservation already settled"})
		return
	}

	billErr := postAudioConsumeQuotaFn(c, info, usage, fmt.Sprintf("ASR realtime %.2fs", duration))

	// PostAudioConsumeQuota recomputes the quota internally via
	// calculateAudioQuota; re-derive it here from the PriceData populated
	// by ModelPriceHelper so the response (and the AsrReservation row) hold
	// the actual amount charged, not just a hand-rolled estimate.
	actualQuota := computeAudioQuotaFromPriceData(info.PriceData, actualTokens)

	// Settlement failed outright: nothing moved (service.ErrQuotaPartiallyApplied
	// would say otherwise, and is handled after the transition below). Hand the
	// claim back so the row is exactly as retryable as it was a moment ago, and
	// re-applying the same delta once is the correct recovery. Marking it
	// terminal here would strand a reservation that was never billed, leaving
	// the user charged the full 30-second reserve for a session that may have
	// lasted two seconds.
	//
	// Releasing is safe only on this branch, and only because
	// service.PostConsumeQuota fails the funding step before touching anything
	// else — the distinction the ErrQuotaPartiallyApplied sentinel exists to
	// draw, and which service/billing_partial_test.go pins.
	//
	// "Retryable" is a statement about the balance, not about the whole handler:
	// a retry re-applies the quota delta exactly once, but statistics that ran
	// before the failure (used_quota, request_count) and the consume-log row
	// written after it are counted again.
	//
	// Whether the client may retry is carried by the "retryable" field rather
	// than by the prose, so that decision never depends on parsing a message.
	if billErr != nil && !errors.Is(billErr, service.ErrQuotaPartiallyApplied) {
		// "Nothing was applied" is true of the database but not of the cache.
		// Increase/DecreaseUserQuota hand the Redis delta to a goroutine and
		// swallow its error BEFORE attempting the synchronous database write
		// (model/user.go), so a settle that failed at the database has still
		// moved the cached balance. Handing the row back on top of that lets a
		// retry apply the cache delta twice against one database delta: over
		// the reserve it double-debits and locks the user out until the key
		// expires; under it — the refund direction, which is this endpoint's
		// common case — it double-credits, and since the cache is what serves
		// quota checks, that permits spending the user does not have.
		//
		// Dropping the key repairs the common case: whenever the goroutine has
		// already landed, its delta goes with the key and the next read
		// rehydrates from the database — which is the state the failed settle
		// actually left.
		//
		// It cannot be made to win the race, though. RedisHIncrBy reads the
		// TTL and runs its HINCRBY in two separate round trips with no WATCH
		// (common/redis.go), so MULTI/EXEC does not abort when the key changed
		// in between: a goroutine that read a positive TTL just before this
		// delete still execs after it, HINCRBY recreates the missing key, and
		// the pipelined Expire re-arms it — leaving a hash holding nothing but
		// the phantom delta. Closing that means making the conditional
		// increment atomic (a Lua script) in shared Redis code that also backs
		// the token cache, and there is no Redis in this repo's test
		// environment to verify such a script against. It is filed separately
		// and is deliberately not attempted here or in #20.
		//
		// So the delete stays as best-effort repair, but the retry promise is
		// made only where it is provably true: with Redis off the cache helpers
		// no-op outright (model/user_cache.go), so the failed database write is
		// the only thing that could have moved anything, and it did not. With
		// Redis on we cannot say that, so we do not say it.
		//
		// The release is gated on the same condition, not just the response
		// flag. Answering "retryable": false while handing the row back to
		// "reserved" would leave the retry advertised-against but not
		// prevented: the status is this design's only real idempotency guard
		// and the response flag is advice, so a client that ignores it — or a
		// future sweeper — could re-enter billing and apply a resurrected cache
		// delta a second time. Withholding the promise while leaving the door
		// open would be relying on exactly the guarantee this change exists to
		// stop issuing.
		//
		// The cost of keeping the claim is a row stranded at "settling" with
		// the user still charged the reserve, needing TES2-21 or a human. That
		// is bounded, visible in the monitor's Unknown bucket, and it is the
		// cheaper side: the alternative creates spendable quota nobody paid
		// for, silently and repeatably.
		//
		// It is also close to free where it matters. This handler never sets
		// BillingSource, so PostConsumeQuota always takes the wallet path,
		// which returns nil early when BATCH_UPDATE_ENABLED is "true"
		// (model/user.go) — so under the production compose file this branch is
		// not reachable at all. The configurations that would strand a row here
		// are precisely the ones where the phantom delta can exist.
		var releasedRows int64
		var releaseErr error
		released := false
		cacheErr := invalidateUserCacheFn(res.UserId)
		if cacheErr == nil && !redisEnabledFn() {
			releasedRows, releaseErr = model.ReleaseAsrReservationClaim(req.ReservationId)
			released = releaseErr == nil && releasedRows == 1
		}
		retryable := released
		logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_BILLING_FAILED reservation_id=%d user_id=%d actual_quota=%d duration_seconds=%d released=%t retryable=%t error=%q",
			req.ReservationId, userID, actualQuota, int(duration), released, retryable, billErr.Error()))
		if cacheErr != nil {
			// Best-effort repair failed too, so the cache may carry a delta the
			// database never took. The claim is kept for the same reason as
			// above, and this names the case distinctly because it is the one
			// where even a deliberate recovery has to reconcile the cache
			// first.
			logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_QUOTA_CACHE_STALE reservation_id=%d user_id=%d actual_quota=%d reserved_quota=%d bill_error=%q cache_error=%q",
				req.ReservationId, res.UserId, actualQuota, res.ReservedQuota, billErr.Error(), cacheErr.Error()))
		} else if !redisEnabledFn() && !retryable {
			// The claim could not be handed back, so the row is stranded at
			// "settling" and no client can report it again. Say so rather than
			// promising a retry that will be refused with 409.
			logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_CLAIM_RELEASE_FAILED reservation_id=%d user_id=%d rows=%d error=%v",
				req.ReservationId, userID, releasedRows, releaseErr))
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":     "Billing settlement failed, usage was not charged: " + billErr.Error(),
			"retryable": retryable,
		})
		return
	}

	// Half-applied settlement: the funding source (wallet or subscription) took
	// the full delta and only the token-quota counter failed to follow. The row
	// is parked at "token_desync" instead of "settled": the money has already
	// moved, so a re-POST would charge the delta a second time (or, when the
	// client reported under the 30s reserve and the delta is negative, refund it
	// a second time). Unclaimable is the safe end state in both directions.
	//
	// It gets its own status rather than being recorded as a settlement so the
	// TES2-22 monitor counts it as Unknown — a number that needs looking at —
	// rather than as a healthy settle. Recording it as "settled" would leave the
	// drift visible only in a log line, which is only as good as something
	// reading that log.
	//
	// What remains is a tokens.remain_quota/used_quota drift that only a human
	// can reconcile; the row carries the figures the funding side actually
	// applied so the repair does not have to be reconstructed from logs.
	if billErr != nil {
		desyncRows, desyncErr := markAsrReservationTokenDesyncFn(req.ReservationId, actualQuota, int(duration))
		if desyncErr != nil || desyncRows == 0 {
			// Deliberately not retried. This write is no longer what protects
			// the already-committed funding delta from a second charge — the
			// claim is, and it happened before any money moved. The row is
			// still at "settling", which no client can claim, so a failure here
			// costs visibility, not safety. Retrying would imply a protection
			// that is actually coming from somewhere else.
			logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_DESYNC_PERSIST_FAILED reservation_id=%d user_id=%d rows=%d error=%v",
				req.ReservationId, userID, desyncRows, desyncErr))
		}
		logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_TOKEN_QUOTA_DESYNC reservation_id=%d user_id=%d token_id=%d actual_quota=%d reserved_quota=%d duration_seconds=%d error=%q",
			req.ReservationId, userID, res.TokenId, actualQuota, res.ReservedQuota, int(duration), billErr.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":     "Billing settlement partially applied: funding was charged but token quota was not adjusted. Do not retry; this needs reconciliation.",
			"retryable": false,
		})
		return
	}

	settledRows, err := markAsrReservationSettledFn(req.ReservationId, actualQuota, int(duration))
	if err != nil || settledRows == 0 {
		// The money moved but the row did not follow, so it is stranded at
		// "settling": no client can settle it again (only "reserved" is
		// claimable), and nothing yet un-strands it automatically. That is the
		// safe side of the trade — the alternative, leaving it re-reportable,
		// is the double-apply this change exists to stop — but it does mean the
		// user keeps whatever the reserve over-charged until someone acts on
		// these logs or the TES2-21 sweeper ships.
		//
		// Reported as 500, not 200. The old code swallowed both outcomes and
		// answered as though the settle had completed, which is what let the
		// race stay invisible. A retry is harmless: the claim will refuse it.
		reason := "row left settling before the terminal update (sweep, delete, or manual edit)"
		if err != nil {
			reason = err.Error()
		}
		logger.LogError(c, fmt.Sprintf("ASR_RESERVATION_SETTLE_PERSIST_FAILED reservation_id=%d user_id=%d actual_quota=%d duration_seconds=%d rows=%d error=%q",
			req.ReservationId, userID, actualQuota, int(duration), settledRows, reason))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Usage was billed but the reservation could not be finalized"})
		return
	}

	c.JSON(http.StatusOK, ElevenLabsUsageResponse{
		Success:      true,
		ActualQuota:  actualQuota,
		DeltaApplied: actualQuota - res.ReservedQuota,
	})
}

// computeAudioQuotaFromPriceData mirrors service.calculateAudioQuota for the
// audio-completion-only case (no input/text tokens) so the handler can echo
// the charge in the response. Keeping it here (instead of exporting from
// service/) avoids touching upstream package boundaries (Rule 4).
func computeAudioQuotaFromPriceData(p types.PriceData, audioOutputTokens int) int {
	if p.UsePrice {
		return int(p.ModelPrice * common.QuotaPerUnit * p.GroupRatioInfo.GroupRatio)
	}
	ratio := p.ModelRatio * p.GroupRatioInfo.GroupRatio
	audioRatio := p.AudioRatio
	if audioRatio == 0 {
		audioRatio = 1
	}
	audioCompletionRatio := p.AudioCompletionRatio
	if audioCompletionRatio == 0 {
		audioCompletionRatio = 1
	}
	q := float64(audioOutputTokens) * audioRatio * audioCompletionRatio * ratio
	if ratio != 0 && q <= 0 {
		q = 1
	}
	return int(math.Round(q))
}

func callElevenLabsTokenAPI(apiKey string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest("POST", "https://api.elevenlabs.io/v1/single-use-token/realtime_scribe", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	if common.DebugEnabled {
		if dump, err := httputil.DumpRequestOut(req, false); err == nil {
			common.SysLog("elevenlabs token request: " + string(dump))
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if common.DebugEnabled {
		if dump, err := httputil.DumpResponse(resp, false); err == nil {
			common.SysLog("elevenlabs token response: " + string(dump))
		}
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ElevenLabs API returned status %d", resp.StatusCode)
	}

	var result ElevenLabsSingleUseTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("empty token in response")
	}
	return result.Token, nil
}
