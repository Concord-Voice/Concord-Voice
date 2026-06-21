package attestation

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// extractBearer extracts the raw OIDC token from the Authorization header.
// Returns (raw, true) on success, or ("", false) after writing a 401 response.
// Caller short-circuits on !ok.
//
// Axis-agnostic — both publish endpoints share the same bearer extraction;
// only the OIDC verifier method differs (VerifySPA vs VerifyBinary).
func (h *Handler) extractBearer(c *gin.Context) (string, bool) {
	raw := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if raw == "" || raw == c.GetHeader("Authorization") {
		// Either empty header or Bearer prefix missing.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return "", false
	}
	return raw, true
}

// requireOIDC short-circuits with 503 Service Unavailable when the OIDC
// verifier is unavailable. h.oidc is interface-nil in degraded mode
// (REQUIRE_CLIENT_ATTESTATION=false and OIDC discovery failed at startup —
// see api/attestation_wiring.go). Calling h.oidc.VerifySPA / VerifyBinary in
// that state would panic on the nil receiver; this guard refuses cleanly so
// the startup warning log remains the authoritative operator signal.
//
// Returns true when h.oidc is usable. False means a 503 was already written
// and the caller must short-circuit.
func (h *Handler) requireOIDC(c *gin.Context) bool {
	if h.oidc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "attestation publish endpoint disabled — OIDC verifier unavailable",
		})
		return false
	}
	return true
}

// runOIDCVerify invokes the supplied per-axis verifier function and writes
// a 401 response on failure. Returns (sub, true) on success or ("", false)
// after writing the error. Caller short-circuits on !ok.
//
// Centralizes the failure-path logging so both axes emit the same
// observability shape — axis attribution comes from the supplied `axis`
// label, not from the OIDC sentinel errors (which are axis-agnostic by
// design, see oidc.go).
func (h *Handler) runOIDCVerify(
	c *gin.Context,
	axis, raw string,
	verify func(ctx context.Context, raw string) (string, error),
) (string, bool) {
	sub, err := verify(c.Request.Context(), raw)
	if err != nil {
		h.log.With("event", "attestation.publish_oidc_rejected",
			"axis", axis,
			"error", err.Error()).
			Warn("OIDC verify failed")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "OIDC verification failed"})
		return "", false
	}
	return sub, true
}

// PublishSPA accepts CI-published attestation hashes for a single SPA release.
// Authenticated via GitHub Actions OIDC token in the Authorization header.
// POST /api/v1/internal/attestation/publish/spa
//
// Body: {spa_version, html_hash}
//
// Published by main-cd.yml on every main-push (no signing → no cert_hash on
// this axis). W1 (#677 reconciliation): the OIDC verifier rejects tokens that
// were not minted by main-cd.yml, so a token issued for build-desktop.yml
// cannot publish to this endpoint.
//
// On success: 201 Created. Per ADR-0010 D11: same-spa_version+hash is
// idempotent (201 Created silent upsert); same-spa_version with DIFFERENT
// hash is a conflict (409) + CRITICAL log (potential supply-chain incident).
func (h *Handler) PublishSPA(c *gin.Context) {
	if !h.requireOIDC(c) {
		return
	}
	raw, ok := h.extractBearer(c)
	if !ok {
		return
	}
	sub, ok := h.runOIDCVerify(c, "spa", raw, h.oidc.VerifySPA)
	if !ok {
		return
	}

	var p PublishSPAPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Hash format guard (finding #22). Gin's struct tags don't support
	// regex validation; check manually so malformed hashes from CI runners
	// fail at the handler boundary with a clear 400, not downstream.
	if err := ValidateHash(p.HTMLHash); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	if err := h.repo.InsertSPA(ctx, p, sub); err != nil {
		if errors.Is(err, ErrConflict) {
			h.log.With("event", "attestation.publish_conflict",
				"axis", "spa",
				"spa_version", p.SpaVersion).
				Error("CRITICAL: hash mismatch on SPA re-publish")
			c.JSON(http.StatusConflict, gin.H{"error": "spa hash mismatch for spa_version"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record SPA"})
		return
	}

	if h.nc != nil {
		// Best-effort publish; cache poll-fallback covers the failure mode.
		_ = h.nc.Publish(subjectRegistryUpdated, map[string]string{
			"axis":        "spa",
			"spa_version": p.SpaVersion,
		})
	}

	h.log.With(
		"event", "attestation.publish_received",
		"axis", "spa",
		"spa_version", p.SpaVersion,
		"oidc_sub", sub,
	).Info("SPA publish accepted")

	c.JSON(http.StatusCreated, gin.H{
		"spa_version": p.SpaVersion,
	})
}

// PublishBinary accepts CI-published attestation hashes for a single binary
// release. Authenticated via GitHub Actions OIDC token in the Authorization
// header.
// POST /api/v1/internal/attestation/publish/binary
//
// Body: {version, platform, cert_hash}
//
// Published by build-desktop.yml post-signing (cert_hash is available only
// after Authenticode/notarytool steps complete). W1 (#677 reconciliation):
// the OIDC verifier rejects tokens that were not minted by build-desktop.yml,
// so a token issued for main-cd.yml cannot publish to this endpoint.
//
// On success: 201 Created. Per ADR-0010 D11: same-version+platform+hash is
// idempotent (201 Created silent upsert); same-version+platform with
// DIFFERENT hash is a conflict (409) + CRITICAL log (potential supply-chain
// incident).
func (h *Handler) PublishBinary(c *gin.Context) {
	if !h.requireOIDC(c) {
		return
	}
	raw, ok := h.extractBearer(c)
	if !ok {
		return
	}
	sub, ok := h.runOIDCVerify(c, "binary", raw, h.oidc.VerifyBinary)
	if !ok {
		return
	}

	var p PublishBinaryPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Re-parse via ParsePlatform: gin's JSON unbinder happily casts any string
	// to Platform (the type's underlying kind is `string`), so trust-boundary
	// validation must happen here. ParsePlatform combines the cast + Valid()
	// check into one audited step — see types.go ParsePlatform docs.
	if _, err := ParsePlatform(string(p.Platform)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid platform"})
		return
	}
	// Hash format guard (finding #22). Gin's struct tags don't support
	// regex validation; check manually so malformed hashes from CI runners
	// fail at the handler boundary with a clear 400, not downstream.
	if err := ValidateHash(p.CertHash); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	if err := h.repo.InsertBinary(ctx, p, sub); err != nil {
		if errors.Is(err, ErrConflict) {
			h.log.With("event", "attestation.publish_conflict",
				"axis", "binary",
				"version", p.Version,
				"platform", string(p.Platform)).
				Error("CRITICAL: hash mismatch on binary re-publish")
			c.JSON(http.StatusConflict, gin.H{"error": "binary hash mismatch for version+platform"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record binary"})
		return
	}

	if h.nc != nil {
		// Best-effort publish; cache poll-fallback covers the failure mode.
		_ = h.nc.Publish(subjectRegistryUpdated, map[string]string{
			"axis":     "binary",
			"version":  p.Version,
			"platform": string(p.Platform),
		})
	}

	h.log.With(
		"event", "attestation.publish_received",
		"axis", "binary",
		"version", p.Version,
		"platform", string(p.Platform),
		"oidc_sub", sub,
	).Info("binary publish accepted")

	c.JSON(http.StatusCreated, gin.H{
		"version":  p.Version,
		"platform": string(p.Platform),
	})
}
