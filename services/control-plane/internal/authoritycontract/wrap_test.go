package authoritycontract

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func init() { codecChecks["device-wrap-open"] = checkWrapVector }

// checkWrapVector opens a committed Web Crypto wrap (DoR §3.1 "Wraps"). A
// refused wrap returns nothing: no secrets and no use.
func checkWrapVector(t *testing.T, v vector) {
	h, err := ParseDeviceHead(mustB64(t, v.PrevDeviceHead), mustB64(t, v.PrevHead))
	require.NoError(t, err)
	dkx, err := ecdh.P384().NewPrivateKey(mustB64(t, v.RecipientDkxD))
	require.NoError(t, err)
	s, use, got := VerifyDeviceWrap(h, mustB64(t, v.BytesB64), dkx)
	require.Equal(t, v.Expect.Reason, string(got))
	if got == ReasonOK {
		require.Equal(t, v.Expect.WrapUse, use.String())
		require.NotEmpty(t, s.EKPKCS8)
		return
	}
	require.Equal(t, DeviceSecrets{}, s)
	require.Zero(t, use)
}

// wrapFixture is a committed wrap vector's head, recipient key and wrap.
type wrapFixture struct {
	h    DeviceHead
	dkx  *ecdh.PrivateKey
	wrap []byte
	w    DeviceWrap
	v    vector
}

func wrapVector(t *testing.T, name string) wrapFixture {
	t.Helper()
	for _, v := range loadVectors(t).Vectors {
		if v.Name != name {
			continue
		}
		h, err := ParseDeviceHead(mustB64(t, v.PrevDeviceHead), mustB64(t, v.PrevHead))
		require.NoError(t, err)
		dkx, err := ecdh.P384().NewPrivateKey(mustB64(t, v.RecipientDkxD))
		require.NoError(t, err)
		wrap := mustB64(t, v.BytesB64)
		w, err := DecodeDeviceWrap(wrap)
		require.NoError(t, err)
		return wrapFixture{h: h, dkx: dkx, wrap: wrap, w: w, v: v}
	}
	t.Fatalf("no vector %s", name)
	return wrapFixture{}
}

// selfEntry is self's entry in the head's persisted active set.
func selfEntry(t *testing.T, h DeviceHead) DeviceEntry {
	t.Helper()
	i := slices.IndexFunc(h.View().Active, func(e DeviceEntry) bool { return e.ID == h.SelfDeviceID })
	require.GreaterOrEqual(t, i, 0, "self is active")
	return h.View().Active[i]
}

// Every wrap vector is opened against the head VerifyDeviceLedger persists for
// that account, so the secrets records the wraps rely on (device-head #10,
// including the add_device that admits self) are the verifier's, not only the
// generator's.
func TestWrapVectorsOpenAgainstAVerifiedHead(t *testing.T) {
	var ledger vector
	var wraps []vector
	for _, v := range loadVectors(t).Vectors {
		switch {
		case v.Name == "wrap/head-from-verified-ledger":
			ledger = v
		case v.Type == "device-wrap-open":
			wraps = append(wraps, v)
		}
	}
	require.Equal(t, "device-ledger", ledger.Type)
	require.Equal(t, "ok", ledger.Expect.Reason)
	require.Len(t, wraps, 20)
	for _, v := range wraps {
		require.Equal(t, ledger.Expect.DeviceHeadB64, v.PrevDeviceHead, v.Name)
		require.Equal(t, ledger.Expect.HeadB64, v.PrevHead, v.Name)
	}
	h, err := ParseDeviceHead(mustB64(t, ledger.Expect.DeviceHeadB64), mustB64(t, ledger.Expect.HeadB64))
	require.NoError(t, err)
	ops := make([]string, len(h.Secrets))
	for i, rec := range h.Secrets {
		ops[i] = strings.Split(rec, ".")[1]
	}
	require.Equal(t, []string{OpAddDevice, OpRotateEK, OpRotateRoot}, ops)
}

// Go seals, Go opens: the KDF, AAD and framing agree with themselves; the
// vectors prove they agree with Web Crypto. Each tamper leaves a decodable
// wrap, so only the AEAD (or the ECDH under another epk) can refuse it.
func TestSealOpenRoundTripRefusesTampering(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	sec, use, r := VerifyDeviceWrap(f.h, f.wrap, f.dkx)
	require.Equal(t, ReasonOK, r)
	require.Equal(t, WrapCurrent, use)
	sealed, err := SealDeviceWrap(f.h, selfEntry(t, f.h), f.w.DeviceEntryDigest, sec)
	require.NoError(t, err)
	got, use, r := VerifyDeviceWrap(f.h, sealed, f.dkx)
	require.Equal(t, ReasonOK, r)
	require.Equal(t, WrapCurrent, use)
	require.Equal(t, sec, got)

	w, err := DecodeDeviceWrap(sealed)
	require.NoError(t, err)
	require.False(t, bytes.Equal(w.EPK, f.w.EPK), "the epk is fresh per wrap")
	require.False(t, bytes.Equal(w.Nonce, f.w.Nonce), "the nonce is fresh per wrap")
	for name, edit := range map[string]func(*DeviceWrap){
		"ct":    func(x *DeviceWrap) { x.CT = slices.Clone(x.CT); x.CT[3] ^= 1 },
		"tag":   func(x *DeviceWrap) { x.CT = slices.Clone(x.CT); x.CT[len(x.CT)-1] ^= 1 },
		"nonce": func(x *DeviceWrap) { x.Nonce = slices.Clone(x.Nonce); x.Nonce[0] ^= 1 },
		"epk":   func(x *DeviceWrap) { x.EPK = testPoint(t) },
	} {
		x := w
		edit(&x)
		b := x.Encode()
		require.NotNil(t, b, name)
		_, _, r := VerifyDeviceWrap(f.h, b, f.dkx)
		require.Equal(t, ReasonWrapInvalid, r, name)
	}
}

// The HKDF info and the AAD are byte-exact to §2.5: info = "concord-device-wrap/v1"
// ‖ 0x00 ‖ realm(36) ‖ account(36) ‖ recipient(36) ‖ epk(97) ‖ recipient_dkx(97)
// (325 bytes); AAD = realm ‖ account ‖ recipient ‖ device_entry_digest ‖ epk ‖
// nonce (265 bytes).
func TestWrapInfoAndAADLayout(t *testing.T) {
	realm, account, recipient := newID(), newID(), newID()
	entry, epk, dkx, nonce := rnd(48), testPoint(t), testPoint(t), rnd(12)

	info := []byte(wrapInfo(realm, account, recipient, epk, dkx))
	require.Len(t, info, 325)
	require.Len(t, "concord-device-wrap/v1", 22)
	want := slices.Concat([]byte("concord-device-wrap/v1"), []byte{0}, []byte(realm), []byte(account),
		[]byte(recipient), epk, dkx)
	require.Equal(t, want, info)

	aad := wrapAAD(realm, account, recipient, entry, epk, nonce)
	require.Len(t, aad, 265)
	require.Equal(t, slices.Concat([]byte(realm), []byte(account), []byte(recipient), entry, epk, nonce), aad)
}

// The epk check (§2.4, D-244, DoR:2848): crypto/ecdh refuses a compressed, a
// hybrid, an off-curve and an infinity encoding, and so does the device-wrap
// codec at the epk position. Two independent layers, each pinned here: the
// codec through DecodeDeviceWrap, the ECDH through openWrap on a decoded struct
// the codec never saw.
func TestWrapEPKValidation(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	p := testPoint(t)
	compressed := append([]byte{0x02 | p[96]&1}, p[1:49]...)
	off := slices.Clone(p)
	off[96] ^= 1
	bad := map[string][]byte{
		"compressed": compressed, "hybrid-0x06/0x07": hybridOf(p), "off-curve": off,
		"infinity": {0x00}, "zero-point": append([]byte{0x04}, make([]byte, 96)...), "empty": nil,
	}
	for name, epk := range bad {
		_, err := ecdh.P384().NewPublicKey(epk)
		require.Error(t, err, name)
		x := f.w
		x.EPK = epk
		s, use, r := openWrap(f.h, x, f.dkx)
		require.Equal(t, ReasonWrapInvalid, r, name)
		require.Equal(t, DeviceSecrets{}, s, name)
		require.Zero(t, use, name)
	}
	// The codec half: a 97-byte epk that is not a point, spliced into a valid
	// wrap's text, is malformed.
	from := `"` + base64.StdEncoding.EncodeToString(f.w.EPK) + `"`
	require.Equal(t, 1, bytes.Count(f.wrap, []byte(from)))
	for _, epk := range [][]byte{off, hybridOf(p)} {
		spliced := bytes.Replace(f.wrap, []byte(from), []byte(`"`+base64.StdEncoding.EncodeToString(epk)+`"`), 1)
		_, err := DecodeDeviceWrap(spliced)
		require.ErrorIs(t, err, ErrMalformed)
		_, _, r := VerifyDeviceWrap(f.h, spliced, f.dkx)
		require.Equal(t, ReasonWrapInvalid, r)
	}
}

// Check 1's head side: the CP monitor ("" self) holds no secrets entry, a wrap
// for another realm or account is not this head's, and a nil key or an
// undecodable wrap is refused rather than a panic.
func TestVerifyDeviceWrapCheck1Head(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	_, _, r := VerifyDeviceWrap(f.h, f.wrap, f.dkx)
	require.Equal(t, ReasonOK, r, "control")
	for name, edit := range map[string]func(*DeviceHead){
		"monitor":       func(h *DeviceHead) { h.SelfDeviceID = "" },
		"other-realm":   func(h *DeviceHead) { h.RealmID = newID() },
		"other-account": func(h *DeviceHead) { h.AccountID = newID() },
		"other-self":    func(h *DeviceHead) { h.SelfDeviceID = newID() },
		"no-secrets":    func(h *DeviceHead) { h.Secrets = nil },
	} {
		h := f.h
		edit(&h)
		s, use, r := VerifyDeviceWrap(h, f.wrap, f.dkx)
		require.Equal(t, ReasonWrapInvalid, r, name)
		require.Equal(t, DeviceSecrets{}, s, name)
		require.Zero(t, use, name)
	}
	_, _, r = VerifyDeviceWrap(f.h, f.wrap, nil)
	require.Equal(t, ReasonWrapInvalid, r, "nil dkx")
	other, err := ecdh.P384().GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, _, r = VerifyDeviceWrap(f.h, f.wrap, other)
	require.Equal(t, ReasonWrapInvalid, r, "another dkx")
	_, _, r = VerifyDeviceWrap(f.h, f.wrap[:len(f.wrap)-1], f.dkx)
	require.Equal(t, ReasonWrapInvalid, r, "undecodable")
}

// openWrap is reached only after decoding, which fixes the nonce at 12 bytes;
// a struct with another length is refused rather than reaching GCM's Open,
// which panics on it.
func TestOpenWrapRefusesANonceOfAnotherLength(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	for _, n := range []int{0, 11, 13, 16} {
		x := f.w
		x.Nonce = rnd(n)
		require.NotPanics(t, func() {
			_, _, r := openWrap(f.h, x, f.dkx)
			require.Equal(t, ReasonWrapInvalid, r, n)
		})
	}
}

// secretsEntry fails closed on a head it cannot read unambiguously: a record
// named twice, a record whose root presence disagrees with its op, and a
// malformed record anywhere all name no entry. The last record is current.
func TestSecretsEntryFailsClosed(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	require.Len(t, f.h.Secrets, 3)
	for i, rec := range f.h.Secrets {
		e, ok := secretsEntry(f.h, mustB64(t, strings.Split(rec, ".")[0]))
		require.True(t, ok, i)
		require.Equal(t, i == 2, e.latest, i)
	}
	entryOf := func(rec string) []byte { return mustB64(t, strings.Split(rec, ".")[0]) }
	withSecrets := func(recs ...string) DeviceHead { h := f.h; h.Secrets = recs; return h }
	parts := func(rec string) []string { return strings.Split(rec, ".") }

	added, rotEK, latest := f.h.Secrets[0], f.h.Secrets[1], f.h.Secrets[2]
	_, ok := secretsEntry(withSecrets(added, rotEK, latest, latest), entryOf(latest))
	require.False(t, ok, "named twice")
	p := parts(rotEK)
	p[2] = parts(latest)[2]
	_, ok = secretsEntry(withSecrets(added, strings.Join(p, "."), latest), entryOf(rotEK))
	require.False(t, ok, "a rotate_ek record with a root")
	p = parts(latest)
	p[2] = ""
	_, ok = secretsEntry(withSecrets(added, rotEK, strings.Join(p, ".")), entryOf(latest))
	require.False(t, ok, "a rotate_root record without a root")
	p = parts(added)
	p[1] = OpRotateEK
	_, ok = secretsEntry(withSecrets(strings.Join(p, "."), rotEK, latest), entryOf(added))
	require.False(t, ok, "add_device relabelled rotate_ek keeps its root")
	_, ok = secretsEntry(withSecrets(added, "not-a-record", latest), entryOf(latest))
	require.False(t, ok, "a malformed record elsewhere")
	_, ok = secretsEntry(withSecrets(added, rotEK, latest), rnd(48))
	require.False(t, ok, "unknown entry")
}

// Check 3's root half accepts only a P-384 ECDSA key whose scalar signs under the
// entry's root: another curve (P-521's r and s would overflow the 96-byte P1363
// form), an RSA key and garbage are refused without a panic.
func TestRootCorrespondsAcceptsOnlyTheEntryRoot(t *testing.T) {
	entry := rnd(48)
	k := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	require.True(t, rootCorresponds(der, pointOf(t, k), entry), "control")
	require.False(t, rootCorresponds(der, testPoint(t), entry), "another root")
	require.False(t, rootCorresponds(der, nil, entry), "no root")
	for _, c := range []elliptic.Curve{elliptic.P521(), elliptic.P256()} {
		o, err := ecdsa.GenerateKey(c, rand.Reader)
		require.NoError(t, err)
		d, err := x509.MarshalPKCS8PrivateKey(o)
		require.NoError(t, err)
		require.NotPanics(t, func() { require.False(t, rootCorresponds(d, pointOf(t, k), entry), c.Params().Name) })
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(testRSA(t))
	require.NoError(t, err)
	require.False(t, rootCorresponds(rsaDER, pointOf(t, k), entry), "rsa")
	require.False(t, rootCorresponds(rnd(185), pointOf(t, k), entry), "garbage")
}

// Check 3's EK half: an encrypt-and-decrypt round trip under the expected SPKI.
func TestEKCorrespondsAcceptsOnlyTheEntryEK(t *testing.T) {
	der, err := x509.MarshalPKCS8PrivateKey(testRSA(t))
	require.NoError(t, err)
	require.True(t, ekCorresponds(der, testEKSPKI(t)), "control")
	f := wrapVector(t, "wrap/current")
	e, ok := secretsEntry(f.h, f.w.DeviceEntryDigest)
	require.True(t, ok)
	require.False(t, ekCorresponds(der, e.spki), "another EK")
	ecDER, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	require.NoError(t, err)
	require.False(t, ekCorresponds(ecDER, testEKSPKI(t)), "an EC key")
	require.False(t, ekCorresponds(der, testEKSPKI(t)[:549]), "an invalid SPKI")
	require.False(t, ekCorresponds(rnd(64), testEKSPKI(t)), "garbage")
}

// Bytes after the PKCS#8 element are refused for both keys (E-N4):
// x509.ParsePKCS8PrivateKey alone ignores them (measured), so parsePKCS8
// refuses them explicitly; the unpadded DER of the same key is the control.
func TestPKCS8TrailingBytesAreRefused(t *testing.T) {
	entry := rnd(48)
	k := testKey(t)
	ecDER, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	rsaDER, err := x509.MarshalPKCS8PrivateKey(testRSA(t))
	require.NoError(t, err)
	require.True(t, rootCorresponds(ecDER, pointOf(t, k), entry), "control: the root")
	require.True(t, ekCorresponds(rsaDER, testEKSPKI(t)), "control: the EK")
	for _, extra := range [][]byte{{0}, {0x05, 0x00}, {1, 2, 3}} {
		_, stdErr := x509.ParsePKCS8PrivateKey(slices.Concat(ecDER, extra))
		require.NoError(t, stdErr, "the stdlib parser accepts trailing %x: the explicit refusal is load-bearing", extra)
		_, err := parsePKCS8(slices.Concat(ecDER, extra))
		require.Error(t, err, "%x", extra)
		require.False(t, rootCorresponds(slices.Concat(ecDER, extra), pointOf(t, k), entry), "root + %x", extra)
		require.False(t, ekCorresponds(slices.Concat(rsaDER, extra), testEKSPKI(t)), "ek + %x", extra)
	}
	_, err = parsePKCS8(nil)
	require.Error(t, err, "empty")
}

// A sealed wrap with a root of another curve reaches check 3 and is refused
// there, without a panic (the P-521 case above, end to end).
func TestVerifyDeviceWrapRefusesAP521Root(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	sec, _, r := VerifyDeviceWrap(f.h, f.wrap, f.dkx)
	require.Equal(t, ReasonOK, r)
	o, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	require.NoError(t, err)
	sec.RootPKCS8, err = x509.MarshalPKCS8PrivateKey(o)
	require.NoError(t, err)
	sealed, err := SealDeviceWrap(f.h, selfEntry(t, f.h), f.w.DeviceEntryDigest, sec)
	require.NoError(t, err)
	require.NotPanics(t, func() {
		_, _, r = VerifyDeviceWrap(f.h, sealed, f.dkx)
	})
	require.Equal(t, ReasonWrapInvalid, r)
}

// SealDeviceWrap seals only to an active device of the head with the dkx the
// set records (a removed device never receives a later wrap, AC20 P3), under a
// 48-byte entry digest, and only a valid device-secrets.
func TestSealDeviceWrapRefusesNonMembers(t *testing.T) {
	f := wrapVector(t, "wrap/current")
	sec, _, r := VerifyDeviceWrap(f.h, f.wrap, f.dkx)
	require.Equal(t, ReasonOK, r)
	self := selfEntry(t, f.h)
	_, err := SealDeviceWrap(f.h, self, f.w.DeviceEntryDigest, sec)
	require.NoError(t, err, "control")

	stranger := self
	stranger.ID = newID()
	otherDKX := self
	copy(otherDKX.DKX[:], testPoint(t))
	for name, c := range map[string]struct {
		h     DeviceHead
		to    DeviceEntry
		entry []byte
		s     DeviceSecrets
	}{
		"not-in-the-set":  {f.h, stranger, f.w.DeviceEntryDigest, sec},
		"another-dkx":     {f.h, otherDKX, f.w.DeviceEntryDigest, sec},
		"short-entry":     {f.h, self, f.w.DeviceEntryDigest[:47], sec},
		"no-ek":           {f.h, self, f.w.DeviceEntryDigest, DeviceSecrets{RootPKCS8: sec.RootPKCS8}},
		"empty-set":       {func() DeviceHead { h := f.h; h.DeviceSet = nil; return h }(), self, f.w.DeviceEntryDigest, sec},
		"malformed-realm": {func() DeviceHead { h := f.h; h.RealmID = "x"; return h }(), self, f.w.DeviceEntryDigest, sec},
	} {
		out, err := SealDeviceWrap(c.h, c.to, c.entry, c.s)
		require.Error(t, err, name)
		require.Nil(t, out, name)
	}
}

// D-S2: a device head whose retained fix shows the raw pending head's new-root
// activation, which its set has not recorded (device-head #7), seals only to
// the set that activation leaves: the pending device, never the active one it
// drops. With the fix removed, the persisted set rules again (control).
func TestSealRecipientFollowsAShownActivation(t *testing.T) {
	var h DeviceHead
	for _, v := range loadVectors(t).Vectors {
		if v.Name == "ledger/status-shows-activation-before-completion" {
			var err error
			h, err = ParseDeviceHead(mustB64(t, v.Expect.DeviceHeadB64), mustB64(t, v.Expect.HeadB64))
			require.NoError(t, err)
		}
	}
	view := h.View()
	require.NotNil(t, view.Pending, "the set stops before the activation")
	require.Len(t, view.Active, 1)
	old, next := view.Active[0], *view.Pending
	require.False(t, activeRecipient(h, old), "the activation dropped it")
	require.True(t, activeRecipient(h, next), "the activation made it active")

	h.Chain.Fix = nil
	require.True(t, activeRecipient(h, old), "control: no fix, the persisted set")
	require.False(t, activeRecipient(h, next), "control: no fix, still pending")
}
