package decide

import (
	"time"
)

// Shared test fixtures (plan Task 4: "Fixture creds ... all mature at T0 = 2026-11-01T12:00:00Z,
// MaturesAt T0-1h"). Later tasks (the enumerator, the world model) reuse these.

// t0 is the fixed "now" of every vector and fixture.
var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// legacyInstant is the legacy maturity sentinel, written as a literal so a fixture does not
// depend on the production LegacyMaturity function.
var legacyInstant = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// awaitingInstant is the awaiting-notice sentinel, written as a literal.
var awaitingInstant = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

// cref builds a version-1 credential reference.
func cref(kind CredKind, id string) CredRef { return CredRef{Kind: kind, ID: id, Version: 1} }

// The fixture credential refs.
var (
	fxPW    = cref(CredPassword, "pw")
	fxAddr  = cref(CredAddress, "addr")
	fxSSO   = cref(CredSSO, "sso")
	fxTOTP  = cref(CredTOTP, "totp")
	fxWA0   = cref(CredWebAuthn, "wa0") // a device-bound key: tests list it in Proof.DeviceBound
	fxWA1   = cref(CredWebAuthn, "wa1") // a synced passkey: never listed in Proof.DeviceBound
	fxPaper = cref(CredBackupCodeSet, "paper")
	fxRK    = cref(CredRecoveryKey, "rk")
	fxRD    = cref(CredRecoveryDevice, "rd")
)

// matureCred returns an uncontested credential that matured an hour before t0 and was
// enrolled in the legacy epoch. A backup set is Paper. A WebAuthn carries no backup-eligibility
// of its own: whether an assertion was device-bound travels on Proof.DeviceBound (F1).
func matureCred(r CredRef) Cred {
	c := Cred{Ref: r, MaturesAt: t0.Add(-time.Hour), EnrolledAt: legacyInstant}
	if r.Kind == CredBackupCodeSet {
		c.Paper = true
	}
	return c
}

// credMaturing returns matureCred with MaturesAt set.
func credMaturing(r CredRef, matures time.Time) Cred {
	c := matureCred(r)
	c.MaturesAt = matures
	return c
}

// credContested returns a credential that matured an hour before t0 but was contested two
// hours before t0.
func credContested(r CredRef) Cred {
	c := matureCred(r)
	c.ContestedAt = t0.Add(-2 * time.Hour)
	return c
}

// standardCreds returns the nine fixture credentials, all mature at t0.
func standardCreds() []Cred {
	return []Cred{
		matureCred(fxPW), matureCred(fxAddr), matureCred(fxSSO), matureCred(fxTOTP),
		matureCred(fxWA0), matureCred(fxWA1), matureCred(fxPaper), matureCred(fxRK), matureCred(fxRD),
	}
}

// credsOf returns mature fixture credentials for the given refs.
func credsOf(refs ...CredRef) []Cred {
	out := make([]Cred, 0, len(refs))
	for _, r := range refs {
		out = append(out, matureCred(r))
	}
	return out
}

// proofBound builds a proof of used in which the listed WebAuthn refs carried BE=0 in THIS
// assertion (Proof.DeviceBound). Every WebAuthn of used that is not listed is a synced passkey.
func proofBound(used []CredRef, bound ...CredRef) Proof {
	return Proof{Used: used, DeviceBound: bound}
}

// inputAt builds an Input at t0 over the given creds with a password-login account and a clear
// guessable budget (the polarity of H1: the zero value of both is the guarded one).
func inputAt(act Action, creds []Cred, used ...CredRef) Input {
	return Input{
		Now:                  t0,
		Act:                  act,
		Proof:                Proof{Used: used},
		Creds:                creds,
		GuessableBudgetClear: true,
	}
}

// effectsOfKind returns the effects of one kind.
func effectsOfKind(o Outcome, k EffectKind) []Effect {
	var out []Effect
	for _, e := range o.Effects {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// noticesOfKind returns the notices of one kind.
func noticesOfKind(o Outcome, k NoticeKind) []Notice {
	var out []Notice
	for _, n := range o.Notices {
		if n.Kind == k {
			out = append(out, n)
		}
	}
	return out
}

// effectKindsSet returns the set of effect kinds present.
func effectKindsSet(o Outcome) map[EffectKind]bool {
	out := map[EffectKind]bool{}
	for _, e := range o.Effects {
		out[e.Kind] = true
	}
	return out
}
