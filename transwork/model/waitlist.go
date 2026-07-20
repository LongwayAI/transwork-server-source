package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// WaitlistEntry is a Gressio desktop "join the waitlist" submission and the
// source of truth for the waitlist (no SMTP dependency). Email is the
// applicant's verified account email (taken from the auth token, not the form),
// so it is always a reachable address even if a form field is mistyped.
//
// Column types are restricted to portable varchar/text/bigint/int so the table
// migrates identically on SQLite, MySQL >= 5.7.8, and PostgreSQL >= 9.6 (repo
// Rule 2).
type WaitlistEntry struct {
	Id      int    `json:"id"`
	UserId  int    `json:"user_id" gorm:"index"`
	Email   string `json:"email" gorm:"type:varchar(255);index"`
	Name    string `json:"name" gorm:"type:varchar(255)"`
	Job     string `json:"job" gorm:"type:varchar(255)"`
	Role    string `json:"role" gorm:"type:varchar(255)"`
	UseCase string `json:"use_case" gorm:"type:text"`

	CreatedAt int64 `json:"created_at" gorm:"bigint"`
}

func (w *WaitlistEntry) BeforeCreate(tx *gorm.DB) error {
	if w.CreatedAt == 0 {
		w.CreatedAt = common.GetTimestamp()
	}
	return nil
}
