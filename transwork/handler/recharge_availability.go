package handler

import "github.com/QuantumNous/new-api/controller"

// rechargeMasterSwitch is the desktop-recharge master switch (gate 2). Flip to
// true and redeploy to enable the paid Recharge UI. Even when true, recharge
// only shows if a payment provider is also configured (gate 1). This is the
// deliberate "change one line and push" switch.
//
// Future: replace this var with a read from an admin-dashboard transwork
// setting; RechargeEnabled() stays the single decision point.
var rechargeMasterSwitch = false

// RechargeEnabled is the single decision point for whether the desktop client
// shows the Recharge UI: master switch (gate 2) AND at least one configured
// payment provider (gate 1).
func RechargeEnabled() bool {
	return rechargeMasterSwitch && controller.IsAnyOnlineTopUpEnabled()
}

// SetRechargeMasterSwitchForTest overrides the master switch (gate 2) and
// returns a function that restores the previous value. Test-only seam so
// route-level tests can exercise both the disabled (403) and enabled
// (pass-through) paths of the recharge gate.
func SetRechargeMasterSwitchForTest(enabled bool) (restore func()) {
	prev := rechargeMasterSwitch
	rechargeMasterSwitch = enabled
	return func() { rechargeMasterSwitch = prev }
}
