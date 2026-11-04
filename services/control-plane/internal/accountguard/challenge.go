package accountguard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// ErrChallengeInvalid is the one error a challenge presented by a client produces: bad
// base64 or a second spelling of the same bytes, a wrong length, a key id other than the
// current one, a MAC that does not verify, a purpose outside the set or other than the
// one asked for, or an issue time further than decide.ChallengeSkew from now. It is
// returned bare, never wrapped, so no caller can tell one cause from another.
var ErrChallengeInvalid = errors.New("accountguard: invalid challenge")

// ChallengePurpose names the one ceremony a challenge was minted for. A challenge
// verified for any other purpose is refused. The zero value is not a purpose.
type ChallengePurpose uint8

// The six purposes a challenge serves.
const (
	ChallengeVerify            ChallengePurpose = iota + 1 // verifying a credential
	ChallengeCancel                                        // the cancel action
	ChallengeResolve                                       // the resolve action
	ChallengeContest                                       // contesting a request
	ChallengeSignOutEverywhere                             // signing out everywhere
	ChallengeBreakGlass                                    // break-glass recovery
)

// valid reports whether p is in the closed set.
func (p ChallengePurpose) valid() bool {
	switch p {
	case ChallengeVerify, ChallengeCancel, ChallengeResolve, ChallengeContest,
		ChallengeSignOutEverywhere, ChallengeBreakGlass:
		return true
	default:
		return false
	}
}

const (
	// challengeNonceLen is the random nonce's length.
	challengeNonceLen = 16
	// challengeTimeLen is the issue time's length (unix ms, int64).
	challengeTimeLen = 8
	// challengeBodyLen is everything the MAC covers: key id, nonce, issue time, purpose.
	challengeBodyLen = keyIDHeaderLen + challengeNonceLen + challengeTimeLen + 1
	// challengeWireLen is the body followed by its HMAC-SHA256.
	challengeWireLen = challengeBodyLen + sha256.Size
)

// MintChallenge returns a fresh challenge for purpose p issued at now. The wire form is
// unpadded base64url over
//
//	keyID (int16 BE) || nonce (16 bytes) || issued (unix ms, int64 BE) || purpose (1 byte)
//	    || HMAC-SHA256(every preceding byte)
//
// under the ring's challenge key, with now truncated to the millisecond. The ring is
// resolved first, so a ring that cannot name its key wraps ErrKeyUnavailable whatever p
// is. A purpose outside the closed set, or a zero or unrepresentable now, is refused as
// the caller's bug. On any error the challenge is "".
func MintChallenge(ctx context.Context, keys *KeyRing, p ChallengePurpose, now time.Time) (string, error) {
	keyID, err := keys.CurrentKeyID(ctx)
	if err != nil {
		return "", fmt.Errorf("mint challenge: %w", err)
	}
	key, err := keys.Key(LabelChallenge)
	if err != nil {
		return "", fmt.Errorf("mint challenge: %w", err)
	}
	if !p.valid() {
		return "", fmt.Errorf("mint challenge: %w: unknown challenge purpose %d", errBadInput, p)
	}
	issuedMs, ok := unixMilli(now)
	if now.IsZero() || !ok {
		return "", fmt.Errorf("mint challenge: %w: issue time zero or out of range", errBadInput)
	}

	header, err := keyIDHeader(keyID)
	if err != nil {
		return "", fmt.Errorf("mint challenge: %w", err)
	}
	body := make([]byte, 0, challengeWireLen)
	body = append(body, header...)
	nonce := make([]byte, challengeNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("mint challenge: nonce: %w", err)
	}
	body = append(body, nonce...)
	// binary.Append writes an int64 as its two's-complement bytes; its error is nil for a
	// fixed-size value, so this branch is unreachable today and checked regardless.
	if body, err = binary.Append(body, binary.BigEndian, issuedMs); err != nil {
		return "", fmt.Errorf("mint challenge: issue time: %w", err)
	}
	body = append(body, byte(p))
	return base64.RawURLEncoding.EncodeToString(append(body, challengeMAC(key, body)...)), nil
}

// VerifyChallenge checks that s was minted under the ring's current key for purpose
// want, and that its issue time is within decide.ChallengeSkew of now, both ends
// inclusive, at millisecond precision. On success it returns SHA-256 of the challenge's
// nonce, which the caller spends to make the challenge single-use; this function spends
// nothing and writes nothing, so verifying twice succeeds twice.
//
// The ring is resolved first, so a ring that cannot name its key wraps ErrKeyUnavailable
// even for a malformed challenge. Every fault in s itself returns the bare
// ErrChallengeInvalid. On any error the hash is the zero value. Nothing is logged.
func VerifyChallenge(ctx context.Context, keys *KeyRing, s string, want ChallengePurpose, now time.Time) ([32]byte, error) {
	keyID, err := keys.CurrentKeyID(ctx)
	if err != nil {
		return [32]byte{}, fmt.Errorf("verify challenge: %w", err)
	}
	key, err := keys.Key(LabelChallenge)
	if err != nil {
		return [32]byte{}, fmt.Errorf("verify challenge: %w", err)
	}
	header, err := keyIDHeader(keyID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("verify challenge: %w", err)
	}

	wire, ok := decodeCanonical(s)
	if !ok || len(wire) != challengeWireLen || !bytes.Equal(wire[:keyIDHeaderLen], header) {
		return [32]byte{}, ErrChallengeInvalid
	}
	body, mac := wire[:challengeBodyLen], wire[challengeBodyLen:]
	// hmac.Equal compares in constant time. Nothing below reads an unauthenticated byte.
	if !hmac.Equal(challengeMAC(key, body), mac) {
		return [32]byte{}, ErrChallengeInvalid
	}
	p := ChallengePurpose(body[challengeBodyLen-1])
	if !p.valid() || p != want {
		return [32]byte{}, ErrChallengeInvalid
	}
	// binary.Decode reads the two's-complement bytes straight into the int64; its error
	// is nil for an 8-byte buffer, so the branch is unreachable and checked regardless.
	var issuedMs int64
	timeAt := keyIDHeaderLen + challengeNonceLen
	if _, err := binary.Decode(body[timeAt:timeAt+challengeTimeLen], binary.BigEndian, &issuedMs); err != nil {
		return [32]byte{}, ErrChallengeInvalid
	}
	if !challengeFresh(issuedMs, now) {
		return [32]byte{}, ErrChallengeInvalid
	}
	return sha256.Sum256(body[keyIDHeaderLen:timeAt]), nil
}

// challengeMAC returns HMAC-SHA256(body) under key.
func challengeMAC(key, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(body) // hash.Hash.Write never returns an error
	return m.Sum(nil)
}

// challengeFresh reports |now - issued| <= ChallengeSkew at millisecond precision: now is
// truncated to the millisecond and both ends are inclusive. Comparing times rather than
// subtracting millisecond counts cannot overflow.
func challengeFresh(issuedMs int64, now time.Time) bool {
	issued := time.UnixMilli(issuedMs)
	at := now.Truncate(time.Millisecond)
	return !at.Before(issued.Add(-decide.ChallengeSkew)) && !at.After(issued.Add(decide.ChallengeSkew))
}
