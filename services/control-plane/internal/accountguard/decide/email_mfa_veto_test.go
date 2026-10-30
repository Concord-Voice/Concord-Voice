package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 3, red-team F4 (architect rulings-3): in C4 every email-MFA code is delivered to the
// primary address (child spec H2, CredAddress), so "a code sent to a verified address" proves
// that mailbox, and parent §6.1 K row and §4 ("Mailbox holder: cannot ... veto the mailbox's own
// removal") forbid it vetoing the mailbox's own replacement. selfVetoFiltered therefore drops a
// CredEmailMFA ref when the request targets the primary address, and only then.
//
// C12 obligation (doc.go): once email_mfa_target_id may name a backup address, this filter is
// wrong in both directions, and Cred must carry the email-MFA delivery slot.

func emailMFACancelFixture() (creds []Cred, emfa CredRef) {
	emfa = cref(CredEmailMFA, "emfa") // H2: maturity and delivery ride the primary address
	return []Cred{credMaturing(fxPW, legacyInstant), credMaturing(fxAddr, legacyInstant),
		credMaturing(emfa, legacyInstant), credMaturing(cref(CredBackupAddress, "bk"), legacyInstant),
		credMaturing(fxTOTP, legacyInstant)}, emfa
}

func cancelWith(creds []Cred, req Req, used ...CredRef) Outcome {
	return Decide(Input{Now: t0, Act: Cancel{IDs: []string{req.ID}}, Proof: Proof{Used: used}, Creds: creds,
		Reqs: []Req{req}, GuessableBudgetClear: true})
}

// TestCancel_EmailMFACodeCannotVetoTheReplacementOfItsMailbox promotes the red-team PoC.
func TestCancel_EmailMFACodeCannotVetoTheReplacementOfItsMailbox(t *testing.T) {
	creds, emfa := emailMFACancelFixture()
	opened := t0.Add(-30 * time.Minute)
	change := Req{ID: "ac", Kind: ReqAddressChange, Status: StatusPending, OriginClass: C1, Origins: []CredRef{fxPW},
		Targets: []CredRef{fxAddr}, OpenedAt: opened, CompletesAt: opened.Add(DeltaCP)}

	require.Equal(t, VerdictRefused, cancelWith(creds, change, fxAddr).Verdict, "positive control: the address code is self-vetoed")
	require.Equal(t, VerdictApplied, cancelWith(creds, change, fxPW).Verdict, "positive control: a separate K still cancels")

	for name, used := range map[string][]CredRef{"email-MFA code": {emfa}, "address + email-MFA codes": {fxAddr, emfa}} {
		out := cancelWith(creds, change, used...)
		assert.Equal(t, VerdictRefused, out.Verdict, "%s from the mailbox being replaced must not cancel: %+v", name, out.Effects)
		assert.Equal(t, RowT7, out.Row)
	}

	// The filter drops only the email-MFA ref: a proof that also holds a K the request does not
	// remove still cancels.
	assert.Equal(t, VerdictApplied, cancelWith(creds, change, emfa, fxPW).Verdict)
}

// TestCancel_EmailMFAFilterIsNotOverBroad: the filter keys on the request targeting the primary
// address. An email-MFA code still cancels a backup-address removal, a TOTP removal and a
// break-glass, so the veto never reaches further than the mailbox it proves.
func TestCancel_EmailMFAFilterIsNotOverBroad(t *testing.T) {
	creds, emfa := emailMFACancelFixture()
	opened := t0.Add(-30 * time.Minute)
	mk := func(id string, kind ReqKind, targets ...CredRef) Req {
		return Req{ID: id, Kind: kind, Status: StatusPending, OriginClass: C1, Origins: []CredRef{fxPW},
			Targets: targets, OpenedAt: opened, CompletesAt: opened.Add(DeltaCP)}
	}
	cases := []struct {
		name string
		req  Req
	}{
		{"a backup address removal", mk("bk", ReqCredentialRemove, cref(CredBackupAddress, "bk"))},
		{"a TOTP credential_remove", mk("totp", ReqCredentialRemove, fxTOTP)},
		{"a break-glass (its targets are never read)", mk("bg", ReqBreakGlass)},
		{"an address_change that does not target the primary", mk("other", ReqAddressChange, cref(CredBackupAddress, "bk"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := cancelWith(creds, tc.req, emfa)
			assert.Equal(t, VerdictApplied, out.Verdict, "an email-MFA code cancels it")
			require.Len(t, effectsOfKind(out, EffectCancel), 1)
			assert.Equal(t, C1, effectsOfKind(out, EffectCancel)[0].CancellerClass)
		})
	}

	t.Run("an address_change targeting the primary is the only case that drops it", func(t *testing.T) {
		out := cancelWith(creds, mk("ac", ReqAddressChange, fxAddr), emfa)
		assert.Equal(t, VerdictRefused, out.Verdict)
	})
	t.Run("the filtered code adds nothing, and a TOTP beside it still cancels a C2 request", func(t *testing.T) {
		req := mk("ac2", ReqAddressChange, fxAddr)
		req.OriginClass = C2
		assert.Equal(t, VerdictApplied, cancelWith(creds, req, emfa, fxTOTP).Verdict, "the TOTP alone is C2")
		assert.Equal(t, VerdictRefused, cancelWith(creds, req, emfa).Verdict)
	})
}
