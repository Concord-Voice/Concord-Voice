package store

import (
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
)

// Authority-only constants (DoR §2.3; not Class F: changing one is a release).
const (
	GPrimeMs           uint64 = 86_400_000
	DrainGraceMs       uint64 = 660_000
	RestoreMarginMs    uint64 = 60_000
	ExtensionCapMs     uint64 = 259_200_000
	NCancelMs                 = policy.NCancelMs
	VMs                uint64 = 259_200_000
	CommitRate                = 5  // per second, provisional (LD-25)
	CommitBurst               = 50 // provisional (LD-25)
	DiskHardFloorBytes uint64 = 512 << 20

	MutatingSlots      = 4 // one reserved for enroll_new (D-179)
	CancelReserveSlots = 2
	ReadSlots          = 8

	BodyCapTx     = 32 << 10
	BodyCapDevice = 16 << 10

	TickGapThresholdMs  uint64 = 90_000
	PeerContactWindowMs uint64 = 120_000

	PurgeBatch      = 1000
	PurgeCandidates = 100
)

// Request deadlines and the intervals of the store's loops.
var (
	RequestDeadlineTx   = 8 * time.Second
	RequestDeadlineRead = 4 * time.Second
	ReleasePollInterval = 10 * time.Millisecond
	RolePollInterval    = 5 * time.Second
	TickInterval        = 30 * time.Second
	PurgeBudget         = 20 * time.Second
)
