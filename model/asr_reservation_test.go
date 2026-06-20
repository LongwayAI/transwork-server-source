package model

import (
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

func TestMarkAsrReservationSettled_OnlyAffectsReservedRows(t *testing.T) {
	truncateTables(t)

	res := &AsrReservation{
		UserId:          11,
		ModelName:       "scribe_v2_realtime",
		ReservedQuota:   500,
		ReservedSeconds: 30,
	}
	require.NoError(t, CreateAsrReservation(res))

	// First settle: 1 row affected, status transitions to settled.
	affected, err := MarkAsrReservationSettled(res.Id, 1000, 60)
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected, "first settle should affect exactly one row")

	got, err := GetAsrReservationById(res.Id, 11)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AsrReservationStatusSettled, got.Status)
	assert.Equal(t, 1000, got.SettledQuota)
	assert.Equal(t, 60, got.SettledSeconds)
	assert.NotZero(t, got.SettledAt)

	// Second settle (idempotency / 409 detection): zero rows affected because
	// status is no longer "reserved".
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
