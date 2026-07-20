package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	twstorage "github.com/QuantumNous/new-api/transwork/storage"
	"golang.org/x/sync/singleflight"
)

// DEV-540: /api/transcribe must be idempotent per object_key. A successful
// transcription is cached as a sidecar JSON object next to the audio in
// object storage, and concurrent requests for the same object_key share one
// transcription via singleflight — so a client retry after a lost response is
// never billed twice.
//
// The sidecar key is server-only writable: GenerateAudioUploadURL whitelists
// audio file extensions, so a signed upload URL can never target a
// *.transcript.json key and a client cannot forge a cached transcript.

const (
	transcriptCacheSuffix = ".transcript.json"
	// Retries after a lost response arrive within seconds; a few hours of
	// cache is generous while still letting a genuinely re-used object_key be
	// re-transcribed eventually.
	transcriptCacheTTL = 6 * time.Hour
)

// transcribeFlight dedupes in-flight transcriptions per object_key. In the
// DEV-539 failure the client connection resets seconds into a 1–2 minute
// transcription, so the retry usually arrives while the first run is still
// processing (i.e. before the sidecar cache exists). The duplicate caller
// waits for and shares the initiator's result — and its single billing
// settle — instead of starting a second billed transcription.
var transcribeFlight singleflight.Group

// transcribeError carries the HTTP status a transcription failure maps to, so
// the singleflight core can report errors without holding a gin context.
type transcribeError struct {
	status  int
	message string
}

func (e *transcribeError) Error() string { return e.message }

func newTranscribeError(status int, message string) *transcribeError {
	return &transcribeError{status: status, message: message}
}

func transcribeErrorStatus(err error) (int, string) {
	var te *transcribeError
	if errors.As(err, &te) {
		return te.status, te.message
	}
	return http.StatusInternalServerError, err.Error()
}

func transcriptCacheObjectKey(objectKey string) string {
	return objectKey + transcriptCacheSuffix
}

func transcriptCacheFresh(created, now time.Time) bool {
	if created.IsZero() {
		return false
	}
	return now.Sub(created) <= transcriptCacheTTL
}

func readCachedTranscript(store twstorage.ObjectStore, objectKey string) (*TranscribeResponse, bool) {
	ctx := context.Background()
	cacheKey := transcriptCacheObjectKey(objectKey)
	attrs, err := store.Attrs(ctx, cacheKey)
	if err != nil {
		return nil, false
	}
	if !transcriptCacheFresh(attrs.Created, time.Now()) {
		return nil, false
	}
	data, err := store.Read(ctx, cacheKey)
	if err != nil {
		return nil, false
	}
	var resp TranscribeResponse
	if err := common.Unmarshal(data, &resp); err != nil {
		return nil, false
	}
	return &resp, true
}

func writeCachedTranscript(store twstorage.ObjectStore, objectKey string, resp *TranscribeResponse) error {
	data, err := common.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal transcript cache: %w", err)
	}
	if err := store.Write(context.Background(), transcriptCacheObjectKey(objectKey), "application/json", data); err != nil {
		return fmt.Errorf("failed to write transcript cache: %w", err)
	}
	return nil
}
