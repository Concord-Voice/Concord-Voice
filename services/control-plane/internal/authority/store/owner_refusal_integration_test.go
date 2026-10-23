package store

import (
	"errors"
	"strings"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Refusals of the owner verbs and the hold routes that no other test reaches.
// Each row asserts the sentinel and the reason the verb names, and that
// nothing was written.

// ignoredDelegation installs, beside the genuine serial 1, a serial 2 whose
// second signature fails dual control, so the installed trust ignores it
// (AddSigned returns ErrIgnored). It lists k's keys, which therefore appear in
// no accepted delegation.
func ignoredDelegation(t *testing.T, e *liveEnv, k uninstalledKeys) {
	t.Helper()
	good := e.r.Current
	bad := e.r.NextDelegation(t, k.listing())
	e.r.Current = good
	bad.SigB = append([]byte(nil), bad.SigB...)
	bad.SigB[0] ^= 0xff
	e.r.Install(t, e.f.App, bad)
}

func TestOwnerVerbRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
		text string
		run  func(t *testing.T) error
	}{
		{"clear-held --dry-run names an account that is not a UUID", ErrRefused, `"not-an-account" is not an account id`,
			func(t *testing.T) error {
				_, err := signedOwner(t, newEnv(t)).ClearHeldPlan(liveCtx(t), []string{"not-an-account"})
				return err
			}},
		{"clear-held names an account that is not a UUID", ErrRefused, `"not-an-account" is not an account id`,
			func(t *testing.T) error {
				_, err := signedOwner(t, newEnv(t)).ClearHeld(liveCtx(t), []string{"not-an-account"})
				return err
			}},
		{"revoke-key with a trust file whose trust halts", ErrRefused, "the trust file's trust halts (T4)",
			func(t *testing.T) error {
				s := newStandbyEnv(t)
				// Serial 3 has no cutover after serial 2 set one: every item verifies
				// under the anchors, and the trust halts (T4: a higher serial lowers it).
				s.r.Current = s.d1
				set := s.r.NextDelegation(t, func(d *ac.Delegation) { d.CutoverAtMs = d.NotBeforeMs + 1000 })
				lower := s.r.NextDelegation(t, func(d *ac.Delegation) { d.CutoverAtMs = 0 })
				return s.ownerWithTrustFile(t, trustFile(t, s.d1, set, lower)).RevokeKey(liveCtx(t), "cak", ac.KeyDigest(s.k.cak))
			}},
		{"revoke-key of a digest only an ignored installed delegation lists", ErrRefused, "no installed or trust-file delegation lists this one",
			func(t *testing.T) error {
				e := newLiveEnv(t, OwnerConfig{})
				k := newUninstalledKeys(t)
				ignoredDelegation(t, e, k)
				return e.o.RevokeKey(liveCtx(t), "cak", ac.KeyDigest(k.cak))
			}},
		{"end-drain of a delegation the installed trust ignores", ErrRefused, "the installed trust does not accept that delegation",
			func(t *testing.T) error {
				e := newLiveEnv(t, OwnerConfig{})
				ignoredDelegation(t, e, newUninstalledKeys(t))
				return e.o.EndDrain(liveCtx(t), 0, 2)
			}},
		{"reconcile/required with a hold-req that is not a hold-req", ErrBadAuthz, "the body is not a hold-req in account order",
			func(t *testing.T) error {
				return newRestored(t, 1, restoredOpts{}).e.s.ReconcileRequired(liveCtx(t), []byte("not a hold-req"))
			}},
		{"reconcile/required with bundles in descending account order", ErrBadAuthz, "the body is not a hold-req in account order",
			func(t *testing.T) error {
				rs := newRestored(t, 2, restoredOpts{})
				return rs.e.s.ReconcileRequired(liveCtx(t), holdReqBytes(t, rs.ahead[1], rs.ahead[0]))
			}},
		{"reconcile/hold with bundles in descending account order", ErrBadAuthz, "the body is not a hold-req in account order",
			func(t *testing.T) error {
				rs := newRestored(t, 2, restoredOpts{})
				rs.enterUnreconciled(t)
				_, err := rs.e.s.ReconcileHold(liveCtx(t), holdReqBytes(t, rs.ahead[1], rs.ahead[0]))
				if held := rs.heldAccounts(t); len(held) != 0 {
					t.Fatalf("a refused request held %v, want nothing", held)
				}
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("got %v, want %v naming %q", err, tc.want, tc.text)
			}
		})
	}
}
