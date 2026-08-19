package transwork

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	twhandler "github.com/QuantumNous/new-api/transwork/handler"
)

// asrStaleReservationAge is the age past which a still-"reserved" ASR row is
// counted as stale: the wall-clock cap on a settleable session
// (asrRealtimeMaxSeconds in transwork/handler/elevenlabs.go) plus a reporting
// grace period.
//
// The cap alone is not enough. Settlement is a separate HTTP request that by
// definition happens after the realtime connection ends, so a legitimate
// six-hour session is still reporting at 6h+1s — it would be counted as leaked
// until its POST landed, and network retries widen that gap. The grace covers
// the report itself rather than the session, so a counted row is genuinely past
// every legitimate flow rather than merely past the session cap.
const asrStaleReservationAge = asrRealtimeSessionCap + asrSettlementReportGrace

// asrRealtimeSessionCap is the handler's own cap on a settleable session, not a
// copy of it. Deriving it means raising the cap (say to 8h for longer dictation)
// cannot silently leave this monitor counting live sessions as leaks — the
// failure a duplicated literal would hide, since a test comparing two copies of
// the same stale number still passes.
const asrRealtimeSessionCap = time.Duration(twhandler.AsrRealtimeMaxSeconds) * time.Second

// asrSettlementReportGrace is how long after a session's wall-clock cap we
// still expect a late /api/elevenlabs/usage to arrive. Generous on purpose:
// over-waiting only delays a leak showing up in the rate, while under-waiting
// permanently books healthy sessions as leaks and biases the number the
// TES2-21 decision will be made on.
const asrSettlementReportGrace = 30 * time.Minute

// asrStaleReservationLookback bounds how far back the rate is computed. The
// question this answers is "what fraction of realtime ASR sessions never
// settled this week?", so both the stale count and its denominator are taken
// over the trailing week; an all-time average would drift and hide a
// regression.
const asrStaleReservationLookback = 7 * 24 * time.Hour

// asrStaleReservationInterval is how often the count is logged. This is a
// daily-numbers signal, not a metrics stack (see TES2-17), so hourly is enough.
const asrStaleReservationInterval = time.Hour

// asrStaleReservationQueryTimeout bounds a single poll. The ticker loop is
// sequential, so without a deadline one stalled query (a networked MySQL or
// PostgreSQL outage that hangs rather than returning an error) would block this
// goroutine for the rest of the process lifetime — silently retiring the
// monitor during exactly the kind of incident it exists to make visible. On
// timeout the poll gives up and the next tick retries.
const asrStaleReservationQueryTimeout = 30 * time.Second

// StartAsrStaleReservationMonitor logs, on a timer, how many realtime ASR
// reservations were never settled — sessions that minted a token but never
// POSTed /api/elevenlabs/usage, leaving the 30s reserve floor as the permanent
// charge. The sweeper that would actually settle them is TES2-21 and is
// deliberately deferred; this only makes the leak rate observable so that
// deferral can be re-evaluated on evidence.
//
// Master-node only, so a future multi-node deploy does not log the same number
// N times. Logs once immediately so a deploy can be verified without waiting
// out the first interval, and logs even when the count is zero (a missing line
// means the monitor is not running, which is itself the thing to alert on).
func StartAsrStaleReservationMonitor() {
	if !common.IsMasterNode {
		return
	}
	go func() {
		logStaleAsrReservations()
		ticker := time.NewTicker(asrStaleReservationInterval)
		defer ticker.Stop()
		for range ticker.C {
			logStaleAsrReservations()
		}
	}()
}

func logStaleAsrReservations() {
	olderThan := int64(asrStaleReservationAge / time.Second)
	lookback := int64(asrStaleReservationLookback / time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), asrStaleReservationQueryTimeout)
	defer cancel()

	// Both terms come back from one window computed from a single clock read
	// and one database snapshot, so the percentage always describes a state the
	// database was actually in.
	rate, err := model.CountAsrReservationStaleRate(ctx, olderThan, lookback)
	if err != nil {
		common.SysError("ASR_STALE_RESERVATIONS_QUERY_FAILED error=" + err.Error())
		return
	}
	// count and total are the authoritative pair — pct is a convenience and is
	// rounded, so anything alerting on an exact threshold should key on the two
	// integers. unknown is normally 0; a nonzero value means rows in neither
	// terminal state are diluting the denominator and should be investigated.
	// lookback_days, not window_days: the sampled range is bounded at BOTH ends
	// (now-lookback up to now-staleness age), so its true width is the lookback
	// minus the staleness age. Calling the horizon a window invited a reader to
	// derive a daily rate from a duration the sample does not cover; the two
	// edges are both on the line, so the exact width stays recoverable.
	common.SysLog(fmt.Sprintf("ASR_STALE_RESERVATIONS count=%d total=%d unknown=%d pct=%.4f lookback_days=%d older_than_minutes=%d",
		rate.Stale, rate.Total, rate.Unknown, staleRatePct(rate.Stale, rate.Total),
		int64(asrStaleReservationLookback/(24*time.Hour)), int64(asrStaleReservationAge/time.Minute)))
}

// staleRatePct is the leaked-session percentage. An empty window is a real
// state (no realtime ASR traffic yet), not an error, so it reports 0 rather
// than dividing by zero — the accompanying total=0 in the log line is what
// distinguishes "no traffic" from "no leak".
func staleRatePct(count, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(count) / float64(total) * 100
}
