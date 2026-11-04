package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The runner's invariants (#3680 PR3, Task 10; spec §5.1 as replaced by the plan's R1-R7).
//
//  1. Nothing is written, and no fence op is begun, for an outcome that has not first been
//     decided and covered in a transaction holding every lock it reads under. Every pass runs
//     the same prefix: lock_timeout, the login-family writer barrier, the users row at
//     FOR NO KEY UPDATE, then (with a session) the actor's family and the step-up subject, the
//     activation marker FOR SHARE, the address locks, the database clock, the snapshot, the
//     proof's revalidation, the policy. covers runs on every outcome that writes, before
//     sourceTx, before any write and before the fence's Begin (R6).
//  2. A rotating outcome is written only by pass 2, under a credepoch.Op begun before pass 2's
//     transaction opens, and pass 2's own decision is the one applied: pass 1's is discarded
//     with its transaction. A refusal on pass 2 stands. An outcome on pass 2 that writes but
//     does not rotate, or whose SetsPassword disagrees with the strategy pass 1 chose, breaks
//     the pass's assertion: the op is settled with Rollback, and the whole run is retried once
//     from pass 1, then refused as a contract error. So a non-rotating outcome never touches
//     the fence (no op, no Redis), and a rotating one is never committed without its op.
//  3. A password is written only under the forced clear (the strategy pass 1 picks when its
//     outcome sets one). An outcome that sets a password without rotating always reaches pass
//     2, where it breaks the assertion: it can never commit.
//  4. No Redis between BEGIN and COMMIT. Begin precedes BeginTx; Commit, Rollback and
//     ReconcileCommittedEviction run after the transaction settled, and how it settled picks
//     which one (the R4 table): a definite commit commits the op, a definite rollback rolls it
//     back, and anything else is ambiguous and reconciles. An ambiguous commit is never
//     reported as success, and it never mints a continuation.
//  5. The post-commit work (family close, the permission-generation bump, the continuation
//     mint) runs only where the commit may be durable, never fails the request, and logs a
//     fixed failure_class and nothing the request carried.
//
// Lock order (R5, with ruling 19's marker): SET LOCAL lock_timeout, the writer barrier, users,
// the actor's family, the activation marker FOR SHARE, the address advisory locks, the C4
// request rows, the factor rows, then in pass 2 the captured families, the ejection parent and
// the presence children. The marker's share lock makes an activation UPDATE queue behind every
// in-flight pass, and a pass behind the activation read the new value; the lock_timeout bounds
// the wait (55P03, lock_conflict). An activation that holds the marker and then waits on a
// users row a pass already holds is a deadlock PostgreSQL detects (40P01), not a hang.
//
// The callbacks run under these locks, so they owe the order too (ruling 30). Request.Revalidate
// (PR4/PR6) and the notice sink's InsertTx (PR4) may take only locks later in the R5 order than
// the factor rows: the challenge and outbox rows they own. They lock no users row, no family,
// no accountguard_state row, no factor table and no barrier, and they run no savepoint that
// spans the barrier or the users lock. Their tests carry that obligation; the runner cannot
// check it. Run's caller owes one more: Run must not be called while the caller holds any
// presencehistory sender gate (the gates are striped across users, so another user's gate can
// be req.UserID's). A password-setting outcome takes req.UserID's gate after the fence's
// blocked marker is published. The wait for it, and nothing after it, is bounded by
// decide.LockTimeout: a wait that runs out is a definite rollback (the op is rolled back and
// nothing is written) and Run returns ErrLockConflict, 503 lock_conflict.
//
// Errors. Run returns Result{} with every non-nil error. No error it returns prints a driver's
// or a callback's text: every such cause is wrapped in withheld, whose message is a fixed class
// while errors.Is and errors.As still reach the cause, so HTTPStatus and a caller's branch on a
// sentinel, a loginfamily class, a presencehistory.ServiceError or a SQLSTATE keep working.
//
// What breaks them: a write or a Begin moved above covers or above the refusal check; a pass-2
// refusal retried rather than returned; a retry that does not first Rollback the held op; a
// Redis call moved inside a transaction; a commit error classified as a rollback; the strategy
// read from pass 2's outcome rather than pass 1's (pass 2 then cannot catch a disagreement).

// PermBumper replaces a user's permission generation after a committed factor removal or
// contest (the #3453 factor hook). *rbac.Resolver satisfies it; the method name is the
// Resolver's on purpose, so the bare rbac.PermissionCache.BumpUser cannot be wired: it has
// no retry and no DropUserGeneration fallback, and a stale generation keeps a step-up grace
// taken before the removal matching.
type PermBumper interface {
	BumpUserPermissionGeneration(ctx context.Context, userID string) error
}

// ContinuationMinter mints the actor a fresh session bound to the epoch a committed rotation
// installed, in its own transaction (R2; precedent users.appendContinuationPair).
type ContinuationMinter interface {
	MintContinuation(ctx context.Context, userID uuid.UUID, epoch string) (Continuation, error)
}

// Continuation is the minted session. Its fields are opaque to accountguard.
type Continuation struct{ AccessToken, RefreshToken string }

// Deps are the runner's collaborators. DB, Fence and Presence are required: Run refuses a
// runner missing one with ErrContract before it opens anything. Keys resolves the fingerprint
// key id; a missing ring is ErrKeyUnavailable. Families, Perms and Minter serve the post-commit
// fast paths, and a missing one is logged at the step that needed it, never a failure of a
// committed request. Log defaults to a discarding logger.
type Deps struct {
	DB       *sql.DB
	Keys     *KeyRing
	Fence    *credepoch.Fence
	Presence *presencehistory.Service
	Families loginfamily.FamilyCloser
	Perms    PermBumper
	Minter   ContinuationMinter
	Log      *slog.Logger
}

// Request is one action to run. Session is the caller's verified access identity, nil for a
// worker; TokenEpoch is the epoch claim of that session's token. Revalidate verifies the
// caller's proof under the runner's locks and is called exactly once per pass, with the
// pass's database clock. PendingPasswordHash is the new password of a break_glass the action
// opens, and nothing else's.
type Request struct {
	UserID              uuid.UUID
	Session             *loginfamily.AccessIdentity
	TokenEpoch          string
	Act                 decide.Action
	Revalidate          func(ctx context.Context, tx *sql.Tx, now time.Time) (decide.Proof, error)
	PendingPasswordHash string
}

// Result is a run's outcome. A refusal is an outcome, returned with a nil error; its caller
// maps it. Continuation is set only after a definite commit of an outcome that asks for one.
type Result struct {
	Outcome      decide.Outcome
	Continuation *Continuation
}

// Runner is the package's transaction opener.
type Runner struct {
	d Deps
	// policy decides an action over the loaded snapshot. New installs the legacy stub.
	policy func(decide.Input) decide.Outcome
	// reg is the closed applier registry. New installs productionRegistry().
	reg registry
	// commitPlain commits the plain strategy's transaction; nil means tx.Commit. A test seam.
	commitPlain func(*sql.Tx) error
	// beforeCommit runs inside the transaction immediately before every commit attempt, plain
	// or forced; nil means none. A test seam.
	beforeCommit func()
}

// New is the production runner. Its policy is the legacy stub, which refuses every action
// with the zero outcome, so PR3 applies nothing in production; PR6 replaces it. A set
// activation marker refuses before any policy runs.
func New(d Deps) *Runner {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Runner{d: d, policy: legacyPolicy, reg: productionRegistry()}
}

// legacyPolicy is the stub PR6 replaces: the zero outcome, RowNone and VerdictRefused.
func legacyPolicy(decide.Input) decide.Outcome { return decide.Outcome{} }

// passAttempts is one attempt plus the one retry a broken pass-2 assertion earns (ruling 5).
const passAttempts = 2

// postCommitTimeout bounds the permission-generation bump, detached from the request so a
// client that disconnects cannot skip it.
const postCommitTimeout = 5 * time.Second

// The fixed failure classes the runner logs.
const (
	failureCommitUnknown   = "commit_unknown"
	failureFamilyClose     = "family_close_deferred"
	failurePermGen         = "permgen_bump_failed"
	failureContinuation    = "continuation_mint_failed"
	failurePassAssertion   = "pass2_assertion"
	failureRollback        = "rollback_failed"
	failureCompleteRefused = "complete_refused"
	logPostCommitDeferred  = "account guard post-commit step deferred"
)

// The runner's own statements. Every one is parameterized or takes no argument.
const (
	// lockTimeoutSQL is SET LOCAL lock_timeout in the form that takes a parameter: is_local
	// is true, so it ends with the transaction.
	lockTimeoutSQL = `SELECT set_config('lock_timeout', $1, true)`
	// activationSQL reads the singleton marker under a share lock held to the pass's end
	// (ruling 19), so the marker cannot change under a pass that read it. A missing row is an
	// error, never "not activated" (000189).
	activationSQL = `SELECT activated_at IS NOT NULL FROM accountguard_state WHERE singleton FOR SHARE`
	// clockSQL is the pass's one database clock.
	clockSQL = `SELECT clock_timestamp()`
	// addressLockSQL serializes the writers of one normalized address (spec §5.1 step 4). It
	// is the one place the key is derived, for the runner and for EnrolAtRegistrationTx. Its
	// argument is already folded by the application's normalizer (email.NormalizeAddress, in
	// lockAddressTx), and the SQL fold lower(btrim(...)) is the address id snapshot.go and the
	// reservation CHECK use, so any spelling of an address takes its one key, and an address
	// already normalized takes the key it always did.
	addressLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('concord.accountguard.address:' || lower(btrim($1::text)), 0))`
	// normalizedAddressesSQL is the lock order of a set of addresses, each already folded by
	// email.NormalizeAddress: each one's normalized key, distinct, ascending. PostgreSQL does
	// the final fold, the de-duplication and the order, so the order and the key addressLockSQL
	// derives from the same lower(btrim(...)) cannot drift apart, as a Go sort could. Ordering
	// is by the database's own collation, which every session of one database shares.
	normalizedAddressesSQL = `SELECT DISTINCT lower(btrim(a)) AS k FROM unnest($1::text[]) a ORDER BY k`
	// previousEpochSQL reads the epoch a rotation supersedes, under the users lock.
	previousEpochSQL = `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`
	// resetOverridePrefsSQL discards preference ciphertext encrypted under the key the old
	// password derived; it cascades to the materialized exceptions (precedent
	// auth.recoveryPresenceOverrideResetOps). Zero rows is a legitimate result.
	resetOverridePrefsSQL = `DELETE FROM presence_override_preferences WHERE user_id = $1`
)

// lockTimeoutSetting is decide.LockTimeout spelled as a lock_timeout value.
var lockTimeoutSetting = strconv.FormatInt(decide.LockTimeout.Milliseconds(), 10) + "ms"

// Run decides req's action and applies the outcome (R1-R5). It returns a refusal as a Result
// with a nil error. Every other failure is a non-nil error HTTPStatus maps, with Result{},
// including a commit whose outcome is unknown: the caller must never report a success it
// cannot prove. The error's text carries no driver or callback text (withheld).
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if err := r.ready(req); err != nil {
		return Result{}, err
	}
	keyID, err := r.d.Keys.CurrentKeyID(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("run: %w", withhold(err))
	}
	if req.Session != nil {
		// The step-up subject's family guard reads the signed identity from the context.
		ctx = loginfamily.WithAccessIdentity(ctx, *req.Session)
	}
	for range passAttempts {
		first, err := r.firstPass(ctx, req, keyID)
		if err != nil || !first.rotate {
			return r.settle(req, first.res, err)
		}
		res, violated, err := r.secondPass(ctx, req, keyID, first.forced)
		if !violated {
			return r.settle(req, res, err)
		}
		r.d.Log.Warn("account guard pass 2 broke its assertion; retrying from pass 1",
			"failure_class", failurePassAssertion, "user_id", req.UserID.String())
	}
	return Result{}, fmt.Errorf("run: %w: pass 2 broke its assertion on every attempt", ErrContract)
}

// settle is Run's single exit for a pass's verdict. An error returns Result{} (ruling 26), so a
// caller can never read an outcome beside a failure, marked with ErrLockConflict when it is
// one. A Complete refused at RowNone wedges its request, so each one is logged once with a
// fixed class and the user id only (decide/doc.go; ruling 28).
func (r *Runner) settle(req Request, res Result, err error) (Result, error) {
	if err != nil {
		return Result{}, withLockConflict(err)
	}
	if _, ok := req.Act.(decide.Complete); ok && res.Outcome.Row == decide.RowNone {
		r.d.Log.Warn("account guard refused a completion",
			"failure_class", failureCompleteRefused, "user_id", req.UserID.String())
	}
	return res, nil
}

// ready refuses a runner or a request that cannot run, before anything is opened.
func (r *Runner) ready(req Request) error {
	if r == nil || r.d.DB == nil || r.d.Fence == nil || r.d.Presence == nil || r.policy == nil || req.Revalidate == nil {
		return fmt.Errorf("run: %w: a dependency or the revalidator is missing", ErrContract)
	}
	return nil
}

// firstResult is pass 1's verdict. rotate means pass 2 must decide again and apply, and
// forced is the strategy pass 1's outcome chose for it.
type firstResult struct {
	res    Result
	rotate bool
	forced bool
}

// firstPass is pass 1: the plain strategy, with no fence op. A non-rotating outcome is
// applied and committed here. A rotating one, or one that sets a password, is covered and
// then rolled back before any write, so pass 2 can begin its op outside a transaction.
func (r *Runner) firstPass(ctx context.Context, req Request, keyID int16) (firstResult, error) {
	tx, err := r.d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return firstResult{}, fmt.Errorf("pass 1: begin: %w", withhold(err))
	}
	defer r.rollback(tx)
	d, err := r.decideTx(ctx, tx, req, keyID)
	if err != nil {
		return firstResult{}, fmt.Errorf("pass 1: %w", err)
	}
	o := d.a.outcome
	if refused(o) {
		return firstResult{res: Result{Outcome: o}}, nil
	}
	if err := r.reg.covers(d.a); err != nil {
		return firstResult{}, fmt.Errorf("pass 1: %w", err)
	}
	if o.RotateEpoch || o.SetsPassword {
		return firstResult{rotate: true, forced: o.SetsPassword}, nil // the deferred rollback is definite
	}
	if err := sourceTx(ctx, tx, o.Source); err != nil {
		return firstResult{}, fmt.Errorf("pass 1: %w", withhold(err))
	}
	if err := r.applyTx(ctx, tx, d.a); err != nil {
		return firstResult{}, fmt.Errorf("pass 1: %w", err)
	}
	res := Result{Outcome: o}
	if err := r.commitPlainTx(tx); err != nil {
		r.bumpPermGen(ctx, req.UserID, o)
		return firstResult{res: res}, r.commitUnknown(req.UserID, "pass 1", err)
	}
	r.bumpPermGen(ctx, req.UserID, o)
	return firstResult{res: res}, nil
}

// secondPass is pass 2. It begins the fence op before any transaction opens, then runs the
// plain strategy or, when pass 1's outcome set a password, the forced clear inside the
// presence sender gate. violated reports a broken pass-2 assertion, with the op settled.
func (r *Runner) secondPass(ctx context.Context, req Request, keyID int16, forced bool) (Result, bool, error) {
	op, err := r.d.Fence.Begin(ctx, req.UserID.String())
	if err != nil {
		return Result{}, false, fmt.Errorf("pass 2: fence: %w", withhold(err))
	}
	if !forced {
		return r.plainSecond(ctx, req, keyID, op)
	}
	var (
		res      Result
		violated bool
		started  bool
	)
	// The gates are striped across users, so the gate can be held by anyone. The wait for it is
	// bounded by decide.LockTimeout, and nothing after it is: readiness and the work run under
	// ctx.
	err = r.d.Presence.WithReadySenderModeWithin(ctx, req.UserID, presencehistory.ForcedSecurityClear, decide.LockTimeout, func() error {
		started = true
		var workErr error
		res, violated, workErr = r.forcedSecond(ctx, req, keyID, op)
		return workErr
	})
	if !started {
		op.Rollback(ctx) // definite: no transaction was opened
		if errors.Is(err, presencehistory.ErrSenderGateBusy) {
			return Result{}, false, fmt.Errorf("pass 2: presence sender gate: %w", ErrLockConflict)
		}
		return Result{}, false, fmt.Errorf("pass 2: presence readiness: %w", withhold(err))
	}
	return res, violated, err
}

// plainSecond is pass 2 under the plain strategy: Commit after a definite commit, Rollback
// after a definite rollback, ReconcileCommittedEviction after any commit error (R4).
func (r *Runner) plainSecond(ctx context.Context, req Request, keyID int16, op *credepoch.Op) (Result, bool, error) {
	tx, err := r.d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		op.Rollback(ctx)
		return Result{}, false, fmt.Errorf("pass 2: begin: %w", withhold(err))
	}
	defer r.rollback(tx)
	p, err := r.prepareSecondTx(ctx, tx, req, keyID, op, false)
	if err != nil || p.step != stepReady {
		r.rollback(tx) // settle the transaction before the fence's Redis call
		op.Rollback(ctx)
		return Result{Outcome: p.outcome}, p.step == stepViolated, err
	}
	res := Result{Outcome: p.outcome}
	commitErr := r.commitPlainTx(tx)
	// A commit that may be durable fences the captured families' sockets now.
	r.closeFamilies(ctx, req.UserID, p.owners)
	if commitErr != nil {
		op.ReconcileCommittedEviction(ctx)
		r.bumpPermGen(ctx, req.UserID, p.outcome)
		return res, false, r.commitUnknown(req.UserID, "pass 2", commitErr)
	}
	op.Commit(ctx)
	r.bumpPermGen(ctx, req.UserID, p.outcome)
	if p.outcome.Continuation {
		res.Continuation = r.mint(ctx, req.UserID, op.NewEpochValue())
	}
	return res, false, nil
}

// forcedSecond is pass 2 under the forced clear, committed by CompleteForcedSecurityClear.
// Its completion picks the fence call: Commit when acknowledged, Rollback when proven rolled
// back, ReconcileCommittedEviction otherwise; the families are closed iff it RequiresDisconnect
// (R4). It mints no continuation.
func (r *Runner) forcedSecond(ctx context.Context, req Request, keyID int16, op *credepoch.Op) (Result, bool, error) {
	tx, err := r.d.Presence.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		op.Rollback(ctx)
		return Result{}, false, fmt.Errorf("pass 2: begin: %w", withhold(err))
	}
	defer r.rollbackPresence(tx)
	p, err := r.prepareSecondTx(ctx, tx, req, keyID, op, true)
	if err != nil || p.step != stepReady {
		r.rollbackPresence(tx) // settle the transaction before the fence's Redis call
		op.Rollback(ctx)
		return Result{Outcome: p.outcome}, p.step == stepViolated, err
	}
	res := Result{Outcome: p.outcome}
	r.hookBeforeCommit()
	completion := r.d.Presence.CompleteForcedSecurityClear(ctx, tx, p.clear)
	if completion.RequiresDisconnect() {
		r.closeFamilies(ctx, req.UserID, p.owners)
	}
	switch {
	case completion.Err == nil:
		op.Commit(ctx)
		r.bumpPermGen(ctx, req.UserID, p.outcome)
		return res, false, nil
	case completion.Outcome == presencehistory.ForcedClearRolledBack:
		op.Rollback(ctx)
		return res, false, fmt.Errorf("pass 2: forced clear rolled back: %w",
			loginfamily.NewError(loginfamily.ReasonSQLUnavailable, withhold(completion.Err)))
	default:
		op.ReconcileCommittedEviction(ctx)
		r.bumpPermGen(ctx, req.UserID, p.outcome)
		return res, false, r.commitUnknown(req.UserID, "pass 2", completion.Err)
	}
}

// step is how far prepareSecondTx got. The zero step, a failure, commits nothing.
type step uint8

const (
	_ step = iota
	stepRefused
	stepViolated
	stepReady
)

// prepared is pass 2's applied, uncommitted transaction.
type prepared struct {
	step    step
	outcome decide.Outcome
	owners  []loginfamily.DeliveryOwner
	clear   presencehistory.ForcedClearResult
}

// prepareSecondTx decides again and, when pass 2's outcome holds the assertion, writes it in
// R5's order: sourceTx, the R2 rotation, the forced clear's audience step, the effects and
// notices, the override-preference reset when it sets a password. It never commits.
func (r *Runner) prepareSecondTx(ctx context.Context, tx *sql.Tx, req Request, keyID int16, op *credepoch.Op, forced bool) (prepared, error) {
	d, err := r.decideTx(ctx, tx, req, keyID)
	if err != nil {
		return prepared{}, fmt.Errorf("pass 2: %w", err)
	}
	p := prepared{outcome: d.a.outcome}
	o := p.outcome
	switch {
	case refused(o):
		p.step = stepRefused // pass 2 is authoritative: its refusal stands
		return p, nil
	case !o.RotateEpoch || o.SetsPassword != forced: // pragma: allowlist secret
		p.step = stepViolated
		return p, nil
	}
	if err := r.reg.covers(d.a); err != nil {
		return p, fmt.Errorf("pass 2: %w", err)
	}
	if err := sourceTx(ctx, tx, o.Source); err != nil {
		return p, fmt.Errorf("pass 2: %w", withhold(err))
	}
	if p.owners, err = rotateFamiliesTx(ctx, tx, req.UserID, d.user, op); err != nil {
		return p, fmt.Errorf("pass 2: rotate: %w", withhold(err))
	}
	if forced {
		if p.clear, err = r.d.Presence.BeginForcedSecurityClear(ctx, tx, req.UserID); err != nil {
			return p, fmt.Errorf("pass 2: forced clear: %w", withhold(err))
		}
	}
	if err := r.applyTx(ctx, tx, d.a); err != nil {
		return p, fmt.Errorf("pass 2: %w", err)
	}
	if o.SetsPassword {
		if _, err := tx.ExecContext(ctx, resetOverridePrefsSQL, req.UserID); err != nil {
			return p, fmt.Errorf("pass 2: reset presence overrides: %w", withhold(err))
		}
	}
	p.step = stepReady
	return p, nil
}

// decided is a pass's decision: the users authority it holds and the applier context of the
// policy's outcome.
type decided struct {
	user loginfamily.UserAuthority
	a    applyCtx
}

// decideTx runs a pass's prefix in tx and decides. The clock is read after the marker and
// immediately before loadInputTx (ruling 1), and the same instant is the snapshot's reopen
// window, Revalidate's clock and Input.Now. The row-lock waits inside loadInputTx can make
// it stale by at most decide.LockTimeout; every write stamps that same instant.
func (r *Runner) decideTx(ctx context.Context, tx *sql.Tx, req Request, keyID int16) (decided, error) {
	if _, err := tx.ExecContext(ctx, lockTimeoutSQL, lockTimeoutSetting); err != nil {
		return decided{}, fmt.Errorf("lock_timeout: %w", withhold(err))
	}
	user, err := lockAuthorityTx(ctx, tx, req)
	if err != nil {
		return decided{}, err
	}
	if err := refuseActivatedTx(ctx, tx); err != nil {
		return decided{}, err
	}
	// After users, before any C4 row (R5). PR3's wired set touches no address; C5 names them.
	if err := lockAddressesTx(ctx, tx, nil); err != nil {
		return decided{}, err
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, clockSQL).Scan(&now); err != nil {
		return decided{}, fmt.Errorf("clock: %w", withhold(err))
	}
	in, err := loadInputTx(ctx, tx, req.UserID, keyID, now)
	if err != nil {
		return decided{}, fmt.Errorf("snapshot: %w", withhold(err))
	}
	proof, err := req.Revalidate(ctx, tx, now)
	if err != nil {
		return decided{}, fmt.Errorf("revalidate: %w", withhold(err))
	}
	in.Act, in.Proof = req.Act, proof
	return decided{user: user, a: newApplyCtx(req, now, r.policy(in), in.Reqs)}, nil
}

// lockAuthorityTx is the R1 prefix, in its forced order: the writer barrier, the users row at
// FOR NO KEY UPDATE (the strongest mode the pass needs, so there is never an upgrade), and,
// with a session, the actor's family and then the step-up subject, which claims the barrier
// itself and so must follow it. A sessionless request (a worker) skips both session steps.
// No savepoint rollback may span the barrier or the users lock.
func lockAuthorityTx(ctx context.Context, tx *sql.Tx, req Request) (loginfamily.UserAuthority, error) {
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return loginfamily.UserAuthority{}, fmt.Errorf("writer barrier: %w", withhold(err))
	}
	set, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{req.UserID}, loginfamily.UserNoKeyUpdate)
	if err != nil {
		return loginfamily.UserAuthority{}, fmt.Errorf("lock user: %w", withhold(err))
	}
	user, err := set.ForUser(req.UserID)
	if err != nil {
		return loginfamily.UserAuthority{}, fmt.Errorf("lock user: %w", withhold(err))
	}
	if req.Session == nil {
		return user, nil
	}
	if _, err := loginfamily.GuardAfterUserLockTx(ctx, tx, user, *req.Session, loginfamily.FamilyNoKeyUpdate); err != nil {
		return loginfamily.UserAuthority{}, fmt.Errorf("guard family: %w", withhold(err))
	}
	if _, serr := stepup.LockSubjectTx(ctx, tx, req.UserID.String(), stepup.LockForNoKeyUpdate, req.TokenEpoch); serr != nil {
		return loginfamily.UserAuthority{}, fmt.Errorf("lock subject: %w", withhold(subjectError(serr)))
	}
	return user, nil
}

// subjectError classifies a step-up subject refusal and withholds it, so no text of stepup's
// reaches the error Run returns. Under the users lock its only 401s are a token epoch that no
// longer matches and a subject that is gone: both a denial of the session. Anything else stays
// in the chain for HTTPStatus: a 503 is service_unavailable, any other status a 500, and a
// connection-class Cause is the outage whatever the refusal's status (connectionOutage).
func subjectError(serr *stepup.Error) error {
	if serr.Status == http.StatusUnauthorized {
		return loginfamily.NewError(loginfamily.ReasonEpochMismatch, withhold(serr))
	}
	return withhold(serr)
}

// refuseActivatedTx reads the activation marker FOR SHARE, after the R1 prefix and before the
// address locks: set is ErrActivated, which every pass refuses before any policy runs (R6
// 2-B). A missing marker row is a contract error.
func refuseActivatedTx(ctx context.Context, tx *sql.Tx) error {
	var activated bool
	if err := tx.QueryRowContext(ctx, activationSQL).Scan(&activated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("activation marker: %w: no marker row", ErrContract)
		}
		return fmt.Errorf("activation marker: %w", withhold(err))
	}
	if activated {
		return fmt.Errorf("activation marker: %w", ErrActivated)
	}
	return nil
}

// lockAddressesTx takes the address advisory locks of addrs, each normalized address once, in
// ascending order of the normalized key. The order must be the key's, not the spelling's: two
// passes holding {"B@x", "a@x"} and {"a@x", "b@x"} lock the same two keys, and sorting the raw
// spellings would take them in opposite orders and deadlock. Each address is folded by the
// application's normalizer first (email.NormalizeAddress: Unicode white space, which btrim does
// not trim), then normalizedAddressesSQL derives, de-duplicates and orders the keys, and
// lockAddressTx re-normalizes each one (both folds are idempotent). The error carries no
// address.
func lockAddressesTx(ctx context.Context, tx *sql.Tx, addrs []string) error {
	if len(addrs) == 0 {
		return nil
	}
	folded := make([]string, len(addrs))
	for i, addr := range addrs {
		folded[i] = email.NormalizeAddress(addr)
	}
	keys, err := queryRows(ctx, tx, normalizedAddressesSQL, scanAddressKey, pq.Array(folded))
	if err != nil {
		return fmt.Errorf("address lock: %w", withhold(err))
	}
	for _, key := range keys {
		if err := lockAddressTx(ctx, tx, key); err != nil {
			return err
		}
	}
	return nil
}

// scanAddressKey reads one normalized address key.
func scanAddressKey(rows *sql.Rows) (string, error) {
	var key string
	if err := rows.Scan(&key); err != nil {
		return "", err
	}
	return key, nil
}

// lockAddressTx takes the advisory lock of one address, held to the transaction's end. The
// lock is on the normalized address, whatever spelling address is in: the application's
// normalizer, then addressLockSQL's SQL fold. The error carries no address.
func lockAddressTx(ctx context.Context, tx *sql.Tx, address string) error {
	if _, err := tx.ExecContext(ctx, addressLockSQL, email.NormalizeAddress(address)); err != nil {
		return fmt.Errorf("address lock: %w", withhold(err))
	}
	return nil
}

// newApplyCtx is the applier context of one pass (ruling 7): one new dispute group, one
// request id per EffectOpen index, a fresh password-write counter.
func newApplyCtx(req Request, now time.Time, o decide.Outcome, reqs []decide.Req) applyCtx {
	ids := make(map[int]uuid.UUID)
	for i, e := range o.Effects {
		if e.Kind == decide.EffectOpen {
			ids[i] = uuid.New()
		}
	}
	return applyCtx{
		userID: req.UserID, now: now, outcome: o, reqs: reqs,
		pendingPasswordHash: req.PendingPasswordHash, newGroupID: uuid.New(),
		requestIDs: ids, passwordWrites: new(int),
	}
}

// refused reports whether o writes nothing: a refused verdict, whatever else it carries, or an
// outcome with no effect, no notice and no flag (ruling 5). Such an outcome is rolled back
// and returned, never applied.
func refused(o decide.Outcome) bool {
	return o.Verdict == decide.VerdictRefused ||
		(len(o.Effects) == 0 && len(o.Notices) == 0 &&
			!o.RotateEpoch && !o.SetsPassword && !o.Continuation && !o.PermGenBump)
}

// applyTx applies a's effects in applyOrder, writes its notices through the sink, and holds
// the writes to the outcome's SetsPassword with finish. Any error means the caller must not
// commit.
func (r *Runner) applyTx(ctx context.Context, tx *sql.Tx, a applyCtx) error {
	for _, i := range applyOrder(a.outcome) {
		if err := r.reg.apply(ctx, tx, a, i); err != nil {
			// An applier may be a callback (PR6's resyncer), so its text is withheld too.
			return withhold(err)
		}
	}
	if len(a.outcome.Notices) > 0 {
		// covers has refused a nil sink already; this is the same check, behind it.
		if isNilInterface(r.reg.notices) {
			return fmt.Errorf("notices: %w: no notice sink", ErrEffectUnwired)
		}
		if err := r.reg.notices.InsertTx(ctx, tx, a.userID, a.requestIDs, a.outcome.Notices); err != nil {
			return fmt.Errorf("notices: %w", withhold(err))
		}
	}
	return r.reg.finish(a)
}

// rotateFamiliesTx is the R2 rotation (precedent auth.rotateRecoveryFamiliesTx): read the epoch
// the rotation supersedes, capture its families, stamp the op's new epoch, then terminalize
// the captured families under the epoch owner. It returns the owners the post-commit close
// needs. It does not commit.
func rotateFamiliesTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID, user loginfamily.UserAuthority, op *credepoch.Op) ([]loginfamily.DeliveryOwner, error) {
	var previous string
	if err := tx.QueryRowContext(ctx, previousEpochSQL, userID).Scan(&previous); err != nil {
		return nil, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, withhold(err))
	}
	targets, err := loginfamily.CaptureEpochTargetsTx(ctx, tx, user, previous)
	if err != nil {
		return nil, err
	}
	owner, err := op.RotateTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	revoked, err := loginfamily.RevokeFamiliesForEpochTx(ctx, tx, targets, owner)
	if err != nil {
		return nil, err
	}
	return revoked.Owners, nil
}

// commitPlainTx commits the plain strategy's transaction. Every commit error is ambiguous
// (the credepoch callers' classification): only a commit never attempted is a definite
// rollback.
func (r *Runner) commitPlainTx(tx *sql.Tx) error {
	r.hookBeforeCommit()
	if r.commitPlain != nil {
		return r.commitPlain(tx)
	}
	return tx.Commit()
}

func (r *Runner) hookBeforeCommit() {
	if r.beforeCommit != nil {
		r.beforeCommit()
	}
}

// commitUnknown logs a commit whose outcome is unknown and returns it as loginfamily's
// unknown-commit class, which HTTPStatus answers 503.
func (r *Runner) commitUnknown(userID uuid.UUID, pass string, err error) error {
	r.d.Log.Warn("account guard commit outcome unknown",
		"failure_class", failureCommitUnknown, "user_id", userID.String())
	return fmt.Errorf("%s: commit: %w", pass, loginfamily.NewError(loginfamily.ReasonCommitUnknown, withhold(err)))
}

// closeFamilies fences the sockets of the families a possibly-durable rotation made terminal.
// It reads committed rows only, so a commit that did not land closes nothing.
func (r *Runner) closeFamilies(ctx context.Context, userID uuid.UUID, owners []loginfamily.DeliveryOwner) {
	err := errUnwired
	if !isNilInterface(r.d.Families) {
		err = loginfamily.CloseCommittedFamilies(ctx, r.d.DB, r.d.Families, owners)
	}
	r.logDeferred(err, failureFamilyClose, userID)
}

// bumpPermGen runs the #3453 factor hook when the outcome removed or contested a factor.
func (r *Runner) bumpPermGen(ctx context.Context, userID uuid.UUID, o decide.Outcome) {
	if !o.PermGenBump {
		return
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postCommitTimeout)
	defer cancel()
	err := errUnwired
	if !isNilInterface(r.d.Perms) {
		err = r.d.Perms.BumpUserPermissionGeneration(bctx, userID.String())
	}
	r.logDeferred(err, failurePermGen, userID)
}

// mint asks for the continuation bound to the committed epoch. A failure degrades to a
// sign-in, never to a failed request.
func (r *Runner) mint(ctx context.Context, userID uuid.UUID, epoch string) *Continuation {
	if isNilInterface(r.d.Minter) {
		r.logDeferred(errUnwired, failureContinuation, userID)
		return nil
	}
	c, err := r.d.Minter.MintContinuation(ctx, userID, epoch)
	if err != nil {
		r.logDeferred(err, failureContinuation, userID)
		return nil
	}
	return &c
}

// errUnwired is the cause logDeferred records for a post-commit dependency left nil.
var errUnwired = errors.New("account guard: post-commit dependency unwired")

// logDeferred logs a failed post-commit step. cause_code is HTTPStatus's fixed code for the
// cause ("unwired" for a nil dependency), never the cause's text (ruling 27), so on-call can
// tell a store outage from an internal fault without a driver message reaching the log.
func (r *Runner) logDeferred(err error, class string, userID uuid.UUID) {
	if err == nil {
		return
	}
	code := "unwired"
	if !errors.Is(err, errUnwired) {
		_, code = HTTPStatus(err)
	}
	r.d.Log.Warn(logPostCommitDeferred, "failure_class", class, "cause_code", code, "user_id", userID.String())
}

// rollback ends a plain transaction if it is still open. A transaction that never committed
// is not durable whatever its rollback returns, so the error changes no outcome; it is logged
// and the server ends the transaction with its connection.
func (r *Runner) rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		r.d.Log.Warn("account guard rollback failed", "failure_class", failureRollback)
	}
}

// rollbackPresence ends a forced-clear transaction through the presence service's boundary.
func (r *Runner) rollbackPresence(tx *sql.Tx) {
	if err := r.d.Presence.RollbackTx(tx); err != nil && !errors.Is(err, sql.ErrTxDone) {
		r.d.Log.Warn("account guard rollback failed", "failure_class", failureRollback)
	}
}

// withheld keeps a driver's or a callback's error out of the text of the error Run returns
// (ruling 27): a pq error's message and detail can echo a parameter, and a callback's text is
// not this package's to vouch for. Its message is a fixed class, HTTPStatus's code for the
// cause; Unwrap keeps the cause in the chain, so every sentinel, class and SQLSTATE a caller
// branches on is still found by errors.Is and errors.As. Like loginfamily.Error, the cause is
// for control flow and must never be logged.
type withheld struct{ cause error }

// withhold wraps err, which every caller has already checked is non-nil, in withheld.
func withhold(err error) error { return &withheld{cause: err} }

func (w *withheld) Error() string {
	_, code := HTTPStatus(w.cause)
	return "cause withheld: " + code
}

func (w *withheld) Unwrap() error { return w.cause }

// withLockConflict marks a deadlock or lock-timeout failure with ErrLockConflict.
func withLockConflict(err error) error {
	if err != nil && !errors.Is(err, ErrLockConflict) && isLockConflict(err) {
		return fmt.Errorf("%w: %w", ErrLockConflict, err)
	}
	return err
}
