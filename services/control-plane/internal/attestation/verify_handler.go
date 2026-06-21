package attestation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Verify validates client signals and issues an attestation token bound to
// (session, machine). POST /api/v1/attestation/verify
//
// Authenticated upstream by AuthRequired (sets c.Get("user_id")).
// Session ID required via X-Session-ID header. Machine ID required via
// X-Machine-Id header for desktop platforms (omitted for platform="web").
//
// Per ADR-0010:
// - D1: Option B handshake with session-bound short-lived token
// - D2: Fail-closed on Redis errors (matches AuthRequired posture)
// - D4: Linux skips signal 1 (cert hash)
// - D8: Web skips signals 1+2; token keyed by attestation:<session>:web
//
// ttl is the configured ATTESTATION_TOKEN_TTL (default 2h, range 30m-24h).
func (h *Handler) Verify(c *gin.Context, ttl time.Duration) {
	sessionID, payload, ok := h.parseVerifyRequest(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	if !h.checkVerifySignals(ctx, c, payload) {
		return
	}

	h.issueToken(ctx, c, sessionID, payload, ttl)
}

// parseVerifyRequest validates auth context, headers, and request body.
// Returns the session ID + parsed payload on success.
// On any failure writes the appropriate 4xx response and returns ok=false.
func (h *Handler) parseVerifyRequest(c *gin.Context) (string, VerifyPayload, bool) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return "", VerifyPayload{}, false
	}
	sessionID := c.GetHeader("X-Session-ID")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Session-ID required"})
		return "", VerifyPayload{}, false
	}

	var p VerifyPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return "", VerifyPayload{}, false
	}
	// Re-parse via ParsePlatform: gin's JSON unbinder happily casts any string
	// to Platform (the type's underlying kind is `string`), so trust-boundary
	// validation must happen here. ParsePlatform combines the cast + Valid()
	// check into one audited step — see types.go ParsePlatform docs.
	if _, err := ParsePlatform(string(p.Platform)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid platform"})
		return "", VerifyPayload{}, false
	}
	return sessionID, p, true
}

// checkVerifySignals applies the three attestation signals against the cache:
// (1) binary cert hash on desktop-installed platforms, (2) machine ID presence
// on non-web platforms, (3) SPA hash on all platforms. Also enforces (4) the
// O(1) revocation check against the Redis revoked_versions SET.
//
// On any signal mismatch writes a 403 reject response and returns ok=false.
func (h *Handler) checkVerifySignals(ctx context.Context, c *gin.Context, p VerifyPayload) bool {
	// Signal 1: cert hash (skipped for Linux + web)
	if p.Platform.RequiresCertHash() {
		rb, ok := h.cache.LookupBinary(p.Version, p.Platform)
		if !ok || rb.CertHash != p.CertHash {
			h.reject(c, ErrUnknownRelease, p.Version, p.Platform)
			return false
		}
	}

	// Signal 2: machine ID (skipped for web)
	if p.Platform.RequiresMachineID() && p.MachineID == "" {
		h.reject(c, ErrInvalid, p.Version, p.Platform)
		return false
	}

	// Signal 3: SPA hash — always required. One commit ⇒ one bundle ⇒ one hash;
	// remote and bundled index.html are byte-identical (reconciliation spec).
	rs, ok := h.cache.LookupSPA(p.SpaVersion)
	if !ok || rs.HTMLHash != p.SpaHash {
		h.reject(c, ErrUnknownRelease, p.Version, p.Platform)
		return false
	}

	// Revocation check — O(1) against Redis revoked_versions SET.
	// Fail-closed: IsRevoked returns true on Redis error.
	if h.cache.IsRevoked(ctx, p.Version) {
		h.reject(c, ErrRevoked, p.Version, p.Platform)
		return false
	}
	return true
}

// issueToken generates a fresh token, persists it to Redis with the configured
// TTL, and writes the success response. Fails closed (503) on Redis errors
// per ADR-0010 D2 — same posture as AuthRequired.
func (h *Handler) issueToken(ctx context.Context, c *gin.Context, sessionID string, p VerifyPayload, ttl time.Duration) {
	token := uuid.NewString()
	record := TokenRecord{
		Token:      token,
		Version:    p.Version,
		SpaVersion: p.SpaVersion,
		IssuedAt:   time.Now(),
	}
	bs, err := json.Marshal(record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal token record"})
		return
	}

	if h.rdb == nil {
		// Self-hosted mode without Redis is not a supported attestation path
		// — REQUIRE_CLIENT_ATTESTATION should be false. Fail loudly so the
		// operator sees the misconfiguration immediately.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attestation requires Redis"})
		return
	}
	key := tokenKey(sessionID, p)
	if err := h.rdb.Set(ctx, key, bs, ttl).Err(); err != nil {
		// Fail-closed per D2.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attestation registry temporarily unavailable"})
		return
	}

	LogIssued(ctx, h.log, h.rdb, p.Version, p.SpaVersion, p.Platform)
	c.JSON(http.StatusOK, VerifyResponse{
		AttestationToken: token,
		TTLSeconds:       int(ttl.Seconds()),
		ExpiresAt:        time.Now().Add(ttl),
	})
}

// tokenKey returns the Redis key for the token binding. Desktop platforms key
// by (session, machine_id); web (no hardware fingerprint) keys by session only.
func tokenKey(sessionID string, p VerifyPayload) string {
	if p.Platform == PlatformWeb {
		return fmt.Sprintf("attestation:%s:web", sessionID)
	}
	return fmt.Sprintf("attestation:%s:%s", sessionID, p.MachineID)
}

// reject emits a structured rejection log + 403 with the appropriate error code.
// UpdateAvailable hint set for UNKNOWN_RELEASE and VERSION_TOO_OLD.
func (h *Handler) reject(c *gin.Context, code ErrorCode, version string, platform Platform) {
	LogRejected(c.Request.Context(), h.log, h.rdb, code, version, platform)
	body := ErrorResponse{
		Error: "Attestation failed",
		Code:  code,
	}
	if code == ErrUnknownRelease || code == ErrVersionTooOld {
		body.UpdateAvailable = true
		body.DownloadHelpURL = DownloadHelpURLDefault
	}
	c.AbortWithStatusJSON(http.StatusForbidden, body)
}
