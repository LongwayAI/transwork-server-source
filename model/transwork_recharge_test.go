package model

import "testing"

func setDefaultTiers() {
	SetDefaultRechargeConfig(RechargeConfigData{
		CreditsPerDollar: 100,
		Tiers: []RechargeTier{
			{USD: 10, BonusPct: 0},
			{USD: 30, BonusPct: 5},
			{USD: 100, BonusPct: 10},
		},
	})
}

// Pins the bonus-lookup logic against the default tiers (highest tier whose
// price <= amount; base rate when none match).
func TestStripeTopupBonusFactor(t *testing.T) {
	setDefaultTiers()
	defer SetDefaultRechargeConfig(RechargeConfigData{})

	cases := []struct {
		amount int64
		want   float64
	}{
		{0, 1.00},
		{10, 1.00},  // $10  -> 1,000 credits (base)
		{29, 1.00},  // just under the $30 tier
		{30, 1.05},  // $30  -> 3,150 credits (+5%)
		{50, 1.05},  // larger-than-$30 still gets the better rate
		{99, 1.05},  // just under the $100 tier
		{100, 1.10}, // $100 -> 11,000 credits (+10%)
		{500, 1.10}, // larger-than-$100 caps at the top tier
	}
	for _, c := range cases {
		if got := StripeTopupBonusFactor(c.amount); got != c.want {
			t.Errorf("StripeTopupBonusFactor(%d) = %v, want %v", c.amount, got, c.want)
		}
	}
}

// With nothing configured, crediting must fall back to the base rate.
func TestStripeTopupBonusFactor_Unconfigured(t *testing.T) {
	SetDefaultRechargeConfig(RechargeConfigData{})
	rechargeAdmin.Config = ""
	if got := StripeTopupBonusFactor(100); got != 1.00 {
		t.Errorf("unconfigured StripeTopupBonusFactor(100) = %v, want 1.00", got)
	}
}

// A valid admin override wins over the default, and its tiers drive the bonus.
func TestEffectiveRechargeConfig_AdminOverride(t *testing.T) {
	setDefaultTiers()
	defer SetDefaultRechargeConfig(RechargeConfigData{})
	rechargeAdmin.Config = `{"credits_per_dollar":100,"tiers":[{"usd":10,"bonus_pct":0},{"usd":50,"bonus_pct":8}]}`
	defer func() { rechargeAdmin.Config = "" }()

	cfg := EffectiveRechargeConfig()
	if len(cfg.Tiers) != 2 {
		t.Fatalf("admin override tiers = %d, want 2", len(cfg.Tiers))
	}
	if got := StripeTopupBonusFactor(50); got != 1.08 {
		t.Errorf("override StripeTopupBonusFactor(50) = %v, want 1.08", got)
	}
}

// A malformed admin override must fall back to the default (never crash/zero out).
func TestEffectiveRechargeConfig_InvalidAdminFallsBack(t *testing.T) {
	setDefaultTiers()
	defer SetDefaultRechargeConfig(RechargeConfigData{})
	rechargeAdmin.Config = "not valid json"
	defer func() { rechargeAdmin.Config = "" }()

	if got := StripeTopupBonusFactor(100); got != 1.10 {
		t.Errorf("invalid-admin StripeTopupBonusFactor(100) = %v, want 1.10 (default)", got)
	}
}
