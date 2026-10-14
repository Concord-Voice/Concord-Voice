package authoritycontract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func ev(mfa bool, factors ...string) Evidence {
	return Evidence{Factors: factors, MFAEnrolled: mfa, PasswordLogin: true, RecoveryWindowClear: true}
}

// DoR §3.1 "Floors (LD-36, D-248; Go only)".
func TestFloorsLD36(t *testing.T) {
	cases := []struct {
		name  string
		f     FloorName
		row   int
		actor Actor
		e     Evidence
		want  bool
	}{
		{"passkey meets S's MFA leg", FloorS, 5, ActorUser, ev(true, FactorPasskey), true},
		{"passkey meets H's MFA leg", FloorH, 7, ActorUser, ev(true, FactorPassword, FactorPasskey), true},
		{"passkey meets R_full's MFA leg", FloorRFull, 11, ActorUser, ev(true, FactorPassword, FactorPasskey, FactorEmail), true},
		{"passkey meets R_email's MFA leg", FloorREmail, 11, ActorUser, ev(true, FactorPasskey, FactorEmail), true},
		{"passkey alone is never a primary leg (no MFA)", FloorS, 5, ActorUser, ev(false, FactorPasskey), false},
		{"passkey alone is never H's primary leg", FloorH, 7, ActorUser, ev(true, FactorPasskey), false},
		{"backup-code meets H", FloorH, 7, ActorUser, ev(true, FactorPassword, FactorBackupCode), true},
		{"backup-code meets R_full", FloorRFull, 11, ActorUser, ev(true, FactorPassword, FactorBackupCode, FactorEmail), true},
		{"backup-code meets R_email", FloorREmail, 11, ActorUser, ev(true, FactorEmail, FactorBackupCode), true},
		{"backup-code fails S", FloorS, 5, ActorUser, ev(true, FactorPassword, FactorBackupCode), false},
		{"totp never stands for a backup code (and meets S)", FloorS, 5, ActorUser, ev(true, FactorTOTP), true},
		{"R_rk: recovery-key + password", FloorRRk, 11, ActorUser, ev(false, FactorRecoveryKey, FactorPassword), true},
		{"R_rk: recovery-key + email", FloorRRk, 12, ActorUser, ev(false, FactorRecoveryKey, FactorEmail), true},
		{"R_rk refused with operator_assisted", FloorRRk, 11, ActorUser,
			Evidence{Factors: []string{FactorRecoveryKey, FactorPassword}, PasswordLogin: true, OperatorAssisted: true}, false},
		{"R_rk refused when a recovery key is bound", FloorRRk, 11, ActorUser,
			Evidence{Factors: []string{FactorRecoveryKey, FactorPassword}, PasswordLogin: true, RecoveryKeyBound: true}, false},
		{"R_rk refused on row 3", FloorRRk, 3, ActorUser, ev(false, FactorRecoveryKey, FactorPassword), false},
		{"R_rk refused on row 2", FloorRRk, 2, ActorUser, ev(false, FactorRecoveryKey, FactorPassword), false},
		{"device-key meets S nothing", FloorS, 5, ActorUser, ev(true, FactorDeviceKey), false},
		{"device-key meets H nothing", FloorH, 7, ActorUser, ev(false, FactorDeviceKey), false},
		{"email-backup alone meets R_full's email leg", FloorRFull, 11, ActorUser, ev(false, FactorPassword, FactorEmailBackup), true},
		{"email-backup alone meets R_email", FloorREmail, 11, ActorUser, ev(false, FactorEmailBackup), true},
		{"email-backup alone meets R_rk's email leg", FloorRRk, 11, ActorUser, ev(false, FactorRecoveryKey, FactorEmailBackup), true},
		{"superset: extra factors still meet S", FloorS, 5, ActorUser,
			ev(true, FactorEmail, FactorPassword, FactorSession, FactorTOTP), true},
		{"SSO account: sso is S's primary leg", FloorS, 5, ActorUser,
			Evidence{Factors: []string{FactorSSO}}, true},
		{"SSO account: password is not its primary leg", FloorS, 5, ActorUser,
			Evidence{Factors: []string{FactorPassword}}, false},
		{"R_full needs recovery_window_clear", FloorRFull, 11, ActorUser,
			Evidence{Factors: []string{FactorPassword, FactorEmail}, PasswordLogin: true}, false},
		{"OP needs actor operator and admin-webauthn", FloorOP, 8, ActorOperator, ev(false, FactorAdminWebAuthn), true},
		{"OP refuses actor user", FloorOP, 8, ActorUser, ev(false, FactorAdminWebAuthn), false},
		{"REG", FloorREG, 1, ActorUser, ev(false, FactorRegistration), true},
		{"ERASE_LIST", FloorEraseList, 18, ActorUser, ev(false, FactorErasureList), true},
		{"NONE", FloorNone, 13, ActorUser, ev(false), true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, meets(c.f, c.actor, c.row, c.e), c.name)
	}
}

func TestFloorByRow(t *testing.T) {
	require.True(t, Floor(Op(OpEnrollNew), Actor(ActorUser), 1, ev(false, FactorRegistration)))
	require.False(t, Floor(Op(OpEnrollNew), Actor(ActorUser), 1, ev(false, FactorPassword)))
	require.True(t, Floor(Op(OpReplaceRoot), Actor(ActorUser), 11, ev(false, FactorRecoveryKey, FactorPassword)), "R_rk on row 11")
	require.False(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 3, ev(false, FactorRecoveryKey, FactorPassword)), "R_rk not row 3")
	// Row 4: S with a PoP under a live pin; R_full or R_email otherwise.
	withPoP := ev(true, FactorTOTP)
	withPoP.PoP, withPoP.LegacyPinLive = true, true
	require.True(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 4, withPoP))
	require.False(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 4, ev(true, FactorTOTP)))
	require.True(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 4, ev(false, FactorEmail)))
	// Open question 9, ruled: a PoP under a revoked pin, or with liveness
	// left unset, does not lower row 4; the PoP-less floors still apply.
	revoked := ev(true, FactorTOTP)
	revoked.PoP = true
	require.False(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 4, revoked), "PoP under a revoked pin")
	revokedEmail := ev(false, FactorEmail)
	revokedEmail.PoP = true
	require.True(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 4, revokedEmail), "R_email still meets row 4")
	livePinOnly := ev(true, FactorTOTP)
	livePinOnly.LegacyPinLive = true
	require.False(t, Floor(Op(OpEnrollExisting), Actor(ActorUser), 4, livePinOnly), "a live pin without a PoP")
	require.True(t, Floor(Op(OpErase), Actor(ActorUser), 18, ev(false, FactorErasureList)))
	require.True(t, Floor(Op(OpErase), Actor(ActorUser), 18, ev(false, FactorPassword)))
	require.False(t, Floor(Op(OpErase), Actor(ActorOperator), 18, ev(false, FactorPassword)), "actor must match exactly")
	require.True(t, Floor(Op(OpAddDevice), Actor(ActorUser), 0, ev(true, FactorWebAuthn)))
	require.False(t, Floor(Op(OpAddDevice), Actor(ActorUser), 0, ev(true, FactorBackupCode)), "add_device is S")
	require.True(t, Floor(Op(OpBindRecovery), Actor(ActorUser), 0, ev(true, FactorPassword, FactorBackupCode)), "bind is H")
	require.True(t, Floor(Op(OpCancelRecoveryBind), Actor(ActorUser), 0, ev(false)))
	require.False(t, Floor(Op(OpRotateEK), Actor(ActorUser), 99, ev(false, FactorPassword)), "no such row")
	require.False(t, Floor(Op(OpRotateEK), Actor(ActorUser), 5, ev(true, FactorTOTP)), "op must match the row")
}

// DoR:560 — H's MFA leg is required whenever the account is MFA-enrolled: a
// password alone does not meet it, and the same evidence meets it with no
// enrolment (a primary leg alone) or with an MFA factor added.
func TestFloorHNeedsTheMFALegWhenEnrolled(t *testing.T) {
	require.False(t, meets(FloorH, Actor(ActorUser), 7, ev(true, FactorPassword)), "enrolled, password only")
	require.False(t, Floor(Op(OpSuspend), Actor(ActorUser), 7, ev(true, FactorPassword)), "row 7 through Floor")
	require.True(t, meets(FloorH, Actor(ActorUser), 7, ev(false, FactorPassword)), "control: not enrolled")
	require.True(t, meets(FloorH, Actor(ActorUser), 7, ev(true, FactorPassword, FactorTOTP)), "control: enrolled with the MFA leg")
}

// DoR:562 — R_email carries the MFA leg too: an email factor alone, with the
// account MFA-enrolled, meets neither R_email nor (through it) row 11.
func TestFloorREmailNeedsTheMFALegWhenEnrolled(t *testing.T) {
	require.False(t, meets(FloorREmail, Actor(ActorUser), 11, ev(true, FactorEmail)), "enrolled, email only")
	require.False(t, meets(FloorREmail, Actor(ActorUser), 11, ev(true, FactorEmailBackup)), "enrolled, email-backup only")
	require.False(t, Floor(Op(OpReplaceRoot), Actor(ActorUser), 11, ev(true, FactorEmail)), "row 11 through Floor")
	require.True(t, meets(FloorREmail, Actor(ActorUser), 11, ev(false, FactorEmail)), "control: not enrolled")
	require.True(t, meets(FloorREmail, Actor(ActorUser), 11, ev(true, FactorEmail, FactorTOTP)), "control: enrolled with the MFA leg")
	require.True(t, Floor(Op(OpReplaceRoot), Actor(ActorUser), 11, ev(true, FactorEmail, FactorTOTP)), "control: row 11 through Floor")
}

// DoR:561 — R_full is refused when the recovery is operator-assisted, with
// every other leg present.
func TestFloorRFullRefusedWithOperatorAssisted(t *testing.T) {
	e := ev(true, FactorPassword, FactorTOTP, FactorEmail)
	require.True(t, meets(FloorRFull, Actor(ActorUser), 11, e), "control: every leg present")
	e.OperatorAssisted = true
	require.False(t, meets(FloorRFull, Actor(ActorUser), 11, e), "operator_assisted")
	n := ev(false, FactorPassword, FactorEmail)
	require.True(t, meets(FloorRFull, Actor(ActorUser), 11, n), "control: not enrolled")
	n.OperatorAssisted = true
	require.False(t, meets(FloorRFull, Actor(ActorUser), 11, n), "operator_assisted, not enrolled")
}

func TestEvidenceOf(t *testing.T) {
	e := EvidenceOf(CPEvidence{Factors: []string{FactorPassword}, MFAEnrolled: 1, PasswordLogin: 1, RecoveryWindowClear: 0, OperatorAssisted: 1})
	require.Equal(t, Evidence{Factors: []string{FactorPassword}, MFAEnrolled: true, PasswordLogin: true, OperatorAssisted: true}, e)
}

// A cp-evidence bit outside u01 never arrives through the codec, but
// EvidenceOf takes a plain struct. mfa_enrolled and password_login have no
// reading that is stricter in every floor (S swaps legs on either value), so
// an out-of-range bit yields evidence that meets no floor except NONE. Before
// this, operator_assisted = 2 read as 0 and met R_full and R_rk.
func TestEvidenceOfFailsClosedOnABitOutsideU01(t *testing.T) {
	full := CPEvidence{Factors: []string{FactorPassword, FactorEmail, FactorRecoveryKey, FactorTOTP},
		MFAEnrolled: 1, PasswordLogin: 1, RecoveryWindowClear: 1}
	require.True(t, Floor(Op(OpReplaceRoot), Actor(ActorUser), 11, EvidenceOf(full)), "control: the in-range evidence meets row 11")
	for name, mut := range map[string]func(*CPEvidence){
		"mfa_enrolled":          func(c *CPEvidence) { c.MFAEnrolled = 2 },
		"password_login":        func(c *CPEvidence) { c.PasswordLogin = 2 },
		"recovery_window_clear": func(c *CPEvidence) { c.RecoveryWindowClear = 2 },
		"operator_assisted":     func(c *CPEvidence) { c.OperatorAssisted = 2 },
	} {
		c := full
		mut(&c)
		e := EvidenceOf(c)
		for _, f := range []FloorName{FloorS, FloorH, FloorRFull, FloorREmail, FloorRRk, FloorREG, FloorEraseList, FloorOP} {
			require.False(t, meets(f, Actor(ActorUser), 11, e), "%s = 2 meets %s", name, f)
			require.False(t, meets(f, Actor(ActorOperator), 11, e), "%s = 2 meets %s (operator)", name, f)
		}
		require.True(t, meets(FloorNone, Actor(ActorUser), 13, e), "NONE has no requirement")
		require.False(t, Floor(Op(OpReplaceRoot), Actor(ActorUser), 11, e), "%s = 2 meets row 11", name)
	}
}
