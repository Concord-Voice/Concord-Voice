package attestation_test

// Adversarial regression tests for the iOS App Attest path (ADR-0010 D17),
// from the 2026-10-29 red-team pass. Each test passes when the attack is
// REFUSED.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation/appattest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/appattesttest"
)

// RT-F1: a key proves only for the account that attested it. Another
// account presenting the same key gets KEY_UNKNOWN and must attest its own.
func TestRT_AssertionKeyBoundToAttestingUser(t *testing.T) {
	e := newIOSEnv(t, false)
	e.attest(t) // user-1 attests the device key on s-1/m-1

	ch := e.challenge(t, "user-2", "s-bot", "m-bot")
	asrt := e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, 1, e.f.CredKey)
	code, body := e.verify(t, iosBody("m-bot", e.f.KeyID, ch, nil, asrt), "user-2", "s-bot", "m-bot")
	require.Equal(t, http.StatusForbidden, code)
	require.Contains(t, body, string(attestation.ErrKeyUnknown))
	_, err := e.rdb.Get(context.Background(), "attestation:s-bot:m-bot").Result()
	require.Error(t, err, "no token may be minted for the other account")
}

// RT-F3: an attestation with an oversized x5c chain is refused before any
// certificate is parsed.
func TestRT_OversizedX5CRefused(t *testing.T) {
	e := newIOSEnv(t, false)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	good := e.f.Attestation(t, cdhOf(ch), nil)
	var obj struct {
		Fmt      string         `cbor:"fmt"`
		AttStmt  map[string]any `cbor:"attStmt"`
		AuthData []byte         `cbor:"authData"`
	}
	require.NoError(t, cbor.Unmarshal(good, &obj))
	x5c, ok := obj.AttStmt["x5c"].([]any)
	require.True(t, ok)
	require.Len(t, x5c, 2)
	leaf, ok := x5c[0].([]byte)
	require.True(t, ok)
	inter, ok := x5c[1].([]byte)
	require.True(t, ok)
	obj.AttStmt["x5c"] = [][]byte{leaf, inter, inter, inter, inter}
	big, err := cbor.Marshal(obj)
	require.NoError(t, err)
	code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, big, nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
}

// RT-F3: the verify body is capped before JSON decoding.
func TestRT_VerifyBodyCapped(t *testing.T) {
	e := newIOSEnv(t, false)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	body := iosBody("m-1", e.f.KeyID, ch, bytes.Repeat([]byte{0}, attestation.MaxVerifyBodyBytes), nil)
	code, _ := e.verify(t, body, "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusBadRequest, code)
}

func TestRTCleared_VersionShapes(t *testing.T) {
	e := newIOSEnv(t, false)
	for _, v := range []string{"1.0.0(5)\n", " 1.0.0(5)", "1.0.0(5) ", "1.0.0（5）", "1.0.0(5)\x00", strings.Repeat("1", 33), "1.0.0(5)/../x", "1.0.0%285%29"} {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		att := e.f.Attestation(t, attestation.IOSClientDataHash(ch, v), nil)
		var p attestation.VerifyPayload
		require.NoError(t, json.Unmarshal([]byte(iosBody("m-1", e.f.KeyID, ch, att, nil)), &p))
		p.Version = v
		b, _ := json.Marshal(p)
		code, _ := e.verify(t, string(b), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code, "version %q", v)
	}
}

func TestRTCleared_RevokedVersionCaseVariantNeedsNewProof(t *testing.T) {
	// A genuine build signs its own exact version; claiming a case variant
	// with that proof fails the clientDataHash.
	e := newIOSEnv(t, false)
	require.NoError(t, e.rdb.SAdd(context.Background(), attestation.RevokedIOSVersionsKey, "ios-1.0.0(5)").Err())
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	att := e.f.Attestation(t, cdhOf(ch), nil) // signed for "1.0.0(5)"
	var p attestation.VerifyPayload
	require.NoError(t, json.Unmarshal([]byte(iosBody("m-1", e.f.KeyID, ch, att, nil)), &p))
	p.Version = "1.0.0(05)"
	b, _ := json.Marshal(p)
	code, errCode := e.verify(t, string(b), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, attestation.ErrInvalid, errCode)
}

func TestRTCleared_ChallengeReplayAfterSuccess(t *testing.T) {
	e := newIOSEnv(t, false)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	body := iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil)
	code, _ := e.verify(t, body, "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusOK, code)
	code, _ = e.verify(t, body, "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
}

func TestRTCleared_ChallengeExpiry(t *testing.T) {
	e := newIOSEnv(t, false)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	ttl := e.rdb.TTL(context.Background(), "attestation:challenge:"+hexSHA(ch)).Val()
	require.True(t, ttl > 0 && ttl <= attestation.ChallengeTTL, "ttl=%s", ttl)
}

func TestRTCleared_AssertionCounterRollbackAndEqual(t *testing.T) {
	e := newIOSEnv(t, false)
	e.attest(t)
	ch := e.challenge(t, "user-1", "s-2", "m-1")
	code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, 10, e.f.CredKey)), "user-1", "s-2", "m-1")
	require.Equal(t, http.StatusOK, code)
	for _, ctr := range []uint32{9, 10, 0} {
		ch := e.challenge(t, "user-1", "s-2", "m-1")
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, ctr, e.f.CredKey)), "user-1", "s-2", "m-1")
		require.Equal(t, http.StatusForbidden, code, "counter %d", ctr)
	}
}

func TestRTCleared_AttestationRequiresZeroCounterAndRightApp(t *testing.T) {
	e := newIOSEnv(t, false)
	for name, mod := range map[string]func(*appattesttest.Options){
		"counter 1":        func(o *appattesttest.Options) { o.Counter = 1 },
		"other team":       func(o *appattesttest.Options) { o.RPID = "OTHERTEAM1.com.example.app" },
		"dev aaguid":       func(o *appattesttest.Options) { o.AAGUID = appattesttest.AAGUIDDevelopment },
		"zero aaguid":      func(o *appattesttest.Options) { o.AAGUID = make([]byte, 16) },
		"credid mismatch":  func(o *appattesttest.Options) { o.CredID = bytes.Repeat([]byte{1}, 32) },
		"nonce other hash": func(o *appattesttest.Options) { o.NonceFor = bytes.Repeat([]byte{2}, 32) },
		"other key":        func(o *appattesttest.Options) { o.CredKey = appattesttest.MustKey(t, o.CredKey.Curve) },
	} {
		t.Run(name, func(t *testing.T) {
			ch := e.challenge(t, "user-1", "s-1", "m-1")
			code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), mod), nil), "user-1", "s-1", "m-1")
			require.Equal(t, http.StatusForbidden, code)
		})
	}
}

func TestRTCleared_ForeignRootChain(t *testing.T) {
	// A self-made chain (attacker root) is refused by a verifier pinned to another root.
	e := newIOSEnv(t, false)
	evil := appattesttest.New(t)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	code, _ := e.verify(t, iosBody("m-1", evil.KeyID, ch, evil.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
}

func TestRTCleared_KeyIDCrossAttestByOtherUser(t *testing.T) {
	// user-2 cannot re-attest (overwrite) user-1's key row.
	e := newIOSEnv(t, false)
	e.attest(t)
	ch := e.challenge(t, "user-2", "s-9", "m-9")
	code, _ := e.verify(t, iosBody("m-9", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-2", "s-9", "m-9")
	require.Equal(t, http.StatusForbidden, code)
}

func TestRTCleared_RedisDownMidVerifyFailsClosed(t *testing.T) {
	e := newIOSEnv(t, false)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	body := iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil)
	require.NoError(t, e.rdb.Close())
	code, _ := e.verify(t, body, "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusServiceUnavailable, code)
}

func TestRTCleared_MalformedCBOR(t *testing.T) {
	e := newIOSEnv(t, false)
	for _, blob := range [][]byte{{0xff}, {0x9f, 0x9f, 0x9f}, bytes.Repeat([]byte{0x81}, 100000), {0xbf}} {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, blob, nil), "user-1", "s-1", "m-1")
		require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden}, code)
		ch = e.challenge(t, "user-1", "s-1", "m-1")
		code, _ = e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, blob), "user-1", "s-1", "m-1")
		require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden}, code)
	}
	_ = base64.StdEncoding
}

func hexSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

var _ = appattest.ErrRejected
