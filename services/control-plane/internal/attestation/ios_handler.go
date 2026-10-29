package attestation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation/appattest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
)

// RevokedIOSVersionsKey holds revoked "ios-<version>" values with no TTL, so a
// desktop revoke's 25h expiry on the shared set cannot undo them (D17). Both
// /verify and RequireAttestation check it.
const RevokedIOSVersionsKey = "attestation:revoked_ios_versions"

// ChallengeTTL bounds how long an App Attest challenge stays redeemable.
const ChallengeTTL = 5 * time.Minute

// iosVersionPattern bounds the client-reported version, e.g. "1.0.0(5)". Apple
// does not attest it, but the app signs it (IOSClientDataHash), so a proxy
// cannot rewrite it; it feeds D13 revocation and logs.
var iosVersionPattern = regexp.MustCompile(`^[0-9A-Za-z.()+-]{1,32}$`)

// ChallengeResponse is the body of POST /api/v1/attestation/challenge.
type ChallengeResponse struct {
	Challenge  string `json:"challenge"`
	TTLSeconds int    `json:"ttl_seconds"`
}

// SetAppAttest enables the iOS path. Without it, platform "ios" is rejected
// and the challenge endpoint answers 503.
func (h *Handler) SetAppAttest(v *appattest.Verifier) { h.appAttest = v }

func challengeKey(challenge []byte) string {
	sum := sha256.Sum256(challenge)
	return "attestation:challenge:" + hex.EncodeToString(sum[:])
}

// IOSClientDataHash is what the app signs: SHA-256 of a domain tag, the
// challenge and the version it reports. Binding the version stops a relayed
// genuine proof from claiming a different, unrevoked version. The challenge is
// always 32 bytes, so the encoding is unambiguous.
func IOSClientDataHash(challenge []byte, version string) []byte {
	h := sha256.New()
	h.Write([]byte("concord-appattest-v1\x00"))
	h.Write(challenge)
	h.Write([]byte{0})
	h.Write([]byte(version))
	return h.Sum(nil)
}

// challengeBinding ties a challenge to the session, machine and hashed user
// that requested it, so it cannot be redeemed from anywhere else.
func challengeBinding(sessionID, machineID, userID string) string {
	return sessionID + "\x00" + machineID + "\x00" + HashUserID(userID)
}

// Challenge issues a single-use App Attest challenge.
// POST /api/v1/attestation/challenge, behind AuthRequired. Requires the same
// X-Session-ID and X-Machine-Id headers the token will be bound to.
func (h *Handler) Challenge(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}
	sessionID, machineID := c.GetHeader("X-Session-ID"), c.GetHeader("X-Machine-Id")
	if sessionID == "" || machineID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Session-ID and X-Machine-Id required"})
		return
	}
	if h.appAttest == nil || h.rdb == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "app attest is not enabled on this server"})
		return
	}
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate challenge"})
		return
	}
	ctx := c.Request.Context()
	if err := h.rdb.Set(ctx, challengeKey(challenge), challengeBinding(sessionID, machineID, userID), ChallengeTTL).Err(); err != nil {
		h.emit(ctx, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable})
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errRegistryUnavailable})
		return
	}
	c.JSON(http.StatusOK, ChallengeResponse{
		Challenge:  base64.StdEncoding.EncodeToString(challenge),
		TTLSeconds: int(ChallengeTTL.Seconds()),
	})
}

// errIOSUnavailable marks a dependency failure (503) as opposed to a rejection.
var errIOSUnavailable = errors.New("attestation: dependency unavailable")

// checkIOS runs the App Attest path of Verify (ADR-0010 D17). On success it
// rewrites p.Version to the "ios-" namespace the token and the revoked set use.
// On failure it has already written the response.
func (h *Handler) checkIOS(ctx context.Context, c *gin.Context, sessionID string, p *VerifyPayload) bool {
	if h.appAttest == nil || !iosVersionPattern.MatchString(p.Version) || p.MachineID == "" {
		// The version is unchecked here, so it is not logged.
		h.reject(c, ErrInvalid, "ios-invalid", p.Platform)
		return false
	}
	version := "ios-" + p.Version
	code, err := h.proveIOS(ctx, c.GetString("user_id"), sessionID, p)
	if errors.Is(err, errIOSUnavailable) {
		h.unavailable(ctx, c)
		return false
	}
	if err != nil {
		h.reject(c, code, version, p.Platform)
		return false
	}

	revoked, err := h.cache.IsRevoked(ctx, version)
	if err != nil {
		h.unavailable(ctx, c)
		return false
	}
	if !revoked {
		// The shared set expires 25h after any desktop revoke, and iOS has no
		// release registry behind it, so iOS revocations also live here forever.
		revoked, err = h.rdb.SIsMember(ctx, RevokedIOSVersionsKey, version).Result()
		if err != nil {
			h.unavailable(ctx, c)
			return false
		}
	}
	if revoked {
		h.reject(c, ErrRevoked, version, p.Platform)
		return false
	}
	p.Version, p.SpaVersion = version, ""
	return true
}

// unavailable answers 503 when Postgres or Redis fails mid-verify.
func (h *Handler) unavailable(ctx context.Context, c *gin.Context) {
	h.emit(ctx, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable})
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": errRegistryUnavailable})
}

// proveIOS redeems the challenge and checks the attestation or assertion.
// It returns the rejection code to send, or errIOSUnavailable for a 503.
func (h *Handler) proveIOS(ctx context.Context, userID, sessionID string, p *VerifyPayload) (ErrorCode, error) {
	keyID, err1 := base64.StdEncoding.DecodeString(p.KeyID)
	challenge, err2 := base64.StdEncoding.DecodeString(p.Challenge)
	if err1 != nil || err2 != nil || len(keyID) != sha256.Size || len(challenge) == 0 ||
		(p.Attestation == "") == (p.Assertion == "") {
		return ErrInvalid, appattest.ErrRejected
	}

	if h.rdb == nil {
		return "", errIOSUnavailable
	}
	// Single use: GETDEL consumes the challenge even when the proof then fails.
	bound, err := h.rdb.GetDel(ctx, challengeKey(challenge)).Result()
	if errors.Is(err, redis.Nil) {
		return ErrInvalid, appattest.ErrRejected
	}
	if err != nil {
		return "", errIOSUnavailable
	}
	if subtle.ConstantTimeCompare([]byte(bound), []byte(challengeBinding(sessionID, p.MachineID, userID))) != 1 {
		return ErrInvalid, appattest.ErrRejected
	}
	clientDataHash := IOSClientDataHash(challenge, p.Version)

	if p.Attestation != "" {
		return h.proveAttestation(ctx, userID, keyID, p.Attestation, clientDataHash)
	}
	return h.proveAssertion(ctx, keyID, p.Assertion, clientDataHash)
}

func (h *Handler) proveAttestation(ctx context.Context, userID string, keyID []byte, encoded string, clientDataHash []byte) (ErrorCode, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ErrInvalid, appattest.ErrRejected
	}
	key, err := h.appAttest.VerifyAttestation(keyID, raw, clientDataHash)
	if err != nil {
		return ErrInvalid, err
	}
	err = h.repo.InsertAppAttestKey(ctx, AppAttestKey{KeyID: keyID, PublicKey: key.PublicKey, Environment: string(key.Environment)}, HashUserID(userID))
	if errors.Is(err, ErrConflict) {
		// A key is attested once; a second attestation of it is a replay.
		return ErrInvalid, err
	}
	if err != nil {
		return "", errIOSUnavailable
	}
	return "", nil
}

func (h *Handler) proveAssertion(ctx context.Context, keyID []byte, encoded string, clientDataHash []byte) (ErrorCode, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ErrInvalid, appattest.ErrRejected
	}
	key, err := h.repo.GetAppAttestKey(ctx, keyID)
	if err != nil {
		return "", errIOSUnavailable
	}
	if key == nil {
		return ErrKeyUnknown, appattest.ErrRejected
	}
	// A development key stops working once the server stops allowing them.
	if key.Environment != string(appattest.Production) && !h.appAttest.AllowsDevelopment() {
		return ErrInvalid, appattest.ErrRejected
	}
	counter, err := h.appAttest.VerifyAssertion(raw, clientDataHash, key.PublicKey, key.SignCount)
	if err != nil {
		return ErrInvalid, err
	}
	err = h.repo.AdvanceAppAttestCounter(ctx, keyID, counter)
	if errors.Is(err, ErrConflict) {
		return ErrInvalid, err
	}
	if err != nil {
		return "", errIOSUnavailable
	}
	return "", nil
}
