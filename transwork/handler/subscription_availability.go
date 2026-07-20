package handler

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting"
)

// subscriptionMasterSwitchKey is the admin option (gate 2) backing the desktop
// Subscription UI. Toggle it from the admin dashboard: Payment settings → 通用设置
// (General). Even when on, subscription only shows if a Stripe secret key is also
// configured (gate 1). Default is unset/"false": subscription stays hidden until
// an admin enables it.
const subscriptionMasterSwitchKey = "DesktopSubscriptionEnabled"

// SubscriptionEnabled is the single decision point for whether the desktop client
// shows the Subscription UI (subscribe button, plan/manage section, monthly
// credit bucket): master switch (gate 2) AND a configured Stripe secret (gate 1).
// Any non-"true" value (including missing) is fail-closed false.
func SubscriptionEnabled() bool {
	common.OptionMapRWMutex.RLock()
	enabled := common.OptionMap[subscriptionMasterSwitchKey] == "true"
	common.OptionMapRWMutex.RUnlock()
	return enabled && setting.StripeApiSecret != ""
}

// SetSubscriptionMasterSwitchForTest overrides the master switch (gate 2) by
// writing the backing option, and returns a function that restores the previous
// value. Test-only seam so route-level tests can exercise both the disabled and
// enabled paths of the subscription gate.
func SetSubscriptionMasterSwitchForTest(enabled bool) (restore func()) {
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	prev, had := common.OptionMap[subscriptionMasterSwitchKey]
	if enabled {
		common.OptionMap[subscriptionMasterSwitchKey] = "true"
	} else {
		common.OptionMap[subscriptionMasterSwitchKey] = "false"
	}
	common.OptionMapRWMutex.Unlock()
	return func() {
		common.OptionMapRWMutex.Lock()
		if had {
			common.OptionMap[subscriptionMasterSwitchKey] = prev
		} else {
			delete(common.OptionMap, subscriptionMasterSwitchKey)
		}
		common.OptionMapRWMutex.Unlock()
	}
}
