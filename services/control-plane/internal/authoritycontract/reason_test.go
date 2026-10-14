package authoritycontract

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// DoR §2.12's two Reason tables, row by row.
func TestReasonVerdictTable(t *testing.T) {
	type row struct {
		v      Verdict
		notice bool
	}
	want := map[Reason]row{
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
	require.Len(t, AllReasons(), len(want))
	for _, r := range AllReasons() {
		w, ok := want[r]
		require.True(t, ok, "reason %q is not in the §2.12 tables", r)
		require.Equal(t, w.v, r.Verdict(), "verdict of %q", r)
		require.Equal(t, w.notice, r.Notice(), "notice of %q", r)
	}
	require.Equal(t, VerdictWithhold, Reason("no_such_reason").Verdict(), "an unknown reason fails closed")
	require.False(t, Reason("no_such_reason").Notice())
}

func TestVerdictStrings(t *testing.T) {
	require.Equal(t, []string{"ok", "transient", "withhold", "refuse-op", "halt"}, []string{
		VerdictOK.String(), VerdictTransient.String(), VerdictWithhold.String(), VerdictRefuseOp.String(), VerdictHalt.String()})
}

// An unassigned Verdict must never read as OK (fail closed).
func TestZeroVerdictIsWithhold(t *testing.T) {
	var v Verdict
	require.Equal(t, VerdictWithhold, v)
	require.Equal(t, "withhold", v.String())
}

func TestReasonOf(t *testing.T) {
	require.Equal(t, ReasonOK, reasonOf(nil))
	require.Equal(t, ReasonMalformed, reasonOf(fmt.Errorf("x: %w", ErrMalformed)))
	require.Equal(t, ReasonMalformed, reasonOf(errors.New("anything else fails closed as malformed")))
}

func TestParseUse(t *testing.T) {
	u, ok := ParseUse("new-secret")
	require.True(t, ok)
	require.Equal(t, UseNewSecret, u)
	u, ok = ParseUse("history")
	require.True(t, ok)
	require.Equal(t, UseHistory, u)
	_, ok = ParseUse("")
	require.False(t, ok)
}

func TestAddMs(t *testing.T) {
	s, ok := addMs(1, 2, 3)
	require.True(t, ok)
	require.Equal(t, uint64(6), s)
	s, ok = addMs(MaxSafeInt, 0)
	require.True(t, ok)
	require.Equal(t, MaxSafeInt, s)
	_, ok = addMs(MaxSafeInt, 1)
	require.False(t, ok, "a sum above 2^53−1 is refused (R2, DR5, DR9)")
	_, ok = addMs(MaxSafeInt + 1)
	require.False(t, ok)
}

func TestMsOf(t *testing.T) {
	require.Equal(t, uint64(1_700_000_000_123), msOf(time.UnixMilli(1_700_000_000_123)))
	require.Equal(t, uint64(0), msOf(time.UnixMilli(-5)), "a time before the epoch reads as 0")
}

func TestMaxHW(t *testing.T) {
	a := HighWater{Gen: 0, Serial: 5, MinValid: 5}
	b := HighWater{Gen: 1, Serial: 1, MinValid: 1}
	require.Equal(t, b, maxHW(a, b))
	require.Equal(t, b, maxHW(b, a))
	require.Equal(t, a, maxHW(a, HighWater{Gen: 0, Serial: 4, MinValid: 1}))
}

func TestSigMsgIsTheSigLayout(t *testing.T) {
	d := digestOf("intent")
	require.Equal(t, Sig{Role: RoleOperator, Digest: d}.Encode(), sigMsg(RoleOperator, d))
}

func TestHaltError(t *testing.T) {
	var err error = &HaltError{Rule: "T4 succession contradiction"}
	var h *HaltError
	require.True(t, errors.As(err, &h))
	require.Contains(t, err.Error(), "T4")
	var none *HaltError
	require.Equal(t, "authoritycontract: trust halt", none.Error(), "a nil *HaltError must not panic")
}
