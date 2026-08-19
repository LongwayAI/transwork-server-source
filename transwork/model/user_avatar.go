package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// UserAvatar stores the profile-picture URL the identity provider reports for a
// user — e.g. the Google avatar Logto forwards as the OIDC `picture` claim.
// UserId is the primary key, so each user has at most one row.
//
// The value is refreshed on every sign-in rather than written once at
// registration: provider avatar URLs are stable but not permanent, and a user
// who changes their upstream picture should see the new one after re-login.
//
// This lives in the overlay instead of as a column on the upstream User model so
// the upstream users table stays untouched (repo Rule 4). Column types are
// restricted to portable varchar/bigint so the table migrates identically on
// SQLite, MySQL >= 5.7.8, and PostgreSQL >= 9.6 (repo Rule 2).
type UserAvatar struct {
	UserId    int    `json:"user_id" gorm:"primaryKey"`
	AvatarUrl string `json:"avatar_url" gorm:"type:varchar(1024)"`
	UpdatedAt int64  `json:"updated_at" gorm:"type:bigint"`
}

func (a *UserAvatar) BeforeCreate(tx *gorm.DB) error {
	a.UpdatedAt = common.GetTimestamp()
	return nil
}
