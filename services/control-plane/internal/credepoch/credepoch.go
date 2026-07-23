// Package credepoch owns the per-user credential-epoch fence (#2201): a durable
// users.credential_epoch column cached in Redis, verified identically by HTTP
// middleware, WS-ticket redemption, and WS-JWT upgrade, rotated by the four
// destructive credential flows, and recheckable inside sensitive-write
// transactions. No other package may hand-roll these key formats or semantics.
//
// Cache states for cred_epoch:<userID>:
//
//	active:<epoch>  — the user's current epoch (mirrors users.credential_epoch)
//	none            — the user has no epoch marker yet (column is NULL)
//	blocked:<opID>  — a destructive credential operation is in flight; fail closed
//
// The DB column is the source of truth. Cache misses read through and back-fill;
// Redis transport errors fall back to a direct DB read; only both stores failing
// rejects. The blocked marker's TTL plus read-through IS the crash/ambiguity
// reconciliation: after expiry the DB answers with whichever epoch actually
// committed. See [internal]specs/2026-07-22-2201-credential-epoch-design.md.
package credepoch

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyPrefix    = "cred_epoch:"
	valueActive  = "active:"
	valueNone    = "none"
	valueBlocked = "blocked:"
	// blockedTTL bounds crash recovery: a process that dies between Begin and
	// Commit/Rollback leaves the blocked marker to expire, after which
	// read-through converges on the committed DB state (spec §4.4 step 3c).
	blockedTTL = 5 * time.Minute
	// cacheTTL matches auth.AccessTokenTTL — a self-healing backstop for a
	// missed post-commit publish; steady-state correctness never depends on it.
	cacheTTL = 15 * time.Minute
	// redisOpTimeout mirrors the middleware blacklist/disabled check timeouts.
	redisOpTimeout = 2 * time.Second
	// writeOpTimeout bounds post-transaction publishes (age-handler template).
	writeOpTimeout = 5 * time.Second
)

var (
	// ErrBlocked rejects requests while a destructive credential operation is
	// in flight for the user (fail closed).
	ErrBlocked = errors.New("credential operation in flight")
	// ErrEpochMismatch rejects tokens minted under a superseded epoch, or
	// lacking an epoch claim after the user's first rotation (fail closed).
	ErrEpochMismatch = errors.New("credential epoch mismatch")
	// ErrUnavailable rejects when neither Redis nor the DB could answer
	// (fail closed).
	ErrUnavailable = errors.New("credential epoch unavailable")
)

// RowQuerier is the narrow DB surface the fence needs; satisfied by *sql.DB
// and *sql.Tx.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Logger is the narrow logging surface the fence needs (satisfied by
// *logger.Logger). Reject-path logs carry fixed reason enums only — never
// token material or epoch values (observability rules).
type Logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Fence verifies and mutates per-user credential-epoch state.
type Fence struct {
	db    RowQuerier
	redis *redis.Client
	log   Logger
}

// New constructs the process-wide Fence over the shared DB pool and Redis client.
func New(db RowQuerier, redisClient *redis.Client, log Logger) *Fence {
	return &Fence{db: db, redis: redisClient, log: log}
}

// Key returns the Redis key for a user's epoch state — the single source of
// truth for the format (mirrors middleware.UserDisabledKey).
func Key(userID string) string { return keyPrefix + userID }

// NewEpoch returns a fresh random epoch: 32 hex chars from 16 CSPRNG bytes.
// Random, not a counter — a stolen old token must not predict the next value.
func NewEpoch() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("credepoch: entropy: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Check enforces the verification algorithm (spec §4.3) for a request
// presenting tokenEpoch ("" = claim absent). Returns nil to admit;
// ErrBlocked / ErrEpochMismatch / ErrUnavailable to reject (all fail closed).
func (f *Fence) Check(ctx context.Context, userID, tokenEpoch string) error {
	authoritative, err := f.authoritativeEpoch(ctx, userID)
	if err != nil {
		return err
	}
	if authoritative == "" { // no epoch marker yet — pre-first-rotation
		return nil
	}
	if tokenEpoch == authoritative {
		return nil
	}
	return ErrEpochMismatch
}

// authoritativeEpoch resolves the user's current epoch ("" = none). Cache
// first; on miss (redis.Nil) it reads the DB and back-fills; on a Redis
// transport error it reads the DB directly (no back-fill — a SETNX during an
// outage window could race a Begin whose blocked marker failed to publish).
func (f *Fence) authoritativeEpoch(ctx context.Context, userID string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	val, rerr := f.redis.Get(rctx, Key(userID)).Result()
	switch {
	case rerr == nil:
		switch {
		case strings.HasPrefix(val, valueBlocked):
			return "", ErrBlocked
		case val == valueNone:
			return "", nil
		case strings.HasPrefix(val, valueActive):
			return strings.TrimPrefix(val, valueActive), nil
		}
		// Unrecognized value (external corruption / format skew): re-derive from
		// the DB and REPAIR the key with Set. backfill's SetNX is a no-op while
		// the bad value still occupies the key, so it would otherwise linger the
		// full TTL and force a DB round-trip for this user on every request.
		f.log.Warn("credepoch: unrecognized cache value; repairing from DB", "reason", "redis_corrupt")
		repairEpoch, repairErr := f.dbEpoch(ctx, userID)
		if repairErr != nil {
			f.log.Error("credepoch: read-through failed", "reason", "db_read")
			return "", ErrUnavailable
		}
		f.repair(ctx, userID, repairEpoch)
		return repairEpoch, nil
	case errors.Is(rerr, redis.Nil):
		// Cache miss — read through and back-fill below.
	default:
		dbEpoch, dbErr := f.dbEpoch(ctx, userID)
		if dbErr != nil {
			f.log.Error("credepoch: both stores unavailable", "reason", "redis_and_db")
			return "", ErrUnavailable
		}
		return dbEpoch, nil
	}

	dbEpoch, dbErr := f.dbEpoch(ctx, userID)
	if dbErr != nil {
		f.log.Error("credepoch: read-through failed", "reason", "db_read")
		return "", ErrUnavailable
	}
	f.backfill(ctx, userID, dbEpoch)
	return dbEpoch, nil
}

func (f *Fence) dbEpoch(ctx context.Context, userID string) (string, error) {
	var e sql.NullString
	if err := f.db.QueryRowContext(ctx,
		`SELECT credential_epoch FROM users WHERE id = $1`, userID,
	).Scan(&e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unknown user: no epoch marker. Other gates (signature, FK
			// integrity of the session) reject requests for deleted users.
			return "", nil
		}
		return "", err
	}
	if e.Valid {
		return e.String, nil
	}
	return "", nil
}

// cacheValue renders the cache string for an epoch ("" → the none marker).
func cacheValue(epoch string) string {
	if epoch == "" {
		return valueNone
	}
	return valueActive + epoch
}

// backfill caches a read-through result. SetNX so a concurrent Begin()'s
// blocked marker is never overwritten by a racing back-fill.
func (f *Fence) backfill(ctx context.Context, userID, epoch string) {
	wctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	if err := f.redis.SetNX(wctx, Key(userID), cacheValue(epoch), cacheTTL).Err(); err != nil {
		f.log.Warn("credepoch: cache back-fill failed", "reason", "redis_set")
	}
}

// repair overwrites a corrupt/unrecognized cache value with DB truth (Set, not
// SetNX — the point is to REPLACE the bad value, which SetNX won't do while the
// key exists). ponytail: accepts a microsecond Set-vs-Begin clobber race that
// needs external key corruption to coincide with a concurrent destructive flow
// on the same user; the DB read-through + GuardTx keep writes fail-closed.
func (f *Fence) repair(ctx context.Context, userID, epoch string) {
	wctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	if err := f.redis.Set(wctx, Key(userID), cacheValue(epoch), cacheTTL).Err(); err != nil {
		f.log.Warn("credepoch: cache repair failed", "reason", "redis_set")
	}
}

// commitScript / rollbackScript make the post-transaction marker updates
// operation-SCOPED so overlapping destructive flows for the same user cannot
// clobber each other's blocked marker (Codex/CodeRabbit #2397 review). ARGV[1]
// is this op's "blocked:<opID>" — only that exact value is ours to transition.
//
// commit: if we still own the marker → active:<newEpoch>; a NEWER op's blocked
// marker → leave untouched (stays fail-closed until it finalizes); anything else
// (a stale pre-rotation active: from a failed Begin, or a concurrent active:) →
// DEL to force a read-through to the authoritative DB epoch (subsumes the
// earlier A-fix that closed the failed-active-publish fail-open window).
var commitScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
  return 'set'
elseif v and string.sub(v, 1, 8) == 'blocked:' then
  return 'skip'
else
  redis.call('DEL', KEYS[1])
  return 'del'
end
`)

// rollback: DEL only if we still own the marker; a concurrent op's newer blocked
// marker must survive.
var rollbackScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 'del'
end
return 'skip'
`)

// Op is one destructive-flow fence operation (spec §4.4): Begin → RotateTx
// inside the flow's transaction → exactly one of Commit (definite commit) /
// Rollback (definite rollback) / neither (ambiguous commit — the blocked
// marker's TTL plus read-through reconciles).
type Op struct {
	f        *Fence
	userID   string
	opID     string
	newEpoch string
}

// NewEpochValue exposes the epoch this operation installs — used by recovery
// flows that execute SQL descriptors rather than callbacks, and by tests.
func (o *Op) NewEpochValue() string { return o.newEpoch }

// Begin publishes the blocked marker BEFORE the destructive transaction.
// A Redis failure is logged and tolerated: an outage must not block account
// recovery, and the transactional fence (GuardTx + post-commit DB state)
// still holds (spec §4.4 step 1).
func (f *Fence) Begin(ctx context.Context, userID string) (*Op, error) {
	opID, err := NewEpoch() // same entropy shape; opaque operation id
	if err != nil {
		return nil, err
	}
	newEpoch, err := NewEpoch()
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, writeOpTimeout)
	defer cancel()
	if err := f.redis.Set(wctx, Key(userID), valueBlocked+opID, blockedTTL).Err(); err != nil {
		f.log.Warn("credepoch: blocked-marker publish failed; durable fence still applies",
			"reason", "redis_set")
	}
	return &Op{f: f, userID: userID, opID: opID, newEpoch: newEpoch}, nil
}

// RotateTx stamps the new epoch inside the destructive flow's transaction.
// Callers MUST already hold the user-row lock (FOR NO KEY UPDATE).
func (o *Op) RotateTx(ctx context.Context, tx RowQuerier) error {
	var id string
	if err := tx.QueryRowContext(ctx,
		`UPDATE users SET credential_epoch = $1, updated_at = NOW() WHERE id = $2 RETURNING id`,
		o.newEpoch, o.userID,
	).Scan(&id); err != nil {
		return fmt.Errorf("credepoch: rotate: %w", err)
	}
	return nil
}

// Commit finalizes this op's blocked marker after a DEFINITE commit, atomically
// and operation-scoped (see commitScript). context.WithoutCancel so a canceled
// request context cannot strand the marker.
//
// On a transport error the CAS never executes, so its stale-value DEL branch
// does not run and a pre-rotation cache value (e.g. active:<old> / none left
// when Begin's publish also failed) can survive as a read-through-skipping hit,
// admitting the revoked epoch until the cache entry ages out. Retry the same
// idempotent, owner-scoped CAS once with a fresh context to cover a fast Redis
// recovery. A durable Redis outage spanning the whole flow cannot be fixed
// synchronously (no write reaches a down server) and falls back to the bounded
// <=cacheTTL residual documented in [internal]rules/backend.md — the DB stays
// authoritative and every sensitive write is still GuardTx-fenced. Rollback
// needs no such retry: a failed blocked-marker clear leaves it to its TTL, which
// is fail-CLOSED.
func (o *Op) Commit(ctx context.Context) {
	if o.runCommitScript(ctx) == nil {
		return
	}
	if err := o.runCommitScript(ctx); err != nil {
		o.f.log.Warn("credepoch: commit marker update failed after retry; TTL + read-through will converge",
			"reason", "redis_eval")
	}
}

func (o *Op) runCommitScript(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeOpTimeout)
	defer cancel()
	return commitScript.Run(wctx, o.f.redis, []string{Key(o.userID)},
		valueBlocked+o.opID, valueActive+o.newEpoch, cacheTTL.Milliseconds()).Err()
}

// Rollback clears this op's blocked marker after a DEFINITE rollback (only if we
// still own it — see rollbackScript); read-through restores the durable truth.
// After an AMBIGUOUS commit call neither Commit nor Rollback (spec §4.5).
func (o *Op) Rollback(ctx context.Context) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeOpTimeout)
	defer cancel()
	if err := rollbackScript.Run(wctx, o.f.redis, []string{Key(o.userID)}, valueBlocked+o.opID).Err(); err != nil {
		o.f.log.Warn("credepoch: rollback marker clear failed; blocked TTL will expire",
			"reason", "redis_eval")
	}
}

// GuardTx is the sensitive-write fence (spec §5): call inside the write's
// transaction BEFORE its first mutation. FOR SHARE conflicts with the
// destructive flows' FOR NO KEY UPDATE user-row lock, so a write racing a
// reset blocks until the reset commits, then reads the NEW epoch and aborts
// on mismatch. Locking the users row first also respects the canonical
// 000087 lock order (users → settings → pending → exceptions → history).
// tokenEpoch is the requester's cred_epoch claim ("" = absent).
func GuardTx(ctx context.Context, tx RowQuerier, userID, tokenEpoch string) error {
	var e sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`, userID,
	).Scan(&e); err != nil {
		return fmt.Errorf("credepoch: guard read: %w", err)
	}
	return MatchEpoch(e, tokenEpoch)
}

// MatchEpoch is the pure epoch-fence comparison shared by GuardTx and by
// callers that lock+read the users row themselves (e.g. a write that needs a
// stronger FOR NO KEY UPDATE lock than GuardTx's FOR SHARE): a NULL/empty
// stored epoch (never rotated) admits any token; otherwise the token's
// cred_epoch claim ("" = absent) must equal it. Returns ErrEpochMismatch on
// disagreement, nil to admit.
func MatchEpoch(storedEpoch sql.NullString, tokenEpoch string) error {
	if !storedEpoch.Valid || storedEpoch.String == "" {
		return nil
	}
	if tokenEpoch == storedEpoch.String {
		return nil
	}
	return ErrEpochMismatch
}
