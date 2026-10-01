package stepup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// DeleteSoftLockThreshold and DeleteSoftLockWindow are const, never
// configuration — a configurable security bound is one more place it can be
// wrong ([internal]rules/backend.md). Together they mean: more than 15
// authorized delete attempts by one population member inside a rolling 30s
// window require MFA confirmation or a wait. See design spec §2.1 — this does
// NOT bound a paced script (RateLimitByUser(10, 1m) is the sustained-rate
// control); it bounds a burst that straddles the route limiter's fixed 60s
// window, where a burst can reach 20.
const (
	DeleteSoftLockThreshold = 15
	DeleteSoftLockWindow    = 30 * time.Second
)

// DeleteSoftLockDayThreshold and DeleteSoftLockDayWindow are the sustained
// tier (design spec "Developer decisions" § D-2, X25): more than 100
// authorized delete attempts by one user, across ALL scopes, inside a rolling
// 24h window require confirmation. This is what bounds a paced script: at
// ≤10/min the route limiter (RateLimitByUser(10, 1m)) never puts 16 deletes
// into one 30s burst window, but it also never trips on its own — the day
// tier is what caps that script at 100 unconfirmed deletes per user per 24h.
const (
	DeleteSoftLockDayThreshold = 100
	DeleteSoftLockDayWindow    = 24 * time.Hour
)

// ErrMsgDeleteGuardUnavailable is the 503 body when the soft-lock cannot be
// evaluated at all (Redis refusing writes, or unwired). It names no
// population and no setting (H3): a member outside either population must
// not be able to tell this response apart from one that could, in principle,
// affect them.
const ErrMsgDeleteGuardUnavailable = "Deleting messages is temporarily unavailable. Try again in a moment."

var errDeleteSoftLockUnwired = errors.New("delete soft-lock has no Redis client")

// softLockScopeKind is the closed set of populations a soft-lock counter can
// be scoped to. Unexported: a caller builds a SoftLockScope only through
// DMDeleteScope or ServerDeleteScope, never by hand.
type softLockScopeKind int

const (
	softLockScopeDM softLockScopeKind = iota
	softLockScopeServer
)

// SoftLockScope names which population counter a Hit or Reset applies to.
// Build one with DMDeleteScope or ServerDeleteScope.
type SoftLockScope struct {
	kind     softLockScopeKind
	serverID uuid.UUID
}

// DMDeleteScope is the population counter for DM message deletes.
func DMDeleteScope() SoftLockScope {
	return SoftLockScope{kind: softLockScopeDM}
}

// ServerDeleteScope is the population counter for channel message deletes on
// an enforcing server. serverID is a uuid.UUID, so a raw c.Param value is a
// compile error rather than a runtime one — the row read must parse it first.
func ServerDeleteScope(serverID uuid.UUID) SoftLockScope {
	return SoftLockScope{kind: softLockScopeServer, serverID: serverID}
}

// SoftLockVerdict is Hit's outcome for one authorized delete attempt.
type SoftLockVerdict struct {
	// Over is true once the count for this window exceeds
	// DeleteSoftLockThreshold.
	Over bool
	// RetryAfter is how long the caller should suggest waiting. It is
	// meaningful only when Over is true.
	RetryAfter time.Duration
}

// DeleteSoftLock is the delete-rate soft-lock counter (design spec §2.3,
// D-2). It lives in this package, not mfaenforce, because mfaenforce's
// import allowlist forbids go-redis.
//
// HitN is one TxPipelined MULTI/EXEC covering both tiers: the scope's burst
// key gets INCRBY n; EXPIRE NX 30s; PTTL, and the user's day key (shared
// across every scope) gets the same three ops with a 24h window. NX sets a
// TTL only on a key that has none, so a TTL-less key (e.g. one whose EXPIRE
// was lost to a prior partial failure) is repaired on the very next hit, and
// a refused hit never extends either window — both properties come from the
// same primitive, EXPIRE with NX, rather than from an extra branch.
// Atomicity (one MULTI/EXEC) is what gives N concurrent requests arriving at
// burst count 15 the distinct values 16..15+N: there is no separate
// check-then-act step to overshoot.
//
// Any Redis error — including a Redis 6 backend, where EXPIRE ... NX is
// unsupported and aborts the EXEC — denies with a 503 whose Cause is set for
// the caller to log. HitN never returns Over=false on a Redis failure: a
// counter that silently no-ops when Redis is unavailable is the exact
// fail-open this type exists to prevent.
type DeleteSoftLock struct {
	rdb *redis.Client
}

// NewDeleteSoftLock binds the soft-lock to its Redis client. A nil client is
// allowed and DENIES: every Hit returns 503 rather than silently never
// refusing (the same posture as Budget).
func NewDeleteSoftLock(rdb *redis.Client) DeleteSoftLock {
	return DeleteSoftLock{rdb: rdb}
}

// Key is the one place a burst-tier key is spelled, so HitN and Reset cannot
// disagree about it. userID comes from the auth context; a server scope's
// serverID comes from the DB row read for that request. Neither key holds a
// message id or any message content.
func (l DeleteSoftLock) Key(userID uuid.UUID, s SoftLockScope) string {
	if s.kind == softLockScopeServer {
		return fmt.Sprintf("stepup:delete_softlock:%s:server:%s", userID, s.serverID)
	}
	return fmt.Sprintf("stepup:delete_softlock:%s:dm", userID)
}

// DayKey is the one place the sustained-tier key is spelled. Unlike Key, it
// does not vary with s: it is one counter per user, shared across every
// scope, by design (D-2) — a paced script that spreads its deletes across
// many DMs or servers must still trip this tier.
func (l DeleteSoftLock) DayKey(userID uuid.UUID) string {
	return fmt.Sprintf("stepup:delete_softlock:%s:day", userID)
}

// Hit is HitN with n=1: it records one authorized delete attempt.
func (l DeleteSoftLock) Hit(ctx context.Context, userID uuid.UUID, s SoftLockScope) (SoftLockVerdict, *Error) {
	return l.HitN(ctx, userID, s, 1)
}

// HitN records n authorized delete attempts at once (see design spec §2.7:
// an authorized attempt is the counted unit, not a completed deletion — a 404
// or 500 raised inside the transaction still counts) and reports whether
// either tier is over threshold. n>1 exists for a bounded self-purge count
// (D-1): the purge handler runs one COUNT before its gate transaction and
// charges the whole batch in a single HitN rather than looping Hit.
func (l DeleteSoftLock) HitN(ctx context.Context, userID uuid.UUID, s SoftLockScope, n int64) (SoftLockVerdict, *Error) {
	if l.rdb == nil {
		return SoftLockVerdict{}, deleteSoftLockUnavailable(errDeleteSoftLockUnwired)
	}

	burstKey := l.Key(userID, s)
	dayKey := l.DayKey(userID)

	var burstIncr, dayIncr *redis.IntCmd
	var burstPTTL, dayPTTL *redis.DurationCmd
	_, err := l.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		burstIncr = p.IncrBy(ctx, burstKey, n)
		p.ExpireNX(ctx, burstKey, DeleteSoftLockWindow)
		burstPTTL = p.PTTL(ctx, burstKey)

		dayIncr = p.IncrBy(ctx, dayKey, n)
		p.ExpireNX(ctx, dayKey, DeleteSoftLockDayWindow)
		dayPTTL = p.PTTL(ctx, dayKey)
		return nil
	})
	if err != nil {
		return SoftLockVerdict{}, deleteSoftLockUnavailable(fmt.Errorf("delete soft-lock hit: %w", err))
	}

	burstCount, err := burstIncr.Result()
	if err != nil {
		return SoftLockVerdict{}, deleteSoftLockUnavailable(fmt.Errorf("delete soft-lock hit: %w", err))
	}
	burstTTL, err := burstPTTL.Result()
	if err != nil {
		return SoftLockVerdict{}, deleteSoftLockUnavailable(fmt.Errorf("delete soft-lock hit: %w", err))
	}
	dayCount, err := dayIncr.Result()
	if err != nil {
		return SoftLockVerdict{}, deleteSoftLockUnavailable(fmt.Errorf("delete soft-lock hit: %w", err))
	}
	dayTTL, err := dayPTTL.Result()
	if err != nil {
		return SoftLockVerdict{}, deleteSoftLockUnavailable(fmt.Errorf("delete soft-lock hit: %w", err))
	}

	burstOver := burstCount > DeleteSoftLockThreshold
	dayOver := dayCount > DeleteSoftLockDayThreshold

	verdict := SoftLockVerdict{Over: burstOver || dayOver}
	switch {
	case burstOver && dayOver:
		verdict.RetryAfter = maxDuration(
			deleteSoftLockRetryAfter(burstTTL, DeleteSoftLockWindow),
			deleteSoftLockRetryAfter(dayTTL, DeleteSoftLockDayWindow),
		)
	case burstOver:
		verdict.RetryAfter = deleteSoftLockRetryAfter(burstTTL, DeleteSoftLockWindow)
	case dayOver:
		verdict.RetryAfter = deleteSoftLockRetryAfter(dayTTL, DeleteSoftLockDayWindow)
	}

	return verdict, nil
}

// deleteSoftLockRetryAfter is ceil(pttl), with a 1s minimum, falling back to
// that tier's own window when pttl is at or below zero (no TTL yet visible,
// or a clock/replication edge). Rounding up rather than down never tells a
// caller to retry a moment before the key has actually expired.
func deleteSoftLockRetryAfter(pttl, window time.Duration) time.Duration {
	if pttl <= 0 {
		return window
	}
	secs := int64(math.Ceil(pttl.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// SettleTimeout bounds the post-commit writes of a verified confirmation —
// DeleteSoftLock.Reset and Budget.Clear — on the context SettleContext gives
// them.
const SettleTimeout = 2 * time.Second

// SettleContext is the context for a verified confirmation's post-commit
// writes: detached from the request, because a client that hangs up just
// after its delete commits would otherwise cancel exactly the writes that
// record the confirmation, and bounded by SettleTimeout, because detached
// alone it would wait on a stalled Redis for as long as the client would
// have.
func SettleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), SettleTimeout)
}

// Reset clears both the scope's burst counter and the user's day counter,
// in one DEL, after a verified, committed delete. It is best-effort by
// design, mirroring Budget.Clear: the caller logs a failure at Warn and
// continues. A successful Reset discards concurrent in-flight INCRs — a
// small accepted fail-open bounded by in-flight requests, a just-verified
// MFA factor, and the existing 10/min route limiter (design spec §2.7, RS4).
// A nil client has nothing to reset.
func (l DeleteSoftLock) Reset(ctx context.Context, userID uuid.UUID, s SoftLockScope) error {
	if l.rdb == nil {
		return nil
	}
	if err := l.rdb.Del(ctx, l.Key(userID, s), l.DayKey(userID)).Err(); err != nil {
		return fmt.Errorf("reset delete soft-lock: %w", err)
	}
	return nil
}

func deleteSoftLockUnavailable(cause error) *Error {
	return &Error{
		Status: http.StatusServiceUnavailable,
		Body:   gin.H{"error": ErrMsgDeleteGuardUnavailable},
		Cause:  cause,
	}
}
