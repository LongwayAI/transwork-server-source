package handler

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
)

// rechargeMasterSwitchKey is the admin option (gate 2) backing the desktop
// Recharge UI. Toggle it from the admin dashboard: Payment settings → 通用设置
// (General). Even when on, recharge only shows if a payment provider is also
// configured (gate 1). Default is unset/"false": recharge stays hidden until an
// admin enables it.
const rechargeMasterSwitchKey = "DesktopRechargeEnabled"

// RechargeEnabled is the single decision point for whether the desktop client
// shows the Recharge UI: master switch (gate 2) AND at least one configured
// payment provider (gate 1). Any non-"true" value (including missing) is
// fail-closed false.
func RechargeEnabled() bool {
	common.OptionMapRWMutex.RLock()
	enabled := common.OptionMap[rechargeMasterSwitchKey] == "true"
	common.OptionMapRWMutex.RUnlock()
	return enabled && controller.IsAnyOnlineTopUpEnabled()
}

// SetRechargeMasterSwitchForTest overrides the master switch (gate 2) by writing
// the backing option, and returns a function that restores the previous value.
// Test-only seam so route-level tests can exercise both the disabled (403) and
// enabled (pass-through) paths of the recharge gate.
func SetRechargeMasterSwitchForTest(enabled bool) (restore func()) {
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	prev, had := common.OptionMap[rechargeMasterSwitchKey]
	if enabled {
		common.OptionMap[rechargeMasterSwitchKey] = "true"
	} else {
		common.OptionMap[rechargeMasterSwitchKey] = "false"
	}
	common.OptionMapRWMutex.Unlock()
	return func() {
		common.OptionMapRWMutex.Lock()
		if had {
			common.OptionMap[rechargeMasterSwitchKey] = prev
		} else {
			delete(common.OptionMap, rechargeMasterSwitchKey)
		}
		common.OptionMapRWMutex.Unlock()
	}
}
