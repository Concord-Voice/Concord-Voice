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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// errAlreadyDisabled is returned by persist when the atomic disabled-guard on the
// UPSERT finds the account already disabled (the source-of-truth backstop for the
// case where the Redis denylist fast-path in isDisabled missed it). Mapped to 403.
var errAlreadyDisabled = errors.New("account already disabled")

// SessionDisconnector forcefully disconnects a user's live WebSocket sessions.
// Declared as an interface (satisfied by *websocket.Hub) to avoid an import cycle.
// DisconnectFamilyContext fences each committed terminal family; DisconnectUser
// is the user-wide fallback when no exact family close can be proven.
type SessionDisconnector interface {
	DisconnectUser(userID uuid.UUID)
	loginfamily.FamilyCloser
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
	case errors.Is(persistErr, credepoch.ErrEpochMismatch):
		fail(c, http.StatusUnauthorized, "unauthenticated")
		return
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

// statusResponse is the wire shape for GET /api/v1/age/status. Identity-blind: it
// carries ONLY the verified-outcome booleans — never any DOB/age/jurisdiction-name
// value (none exist in age_verification_records by construction; ADR-0025).
type statusResponse struct {
	Verified bool `json:"verified"`
	ValidAge bool `json:"valid_age"`
	NSFWAuth bool `json:"nsfw_auth"`
}

// GetStatus handles GET /api/v1/age/status — the read-back companion to SubmitClaim
// (#1763). It lets the client rehydrate the verified state on mount instead of
// re-rendering the first-run DOB-entry form. Returns the authenticated user's durable
// outcome (the two eligibility booleans) from age_verification_records; no row →
// {verified:false}. The user_id is taken from the authenticated context, never the
// request — the row is strictly JWT-scoped, so a client cannot read another user's
// status. No DOB/age value is exposed because the schema stores none.
func (h *Handler) GetStatus(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	if userID == "" {
		fail(c, http.StatusUnauthorized, "unauthenticated")
		return
	}

	var validAge, nsfwAuth bool
	err := h.db.QueryRowContext(ctx,
		`SELECT valid_age, nsfw_auth FROM age_verification_records WHERE user_id = $1`,
		userID).Scan(&validAge, &nsfwAuth)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, statusResponse{Verified: false})
		return
	}
	if err != nil {
		fail(c, http.StatusServiceUnavailable, "unavailable")
		return
	}
	c.JSON(http.StatusOK, statusResponse{Verified: true, ValidAge: validAge, NSFWAuth: nsfwAuth})
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
// terminally disables the account (spec §4.6): users.disabled and captured family
// terminalization in one tx, then a synchronous denylist set before
// returning. The denylist set completing before the 200 response is the invariant that
// closes the disable/refresh race together with the disabled=FALSE-guarded refresh path.
func (h *Handler) persist(ctx context.Context, claim Claim, sigB64 string) (lastChange time.Time, err error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, rollbackErr))
			lastChange = time.Time{}
		}
	}()
	id, err := uuid.Parse(claim.UserID)
	if err != nil {
		return time.Time{}, loginfamily.NewError(loginfamily.ReasonAccessIdentity, err)
	}
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{id}, loginfamily.UserNoKeyUpdate)
	if err != nil {
		return time.Time{}, err
	}
	user, err := users.ForUser(id)
	if err != nil {
		return time.Time{}, err
	}
	// Session revocation rotates neither the credential epoch nor the claim
	// key, so revalidate the requesting login under the user lock: a family
	// revoked while this request waited must not disable the account.
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	if !ok || identity.UserID != id {
		return time.Time{}, credepoch.ErrEpochMismatch
	}
	if err = loginfamily.GuardLockedActorFamilyTx(ctx, tx, identity); err != nil {
		if errors.Is(err, loginfamily.ErrDenied) {
			return time.Time{}, errors.Join(credepoch.ErrEpochMismatch, err)
		}
		return time.Time{}, err
	}
	var disabled bool
	if err = tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id=$1`, id).Scan(&disabled); err != nil {
		return time.Time{}, err
	}
	if disabled {
		return time.Time{}, errAlreadyDisabled
	}
	var owners []loginfamily.DeliveryOwner
	if !claim.ValidAge {
		targets, err := loginfamily.CaptureUserTargetsTx(ctx, tx, user, nil)
		if err != nil {
			return time.Time{}, err
		}
		revoked, err := loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonDisablement})
		if err != nil {
			return time.Time{}, err
		}
		owners = revoked.Owners
	}

	// Atomic disabled-guard + RETURNING. The `WHERE NOT EXISTS (... disabled=TRUE)`
	// makes the UPSERT a no-op for an account that is ALREADY disabled — the
	// source-of-truth backstop for the Redis fast-path in step 2 (a stale/missing
	// denylist key can no longer let a disabled user rewrite their record). It does
	// NOT block the disabling claim itself (a not-yet-disabled user passes the guard,
	// then the tx disables them below). RETURNING echoes the DB-authoritative
	// last_change so the response can't drift from the stored clock.
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
		h.closeDisabledSessions(ctx, id, owners)
	}
	return lastChange, nil
}

// closeDisabledSessions fences the disabled user's sockets before the 200. The
// exact committed families are closed first; with no owner, or a failed exact
// close, the whole (now disabled) user is disconnected. Neither path fails the
// committed disablement, and the durable owner's reconciler stays the backstop.
func (h *Handler) closeDisabledSessions(ctx context.Context, userID uuid.UUID, owners []loginfamily.DeliveryOwner) {
	if h.hub == nil {
		return
	}
	if len(owners) > 0 {
		err := loginfamily.CloseCommittedFamilies(ctx, h.db, h.hub, owners)
		if err == nil {
			return
		}
		h.log.Warn("age: exact family close failed; disconnecting user")
	}
	h.hub.DisconnectUser(userID)
}
