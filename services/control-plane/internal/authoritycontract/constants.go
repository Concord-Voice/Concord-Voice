package authoritycontract

// EN1 prefix (§2.3, §3.1). Frozen in protocol v1.
const (
	Prefix  = "concord-account-authority"
	Version = 1
)

// MaxSafeInt is EN3's bound on u: 2^53 − 1.
const MaxSafeInt uint64 = 1<<53 - 1

// Protocol constants (§2.3). Frozen in v1; changing one is protocol v2 (D-162).
const (
	EpsilonMs               uint64 = 120_000
	VMaxMs                  uint64 = 259_200_000
	DelegationLifetimeMaxMs uint64 = 34_560_000_000
	DeltaMaxMs              uint64 = 2_592_000_000
	IntentWindowMs          uint64 = 660_000
	QueryWindowMs           uint64 = 120_000
	CancelSlackMs           uint64 = 60_000
	RecoveryReplaceMs       uint64 = 2_592_000_000
	ANBNever                uint64 = 9_007_199_254_740_991
)

// Item caps (§2.3).
const (
	MaxDevices          = 10
	MaxCAKs             = 4
	HistoryPage         = 200
	QueryRespBundles    = 16
	StatusBatch         = 500
	TrustItems          = 256
	MaxRSKs             = 4
	MaxOperatorKeys     = 4
	MaxAuthorityTLSKeys = 8
	MaxCPTLSPins        = 8
	DevicePage          = 16
)

// Standalone caps (§2.3), EN5 step 1 for a message decoded outside an
// envelope position. Every other layout's cap is the sum of its position caps
// plus its prefix and separators (maxLen).
const (
	CapAnchors       = 1024
	CapSigned        = 6144
	CapDeviceSet     = 4608
	CapDeviceWrap    = 6144
	CapDeviceIntent  = 2048
	CapDeviceBundle  = 8192
	CapDeviceHistory = 180_224
	CapRecoveryBind  = 1024
	CapRelayRecover  = 10_278
	CapDeviceResult  = 1024
	CapDeviceSecrets = 4080
	CapTrust         = 2_097_152
	HeadBlobMax      = 16_777_216
)
