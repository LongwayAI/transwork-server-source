package handler

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting"
)

func TestSubscriptionEnabled_RequiresBothGates(t *testing.T) {
	restoreSwitch := SetSubscriptionMasterSwitchForTest(false)
	origSecret := setting.StripeApiSecret
	t.Cleanup(func() {
		restoreSwitch()
		setting.StripeApiSecret = origSecret
	})

	setMasterSwitch := func(on bool) {
		val := "false"
		if on {
			val = "true"
		}
		common.OptionMapRWMutex.Lock()
		if common.OptionMap == nil {
			common.OptionMap = make(map[string]string)
		}
		common.OptionMap["DesktopSubscriptionEnabled"] = val
		common.OptionMapRWMutex.Unlock()
	}

	// Gate 1 on (Stripe secret configured), gate 2 off (master switch) -> false.
	setting.StripeApiSecret = "sk_test"
	setMasterSwitch(false)
	if SubscriptionEnabled() {
		t.Fatal("expected false when the master switch is off")
	}

	// Gate 2 on (master switch), gate 1 off (no Stripe secret) -> false.
	setting.StripeApiSecret = ""
	setMasterSwitch(true)
	if SubscriptionEnabled() {
		t.Fatal("expected false when no Stripe secret is configured")
	}

	// Both gates on -> true.
	setting.StripeApiSecret = "sk_test"
	setMasterSwitch(true)
	if !SubscriptionEnabled() {
		t.Fatal("expected true when both gates are on")
	}
}
