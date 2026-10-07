package mfaenforce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gates (#3454) compose the primitives in this file with
// LockGateTx. A route runs, in order:
//
//  1. stepup.Charge and stepup.GraceStore.Read, both BEFORE BeginTx: neither
//     Redis call may sit inside a gate transaction (A-2, A-9);
//  2. its transaction, which takes LockGateTx (WithGateTx opens one for the
//     families that have none), then its OWN permission check (I7), then
//     Require;
//  3. after COMMIT, Settle with Require's outcome.
//
// Errors from steps 1 and 2 are answered by WriteError.

// Outcome is what Require decided. Only Verified settles anything.
type Outcome int

const (
	// Unconfirmed means the server does not enforce, or the route's predicate did
	// not fire. Nothing was asked of the actor.
	Unconfirmed Outcome = iota
	// Verified means a factor was verified on the transaction. After commit the
	// budget is cleared and a grace granted (Settle).
	Verified
	// GraceCovered means a valid grace confirmed the action with no verifier call.
	// Settle grants nothing, so the grace never slides.
	GraceCovered
)

// Confirmed reports whether the action was confirmed, by a factor or by
// grace. It is the provenance a purge records as confirmed (A-3.6).
func (o Outcome) Confirmed() bool { return o == Verified || o == GraceCovered }

// Confirm is what one D1 gate confirms with.
type Confirm struct {
	// Verifier checks the code on the gate's transaction. Nil is a 500 when
	// the gate fires, never a skipped check.
	Verifier stepup.MFATxCodeVerifier
	ActorID  string
	// Purpose is the route's own purpose: a WebAuthn token is spent only
	// under it, and only a grace-eligible purpose can be covered by grace.
	Purpose stepup.Purpose
	// Code is the request's mfa_code. A step_up_token is never read: it can
	// only carry a password, which no D1 gate accepts.
	Code string
	// Grace is the route's pre-read (stepup.GraceStore.Read), judged here
	// under the server rule.
	Grace stepup.GraceRead
}

// Require is a D1 gate's confirmation, run on the gate's transaction after
// LockGateTx and after the route's own permission check (I7). fires is the
// route's predicate: true for an unconditional D1 action, and the
// prior-state verdict (shortening a retention, granting a dangerous bit) for
// a conditional one.
//
// It returns Unconfirmed when g does not enforce or fires is false, without
// consulting the grace or the verifier; GraceCovered when in.Grace covers
// in.Purpose under the server rule; otherwise ConfirmTx's verdict, Verified
// on success. A code sent alongside a valid grace is ignored, and the charge
// it cost stands. Require makes no Redis call and sets no timeout: its only
// waits are row locks, which the transaction's lock_timeout bounds.
func Require(ctx context.Context, tx *sql.Tx, g Gate, in Confirm, fires bool) (Outcome, *stepup.Error) {
	if !g.Enforcing || !fires {
		return Unconfirmed, nil
	}
	if in.Grace.Covers(in.Purpose, stepup.GraceServerRule, g.Subject) {
		return GraceCovered, nil
	}
	if e := ConfirmTx(ctx, tx, g.Subject, in.Verifier, in.ActorID, in.Purpose, in.Code); e != nil {
		return Unconfirmed, e
	}
	return Verified, nil
}

// Logger is the slice of pkg/logger these helpers write to; *logger.Logger
// satisfies it. Declared here so the leaf's import allowlist stays closed.
type Logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Fixed failure classes. None names a branch I7 keeps indistinguishable: a
// grace-covered and a verified action log nothing different on success.
const (
	// FailureClassGateLock is a lock conflict answered with WriteBusy. Same
	// value as the toggle's.
	FailureClassGateLock = "mfa_gate_lock"
	// FailureClassGateError is any other gate failure that has a cause.
	FailureClassGateError = "mfa_gate_failed"
	// FailureClassGateBudgetClear is a failed post-commit budget clear.
	FailureClassGateBudgetClear = "mfa_gate_budget_clear_failed"
)

const logGateFailed = "Dangerous-action gate failed"

// ErrMsgGateFailed is the 500 body WriteError sends for an unclassified error.
const ErrMsgGateFailed = "Internal server error"

// Settle runs a D1 gate's post-commit writes. Call it only after the
// transaction that produced outcome COMMITTED. It does nothing unless outcome
// is Verified: then, on stepup.SettleContext, it clears the budget and grants
// an mfa grace from the route's pre-read. Both are best-effort; a failure is a
// Warn with a fixed failure_class and changes no response.
func Settle(
	ctx context.Context, log Logger, outcome Outcome, budget stepup.Budget, store stepup.GraceStore,
	read stepup.GraceRead, actorID string,
) {
	if outcome != Verified {
		return
	}
	sctx, cancel := stepup.SettleContext(ctx)
	defer cancel()
	if err := budget.Clear(sctx, actorID); err != nil {
		log.Warn("Could not reset the step-up budget after a verified gate", "failure_class", FailureClassGateBudgetClear, "error", err)
	}
	if err := store.Grant(sctx, read, stepup.GraceStrengthMFA); err != nil {
		log.Warn("Could not record a step-up grace after a verified gate", "failure_class", stepup.FailureClassGraceGrant, "error", err)
	}
}

// WriteError answers an error from Charge, LockGateTx, Require or
// WithGateTx, in the package comment's order:
//
//  1. IsLockConflict: WriteBusy, with a Warn. It comes first because a
//     users-row lock timeout arrives inside a *stepup.Error's Cause.
//  2. *stepup.Error: its own status and body, logged at Error only when it
//     carries a Cause (a 4xx refusal is an outcome, not a fault).
//  3. ErrServerNotFound: serverGone, the route's own answer for a server that
//     vanished mid-request (#3508 made it route-specific).
//  4. Anything else, or ErrServerNotFound with a nil serverGone: 500.
//
// A route classifies its own sentinels before calling this. A route with its
// own 500 copy calls it only when IsGateError(err), and writes its own 500
// for the rest, so the generic body in step 4 never replaces the route's.
func WriteError(c *gin.Context, log Logger, err error, serverGone func(*gin.Context)) {
	var stepErr *stepup.Error
	isStepErr := errors.As(err, &stepErr)
	if IsLockConflict(err) {
		cause := err
		if isStepErr && stepErr.Cause != nil {
			cause = stepErr.Cause
		}
		log.Warn(logGateFailed, "failure_class", FailureClassGateLock, "error", cause)
		WriteBusy(c)
		return
	}
	if isStepErr {
		if stepErr.Cause != nil {
			log.Error(logGateFailed, "failure_class", FailureClassGateError, "error", stepErr.Cause)
		}
		stepErr.Write(c)
		return
	}
	if serverGone != nil && errors.Is(err, ErrServerNotFound) {
		serverGone(c)
		return
	}
	log.Error(logGateFailed, "failure_class", FailureClassGateError, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": ErrMsgGateFailed})
}

// IsGateError reports whether err is one WriteError answers with the gate's
// own status and body (steps 1-3): a lock conflict, a *stepup.Error, or
// ErrServerNotFound, wrapped or not. Anything else is the route's own failure,
// which WriteError would answer with a generic 500.
func IsGateError(err error) bool {
	var stepErr *stepup.Error
	return IsLockConflict(err) || errors.As(err, &stepErr) || errors.Is(err, ErrServerNotFound)
}

// GateSpec is the gate a WithGateTx transaction takes.
type GateSpec struct {
	ServerID, ActorID string
	UserLock          stepup.Lock
	ServerLock        ServerLock
	// TokenEpoch is the session's cred_epoch claim.
	TokenEpoch string
	// HoldsExternalWrite also sets idle_in_transaction_session_timeout = '10s',
	// for a transaction held open across a write to a store that cannot roll
	// back (the server icon and banner uploads, #3454 C3), so a stalled
	// handler cannot pin the servers row past it.
	HoldsExternalWrite bool
}

// Each bound is its own const statement, never built from input.
const (
	gateLockTimeoutSQL = `SET LOCAL lock_timeout = '3s'`
	gateIdleTimeoutSQL = `SET LOCAL idle_in_transaction_session_timeout = '10s'`
)

// WithGateTx runs fn in a new READ COMMITTED transaction gated by spec: it
// sets lock_timeout (I-BOUND), optionally the idle bound, takes LockGateTx,
// runs fn, and commits. Any error rolls back. It serves only the families
// with no transaction of their own (UpdateServer and the two uploads, A-7);
// a route that already has a transaction calls LockGateTx inside it.
//
// fn receives the Gate and must run the route's permission check before
// Require (I7). An error from fn is returned unwrapped, so a route's own
// sentinels survive; a commit failure is wrapped. The rollback is
// unconditional (a no-op after a commit), so a panic in fn still releases the
// gate's locks rather than leaving them held until the context ends. A
// rollback that itself fails is joined onto the returned error, never
// discarded; errors.Is and errors.As still find the original.
func WithGateTx(ctx context.Context, db *sql.DB, spec GateSpec, fn func(*sql.Tx, Gate) error) (err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("mfaenforce: begin gate transaction: %w", err)
	}
	defer func() {
		// sql.ErrTxDone is the committed (or already rolled back) case.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("mfaenforce: roll back gate transaction: %w", rbErr))
		}
	}()
	if _, err := tx.ExecContext(ctx, gateLockTimeoutSQL); err != nil {
		return fmt.Errorf("mfaenforce: set lock_timeout: %w", err)
	}
	if spec.HoldsExternalWrite {
		if _, err := tx.ExecContext(ctx, gateIdleTimeoutSQL); err != nil {
			return fmt.Errorf("mfaenforce: set idle_in_transaction_session_timeout: %w", err)
		}
	}
	g, err := LockGateTx(ctx, tx, spec.ServerID, spec.ActorID, spec.UserLock, spec.ServerLock, spec.TokenEpoch)
	if err != nil {
		return err
	}
	if err := fn(tx, g); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mfaenforce: commit gate transaction: %w", err)
	}
	return nil
}
