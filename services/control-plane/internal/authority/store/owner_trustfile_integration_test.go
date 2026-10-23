package store

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Task 11: OwnerConfig.TrustFile. revoke-key matches a digest against the
// installed trust UNION the delegations in the trust file that verify under
// the anchors, so a standby's not-yet-installed delegation can be revoked
// before it is installed. end-drain stays installed-only. A trust file that
// does not verify under the anchors refuses the whole verb.

// uninstalledKeys are the keys a delegation lists only in the trust file.
type uninstalledKeys struct {
	cak, operator, tlsKey []byte // 97-byte points
	pin                   []byte // ac.TLSPin of tlsKey
}

func newUninstalledKeys(t *testing.T) uninstalledKeys {
	t.Helper()
	k := uninstalledKeys{cak: freshPoint(t), operator: freshPoint(t), tlsKey: freshPoint(t)}
	k.pin = tlsPin(t, k.tlsKey)
	return k
}

// listing is the RealmOption that makes a delegation list k's CAK, operator
// key and CP TLS pin and no other.
func (k uninstalledKeys) listing() authoritytest.RealmOption {
	return func(d *ac.Delegation) {
		d.CAKPublicKeys = [][]byte{k.cak}
		d.OperatorPublicKeys = [][]byte{k.operator}
		d.CPTLSPins = [][]byte{k.pin}
	}
}

// standbyEnv is a live env with serial 1 installed, plus the trust file a
// standby would be handed: serial 1 and a serial 2 that lists k and is NOT
// installed. file is the encoded trust file; e.o has no TrustFile.
type standbyEnv struct {
	*liveEnv
	k    uninstalledKeys
	d1   authoritytest.SignedDelegation
	d2   authoritytest.SignedDelegation
	file []byte
}

func newStandbyEnv(t *testing.T) *standbyEnv {
	t.Helper()
	e := newLiveEnv(t, OwnerConfig{})
	s := &standbyEnv{liveEnv: e, k: newUninstalledKeys(t), d1: e.r.Current}
	s.d2 = e.r.NextDelegation(t, s.k.listing()) // signed, deliberately not installed
	if got := installedSerials(t, e.f); len(got) != 1 || got[0] != 1 {
		t.Fatalf("installed serials = %v, want only serial 1 (the control: serial 2 is not installed)", got)
	}
	s.file = trustFile(t, s.d1, s.d2)
	return s
}

// ownerWithTrustFile is e's owner reopened with TrustFile set.
func (s *standbyEnv) ownerWithTrustFile(t *testing.T, file []byte) *Owner {
	t.Helper()
	o, err := OpenOwner(OwnerConfig{DB: s.f.Owner, Anchors: s.r.AnchorBytes, TrustFile: file})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// A CAK or operator digest only the trust file's delegation lists is revoked;
// the same digest with no trust file is refused and nothing is inserted.
func TestRevokeKeyMatchesTheTrustFile(t *testing.T) {
	for _, c := range []struct {
		kind   string
		digest func(k uninstalledKeys) []byte
	}{
		{"cak", func(k uninstalledKeys) []byte { return ac.KeyDigest(k.cak) }},
		{"operator", func(k uninstalledKeys) []byte { return ac.KeyDigest(k.operator) }},
	} {
		t.Run(c.kind+" in the trust file is revoked", func(t *testing.T) {
			s := newStandbyEnv(t)
			o := s.ownerWithTrustFile(t, s.file)
			d := c.digest(s.k)
			if err := o.RevokeKey(liveCtx(t), c.kind, d); err != nil {
				t.Fatalf("RevokeKey(%s, a digest only the trust file lists) = %v, want nil", c.kind, err)
			}
			if kind, ok := revokedKind(t, s.f.App, d); !ok || kind != c.kind {
				t.Fatalf("the row for the digest is kind %q (present %v), want %s", kind, ok, c.kind)
			}
			if n := revokedCount(t, s.f.App); n != 1 {
				t.Fatalf("revoked_keys holds %d rows, want 1", n)
			}
		})

		t.Run(c.kind+" without a trust file is refused", func(t *testing.T) {
			s := newStandbyEnv(t) // s.o has TrustFile nil
			before := authoritySnapshot(t, s.f.App)
			err := s.o.RevokeKey(liveCtx(t), c.kind, c.digest(s.k))
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("RevokeKey(%s, an uninstalled digest, TrustFile nil) = %v, want ErrRefused", c.kind, err)
			}
			if n := revokedCount(t, s.f.App); n != 0 {
				t.Fatalf("revoked_keys holds %d rows after a refusal, want 0", n)
			}
			if after := authoritySnapshot(t, s.f.App); after != before {
				t.Fatalf("a refused RevokeKey wrote:\nbefore %s\nafter  %s", before, after)
			}
		})
	}

	// The union keeps the installed half: with a trust file set, a digest only
	// the installed delegation lists still revokes.
	t.Run("an installed digest still revokes with a trust file set", func(t *testing.T) {
		s := newStandbyEnv(t)
		o := s.ownerWithTrustFile(t, s.file)
		d := ac.KeyDigest(s.d1.D.CAKPublicKeys[0])
		if err := o.RevokeKey(liveCtx(t), "cak", d); err != nil {
			t.Fatalf("RevokeKey(cak, an installed digest, TrustFile set) = %v, want nil", err)
		}
		if _, ok := revokedKind(t, s.f.App, d); !ok {
			t.Fatal("no revoked_keys row for the installed digest")
		}
	})
}

// B3-D10 holds for the trust file: cp_tls takes a pin, whether the pin is
// installed or only in the verified trust file; a key digest is never a pin.
func TestRevokeKeyCPTLSPinFromTheTrustFile(t *testing.T) {
	t.Run("a pin only the trust file lists is revoked", func(t *testing.T) {
		s := newStandbyEnv(t)
		o := s.ownerWithTrustFile(t, s.file)
		if err := o.RevokeKey(liveCtx(t), "cp_tls", s.k.pin); err != nil {
			t.Fatalf("RevokeKey(cp_tls, a pin only the trust file lists) = %v, want nil", err)
		}
		if kind, ok := revokedKind(t, s.f.App, s.k.pin); !ok || kind != "cp_tls" {
			t.Fatalf("the row for the pin is kind %q (present %v), want cp_tls", kind, ok)
		}
		if revoked, err := s.s.Revoked(liveCtx(t), s.k.pin); err != nil || !revoked {
			t.Fatalf("Revoked(pin) = %v, %v; want true", revoked, err)
		}
	})

	t.Run("the key digest of the trust file's CP key is refused", func(t *testing.T) {
		s := newStandbyEnv(t)
		o := s.ownerWithTrustFile(t, s.file)
		err := o.RevokeKey(liveCtx(t), "cp_tls", ac.KeyDigest(s.k.tlsKey))
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("RevokeKey(cp_tls, KeyDigest(point)) = %v, want ErrRefused", err)
		}
		if !strings.Contains(err.Error(), "cp_tls takes the pin") {
			t.Fatalf("the refusal %q does not say \"cp_tls takes the pin\"", err)
		}
		if n := revokedCount(t, s.f.App); n != 0 {
			t.Fatalf("revoked_keys holds %d rows after a refusal, want 0", n)
		}
	})

	t.Run("a pin only the trust file lists is refused without it", func(t *testing.T) {
		s := newStandbyEnv(t)
		if err := s.o.RevokeKey(liveCtx(t), "cp_tls", s.k.pin); !errors.Is(err, ErrRefused) {
			t.Fatalf("RevokeKey(cp_tls, an uninstalled pin, TrustFile nil) = %v, want ErrRefused", err)
		}
		if n := revokedCount(t, s.f.App); n != 0 {
			t.Fatalf("revoked_keys holds %d rows after a refusal, want 0", n)
		}
	})
}

// roguelySigned is d2 signed by two keys that are not the realm's anchors.
func roguelySigned(t *testing.T, d authoritytest.SignedDelegation) authoritytest.SignedDelegation {
	t.Helper()
	a, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d.SigA, d.SigB = authoritytest.SignRaw(t, a, d.Msg), authoritytest.SignRaw(t, b, d.Msg)
	d.Signed = ac.Signed{Type: "delegation", Msg: d.Msg, SigA: d.SigA, SigB: d.SigB}.Encode()
	if d.Signed == nil {
		t.Fatal("the rogue-signed delegation does not encode")
	}
	return d
}

// A trust file that does not verify under the anchors is never silently
// ignored: the whole verb refuses, whatever digest it was given, and writes
// nothing.
func TestRevokeKeyRefusesAnUnverifiedTrustFile(t *testing.T) {
	cases := []struct {
		name string
		file func(s *standbyEnv) []byte
	}{
		{"signed by keys that are not the anchors", func(s *standbyEnv) []byte {
			return trustFile(t, s.d1, roguelySigned(t, s.d2))
		}},
		{"does not decode", func(*standbyEnv) []byte { return []byte("not a trust file") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStandbyEnv(t)
			o := s.ownerWithTrustFile(t, c.file(s))
			before := authoritySnapshot(t, s.f.App)
			for _, v := range []struct {
				what, kind string
				digest     []byte
			}{
				{"a digest only the bad file lists", "cak", ac.KeyDigest(s.k.cak)},
				{"a pin only the bad file lists", "cp_tls", s.k.pin},
				{"a digest that IS installed", "cak", ac.KeyDigest(s.d1.D.CAKPublicKeys[0])},
				{"an installed pin", "cp_tls", s.d1.D.CPTLSPins[0]},
			} {
				if err := o.RevokeKey(liveCtx(t), v.kind, v.digest); !errors.Is(err, ErrRefused) {
					t.Fatalf("RevokeKey(%s: %s) with an unverified trust file = %v, want ErrRefused", v.kind, v.what, err)
				}
			}
			if n := revokedCount(t, s.f.App); n != 0 {
				t.Fatalf("revoked_keys holds %d rows after refusals, want 0", n)
			}
			if after := authoritySnapshot(t, s.f.App); after != before {
				t.Fatalf("a refused RevokeKey wrote:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// end-drain reads the installed trust only: a delegation, or a superseding
// delegation, that only the trust file holds counts for nothing.
func TestEndDrainIgnoresTheTrustFile(t *testing.T) {
	s := newStandbyEnv(t)
	o := s.ownerWithTrustFile(t, s.file)
	before := authoritySnapshot(t, s.f.App)

	// Serial 2 exists only in the trust file: unknown, as for any other
	// (gen, serial) no installed delegation has.
	if err := o.EndDrain(liveCtx(t), 0, 2); !errors.Is(err, ErrRefused) {
		t.Fatalf("EndDrain(0, 2), a delegation only the trust file holds = %v, want ErrRefused", err)
	}
	// Serial 1 is installed, and serial 2 would supersede it, but only the
	// trust file says so: under the installed trust serial 1 is current.
	if err := o.EndDrain(liveCtx(t), 0, 1); !errors.Is(err, ErrRefused) {
		t.Fatalf("EndDrain(0, 1), superseded only by the trust file's serial 2 = %v, want ErrRefused", err)
	}
	if g := drainGrace(t, s.f.App, 1); g != 660000 {
		t.Fatalf("serial 1 drain_grace_ms = %d after refusals, want 660000", g)
	}
	if got := installedSerials(t, s.f); len(got) != 1 {
		t.Fatalf("installed serials = %v after end-drain, want end-drain to install nothing", got)
	}
	if after := authoritySnapshot(t, s.f.App); after != before {
		t.Fatalf("a refused EndDrain wrote:\nbefore %s\nafter  %s", before, after)
	}
}
