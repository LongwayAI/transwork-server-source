package credits

import (
	"math"
	"strconv"

	"github.com/QuantumNous/new-api/common"
)

const PerUSD = 100

func FromQuota(quota int) float64 {
	if common.QuotaPerUnit <= 0 {
		return float64(quota)
	}
	return float64(quota) / common.QuotaPerUnit * PerUSD
}

// ToQuota converts a Gressio credit amount to raw quota units, the inverse of
// FromQuota. Used when an admin configures a value in credits (e.g. the new-user
// free starter credit) that must be applied to a user's raw quota balance.
func ToQuota(credits float64) int {
	if common.QuotaPerUnit <= 0 {
		return int(credits)
	}
	return int(math.Round(credits / PerUSD * common.QuotaPerUnit))
}

func FormatQuota(quota int) string {
	if common.QuotaPerUnit <= 0 {
		return strconv.Itoa(quota)
	}
	return strconv.Itoa(int(math.Floor(FromQuota(quota)))) + " credits"
}
