package authoritycontract

import "slices"

// Evidence is what the authority-only floors read: the cp-evidence factors
// and bits, plus the private head's "recovery key bound" (R_rk, D-248),
// whether a valid PoP accompanies the op (row 4), and whether the legacy pin
// that PoP proves is live: the from-state's legacy_pin_revoked is 0.
type Evidence struct {
	Factors                                                           []string
	MFAEnrolled, PasswordLogin, RecoveryWindowClear, OperatorAssisted bool
	RecoveryKeyBound                                                  bool
	PoP, LegacyPinLive                                                bool
}

// EvidenceOf reads a decoded cp-evidence. RecoveryKeyBound, PoP and
// LegacyPinLive are the caller's: they come from the private head, the bundle
// and the from-state, not the evidence.
//
// The codec admits only 0 and 1 for the four bits, but CPEvidence is a plain
// struct. A bit outside u01 yields evidence with no factors, which meets no
// floor except NONE: mfa_enrolled and password_login have no reading that is
// stricter in every floor (S changes legs on either value), and reading
// operator_assisted = 2 as 0 would meet R_full and R_rk.
func EvidenceOf(ev CPEvidence) Evidence {
	if ev.MFAEnrolled > 1 || ev.PasswordLogin > 1 || ev.RecoveryWindowClear > 1 || ev.OperatorAssisted > 1 {
		return Evidence{}
	}
	return Evidence{Factors: ev.Factors, MFAEnrolled: ev.MFAEnrolled == 1, PasswordLogin: ev.PasswordLogin == 1,
		RecoveryWindowClear: ev.RecoveryWindowClear == 1, OperatorAssisted: ev.OperatorAssisted == 1}
}

func (e Evidence) has(n string) bool { return slices.Contains(e.Factors, n) }

// primary is the primary leg: password when the account logs in by
// password, otherwise sso. passkey is never a primary leg.
func (e Evidence) primary() bool {
	if e.PasswordLogin {
		return e.has(FactorPassword)
	}
	return e.has(FactorSSO)
}

// mfaS is S's MFA leg; backup-code never meets it (LD-36, D-248).
func (e Evidence) mfaS() bool {
	return e.has(FactorTOTP) || e.has(FactorWebAuthn) || e.has(FactorPasskey)
}

// mfaH is the MFA leg of H, R_full and R_email.
func (e Evidence) mfaH() bool { return e.mfaS() || e.has(FactorBackupCode) }

// email is `email` ∨ `email-backup` (LD-41, D-263).
func (e Evidence) email() bool { return e.has(FactorEmail) || e.has(FactorEmailBackup) }

func (e Evidence) s() bool {
	if e.MFAEnrolled {
		return e.mfaS()
	}
	return e.primary()
}

func (e Evidence) h() bool { return e.primary() && (!e.MFAEnrolled || e.mfaH()) }

func (e Evidence) rFull() bool {
	return e.h() && e.email() && e.RecoveryWindowClear && !e.OperatorAssisted
}

func (e Evidence) rEmail() bool { return e.email() && (!e.MFAEnrolled || e.mfaH()) }

// rRk is LD-36's floor: rows 11 and 12 only, and only with no key bound.
func (e Evidence) rRk(row int) bool {
	return (row == 11 || row == 12) && e.has(FactorRecoveryKey) && (e.has(FactorPassword) || e.email()) &&
		!e.OperatorAssisted && !e.RecoveryKeyBound
}

// meets evaluates one floor. Every predicate tests membership, so a
// superset of the required factors always meets it (§2.8).
func meets(f FloorName, actor Actor, row int, e Evidence) bool {
	switch f {
	case FloorS:
		return e.s()
	case FloorH:
		return e.h()
	case FloorRFull:
		return e.rFull()
	case FloorREmail:
		return e.rEmail()
	case FloorRRk:
		return e.rRk(row)
	case FloorREG:
		return e.has(FactorRegistration)
	case FloorEraseList:
		return e.has(FactorErasureList)
	case FloorNone:
		return true
	case FloorOP:
		return actor == Actor(ActorOperator) && e.has(FactorAdminWebAuthn)
	}
	return false
}

// Floor is §2.8's pure floor check for a published row (1–22) or, with row
// 0, the private op `op` names (V2-06). op and actor MUST match the row.
func Floor(op Op, actor Actor, row int, ev Evidence) bool {
	var r Row
	var ok bool
	if row == 0 {
		r, ok = PrivateRow(string(op))
	} else {
		r, ok = RowByN(row)
	}
	if !ok || r.Op != string(op) || r.Actor != string(actor) {
		return false
	}
	need := r.Floors // any one suffices
	// Row 4's PoP floor needs a PoP under a live pin: with a revoked legacy key
	// and the password, the PoP floor would let anyone dispute-burn every
	// R_full or R_email enrolment (open question 9, ruled). LegacyPinLive's
	// zero value refuses it, so a caller that never fills it gets the stricter
	// floor.
	if ev.PoP && ev.LegacyPinLive && len(r.PoPFloors) > 0 {
		need = r.PoPFloors
	}
	for _, f := range need {
		if meets(f, actor, row, ev) {
			return true
		}
	}
	return false
}
