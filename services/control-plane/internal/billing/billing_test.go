package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestConstants pins the identifiers that Stripe objects, the entitlement
// resolver and later billing PRs all key on. Changing one is a catalog
// migration in both Stripe modes, not a refactor.
func TestConstants(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ got, want string }{
		"coupon id":               {CouponFoundersID, "founders-50"},
		"portal metadata key":     {PortalMetadataKey, "concord_portal"},
		"portal metadata version": {PortalMetadataVersion, "v1"},
		"plan metadata key":       {PlanMetadataKey, "concord_plan"},
		"product metadata key":    {ProductMetadataKey, "concord_product"},
		"plan supersonic":         {PlanSupersonic, "supersonic"},
		"plan wing":               {PlanWing, "wing"},
		"supersonic monthly":      {LookupKeySupersonicMonthly, "supersonic_monthly"},
		"supersonic yearly":       {LookupKeySupersonicYearly, "supersonic_yearly"},
		"wing monthly":            {LookupKeyWingMonthly, "wing_monthly"},
		"wing yearly":             {LookupKeyWingYearly, "wing_yearly"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.got)
		})
	}
}
