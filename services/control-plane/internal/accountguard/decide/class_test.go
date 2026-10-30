package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan Task 4. Class table: parent §6.1 ("C1 Any one K; a backup code alone; one anchor
// alone", "C2 K + any P; one non-paper P alone; an anchor + K; an anchor + a backup code;
// two anchors", "C3 An anchor + a mature non-paper P that is not backup-eligible").

func TestClassOf_Table(t *testing.T) {
	cases := []struct {
		name  string
		used  []CredRef
		bound []CredRef // the WebAuthn refs of used whose assertion carried BE=0 (Proof.DeviceBound)
		want  Class
	}{
		{"password alone is C1 (one K)", []CredRef{fxPW}, nil, C1},
		{"two Ks are still C1", []CredRef{fxAddr, fxPW}, nil, C1},
		{"backup code alone is C1", []CredRef{fxPaper}, nil, C1},
		{"recovery key alone is C1", []CredRef{fxRK}, nil, C1},
		{"recovery device alone is C1", []CredRef{fxRD}, nil, C1},
		{"TOTP alone is C2 (non-paper P)", []CredRef{fxTOTP}, nil, C2},
		{"backup-eligible WebAuthn alone is C2", []CredRef{fxWA1}, nil, C2},
		{"backup-ineligible WebAuthn alone is C2", []CredRef{fxWA0}, nil, C2},
		{"K + paper is C2 (K + any P)", []CredRef{fxPW, fxPaper}, nil, C2},
		{"anchor + K is C2", []CredRef{fxRK, fxPW}, nil, C2},
		{"anchor + paper is C2", []CredRef{fxRK, fxPaper}, nil, C2},
		{"two anchors is C2", []CredRef{fxRK, fxRD}, nil, C2},
		{"anchor + TOTP is C3", []CredRef{fxRK, fxTOTP}, nil, C3},
		{"anchor + WebAuthn BE=0 is C3", []CredRef{fxRK, fxWA0}, []CredRef{fxWA0}, C3},
		{"anchor + WebAuthn BE=1 is only C2", []CredRef{fxRK, fxWA1}, nil, C2},
		{"anchor + WebAuthn with an empty DeviceBound is only C2", []CredRef{fxRK, fxWA0}, nil, C2},
		{"anchor + WebAuthn that is not the device-bound one is only C2", []CredRef{fxRK, fxWA1}, []CredRef{fxWA0}, C2},
		{"anchor + paper + K is C2", []CredRef{fxRK, fxPaper, fxPW}, nil, C2},
		{"empty proof is ClassNone", nil, nil, ClassNone},
	}
	creds := standardCreds()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassOf(proofBound(tc.used, tc.bound...), creds, t0))
		})
	}
}

// Review Focus 1: the same anchor twice, or two codes from one backup set, must not raise the class.
func TestClassOf_DuplicateRefsCountOnce(t *testing.T) {
	creds := standardCreds()
	assert.Equal(t, C1, ClassOf(Proof{Used: []CredRef{fxRK, fxRK}}, creds, t0), "the same anchor twice")
	assert.Equal(t, C1, ClassOf(Proof{Used: []CredRef{fxPaper, fxPaper}}, creds, t0), "two codes from one set")
	assert.Equal(t, C1, ClassOf(Proof{Used: []CredRef{fxPW, fxPW, fxPW}}, creds, t0), "the same K three times")
}

// Review Focus 3: a ref at a stale version, or naming no credential, contributes nothing.
func TestClassOf_StaleVersionAndUnknownRefContributeNothing(t *testing.T) {
	totp2 := matureCred(CredRef{Kind: CredTOTP, ID: "totp", Version: 2})
	creds := []Cred{totp2, matureCred(fxPW)}

	stale := CredRef{Kind: CredTOTP, ID: "totp", Version: 1}
	assert.Equal(t, ClassNone, ClassOf(Proof{Used: []CredRef{stale}}, creds, t0), "totp v1 against a credential at v2")

	current := CredRef{Kind: CredTOTP, ID: "totp", Version: 2}
	assert.Equal(t, C2, ClassOf(Proof{Used: []CredRef{current}}, creds, t0), "positive control: totp v2")

	ghost := cref(CredTOTP, "no-such-credential")
	assert.Equal(t, ClassNone, ClassOf(Proof{Used: []CredRef{ghost}}, creds, t0), "a ref naming no credential")

	// A stale ref beside a valid one adds nothing: {pw, stale totp} stays C1.
	assert.Equal(t, C1, ClassOf(Proof{Used: []CredRef{fxPW, stale}}, creds, t0))

	// A ref of the right ID and version but the wrong kind is a different credential.
	wrongKind := CredRef{Kind: CredWebAuthn, ID: "totp", Version: 2}
	assert.Equal(t, ClassNone, ClassOf(Proof{Used: []CredRef{wrongKind}}, creds, t0))
}

func TestClassOf_ContestedNeverCounts(t *testing.T) {
	contested := credContested(fxTOTP)
	creds := []Cred{contested}
	for _, ref := range []time.Time{t0, t0.Add(1000 * time.Hour), t0.Add(-time.Hour), t0.Add(-100 * time.Hour)} {
		assert.Equal(t, ClassNone, ClassOf(Proof{Used: []CredRef{fxTOTP}}, creds, ref), "ref %v", ref)
	}
}

// Parent §6.1: a credential counts only if it matured before the reference instant;
// D7: MaturesAt.Before(ref), so maturing exactly at ref does not count.
func TestBoundary_MaturesExactlyAtRefDoesNotCount(t *testing.T) {
	ref := t0
	proof := Proof{Used: []CredRef{fxTOTP}}

	atRef := []Cred{credMaturing(fxTOTP, ref)}
	assert.Equal(t, ClassNone, ClassOf(proof, atRef, ref), "MaturesAt == ref")

	justBefore := []Cred{credMaturing(fxTOTP, ref.Add(-time.Nanosecond))}
	assert.Equal(t, C2, ClassOf(proof, justBefore, ref), "MaturesAt == ref-1ns")

	justAfter := []Cred{credMaturing(fxTOTP, ref.Add(time.Nanosecond))}
	assert.Equal(t, ClassNone, ClassOf(proof, justAfter, ref), "MaturesAt == ref+1ns")
}

func TestClassOf_AwaitingNoticeSentinelNeverCounts(t *testing.T) {
	creds := []Cred{credMaturing(fxTOTP, awaitingInstant)}
	assert.Equal(t, ClassNone, ClassOf(Proof{Used: []CredRef{fxTOTP}}, creds, t0))
}

func TestClassOf_LegacySentinelCounts(t *testing.T) {
	creds := []Cred{credMaturing(fxTOTP, legacyInstant)}
	assert.Equal(t, C2, ClassOf(Proof{Used: []CredRef{fxTOTP}}, creds, t0))
}

// Parent §6.1: "TOTP counts as non-backup-eligible although its seed may sync." A TOTP pairs into
// C3 with no DeviceBound entry, and naming it there changes nothing.
func TestClassOf_TOTPNeedsNoDeviceBound(t *testing.T) {
	creds := []Cred{matureCred(fxRK), matureCred(fxTOTP)}
	assert.Equal(t, C3, ClassOf(Proof{Used: []CredRef{fxRK, fxTOTP}}, creds, t0))
	assert.Equal(t, C3, ClassOf(proofBound([]CredRef{fxRK, fxTOTP}, fxTOTP), creds, t0), "a non-WebAuthn ref in DeviceBound is inert")
}

// Red-team F1 (architect rulings 3, option A): BE is read from THIS assertion. A DeviceBound ref
// that is not in Used proves nothing, so a synced passkey cannot be promoted by listing another
// credential's ref, a stale version, or a ref the proof never carried.
func TestClassOf_DeviceBoundRefOutsideUsedIsIgnored(t *testing.T) {
	creds := []Cred{matureCred(fxRK), matureCred(fxWA0), matureCred(fxWA1)}
	synced := []CredRef{fxRK, fxWA1}

	assert.Equal(t, C2, ClassOf(proofBound(synced), creds, t0), "positive control: a synced passkey beside an anchor")
	assert.Equal(t, C3, ClassOf(proofBound([]CredRef{fxRK, fxWA0}, fxWA0), creds, t0),
		"positive control: the same shape with the key in both Used and DeviceBound")

	assert.Equal(t, C2, ClassOf(proofBound(synced, fxWA0), creds, t0), "a device-bound ref the proof never used")
	assert.Equal(t, C2, ClassOf(proofBound(synced, fxRK), creds, t0), "a non-WebAuthn ref in DeviceBound")
	stale := CredRef{Kind: CredWebAuthn, ID: "wa1", Version: 2}
	assert.Equal(t, C2, ClassOf(proofBound(synced, stale), creds, t0), "a DeviceBound ref at another version")
	assert.Equal(t, C2, ClassOf(proofBound(synced, cref(CredWebAuthn, "ghost")), creds, t0), "a DeviceBound ref naming nothing")

	// A DeviceBound ref names the assertion, not the credential: it cannot rescue a stale Used ref
	// or an uncounted credential.
	assert.Equal(t, C1, ClassOf(proofBound([]CredRef{fxRK, stale}, stale), creds, t0), "stale Used ref")
	immature := []Cred{matureCred(fxRK), credMaturing(fxWA0, t0.Add(time.Hour))}
	assert.Equal(t, C1, ClassOf(proofBound([]CredRef{fxRK, fxWA0}, fxWA0), immature, t0), "an immature device-bound key")
	contested := []Cred{matureCred(fxRK), credContested(fxWA0)}
	assert.Equal(t, C1, ClassOf(proofBound([]CredRef{fxRK, fxWA0}, fxWA0), contested, t0), "a contested device-bound key")
}

// An immature or contested element never raises the class: {rk, immature totp} is C1, not C3.
func TestClassOf_UncountedElementsDoNotRaiseTheClass(t *testing.T) {
	immature := []Cred{matureCred(fxRK), credMaturing(fxTOTP, t0.Add(time.Hour))}
	assert.Equal(t, C1, ClassOf(Proof{Used: []CredRef{fxRK, fxTOTP}}, immature, t0))

	contested := []Cred{matureCred(fxRK), credContested(fxTOTP)}
	assert.Equal(t, C1, ClassOf(Proof{Used: []CredRef{fxRK, fxTOTP}}, contested, t0))
}

// ClassOf must not modify its arguments.
func TestClassOf_DoesNotMutateArguments(t *testing.T) {
	used := []CredRef{fxRK, fxTOTP, fxPW}
	creds := standardCreds()
	usedCopy := append([]CredRef(nil), used...)
	credsCopy := append([]Cred(nil), creds...)
	_ = ClassOf(Proof{Used: used}, creds, t0)
	assert.Equal(t, usedCopy, used)
	assert.Equal(t, credsCopy, creds)
}

// LoginFloor: parent §6.1 "Login floor ... (password_login ? password : sso) + (mfa ? inline
// factor : nothing)"; plan Task 4: maturity ignored, a contested credential excluded, refs
// match exactly, an address or email-MFA ref never meets the MFA leg (parent mutation #26).
func TestLoginFloor(t *testing.T) {
	noMFA := credsOf(fxPW, fxAddr, fxSSO)
	mfa := credsOf(fxPW, fxAddr, fxSSO, fxTOTP, fxPaper)

	t.Run("no-MFA password login", func(t *testing.T) {
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, noMFA, false))
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxSSO}}, noMFA, false), "an SSO assertion is not the K leg of a password login")
		assert.False(t, LoginFloor(Proof{}, noMFA, false))
	})
	t.Run("no-MFA SSO login", func(t *testing.T) {
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxSSO}}, noMFA, true))
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, noMFA, true), "a password is not the K leg of an SSO login")
	})
	t.Run("MFA account", func(t *testing.T) {
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, mfa, false), "a lone password on an MFA account")
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxTOTP}}, mfa, false))
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxPaper}}, mfa, false), "a backup code is an inline factor at login")
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxTOTP}}, mfa, false), "the MFA leg alone is not the floor")
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxSSO, fxTOTP}}, mfa, true))
	})
	t.Run("an address never meets the MFA leg (#26)", func(t *testing.T) {
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxAddr}}, mfa, false))
	})
	t.Run("email MFA never meets the MFA leg", func(t *testing.T) {
		emailMFA := cref(CredEmailMFA, "emfa")
		creds := append(credsOf(emailMFA), mfa...)
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW, emailMFA}}, creds, false))
	})
	t.Run("WebAuthn meets the MFA leg", func(t *testing.T) {
		creds := credsOf(fxPW, fxWA0)
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxWA0}}, creds, false))
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, creds, false))
	})
	t.Run("a contested factor is excluded (#14)", func(t *testing.T) {
		// A second, uncontested factor keeps the account an MFA account.
		creds := []Cred{matureCred(fxPW), credContested(fxTOTP), matureCred(fxPaper)}
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxTOTP}}, creds, false))
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxPaper}}, creds, false), "positive control: the uncontested set")
	})
	t.Run("a contested password is excluded", func(t *testing.T) {
		creds := []Cred{credContested(fxPW), matureCred(fxAddr)}
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, creds, false))
	})
	t.Run("maturity is ignored", func(t *testing.T) {
		immaturePW := []Cred{credMaturing(fxPW, t0.Add(48*time.Hour)), matureCred(fxAddr)}
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, immaturePW, false), "F5: an immature password still signs in")

		awaitingPW := []Cred{credMaturing(fxPW, awaitingInstant)}
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, awaitingPW, false))

		immatureTOTP := []Cred{matureCred(fxPW), credMaturing(fxTOTP, t0.Add(48*time.Hour))}
		assert.True(t, LoginFloor(Proof{Used: []CredRef{fxPW, fxTOTP}}, immatureTOTP, false), "an immature factor still satisfies the MFA leg")
	})
	t.Run("refs match exactly", func(t *testing.T) {
		stalePW := CredRef{Kind: CredPassword, ID: "pw", Version: 0}
		assert.False(t, LoginFloor(Proof{Used: []CredRef{stalePW}}, noMFA, false), "stale password version")
		assert.False(t, LoginFloor(Proof{Used: []CredRef{cref(CredPassword, "ghost")}}, noMFA, false))
	})
	t.Run("a second pw version replaces the first", func(t *testing.T) {
		pw2 := CredRef{Kind: CredPassword, ID: "pw", Version: 2}
		creds := []Cred{matureCred(pw2)}
		assert.True(t, LoginFloor(Proof{Used: []CredRef{pw2}}, creds, false))
		assert.False(t, LoginFloor(Proof{Used: []CredRef{fxPW}}, creds, false), "the rotated-away password version")
	})
}

func TestRank(t *testing.T) {
	cases := []struct {
		kind CredKind
		want Rank
	}{
		{CredPassword, Rank1},
		{CredAddress, Rank1},
		{CredBackupAddress, Rank1},
		{CredSSO, Rank1},
		{CredEmailMFA, Rank1},
		{CredTOTP, Rank2},
		{CredWebAuthn, Rank2},
		{CredBackupCodeSet, Rank2},
		{CredRecoveryKey, Rank3},
		{CredRecoveryDevice, Rank3},
		{CredUnknown, RankInvalid},
		{CredKind(200), RankInvalid},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.kind.Rank(), "kind %d", tc.kind)
	}
}

// classOracle is an independent statement of parent §6.1's class table over the composition
// of the counted elements: k = knowledge credentials, paper = backup-code sets, np0 / np1 =
// non-paper possession factors that are device-bound (np0) or not (np1), a = anchors.
func classOracle(k, paper, np0, np1, a int) Class {
	nonPaperP := np0 + np1
	anyP := paper + nonPaperP
	switch {
	case a >= 1 && np0 >= 1:
		return C3
	case (k >= 1 && anyP >= 1) || nonPaperP >= 1 || (a >= 1 && k >= 1) || (a >= 1 && paper >= 1) || a >= 2:
		return C2
	case k >= 1 || paper >= 1 || a >= 1:
		return C1
	default:
		return ClassNone
	}
}

// TestClassOracle_AgreesWithTheParentTable guards the oracle itself against the parent's rows.
func TestClassOracle_AgreesWithTheParentTable(t *testing.T) {
	assert.Equal(t, ClassNone, classOracle(0, 0, 0, 0, 0))
	assert.Equal(t, C1, classOracle(2, 0, 0, 0, 0))
	assert.Equal(t, C1, classOracle(0, 1, 0, 0, 0))
	assert.Equal(t, C1, classOracle(0, 0, 0, 0, 1))
	assert.Equal(t, C2, classOracle(1, 1, 0, 0, 0))
	assert.Equal(t, C2, classOracle(0, 0, 0, 1, 0))
	assert.Equal(t, C2, classOracle(1, 0, 0, 0, 1))
	assert.Equal(t, C2, classOracle(0, 1, 0, 0, 1))
	assert.Equal(t, C2, classOracle(0, 0, 0, 0, 2))
	assert.Equal(t, C3, classOracle(0, 0, 1, 0, 1))
	assert.Equal(t, C2, classOracle(0, 0, 0, 1, 1))
	require.Equal(t, C3, classOracle(1, 1, 1, 1, 1))
}
