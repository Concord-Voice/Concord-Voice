package attestation_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation/appattest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/appattesttest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type iosEnv struct {
	h    *attestation.Handler
	repo *attestation.Repository
	rdb  *redis.Client
	f    *appattesttest.Fixture
}

func newIOSEnv(t *testing.T, allowDev bool) *iosEnv {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(redisCleanup)
	repo := attestation.NewRepository(db)
	cache := attestation.NewCache(repo, nil, rdb, logger.New("development"))
	h := attestation.NewHandler(repo, cache, &fakeOIDC{}, nil, rdb, logger.New("development"))
	f := appattesttest.New(t)
	v, err := appattest.NewWithRoots(f.Roots, []string{appattesttest.AppID}, allowDev, func() time.Time { return appattesttest.Now })
	require.NoError(t, err)
	h.SetAppAttest(v)
	return &iosEnv{h: h, repo: repo, rdb: rdb, f: f}
}

// challenge asks the handler for a challenge and returns its raw bytes.
func (e *iosEnv) challenge(t *testing.T, userID, sessionID, machineID string) []byte {
	t.Helper()
	c, w := toGinVerifyRequest(t, "", userID, sessionID, machineID)
	e.h.Challenge(c)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp attestation.ChallengeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int(attestation.ChallengeTTL.Seconds()), resp.TTLSeconds)
	raw, err := base64.StdEncoding.DecodeString(resp.Challenge)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	return raw
}

func cdhOf(challenge []byte) []byte {
	return attestation.IOSClientDataHash(challenge, "1.0.0(5)")
}

func iosBody(machineID string, keyID, challenge, attestationObj, assertion []byte) string {
	p := attestation.VerifyPayload{
		Version:   "1.0.0(5)",
		Platform:  attestation.PlatformIOS,
		MachineID: machineID,
		KeyID:     base64.StdEncoding.EncodeToString(keyID),
		Challenge: base64.StdEncoding.EncodeToString(challenge),
	}
	if attestationObj != nil {
		p.Attestation = base64.StdEncoding.EncodeToString(attestationObj)
	}
	if assertion != nil {
		p.Assertion = base64.StdEncoding.EncodeToString(assertion)
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func (e *iosEnv) verify(t *testing.T, body, userID, sessionID, machineID string) (int, attestation.ErrorCode) {
	t.Helper()
	c, w := toGinVerifyRequest(t, body, userID, sessionID, machineID)
	e.h.Verify(c, testTokenTTL)
	var resp attestation.ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Code
}

// attest registers the fixture key through the handler.
func (e *iosEnv) attest(t *testing.T) {
	t.Helper()
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusOK, code)
}

func TestIOSVerify_AttestThenAssert(t *testing.T) {
	e := newIOSEnv(t, false)
	e.attest(t)

	raw, err := e.rdb.Get(context.Background(), "attestation:s-1:m-1").Result()
	require.NoError(t, err)
	var rec attestation.TokenRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &rec))
	require.Equal(t, "ios-1.0.0(5)", rec.Version)
	require.Empty(t, rec.SpaVersion)

	key, err := e.repo.GetAppAttestKey(context.Background(), e.f.KeyID)
	require.NoError(t, err)
	require.Equal(t, "production", key.Environment)
	require.Zero(t, key.SignCount)

	ch := e.challenge(t, "user-1", "s-2", "m-1")
	asrt := e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, 1, e.f.CredKey)
	code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, asrt), "user-1", "s-2", "m-1")
	require.Equal(t, http.StatusOK, code)

	key, err = e.repo.GetAppAttestKey(context.Background(), e.f.KeyID)
	require.NoError(t, err)
	require.Equal(t, uint32(1), key.SignCount)

	// The same counter again is a replay, even with a fresh challenge.
	ch = e.challenge(t, "user-1", "s-3", "m-1")
	asrt = e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, 1, e.f.CredKey)
	code, errCode := e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, asrt), "user-1", "s-3", "m-1")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, attestation.ErrInvalid, errCode)
}

func TestIOSVerify_ChallengeIsSingleUseAndBound(t *testing.T) {
	e := newIOSEnv(t, false)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	body := iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil)

	// Redeemed by another user: rejected, and the challenge is spent.
	code, errCode := e.verify(t, body, "user-2", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, attestation.ErrInvalid, errCode)
	code, _ = e.verify(t, body, "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)

	for name, redeem := range map[string][3]string{
		"other session": {"user-1", "s-x", "m-1"},
		"other machine": {"user-1", "s-1", "m-x"},
	} {
		t.Run(name, func(t *testing.T) {
			ch := e.challenge(t, "user-1", "s-1", "m-1")
			body := iosBody(redeem[2], e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil)
			code, _ := e.verify(t, body, redeem[0], redeem[1], redeem[2])
			require.Equal(t, http.StatusForbidden, code)
		})
	}
}

func TestIOSVerify_Rejections(t *testing.T) {
	e := newIOSEnv(t, false)

	t.Run("unknown key", func(t *testing.T) {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		asrt := e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, 1, e.f.CredKey)
		code, errCode := e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, asrt), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code)
		require.Equal(t, attestation.ErrKeyUnknown, errCode)
	})

	t.Run("development key refused", func(t *testing.T) {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		att := e.f.Attestation(t, cdhOf(ch), func(o *appattesttest.Options) { o.AAGUID = appattesttest.AAGUIDDevelopment })
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, att, nil), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code)
	})

	t.Run("unknown challenge", func(t *testing.T) {
		ch := make([]byte, 32)
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code)
	})

	t.Run("both proofs", func(t *testing.T) {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		att := e.f.Attestation(t, cdhOf(ch), nil)
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, att, att), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code)
	})

	t.Run("short key id", func(t *testing.T) {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID[:8], ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code)
	})

	t.Run("bad version", func(t *testing.T) {
		ch := e.challenge(t, "user-1", "s-1", "m-1")
		var p attestation.VerifyPayload
		require.NoError(t, json.Unmarshal([]byte(iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil)), &p))
		p.Version = "1.0 <script>"
		b, _ := json.Marshal(p)
		code, _ := e.verify(t, string(b), "user-1", "s-1", "m-1")
		require.Equal(t, http.StatusForbidden, code)
	})

	t.Run("attested twice", func(t *testing.T) {
		e.attest(t)
		ch := e.challenge(t, "user-1", "s-2", "m-1")
		code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-2", "m-1")
		require.Equal(t, http.StatusForbidden, code)
	})
}

func TestIOSVerify_DevelopmentKeyNeedsFlag(t *testing.T) {
	e := newIOSEnv(t, true)
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	att := e.f.Attestation(t, cdhOf(ch), func(o *appattesttest.Options) { o.AAGUID = appattesttest.AAGUIDDevelopment })
	code, _ := e.verify(t, iosBody("m-1", e.f.KeyID, ch, att, nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusOK, code)

	// Once the server stops allowing development keys, the stored key stops working.
	v, err := appattest.NewWithRoots(e.f.Roots, []string{appattesttest.AppID}, false, func() time.Time { return appattesttest.Now })
	require.NoError(t, err)
	e.h.SetAppAttest(v)
	ch = e.challenge(t, "user-1", "s-2", "m-1")
	asrt := e.f.Assertion(t, cdhOf(ch), appattesttest.AppID, 1, e.f.CredKey)
	code, _ = e.verify(t, iosBody("m-1", e.f.KeyID, ch, nil, asrt), "user-1", "s-2", "m-1")
	require.Equal(t, http.StatusForbidden, code)
}

func TestIOSVerify_RevokedVersion(t *testing.T) {
	e := newIOSEnv(t, false)
	require.NoError(t, e.rdb.SAdd(context.Background(), "attestation:revoked_versions", "ios-1.0.0(5)").Err())
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	code, errCode := e.verify(t, iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, attestation.ErrRevoked, errCode)
}

// An iOS revocation must outlive the shared set, which a desktop revoke expires.
func TestIOSVerify_PersistentRevocation(t *testing.T) {
	e := newIOSEnv(t, false)
	require.NoError(t, e.rdb.SAdd(context.Background(), attestation.RevokedIOSVersionsKey, "ios-1.0.0(5)").Err())
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	code, errCode := e.verify(t, iosBody("m-1", e.f.KeyID, ch, e.f.Attestation(t, cdhOf(ch), nil), nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, attestation.ErrRevoked, errCode)
}

// A proof signed for one version must not pass when the body claims another,
// or a relayed proof could dodge the revocation list.
func TestIOSVerify_VersionIsBoundToProof(t *testing.T) {
	e := newIOSEnv(t, false)
	require.NoError(t, e.rdb.SAdd(context.Background(), "attestation:revoked_versions", "ios-0.9.0(1)").Err())
	ch := e.challenge(t, "user-1", "s-1", "m-1")
	att := e.f.Attestation(t, attestation.IOSClientDataHash(ch, "0.9.0(1)"), nil)
	code, errCode := e.verify(t, iosBody("m-1", e.f.KeyID, ch, att, nil), "user-1", "s-1", "m-1")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, attestation.ErrInvalid, errCode)
}

func TestIOSVerify_NotConfigured(t *testing.T) {
	h, _, _, cleanup := newVerifyHandler(t)
	defer cleanup()
	body := iosBody("m-1", make([]byte, 32), make([]byte, 32), []byte("x"), nil)
	c, w := toGinVerifyRequest(t, body, "user-1", "s-1", "m-1")
	h.Verify(c, testTokenTTL)
	require.Equal(t, http.StatusForbidden, w.Code)

	c, w = toGinVerifyRequest(t, "", "user-1", "s-1", "m-1")
	h.Challenge(c)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestChallenge_RequestErrors(t *testing.T) {
	e := newIOSEnv(t, false)
	c, w := toGinVerifyRequest(t, "", "", "s-1", "m-1")
	e.h.Challenge(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	c, w = toGinVerifyRequest(t, "", "user-1", "s-1", "")
	e.h.Challenge(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChallenge_RedisDown(t *testing.T) {
	e := newIOSEnv(t, false)
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = broken.Close() }()
	h := attestation.NewHandler(e.repo, attestation.NewCache(e.repo, nil, broken, logger.New("development")), &fakeOIDC{}, nil, broken, logger.New("development"))
	v, err := appattest.NewWithRoots(e.f.Roots, []string{appattesttest.AppID}, false, func() time.Time { return appattesttest.Now })
	require.NoError(t, err)
	h.SetAppAttest(v)
	c, w := toGinVerifyRequest(t, "", "user-1", "s-1", "m-1")
	h.Challenge(c)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	// Redeeming a challenge needs Redis too, so verify answers 503 rather than a rejection.
	body := iosBody("m-1", e.f.KeyID, make([]byte, 32), []byte("x"), nil)
	c, w = toGinVerifyRequest(t, body, "user-1", "s-1", "m-1")
	h.Verify(c, testTokenTTL)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
}

// Pins the encoding; the iOS app's unit test checks the same vector.
func TestIOSClientDataHashVector(t *testing.T) {
	require.Equal(t, "c91da518ff44ebf880c7e5979668a8d85ce21238a2fec9aa08cfa2027eadd381",
		hex.EncodeToString(attestation.IOSClientDataHash([]byte("abc"), "1.0.0(5)")))
}
