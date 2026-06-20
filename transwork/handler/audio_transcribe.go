package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"

	gcsstorage "cloud.google.com/go/storage"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
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

	gcsClient, err := twstorage.GetGCSClient()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "GCS client not initialized"})
		return
	}

	bucketName := twstorage.GetGCSBucketName()
	if bucketName == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "GCS bucket not configured"})
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
	uploadURL, err := gcsClient.Bucket(bucketName).SignedURL(objectKey, &gcsstorage.SignedURLOptions{
		Method:      "PUT",
		Expires:     time.Now().Add(15 * time.Minute),
		ContentType: req.ContentType,
	})
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

	gcsClient, err := twstorage.GetGCSClient()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "GCS client not initialized"})
		return
	}

	bucketName := twstorage.GetGCSBucketName()
	if bucketName == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "GCS bucket not configured"})
		return
	}

	group := effectiveUsingGroup(c)
	if group == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No token group available for channel selection"})
		return
	}

	channel, err := model.GetChannel(group, batchTranscriptionModel, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channel: " + err.Error()})
		return
	}
	if channel == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("No available channel for model: %s", batchTranscriptionModel)})
		return
	}
	if channel.Key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Channel not configured properly"})
		return
	}

	audioBytes, fileName, contentType, err := readAudioObjectFromGCS(gcsClient.Bucket(bucketName), req.ObjectKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read audio object"})
		return
	}

	// Reserve quota up-front based on a worst-case PCM upper bound
	// (bytes / (16kHz × 2 bytes/sample)). On success the actual duration
	// from common.GetAudioDuration drives the settle delta; on error the
	// deferred Refund returns the reserve.
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		OriginModelName: batchTranscriptionModel,
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
	estDurationS := float64(len(audioBytes)) / 32000.0
	estTokens := int(math.Ceil(estDurationS / 60.0 * 1000.0))
	priceData, priceErr := helper.ModelPriceHelper(c, info, estTokens, &types.TokenCountMeta{})
	if priceErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ASR pricing not configured: " + priceErr.Error()})
		return
	}
	if apiErr := service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info); apiErr != nil {
		c.JSON(apiErr.StatusCode, gin.H{"error": apiErr.Error()})
		return
	}
	var settled bool
	defer func() {
		if !settled && info.Billing != nil {
			info.Billing.Refund(c)
		}
	}()

	transcript, rawResponse, err := callElevenLabsSTT(
		channel.Key,
		fileName,
		contentType,
		audioBytes,
		req.EffectiveLanguageCode(),
		req.Keyterms,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transcription failed: " + err.Error()})
		return
	}

	// Measure actual duration to compute the settle delta. Fall back to the
	// last word's end timestamp if the audio parser doesn't recognize the
	// format, and to the estimated upper bound as a last resort.
	durSec, durErr := common.GetAudioDuration(
		c.Request.Context(), bytes.NewReader(audioBytes), strings.ToLower(filepath.Ext(fileName)))
	if durErr != nil && len(transcript.Words) > 0 {
		durSec = transcript.Words[len(transcript.Words)-1].End
	}
	if durSec <= 0 {
		durSec = estDurationS
	}
	actualTokens := int(math.Ceil(durSec / 60.0 * 1000.0))
	usage := &dto.Usage{
		CompletionTokens:       actualTokens,
		TotalTokens:            actualTokens,
		CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: actualTokens},
	}
	service.PostAudioConsumeQuota(c, info, usage, fmt.Sprintf("ASR batch %.2fs", durSec))
	settled = true

	c.JSON(http.StatusOK, TranscribeResponse{
		Text:         transcript.Text,
		LanguageCode: transcript.EffectiveLanguageCode(req.EffectiveLanguageCode()),
		ModelName:    batchTranscriptionModel,
		Lines:        buildTranscriptLines(transcript.Words, transcript.Text),
		Words:        transcript.Words,
		RawResponse:  rawResponse,
	})
}

func callElevenLabsSTT(apiKey, fileName, contentType string, audioBytes []byte, languageCode string, keyterms []string) (*ElevenLabsSTTResponse, map[string]interface{}, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	writer.WriteField("model_id", batchTranscriptionModel)
	writer.WriteField("diarize", "true")
	writer.WriteField("timestamps_granularity", "word")
	writer.WriteField("no_verbatim", "true")
	if languageCode != "" {
		writer.WriteField("language_code", languageCode)
	}
	for _, keyterm := range keyterms {
		trimmed := strings.TrimSpace(keyterm)
		if trimmed != "" {
			writer.WriteField("keyterms[]", trimmed)
		}
	}

	filePartHeader := make(textproto.MIMEHeader)
	filePartHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartFilename(fileName)))
	filePartHeader.Set("Content-Type", contentType)
	filePart, err := writer.CreatePart(filePartHeader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create file part: %w", err)
	}
	if _, err := filePart.Write(audioBytes); err != nil {
		return nil, nil, fmt.Errorf("failed to write audio bytes: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.elevenlabs.io/v1/speech-to-text", body)
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
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var rawResponse map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawResponse); err != nil {
		return nil, nil, fmt.Errorf("failed to decode raw response: %w", err)
	}

	return &result, rawResponse, nil
}

func readAudioObjectFromGCS(bucket *gcsstorage.BucketHandle, objectKey string) ([]byte, string, string, error) {
	ctx := context.Background()
	object := bucket.Object(objectKey)

	attrs, err := object.Attrs(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to read object attrs: %w", err)
	}

	reader, err := object.NewReader(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to create object reader: %w", err)
	}
	defer reader.Close()

	audioBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to read object bytes: %w", err)
	}

	fileName := filepath.Base(objectKey)
	if fileName == "." || fileName == "/" || fileName == "" {
		fileName = "recording.wav"
	}

	contentType := strings.TrimSpace(attrs.ContentType)
	if contentType == "" {
		contentType = inferAudioContentType(fileName)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return audioBytes, fileName, contentType, nil
}

func inferAudioContentType(fileName string) string {
	if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName))); contentType != "" {
		return contentType
	}
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4", ".m4a":
		return "audio/mp4"
	case ".webm":
		return "audio/webm"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".aac":
		return "audio/aac"
	case ".amr":
		return "audio/amr"
	default:
		return ""
	}
}

func escapeMultipartFilename(fileName string) string {
	fileName = strings.ReplaceAll(fileName, "\\", "\\\\")
	return strings.ReplaceAll(fileName, `"`, `\"`)
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
