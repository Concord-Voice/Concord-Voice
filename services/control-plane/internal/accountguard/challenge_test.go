package accountguard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the challenge's wire form, its purpose binding, its skew window and its
// failure split. The expected MACs are recomputed here with crypto/hmac over the ring's
// challenge-label key, so the wire layout and the key label are checked against an
// independent computation rather than against the code under test.

const (
	// The fixed layout of the wire: key id, nonce, issue time (ms), purpose, MAC.
	chalKeyIDLen   = 2
	chalNonceLen   = 16
	chalTimeLen    = 8
	chalPurposeLen = 1
	chalMACLen     = 32
	chalBodyLen    = chalKeyIDLen + chalNonceLen + chalTimeLen + chalPurposeLen
	chalWireLen    = chalBodyLen + chalMACLen
)

func allChallengePurposes() map[string]ChallengePurpose {
	return map[string]ChallengePurpose{
		"verify":              ChallengeVerify,
		"cancel":              ChallengeCancel,
		"resolve":             ChallengeResolve,
		"contest":             ChallengeContest,
		"sign-out-everywhere": ChallengeSignOutEverywhere,
		"break-glass":         ChallengeBreakGlass,
	}
}

func mintChallenge(t *testing.T, ring *KeyRing, p ChallengePurpose, now time.Time) string {
	t.Helper()
	s, err := MintChallenge(context.Background(), ring, p, now)
	require.NoError(t, err)
	require.NotEmpty(t, s)
	return s
}

func mustVerifyChallenge(t *testing.T, ring *KeyRing, s string, want ChallengePurpose, now time.Time) [32]byte {
	t.Helper()
	h, err := VerifyChallenge(context.Background(), ring, s, want, now)
	require.NoError(t, err)
	return h
}

// mustRefuseChallenge requires the single opaque client error and a zero nonce hash.
func mustRefuseChallenge(t *testing.T, ring *KeyRing, s string, want ChallengePurpose, now time.Time) {
	t.Helper()
	h, err := VerifyChallenge(context.Background(), ring, s, want, now)
	assertBare(t, err, ErrChallengeInvalid)
	assert.Equal(t, [32]byte{}, h, "a refused challenge must yield the zero nonce hash")
	assertNoEcho(t, err, s, ErrChallengeInvalid)
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data) // hash.Hash.Write never returns an error
	return m.Sum(nil)
}

// chalBody is keyID || nonce || issued (unix ms, int64 BE) || purpose.
func chalBody(t *testing.T, id int16, nonce []byte, issuedMs int64, purpose byte) []byte {
	t.Helper()
	b := append(idBytes(t, id), nonce...)
	b, err := binary.Append(b, binary.BigEndian, issuedMs)
	require.NoError(t, err)
	return append(b, purpose)
}

// rawChallenge builds a wire form whose MAC is HMAC-SHA256(macKey, body).
func rawChallenge(macKey []byte, body []byte) string {
	return encodeWire(append(slices.Clone(body), hmacSHA256(macKey, body)...))
}

// currentChallengeParts returns what a hand-made challenge needs from the ring.
func currentChallengeParts(t *testing.T, ring *KeyRing) (key []byte, id int16) {
	t.Helper()
	key, err := ring.Key(LabelChallenge)
	require.NoError(t, err)
	id, err = ring.CurrentKeyID(context.Background())
	require.NoError(t, err)
	return key, id
}

// chalIssuedMs reads the issue time (unix ms) out of a decoded challenge.
func chalIssuedMs(t *testing.T, wire []byte) int64 {
	t.Helper()
	var ms int64
	_, err := binary.Decode(wire[chalKeyIDLen+chalNonceLen:chalBodyLen-chalPurposeLen], binary.BigEndian, &ms)
	require.NoError(t, err)
	return ms
}

func fixedNonce() []byte { return []byte("0123456789abcdef") }

func TestChallenge_PurposeValuesPinned(t *testing.T) {
	assert.Equal(t, ChallengePurpose(1), ChallengeVerify)
	assert.Equal(t, ChallengePurpose(2), ChallengeCancel)
	assert.Equal(t, ChallengePurpose(3), ChallengeResolve)
	assert.Equal(t, ChallengePurpose(4), ChallengeContest)
	assert.Equal(t, ChallengePurpose(5), ChallengeSignOutEverywhere)
	assert.Equal(t, ChallengePurpose(6), ChallengeBreakGlass)
}

func TestChallenge_WireLayout(t *testing.T) {
	ring, _ := newGuardRing(t)
	issued := guardT0()
	s := mintChallenge(t, ring, ChallengeContest, issued)
	wire := wireBytes(t, s)

	require.Len(t, wire, chalWireLen, "keyID(2) + nonce(16) + time(8) + purpose(1) + MAC(32)")
	assert.NotContains(t, s, "=")

	key, id := currentChallengeParts(t, ring)
	assert.Equal(t, idBytes(t, id), wire[:chalKeyIDLen], "key id, big-endian")
	nonce := wire[chalKeyIDLen : chalKeyIDLen+chalNonceLen]
	assert.NotEqual(t, make([]byte, chalNonceLen), nonce, "the nonce is random, not zero")
	assert.Equal(t, issued.UnixMilli(), chalIssuedMs(t, wire))
	assert.Equal(t, byte(ChallengeContest), wire[chalBodyLen-chalPurposeLen])
	assert.Equal(t, hmacSHA256(key, wire[:chalBodyLen]), wire[chalBodyLen:], "MAC is HMAC-SHA256 under the challenge-label key over every preceding byte")
}

func TestChallenge_RoundTripReturnsNonceHash(t *testing.T) {
	ring, _ := newGuardRing(t)

	for name, p := range allChallengePurposes() {
		t.Run(name, func(t *testing.T) {
			now := guardT0()
			s := mintChallenge(t, ring, p, now)
			wire := wireBytes(t, s)

			got := mustVerifyChallenge(t, ring, s, p, now)

			assert.Equal(t, sha256.Sum256(wire[chalKeyIDLen:chalKeyIDLen+chalNonceLen]), got, "SHA-256 of the 16-byte nonce")
			assert.NotEqual(t, [32]byte{}, got)
		})
	}

	t.Run("verifying twice succeeds both times: PR3 spends nothing", func(t *testing.T) {
		s := mintChallenge(t, ring, ChallengeVerify, guardT0())
		first := mustVerifyChallenge(t, ring, s, ChallengeVerify, guardT0())
		second := mustVerifyChallenge(t, ring, s, ChallengeVerify, guardT0())
		assert.Equal(t, first, second)
	})
}

func TestChallenge_FreshNonceEachMint(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()

	a := mintChallenge(t, ring, ChallengeVerify, now)
	b := mintChallenge(t, ring, ChallengeVerify, now)

	assert.NotEqual(t, a, b)
	wa, wb := wireBytes(t, a), wireBytes(t, b)
	assert.NotEqual(t, wa[chalKeyIDLen:chalKeyIDLen+chalNonceLen], wb[chalKeyIDLen:chalKeyIDLen+chalNonceLen])
	assert.NotEqual(t, wa[chalBodyLen:], wb[chalBodyLen:], "different bodies, different MACs")
	assert.NotEqual(t,
		mustVerifyChallenge(t, ring, a, ChallengeVerify, now),
		mustVerifyChallenge(t, ring, b, ChallengeVerify, now),
		"each mint spends its own nonce hash")
}

func TestChallenge_MintTruncatesToMilliseconds(t *testing.T) {
	ring, _ := newGuardRing(t)
	issued := guardT0().Add(123_900_000 * time.Nanosecond) // 123.9 ms: truncation gives 123, rounding 124
	wire := wireBytes(t, mintChallenge(t, ring, ChallengeVerify, issued))

	got := chalIssuedMs(t, wire)

	assert.Equal(t, guardT0().Add(123*time.Millisecond).UnixMilli(), got)
}

func TestChallenge_SkewBoundary(t *testing.T) {
	ring, _ := newGuardRing(t)
	issued := guardT0()
	s := mintChallenge(t, ring, ChallengeVerify, issued)

	accepted := map[string]time.Duration{
		"the issue instant":            0,
		"exactly the skew after":       guardSkew,
		"exactly the skew before":      -guardSkew,
		"a millisecond inside, after":  guardSkew - time.Millisecond,
		"a millisecond inside, before": -guardSkew + time.Millisecond,
	}
	for name, off := range accepted {
		t.Run("accepted "+name, func(t *testing.T) {
			mustVerifyChallenge(t, ring, s, ChallengeVerify, issued.Add(off))
		})
	}

	refused := map[string]time.Duration{
		"a millisecond past the skew, after":  guardSkew + time.Millisecond,
		"a millisecond past the skew, before": -guardSkew - time.Millisecond,
		"an hour after":                       time.Hour,
		"an hour before":                      -time.Hour,
		"a day after":                         24 * time.Hour,
	}
	for name, off := range refused {
		t.Run("refused "+name, func(t *testing.T) {
			mustRefuseChallenge(t, ring, s, ChallengeVerify, issued.Add(off))
		})
	}

	t.Run("the zero time is refused", func(t *testing.T) {
		mustRefuseChallenge(t, ring, s, ChallengeVerify, time.Time{})
	})

	t.Run("another zone for the same instant", func(t *testing.T) {
		zone := time.FixedZone("far-east", 9*3600)
		mustVerifyChallenge(t, ring, s, ChallengeVerify, issued.Add(guardSkew).In(zone))
		mustRefuseChallenge(t, ring, s, ChallengeVerify, issued.Add(guardSkew+time.Millisecond).In(zone))
	})
}

func TestChallenge_PurposeBound(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()

	for mintName, minted := range allChallengePurposes() {
		s := mintChallenge(t, ring, minted, now)
		for wantName, want := range allChallengePurposes() {
			t.Run("minted "+mintName+" verified as "+wantName, func(t *testing.T) {
				if minted == want {
					mustVerifyChallenge(t, ring, s, want, now)
					return
				}
				mustRefuseChallenge(t, ring, s, want, now)
			})
		}
		t.Run("minted "+mintName+" verified as a purpose outside the set", func(t *testing.T) {
			for _, want := range []ChallengePurpose{0, 7, 0x80, 0xff} {
				mustRefuseChallenge(t, ring, s, want, now)
			}
		})
	}
}

func TestChallenge_MintRefusesUnknownPurpose(t *testing.T) {
	ring, _ := newGuardRing(t)
	for _, p := range []ChallengePurpose{0, 7, 0x80, 0xff} {
		s, err := MintChallenge(context.Background(), ring, p, guardT0())

		require.Error(t, err)
		assert.Empty(t, s)
		assert.NotErrorIs(t, err, ErrChallengeInvalid, "a bad purpose is the caller's bug, not a client fault")
		assert.NotErrorIs(t, err, ErrKeyUnavailable, "and the ring is fine")
	}
}

func TestChallenge_TamperRefused(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	s := mintChallenge(t, ring, ChallengeResolve, now)
	wire := wireBytes(t, s)
	mustVerifyChallenge(t, ring, s, ChallengeResolve, now) // control

	t.Run("flipping any bit of any byte", func(t *testing.T) {
		for i := range wire {
			for _, mask := range []byte{0x01, 0x80, 0xff} {
				mutated := slices.Clone(wire)
				mutated[i] ^= mask
				mustRefuseChallenge(t, ring, encodeWire(mutated), ChallengeResolve, now)
			}
		}
	})

	t.Run("truncated at every length", func(t *testing.T) {
		for n := range wire {
			mustRefuseChallenge(t, ring, encodeWire(wire[:n]), ChallengeResolve, now)
		}
	})

	t.Run("extra trailing bytes", func(t *testing.T) {
		mustRefuseChallenge(t, ring, encodeWire(append(slices.Clone(wire), 0)), ChallengeResolve, now)
		mustRefuseChallenge(t, ring, encodeWire(append(slices.Clone(wire), make([]byte, 32)...)), ChallengeResolve, now)
	})

	t.Run("two challenges spliced", func(t *testing.T) {
		other := wireBytes(t, mintChallenge(t, ring, ChallengeResolve, now))
		spliced := append(slices.Clone(wire[:chalBodyLen]), other[chalBodyLen:]...)
		mustRefuseChallenge(t, ring, encodeWire(spliced), ChallengeResolve, now)
		spliced = append(slices.Clone(other[:chalBodyLen]), wire[chalBodyLen:]...)
		mustRefuseChallenge(t, ring, encodeWire(spliced), ChallengeResolve, now)
	})

	t.Run("text that is not an unpadded base64url challenge", func(t *testing.T) {
		padded := base64.URLEncoding.EncodeToString(wire)
		require.Contains(t, padded, "=", "a 59-byte wire pads")
		for _, bad := range []string{"", " ", "!!!!", padded, s + "!", " " + s, s + "\n"} {
			mustRefuseChallenge(t, ring, bad, ChallengeResolve, now)
		}
	})
}

// Hand-made challenges. The control proves the builder matches the format, so each
// refusal below is about the one thing changed.
func TestChallenge_MACBinding(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	macKey, id := currentChallengeParts(t, ring)
	nonce := fixedNonce()
	body := chalBody(t, id, nonce, now.UnixMilli(), byte(ChallengeVerify))

	t.Run("control: a hand-made challenge verifies and returns the nonce hash", func(t *testing.T) {
		got := mustVerifyChallenge(t, ring, rawChallenge(macKey, body), ChallengeVerify, now)
		assert.Equal(t, sha256.Sum256(nonce), got)
	})

	t.Run("a MAC under a key derived for another label", func(t *testing.T) {
		for _, l := range []Label{LabelGuardToken, LabelFingerprint, LabelIdentifier, LabelAddress, LabelJob, LabelKeyCheck} {
			other, err := ring.Key(l)
			require.NoError(t, err)
			mustRefuseChallenge(t, ring, rawChallenge(other, body), ChallengeVerify, now)
		}
	})

	t.Run("a MAC that leaves a field out", func(t *testing.T) {
		withoutKeyID := hmacSHA256(macKey, body[chalKeyIDLen:])
		withoutPurpose := hmacSHA256(macKey, body[:chalBodyLen-chalPurposeLen])
		withoutTime := hmacSHA256(macKey, slices.Concat(body[:chalKeyIDLen+chalNonceLen], body[chalBodyLen-chalPurposeLen:]))
		for _, mac := range [][]byte{withoutKeyID, withoutPurpose, withoutTime} {
			mustRefuseChallenge(t, ring, encodeWire(append(slices.Clone(body), mac...)), ChallengeVerify, now)
		}
	})

	t.Run("a MAC cut short or padded out", func(t *testing.T) {
		mac := hmacSHA256(macKey, body)
		mustRefuseChallenge(t, ring, encodeWire(append(slices.Clone(body), mac[:31]...)), ChallengeVerify, now)
		mustRefuseChallenge(t, ring, encodeWire(append(slices.Clone(body), append(slices.Clone(mac), 0)...)), ChallengeVerify, now)
	})

	t.Run("a nonce of another length, MAC valid over the new body", func(t *testing.T) {
		for _, n := range []int{0, 15, 17, 32} {
			odd := chalBody(t, id, make([]byte, n), now.UnixMilli(), byte(ChallengeVerify))
			mustRefuseChallenge(t, ring, rawChallenge(macKey, odd), ChallengeVerify, now)
		}
	})

	t.Run("a purpose byte outside the set, MAC valid", func(t *testing.T) {
		for _, b := range []byte{0, 7, 0x80, 0xff} {
			odd := chalBody(t, id, nonce, now.UnixMilli(), b)
			s := rawChallenge(macKey, odd)
			mustRefuseChallenge(t, ring, s, ChallengeVerify, now)
			mustRefuseChallenge(t, ring, s, ChallengeBreakGlass, now)
			mustRefuseChallenge(t, ring, s, ChallengePurpose(b), now) // asking for it by name does not help
		}
	})

	t.Run("a key id other than the current one, MAC valid", func(t *testing.T) {
		for _, other := range []int16{id + 1, id - 1, 0, -1} {
			odd := chalBody(t, other, nonce, now.UnixMilli(), byte(ChallengeVerify))
			mustRefuseChallenge(t, ring, rawChallenge(macKey, odd), ChallengeVerify, now)
		}
	})

	t.Run("a time the MAC covers is the time the skew reads", func(t *testing.T) {
		late := chalBody(t, id, nonce, now.Add(2*guardSkew).UnixMilli(), byte(ChallengeVerify))
		s := rawChallenge(macKey, late)
		mustRefuseChallenge(t, ring, s, ChallengeVerify, now)
		mustVerifyChallenge(t, ring, s, ChallengeVerify, now.Add(2*guardSkew)) // control: it is the clock, not the encoding
	})
}

func TestChallenge_RotatedKeyRefused(t *testing.T) {
	ring, db := newGuardRing(t)
	now := guardT0()

	t.Run("minted under another secret", func(t *testing.T) {
		s := mintChallenge(t, ring, ChallengeVerify, now)
		rotated := NewKeyRing(db, ringInputBeta)
		idOld, err := ring.CurrentKeyID(context.Background())
		require.NoError(t, err)
		idNew, err := rotated.CurrentKeyID(context.Background())
		require.NoError(t, err)
		require.NotEqual(t, idOld, idNew)

		mustRefuseChallenge(t, rotated, s, ChallengeVerify, now)
		mustVerifyChallenge(t, ring, s, ChallengeVerify, now) // control: the original secret still verifies it

		forged := wireBytes(t, s)
		copy(forged, idBytes(t, idNew))
		mustRefuseChallenge(t, rotated, encodeWire(forged), ChallengeVerify, now)

		newS := mintChallenge(t, rotated, ChallengeVerify, now)
		mustVerifyChallenge(t, rotated, newS, ChallengeVerify, now)
		mustRefuseChallenge(t, ring, newS, ChallengeVerify, now)
	})

	t.Run("two secrets that hold the same registry id", func(t *testing.T) {
		resetRegistry(t, db)
		first := NewKeyRing(db, ringInputAlpha)
		s := mintChallenge(t, first, ChallengeVerify, now)
		resetRegistry(t, db) // RESTART IDENTITY: the next secret registered gets the same id
		second := NewKeyRing(db, ringInputBeta)
		idFirst, err := first.CurrentKeyID(context.Background())
		require.NoError(t, err)
		idSecond, err := second.CurrentKeyID(context.Background())
		require.NoError(t, err)
		require.Equal(t, idFirst, idSecond)

		mustRefuseChallenge(t, second, s, ChallengeVerify, now) // the id matches; the MAC must still refuse
	})
}

// A challenge is not a guard token and a guard token is not a challenge, whichever
// function is handed which.
func TestChallenge_AndGuardTokenAreNotInterchangeable(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	chal := mintChallenge(t, ring, ChallengeVerify, now)
	tok := sealToken(t, ring, guardClaims(PurposeCancel))

	mustRefuseChallenge(t, ring, tok, ChallengeVerify, now)
	mustRefuseToken(t, ring, chal, PurposeCancel, now)
}

// A challenge has exactly one valid spelling: PR4 hashes its nonce for single use, so a
// second spelling of the same bytes must not pass as a second value.
func TestChallenge_NonCanonicalBase64Refused(t *testing.T) {
	ring, _ := newGuardRing(t)
	now := guardT0()
	s := mintChallenge(t, ring, ChallengeVerify, now)
	mustVerifyChallenge(t, ring, s, ChallengeVerify, now) // control: the canonical spelling verifies

	alts := respellTrailingBits(t, s)
	require.NotEmpty(t, alts)
	for _, alt := range alts {
		mustRefuseChallenge(t, ring, alt, ChallengeVerify, now)
	}
}

// A zero time, or one whose Unix milliseconds do not fit an int64, cannot be minted: it is
// the caller's bug, not a client fault.
func TestChallenge_MintRefusesUnusableTime(t *testing.T) {
	ring, _ := newGuardRing(t)
	times := map[string]time.Time{
		"zero":                      {},
		"seconds far in the future": time.Unix(1<<62, 0),
		"seconds far in the past":   time.Unix(-(1 << 62), 0),
	}
	for name, at := range times {
		t.Run(name, func(t *testing.T) {
			s, err := MintChallenge(context.Background(), ring, ChallengeVerify, at)

			require.Error(t, err)
			assert.Empty(t, s)
			assert.NotErrorIs(t, err, ErrChallengeInvalid)
			assert.NotErrorIs(t, err, ErrKeyUnavailable)
		})
	}
}
