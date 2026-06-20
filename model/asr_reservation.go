package model

import (
	"errors"

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

const (
	AsrReservationStatusReserved = "reserved"
	AsrReservationStatusSettled  = "settled"
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

// MarkAsrReservationSettled atomically transitions a row from "reserved" to
// "settled", capturing the settled quota/seconds. Returns the number of rows
// affected; callers can use a zero result to detect already-settled
// (concurrent /usage call) and respond 409.
func MarkAsrReservationSettled(id, settledQuota, settledSeconds int) (int64, error) {
	if id <= 0 {
		return 0, nil
	}
	result := DB.Model(&AsrReservation{}).
		Where("id = ? AND status = ?", id, AsrReservationStatusReserved).
		Updates(map[string]interface{}{
			"status":          AsrReservationStatusSettled,
			"settled_quota":   settledQuota,
			"settled_seconds": settledSeconds,
			"settled_at":      common.GetTimestamp(),
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
