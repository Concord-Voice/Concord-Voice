package redemption

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// genericInvalidMessage is the SINGLE user-facing message for every
// not-redeemable condition (bad checksum, not found, revoked, expired,
// not-yet-valid, exhausted, unknown grant_kind). Returning one message for all
// of them is the no-oracle property (spec §4 step 3 / §8): the client can never
// learn WHICH condition failed, only that the code is not valid.
const genericInvalidMessage = "That code is not valid."

// Handler serves the redemption HTTP surface: the authenticated user /redeem
// endpoint and the admin code-generation endpoint. Built with the standard
// handler-struct DI pattern.
type Handler struct {
	engine     *Engine
	issuer     *Issuer
	adminToken string // shared-secret gate for the admin generation endpoint (see AdminGate)
	log        *logger.Logger
}

// NewHandler builds the redemption HTTP handler. adminToken gates the
// generation endpoint; an empty token DISABLES the admin HTTP endpoint (the
// route returns 503), which is the safe default for dev/self-hosted where no
// issuer secret is provisioned. The CLI path is unaffected (it has direct DB
// access).
func NewHandler(engine *Engine, issuer *Issuer, adminToken string, log *logger.Logger) *Handler {
	return &Handler{engine: engine, issuer: issuer, adminToken: strings.TrimSpace(adminToken), log: log}
}

// ── Redeem (authenticated user) ─────────────────────────────────────────────

type redeemRequest struct {
	Code string `json:"code" binding:"required"`
}

// Redeem handles POST /api/v1/redeem. It is rate-limited at the route (per-user
// + per-IP) and logs failures PII-safely (never the code value — only the
// outcome category and the user id, which is sanitized).
func (h *Handler) Redeem(c *gin.Context) {
	userIDStr := c.GetString("user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		// Auth middleware guarantees a valid user_id; a parse failure is a
		// server-side invariant break, not a client error.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req redeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a code is required"})
		return
	}

	outcome, err := h.engine.Redeem(c.Request.Context(), userID, req.Code)
	if err != nil {
		h.handleRedeemError(c, userID, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"description": outcome.Description,
	})
}

// handleRedeemError maps engine errors to responses while preserving the
// no-oracle property AND PII-safe logging.
//
//   - ErrCodeNotValid / unknown grant_kind / checksum fail → 400 generic message.
//   - ErrAlreadyRedeemed → 409 (idempotent "already redeemed"; not an oracle —
//     it only reflects THIS user's own prior redemption).
//   - any other (post-commit notify error, infra error) → the grant may have
//     succeeded; we log and still 200 with the description if present, else 500.
//
// Logging: we log the OUTCOME CATEGORY and the user id only. The code value, its
// hash, and grant_params NEVER reach a log sink (observability.md core #1/#2).
// The user id is run through sanitizeID before logging (CWE-117 defense, even
// though a parsed uuid is structurally hex-only — uniform sanitize per #1645).
func (h *Handler) handleRedeemError(c *gin.Context, userID uuid.UUID, err error) {
	switch {
	case errors.Is(err, ErrCodeNotValid):
		h.log.Info("redeem rejected", "outcome", "invalid", "user_id", sanitizeID(userID.String()))
		c.JSON(http.StatusBadRequest, gin.H{"error": genericInvalidMessage})
	case errors.Is(err, ErrAlreadyRedeemed):
		h.log.Info("redeem rejected", "outcome", "already_redeemed", "user_id", sanitizeID(userID.String()))
		c.JSON(http.StatusConflict, gin.H{"error": "You have already redeemed this code."})
	default:
		// An error AFTER a successful commit (post-commit notify) still means
		// the grant landed — the engine returns the description alongside the
		// wrapped error in that case. We cannot distinguish that here without
		// coupling, so we log and return a generic 500; the grant (if any) is
		// durable and the client will see it on its next entitlements fetch.
		h.log.Error("redeem error", "outcome", "error", "user_id", sanitizeID(userID.String()), "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not redeem the code right now. Please try again."})
	}
}

// ── Admin generation (issuer-authz gated) ───────────────────────────────────

type generateRequest struct {
	GrantKind   string         `json:"grant_kind" binding:"required"`
	GrantParams map[string]any `json:"grant_params"`
	Count       int            `json:"count" binding:"required"`
	Prefix      string         `json:"prefix"`
	SingleUse   *bool          `json:"single_use"` // pointer: distinguish absent (default true) from explicit false
	MaxRedeems  *int           `json:"max_redemptions"`
	ExpiresAt   *time.Time     `json:"expires_at"`
	BatchID     string         `json:"batch_id"`
}

// Generate handles POST /api/v1/admin/redemption/codes. It is gated by AdminGate
// (shared-secret header) BEFORE this handler runs. Returns the minted plaintext
// codes ONCE — they are never persisted server-side and cannot be re-fetched.
//
// SECURITY NOTE / FLAGGED DESIGN GAP: the gate is a config-provisioned shared
// secret, NOT a platform-admin RBAC role — no such role exists in the codebase
// today (Role is per-SERVER only: server_members.role ∈ {owner,admin,member}).
// See AdminGate's doc + the PR description. This is the "closest existing
// privileged check" the issue authorizes as an interim, flagged for review.
func (h *Handler) Generate(c *gin.Context) {
	var req generateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Bound the requested count at the HTTP boundary BEFORE it reaches the
	// issuer (which drives a per-count allocation). A huge or non-positive count
	// is operator misuse → a precise 400 (this is the admin side, no no-oracle
	// concern). The issuer re-validates defensively; this is the first line.
	if req.Count < 1 || req.Count > MaxBatchSize {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": errIssueCountTooLarge.Error(),
		})
		return
	}

	singleUse := true
	if req.SingleUse != nil {
		singleUse = *req.SingleUse
	}

	// Record the issuer identity from the authenticated admin context if present
	// (the admin endpoint sits behind AuthRequired too, so user_id is set).
	var createdBy uuid.NullUUID
	if uid, err := uuid.Parse(c.GetString("user_id")); err == nil {
		createdBy = uuid.NullUUID{UUID: uid, Valid: true}
	}

	spec := IssueSpec{
		GrantKind:   req.GrantKind,
		GrantParams: req.GrantParams,
		Count:       req.Count,
		Prefix:      req.Prefix,
		SingleUse:   singleUse,
		MaxRedeems:  req.MaxRedeems,
		ExpiresAt:   req.ExpiresAt,
		BatchID:     req.BatchID,
		CreatedBy:   createdBy,
		Context:     IssuerContextAdminHTTP,
	}

	codes, err := h.issuer.Issue(c.Request.Context(), spec)
	if err != nil {
		h.handleIssueError(c, err)
		return
	}

	// Log the generation PII-safe: issuer (sanitized), count, grant_kind, batch.
	// NEVER the plaintext or hash. (The durable audit row is written in-tx by
	// the issuer; this log line is the operational breadcrumb.)
	h.log.Info("redemption codes generated",
		"issuer", sanitizeID(c.GetString("user_id")),
		"count", len(codes),
		"grant_kind", sanitizeID(req.GrantKind),
		"batch_id", sanitizeID(req.BatchID),
	)

	plaintexts := make([]string, len(codes))
	for i, c := range codes {
		plaintexts[i] = c.Plaintext
	}
	c.JSON(http.StatusCreated, gin.H{
		"count":      len(codes),
		"grant_kind": req.GrantKind,
		"batch_id":   req.BatchID,
		"codes":      plaintexts, // shown ONCE
	})
}

// handleIssueError maps issuer validation errors to 400 (operator misconfig)
// vs 500 (infra). The grant_kind-unsupported and expiry-required cases are
// operator-facing 400s with a specific message (this is an ADMIN endpoint, so a
// precise error is correct — no no-oracle concern on the issuer side).
func (h *Handler) handleIssueError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errIssueCountInvalid),
		errors.Is(err, errIssueCountTooLarge),
		errors.Is(err, errIssueGrantUnknown),
		errors.Is(err, errIssuePromoNoExpiry),
		errors.Is(err, errIssuePrefixTooLong),
		errors.Is(err, errIssueBatchIDTooLong):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		h.log.Error("redemption issue error", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "code generation failed"})
	}
}

// AdminGate is the issuer-authz middleware for the generation endpoint. It
// requires an X-Admin-Token header that constant-time-matches the configured
// adminToken. When adminToken is empty (unset), the endpoint is DISABLED (503)
// — a missing issuer secret must never default-allow generation.
//
// FLAGGED DESIGN GAP (issue #1303 §5): there is no platform-admin RBAC role in
// the codebase (Role is per-server only). This shared-secret gate is the
// closest existing privileged-auth primitive (mirrors the config-provisioned
// FEEDBACK_PAT / JWT_SECRET secret pattern). It is provisioned via the
// REDEMPTION_ADMIN_TOKEN env var, production-guarded at config load. A proper
// platform-admin role + portal is the deferred follow-on epic (spec §10).
func (h *Handler) AdminGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.adminToken == "" {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "code generation is not enabled on this server",
			})
			return
		}
		provided := c.GetHeader("X-Admin-Token")
		// Constant-time compare avoids a timing side-channel on the shared
		// secret. subtle.ConstantTimeCompare returns 1 only on equal length AND
		// equal bytes; unequal lengths short-circuit to 0 without leaking length
		// via timing beyond the (already public) header-present check.
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.adminToken)) != 1 {
			h.log.Warn("admin generation denied", "user_id", sanitizeID(c.GetString("user_id")))
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

// sanitizeID strips control characters from an id/label before it is logged
// (CWE-117 log-forging defense). Applied uniformly to logged user-derived
// strings — even structurally-safe uuids — per observability.md / #1645 (the
// type-based exemption was retired). Local to the package to avoid importing
// the websocket helper.
func sanitizeID(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
