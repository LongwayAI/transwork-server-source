package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	twstorage "github.com/QuantumNous/new-api/transwork/storage"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type UploadURLRequest struct {
	Filename    string `json:"filename" binding:"required"`
	ContentType string `json:"contentType" binding:"required"`
}

type UploadURLResponse struct {
	UploadURL string `json:"uploadUrl"`
	ObjectKey string `json:"objectKey"`
}

type TranscribeRequest struct {
	ObjectKey    string   `json:"objectKey" binding:"required"`
	Language     string   `json:"language,omitempty"`
	LanguageCode string   `json:"languageCode,omitempty"`
	Keyterms     []string `json:"keyterms,omitempty"`
}

type TranscribeResponse struct {
	Text         string                   `json:"text"`
	LanguageCode string                   `json:"languageCode,omitempty"`
	ModelName    string                   `json:"modelName,omitempty"`
	Lines        []TranscriptLineResponse `json:"lines,omitempty"`
	Words        []ElevenLabsSTTWord      `json:"words,omitempty"`
	RawResponse  map[string]interface{}   `json:"rawResponse,omitempty"`
}

type ElevenLabsSTTResponse struct {
	Text         string              `json:"text"`
	LanguageCode string              `json:"language_code,omitempty"`
	Words        []ElevenLabsSTTWord `json:"words,omitempty"`
}

type ElevenLabsSTTWord struct {
	Start     float64  `json:"start"`
	End       float64  `json:"end"`
	Text      string   `json:"text"`
	Type      string   `json:"type,omitempty"`
	Logprob   *float64 `json:"logprob,omitempty"`
	SpeakerID string   `json:"speaker_id,omitempty"`
}

type TranscriptLineResponse struct {
	ID           string   `json:"id"`
	Timestamp    uint64   `json:"timestamp"`
	EndTimestamp *uint64  `json:"endTimestamp,omitempty"`
	Speaker      *string  `json:"speaker,omitempty"`
	Text         string   `json:"text"`
	Confidence   *float32 `json:"confidence,omitempty"`
	IsFinal      *bool    `json:"isFinal,omitempty"`
}

const batchTranscriptionModel = "scribe_v2"

const (
	// DEV-405: ElevenLabs fetches the audio from object storage via a
	// presigned URL; the TTL matches the 60-min HTTP backstop below so the
	// URL outlives any queue-before-fetch on their side.
	transcribeSourceURLTTL = 60 * time.Minute
	// ElevenLabs rejects source_url files of 2 GB or more (~17 h of 16 kHz
	// PCM WAV), so oversized objects fail fast with a clear error instead.
	maxTranscribeSourceBytes = int64(2) << 30
	// Interim guard from DEV-405/DEV-404 bottleneck #2: bound concurrent
	// batch transcriptions so overlapping meeting finishes queue briefly and
	// then reject gracefully instead of piling onto the upstream unbounded.
	transcribeMaxConcurrent = 16
	transcribeQueueWait     = 5 * time.Minute
	// A canonical PCM WAV header is 44 bytes; 4 KiB covers writers that
	// insert extra metadata chunks before the data chunk.
	wavHeaderProbeBytes = 4096
)

var transcribeSlots = make(chan struct{}, transcribeMaxConcurrent)

// acquireTranscribeSlot blocks up to wait for a transcription slot and
// returns a release func. The caller must release exactly once.
func acquireTranscribeSlot(ctx context.Context, wait time.Duration) (func(), error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case transcribeSlots <- struct{}{}:
		return func() { <-transcribeSlots }, nil
	case <-timer.C:
		return nil, fmt.Errorf("transcription queue is full")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func GenerateAudioUploadURL(c *gin.Context) {
	var req UploadURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	store, err := twstorage.GetObjectStore()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ext := strings.ToLower(filepath.Ext(req.Filename))
	validExtensions := []string{".wav", ".mp3", ".mp4", ".m4a", ".webm", ".ogg", ".oga", ".flac", ".aac", ".amr"}
	isValidExt := false
	for _, validExt := range validExtensions {
		if ext == validExt {
			isValidExt = true
			break
		}
	}
	if !isValidExt {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Unsupported file format: %s", ext)})
		return
	}

	safeFilename := sanitizeAudioFilename(req.Filename)
	if safeFilename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid filename"})
		return
	}

	objectKey := fmt.Sprintf("audio/%d/%s_%s", userID, uuid.New().String(), safeFilename)
	uploadURL, err := store.SignedUploadURL(objectKey, req.ContentType, 15*time.Minute)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate upload URL"})
		return
	}

	c.JSON(http.StatusOK, UploadURLResponse{
		UploadURL: uploadURL,
		ObjectKey: objectKey,
	})
}

func TranscribeAudio(c *gin.Context) {
	var req TranscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	userID := c.GetInt("id")
	if userID == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if !isOwnedAudioObjectKey(userID, req.ObjectKey) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied for requested audio object"})
		return
	}

	store, err := twstorage.GetObjectStore()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// DEV-540: a repeat call for an already-transcribed object (client retry
	// after a lost response) returns the cached result without re-billing.
	if cached, ok := readCachedTranscript(store, req.ObjectKey); ok {
		logger.LogInfo(c, "transcribe cache hit for "+req.ObjectKey+", skipping billing")
		c.JSON(http.StatusOK, cached)
		return
	}

	result, err, shared := transcribeFlight.Do(req.ObjectKey, func() (interface{}, error) {
		return transcribeAndBill(c, req, store)
	})
	if err != nil {
		status, message := transcribeErrorStatus(err)
		c.JSON(status, gin.H{"error": message})
		return
	}
	if shared {
		logger.LogInfo(c, "transcribe joined in-flight transcription for "+req.ObjectKey+", billed once")
	}
	c.JSON(http.StatusOK, result.(*TranscribeResponse))
}

// transcribeAndBill runs the billed transcription for one object_key. It
// executes inside transcribeFlight, so exactly one caller per key runs it at a
// time; c belongs to the initiating request — billing happens once there, and
// duplicate callers share the result without touching their own quota.
func transcribeAndBill(c *gin.Context, req TranscribeRequest, store twstorage.ObjectStore) (*TranscribeResponse, error) {
	// Re-check the cache inside the flight: a request that missed the cache
	// just as a previous flight completed must not start a second billed run.
	if cached, ok := readCachedTranscript(store, req.ObjectKey); ok {
		return cached, nil
	}

	userID := c.GetInt("id")

	group := effectiveUsingGroup(c)
	if group == "" {
		return nil, newTranscribeError(http.StatusInternalServerError, "No token group available for channel selection")
	}

	channel, err := model.GetChannel(group, batchTranscriptionModel, 0)
	if err != nil {
		return nil, newTranscribeError(http.StatusInternalServerError, "Failed to get channel: "+err.Error())
	}
	if channel == nil {
		return nil, newTranscribeError(http.StatusNotFound, fmt.Sprintf("No available channel for model: %s", batchTranscriptionModel))
	}
	if channel.Key == "" {
		return nil, newTranscribeError(http.StatusInternalServerError, "Channel not configured properly")
	}

	// DEV-405: the server never downloads the audio. Object metadata drives
	// the reserve, a presigned URL lets ElevenLabs fetch the bytes directly,
	// and the settle reads at most a WAV header — per-call RAM stays flat.
	attrs, err := store.Attrs(context.Background(), req.ObjectKey)
	if err != nil {
		return nil, newTranscribeError(http.StatusInternalServerError, "Failed to read audio object")
	}
	if attrs.Size >= maxTranscribeSourceBytes {
		return nil, newTranscribeError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Audio object is too large for transcription (%d bytes, limit %d)", attrs.Size, maxTranscribeSourceBytes))
	}

	// Reserve quota up-front based on a worst-case PCM upper bound
	// (bytes / (16kHz × 2 bytes/sample)). On success the settle delta comes
	// from the WAV header or the transcript's last word-end timestamp; on
	// error the deferred Refund returns the reserve.
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		OriginModelName: batchTranscriptionModel,
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
	estDurationS := float64(attrs.Size) / 32000.0
	estTokens := int(math.Ceil(estDurationS / 60.0 * 1000.0))
	priceData, priceErr := helper.ModelPriceHelper(c, info, estTokens, &types.TokenCountMeta{})
	if priceErr != nil {
		return nil, newTranscribeError(http.StatusInternalServerError, "ASR pricing not configured: "+priceErr.Error())
	}
	if apiErr := service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info); apiErr != nil {
		return nil, newTranscribeError(apiErr.StatusCode, apiErr.Error())
	}
	var settled bool
	defer func() {
		if !settled && info.Billing != nil {
			info.Billing.Refund(c)
		}
	}()

	// The queue context is deliberately Background: a joined DEV-540 retry
	// must be able to share this flight even after the initiating client
	// disconnects, so the flight must not die with the initiator's request.
	release, err := acquireTranscribeSlot(context.Background(), transcribeQueueWait)
	if err != nil {
		return nil, newTranscribeError(http.StatusServiceUnavailable, "Transcription capacity is full, please retry shortly")
	}
	defer release()

	sourceURL, err := store.SignedDownloadURL(req.ObjectKey, transcribeSourceURLTTL)
	if err != nil {
		return nil, newTranscribeError(http.StatusInternalServerError, "Failed to generate audio source URL")
	}

	transcript, rawResponse, err := callElevenLabsSTT(
		channel.Key,
		sourceURL,
		req.EffectiveLanguageCode(),
		req.Keyterms,
	)
	if err != nil {
		// No fallback to a buffered upload: that would resurrect the OOM path
		// under exactly the load that breaks it. A client retry mints a fresh
		// presigned URL, which self-heals transient fetch failures.
		return nil, newTranscribeError(http.StatusBadGateway, "Transcription failed: "+err.Error())
	}

	// Measure actual duration to compute the settle delta. WAV byte rates are
	// exact from a header range read (never size/32000 — imported WAVs can be
	// 44.1 kHz stereo); other formats fall back to the last word's end
	// timestamp, and to the estimated upper bound as a last resort.
	var wavDurationS float64
	if strings.ToLower(filepath.Ext(req.ObjectKey)) == ".wav" {
		if header, headerErr := store.ReadRange(context.Background(), req.ObjectKey, 0, wavHeaderProbeBytes); headerErr == nil {
			if d, wavErr := wavDurationFromHeader(header, attrs.Size); wavErr == nil {
				wavDurationS = d
			}
		}
	}
	durSec := settleDurationSeconds(wavDurationS, transcript.Words, estDurationS)
	actualTokens := int(math.Ceil(durSec / 60.0 * 1000.0))
	usage := &dto.Usage{
		CompletionTokens:       actualTokens,
		TotalTokens:            actualTokens,
		CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: actualTokens},
	}
	service.PostAudioConsumeQuota(c, info, usage, fmt.Sprintf("ASR batch %.2fs", durSec))
	settled = true

	resp := &TranscribeResponse{
		Text:         transcript.Text,
		LanguageCode: transcript.EffectiveLanguageCode(req.EffectiveLanguageCode()),
		ModelName:    batchTranscriptionModel,
		Lines:        buildTranscriptLines(transcript.Words, transcript.Text),
		Words:        transcript.Words,
		RawResponse:  rawResponse,
	}

	// Best-effort cache write (DEV-540); a failure only means a later retry
	// would transcribe and bill again, as before.
	if cacheErr := writeCachedTranscript(store, req.ObjectKey, resp); cacheErr != nil {
		common.SysError("transcribe cache write failed for " + req.ObjectKey + ": " + cacheErr.Error())
	}

	return resp, nil
}

// elevenLabsSTTEndpoint is a var so tests can point the client at a fake.
var elevenLabsSTTEndpoint = "https://api.elevenlabs.io/v1/speech-to-text"

func callElevenLabsSTT(apiKey, sourceURL, languageCode string, keyterms []string) (*ElevenLabsSTTResponse, map[string]interface{}, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	writer.WriteField("model_id", batchTranscriptionModel)
	writer.WriteField("diarize", "true")
	writer.WriteField("timestamps_granularity", "word")
	writer.WriteField("no_verbatim", "true")
	// DEV-405: ElevenLabs fetches the audio itself from the presigned URL, so
	// the request carries no file part and the server holds no audio bytes.
	writer.WriteField("source_url", sourceURL)
	if languageCode != "" {
		writer.WriteField("language_code", languageCode)
	}
	for _, keyterm := range keyterms {
		trimmed := strings.TrimSpace(keyterm)
		if trimmed != "" {
			writer.WriteField("keyterms[]", trimmed)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", elevenLabsSTTEndpoint, body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("xi-api-key", apiKey)

	// Backstop for a hung ElevenLabs connection. Sized to cover ~3 h meetings
	// at any plausible Scribe v2 processing ratio; ElevenLabs's own per-file
	// ceiling is 10 h / 3 GB, so this is a generous "is ElevenLabs alive" cap,
	// not a UX budget (the client controls UX wait).
	client := &http.Client{Timeout: 60 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("ElevenLabs API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response: %w", err)
	}

	var result ElevenLabsSTTResponse
	if err := common.Unmarshal(bodyBytes, &result); err != nil {
		return nil, nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var rawResponse map[string]interface{}
	if err := common.Unmarshal(bodyBytes, &rawResponse); err != nil {
		return nil, nil, fmt.Errorf("failed to decode raw response: %w", err)
	}

	return &result, rawResponse, nil
}

// wavDurationFromHeader computes a PCM WAV's exact duration from a range-read
// header prefix and the object's total size, without downloading the audio.
// The data-chunk size field is trusted when sane; streamed writers that leave
// it 0 (or the 0xFFFFFFFF placeholder) fall back to totalSize minus the data
// offset.
func wavDurationFromHeader(header []byte, totalSize int64) (float64, error) {
	if len(header) < 12 || string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, fmt.Errorf("not a RIFF/WAVE header")
	}
	var byteRate uint32
	haveFmt := false
	offset := int64(12)
	for offset+8 <= int64(len(header)) {
		chunkID := string(header[offset : offset+4])
		chunkSize := int64(binary.LittleEndian.Uint32(header[offset+4 : offset+8]))
		dataStart := offset + 8
		switch chunkID {
		case "fmt ":
			if dataStart+16 > int64(len(header)) {
				return 0, fmt.Errorf("truncated fmt chunk")
			}
			byteRate = binary.LittleEndian.Uint32(header[dataStart+8 : dataStart+12])
			haveFmt = true
		case "data":
			if !haveFmt || byteRate == 0 {
				return 0, fmt.Errorf("missing or invalid fmt chunk before data")
			}
			dataSize := chunkSize
			if dataSize == 0 || dataSize == 0xFFFFFFFF || dataStart+dataSize > totalSize {
				dataSize = totalSize - dataStart
			}
			if dataSize <= 0 {
				return 0, fmt.Errorf("empty data chunk")
			}
			return float64(dataSize) / float64(byteRate), nil
		}
		// Chunks are 16-bit aligned; odd sizes carry a pad byte.
		offset = dataStart + chunkSize + (chunkSize & 1)
	}
	return 0, fmt.Errorf("no data chunk in first %d header bytes", len(header))
}

// settleDurationSeconds picks the billed duration: an exact WAV-header
// measurement when available, else the transcript's last word-end timestamp
// (undercounts trailing silence — user-favorable), else the reserve's
// worst-case estimate.
//
// The WAV header is client-controlled (raw presigned PUT), so its measurement
// is bounded before it can drive billing: it may never exceed the size-based
// estimate the reserve was taken against (a forged/corrupt small byteRate
// cannot bill an astronomical duration), and it may never fall below the
// transcript's last word-end (a forged large byteRate cannot shrink hours of
// real audio to ~0 tokens). The word-end floor is applied last because the
// upstream ASR timestamps are ground truth — a genuine low-byte-rate WAV that
// legitimately runs past the estimate is restored by it.
func settleDurationSeconds(wavDurationS float64, words []ElevenLabsSTTWord, estimateS float64) float64 {
	var wordEnd float64
	if len(words) > 0 && words[len(words)-1].End > 0 {
		wordEnd = words[len(words)-1].End
	}
	if wavDurationS > 0 {
		if wavDurationS > estimateS {
			wavDurationS = estimateS
		}
		if wavDurationS < wordEnd {
			wavDurationS = wordEnd
		}
		return wavDurationS
	}
	if wordEnd > 0 {
		return wordEnd
	}
	return estimateS
}

func (r TranscribeRequest) EffectiveLanguageCode() string {
	if strings.TrimSpace(r.LanguageCode) != "" {
		return strings.TrimSpace(r.LanguageCode)
	}
	return strings.TrimSpace(r.Language)
}

func (r ElevenLabsSTTResponse) EffectiveLanguageCode(fallback string) string {
	if strings.TrimSpace(r.LanguageCode) != "" {
		return strings.TrimSpace(r.LanguageCode)
	}
	return strings.TrimSpace(fallback)
}

func sanitizeAudioFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return ""
	}
	filename = strings.ReplaceAll(filename, "\\", "/")
	filename = filepath.Base(filename)
	filename = strings.ReplaceAll(filename, "..", "")
	return strings.TrimSpace(filename)
}

func isOwnedAudioObjectKey(userID int, objectKey string) bool {
	objectKey = strings.TrimSpace(objectKey)
	if objectKey == "" {
		return false
	}
	return strings.HasPrefix(objectKey, fmt.Sprintf("audio/%d/", userID))
}

func effectiveUsingGroup(c *gin.Context) string {
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if group != "" {
		return group
	}
	group = common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
	if group != "" {
		return group
	}
	return c.GetString("group")
}

func buildTranscriptLines(words []ElevenLabsSTTWord, fallbackText string) []TranscriptLineResponse {
	lines := make([]TranscriptLineResponse, 0)
	if len(words) == 0 {
		fallbackText = strings.TrimSpace(fallbackText)
		if fallbackText == "" {
			return lines
		}
		isFinal := true
		lines = append(lines, TranscriptLineResponse{
			ID:        uuid.NewString(),
			Timestamp: 0,
			Text:      fallbackText,
			IsFinal:   &isFinal,
		})
		return lines
	}

	var currentSpeaker *string
	var currentText strings.Builder
	var currentStart uint64
	var currentEnd uint64
	currentLogprobs := make([]float64, 0)

	flush := func() {
		text := strings.TrimSpace(currentText.String())
		if text == "" {
			currentText.Reset()
			currentLogprobs = currentLogprobs[:0]
			return
		}
		line := TranscriptLineResponse{
			ID:        uuid.NewString(),
			Timestamp: currentStart,
			Text:      text,
		}
		if currentEnd > 0 {
			end := currentEnd
			line.EndTimestamp = &end
		}
		if currentSpeaker != nil && *currentSpeaker != "" {
			speaker := *currentSpeaker
			line.Speaker = &speaker
		}
		if len(currentLogprobs) > 0 {
			avg := 0.0
			for _, value := range currentLogprobs {
				avg += value
			}
			avg /= float64(len(currentLogprobs))
			confidence := float32(math.Exp(avg))
			line.Confidence = &confidence
		}
		isFinal := true
		line.IsFinal = &isFinal
		lines = append(lines, line)
		currentText.Reset()
		currentLogprobs = currentLogprobs[:0]
	}

	for _, word := range words {
		switch word.Type {
		case "spacing":
			if currentText.Len() > 0 {
				currentText.WriteString(word.Text)
			}
			continue
		case "", "word":
		default:
			continue
		}

		startMS := secondsToMilliseconds(word.Start)
		endMS := secondsToMilliseconds(word.End)

		var speakerPtr *string
		if trimmed := strings.TrimSpace(word.SpeakerID); trimmed != "" {
			speaker := trimmed
			speakerPtr = &speaker
		}

		if currentText.Len() > 0 && !sameSpeaker(currentSpeaker, speakerPtr) {
			flush()
		}

		if currentText.Len() == 0 {
			currentStart = startMS
			currentSpeaker = speakerPtr
		}
		currentText.WriteString(word.Text)
		currentEnd = endMS
		if word.Logprob != nil {
			currentLogprobs = append(currentLogprobs, *word.Logprob)
		}
	}

	flush()
	return lines
}

func sameSpeaker(left, right *string) bool {
	if left == nil && right == nil {
		return true
	}
	if left == nil || right == nil {
		return false
	}
	return *left == *right
}

func secondsToMilliseconds(value float64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value * 1000.0)
}
