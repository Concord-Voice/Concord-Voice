package authoritycontract

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// The device-rule parity vectors (D-270, A6). gen.mjs's own DR rules (jsApply,
// jsNormalize) recorded every call a scripted account made, plus single-field
// mutations of the first call per op; the shared ApplyDeviceOp and
// NormalizeDeviceSet must give the same set or refusal. A disagreement is a
// defect on one side: settle it against the DoR row, never in vectors.json.
func init() {
	codecChecks["apply-device-op"] = checkApplyDeviceOpVector
	codecChecks["normalize"] = checkNormalizeVector
	codecChecks["for-op"] = checkForOpVector
	codecChecks["pattern"] = checkPatternVector
}

func vectorHeadInput(t *testing.T, h vectorHead) HeadInput {
	t.Helper()
	return HeadInput{State: h.State, Seq: h.Seq, EntryDigest: b64OrNil(t, h.EntryDigestB64),
		RootDigest: b64OrNil(t, h.RootDigestB64), ForceUnverified: h.ForceUnverified}
}

// An authority-function vector's reason is ok exactly when it admits, and a
// refusal reads as the Reason the vector names for its context (the PR-A2
// plan's rule): device_rule on own devices, malformed inside a published bundle.
func requireRefusalReason(t *testing.T, v vector, admitted bool, contexts ...Reason) {
	t.Helper()
	if admitted {
		require.Equal(t, string(ReasonOK), v.Expect.Reason, "an admitted call is ok")
		return
	}
	names := make([]string, len(contexts))
	for i, r := range contexts {
		names[i] = string(r)
	}
	require.Contains(t, names, v.Expect.Reason, "a refusal's reason")
}

func checkApplyDeviceOpVector(t *testing.T, v vector) {
	in := v.Input
	op := DeviceOpInput{Op: in.Op, Actor: in.Actor, Removed: in.Removed, Bind: b64OrNil(t, in.BindB64),
		BindWait: in.BindWait, PriorSig: in.PriorSig, RootForm: in.RootForm, Head: vectorHeadInput(t, in.Head),
		Outcome: in.Outcome, ProposingAnbMs: in.ProposingAnbMs}
	if in.NewDev != "" {
		e, err := ParseDev(in.NewDev)
		require.NoError(t, err)
		op.New = &e
	}
	set, ref := ApplyDeviceOp(mustB64(t, v.BytesB64), op, in.IssuedAtMs, in.DeltaMs)
	require.Equal(t, v.Expect.Refusal, string(ref))
	requireRefusalReason(t, v, ref == "", ReasonDeviceRule)
	if ref != "" {
		require.Nil(t, set, "a refused op returns no set")
		return
	}
	require.Equal(t, v.Expect.SetB64, base64.StdEncoding.EncodeToString(set))
}

func checkNormalizeVector(t *testing.T, v vector) {
	require.True(t, wantOK(v), "Normalize cannot refuse: a negative normalize vector needs its own handler")
	in := v.Input
	got := NormalizeDeviceSet(mustB64(t, v.BytesB64), in.Activate, in.BurnRecovery, in.CompleteSlot, in.ClearPending, in.EmptySlot)
	require.Equal(t, v.Expect.SetB64, base64.StdEncoding.EncodeToString(got))
}

// checkForOpVector is malformed_for_op at the authority. A refusal reads, for
// the published intent, as transition_invalid: a co-member sees a committed
// intent no row admits (decision A5); for a recovery-bind, as device_rule on
// own devices and malformed inside a published recover bundle.
func checkForOpVector(t *testing.T, v vector) {
	b, in := mustB64(t, v.BytesB64), v.Input
	var got Refusal
	var contexts []Reason
	switch in.Tag {
	case "intent":
		x, err := DecodeIntent(b)
		require.NoError(t, err)
		got, contexts = IntentForOp(x, in.Actor, in.Pop, in.RootCancels), []Reason{ReasonTransitionInvalid}
	case "recovery-bind":
		x, err := DecodeRecoveryBind(b)
		require.NoError(t, err)
		got, contexts = RecoveryBindForKind(x), []Reason{ReasonDeviceRule, ReasonMalformed}
	default:
		t.Fatalf("for-op vector %s: no checker for tag %q", v.Name, in.Tag)
	}
	require.Equal(t, v.Expect.Refusal, string(got))
	requireRefusalReason(t, v, got == "", contexts...)
}

// checkPatternVector is the relay-recover envelope rule (§2.6), which A2's
// CheckRelayRecover adds to A1's form-by-positions decode. Every error it
// returns wraps ErrMalformed.
func checkPatternVector(t *testing.T, v vector) {
	require.Equal(t, "relay-recover", v.Input.Tag, v.Name)
	_, err := CheckRelayRecover(mustB64(t, v.BytesB64))
	if err != nil {
		require.ErrorIs(t, err, ErrMalformed)
	}
	require.Equal(t, v.Expect.Reason, string(reasonOf(err)))
}
