package authoritycontract

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"slices"
)

// Device wraps: DoR §2.5 `device-wrap` and `device-secrets` checks 1–4 (D-244).
//
// This is the package's one crypto/rand file (purity_test.go): SealDeviceWrap
// draws the fresh epk and the nonce, and VerifyDeviceWrap's check 3 draws a fresh
// signature and a fresh 32-byte probe. Go 1.26 ignores the Reader argument of
// ecdh.GenerateKey, ecdsa.Sign and rsa.EncryptOAEP and always uses a secure
// source; rand.Reader is passed anyway, so the call sites read the same on any
// toolchain.
//
// Key-material hygiene. The byte slices this file itself holds (Z, K, the
// encoded device-secrets plaintext, the decoded keys of a wrap that checks 2–3
// refuse, the probe and its decryption) are zeroed before the function that
// holds them returns, on every path. Nothing else is, and this file does not
// pretend otherwise: the ephemeral and recipient *ecdh.PrivateKey, the
// *ecdsa.PrivateKey and *rsa.PrivateKey parsed from PKCS#8 (their big.Int and
// precomputed fields), the AES key schedule inside the cipher.AEAD and HKDF's
// HMAC state are out of reach; so are the codec's intermediates. A
// DecodeDeviceSecrets that fails part-way returns no DeviceSecrets, so a key it
// had already decoded (root_pkcs8 before ek_pkcs8 is refused, say) is dropped
// unzeroed; on a decode that succeeds, and in SealDeviceWrap's
// DeviceSecrets.Encode, the base64 text the codec builds for each key is a Go
// string, immutable and never zeroed, and only the plaintext buffer is cleared.
// The accepted DeviceSecrets is the caller's to zero. No error and no Reason
// here carries key material, and nothing here logs.

const (
	// wrapInfoLabel is "concord-device-wrap/v1" ‖ 0x00, HKDF's info prefix.
	wrapInfoLabel = "concord-device-wrap/v1\x00"
	// rootCheckLabel is "concord-device-secrets-check/v1" ‖ 0x00, check 3's
	// signed prefix. The signature never leaves this file.
	rootCheckLabel = "concord-device-secrets-check/v1\x00"
	wrapKeyLen     = 32 // HKDF L: an AES-256 key
	wrapNonceLen   = 12 // GCM's 96-bit CSPRNG nonce
	wrapEntryLen   = 48 // device_entry_digest, b48
	ekProbeLen     = 32 // check 3's fresh value
)

// wrapInfo is HKDF's info: "concord-device-wrap/v1" ‖ 0x00 ‖ ascii realm(36) ‖
// ascii account(36) ‖ ascii recipient(36) ‖ epk(97) ‖ recipient_dkx(97), 325
// bytes. crypto/hkdf takes info as a string.
func wrapInfo(realm, account, recipient string, epk, dkx []byte) string {
	return wrapInfoLabel + realm + account + recipient + string(epk) + string(dkx)
}

// wrapAAD is ascii realm ‖ ascii account ‖ ascii recipient ‖ device_entry_digest
// ‖ epk ‖ nonce, 265 bytes.
func wrapAAD(realm, account, recipient string, entry, epk, nonce []byte) []byte {
	return slices.Concat([]byte(realm), []byte(account), []byte(recipient), entry, epk, nonce)
}

// wrapAEAD derives K = HKDF-SHA-384(Z, salt device_entry_digest, info, 32) and
// returns AES-256-GCM under it. K is zeroed before it returns; the AEAD's own
// key schedule cannot be.
func wrapAEAD(z, entry []byte, info string) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha512.New384, z, entry, info, wrapKeyLen)
	if err != nil {
		return nil, err
	}
	defer clear(k)
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// errSeal names the stage that refused; it never carries key material.
func errSeal(stage string) error {
	return errors.New("authoritycontract: SealDeviceWrap: " + stage)
}

// SealDeviceWrap wraps s to recipient under self's secrets entry entryDigest
// (§2.5, D-244). recipient must be an active device of h's persisted set with
// the dkx that set records: a removed device never receives a later wrap (AC20
// P3), and a dkx the ledger does not bind is not one to encrypt a root to.
//
// h must be the DeviceHead that VerifyDeviceLedger returned just before this
// call. That call refused a device-status older than τ; this one reads no
// clock, so an older head can wrap the root to a device whose removal the CP
// is withholding (DoR:919).
func SealDeviceWrap(h DeviceHead, recipient DeviceEntry, entryDigest []byte, s DeviceSecrets) ([]byte, error) {
	if len(entryDigest) != wrapEntryLen || !activeRecipient(h, recipient) {
		return nil, errSeal("recipient or entry")
	}
	pt := s.Encode()
	if pt == nil {
		return nil, errSeal("device-secrets")
	}
	defer clear(pt)
	epk, nonce, ct, err := sealTo(h, recipient, entryDigest, pt)
	if err != nil {
		return nil, err
	}
	out := DeviceWrap{RealmID: h.RealmID, AccountID: h.AccountID, RecipientDeviceID: recipient.ID,
		DeviceEntryDigest: entryDigest, EPK: epk, Nonce: nonce, CT: ct}.Encode()
	if out == nil {
		return nil, errSeal("device-wrap")
	}
	return out, nil
}

// sealTo runs the key agreement and the AEAD: a fresh ephemeral key, Z its
// 48-byte x-coordinate with recipient's dkx, a fresh nonce.
func sealTo(h DeviceHead, recipient DeviceEntry, entry, pt []byte) (epk, nonce, ct []byte, err error) {
	peer, err := ecdh.P384().NewPublicKey(recipient.DKX[:])
	if err != nil {
		return nil, nil, nil, errSeal("recipient dkx")
	}
	eph, err := ecdh.P384().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, errSeal("ephemeral key")
	}
	z, err := eph.ECDH(peer)
	if err != nil {
		return nil, nil, nil, errSeal("key agreement")
	}
	defer clear(z)
	epk = eph.PublicKey().Bytes()
	g, err := wrapAEAD(z, entry, wrapInfo(h.RealmID, h.AccountID, recipient.ID, epk, recipient.DKX[:]))
	if err != nil {
		return nil, nil, nil, errSeal("key derivation")
	}
	nonce = make([]byte, wrapNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, nil, errSeal("nonce")
	}
	//nolint:gosec // G407 false positive: nonce is fresh crypto/rand output, read just above.
	ct = g.Seal(nil, nonce, pt, wrapAAD(h.RealmID, h.AccountID, recipient.ID, entry, epk, nonce))
	return epk, nonce, ct, nil
}

// activeRecipient: r is active with exactly that dkx in h's effective set
// (D-S2). That is the persisted set, unless h's retained fix shows the raw
// pending head's new-root activation and the set stops before it (device-head
// #7: a status showed it, no entry recorded it): then it is the set that
// activation leaves (DR8, NormalizeDeviceSet), so a device the activation
// dropped gets no wrap. A set that already recorded the activation has no
// pending device left to promote, and keeps its active set.
func activeRecipient(h DeviceHead, r DeviceEntry) bool {
	set := h.DeviceSet
	raw, rr := headPub(h.Chain)
	if fix, err := fixStatus(h.Chain.Fix); rr == ReasonOK && err == nil && installsRoot(raw) && fixNames(fix, raw) &&
		!effectivePending(raw, *fix) {
		set = NormalizeDeviceSet(set, true, false, false, false, false)
	}
	return slices.ContainsFunc(viewOf(set, 0, 0, h.AdmittedBy).Active, func(e DeviceEntry) bool { return e.ID == r.ID && e.DKX == r.DKX })
}

// VerifyDeviceWrap opens wrap with self's dkx and accepts its device-secrets
// only under §2.5's checks 1–4 (D-244). Every failure is wrap_invalid with no
// secrets and no use: the Reason does not say which check refused.
func VerifyDeviceWrap(h DeviceHead, wrap []byte, dkx *ecdh.PrivateKey) (DeviceSecrets, WrapUse, Reason) {
	w, err := DecodeDeviceWrap(wrap) // EN5; the epk position is a §2.4 point (kP97)
	if err != nil || dkx == nil {
		return DeviceSecrets{}, 0, ReasonWrapInvalid
	}
	return openWrap(h, w, dkx)
}

// openWrap is VerifyDeviceWrap after decoding.
func openWrap(h DeviceHead, w DeviceWrap, dkx *ecdh.PrivateKey) (DeviceSecrets, WrapUse, Reason) {
	refused := func() (DeviceSecrets, WrapUse, Reason) { return DeviceSecrets{}, 0, ReasonWrapInvalid }
	// Check 1: this head's account, addressed to self, under one of self's
	// secrets entries. The CP monitor's self is "", which no decoded wrap names
	// (recipient_device_id is s36v4), so the recipient comparison refuses it.
	if w.RealmID != h.RealmID || w.AccountID != h.AccountID || w.RecipientDeviceID != h.SelfDeviceID {
		return refused()
	}
	e, ok := secretsEntry(h, w.DeviceEntryDigest)
	if !ok {
		return refused()
	}
	pt, ok := decryptWrap(w, dkx)
	if !ok {
		return refused()
	}
	s, err := DecodeDeviceSecrets(pt)
	clear(pt)
	if err != nil {
		return refused()
	}
	if !secretsAccepted(e, s, w.DeviceEntryDigest) {
		clear(s.RootPKCS8)
		clear(s.EKPKCS8)
		return refused()
	}
	if e.latest { // check 4
		return s, WrapCurrent, ReasonOK
	}
	return s, WrapHistory, ReasonOK
}

// decryptWrap: Z = ECDH(dkx, epk), K by wrapAEAD, then AES-256-GCM open under
// the 265-byte AAD. crypto/ecdh refuses an epk that is not a 97-byte 0x04 point
// on P-384 (compressed, hybrid, off-curve, infinity), the §2.4 check the codec
// already made at decode; this is the second layer, and the one that makes the
// key agreement possible at all.
func decryptWrap(w DeviceWrap, dkx *ecdh.PrivateKey) ([]byte, bool) {
	epk, err := ecdh.P384().NewPublicKey(w.EPK)
	if err != nil {
		return nil, false
	}
	z, err := dkx.ECDH(epk) // a dkx of another curve is refused here
	if err != nil {
		return nil, false
	}
	defer clear(z)
	info := wrapInfo(w.RealmID, w.AccountID, w.RecipientDeviceID, w.EPK, dkx.PublicKey().Bytes())
	g, err := wrapAEAD(z, w.DeviceEntryDigest, info)
	if err != nil || len(w.Nonce) != g.NonceSize() { // Open panics on a nonce of another length
		return nil, false
	}
	pt, err := g.Open(nil, w.Nonce, w.CT, wrapAAD(w.RealmID, w.AccountID, w.RecipientDeviceID, w.DeviceEntryDigest, w.EPK, w.Nonce))
	if err != nil {
		return nil, false
	}
	return pt, true
}

// secretsRecord is one of self's secrets entries (device-head #10): the op, the
// entry's effective active root (none for rotate_ek) and the EK SPKI current at
// the entry.
type secretsRecord struct {
	op         string
	root, spki []byte
	latest     bool // check 4: self's latest secrets entry
}

// carriesRoot is check 2's rule: root_pkcs8 is present iff op ∈ {add_device,
// rotate_root}.
func carriesRoot(op string) bool { return op == OpAddDevice || op == OpRotateRoot }

// secretsEntry is check 1's lookup: the record naming entry among self's secrets
// entries. It fails closed on a head it cannot read unambiguously: any record
// that does not parse, a record whose root presence disagrees with its op, or an
// entry named twice, names no entry.
func secretsEntry(h DeviceHead, entry []byte) (secretsRecord, bool) {
	var found secretsRecord
	n := 0
	for i, rec := range h.Secrets {
		dig, r, ok := parseSecretsRecord(rec)
		if !ok {
			return secretsRecord{}, false
		}
		if bytes.Equal(dig, entry) {
			r.latest = i == len(h.Secrets)-1
			found, n = r, n+1
		}
	}
	return found, n == 1
}

// parseSecretsRecord reads `<device_entry_digest b64>.<op>.<root point b64, or
// empty>.<EK SPKI b64>`. The point and the SPKI are validated where they are
// used (VerifyP384, ValidateEKSPKI).
func parseSecretsRecord(rec string) (entry []byte, r secretsRecord, ok bool) {
	p, err := recordParts(rec, 4)
	if err != nil || !slices.Contains(secretsOps, p[1]) || (p[2] != "") != carriesRoot(p[1]) {
		return nil, secretsRecord{}, false
	}
	entry, err = decodeB64(p[0])
	if err != nil || len(entry) != wrapEntryLen {
		return nil, secretsRecord{}, false
	}
	r.op = p[1]
	if p[2] != "" {
		if r.root, err = decodeB64(p[2]); err != nil {
			return nil, secretsRecord{}, false
		}
	}
	if r.spki, err = decodeB64(p[3]); err != nil {
		return nil, secretsRecord{}, false
	}
	return entry, r, true
}

// secretsAccepted is checks 2 and 3. The root is proven only for an entry that
// carries one, so a root_pkcs8 on a rotate_ek is refused by check 2 alone.
func secretsAccepted(e secretsRecord, s DeviceSecrets, entry []byte) bool {
	wantRoot := carriesRoot(e.op)
	if (len(s.RootPKCS8) > 0) != wantRoot || len(s.EKPKCS8) == 0 { // check 2
		return false
	}
	if wantRoot && !rootCorresponds(s.RootPKCS8, e.root, entry) { // check 3, the root
		return false
	}
	return ekCorresponds(s.EKPKCS8, e.spki) // check 3, the EK
}

// rootCorresponds is check 3's root half: the imported scalar signs
// "concord-device-secrets-check/v1" ‖ 0x00 ‖ device_entry_digest and the
// signature verifies under the entry's effective active root. PKCS#8's optional
// publicKey field plays no part: x509 derives the public key from the scalar,
// and the verification is under root, never under any key the PKCS#8 names.
func rootCorresponds(der, root, entry []byte) bool {
	k, err := parsePKCS8(der)
	if err != nil {
		return false
	}
	priv, ok := k.(*ecdsa.PrivateKey)
	// P-384 only: a larger curve's r or s would not fit FillBytes(48) below.
	if !ok || priv.Curve != elliptic.P384() {
		return false
	}
	msg := slices.Concat([]byte(rootCheckLabel), entry)
	sum := sha512.Sum384(msg)
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return false
	}
	sig := make([]byte, 96)
	r.FillBytes(sig[:48])
	s.FillBytes(sig[48:])
	return VerifyP384(root, msg, sig) // SHA-384 over msg, under the entry's root
}

// ekCorresponds is check 3's EK half: a fresh 32-byte value RSA-OAEP-encrypted
// under the expected SPKI decrypts to itself under the imported EK. OAEP uses
// SHA-256 with MGF1-SHA-256 and an empty label: the EK is RSA-OAEP-4096/SHA-256
// (DoR §2.2 key table; ek_alg rsa-oaep-4096-sha256, §2.5 ek-binding).
func ekCorresponds(der, spki []byte) bool {
	pub, err := ValidateEKSPKI(spki)
	if err != nil {
		return false
	}
	k, err := parsePKCS8(der)
	if err != nil {
		return false
	}
	priv, ok := k.(*rsa.PrivateKey)
	if !ok {
		return false
	}
	probe := make([]byte, ekProbeLen)
	defer clear(probe)
	if _, err := rand.Read(probe); err != nil {
		return false
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, probe, nil)
	if err != nil {
		return false
	}
	got, err := rsa.DecryptOAEP(sha256.New(), nil, priv, ct, nil)
	defer clear(got)
	return err == nil && subtle.ConstantTimeCompare(got, probe) == 1
}

// parsePKCS8 is x509.ParsePKCS8PrivateKey, accepting only the canonical
// encoding of the key it parses. The stdlib parser is lenient where Web Crypto
// is not (measured on Go 1.26.9 and Node 24): it ignores bytes after the
// PrivateKeyInfo, ignores the ECPrivateKey's publicKey field and its inner
// curve parameters, and ignores bytes after the ECPrivateKey inside the
// OCTET STRING, so whether such a key verified would depend on the
// implementation (NEW-1). Go's re-encoding of an honest key is byte-equal to
// Web Crypto's export (P-384, 185 bytes; RSA-4096, 2375 bytes), so requiring
// it refuses all four and no honest key; a port compares against
// exportKey(importKey(der)) (wrap/pkcs8-*).
func parsePKCS8(der []byte) (any, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	re, err := x509.MarshalPKCS8PrivateKey(k)
	defer clear(re)
	if err != nil || !bytes.Equal(re, der) {
		return nil, errors.New("authoritycontract: a PKCS#8 that is not its key's canonical encoding")
	}
	return k, nil
}
