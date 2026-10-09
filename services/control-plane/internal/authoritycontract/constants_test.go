package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConstantsMatchTheDoR pins every protocol constant and cap to the DoR's
// §2.3 figure as a literal. They are frozen in v1 (D-162): a wrong digit here
// is a protocol v2, so each is written out rather than derived.
func TestConstantsMatchTheDoR(t *testing.T) {
	require.Equal(t, "concord-account-authority", Prefix)
	require.Equal(t, 1, Version)
	require.Equal(t, uint64(9_007_199_254_740_991), MaxSafeInt, "2^53 - 1")

	// Protocol constants (§2.3).
	require.Equal(t, uint64(120_000), EpsilonMs)
	require.Equal(t, uint64(259_200_000), VMaxMs, "72 h")
	require.Equal(t, uint64(34_560_000_000), DelegationLifetimeMaxMs, "400 d")
	require.Equal(t, uint64(2_592_000_000), DeltaMaxMs, "30 d")
	require.Equal(t, uint64(660_000), IntentWindowMs, "11 min")
	require.Equal(t, uint64(120_000), QueryWindowMs)
	require.Equal(t, uint64(60_000), CancelSlackMs)
	require.Equal(t, uint64(2_592_000_000), RecoveryReplaceMs, "30 d")
	require.Equal(t, uint64(9_007_199_254_740_991), ANBNever)
	require.Equal(t, MaxSafeInt, ANBNever, "ANB_NEVER is the largest safe integer, so it stays a legal u")
	require.Less(t, IntentWindowMs+EpsilonMs, DeltaMaxMs, "delta_ms must have room above INTENT_WINDOW + EPSILON (§2.5 delegation #17)")

	// Item caps (§2.3).
	require.Equal(t, 10, MaxDevices)
	require.Equal(t, 4, MaxCAKs)
	require.Equal(t, 200, HistoryPage)
	require.Equal(t, 16, QueryRespBundles)
	require.Equal(t, 500, StatusBatch)
	require.Equal(t, 256, TrustItems)
	require.Equal(t, 4, MaxRSKs)
	require.Equal(t, 4, MaxOperatorKeys)
	require.Equal(t, 8, MaxAuthorityTLSKeys)
	require.Equal(t, 8, MaxCPTLSPins)
	require.Equal(t, 16, DevicePage)

	// Standalone caps (§2.3), in bytes.
	require.Equal(t, 1024, CapAnchors)
	require.Equal(t, 6144, CapSigned)
	require.Equal(t, 4608, CapDeviceSet)
	require.Equal(t, 6144, CapDeviceWrap)
	require.Equal(t, 2048, CapDeviceIntent)
	require.Equal(t, 8192, CapDeviceBundle)
	require.Equal(t, 180_224, CapDeviceHistory)
	require.Equal(t, 1024, CapRecoveryBind)
	require.Equal(t, 10_278, CapRelayRecover)
	require.Equal(t, 1024, CapDeviceResult)
	require.Equal(t, 4080, CapDeviceSecrets, "the 4096-byte ct less its 16-byte tag")
	require.Equal(t, 2*1024*1024, CapTrust, "2 MiB")
	require.Equal(t, 16*1024*1024, HeadBlobMax, "16 MiB")
}
