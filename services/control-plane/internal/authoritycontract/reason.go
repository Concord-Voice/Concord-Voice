package authoritycontract

import "sort"

// Reason is a §2.12 verification outcome. The strings are contract: vectors
// carry them and the #2628 TypeScript port returns them.
type Reason string

// §2.12 published reasons.
const (
	ReasonOK                  Reason = "ok"
	ReasonOKAcceptedPin       Reason = "ok_accepted_pin" // LD-28: ok, flagged unverified
	ReasonNetwork             Reason = "network"         // the caller's; the verifier never returns it
	ReasonStatusStaleTau      Reason = "status_stale_tau"
	ReasonSupersededUncovered Reason = "superseded_uncovered"
	ReasonStatusExpired       Reason = "status_expired"
	ReasonChainBehind         Reason = "chain_behind"
	ReasonDelegationUnknown   Reason = "delegation_unknown"

	// The paused family (§2.12: withhold, no notice, clears itself).
	ReasonSuspended            Reason = "suspended"
	ReasonDisputed             Reason = "disputed"
	ReasonHeld                 Reason = "held"
	ReasonErased               Reason = "erased"
	ReasonPendingErase         Reason = "pending_erase"
	ReasonRootRevoked          Reason = "root_revoked"
	ReasonPendingFromActive    Reason = "pending_from_active"
	ReasonPendingFromSuspended Reason = "pending_from_suspended"
	ReasonPendingFromAbsent    Reason = "pending_from_absent" // non-continuity pending from absent
	ReasonPinRevoked           Reason = "pin_revoked"

	// Withhold with a notice.
	ReasonFork               Reason = "fork"
	ReasonHypothesisConflict Reason = "hypothesis_conflict"
	ReasonFieldMismatch      Reason = "field_mismatch"
	ReasonANBMismatch        Reason = "anb_mismatch"
	ReasonIssuedBounds       Reason = "issued_bounds"
	ReasonPinChanged         Reason = "pin_changed"

	// Withhold, no notice, needs attention.
	ReasonMalformed           Reason = "malformed"
	ReasonBadSignature        Reason = "bad_signature"
	ReasonCAKUnlisted         Reason = "cak_unlisted"
	ReasonTransitionInvalid   Reason = "transition_invalid"
	ReasonChainGap            Reason = "chain_gap"
	ReasonHeadMismatch        Reason = "head_mismatch"
	ReasonStatusFuture        Reason = "status_future"
	ReasonAbsentUnpinned      Reason = "absent_unpinned"
	ReasonAbsentAfterCutover  Reason = "absent_after_cutover"
	ReasonAbsentAfterEnrolled Reason = "absent_after_enrolled"
	ReasonDelegationWindow    Reason = "delegation_window"
	ReasonOutOfScope          Reason = "out_of_scope"
	ReasonStatusSuperseded    Reason = "status_superseded"
	ReasonRealmMismatch       Reason = "realm_mismatch"

	ReasonTrustHalt  Reason = "trust_halt"  // T4: realm-wide refuse-op
	ReasonSenderSide Reason = "sender_side" // the caller's own key or state; never returned by the verifier
)

// Own-device reasons (LD-32): VerifyDeviceLedger and VerifyDeviceWrap only.
const (
	ReasonDeviceBehind         Reason = "device_behind"
	ReasonDeviceStatusStale    Reason = "device_status_stale"
	ReasonDeviceStatusMismatch Reason = "device_status_mismatch"
	ReasonDeviceGap            Reason = "device_gap"
	ReasonDeviceFork           Reason = "device_fork"
	ReasonDeviceRule           Reason = "device_rule"
	ReasonWrapInvalid          Reason = "wrap_invalid"
)

// Verdict is §2.13's per-member outcome class.
type Verdict uint8

// The §2.12 verdicts; String gives each one's wire spelling. Withhold is the
// zero value, so a Verdict that was never assigned fails closed rather than
// reading as OK. Nothing compares verdicts by number; the order is not wire.
const (
	VerdictWithhold Verdict = iota
	VerdictOK
	VerdictTransient
	VerdictRefuseOp
	VerdictHalt
)

func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictTransient:
		return "transient"
	case VerdictRefuseOp:
		return "refuse-op"
	case VerdictHalt:
		return "halt"
	default:
		return "withhold"
	}
}

type reasonClass struct {
	v      Verdict
	notice bool
}

var reasonTable = map[Reason]reasonClass{
	ReasonOK: {VerdictOK, false}, ReasonOKAcceptedPin: {VerdictOK, false},
	ReasonNetwork: {VerdictTransient, false}, ReasonStatusStaleTau: {VerdictTransient, false},
	ReasonSupersededUncovered: {VerdictTransient, false}, ReasonStatusExpired: {VerdictTransient, false},
	ReasonChainBehind: {VerdictTransient, false}, ReasonDelegationUnknown: {VerdictTransient, false},
	ReasonSuspended: {VerdictWithhold, false}, ReasonDisputed: {VerdictWithhold, false},
	ReasonHeld: {VerdictWithhold, false}, ReasonErased: {VerdictWithhold, false},
	ReasonPendingErase: {VerdictWithhold, false}, ReasonRootRevoked: {VerdictWithhold, false},
	ReasonPendingFromActive: {VerdictWithhold, false}, ReasonPendingFromSuspended: {VerdictWithhold, false},
	ReasonPendingFromAbsent: {VerdictWithhold, false}, ReasonPinRevoked: {VerdictWithhold, false},
	ReasonFork: {VerdictWithhold, true}, ReasonHypothesisConflict: {VerdictWithhold, true},
	ReasonFieldMismatch: {VerdictWithhold, true}, ReasonANBMismatch: {VerdictWithhold, true},
	ReasonIssuedBounds: {VerdictWithhold, true}, ReasonPinChanged: {VerdictWithhold, true},
	ReasonMalformed: {VerdictWithhold, false}, ReasonBadSignature: {VerdictWithhold, false},
	ReasonCAKUnlisted: {VerdictWithhold, false}, ReasonTransitionInvalid: {VerdictWithhold, false},
	ReasonChainGap: {VerdictWithhold, false}, ReasonHeadMismatch: {VerdictWithhold, false},
	ReasonStatusFuture: {VerdictWithhold, false}, ReasonAbsentUnpinned: {VerdictWithhold, false},
	ReasonAbsentAfterCutover: {VerdictWithhold, false}, ReasonAbsentAfterEnrolled: {VerdictWithhold, false},
	ReasonDelegationWindow: {VerdictWithhold, false}, ReasonOutOfScope: {VerdictWithhold, false},
	ReasonStatusSuperseded: {VerdictWithhold, false}, ReasonRealmMismatch: {VerdictWithhold, false},
	ReasonTrustHalt: {VerdictHalt, true}, ReasonSenderSide: {VerdictRefuseOp, false},
	ReasonDeviceBehind: {VerdictTransient, false}, ReasonDeviceStatusStale: {VerdictTransient, false},
	ReasonDeviceStatusMismatch: {VerdictWithhold, false}, ReasonDeviceGap: {VerdictWithhold, true},
	ReasonDeviceFork: {VerdictWithhold, true}, ReasonDeviceRule: {VerdictWithhold, true},
	ReasonWrapInvalid: {VerdictWithhold, false},
}

// Verdict maps a Reason to §2.12's verdict. An unknown Reason fails closed.
func (r Reason) Verdict() Verdict {
	if c, ok := reasonTable[r]; ok {
		return c.v
	}
	return VerdictWithhold
}

// Notice reports §2.12's notice column: the member glyph plus a local
// diagnostics event, never a live-region announcement (FD T-37).
func (r Reason) Notice() bool { return reasonTable[r].notice }

// AllReasons lists every Reason, sorted.
func AllReasons() []Reason {
	out := make([]Reason, 0, len(reasonTable))
	for r := range reasonTable {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// reasonOf maps a codec error to its Reason: every A1 error wraps
// ErrMalformed (A1:6965), and any other error also fails closed as malformed.
func reasonOf(err error) Reason {
	if err == nil {
		return ReasonOK
	}
	return ReasonMalformed
}
