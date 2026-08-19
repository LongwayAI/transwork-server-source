package transwork

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
)

// Verifies the embedded recharge_tiers.json parses and is seeded as the model
// default (so Recharge() and /tiers use it when no admin override is set). init()
// has already run by the time this test executes.
func TestEmbeddedRechargeDefaultSeeded(t *testing.T) {
	cfg := model.EffectiveRechargeConfig()
	if cfg.CreditsPerDollar != 100 {
		t.Errorf("credits_per_dollar = %v, want 100", cfg.CreditsPerDollar)
	}
	if len(cfg.Tiers) != 3 {
		t.Fatalf("got %d tiers, want 3", len(cfg.Tiers))
	}
	cases := []struct {
		amount int64
		want   float64
	}{{10, 1.00}, {30, 1.05}, {100, 1.10}}
	for _, c := range cases {
		if got := model.StripeTopupBonusFactor(c.amount); got != c.want {
			t.Errorf("StripeTopupBonusFactor(%d) = %v, want %v", c.amount, got, c.want)
		}
	}
}
