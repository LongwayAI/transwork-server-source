package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// FreeCreditClaim is a permanent record that a user has claimed the one-time
// "continue without an invite code" free starter credit. UserId is the primary
// key, so the database itself guarantees at most one claim per user forever —
// even under a concurrent double-claim, the second insert fails on the unique
// key. This decouples the once-only guarantee from the user's current
// quota/usage, which can change over time.
//
// Column types are restricted to portable bigint/int so the table migrates
// identically on SQLite, MySQL >= 5.7.8, and PostgreSQL >= 9.6 (repo Rule 2).
type FreeCreditClaim struct {
	UserId    int   `json:"user_id" gorm:"primaryKey"`
	Amount    int   `json:"amount" gorm:"type:bigint"` // quota units granted, for audit
	CreatedAt int64 `json:"created_at" gorm:"type:bigint"`
}

func (f *FreeCreditClaim) BeforeCreate(tx *gorm.DB) error {
	if f.CreatedAt == 0 {
		f.CreatedAt = common.GetTimestamp()
	}
	return nil
}
