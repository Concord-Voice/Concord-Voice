package accountguard

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// These tests pin the guard token's wire form, its strict decoder, its clock rules and
// its failure split. Every expected value is a literal or is rebuilt in the test with
// crypto/aes and crypto/cipher, never taken from the code under test, so a change to the
// format or to the AAD fails here instead of agreeing with itself.

const (
	// tokenAADLabel is the AAD prefix the design freezes; the key id follows it.
	tokenAADLabel = "concord/accountguard/guard-token/v1"
	// guardTTL and guardSkew restate decide.GuardTokenTTL and decide.ChallengeSkew as
	// literals so a drifted constant does not move the expectation with it.
	guardTTL  = 5 * time.Minute
	guardSkew = 60 * time.Second

	// The fixed layout of the plaintext: version, user, purpose, two times, ref count.
	plainHeaderLen = 1 + 16 + 1 + 8 + 8 + 2
	// The fixed layout of the wire: key id, GCM nonce, GCM tag.
	wireKeyIDLen = 2
	wireNonceLen = 12
	wireTagLen   = 16
)

// guardT0 is a millisecond-aligned instant every case is built around.
func guardT0() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }

func guardUserID() uuid.UUID { return uuid.MustParse("3f6b1c2e-9a47-4d85-b0e3-71c8a5d2f496") }

// guardRefs returns refs in a deliberately non-canonical order (kind 7, 1, 6), with
// versions that use every byte of an int64, a negative one, and a runner key for an ID.
func guardRefs() []decide.CredRef {
	return []decide.CredRef{
		{Kind: decide.CredWebAuthn, ID: "d1c2b3a4-5566-4778-8899-aabbccddeeff", Version: 0x1122334455667788},
		{Kind: decide.CredPassword, ID: "7c0e5d2a-41f3-4b96-a2d8-0e9f6b3c5a71", Version: -1},
		{Kind: decide.CredTOTP, ID: "runner:totp-slot-17", Version: 4242},
	}
}

func guardClaims(p TokenPurpose) GuardClaims {
	return GuardClaims{UserID: guardUserID(), Purpose: p, Proven: guardRefs(), IssuedAt: guardT0()}
}

// newGuardRing returns a ring over ringInputAlpha on an empty registry.
func newGuardRing(t *testing.T) (*KeyRing, *sql.DB) {
	t.Helper()
	db := ringDB(t)
	return NewKeyRing(db, ringInputAlpha), db
}

func sealToken(t *testing.T, ring *KeyRing, c GuardClaims) string {
	t.Helper()
	tok, err := SealGuardToken(context.Background(), ring, c)
	require.NoError(t, err)
	require.NotEmpty(t, tok)
	return tok
}

func mustOpenToken(t *testing.T, ring *KeyRing, tok string, want TokenPurpose, now time.Time) GuardClaims {
	t.Helper()
	claims, err := OpenGuardToken(context.Background(), ring, tok, want, now)
	require.NoError(t, err)
	return claims
}

// mustRefuseToken requires the single opaque client error and an empty result.
func mustRefuseToken(t *testing.T, ring *KeyRing, tok string, want TokenPurpose, now time.Time) {
	t.Helper()
	claims, err := OpenGuardToken(context.Background(), ring, tok, want, now)
	assertBare(t, err, ErrTokenInvalid)
	assert.Equal(t, GuardClaims{}, claims, "a refused token must yield the zero claims")
	assertNoEcho(t, err, tok, ErrTokenInvalid)
}

// assertNoEcho requires that err's text does not carry the input. An empty input is
// contained in every string, and a short one can sit inside the sentinel's own text, so
// neither can be told apart from the sentinel by this check; assertBare pins the exact
// text in those cases.
func assertNoEcho(t *testing.T, err error, input string, sentinel error) {
	t.Helper()
	if input == "" || strings.Contains(sentinel.Error(), input) {
		return
	}
	assert.NotContains(t, err.Error(), input, "the error text must not carry the input")
}

// assertBare requires err to be the sentinel itself: not wrapped, not re-created, and
// not carrying detail in its text.
func assertBare(t *testing.T, err, sentinel error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, err == sentinel, "want the bare sentinel, got %T", err)
	assert.Equal(t, sentinel.Error(), err.Error())
}

func wireBytes(t *testing.T, tok string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err, "the token must be unpadded base64url")
	return b
}

func encodeWire(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// idBytes is the two-byte big-endian form of a key id.
func idBytes(t *testing.T, id int16) []byte {
	t.Helper()
	b, err := binary.Append(nil, binary.BigEndian, id)
	require.NoError(t, err)
	return b
}

// aadFor is the AAD the design freezes for a key id: the label bytes, then the id.
func aadFor(t *testing.T, id int16) []byte {
	t.Helper()
	return append([]byte(tokenAADLabel), idBytes(t, id)...)
}

func u16(t *testing.T, n int) uint16 {
	t.Helper()
	if n >= 0 && n <= math.MaxUint16 {
		return uint16(n)
	}
	t.Fatalf("u16: %d does not fit a uint16", n)
	return 0
}

// sealRaw seals plaintext the way a token is sealed, but with every input the caller
// names: the key, the id written to the header, and the AAD.
func sealRaw(t *testing.T, key []byte, headerID int16, aad, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, gcm.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	wire := append(idBytes(t, headerID), nonce...)
	wire = gcm.Seal(wire, nonce, plaintext, aad)
	return encodeWire(wire)
}

// resealPlain seals plaintext under exactly the ring's current key, id and AAD, so the
// only thing the code under test can object to is the plaintext.
func resealPlain(t *testing.T, ring *KeyRing, plaintext []byte) string {
	t.Helper()
	key, err := ring.Key(LabelGuardToken)
	require.NoError(t, err)
	id, err := ring.CurrentKeyID(context.Background())
	require.NoError(t, err)
	return sealRaw(t, key, id, aadFor(t, id), plaintext)
}

// decryptToken opens a token with the ring's key and the frozen AAD and returns the
// plaintext. It fails the test if the wire form is not what the design says.
func decryptToken(t *testing.T, ring *KeyRing, tok string) []byte {
	t.Helper()
	wire := wireBytes(t, tok)
	require.Greater(t, len(wire), wireKeyIDLen+wireNonceLen+wireTagLen)
	id, err := ring.CurrentKeyID(context.Background())
	require.NoError(t, err)
	require.Equal(t, idBytes(t, id), wire[:wireKeyIDLen], "the header must hold the key id, big-endian")
	key, err := ring.Key(LabelGuardToken)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := wire[wireKeyIDLen : wireKeyIDLen+wireNonceLen]
	pt, err := gcm.Open(nil, nonce, wire[wireKeyIDLen+wireNonceLen:], aadFor(t, id))
	require.NoError(t, err, "the token must open under the guard-token key and the frozen AAD")
	return pt
}

// plainToken is a hand-made guard-token plaintext. Each field can be set wrong.
type plainToken struct {
	version   byte
	user      uuid.UUID
	purpose   byte
	issuedMs  int64
	expiresMs int64
	count     uint16
	refs      []decide.CredRef
}

// validPlain is a well-formed plaintext issued at issued with the full five-minute life.
func validPlain(t *testing.T, issued time.Time, p TokenPurpose, refs []decide.CredRef) plainToken {
	t.Helper()
	return plainToken{
		version:   1,
		user:      guardUserID(),
		purpose:   byte(p),
		issuedMs:  issued.UnixMilli(),
		expiresMs: issued.Add(guardTTL).UnixMilli(),
		count:     u16(t, len(refs)),
		refs:      refs,
	}
}

func (p plainToken) encode(t *testing.T) []byte {
	t.Helper()
	buf := make([]byte, 0, plainHeaderLen+len(p.refs)*refSizeHint)
	buf = append(buf, p.version)
	buf = append(buf, p.user[:]...)
	buf = append(buf, p.purpose)
	var err error
	for _, v := range []any{p.issuedMs, p.expiresMs, p.count} {
		buf, err = binary.Append(buf, binary.BigEndian, v)
		require.NoError(t, err)
	}
	for _, ref := range p.refs {
		buf = append(buf, rawRef(t, byte(ref.Kind), ref.ID, ref.Version)...)
	}
	return buf
}

// rawRef encodes one ref as kind || uint16 ID length || ID || int64 version, all
// big-endian, without going through appendRef, so a hand-made plaintext can carry a kind
// the real encoder refuses.
func rawRef(t *testing.T, kind byte, id string, version int64) []byte {
	t.Helper()
	b := make([]byte, 0, 1+2+len(id)+8)
	b = append(b, kind)
	b = binary.BigEndian.AppendUint16(b, u16(t, len(id)))
	b = append(b, id...)
	b, err := binary.Append(b, binary.BigEndian, version)
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// Round trip and wire form
// ---------------------------------------------------------------------------

func TestGuardToken_PurposeValuesPinned(t *testing.T) {
	assert.Equal(t, TokenPurpose(1), PurposeCancel)
	assert.Equal(t, TokenPurpose(2), PurposeResolve)
	assert.NotEqual(t, TokenPurpose(0), PurposeCancel, "zero is not a purpose")
}

func TestGuardToken_RoundTrip(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0().Add(time.Minute)

	for name, purpose := range map[string]TokenPurpose{"cancel": PurposeCancel, "resolve": PurposeResolve} {
		t.Run(name+" keeps every claim and the order of the refs", func(t *testing.T) {
			in := guardClaims(purpose)
			got := mustOpenToken(t, ring, sealToken(t, ring, in), purpose, now)

			assert.Equal(t, guardUserID(), got.UserID)
			assert.Equal(t, purpose, got.Purpose)
			assert.Equal(t, guardRefs(), got.Proven, "refs keep the order given, including a negative version")
			assert.True(t, got.IssuedAt.Equal(guardT0()), "IssuedAt %v", got.IssuedAt)
			assert.True(t, got.ExpiresAt.Equal(guardT0().Add(guardTTL)), "ExpiresAt %v", got.ExpiresAt)
		})
	}

	t.Run("no refs", func(t *testing.T) {
		in := guardClaims(PurposeCancel)
		in.Proven = nil
		got := mustOpenToken(t, ring, sealToken(t, ring, in), PurposeCancel, now)
		assert.Empty(t, got.Proven)
		assert.Equal(t, guardUserID(), got.UserID)
	})

	t.Run("a ref whose ID is the largest the prefix can carry", func(t *testing.T) {
		in := guardClaims(PurposeResolve)
		in.Proven = []decide.CredRef{{Kind: decide.CredAddress, ID: strings.Repeat("x", maxEncodable), Version: math.MinInt64}}
		got := mustOpenToken(t, ring, sealToken(t, ring, in), PurposeResolve, now)
		assert.Equal(t, in.Proven, got.Proven)
	})

	t.Run("the largest count of refs", func(t *testing.T) {
		in := guardClaims(PurposeResolve)
		in.Proven = make([]decide.CredRef, maxEncodable)
		for i := range in.Proven {
			in.Proven[i] = decide.CredRef{Kind: decide.CredPassword, Version: int64(i)}
		}
		got := mustOpenToken(t, ring, sealToken(t, ring, in), PurposeResolve, now)
		assert.True(t, slices.Equal(in.Proven, got.Proven))
	})

	t.Run("sealing leaves the caller's claims alone", func(t *testing.T) {
		in := guardClaims(PurposeCancel)
		in.IssuedAt = guardT0().Add(123_900_000 * time.Nanosecond)
		in.ExpiresAt = guardT0().Add(time.Hour)
		before := guardClaims(PurposeCancel)
		before.IssuedAt, before.ExpiresAt = in.IssuedAt, in.ExpiresAt
		_ = sealToken(t, ring, in)
		assert.Equal(t, before, in)
	})
}

// The plaintext and the AAD are pinned by decrypting with an independent AES-GCM call.
func TestGuardToken_PlaintextEncodingPinned(t *testing.T) {
	ring, _ := newGuardRing(t)

	got := decryptToken(t, ring, sealToken(t, ring, guardClaims(PurposeCancel)))
	want := validPlain(t, guardT0(), PurposeCancel, guardRefs()).encode(t)

	assert.Equal(t, want, got)
	assert.Equal(t, byte(1), got[0], "version byte")
	user := guardUserID()
	assert.Equal(t, user[:], got[1:17], "user id")
	assert.Equal(t, byte(PurposeCancel), got[17], "purpose byte")
}

// A-16: the wire form tells an observer nothing about who or what it concerns.
func TestGuardToken_OpaqueWireForm(t *testing.T) {
	ring, _ := newGuardRing(t)
	in := guardClaims(PurposeResolve)
	tok := sealToken(t, ring, in)
	wire := wireBytes(t, tok)

	t.Run("holds none of the claim values", func(t *testing.T) {
		user := guardUserID()
		assert.False(t, bytes.Contains(wire, user[:]), "the user id bytes")
		assert.False(t, bytes.Contains(wire, []byte(user.String())), "the user id text")
		assert.False(t, bytes.Contains(wire, []byte(strings.ReplaceAll(user.String(), "-", ""))), "the user id hex")
		assert.NotContains(t, tok, user.String())
		for _, ref := range in.Proven {
			assert.False(t, bytes.Contains(wire, []byte(ref.ID)), "ref id %q", ref.ID)
			be, err := binary.Append(nil, binary.BigEndian, ref.Version)
			require.NoError(t, err)
			assert.False(t, bytes.Contains(wire, be), "version %d in big-endian", ref.Version)
		}
	})

	t.Run("has the frozen shape", func(t *testing.T) {
		id, err := ring.CurrentKeyID(context.Background())
		require.NoError(t, err)
		assert.Equal(t, idBytes(t, id), wire[:wireKeyIDLen])
		plainLen := plainHeaderLen
		for _, ref := range in.Proven {
			plainLen += 1 + 2 + len(ref.ID) + 8
		}
		assert.Len(t, wire, wireKeyIDLen+wireNonceLen+plainLen+wireTagLen, "no padding, no extra fields")
		assert.NotContains(t, tok, "=", "unpadded")
		assert.NotContains(t, tok, "+")
		assert.NotContains(t, tok, "/")
	})

	t.Run("two seals of equal claims differ in the nonce and everywhere after it", func(t *testing.T) {
		other := sealToken(t, ring, in)
		assert.NotEqual(t, tok, other)
		otherWire := wireBytes(t, other)
		assert.NotEqual(t, wire[wireKeyIDLen:wireKeyIDLen+wireNonceLen], otherWire[wireKeyIDLen:wireKeyIDLen+wireNonceLen], "a fresh GCM nonce")
		assert.NotEqual(t, wire[wireKeyIDLen+wireNonceLen:], otherWire[wireKeyIDLen+wireNonceLen:])
		// Both still open to the same claims.
		now := guardT0().Add(time.Second)
		assert.Equal(t, mustOpenToken(t, ring, tok, PurposeResolve, now), mustOpenToken(t, ring, other, PurposeResolve, now))
	})
}

// Positive control for every hand-made token below: the builder and the reseal helper
// produce something the real opener accepts, so a refusal elsewhere is about the
// mutation and not about the builder.
func TestGuardToken_HandMadeControlOpens(t *testing.T) {
	ring, _ := newGuardRing(t)
	plain := validPlain(t, guardT0(), PurposeCancel, guardRefs())

	got := mustOpenToken(t, ring, resealPlain(t, ring, plain.encode(t)), PurposeCancel, guardT0())

	assert.Equal(t, guardUserID(), got.UserID)
	assert.Equal(t, guardRefs(), got.Proven)
}

// ---------------------------------------------------------------------------
// Tampering and the strict decoder
// ---------------------------------------------------------------------------

func TestGuardToken_EveryTamperRefused(t *testing.T) {
	ring, _ := newGuardRing(t)
	tok := sealToken(t, ring, guardClaims(PurposeCancel))
	wire := wireBytes(t, tok)
	now := guardT0()
	mustOpenToken(t, ring, tok, PurposeCancel, now) // control

	t.Run("flipping any bit of any byte", func(t *testing.T) {
		for i := range wire {
			for _, mask := range []byte{0x01, 0x80, 0xff} {
				mutated := slices.Clone(wire)
				mutated[i] ^= mask
				mustRefuseToken(t, ring, encodeWire(mutated), PurposeCancel, now)
			}
		}
	})

	t.Run("truncated at every length", func(t *testing.T) {
		for n := range wire {
			mustRefuseToken(t, ring, encodeWire(wire[:n]), PurposeCancel, now)
		}
	})

	t.Run("one extra trailing byte", func(t *testing.T) {
		mustRefuseToken(t, ring, encodeWire(append(slices.Clone(wire), 0)), PurposeCancel, now)
	})

	t.Run("two tokens spliced", func(t *testing.T) {
		other := wireBytes(t, sealToken(t, ring, guardClaims(PurposeCancel)))
		spliced := append(slices.Clone(wire[:wireKeyIDLen+wireNonceLen]), other[wireKeyIDLen+wireNonceLen:]...)
		mustRefuseToken(t, ring, encodeWire(spliced), PurposeCancel, now)
	})

	t.Run("text that is not an unpadded base64url token", func(t *testing.T) {
		padded := ""
		for n := 1; n <= 3 && padded == ""; n++ {
			c := guardClaims(PurposeCancel)
			c.Proven = []decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("p", n), Version: 1}}
			w := wireBytes(t, sealToken(t, ring, c))
			if enc := base64.URLEncoding.EncodeToString(w); strings.HasSuffix(enc, "=") {
				padded = enc
			}
		}
		require.NotEmpty(t, padded, "need a wire length that pads")
		mustRefuseToken(t, ring, padded, PurposeCancel, now)

		for _, bad := range []string{"", " ", "!!!!", tok + "!", " " + tok, tok + "\n", strings.ReplaceAll(tok, tok[:1], "+")} {
			mustRefuseToken(t, ring, bad, PurposeCancel, now)
		}
	})
}

func TestGuardToken_StrictDecoder(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	base := validPlain(t, guardT0(), PurposeCancel, guardRefs())
	full := base.encode(t)

	t.Run("truncated at every length", func(t *testing.T) {
		for n := range full {
			mustRefuseToken(t, ring, resealPlain(t, ring, full[:n]), PurposeCancel, now)
		}
	})

	t.Run("trailing bytes", func(t *testing.T) {
		mustRefuseToken(t, ring, resealPlain(t, ring, append(slices.Clone(full), 0)), PurposeCancel, now)
		mustRefuseToken(t, ring, resealPlain(t, ring, append(slices.Clone(full), make([]byte, 40)...)), PurposeCancel, now)
	})

	t.Run("a count that disagrees with the refs present", func(t *testing.T) {
		fewer := base
		fewer.count = u16(t, len(base.refs)-1) // one ref left over
		mustRefuseToken(t, ring, resealPlain(t, ring, fewer.encode(t)), PurposeCancel, now)

		more := base
		more.count = u16(t, len(base.refs)+1) // one ref short
		mustRefuseToken(t, ring, resealPlain(t, ring, more.encode(t)), PurposeCancel, now)

		huge := validPlain(t, guardT0(), PurposeCancel, nil)
		huge.count = math.MaxUint16 // no refs follow; must not allocate its way to success
		mustRefuseToken(t, ring, resealPlain(t, ring, huge.encode(t)), PurposeCancel, now)
	})

	t.Run("a ref whose length prefix overruns the buffer", func(t *testing.T) {
		header := validPlain(t, guardT0(), PurposeCancel, nil)
		header.count = 1
		head := header.encode(t) // fixed fields, count 1, no ref bytes

		overrun := func(idLen uint16, idBytesPresent, tail int) []byte {
			b := slices.Clone(head)
			b = append(b, byte(decide.CredPassword))
			b = binary.BigEndian.AppendUint16(b, idLen)
			b = append(b, bytes.Repeat([]byte("a"), idBytesPresent)...)
			return append(b, make([]byte, tail)...)
		}
		// Control: a prefix that matches what follows is accepted.
		okTok := resealPlain(t, ring, overrun(5, 5, 8))
		got := mustOpenToken(t, ring, okTok, PurposeCancel, now)
		require.Len(t, got.Proven, 1)
		assert.Equal(t, "aaaaa", got.Proven[0].ID)

		mustRefuseToken(t, ring, resealPlain(t, ring, overrun(100, 5, 8)), PurposeCancel, now)
		mustRefuseToken(t, ring, resealPlain(t, ring, overrun(math.MaxUint16, 5, 8)), PurposeCancel, now)
		mustRefuseToken(t, ring, resealPlain(t, ring, overrun(6, 5, 8)), PurposeCancel, now)  // one byte past the ID
		mustRefuseToken(t, ring, resealPlain(t, ring, overrun(13, 5, 8)), PurposeCancel, now) // eats the version
		mustRefuseToken(t, ring, resealPlain(t, ring, overrun(5, 5, 7)), PurposeCancel, now)  // a short version
		mustRefuseToken(t, ring, resealPlain(t, ring, overrun(5, 5, 9)), PurposeCancel, now)  // a long version
	})

	t.Run("version byte other than 1", func(t *testing.T) {
		for _, v := range []byte{0, 2, 3, 0x80, 0xff} {
			p := base
			p.version = v
			mustRefuseToken(t, ring, resealPlain(t, ring, p.encode(t)), PurposeCancel, now)
		}
	})

	t.Run("a purpose byte outside the closed set", func(t *testing.T) {
		for _, b := range []byte{0, 3, 4, 0x80, 0xff} {
			p := base
			p.purpose = b
			plain := p.encode(t)
			mustRefuseToken(t, ring, resealPlain(t, ring, plain), PurposeCancel, now)
			mustRefuseToken(t, ring, resealPlain(t, ring, plain), PurposeResolve, now)
			mustRefuseToken(t, ring, resealPlain(t, ring, plain), TokenPurpose(b), now) // asking for it by name does not help
		}
	})
}

// ---------------------------------------------------------------------------
// Purpose, expiry and clock
// ---------------------------------------------------------------------------

func TestGuardToken_WrongPurposeExpiredRotatedRefused(t *testing.T) {
	ring, db := newGuardRing(t)
	now := guardT0().Add(time.Minute)

	t.Run("wrong purpose, both ways", func(t *testing.T) {
		cancelTok := sealToken(t, ring, guardClaims(PurposeCancel))
		resolveTok := sealToken(t, ring, guardClaims(PurposeResolve))
		mustOpenToken(t, ring, cancelTok, PurposeCancel, now)
		mustOpenToken(t, ring, resolveTok, PurposeResolve, now)
		mustRefuseToken(t, ring, cancelTok, PurposeResolve, now)
		mustRefuseToken(t, ring, resolveTok, PurposeCancel, now)
		for _, want := range []TokenPurpose{0, 3, 0xff} {
			mustRefuseToken(t, ring, cancelTok, want, now)
		}
	})

	t.Run("expired", func(t *testing.T) {
		tok := sealToken(t, ring, guardClaims(PurposeCancel))
		mustRefuseToken(t, ring, tok, PurposeCancel, guardT0().Add(guardTTL))
		mustRefuseToken(t, ring, tok, PurposeCancel, guardT0().Add(24*time.Hour))
	})

	t.Run("sealed under another secret", func(t *testing.T) {
		tok := sealToken(t, ring, guardClaims(PurposeCancel))
		rotated := NewKeyRing(db, ringInputBeta)
		idOld, err := ring.CurrentKeyID(context.Background())
		require.NoError(t, err)
		idNew, err := rotated.CurrentKeyID(context.Background())
		require.NoError(t, err)
		require.NotEqual(t, idOld, idNew, "the two secrets must have two registry ids")

		mustRefuseToken(t, rotated, tok, PurposeCancel, now)
		mustOpenToken(t, ring, tok, PurposeCancel, now) // control: the original secret still opens it

		// A rewritten header cannot move it to the new secret either: the key differs.
		forged := wireBytes(t, tok)
		copy(forged, idBytes(t, idNew))
		mustRefuseToken(t, rotated, encodeWire(forged), PurposeCancel, now)

		// Both rotated and original tokens: the new secret's own token opens on the new ring only.
		newTok := sealToken(t, rotated, guardClaims(PurposeCancel))
		mustOpenToken(t, rotated, newTok, PurposeCancel, now)
		mustRefuseToken(t, ring, newTok, PurposeCancel, now)
	})

	t.Run("two secrets that hold the same registry id", func(t *testing.T) {
		resetRegistry(t, db)
		first := NewKeyRing(db, ringInputAlpha)
		tok := sealToken(t, first, guardClaims(PurposeCancel))
		resetRegistry(t, db) // RESTART IDENTITY: the next secret registered gets the same id
		second := NewKeyRing(db, ringInputBeta)
		idFirst, err := first.CurrentKeyID(context.Background()) // cached
		require.NoError(t, err)
		idSecond, err := second.CurrentKeyID(context.Background())
		require.NoError(t, err)
		require.Equal(t, idFirst, idSecond)

		mustRefuseToken(t, second, tok, PurposeCancel, now) // the id matches; the AEAD must still refuse
	})
}

func TestGuardToken_ExpiryIsFiveMinutes(t *testing.T) {
	ring, _ := newGuardRing(t)

	t.Run("sealed expiry is issue time plus five minutes", func(t *testing.T) {
		got := mustOpenToken(t, ring, sealToken(t, ring, guardClaims(PurposeCancel)), PurposeCancel, guardT0())
		assert.Equal(t, 5*time.Minute, got.ExpiresAt.Sub(got.IssuedAt))
	})

	t.Run("a caller's ExpiresAt is ignored", func(t *testing.T) {
		for _, exp := range []time.Time{{}, guardT0().Add(-time.Hour), guardT0().Add(time.Minute), guardT0().Add(24 * time.Hour)} {
			in := guardClaims(PurposeCancel)
			in.ExpiresAt = exp
			got := mustOpenToken(t, ring, sealToken(t, ring, in), PurposeCancel, guardT0())
			assert.True(t, got.ExpiresAt.Equal(guardT0().Add(guardTTL)), "caller expiry %v produced %v", exp, got.ExpiresAt)
		}
	})

	t.Run("a decoded lifetime other than five minutes is refused", func(t *testing.T) {
		// Control first: the exact five minutes opens.
		mustOpenToken(t, ring, resealPlain(t, ring, validPlain(t, guardT0(), PurposeCancel, nil).encode(t)), PurposeCancel, guardT0())

		for _, delta := range []time.Duration{
			guardTTL + time.Millisecond, guardTTL - time.Millisecond, 10 * time.Minute, time.Hour, 24 * time.Hour,
			time.Second, time.Millisecond, 0, -time.Millisecond, -guardTTL,
		} {
			p := validPlain(t, guardT0(), PurposeCancel, guardRefs())
			p.expiresMs = guardT0().Add(delta).UnixMilli()
			mustRefuseToken(t, ring, resealPlain(t, ring, p.encode(t)), PurposeCancel, guardT0())
		}
	})
}

func TestGuardToken_TimeBoundaries(t *testing.T) {
	ring, _ := newGuardRing(t)
	issued := guardT0()
	tok := sealToken(t, ring, guardClaims(PurposeCancel))
	expires := issued.Add(guardTTL)

	accepted := map[string]time.Time{
		"at the issue instant":                  issued,
		"one millisecond before expiry":         expires.Add(-time.Millisecond),
		"exactly the skew before issue":         issued.Add(-guardSkew),
		"a minute into life":                    issued.Add(time.Minute),
		"the same instant in another zone":      issued.In(time.FixedZone("far-east", 9*3600)),
		"exactly the skew before, other zone":   issued.Add(-guardSkew).In(time.FixedZone("far-west", -8*3600)),
		"one millisecond before expiry, local":  expires.Add(-time.Millisecond).Local(),
		"one millisecond after the skew starts": issued.Add(-guardSkew + time.Millisecond),
	}
	for name, now := range accepted {
		t.Run("accepted "+name, func(t *testing.T) {
			mustOpenToken(t, ring, tok, PurposeCancel, now)
		})
	}

	refused := map[string]time.Time{
		"exactly at expiry":                         expires,
		"one millisecond after expiry":              expires.Add(time.Millisecond),
		"a day after expiry":                        expires.Add(24 * time.Hour),
		"one millisecond past the skew":             issued.Add(-guardSkew - time.Millisecond),
		"an hour before issue":                      issued.Add(-time.Hour),
		"the zero time":                             {},
		"exactly at expiry in another zone":         expires.In(time.FixedZone("far-east", 9*3600)),
		"one millisecond past the skew, other zone": issued.Add(-guardSkew - time.Millisecond).In(time.FixedZone("far-west", -8*3600)),
	}
	for name, now := range refused {
		t.Run("refused "+name, func(t *testing.T) {
			mustRefuseToken(t, ring, tok, PurposeCancel, now)
		})
	}
}

func TestGuardToken_SealTruncatesIssuedAtToMilliseconds(t *testing.T) {
	ring, _ := newGuardRing(t)
	in := guardClaims(PurposeCancel)
	in.IssuedAt = guardT0().Add(123_900_000 * time.Nanosecond) // 123.9 ms: truncation gives 123, rounding 124
	want := guardT0().Add(123 * time.Millisecond)

	got := mustOpenToken(t, ring, sealToken(t, ring, in), PurposeCancel, want)

	assert.True(t, got.IssuedAt.Equal(want), "IssuedAt %v, want %v", got.IssuedAt, want)
	assert.True(t, got.ExpiresAt.Equal(want.Add(guardTTL)), "ExpiresAt %v", got.ExpiresAt)

	// The stored instant is the truncated one, so the boundary sits there too.
	tok := sealToken(t, ring, in)
	mustRefuseToken(t, ring, tok, PurposeCancel, want.Add(guardTTL))
	mustOpenToken(t, ring, tok, PurposeCancel, want.Add(guardTTL-time.Millisecond))

	// Another zone for the same instant seals the same time.
	zoned := guardClaims(PurposeCancel)
	zoned.IssuedAt = guardT0().In(time.FixedZone("far-east", 9*3600))
	back := mustOpenToken(t, ring, sealToken(t, ring, zoned), PurposeCancel, guardT0())
	assert.True(t, back.IssuedAt.Equal(guardT0()))
}

func TestGuardToken_SealRefusesBadClaims(t *testing.T) {
	ring, _ := newGuardRing(t)

	tooMany := make([]decide.CredRef, maxEncodable+1)
	for i := range tooMany {
		tooMany[i] = decide.CredRef{Kind: decide.CredPassword, Version: int64(i)}
	}
	cases := map[string]func(c *GuardClaims){
		"zero IssuedAt":        func(c *GuardClaims) { c.IssuedAt = time.Time{} },
		"nil user":             func(c *GuardClaims) { c.UserID = uuid.Nil },
		"zero purpose":         func(c *GuardClaims) { c.Purpose = 0 },
		"purpose past the set": func(c *GuardClaims) { c.Purpose = 3 },
		"purpose 255":          func(c *GuardClaims) { c.Purpose = 0xff },
		"too many refs":        func(c *GuardClaims) { c.Proven = tooMany },
		"ref ID over the limit": func(c *GuardClaims) {
			c.Proven = []decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable+1)}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := guardClaims(PurposeCancel)
			mutate(&c)

			tok, err := SealGuardToken(context.Background(), ring, c)

			require.Error(t, err)
			assert.Empty(t, tok)
			assert.NotErrorIs(t, err, ErrTokenInvalid, "a bad claim is the caller's bug, not a client fault")
			assert.NotErrorIs(t, err, ErrKeyUnavailable, "and the ring is fine")
		})
	}

	t.Run("an over-limit ref and too many refs say why", func(t *testing.T) {
		c := guardClaims(PurposeCancel)
		c.Proven = []decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable+1)}}
		_, err := SealGuardToken(context.Background(), ring, c)
		assert.ErrorIs(t, err, ErrEncodingTooLong)

		c.Proven = tooMany
		_, err = SealGuardToken(context.Background(), ring, c)
		assert.ErrorIs(t, err, ErrEncodingTooLong)
	})
}

// ---------------------------------------------------------------------------
// Key and AAD binding
// ---------------------------------------------------------------------------

func TestGuardToken_KeyAndAADBinding(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	id, err := ring.CurrentKeyID(context.Background())
	require.NoError(t, err)
	plain := validPlain(t, guardT0(), PurposeCancel, guardRefs()).encode(t)
	tokenKey, err := ring.Key(LabelGuardToken)
	require.NoError(t, err)

	// Control: every input exactly as the design says.
	mustOpenToken(t, ring, sealRaw(t, tokenKey, id, aadFor(t, id), plain), PurposeCancel, now)

	t.Run("a key derived for another label", func(t *testing.T) {
		for _, l := range []Label{LabelChallenge, LabelFingerprint, LabelIdentifier, LabelAddress, LabelJob, LabelKeyCheck} {
			other, err := ring.Key(l)
			require.NoError(t, err)
			mustRefuseToken(t, ring, sealRaw(t, other, id, aadFor(t, id), plain), PurposeCancel, now)
		}
	})

	t.Run("the wrong AAD", func(t *testing.T) {
		reversed := slices.Clone(idBytes(t, id))
		slices.Reverse(reversed)
		aads := map[string][]byte{
			"none":                     nil,
			"label only":               []byte(tokenAADLabel),
			"key id only":              idBytes(t, id),
			"another label":            append([]byte("concord/accountguard/challenge/v1"), idBytes(t, id)...),
			"key id first":             append(idBytes(t, id), []byte(tokenAADLabel)...),
			"key id little-endian":     append([]byte(tokenAADLabel), reversed...),
			"another key id":           aadFor(t, id+1),
			"trailing byte":            append(aadFor(t, id), 0),
			"the label with a NUL end": append([]byte(tokenAADLabel+"\x00"), idBytes(t, id)...),
		}
		require.NotEqual(t, idBytes(t, id), reversed, "need an id whose byte order is visible")
		for name, aad := range aads {
			t.Run(name, func(t *testing.T) {
				mustRefuseToken(t, ring, sealRaw(t, tokenKey, id, aad, plain), PurposeCancel, now)
			})
		}
	})

	t.Run("a header id other than the current one, sealed consistently", func(t *testing.T) {
		for _, other := range []int16{id + 1, id - 1, 0, -1, math.MaxInt16} {
			mustRefuseToken(t, ring, sealRaw(t, tokenKey, other, aadFor(t, other), plain), PurposeCancel, now)
		}
	})

	t.Run("a header id that disagrees with the AAD", func(t *testing.T) {
		mustRefuseToken(t, ring, sealRaw(t, tokenKey, id+1, aadFor(t, id), plain), PurposeCancel, now)
		mustRefuseToken(t, ring, sealRaw(t, tokenKey, id, aadFor(t, id+1), plain), PurposeCancel, now)
	})
}

// ---------------------------------------------------------------------------
// Server fault versus client fault, for all four functions
// ---------------------------------------------------------------------------

type downCase struct {
	name string
	ring *KeyRing
	ctx  context.Context
}

// downCases returns the ways a ring can be unable to name its key. The ring is
// resolved first, so each must report the server fault.
func downCases(t *testing.T, db *sql.DB) []downCase {
	t.Helper()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	dead := extraPool(t)
	require.NoError(t, dead.Close())
	return []downCase{
		{"nil ring", nil, context.Background()},
		{"zero ring", &KeyRing{}, context.Background()},
		{"empty secret", NewKeyRing(db, ""), context.Background()},
		{"closed pool", NewKeyRing(dead, ringInputAlpha), context.Background()},
		{"cancelled context", NewKeyRing(db, ringInputAlpha), cancelled},
	}
}

func assertServerFault(t *testing.T, err error, leaks ...string) {
	t.Helper()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyUnavailable)
	assert.NotErrorIs(t, err, ErrTokenInvalid, "a ring outage must not read as a client fault")
	assert.NotErrorIs(t, err, ErrChallengeInvalid, "a ring outage must not read as a client fault")
	assert.NotContains(t, err.Error(), ringInputAlpha, "the ring's secret")
	for _, leak := range leaks {
		assert.NotContains(t, err.Error(), leak)
	}
}

func TestGuardTokenAndChallenge_RingDownIsKeyUnavailable(t *testing.T) {
	healthy, db := newGuardRing(t)
	tok := sealToken(t, healthy, guardClaims(PurposeCancel))
	chal, err := MintChallenge(context.Background(), healthy, ChallengeVerify, guardT0())
	require.NoError(t, err)
	now := guardT0()

	for _, dc := range downCases(t, db) {
		t.Run(dc.name, func(t *testing.T) {
			t.Run("SealGuardToken", func(t *testing.T) {
				got, err := SealGuardToken(dc.ctx, dc.ring, guardClaims(PurposeCancel))
				assertServerFault(t, err)
				assert.Empty(t, got)
			})
			t.Run("OpenGuardToken on a good token", func(t *testing.T) {
				got, err := OpenGuardToken(dc.ctx, dc.ring, tok, PurposeCancel, now)
				assertServerFault(t, err, tok)
				assert.Equal(t, GuardClaims{}, got)
			})
			for name, garbage := range map[string]string{"empty": "", "not base64": "!!! not a token !!!", "too short": "AAAA"} {
				t.Run("OpenGuardToken on a "+name+" token", func(t *testing.T) {
					got, err := OpenGuardToken(dc.ctx, dc.ring, garbage, PurposeCancel, now)
					assertServerFault(t, err)
					assert.Equal(t, GuardClaims{}, got)
				})
			}
			t.Run("MintChallenge", func(t *testing.T) {
				got, err := MintChallenge(dc.ctx, dc.ring, ChallengeVerify, now)
				assertServerFault(t, err)
				assert.Empty(t, got)
			})
			t.Run("VerifyChallenge on a good challenge", func(t *testing.T) {
				got, err := VerifyChallenge(dc.ctx, dc.ring, chal, ChallengeVerify, now)
				assertServerFault(t, err, chal)
				assert.Equal(t, [32]byte{}, got)
			})
			for name, garbage := range map[string]string{"empty": "", "not base64": "!!! not a challenge !!!", "too short": "AAAA"} {
				t.Run("VerifyChallenge on a "+name+" challenge", func(t *testing.T) {
					got, err := VerifyChallenge(dc.ctx, dc.ring, garbage, ChallengeVerify, now)
					assertServerFault(t, err)
					assert.Equal(t, [32]byte{}, got)
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Canonical spelling, nil user, and ring-first ordering
// ---------------------------------------------------------------------------

// b64URLAlphabet is the base64url alphabet in index order.
const b64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" // pragma: allowlist secret

// respellTrailingBits returns every other spelling of s that a lenient base64 decoder
// reads as the same bytes: the final character carries spare bits, which a canonical
// encoder leaves zero, and each non-zero value of them is a distinct string for the same
// decoded bytes. It fails the test if s has no spare bits (its byte length is a
// multiple of three), because then there is nothing to re-spell.
func respellTrailingBits(t *testing.T, s string) []string {
	t.Helper()
	spare := 0
	switch len(s) % 4 {
	case 2:
		spare = 4
	case 3:
		spare = 2
	}
	require.NotZero(t, spare, "need a wire length with spare trailing bits")
	last := strings.IndexByte(b64URLAlphabet, s[len(s)-1])
	require.GreaterOrEqual(t, last, 0)
	mask := 1<<spare - 1
	require.Zero(t, last&mask, "the canonical spelling leaves the spare bits zero")

	canonical, err := base64.RawURLEncoding.DecodeString(s)
	require.NoError(t, err)
	var out []string
	for v := 1; v <= mask; v++ {
		alt := s[:len(s)-1] + string(b64URLAlphabet[last|v])
		got, err := base64.RawURLEncoding.DecodeString(alt)
		require.NoError(t, err, "a lenient decoder must accept the re-spelling")
		require.Equal(t, canonical, got, "the re-spelling must decode to the same bytes")
		require.NotEqual(t, s, alt)
		out = append(out, alt)
	}
	return out
}

func TestGuardToken_NonCanonicalBase64Refused(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	tok := sealToken(t, ring, guardClaims(PurposeCancel))
	mustOpenToken(t, ring, tok, PurposeCancel, now) // control: the canonical spelling opens

	alts := respellTrailingBits(t, tok)
	require.NotEmpty(t, alts)
	for _, alt := range alts {
		mustRefuseToken(t, ring, alt, PurposeCancel, now) // same decoded bytes, a second spelling of one token
	}
}

func TestGuardToken_NilUserInPlaintextRefused(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()

	// Control: the same plaintext with a real user opens.
	good := validPlain(t, guardT0(), PurposeCancel, guardRefs())
	mustOpenToken(t, ring, resealPlain(t, ring, good.encode(t)), PurposeCancel, now)

	nilUser := good
	nilUser.user = uuid.Nil
	mustRefuseToken(t, ring, resealPlain(t, ring, nilUser.encode(t)), PurposeCancel, now)

	noRefs := validPlain(t, guardT0(), PurposeResolve, nil)
	noRefs.user = uuid.Nil
	mustRefuseToken(t, ring, resealPlain(t, ring, noRefs.encode(t)), PurposeResolve, now)
}

// The ring is resolved first, so a down ring is reported even when the input is also bad.
func TestGuardTokenAndChallenge_RingDownWinsOverBadInput(t *testing.T) {
	_, db := newGuardRing(t)

	tooMany := make([]decide.CredRef, maxEncodable+1)
	badClaims := map[string]func(c *GuardClaims){
		"zero IssuedAt":   func(c *GuardClaims) { c.IssuedAt = time.Time{} },
		"nil user":        func(c *GuardClaims) { c.UserID = uuid.Nil },
		"unknown purpose": func(c *GuardClaims) { c.Purpose = 0 },
		"too many refs":   func(c *GuardClaims) { c.Proven = tooMany },
		"ref ID too long": func(c *GuardClaims) {
			c.Proven = []decide.CredRef{{Kind: decide.CredPassword, ID: strings.Repeat("x", maxEncodable+1)}}
		},
		"everything wrong": func(c *GuardClaims) {
			*c = GuardClaims{Purpose: 0xff, Proven: tooMany}
		},
	}

	for _, dc := range downCases(t, db) {
		t.Run(dc.name, func(t *testing.T) {
			for name, mutate := range badClaims {
				t.Run("SealGuardToken with "+name, func(t *testing.T) {
					c := guardClaims(PurposeCancel)
					mutate(&c)
					got, err := SealGuardToken(dc.ctx, dc.ring, c)
					assertServerFault(t, err)
					assert.NotErrorIs(t, err, ErrEncodingTooLong, "the ring fault, not the input fault")
					assert.Empty(t, got)
				})
			}
			t.Run("MintChallenge with a zero time", func(t *testing.T) {
				got, err := MintChallenge(dc.ctx, dc.ring, ChallengeVerify, time.Time{})
				assertServerFault(t, err)
				assert.Empty(t, got)
			})
			for _, p := range []ChallengePurpose{0, 7, 0xff} {
				t.Run("MintChallenge with an unknown purpose", func(t *testing.T) {
					got, err := MintChallenge(dc.ctx, dc.ring, p, guardT0())
					assertServerFault(t, err)
					assert.Empty(t, got)
				})
			}
		})
	}
}

// A time whose Unix milliseconds do not fit an int64 cannot be sealed, and neither can one
// whose five-minute expiry does not. It is the caller's bug, not a client fault.
func TestGuardToken_SealRefusesUnrepresentableTime(t *testing.T) {
	ring, _ := newGuardRing(t)
	times := map[string]time.Time{
		"seconds far in the future":     time.Unix(1<<62, 0),
		"seconds far in the past":       time.Unix(-(1 << 62), 0),
		"expiry overflows the int64 ms": time.UnixMilli(math.MaxInt64 - 1000),
	}
	for name, at := range times {
		t.Run(name, func(t *testing.T) {
			c := guardClaims(PurposeCancel)
			c.IssuedAt = at

			tok, err := SealGuardToken(context.Background(), ring, c)

			require.Error(t, err)
			assert.Empty(t, tok)
			assert.NotErrorIs(t, err, ErrTokenInvalid)
			assert.NotErrorIs(t, err, ErrKeyUnavailable)
		})
	}
}

// ---------------------------------------------------------------------------
// Floor semantics before 1970, and unknown credential kinds
// ---------------------------------------------------------------------------

// Milliseconds before the epoch floor (toward the earlier instant), as UnixMilli does.
// A truncation toward zero would read 23:59:59.999 as 00:00:00.000 and shift the token.
func TestGuardToken_PreEpochMillisecondsFloor(t *testing.T) {
	ring, _ := newGuardRing(t)
	in := guardClaims(PurposeCancel)
	in.IssuedAt = time.Unix(-1, 999_500_000) // 1969-12-31T23:59:59.9995Z
	want := time.Date(1969, 12, 31, 23, 59, 59, 999_000_000, time.UTC)

	got := mustOpenToken(t, ring, sealToken(t, ring, in), PurposeCancel, want)

	assert.True(t, got.IssuedAt.Equal(want), "IssuedAt %v, want %v", got.IssuedAt, want)
	assert.True(t, got.ExpiresAt.Equal(want.Add(guardTTL)), "ExpiresAt %v", got.ExpiresAt)
}

// ErrEncodingTooLong is shared by the fingerprint and the token encodings now.
func TestErrEncodingTooLong_TextIsShared(t *testing.T) {
	assert.Equal(t, "accountguard: encoding too long", ErrEncodingTooLong.Error())
}

// The drift guard: decide's own spelling table says which kinds exist, and validCredKind
// must say the same for every possible byte.
func TestValidCredKind_MatchesDecideSpellings(t *testing.T) {
	for i := 0; i <= 255; i++ {
		kind := decide.CredKind(i)
		_, err := kind.MarshalText()
		assert.Equal(t, err == nil, validCredKind(kind), "kind %d", i)
	}
	assert.False(t, validCredKind(decide.CredUnknown))
	assert.True(t, validCredKind(decide.CredPassword))
	assert.True(t, validCredKind(decide.CredRecoveryDevice))
	assert.False(t, validCredKind(decide.CredRecoveryDevice+1))
}

// badKindRefs returns refs the encoders must refuse: kind 0, the kind after the last, and
// the largest byte.
func badKindRefs() map[string]decide.CredRef {
	return map[string]decide.CredRef{
		"kind 0":   {Kind: decide.CredUnknown, ID: "a", Version: 1},
		"kind 11":  {Kind: decide.CredRecoveryDevice + 1, ID: "a", Version: 1},
		"kind 255": {Kind: decide.CredKind(255), ID: "a", Version: 1},
	}
}

func TestUnknownCredKind_RefusedByEveryEncoder(t *testing.T) {
	ring, _ := newGuardRing(t)
	good := decide.CredRef{Kind: decide.CredPassword, ID: "p", Version: 1}

	for name, bad := range badKindRefs() {
		for position, refs := range map[string][]decide.CredRef{
			"alone":  {bad},
			"first":  {bad, good},
			"middle": {good, bad, good},
			"last":   {good, bad},
		} {
			t.Run(name+" "+position, func(t *testing.T) {
				t.Run("OriginFingerprint", func(t *testing.T) {
					fp, err := OriginFingerprint(refs)
					require.Error(t, err)
					assert.ErrorIs(t, err, ErrUnknownCredKind)
					assert.NotErrorIs(t, err, ErrEncodingTooLong)
					assert.Equal(t, [32]byte{}, fp)
				})
				t.Run("NewCandidate", func(t *testing.T) {
					c, err := NewCandidate(context.Background(), ring, decide.ReqCredentialRemove, refs, false)
					require.Error(t, err)
					assert.ErrorIs(t, err, ErrUnknownCredKind)
					assertZeroCandidate(t, c)
				})
				t.Run("SealGuardToken", func(t *testing.T) {
					c := guardClaims(PurposeCancel)
					c.Proven = refs
					tok, err := SealGuardToken(context.Background(), ring, c)
					require.Error(t, err)
					assert.ErrorIs(t, err, ErrUnknownCredKind)
					assert.NotErrorIs(t, err, ErrTokenInvalid, "the caller's bug, not a client fault")
					assert.Empty(t, tok)
				})
			})
		}
	}

	t.Run("control: every known kind encodes", func(t *testing.T) {
		for k := decide.CredPassword; k <= decide.CredRecoveryDevice; k++ {
			ref := decide.CredRef{Kind: k, ID: "x", Version: 1}
			_, err := OriginFingerprint([]decide.CredRef{ref})
			require.NoError(t, err, "kind %d", k)
			c := guardClaims(PurposeCancel)
			c.Proven = []decide.CredRef{ref}
			_, err = SealGuardToken(context.Background(), ring, c)
			require.NoError(t, err, "kind %d", k)
		}
	})
}

// A ref of an unknown kind inside an authenticated plaintext is not ours: the opener
// refuses it with the single client error.
func TestGuardToken_UnknownCredKindInPlaintextRefused(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	good := decide.CredRef{Kind: decide.CredPassword, ID: "p", Version: 1}

	// Control: every known kind opens, so a refusal below is about the kind alone.
	for k := decide.CredPassword; k <= decide.CredRecoveryDevice; k++ {
		p := validPlain(t, guardT0(), PurposeCancel, []decide.CredRef{{Kind: k, ID: "x", Version: 1}})
		got := mustOpenToken(t, ring, resealPlain(t, ring, p.encode(t)), PurposeCancel, now)
		require.Len(t, got.Proven, 1)
		assert.Equal(t, k, got.Proven[0].Kind)
	}

	for name, bad := range badKindRefs() {
		for position, refs := range map[string][]decide.CredRef{
			"alone": {bad},
			"first": {bad, good},
			"last":  {good, bad},
		} {
			t.Run(name+" "+position, func(t *testing.T) {
				p := validPlain(t, guardT0(), PurposeCancel, refs)
				mustRefuseToken(t, ring, resealPlain(t, ring, p.encode(t)), PurposeCancel, now)
			})
		}
	}
}
