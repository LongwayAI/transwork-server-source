package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	twstorage "github.com/QuantumNous/new-api/transwork/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildWAVHeader constructs a canonical 44-byte PCM WAV header for tests.
func buildWAVHeader(t *testing.T, sampleRate uint32, channels, bitsPerSample uint16, dataSize uint32) []byte {
	t.Helper()
	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(36+dataSize)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(16)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(1))) // PCM
	require.NoError(t, binary.Write(buf, binary.LittleEndian, channels))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, sampleRate))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, byteRate))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(uint32(channels)*uint32(bitsPerSample)/8)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, bitsPerSample))
	buf.WriteString("data")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, dataSize))
	return buf.Bytes()
}

func TestWavDurationFromHeader_Mono16k(t *testing.T) {
	// The meeting-finish path: client-built 16 kHz mono 16-bit WAV. 320000
	// data bytes at 32000 B/s must bill exactly 10 seconds.
	header := buildWAVHeader(t, 16000, 1, 16, 320000)
	dur, err := wavDurationFromHeader(header, 44+320000)
	require.NoError(t, err)
	assert.InDelta(t, 10.0, dur, 0.001)
}

func TestWavDurationFromHeader_Stereo44kImport(t *testing.T) {
	// An imported 44.1 kHz stereo WAV: byte rate is 176400, not 32000. A naive
	// size/32000 settle would overbill ~5.5x; the header parser must not.
	header := buildWAVHeader(t, 44100, 2, 16, 1764000)
	dur, err := wavDurationFromHeader(header, 44+1764000)
	require.NoError(t, err)
	assert.InDelta(t, 10.0, dur, 0.001)
}

func TestWavDurationFromHeader_ZeroDataSizeFallsBackToObjectSize(t *testing.T) {
	// Streamed writers sometimes leave the data-chunk size field 0; the object
	// size minus the data offset is then the only truthful measure.
	header := buildWAVHeader(t, 16000, 1, 16, 0)
	dur, err := wavDurationFromHeader(header, 44+320000)
	require.NoError(t, err)
	assert.InDelta(t, 10.0, dur, 0.001)
}

func TestWavDurationFromHeader_InvalidHeader(t *testing.T) {
	_, err := wavDurationFromHeader([]byte("this is not a wav file at all"), 1000)
	assert.Error(t, err)
	_, err = wavDurationFromHeader([]byte("RIFF"), 1000)
	assert.Error(t, err)
}

func TestSettleDurationSeconds_WavHeaderWins(t *testing.T) {
	// Exact WAV duration must take precedence: last-word-end undercounts
	// trailing silence, and the estimate is a worst-case upper bound.
	words := []ElevenLabsSTTWord{{Text: "hi", Start: 0.1, End: 9.9}}
	got := settleDurationSeconds(10.5, words, 12.0)
	assert.Equal(t, 10.5, got)
}

func TestSettleDurationSeconds_WordsFallback(t *testing.T) {
	// Non-WAV uploads (imports, voice webm) settle on the ASR response's last
	// word-end timestamp — slightly user-favorable, never over the estimate.
	words := []ElevenLabsSTTWord{{Text: "hi", Start: 0.1, End: 9.9}}
	got := settleDurationSeconds(0, words, 12.0)
	assert.Equal(t, 9.9, got)
}

func TestSettleDurationSeconds_EstimateLastResort(t *testing.T) {
	// No WAV duration and an empty transcript: fall back to the reserve's
	// upper bound so a failed measurement can never bill below zero work.
	got := settleDurationSeconds(0, nil, 12.0)
	assert.Equal(t, 12.0, got)
}

func TestSettleDurationSeconds_ForgedShortHeaderFlooredAtWords(t *testing.T) {
	// Billing-evasion guard: the client controls the WAV header end-to-end
	// (raw presigned PUT), so a forged fmt byteRate can shrink the computed
	// duration to ~0 for hours of real audio. The transcript's last word-end is
	// upstream ground truth — the audio cannot be shorter than the words ASR
	// returned — so the header must never bill below it.
	words := []ElevenLabsSTTWord{{Text: "end", Start: 3599.0, End: 3600.0}}
	got := settleDurationSeconds(0.03, words, 4000.0)
	assert.Equal(t, 3600.0, got)
}

func TestSettleDurationSeconds_CorruptLongHeaderCappedAtEstimate(t *testing.T) {
	// The opposite forgery/corruption: a tiny byteRate inflates the computed
	// duration astronomically. It must never exceed the size-based estimate the
	// reserve was taken against, so a corrupt header cannot bill 68 years.
	words := []ElevenLabsSTTWord{{Text: "hi", Start: 0.1, End: 5.0}}
	got := settleDurationSeconds(999999.0, words, 12.0)
	assert.Equal(t, 12.0, got)
}

func TestSettleDurationSeconds_LowByteRateRescuedByWordsFloor(t *testing.T) {
	// A legitimate low-byte-rate WAV (e.g. 8 kHz) genuinely runs longer than the
	// size/32000 estimate. The estimate ceiling would underbill it, but the
	// word-end floor restores the true duration — the floor wins over the
	// ceiling because the transcript is ground truth.
	words := []ElevenLabsSTTWord{{Text: "end", Start: 19.0, End: 19.5}}
	got := settleDurationSeconds(20.0, words, 10.0)
	assert.Equal(t, 19.5, got)
}

func TestCallElevenLabsSTT_SendsSourceURLNotFile(t *testing.T) {
	// DEV-405: the server must send a presigned URL, not audio bytes. The
	// request keeps the exact production field set so transcription quality
	// and word timestamps (used for billing) are unchanged.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(1<<20))
		assert.Equal(t, "test-key", r.Header.Get("xi-api-key"))
		assert.Equal(t, "https://example.com/audio.wav?sig=abc", r.FormValue("source_url"))
		assert.Equal(t, "scribe_v2", r.FormValue("model_id"))
		assert.Equal(t, "true", r.FormValue("diarize"))
		assert.Equal(t, "word", r.FormValue("timestamps_granularity"))
		assert.Equal(t, "true", r.FormValue("no_verbatim"))
		assert.Equal(t, "en", r.FormValue("language_code"))
		assert.Equal(t, []string{"Gressio"}, r.MultipartForm.Value["keyterms[]"])
		assert.Empty(t, r.MultipartForm.File, "must not send a file part")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"hi","language_code":"eng","words":[{"text":"hi","start":0.1,"end":0.5,"type":"word"}]}`)
	}))
	defer srv.Close()

	prev := elevenLabsSTTEndpoint
	elevenLabsSTTEndpoint = srv.URL
	defer func() { elevenLabsSTTEndpoint = prev }()

	transcript, raw, err := callElevenLabsSTT("test-key", "https://example.com/audio.wav?sig=abc", "en", []string{" Gressio "})
	require.NoError(t, err)
	assert.Equal(t, "hi", transcript.Text)
	require.Len(t, transcript.Words, 1)
	assert.Equal(t, 0.5, transcript.Words[0].End)
	assert.NotNil(t, raw)
}

func TestAcquireTranscribeSlot_QueuesThenRejectsGracefully(t *testing.T) {
	// DEV-404 bottleneck #2: overlapping finishes must queue briefly and then
	// be rejected cleanly instead of piling onto the upstream unbounded.
	releases := make([]func(), 0, transcribeMaxConcurrent)
	for i := 0; i < transcribeMaxConcurrent; i++ {
		release, err := acquireTranscribeSlot(context.Background(), 50*time.Millisecond)
		require.NoError(t, err, "slot %d must be granted", i)
		releases = append(releases, release)
	}

	_, err := acquireTranscribeSlot(context.Background(), 50*time.Millisecond)
	require.Error(t, err, "slot beyond the cap must be rejected after the wait")

	releases[0]()
	release, err := acquireTranscribeSlot(context.Background(), 50*time.Millisecond)
	require.NoError(t, err, "a released slot must be grantable again")
	release()
	for _, r := range releases[1:] {
		r()
	}
}

// fakeObjectStore is an in-memory twstorage.ObjectStore for handler tests.
type fakeObject struct {
	data        []byte
	contentType string
	created     time.Time
}

type fakeObjectStore struct {
	objects map[string]*fakeObject
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string]*fakeObject{}}
}

func (s *fakeObjectStore) SignedUploadURL(objectKey, contentType string, ttl time.Duration) (string, error) {
	return "https://fake.example/upload/" + objectKey, nil
}

func (s *fakeObjectStore) SignedDownloadURL(objectKey string, ttl time.Duration) (string, error) {
	return "https://fake.example/download/" + objectKey, nil
}

func (s *fakeObjectStore) Attrs(ctx context.Context, objectKey string) (*twstorage.ObjectAttrs, error) {
	obj, ok := s.objects[objectKey]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", objectKey)
	}
	return &twstorage.ObjectAttrs{
		Size:        int64(len(obj.data)),
		ContentType: obj.contentType,
		Created:     obj.created,
	}, nil
}

func (s *fakeObjectStore) Read(ctx context.Context, objectKey string) ([]byte, error) {
	obj, ok := s.objects[objectKey]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", objectKey)
	}
	return obj.data, nil
}

func (s *fakeObjectStore) ReadRange(ctx context.Context, objectKey string, offset, length int64) ([]byte, error) {
	obj, ok := s.objects[objectKey]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", objectKey)
	}
	if offset >= int64(len(obj.data)) {
		return nil, nil
	}
	end := offset + length
	if end > int64(len(obj.data)) {
		end = int64(len(obj.data))
	}
	return obj.data[offset:end], nil
}

func (s *fakeObjectStore) Write(ctx context.Context, objectKey, contentType string, data []byte) error {
	s.objects[objectKey] = &fakeObject{data: data, contentType: contentType, created: time.Now()}
	return nil
}

func TestTranscribeCacheRoundTripViaObjectStore(t *testing.T) {
	// DEV-540 idempotency must survive the ObjectStore refactor: a cached
	// transcript written after a billed run is returned to a retry unbilled.
	store := newFakeObjectStore()
	resp := &TranscribeResponse{Text: "hello", ModelName: "scribe_v2"}
	require.NoError(t, writeCachedTranscript(store, "audio/1/x.wav", resp))

	got, ok := readCachedTranscript(store, "audio/1/x.wav")
	require.True(t, ok, "fresh cache must hit")
	assert.Equal(t, "hello", got.Text)
	assert.Equal(t, "scribe_v2", got.ModelName)
}

func TestReadCachedTranscript_StaleEntryMisses(t *testing.T) {
	// A genuinely re-used object key must be re-transcribed once the cache
	// TTL passes — stale hits would serve the wrong meeting's transcript.
	store := newFakeObjectStore()
	require.NoError(t, writeCachedTranscript(store, "audio/1/y.wav", &TranscribeResponse{Text: "old"}))
	store.objects[transcriptCacheObjectKey("audio/1/y.wav")].created = time.Now().Add(-transcriptCacheTTL - time.Minute)

	_, ok := readCachedTranscript(store, "audio/1/y.wav")
	assert.False(t, ok, "stale cache must miss")
}
