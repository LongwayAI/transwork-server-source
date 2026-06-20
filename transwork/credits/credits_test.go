package credits

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
)

func TestFormatQuotaFloorsCredits(t *testing.T) {
	original := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	defer func() {
		common.QuotaPerUnit = original
	}()

	if got := FormatQuota(24_856_800); got != "4971 credits" {
		t.Fatalf("FormatQuota() = %q, want %q", got, "4971 credits")
	}
}

func TestFormatQuotaZero(t *testing.T) {
	original := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	defer func() {
		common.QuotaPerUnit = original
	}()

	if got := FormatQuota(0); got != "0 credits" {
		t.Fatalf("FormatQuota() = %q, want %q", got, "0 credits")
	}
}
