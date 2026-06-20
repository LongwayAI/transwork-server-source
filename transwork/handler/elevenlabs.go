package handler

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
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

// callElevenLabsTokenAPIFn is a package-level seam so handler tests can stub
// the ElevenLabs HTTP round-trip without spinning up a real channel.
var callElevenLabsTokenAPIFn = callElevenLabsTokenAPI

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
		common.SysLog("failed to persist AsrReservation: " + err.Error())
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
// client-reported session duration. Idempotent via the AsrReservation row's
// status field: a second call sees status != "reserved" and returns 409.
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
	service.PostAudioConsumeQuota(c, info, usage, fmt.Sprintf("ASR realtime %.2fs", duration))

	// PostAudioConsumeQuota recomputes the quota internally via
	// calculateAudioQuota; re-derive it here from the PriceData populated
	// by ModelPriceHelper so the response (and the AsrReservation row) hold
	// the actual amount charged, not just a hand-rolled estimate.
	actualQuota := computeAudioQuotaFromPriceData(info.PriceData, actualTokens)
	if _, err := model.MarkAsrReservationSettled(req.ReservationId, actualQuota, int(duration)); err != nil {
		// Settle already applied to user quota; failing the DB row update
		// would leave us inconsistent. Log loudly so we can investigate.
		common.SysLog("failed to mark AsrReservation settled: " + err.Error())
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
