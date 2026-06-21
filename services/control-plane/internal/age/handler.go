package age

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// errAlreadyDisabled is returned by persist when the atomic disabled-guard on the
// UPSERT finds the account already disabled (the source-of-truth backstop for the
// case where the Redis denylist fast-path in isDisabled missed it). Mapped to 403.
var errAlreadyDisabled = errors.New("account already disabled")

// SessionDisconnector forcefully disconnects a user's live WebSocket sessions.
// Declared as an interface (satisfied by *websocket.Hub) to avoid an import cycle.
type SessionDisconnector interface {
	DisconnectUser(userID uuid.UUID)
}

// Handler serves PUT /api/v1/age/claim (#1623, child A).
type Handler struct {
	db    *sql.DB
	redis *redis.Client
	hub   SessionDisconnector
	log   *logger.Logger
	now   func() time.Time
}

// NewHandler builds the age-claim handler. now defaults to time.Now (overridable in tests).
func NewHandler(db *sql.DB, rdb *redis.Client, hub SessionDisconnector, log *logger.Logger) *Handler {
	return &Handler{db: db, redis: rdb, hub: hub, log: log, now: time.Now}
}

// claimRequest is the wire shape. user_id is NEVER accepted from the body — it is
// taken from the authenticated context and reconstructed into the signed canonical
// form. The forward-compat metadata fields (confidence, obligation_sources, assurance,
// attestation) are intentionally NOT accepted in child A.
type claimRequest struct {
	CanonicalVersion       int    `json:"canonical_version"`
	ValidAge               bool   `json:"valid_age"`
	NSFWAuth               bool   `json:"nsfw_auth"`
	JurisdictionObligation int    `json:"jurisdiction_obligation"`
	Nonce                  string `json:"nonce"`
	Timestamp              int64  `json:"timestamp"`
	KeyVersion             int    `json:"key_version"`
	ClientVersion          string `json:"client_version"`
	Signature              string `json:"signature"`
}

func fail(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{"error_code": code})
}

// SubmitClaim handles PUT /api/v1/age/claim. The step order is load-bearing (spec §4.3):
// the disabled-check precedes signature verification so a disabled account can never
// write a new claim or re-enable itself, regardless of key/signature validity.
func (h *Handler) SubmitClaim(c *gin.Context) {
	ctx := c.Request.Context()

	// 1. authenticated user.
	userID := c.GetString("user_id")
	if userID == "" {
		fail(c, http.StatusUnauthorized, "unauthenticated")
		return
	}

	// 2. disabled-check FIRST (fail-closed). A disabled user can never write a claim.
	disabled, err := h.isDisabled(ctx, userID)
	if err != nil {
		fail(c, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if disabled {
		fail(c, http.StatusForbidden, "account_disabled")
		return
	}

	// 3. bind + field-shape validation.
	var req claimRequest
	if c.ShouldBindJSON(&req) != nil {
		fail(c, http.StatusBadRequest, "malformed")
		return
	}
	claim := Claim{
		CanonicalVersion:       req.CanonicalVersion,
		UserID:                 userID, // authenticated identity, not body-supplied
		ValidAge:               req.ValidAge,
		NSFWAuth:               req.NSFWAuth,
		JurisdictionObligation: req.JurisdictionObligation,
		Nonce:                  req.Nonce,
		Timestamp:              req.Timestamp,
		KeyVersion:             req.KeyVersion,
		ClientVersion:          req.ClientVersion,
	}
	if claim.Validate() != nil {
		fail(c, http.StatusBadRequest, "malformed")
		return
	}

	// 4. verify signature against the user's CURRENT key.
	pub, keyErr := LoadCurrentKey(ctx, h.db, userID, claim.KeyVersion)
	switch {
	case errors.Is(keyErr, ErrNoSigningKey):
		fail(c, http.StatusUnprocessableEntity, "no_signing_key")
		return
	case errors.Is(keyErr, ErrStaleKeyVersion):
		fail(c, http.StatusUnprocessableEntity, "stale_key_version")
		return
	case keyErr != nil:
		fail(c, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if VerifySignature(pub, claim, req.Signature) != nil {
		fail(c, http.StatusUnprocessableEntity, "invalid_signature")
		return
	}

	// 5. timestamp window.
	if CheckTimestamp(h.now(), claim.Timestamp) != nil {
		fail(c, http.StatusUnprocessableEntity, "stale_timestamp")
		return
	}

	// 6. single-use nonce (fail-closed).
	switch nonceErr := ClaimNonce(ctx, h.redis, userID, claim.Nonce); {
	case errors.Is(nonceErr, ErrReplayedNonce):
		fail(c, http.StatusConflict, "replayed_nonce")
		return
	case nonceErr != nil:
		fail(c, http.StatusServiceUnavailable, "unavailable")
		return
	}

	// 7. persist (+ terminal disable if valid_age=false). NO nonce delete on failure.
	lastChange, persistErr := h.persist(ctx, claim, req.Signature)
	switch {
	case errors.Is(persistErr, errAlreadyDisabled):
		// Atomic source-of-truth backstop: the account was already disabled but the
		// step-2 denylist fast-path missed it (stale/missing key). Same reject as step 2.
		fail(c, http.StatusForbidden, "account_disabled")
		return
	case persistErr != nil:
		h.log.Error("age: persist failed", "error", persistErr)
		fail(c, http.StatusServiceUnavailable, "unavailable")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid_age":               claim.ValidAge,
		"nsfw_auth":               claim.NSFWAuth,
		"jurisdiction_obligation": claim.JurisdictionObligation,
		"last_change":             lastChange.UTC(),
	})
}

// isDisabled reads the immediate-effect denylist. The caller treats a Redis error as
// fail-closed (503); users.disabled is the source of truth, the denylist is the fast path.
func (h *Handler) isDisabled(ctx context.Context, userID string) (bool, error) {
	n, err := h.redis.Exists(ctx, middleware.UserDisabledKey(userID)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// persist upserts the single age_verification_records row and, when valid_age=false,
// terminally disables the account (spec §4.6): users.disabled + inlined refresh-token
// revoke in one tx, then a SYNCHRONOUS denylist set + live-session disconnect before
// returning. The denylist set completing before the 200 response is the invariant that
// closes the disable/refresh race together with the disabled=FALSE-guarded refresh path.
func (h *Handler) persist(ctx context.Context, claim Claim, sigB64 string) (time.Time, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Atomic disabled-guard + RETURNING. The `WHERE NOT EXISTS (... disabled=TRUE)`
	// makes the UPSERT a no-op for an account that is ALREADY disabled — the
	// source-of-truth backstop for the Redis fast-path in step 2 (a stale/missing
	// denylist key can no longer let a disabled user rewrite their record). It does
	// NOT block the disabling claim itself (a not-yet-disabled user passes the guard,
	// then the tx disables them below). RETURNING echoes the DB-authoritative
	// last_change so the response can't drift from the stored clock.
	var lastChange time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO age_verification_records
		  (user_id, valid_age, nsfw_auth, jurisdiction_obligation, client_signature, client_version, signature_key_version, canonical_version, last_change)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,NOW()
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE id=$1 AND disabled=TRUE)
		ON CONFLICT (user_id) DO UPDATE SET
		  valid_age=$2, nsfw_auth=$3, jurisdiction_obligation=$4, client_signature=$5,
		  client_version=$6, signature_key_version=$7, canonical_version=$8, last_change=NOW()
		RETURNING last_change`,
		claim.UserID, claim.ValidAge, claim.NSFWAuth, claim.JurisdictionObligation,
		sigB64, claim.ClientVersion, claim.KeyVersion, claim.CanonicalVersion).Scan(&lastChange)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errAlreadyDisabled // guard tripped — account already disabled
	}
	if err != nil {
		return time.Time{}, err
	}

	if !claim.ValidAge {
		if _, err = tx.ExecContext(ctx,
			`UPDATE users SET disabled=TRUE, disabled_reason='age_verification', disabled_at=NOW() WHERE id=$1`,
			claim.UserID); err != nil {
			return time.Time{}, err
		}
		// Inlined refresh-token revoke (revokeAllSessionsDB is a sessions.Handler
		// method on its own *sql.DB, not callable inside this tx). Same statement as
		// auth/handlers.go's ChangePassword / recovery-reset paths.
		if _, err = tx.ExecContext(ctx,
			`UPDATE refresh_tokens SET revoked_at=NOW() WHERE user_id=$1 AND revoked_at IS NULL`,
			claim.UserID); err != nil {
			return time.Time{}, err
		}
	}

	if err = tx.Commit(); err != nil {
		return time.Time{}, err
	}

	if !claim.ValidAge {
		// Detach the post-commit side effects from the REQUEST context. The disabling
		// actor controls the HTTP connection; a cancel between Commit and the denylist
		// Set would otherwise skip the immediate-effect key (the "synchronous before
		// 200" invariant). context.WithoutCancel keeps context values but drops the
		// request's cancellation/deadline; the timeout bounds a hung Redis.
		bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if setErr := h.redis.Set(bgCtx, middleware.UserDisabledKey(claim.UserID), "1", 0).Err(); setErr != nil {
			h.log.Error("age: denylist set failed", "error", setErr)
		}
		if uid, parseErr := uuid.Parse(claim.UserID); parseErr == nil {
			h.hub.DisconnectUser(uid)
		} else {
			// userID came from the auth middleware (always a valid UUID); a parse
			// failure here is a should-never-happen invariant breach — log, don't drop.
			h.log.Error("age: disconnect uuid parse failed", "error", parseErr)
		}
	}
	return lastChange, nil
}
