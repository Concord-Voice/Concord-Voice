package servers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The "Enforce MFA On Dangerous Actions" setting (#3453):
//
//	GET /api/v1/servers/:id/mfa-enforcement
//	PUT /api/v1/servers/:id/mfa-enforcement
//
// This resource is the ONLY place servers.enforce_mfa_dangerous_actions
// reaches the wire. models.Server, every server response and server_updated
// never carry it (design L1), so no producer that reuses those shapes can leak
// it to a member who may not read it. Only the owner and holders of raw bit 62
// may read or change it (D4).

// mfaEnforcementStepUpPrefix keys the OFF confirmation's attempt budget. It is
// the MFA-settings routes' budget, not one of its own: the OFF confirmation
// verifies the same factor, so a separate budget would give a stolen owner or
// Administrator session five more guesses per window (PR #3464 review finding
// 8). Same bound, fail-closed posture and clear-after-verified-commit rule as
// every stepup.Budget.
const mfaEnforcementStepUpPrefix = stepup.MFASettingsBudgetPrefix

// maxMFAEnforcementRequestBytes caps the PUT body. The largest legal document
// is {"enabled":false,"mfa_code":"<256 characters>"}.
const maxMFAEnforcementRequestBytes = 1 << 10

// mfaEnforcementLockTimeout bounds every row-lock wait in the PUT. The pool
// sets no lock_timeout of its own, and this transaction holds the actor's users
// row while it waits on the servers row.
const mfaEnforcementLockTimeout = `SET LOCAL lock_timeout = '3s'`

// updateMFAEnforcementSQL leaves updated_at alone on purpose: every member reads
// updated_at through ListServers, so bumping it would tell members that the
// setting changed.
const updateMFAEnforcementSQL = `UPDATE servers SET enforce_mfa_dangerous_actions = $2 WHERE id = $1`

const (
	// errMsgMFAEnforcementForbidden is byte-identical to the body
	// rbac.RequirePermission writes, so a member who may not read or change
	// the setting cannot tell this route's refusal from any other permission
	// refusal.
	errMsgMFAEnforcementForbidden   = "Insufficient permissions"
	errMsgMFAEnforcementBodyTooBig  = "Request body too large"
	errMsgMFAEnforcementInvalidBody = "Invalid request body"
	errMsgMFAEnforcementFailed      = "Failed to update MFA enforcement"
	errMsgMFAEnforcementFetchFailed = "Failed to fetch MFA enforcement"
	errMsgMFAEnforcementBusy        = "The server is busy. Try again."
)

// The failure classes this route logs. mfa_gate_lock is the design's (§7); a
// 500 carries mfa_enforcement_internal plus the wrapped cause, whose prefix
// names the stage. Neither carries anything that differs between enrolled and
// unenrolled actors (I7).
const (
	failureClassMFAGateLock   = "mfa_gate_lock"
	failureClassMFAPermBump   = "perm_generation_bump"
	failureClassMFAEnforceErr = "mfa_enforcement_internal"
)

// errMFAEnforcementForbidden is the authorization refusal: neither the owner
// nor a holder of raw bit 62.
var errMFAEnforcementForbidden = errors.New("servers: not permitted to manage MFA enforcement")

// errMFAEnforcementRowsAffected reports an UPDATE that changed no row even
// though the row is locked, which cannot happen and therefore fails closed.
var errMFAEnforcementRowsAffected = errors.New("servers: MFA enforcement update affected no row")

// MFAVerifier is the MFA surface the OFF confirmation needs: exactly what
// mfaenforce.ConfirmTx takes (stepup.MFATxCodeVerifier). It is declared here,
// at the consumer, so this package does not import internal/mfa;
// *mfa.Handler satisfies it structurally.
type MFAVerifier interface {
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
	VerifyCodeTx(ctx context.Context, tx *sql.Tx, userID string, purpose stepup.Purpose, code string) (bool, error)
}

// SetMFAVerifier wires the verifier that checks the OFF confirmation. An
// unwired verifier fails closed (500 on every OFF), which is why the router
// boot guard asks HasMFAVerifier.
func (h *Handler) SetMFAVerifier(v MFAVerifier) { h.mfaVerifier = v }

// HasMFAVerifier reports whether SetMFAVerifier was called with a non-nil
// verifier. The router's boot guard interrogates the HANDLER through this.
func (h *Handler) HasMFAVerifier() bool { return h.mfaVerifier != nil }

// SetRedis wires the client behind the OFF confirmation's attempt budget. A nil
// client DENIES (503), which is safe but silent, so the boot guard asks
// HasRedis.
func (h *Handler) SetRedis(client *redis.Client) { h.redis = client }

// HasRedis reports whether SetRedis was called with a non-nil client.
func (h *Handler) HasRedis() bool { return h.redis != nil }

// SetSecurityEvents injects the Nightwatch emitter for the toggle's success
// events. A nil emitter discards.
func (h *Handler) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	h.securityEvents = events
}

// Test seams. Production never reassigns these; export_test.go is the only
// writer, and the package's tests run serially.
var (
	newMFAEnforcementBudget = func(rdb *redis.Client) stepup.Budget {
		return stepup.NewBudget(rdb, mfaEnforcementStepUpPrefix)
	}
	beginMFAEnforcementTx = func(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
		return db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	lockMFAEnforcementGate = mfaenforce.LockGateTx
	commitMFAEnforcementTx = func(tx *sql.Tx) error { return tx.Commit() }
)

// mfaEnforcementRequest is the PUT body. Enabled is a pointer so an absent
// field is a 400 rather than a silent false, which would be the OFF request.
type mfaEnforcementRequest struct {
	Enabled *bool  `json:"enabled" binding:"required"`
	MFACode string `json:"mfa_code" binding:"max=256"`
}

// GetMFAEnforcement returns the setting to the owner or a raw-bit
// Administrator. GET /api/v1/servers/:id/mfa-enforcement
//
// The Administrator check reads the raw role bits, never the masked resolver
// (I5): an unenrolled Administrator on an enforcing server is masked, and must
// still be able to read the setting that masks them.
func (h *Handler) GetMFAEnforcement(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	ctx := c.Request.Context()

	var ownerID string
	var enforcing bool
	err := h.db.QueryRowContext(ctx,
		`SELECT owner_id, enforce_mfa_dangerous_actions FROM servers WHERE id = $1`, serverID,
	).Scan(&ownerID, &enforcing)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
		return
	}
	if err != nil {
		h.log.Error(errMsgMFAEnforcementFetchFailed, "failure_class", failureClassMFAEnforceErr,
			"error", fmt.Errorf("read setting: %w", err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAEnforcementFetchFailed})
		return
	}
	if err := authorizeMFAEnforcement(ctx, h.db, ownerID, serverID, userID); err != nil {
		if errors.Is(err, errMFAEnforcementForbidden) {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgMFAEnforcementForbidden})
			return
		}
		h.log.Error(errMsgMFAEnforcementFetchFailed, "failure_class", failureClassMFAEnforceErr, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAEnforcementFetchFailed})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enforce_mfa_dangerous_actions": enforcing})
}

// authorizeMFAEnforcement admits the owner, or a member whose raw role bits
// carry PermAdministrator. q is the pool for GET and the write transaction for
// PUT. The owner id comes from the caller's read of the servers row (under the
// row lock, in the PUT).
func authorizeMFAEnforcement(ctx context.Context, q stepup.RowQuerier, ownerID, serverID, userID string) error {
	if ownerID == userID {
		return nil
	}
	raw, err := rbac.RawRolePermissions(ctx, q, serverID, userID)
	if err != nil {
		return fmt.Errorf("read administrator role bits: %w", err)
	}
	if raw&rbac.PermAdministrator == 0 {
		return errMFAEnforcementForbidden
	}
	return nil
}

// PutMFAEnforcement turns the setting on or off. PUT
// /api/v1/servers/:id/mfa-enforcement
//
// The order is the design's (§8) and each step is load-bearing:
//
//  1. The attempt budget, only for an OFF that carries a code, and BEFORE the
//     transaction: a rollback must not refund it, and a Redis round trip must
//     not pin a pooled connection. It is keyed on the actor, so a 429 here
//     tells the caller only about their own budget (L12).
//  2. A READ COMMITTED transaction with a 3 s lock_timeout.
//  3. mfaenforce.LockGateTx: the actor's users row FOR SHARE (this transaction
//     never writes users), then the servers row FOR NO KEY UPDATE, the lock the
//     UPDATE below takes anyway, so no lock is ever upgraded.
//  4. Authorization, BEFORE anything MFA-derived: a member who may not manage
//     the setting gets the generic 403 whether or not they are enrolled, and
//     learns nothing about the setting's value (I7).
//  5. The gate on the REQUESTED value, never on a computed transition (L6). ON
//     requires an enrolled actor; OFF requires a verified inline factor and
//     never succeeds without one (I8).
//  6. A request for the current value is a no-op only after passing step 5.
//  7. The UPDATE, which leaves updated_at alone.
//
// After a clean commit: clear the budget for a verified OFF, then, only when the
// value changed, bump the server's permission generation and emit the success
// event. A commit that reports an error still bumps whenever the UPDATE ran, and
// emits nothing (see applyMFAEnforcement).
func (h *Handler) PutMFAEnforcement(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	ctx := c.Request.Context()

	req, ok := bindMFAEnforcementRequest(c)
	if !ok {
		return
	}
	enabled := *req.Enabled

	budget := newMFAEnforcementBudget(h.redis)
	if !enabled && req.MFACode != "" {
		if e := budget.Consume(ctx, userID); e != nil {
			if e.Cause != nil {
				h.log.Error("MFA enforcement step-up budget unavailable", "error", e.Cause)
			}
			e.Write(c)
			return
		}
	}

	changed, err := h.applyMFAEnforcement(ctx, c, serverID, userID, enabled, req.MFACode)
	if err != nil {
		h.respondMFAEnforcementError(c, err)
		return
	}

	if !enabled {
		// Only a verified confirmation reaches here with enabled == false, so
		// this is the verified, committed success the budget's clear rule asks
		// for. Best-effort: a failure leaves the counter high, which fails
		// toward more limiting.
		if err := budget.Clear(ctx, userID); err != nil {
			h.log.Warn("Could not reset the MFA enforcement step-up budget after a verified change", "error", err)
		}
	}
	if changed {
		h.afterMFAEnforcementChange(c, serverID, enabled)
	}
	c.JSON(http.StatusOK, gin.H{"enforce_mfa_dangerous_actions": enabled})
}

// bindMFAEnforcementRequest applies the strict full-document read backend.md
// requires of a JSON body: MaxBytesReader alone lets the decoder stop after
// the first value, so the cached body is re-checked with json.Valid. 413 is
// decided before 400.
func bindMFAEnforcementRequest(c *gin.Context) (mfaEnforcementRequest, bool) {
	var req mfaEnforcementRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxMFAEnforcementRequestBytes)
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": errMsgMFAEnforcementBodyTooBig})
			return mfaEnforcementRequest{}, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgMFAEnforcementInvalidBody})
		return mfaEnforcementRequest{}, false
	}
	body, ok := c.Get(gin.BodyBytesKey)
	bodyBytes, bodyIsBytes := body.([]byte)
	if !ok || !bodyIsBytes || !json.Valid(bodyBytes) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgMFAEnforcementInvalidBody})
		return mfaEnforcementRequest{}, false
	}
	return req, true
}

// applyMFAEnforcement runs steps 2 to 7 and the commit. changed reports whether
// the UPDATE ran and committed. Every refusal and failure rolls back.
//
// A Commit() error does not prove a rollback: the server can apply the COMMIT
// and lose only its acknowledgement (spec correction C-3). So when the UPDATE
// ran, the server's permission generation is bumped whatever Commit returns.
// On a true rollback that costs one cache miss per member; skipping it after a
// commit that applied ON would serve every member's unmasked value for the
// cache TTL. The success event is not sent: that would claim a change the
// route cannot confirm.
func (h *Handler) applyMFAEnforcement(
	ctx context.Context, c *gin.Context, serverID, userID string, enabled bool, code string,
) (changed bool, err error) {
	tx, err := beginMFAEnforcementTx(ctx, h.db)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after a successful commit

	if _, err := tx.ExecContext(ctx, mfaEnforcementLockTimeout); err != nil {
		return false, fmt.Errorf("set lock timeout: %w", err)
	}
	gate, err := lockMFAEnforcementGate(ctx, tx, serverID, userID,
		stepup.LockForShare, mfaenforce.ServerForNoKeyUpdate, middleware.TokenCredentialEpoch(c))
	if err != nil {
		return false, err
	}
	if err := authorizeMFAEnforcement(ctx, tx, gate.OwnerID, serverID, userID); err != nil {
		return false, err
	}
	if e := h.confirmMFAEnforcementRequest(ctx, tx, gate.Subject, userID, enabled, code); e != nil {
		return false, e
	}

	if gate.Enforcing != enabled {
		res, err := tx.ExecContext(ctx, updateMFAEnforcementSQL, serverID, enabled)
		if err != nil {
			return false, fmt.Errorf("update setting: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return false, errors.Join(errMFAEnforcementRowsAffected, err)
		}
		changed = true
	}
	if err := commitMFAEnforcementTx(tx); err != nil {
		if changed {
			h.bumpMFAEnforcementGeneration(ctx, serverID)
		}
		return false, fmt.Errorf("commit: %w", err)
	}
	return changed, nil
}

// confirmMFAEnforcementRequest is step 5, on the REQUESTED value. ON needs an
// enrolled actor and no code: turning a control on must never cost more than
// using it. OFF is mfaenforce.ConfirmTx, which refuses an unenrolled actor with
// EnrollmentRequired and otherwise verifies the code on tx, so a backup code is
// spent only if the transaction commits (I8).
func (h *Handler) confirmMFAEnforcementRequest(
	ctx context.Context, tx *sql.Tx, subj stepup.Subject, userID string, enabled bool, code string,
) *stepup.Error {
	if enabled {
		if !subj.MFAEnabled {
			return stepup.EnrollmentRequired()
		}
		return nil
	}
	// A nil h.mfaVerifier converts to a nil interface, which ConfirmTx answers
	// with a 500: an unwired verifier fails closed.
	return mfaenforce.ConfirmTx(ctx, tx, subj, h.mfaVerifier, userID, stepup.PurposeServerMFAEnforcementOff, code)
}

// respondMFAEnforcementError maps a PUT error, in the order mfaenforce's
// package comment prescribes: a lock conflict first (it can arrive wrapped in
// a *stepup.Error), then a step-up refusal, then the not-found and forbidden
// sentinels, then 500. 4xx outcomes are not logged; nothing logged carries the
// actor's enrollment, the server's setting, or any part of the code (I7).
func (h *Handler) respondMFAEnforcementError(c *gin.Context, err error) {
	var stepErr *stepup.Error
	isStepErr := errors.As(err, &stepErr)
	if mfaenforce.IsLockConflict(err) {
		cause := err
		if isStepErr && stepErr.Cause != nil {
			cause = stepErr.Cause // a users-row timeout arrives inside a *stepup.Error
		}
		h.log.Warn(errMsgMFAEnforcementFailed, "failure_class", failureClassMFAGateLock, "error", cause)
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgMFAEnforcementBusy})
		return
	}
	if isStepErr {
		if stepErr.Cause != nil {
			h.log.Error(errMsgMFAEnforcementFailed, "failure_class", failureClassMFAEnforceErr, "error", stepErr.Cause)
		}
		stepErr.Write(c)
		return
	}
	switch {
	case errors.Is(err, mfaenforce.ErrServerNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
	case errors.Is(err, errMFAEnforcementForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgMFAEnforcementForbidden})
	default:
		h.log.Error(errMsgMFAEnforcementFailed, "failure_class", failureClassMFAEnforceErr, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAEnforcementFailed})
	}
}

// bumpMFAEnforcementGeneration bumps the server's permission generation so the
// next read on any member is fresh (I6). It is detached from the request,
// because a client that hangs up must not leave it undone. Both callers reach
// it only after the transaction's outcome is settled or unknowable: after a
// clean commit, or after a commit error (see applyMFAEnforcement).
func (h *Handler) bumpMFAEnforcementGeneration(ctx context.Context, serverID string) {
	if err := h.resolver.BumpServerPermissionGeneration(context.WithoutCancel(ctx), serverID); err != nil {
		h.log.Error("Failed to refresh cached permissions after an MFA enforcement change",
			"failure_class", failureClassMFAPermBump, "error", err)
	}
}

// afterMFAEnforcementChange runs after a commit that changed the value: bump
// the server's permission generation so the next read on any member is fresh
// (I6), then record the success for Nightwatch. Both are detached from the
// request, because a client that hangs up must not leave either undone.
//
// The success reason names the direction. That is the server's setting, not
// anyone's enrollment: both directions imply an enrolled actor, and every
// refusal on this route reaches Nightwatch as the one privileged_route_denied
// fallback, so no refusal is ever told apart from another (I7).
func (h *Handler) afterMFAEnforcementChange(c *gin.Context, serverID string, enabled bool) {
	ctx := context.WithoutCancel(c.Request.Context())
	h.bumpMFAEnforcementGeneration(ctx, serverID)
	event := securityevent.Event{
		EventType:     securityevent.EventPrivilegedAction,
		Outcome:       securityevent.OutcomeSuccess,
		Severity:      securityevent.SeverityInformational,
		ReasonCode:    securityevent.ReasonServerMFAEnforcementEnabled,
		AuthMethod:    securityevent.AuthSession,
		RouteTemplate: securityevent.RouteServerMFAEnforcement,
	}
	if !enabled {
		event.Severity = securityevent.SeverityMedium
		event.ReasonCode = securityevent.ReasonServerMFAEnforcementDisabled
	}
	if h.securityEvents != nil {
		h.securityEvents.Emit(ctx, event)
	}
	middleware.MarkNightwatchHandled(c)
}
