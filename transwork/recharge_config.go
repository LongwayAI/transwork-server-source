package transwork

import (
	_ "embed"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// recharge_tiers.json is the built-in DEFAULT for desktop recharge tiers and the
// standard credit exchange rate. Embedded at build time so it always ships with
// the binary. It is overridden at runtime by the admin "transwork_recharge.config"
// setting (editable in the dashboard); this file is the fallback when that is
// unset or invalid. Edit + rebuild to change the default.
//
//go:embed recharge_tiers.json
var rechargeTiersJSON []byte

// init parses the embedded default once and seeds it into the model, where both
// crediting (Recharge) and the /tiers endpoint read the effective config. A
// malformed default is logged and skipped (effective config then stays empty =>
// base rate, no bonus) rather than crashing the server.
func init() {
	var cfg model.RechargeConfigData
	if err := common.Unmarshal(rechargeTiersJSON, &cfg); err != nil {
		common.SysError("failed to parse embedded recharge_tiers.json: " + err.Error())
		return
	}
	model.SetDefaultRechargeConfig(cfg)
}

// GetRechargeTiers returns the effective recharge config (admin override if set,
// else the built-in default) so the desktop client renders exactly what the
// server grants.
func GetRechargeTiers(c *gin.Context) {
	cfg := model.EffectiveRechargeConfig()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"credits_per_dollar": cfg.CreditsPerDollar,
			"tiers":              cfg.Tiers,
		},
	})
}
