package stepup

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The mint endpoint's refusal copy. ErrMsgInvalidPassword (stepup.go) is the
// wrong-password body, byte-identical to the seam's so the desktop's
// classifier needs no second string.
const (
	// ErrMsgInvalidStepUpPurpose refuses a purpose outside OwnRulePurposes.
	// It is mfa's errMsgInvalidInlinePurpose, the same refusal for the same
	// mistake at the WebAuthn begin route.
	ErrMsgInvalidStepUpPurpose = "A valid verification purpose is required"

	// ErrMsgStepUpLocked is the 423 body once the account's shared /login
	// lockout is engaged. The lockout is the account's, not this endpoint's,
	// so the copy names no endpoint.
	ErrMsgStepUpLocked = "Too many incorrect passwords. Try again later." // pragma: allowlist secret

	// errCodeAccountLocked is the error_code CompleteLink's 423 already sends.
	errCodeAccountLocked = "account_locked"

	// maxMintPasswordBytes bounds the credential handed to Argon2id, counted
	// in BYTES: it is a work bound on the hash input, not a password policy.
	maxMintPasswordBytes = 1 << 10

	// failureClassMintVerify is the mint's one fault log line.
	failureClassMintVerify = "step_up_password_verify_failed"
)

// PasswordVerifier verifies an account password against the SAME lockout
// /login and CompleteLink count against. *auth.Handler satisfies it. It
// returns auth.ErrAccountLocked before any hash work once the lockout is
// engaged, and auth.ErrInvalidCredentials, after counting the failure, for a
// wrong password; any other error is a lookup or hash fault. That error may
// carry hash-derived text, so it is never logged.
type PasswordVerifier interface {
	VerifyPassword(ctx context.Context, userID, password string) (credentialEpoch string, err error)
}

// PasswordTokenHandler serves POST /api/v1/auth/step-up/password: the one
// endpoint an own-rule route's password reaches (#3509, design spec
// "Developer decisions, 2026-10-01", T-2). It verifies the password and mints
// a single-use password token bound to one own-rule purpose, which the route
// then spends in its own transaction (VerifyOwnRuleTx).
//
// It must never be a cheaper password oracle than /login. The router puts
// AuthBanCheck and fail-closed per-IP and per-user limiters in front of it,
// the password is checked through the shared email-keyed lockout
// (PasswordVerifier), a wrong password feeds the shared per-IP auth-failure
// ban, and every verified, refused or locked-out attempt reaches Nightwatch
// exactly as /login's does.
type PasswordTokenHandler struct {
	db             *sql.DB
	passwords      PasswordVerifier
	redis          *redis.Client
	log            *logger.Logger
	securityEvents securityevent.Emitter
}

// NewPasswordTokenHandler builds the mint endpoint. redis is the client the
// per-IP auth-failure ban counts on; nil leaves that counter unrecorded, as it
// does at /login.
func NewPasswordTokenHandler(
	db *sql.DB, passwords PasswordVerifier, rdb *redis.Client, log *logger.Logger,
) *PasswordTokenHandler {
	return &PasswordTokenHandler{db: db, passwords: passwords, redis: rdb, log: log, securityEvents: securityevent.Discard}
}

// SetSecurityEvents injects the Nightwatch emitter. Nil keeps Discard.
// Telemetry never changes the response.
func (h *PasswordTokenHandler) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	h.securityEvents = events
}

// The mint's security events, the same shapes /login emits on
// RouteStepUpPassword. Each is dimension-free: no user, purpose or account
// state rides on it (observability.md principle 7).
var (
	mintRefusedEvent = securityevent.Event{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
		AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteStepUpPassword,
	}
	mintAccountLockedEvent = securityevent.Event{
		EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAccountLocked,
		AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteStepUpPassword,
	}
	mintSucceededEvent = securityevent.Event{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonAuthenticationSucceeded,
		AuthMethod: securityevent.AuthPassword, RouteTemplate: securityevent.RouteStepUpPassword,
	}
)

// emit records event and marks the request handled, so the router's
// Nightwatch observer does not add a route fallback for the same decision.
func (h *PasswordTokenHandler) emit(c *gin.Context, event securityevent.Event) {
	h.securityEvents.Emit(c.Request.Context(), event)
	middleware.MarkNightwatchHandled(c)
}

// passwordTokenRequest is the mint body. The password is read by this type and
// by nothing else; it never leaves the handler but to the verifier.
type passwordTokenRequest struct {
	CurrentPassword string  `json:"current_password"`
	Purpose         Purpose `json:"purpose"`
}

// MintPasswordToken answers, in order, each refusal before the next step runs:
//
//  1. 400 ErrMsgInvalidRequestBody: not one JSON object of at most 4 KiB, or a
//     non-string field.
//  2. 400 ErrMsgInvalidStepUpPurpose: a purpose outside OwnRulePurposes —
//     before the password is looked at.
//  3. 400 ErrMsgInvalidRequestBody: an empty password or one over 1 KiB.
//  4. 403 mfa_required with mfa_methods: an account with inline MFA (policy
//     P1) confirms these routes with MFA, never with a password — before the
//     password is verified.
//  5. 400 with the purpose's own NoFactors copy (OwnRuleCopy) and
//     step_up_unavailable: an account with
//     no usable password factor, the same answer the route itself gives —
//     before the password is verified.
//  6. 423 account_locked, or 403 ErrMsgInvalidPassword, from the shared
//     lockout; 500 on a verifier fault.
//  7. 401 from MintToken's session fence, or 500.
//
// Success is 200 {step_up_token, expires_in} with Cache-Control: no-store.
func (h *PasswordTokenHandler) MintPasswordToken(c *gin.Context) {
	userID := c.GetString("user_id")
	req, e := readPasswordTokenRequest(c)
	if e != nil {
		e.Write(c)
		return
	}
	ctx := c.Request.Context()
	if e := refuseSubject(ctx, h.db, userID, req.Purpose); e != nil {
		h.logCause(e)
		e.Write(c)
		return
	}
	if !h.verifyPassword(c, userID, req.CurrentPassword) {
		return
	}
	token, e := MintToken(ctx, h.db, userID, FactorPassword, req.Purpose, middleware.TokenCredentialEpoch(c))
	if e != nil {
		h.logCause(e)
		e.Write(c)
		return
	}
	h.emit(c, mintSucceededEvent)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"step_up_token": token, "expires_in": int(TokenTTL.Seconds())})
}

// readPasswordTokenRequest is refusals 1–3. The purpose is checked before the
// password field is, so an unknown purpose never reaches anything that
// examines the credential.
func readPasswordTokenRequest(c *gin.Context) (passwordTokenRequest, *Error) {
	var req passwordTokenRequest
	if present, e := readOneObject(c, &req); e != nil || !present {
		return passwordTokenRequest{}, invalidRequestBody()
	}
	if !req.Purpose.OwnRule() {
		return passwordTokenRequest{}, &Error{
			Status: http.StatusBadRequest, Body: gin.H{"error": ErrMsgInvalidStepUpPurpose},
		}
	}
	if req.CurrentPassword == "" || len(req.CurrentPassword) > maxMintPasswordBytes {
		return passwordTokenRequest{}, invalidRequestBody()
	}
	return req, nil
}

// refuseSubject is refusals 4 and 5, from one P1 read and before any password
// work: an account with an inline factor is refused with the factors it
// holds, and one with no usable password factor gets the route's own
// NoFactors 400 rather than a 500 from a verifier handed an empty hash. A
// failed read is a 500, never "no MFA".
func refuseSubject(ctx context.Context, q RowQuerier, userID string, purpose Purpose) *Error {
	subj, e := LoadSubject(ctx, q, userID)
	if e != nil {
		return e
	}
	if subj.MFAEnabled {
		return &Error{Status: http.StatusForbidden, Body: gin.H{
			"error": "MFA verification required", "mfa_required": true, "mfa_methods": subj.MFAMethods,
		}}
	}
	if subj.PasswordHash == "" {
		return noFactorsError(OwnRuleCopy(purpose).NoFactors)
	}
	return nil
}

// verifyPassword is refusal 6. It writes the refusal itself and reports
// whether the password verified. A wrong password is counted twice, as at
// /login: against the account's lockout (inside the verifier) and against the
// client IP's auth-failure ban here; it and the lockout each reach Nightwatch
// with /login's event shapes.
func (h *PasswordTokenHandler) verifyPassword(c *gin.Context, userID, password string) bool {
	_, err := h.passwords.VerifyPassword(c.Request.Context(), userID, password)
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrAccountLocked):
		h.emit(c, mintAccountLockedEvent)
		c.JSON(http.StatusLocked, gin.H{"error": ErrMsgStepUpLocked, "error_code": errCodeAccountLocked})
	case errors.Is(err, auth.ErrInvalidCredentials):
		outcome := middleware.RecordAuthFailure(c.Request.Context(), h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		h.emit(c, mintRefusedEvent)
		middleware.MarkAuthFailureOutcome(c, outcome)
		c.JSON(http.StatusForbidden, gin.H{"error": ErrMsgInvalidPassword})
	default:
		// err is deliberately not logged: a hash decode failure can embed the
		// hash (observability.md principle 1). The fixed class says which
		// stage broke.
		h.log.Error("Password step-up verification failed", "failure_class", failureClassMintVerify)
		c.JSON(http.StatusInternalServerError, gin.H{"error": ErrMsgVerificationFailed})
	}
	return false
}

// logCause logs a 5xx refusal's Cause. No Cause on this path is derived from
// the password: LoadSubject's and MintToken's are database errors.
func (h *PasswordTokenHandler) logCause(e *Error) {
	if e.Cause != nil {
		h.log.Error("Password step-up token mint failed", "error", e.Cause)
	}
}
