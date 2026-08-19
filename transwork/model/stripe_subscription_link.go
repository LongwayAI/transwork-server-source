// Package model holds Gressio overlay GORM models that live alongside — but
// outside of — the upstream new-api `model` package, so they migrate through the
// overlay's own Init() seam and never require editing upstream migrate lists.
package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// Overlay-local link statuses. These track the Stripe subscription lifecycle and
// are deliberately independent of the upstream UserSubscription.Status values.
const (
	LinkStatusActive   = "active"
	LinkStatusPastDue  = "past_due"
	LinkStatusCanceled = "canceled"
)

// StripeSubscriptionLink maps a real recurring Stripe Subscription (sub_…) to the
// local user/plan/UserSubscription so the overlay webhook can extend access on
// renewal, lapse it on cancellation, and stay idempotent under Stripe's
// at-least-once delivery (design B1).
//
// Column types are restricted to portable varchar/bigint/int so the table
// migrates identically on SQLite, MySQL >= 5.7.8, and PostgreSQL >= 9.6 (repo
// Rule 2).
type StripeSubscriptionLink struct {
	Id int `json:"id"`

	// StripeSubscriptionId is the recurring object (sub_…). Every renewal is keyed
	// on this, never on the customer id — the structural fix for the Translide bug.
	StripeSubscriptionId string `json:"stripe_subscription_id" gorm:"type:varchar(64);uniqueIndex"`
	StripeCustomerId     string `json:"stripe_customer_id" gorm:"type:varchar(64);index"`

	UserId int `json:"user_id" gorm:"index"`
	PlanId int `json:"plan_id" gorm:"type:int"`

	// UserSubscriptionId is nullable-by-zero: the upstream endpoint may not have
	// created the UserSubscription yet when checkout.session.completed arrives, so
	// this is resolved lazily on the first renewal (design B3/O2).
	UserSubscriptionId int `json:"user_subscription_id" gorm:"index"`

	// Status is the overlay-local lifecycle marker (active/past_due/canceled).
	Status string `json:"status" gorm:"type:varchar(32)"`

	// AutoRenew mirrors !cancel_at_period_end from customer.subscription.updated.
	AutoRenew bool `json:"auto_renew"`

	// CurrentPeriodEnd is the last-known Stripe current_period_end (display only).
	CurrentPeriodEnd int64 `json:"current_period_end" gorm:"type:bigint"`

	// LastInvoiceId / LastEventId are the per-link idempotency guards (design B4).
	LastInvoiceId string `json:"last_invoice_id" gorm:"type:varchar(64)"`
	LastEventId   string `json:"last_event_id" gorm:"type:varchar(64)"`

	CreatedAt int64 `json:"created_at" gorm:"bigint"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint"`
}

func (l *StripeSubscriptionLink) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	if l.CreatedAt == 0 {
		l.CreatedAt = now
	}
	l.UpdatedAt = now
	return nil
}

func (l *StripeSubscriptionLink) BeforeUpdate(tx *gorm.DB) error {
	l.UpdatedAt = common.GetTimestamp()
	return nil
}
