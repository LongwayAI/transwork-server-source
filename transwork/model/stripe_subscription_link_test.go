package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return db
}

// The link table is the whole feature's system of record; if AutoMigrate can't
// stand it up on SQLite (one of the three mandated engines, Rule 2) the overlay
// never boots. This proves the model migrates cleanly with only portable types.
func TestStripeSubscriptionLinkMigratesOnSQLite(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&StripeSubscriptionLink{}); err != nil {
		t.Fatalf("AutoMigrate StripeSubscriptionLink: %v", err)
	}
	if !db.Migrator().HasTable(&StripeSubscriptionLink{}) {
		t.Fatal("expected table to exist after migrate")
	}

	// Every spec'd column must be present, or downstream queries silently break.
	for _, col := range []string{
		"id", "stripe_subscription_id", "stripe_customer_id", "user_id", "plan_id",
		"user_subscription_id", "status", "auto_renew", "current_period_end",
		"last_invoice_id", "last_event_id", "created_at", "updated_at",
	} {
		if !db.Migrator().HasColumn(&StripeSubscriptionLink{}, col) {
			t.Errorf("expected column %q to exist", col)
		}
	}
}

// The unique index on stripe_subscription_id is the idempotency backbone: the
// webhook upserts one row per Stripe subscription, so a duplicate insert of the
// same sub_… must be rejected by the DB, not merely by application logic.
func TestStripeSubscriptionLinkUniqueSubscriptionId(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&StripeSubscriptionLink{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := db.Create(&StripeSubscriptionLink{StripeSubscriptionId: "sub_dup", UserId: 1}).Error; err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}
	if err := db.Create(&StripeSubscriptionLink{StripeSubscriptionId: "sub_dup", UserId: 2}).Error; err == nil {
		t.Fatal("expected duplicate stripe_subscription_id to violate the unique index")
	}
}
