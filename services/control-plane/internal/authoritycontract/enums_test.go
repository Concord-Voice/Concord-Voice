package authoritycontract

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClosedSets(t *testing.T) {
	for name, c := range map[string]struct {
		set  []string
		want int
	}{
		"published ops (§2.5 #6)":          {publishedOps, 11},
		"device-intent ops (§2.5 #6)":      {deviceOps, 10},
		"delegation scope (§2.5 #14)":      {scopeValues, 15},
		"factor vocabulary (LD-33, LD-41)": {factorNames, 14},
		"refusal codes (§3.3)":             {refusalCodes, 27},
		"error codes (§3.3)":               {errorCodes, 15},
		"states (§2.5 result #10)":         {stateValues, 6},
		"sig roles (§2.5 sig)":             {sigRoles, 6},
	} {
		require.Len(t, c.set, c.want, name)
		sorted := slices.Clone(c.set)
		slices.Sort(sorted)
		require.Len(t, slices.Compact(sorted), c.want, "%s has a duplicate", name)
	}
	require.NotContains(t, factorNames, "backup-email-recovery", "LD-33 dropped it")
	require.Contains(t, factorNames, FactorEmailBackup, "LD-41 added it")
}

// TestClosedSetMembership pins every closed set to the DoR's own wording, as
// literals that do not go through the constants, so a typo in a constant or a
// dropped member fails here. The sets are frozen in protocol v1 (§2.3), and a
// change to any of them is protocol v2.
func TestClosedSetMembership(t *testing.T) {
	eleven := []string{"cancel_pending", "enroll_existing", "enroll_new", "erase", "recover", "replace_root",
		"revoke_root", "rotate_ek", "rotate_root", "suspend", "unsuspend"}

	for name, c := range map[string]struct{ got, want []string }{
		// §2.5 intent #6: the 11 published ops.
		"publishedOps": {publishedOps, eleven},
		// §2.5 device-intent #6 and device-result #8: the 10 device-ledger ops.
		"deviceOps": {deviceOps, []string{"add_device", "bind_recovery", "cancel_recovery_bind",
			"enroll_existing", "enroll_new", "recover", "replace_root", "rotate_ek", "rotate_root", "unsuspend"}},
		// §2.5 delegation #14: the 11 published ops, the three private ops, and status.
		"scopeValues": {scopeValues, append(slices.Clone(eleven), "add_device", "bind_recovery", "cancel_recovery_bind", "status")},
		// §2.5 result #10 and status #7.
		"stateValues": {stateValues, []string{"absent", "active", "suspended", "pending", "disputed", "erased"}},
		// §2.5 result #11 and status #8: "" unless state is pending or disputed.
		"priorStateValues": {priorStateValues, []string{"", "absent", "active", "suspended"}},
		// §2.5 result #23: "" leaves the assurance unchanged (§2.8).
		"assuranceValues": {assuranceValues, []string{"", "initial", "continuity", "recovery", "unverified"}},
		// §2.8 and §2.12 chain-head root_index: a segment always has an assurance.
		"segmentAssurances": {segmentAssurances, []string{"initial", "continuity", "recovery", "unverified"}},
		// §2.5 result #12 and status #9: suspended_by.
		"suspenders": {suspenders, []string{"operator", "user"}},
		// §2.5 cp-authz actor.
		"actorValues": {actorValues, []string{"operator", "restore", "user"}},
		// §2.5 sig role.
		"sigRoles": {sigRoles, []string{"actor-device", "candidate", "current-root", "new-device", "operator", "recovery-key"}},
		// §2.5 cp-evidence factors: the 14-name vocabulary (LD-33, LD-41).
		"factorNames": {factorNames, []string{"admin-webauthn", "backup-code", "device-key", "email", "email-backup",
			"erasure-list", "passkey", "password", "recovery-key", "registration", "session", "sso", "totp", "webauthn"}},
		// §2.22 recovery-bind #6 kind.
		"bindKinds": {bindKinds, []string{"bind", "cancel", "unbind"}},
		// §2.5 device-intent #19 bind_wait: "" on the old-key forms and every other op.
		"bindWaits": {bindWaits, []string{"", "delta", "now", "replace"}},
		// §2.6 signed type.
		"signedTypes": {signedTypes, []string{"delegation", "device-status", "status", "succession"}},
		// §2.5 device-secrets check 1 and §2.12 device-head secrets records: an add_device
		// admitting self, or a rotate_root or rotate_ek with self in its active set.
		"secretsOps": {secretsOps, []string{"add_device", "rotate_ek", "rotate_root"}},
		// §2.6 signer role.
		"nodeRoles": {nodeRoles, []string{"leader", "standby"}},
		// §2.5 ek-binding ek_alg.
		"ekAlgs": {ekAlgs, []string{"rsa-oaep-4096-sha256"}},
		// §3.3 refusal codes (27, recorded).
		"refusalCodes": {refusalCodes, []string{"authz_out_of_window", "device_key_bound", "device_limit",
			"device_too_new", "device_unknown", "ek_generation", "ek_mismatch", "ek_reused", "expired",
			"expiry_out_of_range", "held", "in_population", "insufficient_authorization", "legacy_mismatch",
			"malformed_for_op", "policy_disabled", "predecessor_mismatch", "recovery_key_bound",
			"recovery_key_unbound", "root_bound_elsewhere", "root_revoked", "root_reused", "stale_device_seq",
			"stale_head", "stale_recovery_gen", "stale_seq", "wrong_state"}},
		// §3.3 error enum (15).
		"errorCodes": {errorCodes, []string{"malformed", "bad_signature", "bad_authz", "request_conflict",
			"too_large", "store", "signer", "clock", "reconcile", "not_sealed", "busy", "not_ready", "not_leader",
			"replication", "disk"}},
	} {
		// ElementsMatch compares as multisets, so a duplicate in got fails too.
		require.ElementsMatch(t, c.want, c.got, name)
	}
}

// The three op sets are views of one vocabulary (§2.5): every device op and
// every published op is a delegation scope value, every secrets op is a device
// op, and the scope adds only the private ops that have no published intent,
// plus status.
func TestOpSetsAreConsistent(t *testing.T) {
	for _, op := range publishedOps {
		require.Contains(t, scopeValues, op)
	}
	for _, op := range deviceOps {
		require.Contains(t, scopeValues, op)
	}
	for _, op := range secretsOps {
		require.Contains(t, deviceOps, op)
	}
	for _, op := range []string{OpAddDevice, OpBindRecovery, OpCancelRecoveryBind} {
		require.NotContains(t, publishedOps, op, "a private op has no published intent (LD-32, LD-34)")
		require.Contains(t, deviceOps, op)
	}
	require.NotContains(t, deviceOps, ScopeStatus)
	require.NotContains(t, publishedOps, ScopeStatus)
	require.Contains(t, scopeValues, ScopeStatus)

	// "" is a member only where the DoR allows the position to be empty.
	for name, set := range map[string][]string{"state": stateValues, "actor": actorValues, "segment": segmentAssurances} {
		require.NotContains(t, set, "", name)
	}
	require.Contains(t, priorStateValues, "")
	require.Contains(t, assuranceValues, "")
	require.Contains(t, bindWaits, "")
}

// Refusal and ErrorCode are distinct types so one cannot be passed for the
// other; a refusal code is never an error code.
func TestRefusalAndErrorCodesAreDisjoint(t *testing.T) {
	for _, c := range refusalCodes {
		require.NotContains(t, errorCodes, c)
	}
}
