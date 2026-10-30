package decide

import (
	"sort"
	"time"
)

// Rank returns the kind's rank as a target (parent §6.1): the K kinds (password, addresses,
// SSO, email MFA) are Rank1, the P kinds (TOTP, WebAuthn, backup-code set) Rank2, and the
// anchors (recovery key, recovery device) Rank3. An unknown kind is RankInvalid.
func (k CredKind) Rank() Rank {
	switch k {
	case CredPassword, CredAddress, CredBackupAddress, CredSSO, CredEmailMFA:
		return Rank1
	case CredTOTP, CredWebAuthn, CredBackupCodeSet:
		return Rank2
	case CredRecoveryKey, CredRecoveryDevice:
		return Rank3
	default:
		return RankInvalid
	}
}

// composition counts the distinct counted credentials of a proof by the categories of the
// parent §6.1 class table.
type composition struct {
	k     int // knowledge and channel: password, addresses, SSO, email MFA
	paper int // a backup-code set, or a possession factor or anchor flagged Paper
	np0   int // non-paper TOTP, or WebAuthn whose assertion is in Proof.DeviceBound (BE=0)
	np1   int // any other non-paper WebAuthn: a synced passkey, or one with no BE=0 assertion
	a     int // anchors
}

// add counts one credential. A K credential stays K whatever its Paper flag, because
// reading it as paper would raise "two Ks" (C1) to "K + P" (C2). Any other credential
// flagged Paper counts as paper, which never raises a class. deviceBound reports that THIS
// assertion of a WebAuthn carried BE=0; it is read for nothing else.
func (comp *composition) add(c Cred, deviceBound bool) {
	switch c.Ref.Kind.Rank() {
	case Rank1:
		comp.k++
	case Rank2, Rank3:
		comp.addPossessionOrAnchor(c, deviceBound)
	}
}

func (comp *composition) addPossessionOrAnchor(c Cred, deviceBound bool) {
	switch {
	case c.Paper || c.Ref.Kind == CredBackupCodeSet:
		comp.paper++
	case c.Ref.Kind.Rank() == Rank3:
		comp.a++
	case c.Ref.Kind == CredWebAuthn && !deviceBound:
		// Red-team F1: a WebAuthn is device-bound only on the evidence of its own assertion.
		comp.np1++
	default:
		// TOTP counts as non-backup-eligible although its seed may sync (parent §6.1).
		comp.np0++
	}
}

// class applies the parent §6.1 table, first match wins.
func (comp composition) class() Class {
	nonPaperP := comp.np0 + comp.np1
	anyP := comp.paper + nonPaperP
	switch {
	case comp.a >= 1 && comp.np0 >= 1:
		return C3
	case comp.k >= 1 && anyP >= 1, nonPaperP >= 1, comp.a >= 1 && comp.k >= 1,
		comp.a >= 1 && comp.paper >= 1, comp.a >= 2:
		return C2
	case comp.k >= 1, comp.paper >= 1, comp.a >= 1:
		return C1
	default:
		return ClassNone
	}
}

// ClassOf returns the class of proof p over creds at the reference instant ref (parent
// §6.1). Only distinct refs that name a credential exactly (kind, ID and version) and count
// at ref contribute: a duplicate, stale-version, unknown, immature or contested ref adds
// nothing. ref is Now for T2, T3, T6 and T1b, the request's OpenedAt for T7, the contested
// enrolment's EnrolledAt for T5, and the earliest DisputedAt for T12.
//
// A WebAuthn is a device-bound P (it pairs with an anchor into C3) only when its Used ref
// also appears exactly in p.DeviceBound; otherwise it is a synced P. DeviceBound is consulted
// only for a Used ref that already contributes, so a DeviceBound ref outside Used, at another
// version, or of another kind adds nothing (red-team F1).
func ClassOf(p Proof, creds []Cred, ref time.Time) Class {
	var comp composition
	for _, r := range sortRefs(p.Used) {
		if c, ok := findCred(creds, r); ok && counts(c, ref) {
			comp.add(c, containsRef(p.DeviceBound, r))
		}
	}
	return comp.class()
}

// LoginFloor reports whether p meets the login floor (parent §6.1): the K leg (an SSO
// identity when ssoLogin, otherwise the password) plus, on an account that holds an
// uncontested TOTP, WebAuthn or backup-code set, one of those as the inline factor. Maturity
// is ignored (F5: an immature password still signs in), a contested credential is excluded,
// and refs match exactly. An address or email-MFA code never meets the inline-factor leg
// (mutation #26). The zero ssoLogin is a password account (H1), so a caller that never
// loaded the login mode cannot let an SSO assertion alone meet a password account's floor.
func LoginFloor(p Proof, creds []Cred, ssoLogin bool) bool {
	kLeg := CredPassword
	if ssoLogin {
		kLeg = CredSSO
	}
	if !proofHasLive(p, creds, func(k CredKind) bool { return k == kLeg }) {
		return false
	}
	if !accountHasInlineFactor(creds) {
		return true
	}
	return proofHasLive(p, creds, isInlineFactor)
}

// isInlineFactor reports whether a kind is an inline factor at sign-in.
func isInlineFactor(k CredKind) bool {
	return k == CredTOTP || k == CredWebAuthn || k == CredBackupCodeSet
}

// accountHasInlineFactor reports whether the account is an MFA account: it holds an
// uncontested inline factor.
func accountHasInlineFactor(creds []Cred) bool {
	for _, c := range creds {
		if isInlineFactor(c.Ref.Kind) && c.ContestedAt.IsZero() {
			return true
		}
	}
	return false
}

// proofHasLive reports whether p uses an uncontested credential of a kind that want accepts.
func proofHasLive(p Proof, creds []Cred, want func(CredKind) bool) bool {
	for _, r := range p.Used {
		if !want(r.Kind) {
			continue
		}
		if c, ok := findCred(creds, r); ok && c.ContestedAt.IsZero() {
			return true
		}
	}
	return false
}

// counts reports whether c counts toward a class at the reference instant ref (plan D7): it
// is uncontested and matured strictly before ref. A credential maturing exactly at ref does
// not count, and neither does one whose maturity never reached Decide (a zero MaturesAt).
func counts(c Cred, ref time.Time) bool {
	return c.ContestedAt.IsZero() && !c.MaturesAt.IsZero() && c.MaturesAt.Before(ref)
}

// sameCred is credential identity: kind, ID and version (plan D6).
func sameCred(a, b CredRef) bool { return a == b }

// sameSlot compares the (kind, ID) slot only, for the target fence, reservation lookups and
// the break-glass reset exclusion (plan D6).
func sameSlot(a, b CredRef) bool { return a.Kind == b.Kind && a.ID == b.ID }

// findCred returns the credential r names exactly. A ref that names no credential, or that
// names two (a malformed snapshot), is not found.
func findCred(creds []Cred, r CredRef) (Cred, bool) {
	return findUnique(creds, func(c Cred) bool { return sameCred(c.Ref, r) })
}

// findSlot returns the credential in r's slot, at whatever version. An empty slot, or one
// holding two credentials (a malformed snapshot), is not found.
func findSlot(creds []Cred, r CredRef) (Cred, bool) {
	return findUnique(creds, func(c Cred) bool { return sameSlot(c.Ref, r) })
}

func findUnique(creds []Cred, match func(Cred) bool) (Cred, bool) {
	var found Cred
	n := 0
	for _, c := range creds {
		if match(c) {
			found = c
			n++
		}
	}
	return found, n == 1
}

// refLess is the one order of every emitted []CredRef and of the credential tiebreak in
// Effects and Notices (architect ruling 6): Kind by number, then ID byte-wise, then Version.
func refLess(a, b CredRef) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	return a.Version < b.Version
}

// sortRefs returns a sorted copy of refs without duplicates, or nil when refs is empty. It
// never reorders its argument.
func sortRefs(refs []CredRef) []CredRef {
	if len(refs) == 0 {
		return nil
	}
	out := append([]CredRef(nil), refs...)
	sort.Slice(out, func(i, j int) bool { return refLess(out[i], out[j]) })
	n := 1
	for i := 1; i < len(out); i++ {
		if !sameCred(out[i], out[n-1]) {
			out[n] = out[i]
			n++
		}
	}
	return out[:n]
}

// containsRef reports whether refs holds r exactly.
func containsRef(refs []CredRef, r CredRef) bool {
	for _, x := range refs {
		if sameCred(x, r) {
			return true
		}
	}
	return false
}

// containsSlot reports whether refs holds r's slot.
func containsSlot(refs []CredRef, r CredRef) bool {
	for _, x := range refs {
		if sameSlot(x, r) {
			return true
		}
	}
	return false
}

// credRefsWhere returns, in canonical order and once each, the refs of the credentials keep
// accepts. A ref listed twice in a malformed snapshot is kept when either copy is accepted,
// so the result never depends on the snapshot's order.
func credRefsWhere(creds []Cred, keep func(Cred) bool) []CredRef {
	var refs []CredRef
	for _, c := range creds {
		if keep(c) {
			refs = append(refs, c.Ref)
		}
	}
	return sortRefs(refs)
}
