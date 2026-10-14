package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A2 extends A1's harness (A1 Task 14) without renaming any of it: every
// Reason's verdict comes from production code, and the A2 field names join
// the closed set. Vector types are registered by the task that emits them.
func init() {
	for _, r := range AllReasons() {
		verdictOf[string(r)] = r.Verdict().String()
	}
	for _, k := range []string{
		"anchors", "hw", "gen", "serial", "min_valid", "statuses",
		"head_b64", "device_head_b64", "set_b64", "refusal", "wrap_use",
		"tag", "op", "actor", "new_dev", "removed", "bind_b64", "bind_wait", "prior_sig", "root_form",
		"head", "state", "seq", "entry_digest_b64", "root_digest_b64", "force_unverified",
		"outcome", "proposing_anb_ms", "delta_ms",
		"activate", "burn_recovery", "complete_slot", "clear_pending", "empty_slot",
		"pin_b64", "other_b64", "mode", "pop", "root_cancels", "candidate_b64",
	} {
		allowedKeys[k] = true
	}
}

func TestA2HarnessKeepsOnePrivateName(t *testing.T) {
	TestAllowedKeysHaveOnePrivateName(t)
	require.Equal(t, "withhold", verdictOf["transition_invalid"])
	require.Equal(t, "halt", verdictOf["trust_halt"])
}

// b64OrNil decodes a vector's optional base64 field; "" is nil.
func b64OrNil(t *testing.T, s string) []byte {
	t.Helper()
	if s == "" {
		return nil
	}
	return mustB64(t, s)
}

func memOf(t *testing.T, m vectorMem) AccountMemory {
	t.Helper()
	// everEnrolled is u01: any other value is a broken vector, not "never enrolled"
	// (which is the more permissive reading, since it skips absent_after_enrolled).
	require.LessOrEqual(t, m.EverEnrolled, uint64(1), "everEnrolled is u01")
	return AccountMemory{Pin: b64OrNil(t, m.Pin), EverEnrolled: m.EverEnrolled == 1, AcceptedPin: b64OrNil(t, m.AcceptedPin)}
}

// hwOf is a conversion: vectorHW and HighWater have the same fields, so a
// change to either fails to compile here.
func hwOf(h vectorHW) HighWater { return HighWater(h) }

func listB64(t *testing.T, xs []string) [][]byte {
	t.Helper()
	out := make([][]byte, len(xs))
	for i, s := range xs {
		out[i] = mustB64(t, s)
	}
	return out
}

// The shared decoders read §2.12's persisted forms: "" is no value, and
// everEnrolled is u01.
func TestVectorDecoders(t *testing.T) {
	pin, other := digestOf("pin"), digestOf("accepted")
	require.Nil(t, b64OrNil(t, ""))
	require.Equal(t, pin, b64OrNil(t, b64Of(pin)))
	require.Equal(t, AccountMemory{}, memOf(t, vectorMem{}))
	require.Equal(t, AccountMemory{Pin: pin, EverEnrolled: true, AcceptedPin: other},
		memOf(t, vectorMem{Pin: b64Of(pin), EverEnrolled: 1, AcceptedPin: b64Of(other)}))
	require.Equal(t, HighWater{Gen: 1, Serial: 2, MinValid: 3}, hwOf(vectorHW{Gen: 1, Serial: 2, MinValid: 3}))
	require.Equal(t, [][]byte{pin, other}, listB64(t, []string{b64Of(pin), b64Of(other)}))
	require.Empty(t, listB64(t, nil))
}

// Every Reason the verifier can return has at least one vector expecting it
// (DoR §3.1: "every Reason in §2.12 has at least one vector"). network and
// sender_side are the caller's and the verifier never returns them (plan A1).
func TestEveryReasonHasAVector(t *testing.T) {
	require.Empty(t, reasonsWithoutAVector(loadVectors(t).Vectors), "no vector expects these Reasons")
}

// reasonsWithoutAVector lists, sorted, every Reason the verifier can return
// that no vector in vs expects.
func reasonsWithoutAVector(vs []vector) []Reason {
	seen := map[string]bool{}
	for _, v := range vs {
		seen[v.Expect.Reason] = true
	}
	var out []Reason
	for _, r := range AllReasons() {
		if r != ReasonNetwork && r != ReasonSenderSide && !seen[string(r)] {
			out = append(out, r)
		}
	}
	return out
}

// The coverage check binds: without the vectors that expect one Reason it
// names exactly that Reason, for every Reason in turn, and it never asks for
// the two the verifier does not return.
func TestEveryReasonHasAVectorNamesTheMissingReason(t *testing.T) {
	all := loadVectors(t).Vectors
	for _, r := range AllReasons() {
		if r == ReasonNetwork || r == ReasonSenderSide {
			continue
		}
		rest := make([]vector, 0, len(all))
		for _, v := range all {
			if v.Expect.Reason != string(r) {
				rest = append(rest, v)
			}
		}
		require.Equal(t, []Reason{r}, reasonsWithoutAVector(rest), "without the vectors expecting %q", r)
	}
	none := reasonsWithoutAVector(nil)
	require.Len(t, none, len(AllReasons())-2)
	require.NotContains(t, none, ReasonNetwork)
	require.NotContains(t, none, ReasonSenderSide)
}
