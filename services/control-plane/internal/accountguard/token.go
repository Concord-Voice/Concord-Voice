package accountguard

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

var (
	// ErrTokenInvalid is the one error a guard token presented by a client produces: bad
	// base64 or a second spelling of the same bytes, a wrong length, a key id other than
	// the current one, a failed AEAD open, a version, purpose or ref encoding that is not
	// ours (a credential kind outside decide's set included), a purpose other than the one
	// asked for, a lifetime other than GuardTokenTTL,
	// or a time outside the token's life. It is returned bare, never wrapped, so no caller
	// can tell one cause from another and none can echo the cause to the client.
	ErrTokenInvalid = errors.New("accountguard: invalid guard token")
	// errBadInput means a caller asked to seal or mint what no token or challenge may
	// carry: a zero or unrepresentable time, the nil user, or a purpose outside the closed
	// set. It is the caller's bug, so it is neither a client fault nor ErrKeyUnavailable.
	errBadInput = errors.New("accountguard: invalid seal or mint input")
)

// TokenPurpose names the one action a guard token authorizes. A token opened for any
// other purpose is refused. The zero value is not a purpose.
type TokenPurpose uint8

// The two purposes a guard token serves.
const (
	PurposeCancel  TokenPurpose = iota + 1 // the cancel action
	PurposeResolve                         // the resolve action
)

// valid reports whether p is in the closed set.
func (p TokenPurpose) valid() bool {
	switch p {
	case PurposeCancel, PurposeResolve:
		return true
	default:
		return false
	}
}

// GuardClaims is what a guard token carries: whose account, which action, and the
// credentials the holder proved, in the order the runner gave them. It carries no class.
// It holds no key material, so formatting it is safe; the sealed token is a bearer value
// and is never logged.
type GuardClaims struct {
	UserID    uuid.UUID
	Purpose   TokenPurpose
	Proven    []decide.CredRef
	IssuedAt  time.Time
	ExpiresAt time.Time
}

const (
	// guardTokenAADLabel opens the AAD; the two-byte key id follows it. It is versioned and
	// frozen: changing it orphans every token sealed under it.
	guardTokenAADLabel = "concord/accountguard/guard-token/v1"
	// guardTokenVersion is the plaintext's first byte.
	guardTokenVersion = 1
	// keyIDHeaderLen is the length of the big-endian key id that opens a token and a
	// challenge alike.
	keyIDHeaderLen = 2
	// guardNonceLen and guardTagLen are AES-GCM's standard nonce and tag sizes.
	guardNonceLen = 12
	guardTagLen   = 16
	// guardPlainFixedLen is the plaintext before its refs: version, user, purpose, issue
	// and expiry times (unix ms, int64), and the ref count (uint16).
	guardPlainFixedLen = 1 + 16 + 1 + 8 + 8 + 2
	// encodedRefMinLen is the shortest encoded ref: kind, a zero ID length, and version.
	encodedRefMinLen = 1 + 2 + 8
	// guardWireMinLen is the shortest wire form: a token with no refs.
	guardWireMinLen = keyIDHeaderLen + guardNonceLen + guardPlainFixedLen + guardTagLen
)

// canonicalB64 decodes unpadded base64url and refuses non-zero spare bits in the final
// character (RFC 4648 §3.5).
var canonicalB64 = base64.RawURLEncoding.Strict()

// SealGuardToken seals c into an opaque guard token. The wire form is unpadded base64url
// over
//
//	keyID (int16, big-endian) || nonce (12 bytes) || AES-256-GCM(plaintext)
//
// under the ring's guard-token key, with the AAD "concord/accountguard/guard-token/v1"
// followed by the same two key-id bytes. The plaintext is
//
//	0x01 || UserID (16 bytes) || purpose (1 byte) || IssuedAt (unix ms, int64 BE)
//	    || ExpiresAt (unix ms, int64 BE) || count (uint16 BE) || ref...
//
// with each ref encoded as in OriginFingerprint, in the order given. IssuedAt is
// truncated to the millisecond and ExpiresAt is IssuedAt + decide.GuardTokenTTL; the
// caller's ExpiresAt is ignored, and c itself is not modified.
//
// The ring is resolved first, so a ring that cannot name its key wraps ErrKeyUnavailable
// whatever c holds. A zero or unrepresentable IssuedAt, the nil user, or a purpose
// outside the closed set is refused as the caller's bug; more than 0xFFFF refs or an ID
// over 0xFFFF bytes wraps ErrEncodingTooLong, and a ref of a kind outside decide's set
// wraps ErrUnknownCredKind. On any error the token is "".
func SealGuardToken(ctx context.Context, keys *KeyRing, c GuardClaims) (string, error) {
	keyID, err := keys.CurrentKeyID(ctx)
	if err != nil {
		return "", fmt.Errorf("seal guard token: %w", err)
	}
	aead, err := guardTokenAEAD(keys)
	if err != nil {
		return "", fmt.Errorf("seal guard token: %w", err)
	}
	header, err := keyIDHeader(keyID)
	if err != nil {
		return "", fmt.Errorf("seal guard token: %w", err)
	}
	plaintext, err := encodeGuardClaims(c)
	if err != nil {
		return "", fmt.Errorf("seal guard token: %w", err)
	}

	wire := make([]byte, 0, keyIDHeaderLen+guardNonceLen+len(plaintext)+aead.Overhead())
	wire = append(wire, header...)
	nonce := make([]byte, guardNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal guard token: nonce: %w", err)
	}
	wire = append(wire, nonce...)
	wire = aead.Seal(wire, nonce, plaintext, guardTokenAAD(header))
	return base64.RawURLEncoding.EncodeToString(wire), nil
}

// OpenGuardToken opens tok and returns its claims when it was sealed under the ring's
// current key, is for purpose want, and is live at now:
//
//	IssuedAt - decide.ChallengeSkew <= now < ExpiresAt
//
// compared at millisecond precision. now == ExpiresAt is refused.
//
// The ring is resolved first, so a ring that cannot name its key wraps ErrKeyUnavailable
// even for a malformed token: a server outage never reads as a client fault. Every fault
// in tok itself returns the bare ErrTokenInvalid and the zero claims. Nothing is logged.
func OpenGuardToken(ctx context.Context, keys *KeyRing, tok string, want TokenPurpose, now time.Time) (GuardClaims, error) {
	keyID, err := keys.CurrentKeyID(ctx)
	if err != nil {
		return GuardClaims{}, fmt.Errorf("open guard token: %w", err)
	}
	aead, err := guardTokenAEAD(keys)
	if err != nil {
		return GuardClaims{}, fmt.Errorf("open guard token: %w", err)
	}
	header, err := keyIDHeader(keyID)
	if err != nil {
		return GuardClaims{}, fmt.Errorf("open guard token: %w", err)
	}

	plaintext, ok := openGuardWire(aead, header, tok)
	if !ok {
		return GuardClaims{}, ErrTokenInvalid
	}
	c, ok := decodeGuardClaims(plaintext)
	if !ok || c.Purpose != want || !guardTokenLive(c, now) {
		return GuardClaims{}, ErrTokenInvalid
	}
	return c, nil
}

// guardTokenAEAD returns AES-256-GCM under the ring's guard-token key. A ring that cannot
// derive the key wraps ErrKeyUnavailable.
func guardTokenAEAD(keys *KeyRing) (cipher.AEAD, error) {
	key, err := keys.Key(LabelGuardToken)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("guard token cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("guard token cipher: %w", err)
	}
	return aead, nil
}

// keyIDHeader returns the two big-endian bytes of keyID. binary.Append writes an int16 as
// its two's-complement bytes, so no integer conversion is needed; its error is nil for a
// fixed-size value, so the error is checked rather than expected.
func keyIDHeader(keyID int16) ([]byte, error) {
	header, err := binary.Append(make([]byte, 0, keyIDHeaderLen), binary.BigEndian, keyID)
	if err != nil {
		return nil, fmt.Errorf("encode key id: %w", err)
	}
	return header, nil
}

// guardTokenAAD returns the AAD for a token whose key id encodes as header.
func guardTokenAAD(header []byte) []byte {
	aad := make([]byte, 0, len(guardTokenAADLabel)+len(header))
	aad = append(aad, guardTokenAADLabel...)
	return append(aad, header...)
}

// decodeCanonical decodes s as unpadded base64url and reports whether s is the one
// spelling of the bytes it decodes to. Strict refuses non-zero spare bits, but the
// decoder still skips CR and LF, so a string longer than the canonical encoding of what
// it decoded to is refused too.
func decodeCanonical(s string) ([]byte, bool) {
	b, err := canonicalB64.DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodedLen(len(b)) != len(s) {
		return nil, false
	}
	return b, true
}

// unixMilli returns t as whole Unix milliseconds, truncated, and whether that count
// represents t's millisecond exactly (it does not for a date the int64 cannot reach).
func unixMilli(t time.Time) (int64, bool) {
	ms := t.UnixMilli()
	return ms, time.UnixMilli(ms).Equal(t.Truncate(time.Millisecond))
}

// encodeGuardClaims returns the plaintext for c, issued at c.IssuedAt truncated to the
// millisecond and expiring GuardTokenTTL later.
func encodeGuardClaims(c GuardClaims) ([]byte, error) {
	if c.IssuedAt.IsZero() {
		return nil, fmt.Errorf("%w: zero issue time", errBadInput)
	}
	if c.UserID == uuid.Nil {
		return nil, fmt.Errorf("%w: nil user", errBadInput)
	}
	if !c.Purpose.valid() {
		return nil, fmt.Errorf("%w: unknown token purpose %d", errBadInput, c.Purpose)
	}
	issuedMs, okIssued := unixMilli(c.IssuedAt)
	expiresMs, okExpires := unixMilli(time.UnixMilli(issuedMs).Add(decide.GuardTokenTTL))
	if !okIssued || !okExpires {
		return nil, fmt.Errorf("%w: issue time out of range", errBadInput)
	}
	count := len(c.Proven)
	if count > maxPrefixed {
		return nil, fmt.Errorf("encode guard claims: %w: %d refs", ErrEncodingTooLong, count)
	}

	buf := make([]byte, 0, guardPlainFixedLen+count*refSizeHint)
	buf = append(buf, guardTokenVersion)
	buf = append(buf, c.UserID[:]...)
	buf = append(buf, byte(c.Purpose))
	// binary.Append writes an int64 as its two's-complement bytes; its error is nil for a
	// fixed-size value, so these branches are unreachable today and checked regardless.
	buf, err := binary.Append(buf, binary.BigEndian, issuedMs)
	if err != nil {
		return nil, fmt.Errorf("encode guard claims issue time: %w", err)
	}
	if buf, err = binary.Append(buf, binary.BigEndian, expiresMs); err != nil {
		return nil, fmt.Errorf("encode guard claims expiry: %w", err)
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(count))
	for _, ref := range c.Proven {
		if buf, err = appendRef(buf, ref); err != nil {
			return nil, fmt.Errorf("encode guard claims: %w", err)
		}
	}
	return buf, nil
}

// openGuardWire decodes tok, checks its key id against header, and opens it. It returns
// false for any fault without saying which.
func openGuardWire(aead cipher.AEAD, header []byte, tok string) ([]byte, bool) {
	wire, ok := decodeCanonical(tok)
	// The length check also keeps the nonce slice at exactly NonceSize, which Open
	// requires (it panics otherwise).
	if !ok || len(wire) < guardWireMinLen || !bytes.Equal(wire[:keyIDHeaderLen], header) {
		return nil, false
	}
	nonce := wire[keyIDHeaderLen : keyIDHeaderLen+guardNonceLen]
	// The AAD is built from the CURRENT key id, not the one read off the wire, so the
	// AEAD itself refuses a token sealed under any other id even if the comparison above
	// were lost.
	plaintext, err := aead.Open(nil, nonce, wire[keyIDHeaderLen+guardNonceLen:], guardTokenAAD(header))
	if err != nil {
		return nil, false
	}
	return plaintext, true
}

// plainReader reads a plaintext front to back. A read past the end clears ok, after which
// every read returns the zero value, so a decoder checks ok once.
type plainReader struct {
	b  []byte
	ok bool
}

// take returns the next n bytes, or nil once the buffer cannot supply them.
func (r *plainReader) take(n int) []byte {
	if !r.ok || n > len(r.b) {
		r.ok = false
		return nil
	}
	s := r.b[:n:n]
	r.b = r.b[n:]
	return s
}

func (r *plainReader) u8() uint8 {
	b := r.take(1)
	if !r.ok {
		return 0
	}
	return b[0]
}

func (r *plainReader) u16() int {
	b := r.take(2)
	if !r.ok {
		return 0
	}
	return int(binary.BigEndian.Uint16(b))
}

// i64 reads a big-endian int64. binary.Decode reads the two's-complement bytes straight
// into the int64, so no integer conversion is needed.
func (r *plainReader) i64() int64 {
	b := r.take(8)
	if !r.ok {
		return 0
	}
	var v int64
	if _, err := binary.Decode(b, binary.BigEndian, &v); err != nil {
		r.ok = false
		return 0
	}
	return v
}

// decodeGuardClaims parses a plaintext strictly: version 1, a user that is not nil, a
// purpose in the closed set, a lifetime of exactly GuardTokenTTL, exactly count refs each
// of a known kind and within the buffer, and no byte left over. Anything else is refused
// without saying why.
func decodeGuardClaims(plaintext []byte) (GuardClaims, bool) {
	r := plainReader{b: plaintext, ok: true}
	version := r.u8()
	var c GuardClaims
	copy(c.UserID[:], r.take(len(c.UserID)))
	c.Purpose = TokenPurpose(r.u8())
	c.IssuedAt = time.UnixMilli(r.i64()).UTC()
	c.ExpiresAt = time.UnixMilli(r.i64()).UTC()
	count := r.u16()
	// Each ref needs at least encodedRefMinLen bytes, so a count the remaining bytes
	// cannot hold is refused before anything is allocated for it.
	if !r.ok || version != guardTokenVersion || c.UserID == uuid.Nil || !c.Purpose.valid() ||
		count > len(r.b)/encodedRefMinLen {
		return GuardClaims{}, false
	}
	// Sub saturates rather than overflowing, so an extreme pair cannot wrap to the TTL.
	if c.ExpiresAt.Sub(c.IssuedAt) != decide.GuardTokenTTL {
		return GuardClaims{}, false
	}
	c.Proven = decodeGuardRefs(&r, count)
	if !r.ok || len(r.b) != 0 {
		return GuardClaims{}, false
	}
	return c, true
}

// decodeGuardRefs reads count refs. On a short buffer, or a kind outside decide's set,
// it clears r.ok: SealGuardToken never writes such a kind, so a plaintext carrying one is
// not ours even though it authenticated.
func decodeGuardRefs(r *plainReader, count int) []decide.CredRef {
	if count == 0 {
		return nil
	}
	refs := make([]decide.CredRef, 0, count)
	for range count {
		kind := decide.CredKind(r.u8())
		id := string(r.take(r.u16()))
		version := r.i64()
		if !r.ok || !validCredKind(kind) {
			r.ok = false
			return nil
		}
		refs = append(refs, decide.CredRef{Kind: kind, ID: id, Version: version})
	}
	return refs
}

// guardTokenLive reports IssuedAt - ChallengeSkew <= now < ExpiresAt at millisecond
// precision. Both bounds are whole milliseconds, so truncating now changes no outcome;
// it makes the precision explicit and matches the challenge's rule.
func guardTokenLive(c GuardClaims, now time.Time) bool {
	at := now.Truncate(time.Millisecond)
	return !at.Before(c.IssuedAt.Add(-decide.ChallengeSkew)) && at.Before(c.ExpiresAt)
}
