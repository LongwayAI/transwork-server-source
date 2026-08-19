package transwork

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// defaultDrainSeconds bounds how long shutdown waits for in-flight requests.
//
// Deliberately short. http.Server.Shutdown closes the listener the instant it is
// called, so on the current single-container deploy every second spent draining
// is a second in which *new* requests are refused. A long window would trade a
// brief total outage for a longer partial one.
//
// This is a ceiling, not a wait: the drain ends as soon as the last in-flight
// request finishes, which for a deploy with nothing long running is well under a
// second. So this value does not affect the typical deploy at all — it only
// bounds the worst case, and the only requests it can cut are ones that would
// have needed longer than it to finish.
//
// 15s rather than something longer because the population it protects is small
// and shrinking: anything still running after 15s is a long relay stream, and
// those exceed any window short enough to be acceptable here (STREAMING_TIMEOUT
// alone defaults to 120s). Paying a worst case of 15s+settle on every deploy to
// occasionally rescue one such stream is a bad trade while a single container
// owns the listener.
//
// Tune with SHUTDOWN_DRAIN_TIMEOUT — a config change and a restart, no rebuild —
// and keep the compose default in sync (grace must exceed drain + settle). Worth
// raising once blue/green lands: there the incoming traffic has already moved to
// the new container, so the old one draining slowly costs nobody anything.
const defaultDrainSeconds = 15

// settleDelay is a short pause between the drain and the final flush, to let
// billing writes that a handler spawned asynchronously reach the batch stores
// before we read them.
//
// Draining the HTTP server is not the same as quiescing billing. A failed relay
// request refunds its pre-charge via gopool.Go and returns immediately
// (service.BillingSession.Refund), so Shutdown can report the request drained
// while the refund goroutine is still on its way to model.IncreaseTokenQuota.
// Flushing the instant the drain returns would read the stores just before those
// writes arrive, and the process would exit with a user's refund lost.
//
// This is a mitigation, not a guarantee: it shrinks the window rather than
// closing it, and it does nothing for the task poller, which writes counters on
// its own schedule with no relationship to HTTP at all. Closing it properly
// needs every async billing writer registered and joined — see the KNOWN GAP
// note on ListenAndServeGracefully.
const settleDelay = 2 * time.Second

func drainTimeout() time.Duration {
	return time.Duration(common.GetEnvOrDefault("SHUTDOWN_DRAIN_TIMEOUT", defaultDrainSeconds)) * time.Second
}

// ListenAndServeGracefully serves handler on port until SIGTERM/SIGINT, then
// drains in-flight requests and flushes buffered billing counters before
// returning. It replaces gin's Engine.Run, which has no signal handling at all:
// under Engine.Run the process dies the instant Docker sends SIGTERM, severing
// in-flight responses and dropping whatever the batch updater had buffered since
// its last tick (up to BATCH_UPDATE_INTERVAL seconds of quota — real money).
//
// Returns non-nil only when the listener itself fails; a clean shutdown returns
// nil so the caller does not mistake it for a startup error.
//
// KNOWN GAP — a drained HTTP server is not a quiesced application. Two classes
// of billing write outlive the drain:
//
//   - Asynchronous writers spawned by a handler. service.BillingSession.Refund
//     hands its refund to gopool.Go and returns, so the request counts as drained
//     while the write is still in flight. settleDelay narrows this window; it
//     does not close it.
//   - Writers with no HTTP relationship at all. The task poller settles task
//     billing on its own schedule (service/task_billing.go calls
//     model.UpdateUserUsedQuotaAndRequestCount / UpdateChannelUsedQuota), and
//     nothing here stops or waits for it.
//
// Both predate this function — under Engine.Run the whole buffer died on every
// deploy, so this is strictly less lossy, not newly lossy. Closing them needs a
// registry of in-flight billing writers to join before the final flush, tracked
// separately rather than widened into upstream service code here.
//
// KNOWN GAP — hijacked connections are not drained. http.Server.Shutdown neither
// closes nor waits for connections taken over via Hijack, which includes the
// realtime WebSocket relay (controller/relay.go upgrades with a websocket
// Upgrader). Such a session is still severed at process exit, before
// relay/websocket.go reaches service.PostWssConsumeQuota, so its final usage can
// go unbilled. That predates this function — the previous Engine.Run cut those
// sessions too — and closing it needs a registry of live upgraded connections to
// wait on, which is deliberately left to a follow-up rather than widened into
// upstream relay code here.
func ListenAndServeGracefully(handler http.Handler, port string) error {
	srv := &http.Server{Addr: ":" + port, Handler: handler}

	listenErr := make(chan error, 1)
	go func() {
		// ErrServerClosed is the expected result of our own Shutdown call, not a
		// failure, so it must not reach the caller as a startup error.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-listenErr:
		return err
	case sig := <-quit:
		common.SysLog("received " + sig.String() + ", draining in-flight requests")
	}

	shutdown(srv, drainTimeout(), settleDelay, model.FlushBatchUpdates)
	return nil
}

// shutdown drains srv, waits settle for asynchronous billing writes to land,
// then flushes. flush is a parameter rather than a direct call to
// model.FlushBatchUpdates so the drain-settle-flush ordering — the whole point
// of this function — can be asserted in a test.
func shutdown(srv *http.Server, timeout, settle time.Duration, flush func()) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		// Shutdown does not force-close still-active connections; it stops
		// waiting for them. They die with the process a moment later.
		common.SysError("drain deadline exceeded, requests still in flight will be dropped on exit: " + err.Error())
	} else {
		common.SysLog("in-flight requests drained")
	}

	// Let refunds and other writes a handler spawned on its way out reach the
	// batch stores before we read them. See settleDelay.
	time.Sleep(settle)

	// Flush *after* the drain, never before: requests finishing above are still
	// writing into the batch buffer on their way out, so an early flush would
	// leave that tail unwritten — the exact loss this path exists to prevent.
	// It runs even when the deadline was hit, otherwise the deadline would
	// simply reintroduce the loss.
	flush()
	common.SysLog("shutdown complete")
}
