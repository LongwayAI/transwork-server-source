package transwork

import (
	"testing"
	"time"

	twhandler "github.com/QuantumNous/new-api/transwork/handler"
)

// A reservation must not be called stale the instant its session could no
// longer be running: settlement is a separate HTTP request that by definition
// happens after the connection ends, so the threshold has to cover the report
// as well as the session. Without the grace, a legitimate six-hour session is
// booked as a leak from 6h+1s until its POST lands, biasing the very number the
// TES2-21 defer/fix decision rests on.
func TestStaleThresholdLeavesRoomForTheUsageReport(t *testing.T) {
	// Read from the handler rather than restating "6h" here: a local literal
	// would keep agreeing with the monitor's own copy after the real cap moved,
	// so the test would pass while production regressed.
	sessionCap := time.Duration(twhandler.AsrRealtimeMaxSeconds) * time.Second

	if asrStaleReservationAge <= sessionCap {
		t.Fatalf("stale threshold %v must exceed the session cap %v so a late usage report is not counted as a leak",
			asrStaleReservationAge, sessionCap)
	}
	if got := asrStaleReservationAge - sessionCap; got != asrSettlementReportGrace {
		t.Fatalf("grace beyond the session cap = %v, want %v", got, asrSettlementReportGrace)
	}
	// The window must still be bounded by the lookback, or the rate stops
	// being a rolling one.
	if asrStaleReservationAge >= asrStaleReservationLookback {
		t.Fatalf("stale threshold %v must stay inside the lookback %v, else the window is empty",
			asrStaleReservationAge, asrStaleReservationLookback)
	}
}

// An empty window (no realtime ASR traffic yet) must not divide by zero, and
// must not be reported as anything other than 0% — a NaN or a spurious rate
// here would make the ASR_STALE_RESERVATIONS line unalertable.
func TestStaleRatePct(t *testing.T) {
	cases := []struct {
		name  string
		count int64
		total int64
		want  float64
	}{
		{"empty window does not divide by zero", 0, 0, 0},
		{"stale rows with an empty total is still not a division", 3, 0, 0},
		{"negative total is treated as empty", 1, -1, 0},
		{"nothing leaked", 0, 40, 0},
		{"half leaked", 1, 2, 50},
		{"everything leaked", 7, 7, 100},
		{"the 5% revisit threshold from TES2-21", 1, 20, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := staleRatePct(tc.count, tc.total)
			if got != tc.want {
				t.Fatalf("staleRatePct(%d, %d) = %v, want %v", tc.count, tc.total, got, tc.want)
			}
		})
	}
}
