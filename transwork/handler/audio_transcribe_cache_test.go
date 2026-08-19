package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The cache must expire: a stale sidecar suppressing billing forever would let
// an object_key be re-transcribed for free indefinitely, while a retry seconds
// after a lost response (the DEV-540 scenario) must always hit the cache.
func TestTranscriptCacheFresh(t *testing.T) {
	now := time.Now()
	if !transcriptCacheFresh(now.Add(-10*time.Second), now) {
		t.Error("cache written seconds ago (retry window) must be fresh")
	}
	if transcriptCacheFresh(now.Add(-transcriptCacheTTL-time.Minute), now) {
		t.Error("cache older than TTL must be stale")
	}
	if transcriptCacheFresh(time.Time{}, now) {
		t.Error("zero created time must be treated as stale")
	}
}

// Billing errors (e.g. 403 insufficient quota from PreConsumeBilling) must
// keep their original status through the singleflight boundary so the client
// can distinguish "top up" from "server broke".
func TestTranscribeErrorStatus(t *testing.T) {
	status, msg := transcribeErrorStatus(newTranscribeError(http.StatusForbidden, "insufficient quota"))
	if status != http.StatusForbidden || msg != "insufficient quota" {
		t.Errorf("expected 403/insufficient quota, got %d/%s", status, msg)
	}
	status, msg = transcribeErrorStatus(errors.New("boom"))
	if status != http.StatusInternalServerError || msg != "boom" {
		t.Errorf("generic error must map to 500, got %d/%s", status, msg)
	}
}

// The sidecar key must be un-forgeable by clients: GenerateAudioUploadURL only
// signs keys with whitelisted audio extensions, so the sidecar must carry an
// extension a signed upload URL can never target.
func TestTranscriptCacheObjectKeyNotUploadable(t *testing.T) {
	key := transcriptCacheObjectKey("audio/42/uuid_recording.wav")
	if !strings.HasSuffix(key, ".transcript.json") {
		t.Fatalf("unexpected sidecar key: %s", key)
	}
	validExtensions := []string{".wav", ".mp3", ".mp4", ".m4a", ".webm", ".ogg", ".oga", ".flac", ".aac", ".amr"}
	for _, ext := range validExtensions {
		if strings.HasSuffix(key, ext) {
			t.Errorf("sidecar key ends with uploadable extension %s — clients could forge cached transcripts", ext)
		}
	}
}
