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

func FormatQuota(quota int) string {
	if common.QuotaPerUnit <= 0 {
		return strconv.Itoa(quota)
	}
	return strconv.Itoa(int(math.Floor(FromQuota(quota)))) + " credits"
}
