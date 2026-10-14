package authoritycontract

import (
	"errors"
	"time"
)

// Op, Actor, Assurance and BindWait wrap A1's untyped constants (§2.12
// signatures): Op(OpRecover), Actor(ActorUser), BindWait(BindWaitNow).
type (
	// Op is a §2.7 op name (A1's Op* constants).
	Op string
	// Actor is a §2.9 actor (A1's Actor* constants).
	Actor string
	// Assurance is a §2.8 segment assurance (A1's Assurance* constants).
	Assurance string
	// BindWait is a §2.22 bind tier (A1's BindWait* constants); "" is no tier.
	BindWait string
)

// Use is §2.12's freshness mode: NewSecret requires as_of ≥ now − τ,
// History requires now ≤ valid_until; both require as_of ≤ now + ε.
type Use uint8

// The two Use modes; the zero Use is neither.
const (
	UseNewSecret Use = iota + 1
	UseHistory
)

// ParseUse reads a vector's `use` value.
func ParseUse(s string) (Use, bool) {
	switch s {
	case "new-secret":
		return UseNewSecret, true
	case "history":
		return UseHistory, true
	}
	return 0, false
}

// HighWater is T5's never-decreasing mark, persisted by the caller as
// (gen u, serial u, min_valid u) (§2.12 persisted forms).
type HighWater struct{ Gen, Serial, MinValid uint64 }

func (h HighWater) less(o HighWater) bool {
	return h.Gen < o.Gen || (h.Gen == o.Gen && h.Serial < o.Serial)
}

// maxHW keeps the lexicographically higher (gen, serial) and its min_valid.
// On a (gen, serial) tie it keeps the larger min_valid, so HW never decreases
// (T5) even if a persisted HW and this Trust disagree about one delegation.
func maxHW(a, b HighWater) HighWater {
	if a.less(b) {
		return b
	}
	if !b.less(a) && b.MinValid > a.MinValid {
		return b
	}
	return a
}

// AccountMemory is exactly the three inputs the verifier reads (§2.12):
// the first-sight pin (D-149), the ever-enrolled bit, and the accepted pin
// for the conversation being wrapped (LD-28).
type AccountMemory struct {
	Pin          []byte
	EverEnrolled bool
	AcceptedPin  []byte
}

// Fix is a retained verified status (R4): the `signed` status bytes.
type Fix struct{ Status []byte }

// WrapUse is §2.5 device-secrets check 4.
type WrapUse uint8

// The two WrapUse values; the zero WrapUse is neither.
const (
	WrapCurrent WrapUse = iota + 1
	WrapHistory
)

func (u WrapUse) String() string {
	switch u {
	case WrapCurrent:
		return "current"
	case WrapHistory:
		return "history"
	}
	return ""
}

// ErrIgnored is AddSigned's T2/T3 outcome and its duplicate-msg outcome
// (D-222): never a halt (I9).
var ErrIgnored = errors.New("authoritycontract: ignored")

// HaltError is T4's realm-wide refuse-op.
type HaltError struct{ Rule string }

func (e *HaltError) Error() string {
	if e == nil {
		return "authoritycontract: trust halt"
	}
	return "authoritycontract: trust halt: " + e.Rule
}

// HeadInput is the effective published head the shared device rules read
// (V3-02). ForceUnverified is authority-only; own devices pass false.
type HeadInput struct {
	State           string
	Seq             uint64
	EntryDigest     []byte
	RootDigest      []byte
	ForceUnverified bool
}

// DeviceOpInput is one private-ledger op for ApplyDeviceOp (D-270).
// Outcome is "", "P" or "D" (DR5, DR6); Bind is a `recovery-bind` B's bytes.
type DeviceOpInput struct {
	Op, Actor      string
	New            *DeviceEntry
	Removed        []string
	Bind           []byte
	BindWait       string
	PriorSig       bool
	RootForm       bool
	Head           HeadInput
	Outcome        string
	ProposingAnbMs uint64
}

// RecoveryView is the recovery state effective at DeviceView.AsOfMs (LD-34).
// A nil Replacement with a ReplacementBindDigest is a pending unbind (LD-49).
// Unresolved (V3-01): AsOfMs ≥ ReplacementNbMs and no verified device-status or
// entry has fixed the slot's outcome, so it is still pending at AsOfMs and may
// complete at any later instant (DR9 (c): own devices hold no upper bound on
// the pending hypothesis, G-41). A view with no AsOfMs is never unresolved.
type RecoveryView struct {
	Bound                 *[97]byte
	BindDigest            []byte
	Replacement           *[97]byte
	ReplacementBindDigest []byte
	ReplacementNbMs       uint64
	Unresolved            bool
}

// DeviceView is the effective private ledger (LD-32).
type DeviceView struct {
	Active   []DeviceEntry
	Pending  *DeviceEntry
	Recovery RecoveryView
	Dseq     uint64
	AsOfMs   uint64
	admitted map[string]string // device_id → admitting actor ("" for none)
}

// VerifiedStatus is a status that passed VerifyStatus. WrapKey (R6) is the
// only way to the bytes a wrap may target (DoR:938, "only through the opaque
// handle"), and it reads only the unexported seal verifyStatus sets: the
// exported fields report the verdict and are never read back, and the active
// EK is not exported here. A ChainHead still carries its EK slots in exported
// fields (A1's persisted form), so wrapping only to WrapKey's bytes is the
// caller's obligation for those.
type VerifiedStatus struct {
	Reason      Reason
	Status      Status
	Pin         []byte      // the status pin: the first-sight value to store (D-149)
	ekSPKI      []byte      // the active EK SPKI when the effective state is active; read only by seal
	Segment     Assurance   // the effective root's segment assurance; "" with no root
	AcceptedPin bool        // ok_accepted_pin: flag the member unverified (LD-28)
	continuity  bool        // a continuity change pending from absent
	use         Use         // the Use verifyStatus judged freshness under; 0 on a value it did not return
	wrap        *wrapTarget // R6's target, sealed by verifyStatus for an ok or ok_accepted_pin verdict only
}

// wrapTarget is what WrapKey may wrap to (R6), copied when verifyStatus seals
// a verdict: the verified chain's active EK, or the status pin a served SPKI
// must hash to when served is set (an absent account, or a continuity change
// pending from absent).
type wrapTarget struct {
	ek, pin []byte
	served  bool
}

// sigMsg is the `sig` message a role signs (§2.5): the verifier rebuilds it.
// It is nil when role or digest is invalid (Sig.Encode), and no signature
// verifies over nil, so an invalid input fails closed; do not add a fallback.
func sigMsg(role string, digest []byte) []byte { return Sig{Role: role, Digest: digest}.Encode() }

// addMs sums u values, refusing anything above 2^53 − 1 (R2, DR5, DR9, V3-11).
func addMs(xs ...uint64) (uint64, bool) {
	var s uint64
	for _, x := range xs {
		if x > MaxSafeInt || s > MaxSafeInt-x {
			return 0, false
		}
		s += x
	}
	return s, true
}

// msOf reads a parameter time once; the package never reads the clock. It
// reads the zero time.Time and every time before the epoch as 0, which every
// function that takes `now` refuses as malformed (Sec-N4: VerifyStatus, and
// VerifyChain and VerifyDeviceLedger through verifyChainSteps).
func msOf(t time.Time) uint64 {
	ms := t.UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}
