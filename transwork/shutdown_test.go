package transwork

import (
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// startTestServer boots an http.Server on a random loopback port and returns it
// alongside its base URL.
func startTestServer(t *testing.T, handler http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String()
}

// A deploy must not truncate a response that is already being written, and the
// billing flush must land after that response finishes. Flushing first would
// miss whatever the in-flight request bills on its way out — precisely the loss
// this shutdown path exists to prevent, so the ordering is the assertion.
func TestShutdownDrainsInFlightRequestThenFlushes(t *testing.T) {
	var handlerFinished atomic.Bool
	entered := make(chan struct{})

	srv, base := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "complete")
		handlerFinished.Store(true)
	}))

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		resCh <- result{body: string(b), err: err}
	}()

	<-entered // shut down only once the request is genuinely in flight

	// Record the state at the FIRST flush and never overwrite it. Storing on
	// every call would let a premature flush be masked by a later correct one,
	// which makes the ordering assertion vacuous.
	var flushCount atomic.Int32
	var handlerDoneAtFirstFlush atomic.Bool
	shutdown(srv, 5*time.Second, 0, func() {
		if flushCount.Add(1) == 1 {
			handlerDoneAtFirstFlush.Store(handlerFinished.Load())
		}
	})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", res.err)
	}
	if res.body != "complete" {
		t.Errorf("in-flight response was truncated: got %q, want %q", res.body, "complete")
	}
	if got := flushCount.Load(); got != 1 {
		t.Errorf("flush called %d times, want exactly 1", got)
	}
	if !handlerDoneAtFirstFlush.Load() {
		t.Error("flush ran before the in-flight request finished; its billing tail would be lost")
	}
}

// A drained HTTP server is not a quiesced application. A failing relay request
// hands its refund to gopool.Go and returns, so Shutdown reports the request
// drained while the refund is still on its way to the batch stores. Flushing the
// instant the drain returns would read those stores just before the write lands
// and exit with the refund lost, so the flush must not run until the settle
// window has elapsed.
func TestShutdownWaitsForAsyncBillingBeforeFlushing(t *testing.T) {
	srv, _ := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	const settle = 300 * time.Millisecond
	start := time.Now()
	var flushedAfter time.Duration
	shutdown(srv, time.Second, settle, func() { flushedAfter = time.Since(start) })

	if flushedAfter < settle {
		t.Errorf("flush ran %v into shutdown, inside the %v settle window; a refund goroutine spawned by a failing request could still have been in flight",
			flushedAfter, settle)
	}
}

// The drain is bounded on purpose: Shutdown closes the listener the moment it is
// called, so an unbounded wait keeps the door shut for new users for as long as
// one slow request lasts. Past the deadline we stop waiting and let the process
// exit take that request — but the flush must still run, or the deadline would
// quietly reintroduce the data loss the drain was added to stop.
func TestShutdownStopsAtDeadlineAndStillFlushes(t *testing.T) {
	entered := make(chan struct{})
	srv, base := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(3 * time.Second)
	}))

	go func() {
		resp, err := http.Get(base + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-entered

	var flushCalled atomic.Bool
	start := time.Now()
	shutdown(srv, 150*time.Millisecond, 0, func() { flushCalled.Store(true) })
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("shutdown waited %v for a stuck request; the drain deadline is not bounding it", elapsed)
	}
	if !flushCalled.Load() {
		t.Error("flush was skipped when the drain deadline was hit; buffered billing would be lost")
	}
}
