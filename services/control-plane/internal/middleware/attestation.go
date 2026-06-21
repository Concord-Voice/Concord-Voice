package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// revokedVersionsKey aliases attestation.RevokedVersionsKey — the canonical
// definition is in the attestation package (single source of truth, per
// finding #29 of #1264 review). The local identifier is kept so existing call
// sites in this file don't need to qualify each reference.
const revokedVersionsKey = attestation.RevokedVersionsKey

// redisOpTimeout bounds each Redis call so a slow/hung Redis can't stall
// the request indefinitely. Aligned with auth.go's 2-second budget.
const redisOpTimeout = 2 * time.Second

// RequireAttestation returns middleware that gates authenticated routes on a
// valid attestation token. Per ADR-0010:
//   - D1: token was issued by the verify handler, bound to session_id + machine_id
//   - D2: fail-closed on Redis errors (503; matches AuthRequired posture at auth.go:90)
//   - D13: O(1) revocation check against the revoked_versions SET
//
// When enabled is false (the self-hosted default), the middleware is a
// pass-through no-op so existing routes are unaffected.
//
// Must be applied AFTER AuthRequired so user_id is in the context. Reads
// X-Session-ID (always required) and X-Machine-Id (required for desktop;
// absent for platform="web" which keys under "attestation:<session>:web").
func RequireAttestation(enabled bool, rdb *redis.Client, log *logger.Logger) gin.HandlerFunc {
	if !enabled {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		token, sessionID, ok := readAttestationHeaders(c, log, rdb)
		if !ok {
			return
		}

		// Machine ID empty implies a web client; key under the :web suffix
		// per ADR-0010 D8. Desktop clients always carry X-Machine-Id (validated
		// upstream by ValidateCustomHeaders middleware).
		machineID := c.GetHeader("X-Machine-Id")
		key := tokenKey(sessionID, machineID)

		ctx, cancel := context.WithTimeout(c.Request.Context(), redisOpTimeout)
		defer cancel()

		rec, ok := loadAndAuthenticateToken(ctx, c, rdb, log, key, token)
		if !ok {
			return
		}

		if !ensureNotRevoked(ctx, c, rdb, log, rec.Version) {
			return
		}
		// Note: ensureNotRevoked rejects on the revoked path with version
		// attribution via rejectAttForVersion. See finding #24 of #1264 review.

		// Expose the bound version on the context so downstream handlers can
		// read it for audit logs without re-reading Redis.
		c.Set("attestation_version", rec.Version)
		c.Next()
	}
}

// readAttestationHeaders extracts the attestation token + session ID headers.
// Writes the appropriate 403 reject + returns ok=false on missing values.
func readAttestationHeaders(c *gin.Context, log *logger.Logger, rdb *redis.Client) (string, string, bool) {
	token := c.GetHeader("X-Attestation-Token")
	if token == "" {
		rejectAtt(c, log, rdb, attestation.ErrMissing)
		return "", "", false
	}
	sessionID := c.GetHeader("X-Session-ID")
	if sessionID == "" {
		rejectAtt(c, log, rdb, attestation.ErrInvalid)
		return "", "", false
	}
	return token, sessionID, true
}

// loadAndAuthenticateToken reads the token record from Redis, parses it, and
// verifies it matches the bearer token. On Redis miss → ErrExpired (403). On
// Redis error → 503 (distinct from rejectAtt because "registry temporarily
// unavailable" implies retry-later, not a structured rejection). On JSON parse
// failure or token mismatch → ErrInvalid (403).
func loadAndAuthenticateToken(
	ctx context.Context,
	c *gin.Context,
	rdb *redis.Client,
	log *logger.Logger,
	key, token string,
) (attestation.TokenRecord, bool) {
	raw, err := rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		rejectAtt(c, log, rdb, attestation.ErrExpired)
		return attestation.TokenRecord{}, false
	}
	if err != nil {
		// Fail-closed: distinct from the rejectAtt path because a 503
		// signals "registry temporarily unavailable; retry later" rather
		// than the structured attestation-rejection codes.
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "attestation registry temporarily unavailable",
		})
		return attestation.TokenRecord{}, false
	}

	var rec attestation.TokenRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		rejectAtt(c, log, rdb, attestation.ErrInvalid)
		return attestation.TokenRecord{}, false
	}
	// Constant-time comparison: rec.Token gates authenticated routes, so a
	// timing side-channel on the equality check would let an attacker learn
	// the stored token prefix-by-prefix. Per [internal]rules/backend.md:
	// "Use crypto/subtle.ConstantTimeCompare for token/hash comparisons."
	if subtle.ConstantTimeCompare([]byte(rec.Token), []byte(token)) != 1 {
		rejectAtt(c, log, rdb, attestation.ErrInvalid)
		return attestation.TokenRecord{}, false
	}
	return rec, true
}

// ensureNotRevoked runs the O(1) revocation check against the Redis
// revoked_versions SET populated by the revoke handler. Fail-closed on
// Redis errors (matches the AuthRequired posture).
//
// On the revoked path, the version is threaded into the structured rejection
// log (finding #24 of #1264 review). The middleware now knows the bound
// version from the TokenRecord; without this attribution the hourly counter
// loses version attribution for revocation rejections — operators couldn't
// distinguish "v0.2.7 fleet revoked at 14:00" from "many versions revoked
// across the hour".
func ensureNotRevoked(
	ctx context.Context,
	c *gin.Context,
	rdb *redis.Client,
	log *logger.Logger,
	version string,
) bool {
	isRevoked, err := rdb.SIsMember(ctx, revokedVersionsKey, version).Result()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "attestation registry temporarily unavailable",
		})
		return false
	}
	if isRevoked {
		rejectAttForVersion(c, log, rdb, attestation.ErrRevoked, version)
		return false
	}
	return true
}

// tokenKey mirrors attestation.tokenKey but at the middleware layer (the
// attestation package's helper is keyed off VerifyPayload; here we work off
// the raw headers).
func tokenKey(sessionID, machineID string) string {
	if machineID == "" {
		return fmt.Sprintf("attestation:%s:web", sessionID)
	}
	return fmt.Sprintf("attestation:%s:%s", sessionID, machineID)
}

// rejectAtt emits the structured rejection log + 403 with the appropriate
// error code. UpdateAvailable hint is set for UNKNOWN_RELEASE and
// VERSION_TOO_OLD (mirrors the verify handler's reject helper).
//
// Use rejectAttForVersion instead when the bound version is known (post-
// TokenRecord-load, e.g., the revoked path). rejectAtt is reserved for the
// pre-load paths (missing token, missing session, expired record) where the
// middleware genuinely cannot attribute the rejection to a version.
func rejectAtt(c *gin.Context, log *logger.Logger, rdb *redis.Client, code attestation.ErrorCode) {
	rejectAttForVersion(c, log, rdb, code, "")
}

// rejectAttForVersion is the version-attributed sibling of rejectAtt. Use this
// from any reject path that has loaded the TokenRecord (currently:
// ensureNotRevoked). Per finding #24 of #1264 review.
func rejectAttForVersion(c *gin.Context, log *logger.Logger, rdb *redis.Client, code attestation.ErrorCode, version string) {
	// LogRejected best-effort writes an hourly Redis counter + structured log.
	// Platform remains "" — the middleware doesn't know it (would require
	// hydrating the cache from this layer, which would duplicate the lookup
	// done by the verify handler). Version is enough for the failure-mode
	// attribution operators care about.
	attestation.LogRejected(c.Request.Context(), log, rdb, code, version, "")
	body := attestation.ErrorResponse{
		Error: "Attestation failed",
		Code:  code,
	}
	if code == attestation.ErrUnknownRelease || code == attestation.ErrVersionTooOld {
		body.UpdateAvailable = true
		body.DownloadHelpURL = attestation.DownloadHelpURLDefault
	}
	c.AbortWithStatusJSON(http.StatusForbidden, body)
}
