package attestation

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// revokedVersionsTTL bounds the revoked_versions Redis SET so it auto-prunes
// once no bound attestation tokens could remain. Set to max attestation TTL
// (24h) + 1h buffer. Per ADR-0010 D13.
const revokedVersionsTTL = 25 * time.Hour

// Revoke marks a release as revoked.
// POST /api/v1/internal/attestation/revoke
// Authenticated upstream by an admin-only middleware that sets c.Set("admin_user", ...).
//
// On success: 200 OK. On unknown version/spa_version: 400 with ErrNotFound message.
// Per ADR-0010 D13: two-phase — Postgres row marked + Redis revoked_versions SET
// populated for O(1) middleware check + NATS event for cache invalidation.
func (h *Handler) Revoke(c *gin.Context) {
	adminUser, payload, ok := h.parseRevokeRequest(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	if !h.applyRevocation(ctx, c, payload, adminUser) {
		return
	}

	h.publishRevokeNATS(payload)
	h.logRevokeIssued(payload, adminUser)
	c.JSON(http.StatusOK, gin.H{"status": "revoked"})
}

// parseRevokeRequest validates admin auth + payload and returns the parsed
// admin user + payload. On any validation failure writes the appropriate 4xx
// response and returns ok=false so the caller short-circuits.
//
// Validation rules:
//   - admin_user must be set in the context (set by upstream admin middleware)
//   - payload must JSON-bind successfully
//   - exactly one of Version or SpaVersion must be non-empty (XOR)
func (h *Handler) parseRevokeRequest(c *gin.Context) (string, RevokePayload, bool) {
	adminUser := c.GetString("admin_user")
	if adminUser == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "admin authentication required"})
		return "", RevokePayload{}, false
	}

	var p RevokePayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return "", RevokePayload{}, false
	}
	if (p.Version == "" && p.SpaVersion == "") || (p.Version != "" && p.SpaVersion != "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one of version or spa_version required"})
		return "", RevokePayload{}, false
	}
	return adminUser, p, true
}

// applyRevocation dispatches the revocation to the appropriate axis-specific
// helper. Returns ok=false (after writing the response) if the underlying
// revocation failed.
//
// adminUser is the authenticated admin identity (set by upstream middleware),
// forwarded to the repository so it is recorded in revoked_by for forensic
// audit per ADR-0010 D13.
func (h *Handler) applyRevocation(ctx context.Context, c *gin.Context, p RevokePayload, adminUser string) bool {
	if p.Version != "" {
		return h.revokeBinaryAxis(ctx, c, p, adminUser)
	}
	return h.revokeSPAAxis(ctx, c, p, adminUser)
}

// revokeBinaryAxis revokes a binary release: marks the Postgres rows revoked
// (every platform row for the given version), then adds the version to the
// Redis revoked_versions SET (with TTL) so the middleware can do an O(1)
// revocation check. Per ADR-0010 D13.
//
// On ErrNotFound (unknown version): 400. On Redis SADD failure: 500 — Postgres
// state is correct but middleware won't see the revocation until cache refresh,
// so the operator must retry.
func (h *Handler) revokeBinaryAxis(ctx context.Context, c *gin.Context, p RevokePayload, adminUser string) bool {
	if err := h.repo.RevokeBinary(ctx, p.Version, p.Reason, adminUser); err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke binary"})
		return false
	}
	if h.rdb == nil {
		return true
	}
	if err := h.rdb.SAdd(ctx, revokedVersionsKey, p.Version).Err(); err != nil {
		// Postgres state is correct but middleware won't see the revoked_versions
		// gate until cache refresh. Surface as 500 so operator can retry.
		h.log.With("event", "attestation.revoke_redis_failed",
			"version", p.Version, "error", err.Error()).
			Error("revoke registered in Postgres but Redis SADD failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke registered but cache update failed"})
		return false
	}
	if err := h.rdb.Expire(ctx, revokedVersionsKey, revokedVersionsTTL).Err(); err != nil {
		// Set membership is correct — diagnostic only. Without the TTL the
		// SET would live indefinitely (across the 25h max-attestation-token
		// horizon plus buffer), inflating the revoked-version cache. Surface
		// at WARN so operators can re-apply the TTL manually. Per finding
		// #30 of #1264 review.
		h.log.With("event", "attestation.revoked_versions_expire_failed",
			"version", p.Version, "error", err.Error()).
			Warn("revoke succeeded but Redis Expire on revoked_versions SET failed; TTL drift")
	}
	return true
}

// revokeSPAAxis revokes an SPA release. Mirrors revokeBinaryAxis but for the
// release_spas table; SPA revocations do not populate the Redis revoked_versions
// SET because the verify handler reads the SPA registry from the in-memory cache
// (which is refreshed on the NATS event published below).
func (h *Handler) revokeSPAAxis(ctx context.Context, c *gin.Context, p RevokePayload, adminUser string) bool {
	if err := h.repo.RevokeSPA(ctx, p.SpaVersion, p.Reason, adminUser); err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke spa"})
		return false
	}
	return true
}

// publishRevokeNATS emits a best-effort NATS message so other replicas refresh
// their in-memory cache. Failure is intentionally swallowed — the poll-fallback
// in cache.go provides defense-in-depth.
func (h *Handler) publishRevokeNATS(p RevokePayload) {
	if h.nc == nil {
		return
	}
	_ = h.nc.Publish(subjectRegistryRevoked, map[string]string{
		"version":     p.Version,
		"spa_version": p.SpaVersion,
	})
}

// logRevokeIssued emits the structured audit log entry. Centralized so both
// axes log via the same shape.
func (h *Handler) logRevokeIssued(p RevokePayload, adminUser string) {
	h.log.With(
		"event", "attestation.revoke_issued",
		"version", p.Version,
		"spa_version", p.SpaVersion,
		"reason", p.Reason,
		"admin_user", adminUser,
	).Warn("attestation revoke issued")
}
