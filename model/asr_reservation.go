package model

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// AsrReservation bridges the mint→settle HTTP gap for ElevenLabs realtime ASR.
// new-api's BillingSession is request-scoped (in-memory) but realtime ASR splits
// the reserve (POST /api/elevenlabs/token) and settle (POST /api/elevenlabs/usage)
// across two HTTP requests minutes apart, so the reservation amount must be
// persisted.
type AsrReservation struct {
	Id              int    `json:"id" gorm:"primaryKey"`
	UserId          int    `json:"user_id" gorm:"index"`
	TokenId         int    `json:"token_id" gorm:"index;default:0"`
	// TokenKey is intentionally hidden from JSON; it's stored so settle-time can
	// adjust the same token that paid the reserve (the user could mint new
	// tokens between reserve and settle).
	TokenKey        string `json:"-" gorm:"type:varchar(64);default:''"`
	ModelName       string `json:"model_name" gorm:"type:varchar(128);index"`
	UsingGroup      string `json:"using_group" gorm:"type:varchar(64);default:''"`
	ReservedQuota   int    `json:"reserved_quota" gorm:"default:0"`
	ReservedSeconds int    `json:"reserved_seconds" gorm:"default:0"`
	SettledQuota    int    `json:"settled_quota" gorm:"default:0"`
	SettledSeconds  int    `json:"settled_seconds" gorm:"default:0"`
	Status          string `json:"status" gorm:"type:varchar(16);index;default:'reserved'"`
	CreatedAt       int64  `json:"created_at" gorm:"bigint;index"`
	SettledAt       int64  `json:"settled_at" gorm:"bigint;default:0"`
}

// Reservation lifecycle. Every value fits the varchar(16) Status column, so
// adding the two new ones needs no migration on any of the three supported
// databases (Rule 2).
//
//	reserved ──claim──▶ settling ──billing applied──────────────▶ settled
//	                        │
//	                        ├──billing applied nothing──────────▶ reserved
//	                        │
//	                        └──funding applied, token counter ──▶ token_desync
//	                           did not
//
// "settling" exists so that the claim can happen *before* billing rather than
// after it. Settling used to be gated by a read of "reserved" followed later by
// a conditional update, which let two concurrent /api/elevenlabs/usage requests
// both pass the read and both bill; only one update then took effect and the
// loser's zero-row result was discarded, so both returned 200. Since the
// settlement applies a *delta* against the pre-consumed reserve, each extra
// racing request refunded a reserve that was only ever taken once — quota the
// user never paid for. Claiming first makes the losing request lose before it
// can spend anything.
//
// Only "reserved" is claimable, so a client can never re-report a reservation
// that is already settling, settled or token_desync. Recovery from the two
// non-success states is deliberately out-of-band (operator, or the sweeper
// TES2-21 will add), never something a racing client can trigger.
const (
	AsrReservationStatusReserved = "reserved"
	// AsrReservationStatusSettling means a settle request has claimed the row
	// and is billing against it. Transient — a row that stays here has a
	// settle whose outcome is unknown (the process died mid-billing, or the
	// terminal status write failed after the money moved). It must not be
	// auto-released: releasing a claim that may already have billed is exactly
	// the double-apply this status exists to prevent.
	AsrReservationStatusSettling = "settling"
	AsrReservationStatusSettled  = "settled"
	// AsrReservationStatusTokenDesync means billing got halfway: the funding
	// source (wallet or subscription) was adjusted in full, but the per-token
	// spending counter was not (service.ErrQuotaPartiallyApplied).
	//
	// The reservation's own job — bridging the mint→settle gap for the funding
	// — did complete, so this is NOT an under-charge and must never be
	// re-settled. Re-running it when the reported duration came in under the
	// 30-second reserve would refund a wallet that has already been refunded in
	// full, which is the same faucet this whole change closes, just in the
	// other direction. What needs repair is `tokens.remain_quota`/`used_quota`,
	// a secondary ledger, and that is a counter fix rather than a settlement.
	//
	// It gets its own status rather than being recorded as "settled" so the
	// TES2-22 stale-reservation monitor counts it as Unknown — visible as a
	// number that needs looking at — instead of it reading as a healthy
	// settlement. Naming matches the ASR_RESERVATION_TOKEN_QUOTA_DESYNC log
	// marker so a row and its log line can be correlated by grep.
	AsrReservationStatusTokenDesync = "token_desync"
)

// CreateAsrReservation persists a new reservation. CreatedAt is auto-populated
// when zero; Status defaults to "reserved".
func CreateAsrReservation(res *AsrReservation) error {
	if res == nil {
		return errors.New("AsrReservation is nil")
	}
	if res.CreatedAt == 0 {
		res.CreatedAt = common.GetTimestamp()
	}
	if res.Status == "" {
		res.Status = AsrReservationStatusReserved
	}
	return DB.Create(res).Error
}

// GetAsrReservationById returns the reservation only if it belongs to userId.
// Returns (nil, nil) when no matching row exists (or when it exists but is
// owned by a different user) — callers should treat this as "not found".
func GetAsrReservationById(id, userId int) (*AsrReservation, error) {
	if id <= 0 || userId <= 0 {
		return nil, nil
	}
	var res AsrReservation
	err := DB.Where("id = ? AND user_id = ?", id, userId).First(&res).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &res, nil
}

// transitionAsrReservation is the one conditional-update primitive behind every
// status change: a single UPDATE ... WHERE id = ? AND status = ? that carries
// its own precondition, so the check and the write cannot be separated by
// another request. Returns rows affected — zero means the row was not in the
// `from` state (someone else moved it, or it is gone).
//
// One statement rather than a read-then-write in a transaction because a bare
// SELECT does not lock the row on any of the three supported databases at their
// default isolation levels, and SELECT ... FOR UPDATE is not available on
// SQLite (Rule 2). A conditional UPDATE is atomic on all three.
func transitionAsrReservation(id int, from, to string, extra map[string]interface{}) (int64, error) {
	if id <= 0 {
		return 0, nil
	}
	updates := make(map[string]interface{}, len(extra)+1)
	for k, v := range extra {
		updates[k] = v
	}
	updates["status"] = to
	result := DB.Model(&AsrReservation{}).
		Where("id = ? AND status = ?", id, from).
		Updates(updates)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ClaimAsrReservationForSettle atomically transitions "reserved" → "settling",
// taking exclusive ownership of the settlement before any quota is applied.
// Reports whether this caller won the claim.
//
// This is the gate that makes settlement single-apply: of any number of
// concurrent /api/elevenlabs/usage requests for one reservation, exactly one
// UPDATE can match `status = 'reserved'`, so exactly one caller may go on to
// bill. A false result is not an error — it means another request owns the
// settle (or the row already reached a non-reserved state) and this one must
// bill nothing and report a conflict.
func ClaimAsrReservationForSettle(id int) (bool, error) {
	affected, err := transitionAsrReservation(id, AsrReservationStatusReserved, AsrReservationStatusSettling, nil)
	return affected == 1, err
}

// MarkAsrReservationSettled commits a claimed settlement: "settling" →
// "settled", capturing the quota and seconds actually billed.
//
// The precondition is "settling", not "reserved", so this can only ever commit
// a settlement whose claim this process won. A zero result therefore no longer
// means "someone else settled it concurrently" — the claim already excluded
// that — but "the row left `settling` without us", which is anomalous.
func MarkAsrReservationSettled(id, settledQuota, settledSeconds int) (int64, error) {
	return transitionAsrReservation(id, AsrReservationStatusSettling, AsrReservationStatusSettled, map[string]interface{}{
		"settled_quota":   settledQuota,
		"settled_seconds": settledSeconds,
		"settled_at":      common.GetTimestamp(),
	})
}

// ReleaseAsrReservationClaim returns a claimed row to "reserved" so the client
// can report again. Valid ONLY when billing is known to have applied nothing at
// all: the reserve is pre-consumed at mint time and settlement applies a delta
// against it, so a reservation abandoned mid-settle leaves the user charged the
// full 30-second floor for what may have been a two-second session. Handing the
// row back is what lets them recover that.
//
// The settled_* columns are deliberately untouched: nothing was billed, so
// there is nothing to record, and a later successful settle overwrites them
// with the real figures.
func ReleaseAsrReservationClaim(id int) (int64, error) {
	return transitionAsrReservation(id, AsrReservationStatusSettling, AsrReservationStatusReserved, nil)
}

// MarkAsrReservationTokenDesync parks a claimed row at "token_desync" after a
// partial billing failure. The row leaves the client's reach for good (only
// "reserved" is claimable) and keeps the figures the funding side actually
// moved, which are what a human needs to repair the token counter.
//
// Under ratio pricing the settled_* columns hold real applied figures, not an
// estimate: the funding adjustment completed on this branch, so `settled_quota`
// is the quota genuinely settled. The status records that the *token counter*
// did not follow, which is why no new column is needed to tell the two apart.
//
// Under `tiered_expr` pricing they are NOT trustworthy, and that is pre-existing
// rather than introduced here. The caller derives the figure with
// computeAudioQuotaFromPriceData, which reads ModelRatio/ModelPrice —
// modelPriceHelperTiered leaves both at zero, so it yields 0 against a nonzero
// charge. The same wrong value already reaches settled rows and the API response
// on that path. Reconcile such rows from the consume log, not from here, until
// PostAudioConsumeQuota reports the quota it actually charged.
func MarkAsrReservationTokenDesync(id, settledQuota, settledSeconds int) (int64, error) {
	return transitionAsrReservation(id, AsrReservationStatusSettling, AsrReservationStatusTokenDesync, map[string]interface{}{
		"settled_quota":   settledQuota,
		"settled_seconds": settledSeconds,
		"settled_at":      common.GetTimestamp(),
	})
}

// asrReservationWindow is the CreatedAt slice that both the stale count and its
// denominator are taken over. It is a value rather than a pair of durations so
// that the two counts cannot be computed from two different clock reads: a
// single tick of the Unix second between them would move both edges and let a
// boundary row land in one count but not the other, making the ratio — and, at
// the lower edge, even the invariant stale <= total — unsound.
type asrReservationWindow struct {
	from    int64
	to      int64
	hasFrom bool
}

// asrNowFn is a package-level seam (same pattern as callElevenLabsTokenAPIFn in
// the ElevenLabs handler) so a test can count clock reads. The single-read
// property below is otherwise untestable: two separate reads agree in almost
// every run, so a test that only compares the edges would pass against the
// broken implementation.
var asrNowFn = common.GetTimestamp

// errInvertedAsrWindow reports a window whose lower edge is at or above its
// upper edge, which can only come from a caller passing a lookback that does
// not exceed the staleness age. Every row is then outside the range and the
// caller would otherwise receive a plausible, entirely empty 0/0 sample rather
// than being told its bounds are unusable.
var errInvertedAsrWindow = errors.New("asr reservation window is inverted: lookback must exceed the staleness age")

// newAsrReservationWindow reads the clock exactly once and derives both edges
// from it:
//
//   - the upper edge (now - olderThanSeconds) drops sessions still young
//     enough to settle legitimately, which would otherwise be miscounted as
//     leaked;
//   - the lower edge (now - lookbackSeconds) keeps the ratio a rolling rate
//     rather than an all-time average that drifts and hides regressions.
//     A non-positive lookbackSeconds means "no lower edge".
func newAsrReservationWindow(olderThanSeconds, lookbackSeconds int64) asrReservationWindow {
	if olderThanSeconds < 0 {
		olderThanSeconds = 0
	}
	now := asrNowFn()
	w := asrReservationWindow{to: now - olderThanSeconds}
	if lookbackSeconds > 0 {
		w.from = now - lookbackSeconds
		w.hasFrom = true
	}
	return w
}

// countByStatus returns the per-status row counts for the window in ONE
// statement. Two separate COUNTs would each see their own snapshot: a late
// /api/elevenlabs/usage settling an in-window row between them would be read as
// "reserved" by the numerator and as "settled" by the denominator, reporting a
// leak rate that was never true of any single database state. A single grouped
// statement is atomic on all three supported databases, so both terms always
// describe the same snapshot.
//
// GROUP BY on an indexed, non-reserved column keeps this portable — no CASE,
// no SUM (whose return type differs across MySQL/PostgreSQL), no raw SQL.
func (w asrReservationWindow) countByStatus(ctx context.Context) (map[string]int64, error) {
	query := DB.WithContext(ctx).Model(&AsrReservation{}).Where("created_at < ?", w.to)
	if w.hasFrom {
		query = query.Where("created_at >= ?", w.from)
	}
	// Status is scanned as NullString because the column is nullable: an
	// explicit SQL NULL from a migration, import or manual repair would fail a
	// plain string scan ("converting NULL to string is unsupported") and take
	// the whole sample down every hour. One malformed row must land in Unknown,
	// not retire the metric.
	var rows []struct {
		Status sql.NullString
		C      int64
	}
	if err := query.Select("status, COUNT(*) AS c").Group("status").Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(rows))
	for _, r := range rows {
		// Normalise before bucketing, and accumulate rather than assign. MySQL's
		// default collation is case-insensitive and PAD SPACE, so "reserved",
		// "RESERVED" and "reserved " can collapse into one group there with a
		// non-deterministic label, while PostgreSQL and SQLite return them
		// separately. Folding case and trimming space on this side makes the
		// classification identical on all three (Rule 2); += is then required
		// because several groups can now fold to the same key.
		//
		// This converges the differences a malformed row can realistically
		// carry, not every equivalence a collation can define — an
		// accent-insensitive collation would still merge "réserved" on MySQL
		// only. Replicating collation semantics in Go is unbounded, and the
		// deployed database is PostgreSQL (binary comparison), so the residue is
		// documented rather than chased.
		counts[strings.TrimSpace(strings.ToLower(r.Status.String))] += r.C
	}
	return counts, nil
}

// AsrReservationStaleRate is one sample of the leak rate: the terms, not the
// percentage, so the caller decides how to render it and no precision is lost
// before it reaches the log.
//
// Unknown is broken out rather than folded into Total because a row whose
// status is neither "reserved" nor "settled" has an *undetermined* disposition
// — it is not evidence of a healthy session, and silently leaving it in the
// denominator would dilute the rate towards zero exactly when something has
// gone wrong. It is also forward-looking: if the TES2-21 sweeper later
// introduces a third terminal status, these rows surface as a number to
// investigate instead of quietly reading as successful settlements.
type AsrReservationStaleRate struct {
	Stale   int64
	Total   int64
	Unknown int64
}

// CountAsrReservationStaleRate returns, from one window computed off a single
// clock read and one database snapshot, how many reservations are still sitting
// at "reserved" (stale) and how many exist in total regardless of status.
//
// Stale rows are sessions that minted a token but never POSTed
// /api/elevenlabs/usage, so the 30s floor charge became the permanent charge —
// the leak this makes observable. Both values are returned together, rather
// than from two exported counters, because they are only meaningful as a ratio
// and a caller taking them over two separate windows or two separate snapshots
// would silently produce a wrong percentage.
//
// ctx bounds the query: the caller is a long-lived ticker, and an unbounded
// wait on a stalled driver would silently retire the monitor for the rest of
// the process lifetime during exactly the incident it exists to surface.
//
// Both `status` and `created_at` are already indexed, so no schema change is
// needed.
func CountAsrReservationStaleRate(ctx context.Context, olderThanSeconds, lookbackSeconds int64) (AsrReservationStaleRate, error) {
	w := newAsrReservationWindow(olderThanSeconds, lookbackSeconds)
	if w.hasFrom && w.from >= w.to {
		// Fail loud rather than return an empty-looking sample: an inverted
		// window excludes every row, so 0 stale of 0 total would read as "no
		// leak" when it actually means "these bounds cannot match anything".
		return AsrReservationStaleRate{}, errInvertedAsrWindow
	}
	counts, err := w.countByStatus(ctx)
	if err != nil {
		return AsrReservationStaleRate{}, err
	}
	var rate AsrReservationStaleRate
	for status, c := range counts {
		rate.Total += c
		switch status {
		case AsrReservationStatusReserved:
			rate.Stale += c
		case AsrReservationStatusSettled:
		default:
			rate.Unknown += c
		}
	}
	return rate, nil
}
