package handler

import (
	"testing"

	"github.com/QuantumNous/new-api/setting"
)

func TestRechargeEnabled_RequiresBothGates(t *testing.T) {
	origSwitch := rechargeMasterSwitch
	origSecret, origWebhook, origPrice := setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId
	t.Cleanup(func() {
		rechargeMasterSwitch = origSwitch
		setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = origSecret, origWebhook, origPrice
	})

	stripeOn := func() {
		setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = "sk_test", "whsec_test", "price_123"
	}
	stripeOff := func() {
		setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = "", "", ""
	}

	// Gate 1 on (provider configured), gate 2 off (master switch) -> false.
	stripeOn()
	rechargeMasterSwitch = false
	if RechargeEnabled() {
		t.Fatal("expected false when the master switch is off")
	}

	// Gate 2 on (master switch), gate 1 off (no provider) -> false.
	stripeOff()
	rechargeMasterSwitch = true
	if RechargeEnabled() {
		t.Fatal("expected false when no provider is configured")
	}

	// Both gates on -> true.
	stripeOn()
	rechargeMasterSwitch = true
	if !RechargeEnabled() {
		t.Fatal("expected true when both gates are on")
	}
}
