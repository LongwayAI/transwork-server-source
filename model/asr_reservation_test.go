package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAsrReservation_DefaultsAndTimestamp(t *testing.T) {
	truncateTables(t)

	before := time.Now().Unix()
	res := &AsrReservation{
		UserId:          501,
		TokenId:         77,
		TokenKey:        "tk-key-abc",
		ModelName:       "scribe_v2_realtime",
		UsingGroup:      "default",
		ReservedQuota:   100,
		ReservedSeconds: 30,
	}
	require.NoError(t, CreateAsrReservation(res))
	require.NotZero(t, res.Id, "Id should be assigned by GORM after insert")
	assert.GreaterOrEqual(t, res.CreatedAt, before, "CreatedAt should default to now when zero")
	assert.Equal(t, AsrReservationStatusReserved, res.Status, "Status should default to reserved")

	// Verify the row persists with the same defaults via a round-trip read.
	got, err := GetAsrReservationById(res.Id, res.UserId)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusReserved, got.Status)
	assert.Equal(t, 100, got.ReservedQuota)
	assert.Equal(t, 30, got.ReservedSeconds)
	assert.Equal(t, 0, got.SettledQuota)
	assert.Equal(t, int64(0), got.SettledAt)
	// TokenKey is stored so settle-time can adjust the same token that paid.
	assert.Equal(t, "tk-key-abc", got.TokenKey)
}

func TestGetAsrReservationById_OwnerScoped(t *testing.T) {
	truncateTables(t)

	res := &AsrReservation{
		UserId:        7,
		ModelName:     "scribe_v2_realtime",
		ReservedQuota: 10,
	}
	require.NoError(t, CreateAsrReservation(res))

	// Wrong user — must return (nil, nil) so handler can map to 404.
	got, err := GetAsrReservationById(res.Id, 999)
	require.NoError(t, err)
	assert.Nil(t, got, "wrong user must NOT see the reservation")

	// Correct user.
	got, err = GetAsrReservationById(res.Id, 7)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 7, got.UserId)

	// Missing id — must return (nil, nil), not an error.
	got, err = GetAsrReservationById(123456, 7)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMarkAsrReservationSettled_OnlyAffectsSettlingRows(t *testing.T) {
	truncateTables(t)

	res := &AsrReservation{
		UserId:          11,
		ModelName:       "scribe_v2_realtime",
		ReservedQuota:   500,
		ReservedSeconds: 30,
	}
	require.NoError(t, CreateAsrReservation(res))

	// A freshly reserved row cannot be settled: settlement now commits a claim
	// rather than racing for one, so a caller that never claimed must not be
	// able to write a terminal status over a live reservation.
	affected, err := MarkAsrReservationSettled(res.Id, 1000, 60)
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "settling an unclaimed row must affect zero rows")

	got, err := GetAsrReservationById(res.Id, 11)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusReserved, got.Status, "unclaimed row must stay reserved")
	assert.Equal(t, 0, got.SettledQuota, "a rejected settle must not write figures")

	claimed, err := ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	require.True(t, claimed)

	// First settle after the claim: 1 row affected, status transitions to settled.
	affected, err = MarkAsrReservationSettled(res.Id, 1000, 60)
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected, "first settle should affect exactly one row")

	got, err = GetAsrReservationById(res.Id, 11)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusSettled, got.Status)
	assert.Equal(t, 1000, got.SettledQuota)
	assert.Equal(t, 60, got.SettledSeconds)
	assert.NotZero(t, got.SettledAt)

	// Second settle: zero rows affected because status is no longer "settling".
	affected, err = MarkAsrReservationSettled(res.Id, 9999, 9999)
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "settling an already-settled row must affect zero rows")

	// Confirm the original settled values were NOT overwritten.
	got, err = GetAsrReservationById(res.Id, 11)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1000, got.SettledQuota, "second settle must not overwrite")
	assert.Equal(t, 60, got.SettledSeconds, "second settle must not overwrite")
}

// ---------------------------------------------------------------------------
// Settle claim — the gate that makes settlement single-apply
// ---------------------------------------------------------------------------

// claimAndSettle drives a fixture row to "settled" through the real two-step
// path. Fixtures must not write the terminal status directly: settlement is now
// only reachable from "settling", so a shortcut would set up a state production
// can never produce and quietly stop testing the transition it depends on.
func claimAndSettle(t *testing.T, id, settledQuota, settledSeconds int) {
	t.Helper()
	claimed, err := ClaimAsrReservationForSettle(id)
	require.NoError(t, err)
	require.True(t, claimed, "fixture row must be claimable")
	affected, err := MarkAsrReservationSettled(id, settledQuota, settledSeconds)
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)
}

func newClaimableReservation(t *testing.T, userId int) *AsrReservation {
	t.Helper()
	res := &AsrReservation{
		UserId:          userId,
		ModelName:       "scribe_v2_realtime",
		ReservedQuota:   1500,
		ReservedSeconds: 30,
	}
	require.NoError(t, CreateAsrReservation(res))
	return res
}

// TestClaimAsrReservationForSettle_OnlyReservedIsClaimable pins the invariant
// the whole design rests on: "reserved" is the ONLY status a client-driven
// settle can take ownership of. If any other status were claimable, a second
// /usage report could bill a reservation whose quota has already been applied
// (or refunded), which is the quota-minting bug itself.
func TestClaimAsrReservationForSettle_OnlyReservedIsClaimable(t *testing.T) {
	truncateTables(t)

	for _, status := range []string{
		AsrReservationStatusSettling,
		AsrReservationStatusSettled,
		AsrReservationStatusTokenDesync,
	} {
		t.Run(status, func(t *testing.T) {
			res := newClaimableReservation(t, 31)
			require.NoError(t, DB.Model(&AsrReservation{}).
				Where("id = ?", res.Id).Update("status", status).Error)

			claimed, err := ClaimAsrReservationForSettle(res.Id)
			require.NoError(t, err)
			assert.False(t, claimed, "%q must not be claimable by a settle request", status)

			got, err := GetAsrReservationById(res.Id, 31)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, status, got.Status, "a refused claim must not move the row")
		})
	}

	// The positive case, so the assertions above cannot pass merely because
	// claiming never works at all.
	res := newClaimableReservation(t, 31)
	claimed, err := ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	require.True(t, claimed, "a reserved row must be claimable")

	got, err := GetAsrReservationById(res.Id, 31)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusSettling, got.Status)
}

// TestClaimAsrReservationForSettle_ConcurrentSingleWinner is the model-level
// half of the TES2-27 regression: under real concurrency exactly one caller may
// take the claim. Each extra winner would be a request permitted to bill —
// and, for a zero-duration report, to refund a 30-second reserve that was only
// ever taken once.
//
// Runs unsynchronised on purpose (the handler test forces the interleaving
// instead): here the point is that the database, not Go-side sequencing, is
// what serialises the claim.
func TestClaimAsrReservationForSettle_ConcurrentSingleWinner(t *testing.T) {
	truncateTables(t)

	res := newClaimableReservation(t, 32)

	const goroutines = 8
	claims := make([]bool, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start // release all at once to maximise real overlap
			claims[idx], errs[idx] = ClaimAsrReservationForSettle(res.Id)
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i, c := range claims {
		require.NoError(t, errs[i])
		if c {
			won++
		}
	}
	assert.Equal(t, 1, won, "exactly one caller may claim the settlement")

	got, err := GetAsrReservationById(res.Id, 32)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusSettling, got.Status)
}

// TestReleaseAsrReservationClaim_ReturnsRowToReserved covers the branch taken
// when billing applied nothing at all. The row must become claimable again:
// the reserve is pre-consumed at mint time, so a reservation abandoned
// mid-settle leaves the user charged the full 30-second floor, and reporting
// again is how they get the difference back.
func TestReleaseAsrReservationClaim_ReturnsRowToReserved(t *testing.T) {
	truncateTables(t)

	res := newClaimableReservation(t, 33)
	claimed, err := ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	require.True(t, claimed)

	affected, err := ReleaseAsrReservationClaim(res.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected)

	got, err := GetAsrReservationById(res.Id, 33)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusReserved, got.Status)
	assert.Equal(t, 0, got.SettledQuota, "a release must not record a settlement")
	assert.Equal(t, int64(0), got.SettledAt, "a release must not record a settlement")

	// Released means genuinely re-claimable, not merely relabelled.
	claimed, err = ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	assert.True(t, claimed, "a released row must be claimable again")
}

// TestReleaseAsrReservationClaim_RefusesSettledRow guards the direction that
// costs money: a released terminal row could be re-settled, and for an
// under-reserve duration that means refunding a wallet that was already
// refunded in full.
func TestReleaseAsrReservationClaim_RefusesSettledRow(t *testing.T) {
	truncateTables(t)

	res := newClaimableReservation(t, 34)
	claimed, err := ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	require.True(t, claimed)
	affected, err := MarkAsrReservationSettled(res.Id, 900, 20)
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	affected, err = ReleaseAsrReservationClaim(res.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "a settled row must never be released back to reserved")

	got, err := GetAsrReservationById(res.Id, 34)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusSettled, got.Status)
	assert.Equal(t, 900, got.SettledQuota, "release must not disturb the settled figures")
}

// TestMarkAsrReservationTokenDesync_IsUnclaimableAndCountedUnknown covers the
// partial-billing branch: the funding side completed, only the per-token
// counter did not. The row must be unreachable by any further settle (a
// re-settle would double-apply the funding delta), and — because it is NOT a
// healthy settlement — it must not read as one to the stale-reservation
// monitor.
func TestMarkAsrReservationTokenDesync_IsUnclaimableAndCountedUnknown(t *testing.T) {
	truncateTables(t)

	res := newClaimableReservation(t, 35)
	res.CreatedAt = time.Now().Unix() - 7*3600
	require.NoError(t, DB.Model(&AsrReservation{}).
		Where("id = ?", res.Id).Update("created_at", res.CreatedAt).Error)

	claimed, err := ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	require.True(t, claimed)

	affected, err := MarkAsrReservationTokenDesync(res.Id, 501, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	got, err := GetAsrReservationById(res.Id, 35)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusTokenDesync, got.Status)
	// The funding figures that DID apply are kept, so the token counter can be
	// repaired from the row rather than reconstructed from logs.
	assert.Equal(t, 501, got.SettledQuota)
	assert.Equal(t, 10, got.SettledSeconds)

	claimed, err = ClaimAsrReservationForSettle(res.Id)
	require.NoError(t, err)
	assert.False(t, claimed, "a desynced row must never be re-settled")

	// Visible to the TES2-22 monitor as something to investigate rather than as
	// a healthy settlement — the blind spot that hiding it inside "settled"
	// would recreate.
	rate, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 24*3600)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rate.Unknown, "token_desync must land in the Unknown bucket")
	assert.Equal(t, int64(0), rate.Stale, "token_desync is not a never-reported session")
	assert.Equal(t, int64(1), rate.Total)
}

// TestCountStaleAsrReservations_OnlyOldReservedRows pins the definition of
// "leaked" that the monitor reports on: a row counts only if it is BOTH still
// unsettled AND older than the threshold. Counting settled rows would inflate
// the leak number with sessions that billed correctly; counting fresh reserved
// rows would inflate it with sessions still legitimately in flight.
func TestCountStaleAsrReservations_OnlyOldReservedRows(t *testing.T) {
	truncateTables(t)

	now := time.Now().Unix()

	// Old + reserved: the leak we want counted.
	stale := &AsrReservation{UserId: 21, ModelName: "scribe_v2_realtime", ReservedQuota: 1500, CreatedAt: now - 7*3600}
	require.NoError(t, CreateAsrReservation(stale))

	// Old but settled: billed correctly, must NOT count.
	settledOld := &AsrReservation{UserId: 22, ModelName: "scribe_v2_realtime", ReservedQuota: 1500, CreatedAt: now - 7*3600}
	require.NoError(t, CreateAsrReservation(settledOld))
	claimAndSettle(t, settledOld.Id, 3000, 60)

	// Fresh + reserved: session may still be in flight, must NOT count.
	fresh := &AsrReservation{UserId: 23, ModelName: "scribe_v2_realtime", ReservedQuota: 1500, CreatedAt: now - 60}
	require.NoError(t, CreateAsrReservation(fresh))

	// lookback 0 == no lower edge, so this isolates the age/status predicates.
	rate, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rate.Stale, "only the old unsettled row is stale")
	assert.Zero(t, rate.Unknown, "fixture has only the two valid statuses")

	// Widening the threshold past the row's age drops it back out of the count.
	rate, err = CountAsrReservationStaleRate(context.Background(), 24*3600, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), rate.Stale, "a 7h-old row is not stale under a 24h threshold")

	// A zero threshold counts every unsettled row regardless of age.
	rate, err = CountAsrReservationStaleRate(context.Background(), 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), rate.Stale, "zero threshold counts both unsettled rows")
}

// TestAsrReservationCounts_RatioOverSharedWindow pins the leaked-session rate
// the monitor reports. The numerator and denominator must be taken over the
// SAME window and differ ONLY by status, otherwise the percentage is nonsense:
// a denominator over a wider window understates the leak, and a numerator that
// ignores status or the age bound overstates it. The fixture is built so that
// getting either the window or the status set wrong changes the numbers.
func TestAsrReservationCounts_RatioOverSharedWindow(t *testing.T) {
	truncateTables(t)

	now := time.Now().Unix()
	const olderThan = int64(6 * 3600)     // upper edge: old enough to be judged
	const lookback = int64(7 * 24 * 3600) // lower edge: trailing week

	newRow := func(userId int, createdAt int64) *AsrReservation {
		res := &AsrReservation{UserId: userId, ModelName: "scribe_v2_realtime", ReservedQuota: 1500, CreatedAt: createdAt}
		require.NoError(t, CreateAsrReservation(res))
		return res
	}

	// In window, never settled — the leak. Numerator AND denominator.
	newRow(31, now-2*24*3600)

	// In window, settled correctly — denominator ONLY. If the numerator forgot
	// its status filter, stale would be 2 and the rate would read 100%.
	settled := newRow(32, now-2*24*3600)
	claimAndSettle(t, settled.Id, 3000, 60)

	// Too young to judge (1h old, under the 6h edge) — the session may still be
	// in flight. Excluded from BOTH; if the upper edge were dropped it would
	// land in both and skew the rate.
	newRow(33, now-3600)

	// Older than the lookback (30 days) and never settled. Excluded from BOTH;
	// if the lower edge were dropped it would inflate both terms with ancient
	// history and make the rate un-actionable.
	newRow(34, now-30*24*3600)

	rate, err := CountAsrReservationStaleRate(context.Background(), olderThan, lookback)
	require.NoError(t, err)

	assert.Equal(t, int64(1), rate.Stale, "only the in-window unsettled row is stale")
	assert.Equal(t, int64(2), rate.Total, "denominator is every in-window row regardless of status")
	require.NotZero(t, rate.Total)
	assert.InDelta(t, 50.0, float64(rate.Stale)/float64(rate.Total)*100, 0.001, "1 of 2 judged sessions leaked")

	// The denominator must not silently ignore status *and* window: widening
	// only the lookback pulls the 30-day-old row in and changes both terms.
	rateWide, err := CountAsrReservationStaleRate(context.Background(), olderThan, 365*24*3600)
	require.NoError(t, err)
	assert.Equal(t, int64(2), rateWide.Stale, "a wider lookback must pull in the 30-day-old unsettled row")
	assert.Equal(t, int64(3), rateWide.Total, "a wider lookback must widen the denominator too")
}

// TestAsrReservationWindow_SingleClockRead is the regression test for the
// review finding that both counts used to read the clock independently: if the
// Unix second ticked between them, a row sitting exactly on an edge could land
// in one count but not the other, breaking the stale <= total invariant the
// percentage depends on. Both edges must therefore derive from one timestamp.
func TestAsrReservationWindow_SingleClockRead(t *testing.T) {
	const olderThan = int64(6 * 3600)
	const lookback = int64(7 * 24 * 3600)

	// A fake clock that advances a full second on EVERY read. Comparing the two
	// edges alone cannot catch a second read — in production both reads land in
	// the same Unix second almost always, so the broken implementation passed
	// such a test on nearly every run. Counting the reads, and making any extra
	// read visibly move the clock, is what actually pins the invariant.
	reads := 0
	prev := asrNowFn
	asrNowFn = func() int64 {
		reads++
		return 1_700_000_000 + int64(reads)
	}
	t.Cleanup(func() { asrNowFn = prev })

	w := newAsrReservationWindow(olderThan, lookback)
	require.True(t, w.hasFrom, "a positive lookback must produce a lower edge")
	assert.Equal(t, 1, reads, "the window must be built from exactly one clock read")
	// With a clock that moves on every read, this separation only survives if
	// both edges came from the same value.
	assert.Equal(t, lookback-olderThan, w.to-w.from,
		"edges must be derived from a single clock read")

	// A non-positive lookback means no lower edge rather than an empty window.
	unbounded := newAsrReservationWindow(olderThan, 0)
	assert.False(t, unbounded.hasFrom, "lookback<=0 must mean no lower edge")
}

// A negative age must be clamped to 0 rather than pushing the upper edge into
// the future, where it would count rows that do not exist yet.
//
// The clock is pinned rather than advancing here, and the edge is asserted
// exactly. Comparing against a freshly read clock cannot work: an unclamped
// edge is "now + 1", and a second read of an advancing clock returns that same
// later value, so the assertion would hold precisely when the clamp is missing.
func TestAsrReservationWindow_ClampsNegativeAge(t *testing.T) {
	const fixedNow = int64(1_700_000_000)

	prev := asrNowFn
	asrNowFn = func() int64 { return fixedNow }
	t.Cleanup(func() { asrNowFn = prev })

	clamped := newAsrReservationWindow(-1, 0)
	assert.Equal(t, fixedNow, clamped.to,
		"a negative age must clamp to 0, leaving the upper edge at now")

	// A positive age still moves the edge back, so the clamp is not just
	// pinning every input to now.
	normal := newAsrReservationWindow(3600, 0)
	assert.Equal(t, fixedNow-3600, normal.to)
}

// A window whose lookback does not exceed the staleness age matches no row at
// all. Returning 0/0 would read as "no leak"; the caller has to be told its
// bounds are unusable instead.
func TestCountAsrReservationStaleRate_RejectsInvertedWindow(t *testing.T) {
	truncateTables(t)

	res := &AsrReservation{UserId: 71, ModelName: "scribe_v2_realtime", CreatedAt: time.Now().Unix() - 2*24*3600}
	require.NoError(t, CreateAsrReservation(res))

	// lookback (1h) <= staleness age (6h): from >= to.
	_, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 3600)
	require.Error(t, err, "an inverted window must not be reported as an empty sample")
	assert.ErrorIs(t, err, errInvertedAsrWindow)

	// The production ordering (age well inside the lookback) still works.
	rate, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 7*24*3600)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rate.Stale)
}

// Rows the application cannot produce but the database can still hold must not
// take the metric down or be misfiled. A NULL status fails a plain string scan
// outright ("converting NULL to string is unsupported"), which would suppress
// every hourly sample rather than surfacing one bad row; a case-variant status
// is grouped case-insensitively by MySQL's default collation but separately by
// PostgreSQL and SQLite, so the classification has to be folded on this side to
// be identical on all three (Rule 2).
func TestCountAsrReservationStaleRate_ToleratesNullAndCaseVariantStatus(t *testing.T) {
	truncateTables(t)

	createdAt := time.Now().Unix() - 2*24*3600
	reserved := &AsrReservation{UserId: 81, ModelName: "scribe_v2_realtime", CreatedAt: createdAt}
	require.NoError(t, CreateAsrReservation(reserved))

	// Written out of band, exactly as a migration or manual repair would.
	require.NoError(t, DB.Exec(
		"INSERT INTO asr_reservations (user_id, model_name, status, created_at) VALUES (?, ?, NULL, ?)",
		82, "scribe_v2_realtime", createdAt).Error)
	require.NoError(t, DB.Exec(
		"INSERT INTO asr_reservations (user_id, model_name, status, created_at) VALUES (?, ?, ?, ?)",
		83, "scribe_v2_realtime", "RESERVED", createdAt).Error)

	rate, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 7*24*3600)
	require.NoError(t, err, "a NULL status must not fail the whole sample")
	assert.Equal(t, int64(3), rate.Total)
	// The case variant folds into "reserved" on every database rather than
	// landing in a different bucket depending on the engine's collation.
	assert.Equal(t, int64(2), rate.Stale, "case-variant reserved rows must classify identically across databases")
	assert.Equal(t, int64(1), rate.Unknown, "the NULL-status row is undetermined, not healthy")
}

// TestCountAsrReservationStaleRate_StaleNeverExceedsTotal guards the invariant
// that makes the reported percentage meaningful. stale is a status-narrowed
// subset of total over the identical window, so it can never be larger — a
// pct above 100 would mean the two terms had drifted apart.
func TestCountAsrReservationStaleRate_StaleNeverExceedsTotal(t *testing.T) {
	truncateTables(t)

	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		res := &AsrReservation{UserId: 40 + i, ModelName: "scribe_v2_realtime", ReservedQuota: 1500, CreatedAt: now - 2*24*3600}
		require.NoError(t, CreateAsrReservation(res))
	}

	rate, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 7*24*3600)
	require.NoError(t, err)
	assert.LessOrEqual(t, rate.Stale, rate.Total, "stale must be a subset of total")
	assert.Equal(t, int64(5), rate.Stale)
	assert.Equal(t, int64(5), rate.Total)
}

// TestCountAsrReservationStaleRate_UnknownStatusIsNotSilentlyHealthy pins the
// handling of rows in neither terminal state. Folding them into the denominator
// as if they had settled would dilute the leak rate towards zero exactly when
// something is wrong — 1 reserved row among 99 unknown ones would read as a 1%
// leak when in truth 99 sessions have an undetermined disposition.
func TestCountAsrReservationStaleRate_UnknownStatusIsNotSilentlyHealthy(t *testing.T) {
	truncateTables(t)

	now := time.Now().Unix()
	createdAt := now - 2*24*3600

	reserved := &AsrReservation{UserId: 61, ModelName: "scribe_v2_realtime", CreatedAt: createdAt}
	require.NoError(t, CreateAsrReservation(reserved))

	settled := &AsrReservation{UserId: 62, ModelName: "scribe_v2_realtime", CreatedAt: createdAt}
	require.NoError(t, CreateAsrReservation(settled))
	claimAndSettle(t, settled.Id, 3000, 60)

	// A status neither the handler nor the model writes today — a future
	// terminal state (the TES2-21 sweeper) or a bad migration.
	odd := &AsrReservation{UserId: 63, ModelName: "scribe_v2_realtime", CreatedAt: createdAt, Status: "swept"}
	require.NoError(t, CreateAsrReservation(odd))

	rate, err := CountAsrReservationStaleRate(context.Background(), 6*3600, 7*24*3600)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rate.Stale, "only the reserved row is stale")
	assert.Equal(t, int64(3), rate.Total, "total still counts every row in the window")
	assert.Equal(t, int64(1), rate.Unknown,
		"a row in neither terminal state must surface rather than read as settled")
}

// TestCountAsrReservationStaleRate_HonoursContext proves the context is
// actually plumbed through to the driver rather than accepted and ignored. The
// monitor is a long-lived ticker whose only protection against a stalled
// database connection is this deadline, so a ctx that does not reach the query
// would be worse than none — it would look like the hang was guarded.
func TestCountAsrReservationStaleRate_HonoursContext(t *testing.T) {
	truncateTables(t)

	res := &AsrReservation{UserId: 51, ModelName: "scribe_v2_realtime", CreatedAt: time.Now().Unix() - 2*24*3600}
	require.NoError(t, CreateAsrReservation(res))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the query starts

	_, err := CountAsrReservationStaleRate(ctx, 6*3600, 7*24*3600)
	require.Error(t, err, "a cancelled context must abort the query, not be ignored")
	assert.ErrorIs(t, err, context.Canceled)
}
