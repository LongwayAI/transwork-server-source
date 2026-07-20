package transwork

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	twhandler "github.com/QuantumNous/new-api/transwork/handler"
	twmodel "github.com/QuantumNous/new-api/transwork/model"
	twstorage "github.com/QuantumNous/new-api/transwork/storage"
)

func Init() error {
	seedDesktopRechargeOption()
	seedDesktopSubscriptionOption()
	seedSubscriptionOptions()
	seedInviteOptionalOptions()
	if err := model.DB.AutoMigrate(&twmodel.StripeSubscriptionLink{}); err != nil {
		return err
	}
	if err := model.DB.AutoMigrate(&twmodel.WaitlistEntry{}); err != nil {
		return err
	}
	if err := model.DB.AutoMigrate(&twmodel.FreeCreditClaim{}); err != nil {
		return err
	}
	return twstorage.InitGCSClient()
}

// seedDesktopRechargeOption ensures the DesktopRechargeEnabled admin option
// exists with a default of "false" (recharge off) without editing upstream
// model.InitOptionMap. Existing admin-set values are never overwritten.
func seedDesktopRechargeOption() {
	seedOptionIfAbsent("DesktopRechargeEnabled", "false")
}

// seedDesktopSubscriptionOption ensures the DesktopSubscriptionEnabled admin
// option exists with a default of "false" (subscription off) without editing
// upstream model.InitOptionMap. Existing admin-set values are never overwritten.
func seedDesktopSubscriptionOption() {
	seedOptionIfAbsent("DesktopSubscriptionEnabled", "false")
}

// seedSubscriptionOptions creates the overlay-owned Stripe recurring-subscription
// options (design B5) without editing upstream setting/model files. The webhook
// secret defaults empty (the admin pastes endpoint B's secret, O4); the portal
// return URL defaults to the branded gressio.ai page (O8, Option A). Existing
// admin-set values are never overwritten.
func seedSubscriptionOptions() {
	seedOptionIfAbsent(twhandler.OptionKeyStripeWebhookSecret, "")
	seedOptionIfAbsent(twhandler.OptionKeyPortalReturnURL, twhandler.DefaultPortalReturnURL)
}

// seedInviteOptionalOptions creates the overlay-owned options backing the
// "invite code optional + free starter credit" feature (Payment settings). The
// toggle defaults "false" (invite required — the current behavior); the free
// credit amount defaults "100" (in Gressio credits). Existing admin-set values
// are never overwritten.
func seedInviteOptionalOptions() {
	seedOptionIfAbsent("InviteCodeOptional", "false")
	seedOptionIfAbsent("FreeCreditForNewUser", "100")
}

func seedOptionIfAbsent(key, value string) {
	common.OptionMapRWMutex.RLock()
	_, exists := common.OptionMap[key]
	common.OptionMapRWMutex.RUnlock()
	if !exists {
		_ = model.UpdateOption(key, value)
	}
}
