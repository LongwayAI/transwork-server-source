package model

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

// RechargeTier is one desktop recharge option: a USD price and an optional bonus
// percentage granted on top of the standard credit exchange rate.
type RechargeTier struct {
	USD      int64   `json:"usd"`
	BonusPct float64 `json:"bonus_pct"`
}

// RechargeConfigData is the full desktop recharge config: the standard exchange
// rate (credits per USD) plus the tier table. Mirrors the JSON shape served to
// the desktop client and stored in the admin override.
type RechargeConfigData struct {
	CreditsPerDollar float64        `json:"credits_per_dollar"`
	Tiers            []RechargeTier `json:"tiers"`
}

// rechargeAdminConfig is the DB-backed, dashboard-editable override. Admins paste
// a RechargeConfigData JSON blob under Payment settings; it round-trips through
// the config manager as the option key "transwork_recharge.config" and applies
// live (no rebuild). Empty/invalid => the built-in default (seeded from
// transwork/recharge_tiers.json by the overlay) is used.
//
// Lives in the model package (an upstream touchpoint) rather than the transwork
// overlay because Recharge() must read it during crediting and model cannot
// import the overlay package without an import cycle.
type rechargeAdminConfig struct {
	Config string `json:"config"`
}

var rechargeAdmin = &rechargeAdminConfig{}

var defaultRechargeConfig RechargeConfigData

func init() {
	config.GlobalConfig.Register("transwork_recharge", rechargeAdmin)
}

// SetDefaultRechargeConfig seeds the built-in default used when the admin
// override is unset or invalid. Called by the transwork overlay at startup with
// the embedded recharge_tiers.json.
func SetDefaultRechargeConfig(c RechargeConfigData) {
	defaultRechargeConfig = c
}

// EffectiveRechargeConfig returns the admin override when set and valid,
// otherwise the built-in default. Single authority for both crediting and the
// desktop /tiers endpoint, so displayed credits always match granted credits.
func EffectiveRechargeConfig() RechargeConfigData {
	if strings.TrimSpace(rechargeAdmin.Config) != "" {
		var c RechargeConfigData
		if err := common.Unmarshal([]byte(rechargeAdmin.Config), &c); err == nil && len(c.Tiers) > 0 {
			return c
		}
		common.SysError("invalid transwork_recharge.config JSON; falling back to default recharge tiers")
	}
	return defaultRechargeConfig
}

// StripeTopupBonusFactor returns the credit bonus multiplier for a one-time
// Stripe top-up of `amount` USD: the bonus of the highest configured tier whose
// price is <= amount (so any larger top-up still earns the better rate). Returns
// 1.0 (base rate) when no tier matches. Applied to granted quota only — the
// recorded amount paid is untouched.
func StripeTopupBonusFactor(amount int64) float64 {
	bonusPct := 0.0
	var bestUSD int64 = -1
	for _, t := range EffectiveRechargeConfig().Tiers {
		if t.USD <= amount && t.USD > bestUSD {
			bestUSD = t.USD
			bonusPct = t.BonusPct
		}
	}
	return 1.0 + bonusPct/100.0
}
