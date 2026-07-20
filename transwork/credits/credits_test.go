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

func TestToQuotaConvertsCredits(t *testing.T) {
	original := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	defer func() {
		common.QuotaPerUnit = original
	}()

	// 500 credits must grant enough quota to display as 500 credits again — the
	// bug was that "500" was applied as raw quota (0.1 credits, floored to 0).
	if got := ToQuota(500); got != 2_500_000 {
		t.Fatalf("ToQuota(500) = %d, want %d", got, 2_500_000)
	}
	if got := FormatQuota(ToQuota(500)); got != "500 credits" {
		t.Fatalf("round-trip FormatQuota(ToQuota(500)) = %q, want %q", got, "500 credits")
	}
}
