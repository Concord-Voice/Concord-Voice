package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lockout policy constants (#1688 §7). After lockoutThreshold consecutive
// failures the account/IP is locked for an exponentially growing window,
// 2^(n-threshold) minutes, capped at lockoutMaxWindow.
const (
	// lockoutThreshold is the number of consecutive failures tolerated before a
	// lock engages.
	lockoutThreshold = 5
	// lockoutBaseWindow is the window for the first lock (the threshold-th
	// failure): 2^0 * base = 1 minute.
	lockoutBaseWindow = time.Minute
	// lockoutMaxWindow caps the exponential growth so a determined attacker
	// cannot push the lock to an absurd duration that would also harm a
	// legitimate operator's recovery.
	lockoutMaxWindow = 60 * time.Minute
)

// Lockout key prefixes. The account key carries the operator handle directly
// (not PII — admin handles are not emails); the IP key carries a SHA-256 hash of
// the IP so the RAW IP is NEVER stored at rest (#1688 §7 / [internal]rules/observability.md).
const (
	lockoutAcctPrefix = "admin_lockout:acct:"
	lockoutIPPrefix   = "admin_lockout:ip:"
)

// Lockout enforces per-account AND per-IP exponential backoff in Redis. The two
// axes are independent: account lockout stops credential-stuffing one handle;
// IP lockout stops spraying many handles from one source. A login attempt is
// blocked if EITHER axis is locked. The clock is injectable for deterministic
// tests.
type Lockout struct {
	redis *redis.Client
	now   func() time.Time
}

// NewLockout wires a Lockout against Redis with an injectable clock. A nil clock
// defaults to time.Now (production).
func NewLockout(rdb *redis.Client, now func() time.Time) *Lockout {
	if now == nil {
		now = time.Now
	}
	return &Lockout{redis: rdb, now: now}
}

// hashIP returns the hex SHA-256 of an IP string. The raw IP is never stored —
// only this hash forms the Redis key suffix.
func hashIP(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}

// windowFor computes the lock window for a given consecutive-failure count.
// Below the threshold there is no lock (zero window). At and above it the window
// grows as 2^(count-threshold) * base, capped at lockoutMaxWindow.
func windowFor(count int64) time.Duration {
	if count < lockoutThreshold {
		return 0
	}
	exp := count - lockoutThreshold
	// Guard the shift: once 1<<exp * base would exceed the cap, return the cap.
	// 2^6 min = 64 min already exceeds the 60 min cap, so exp >= 6 saturates;
	// the explicit bound also prevents an overflowing shift on a huge count.
	if exp >= 6 {
		return lockoutMaxWindow
	}
	window := time.Duration(int64(1)<<uint(exp)) * lockoutBaseWindow
	if window > lockoutMaxWindow {
		return lockoutMaxWindow
	}
	return window
}

// RecordFailure increments both the per-account and per-IP failure counters and
// (re)sets each key's TTL to that axis's current lock window once the threshold
// is reached. Before the threshold, the counter is held for the base window so a
// burst of failures within that window accumulates (consecutive-failure
// semantics); the key self-expires after the window, resetting the counter.
func (l *Lockout) RecordFailure(ctx context.Context, username, ip string) error {
	if err := l.recordOne(ctx, lockoutAcctPrefix+username); err != nil {
		return fmt.Errorf("record account failure: %w", err)
	}
	if err := l.recordOne(ctx, lockoutIPPrefix+hashIP(ip)); err != nil {
		return fmt.Errorf("record ip failure: %w", err)
	}
	return nil
}

// recordOne increments a single counter key and sets its TTL to the lock window
// for the resulting count. The TTL is what enforces lock expiry: once it lapses,
// the counter is gone and the axis is unlocked again. Until the threshold, the
// TTL is the base window (so failures within it count as consecutive).
func (l *Lockout) recordOne(ctx context.Context, key string) error {
	count, err := l.redis.Incr(ctx, key).Result()
	if err != nil {
		return err
	}
	window := windowFor(count)
	if window <= 0 {
		// Pre-threshold: hold the counter for the base window so a slow drip of
		// failures still accumulates toward the lock.
		window = lockoutBaseWindow
	}
	if err := l.redis.Expire(ctx, key, window).Err(); err != nil {
		return err
	}
	return nil
}

// IsLocked reports whether the account OR the IP is currently locked, along with
// the retry-after duration (the larger of the two axes' remaining lock windows).
// A non-locked attempt returns (false, 0, nil).
func (l *Lockout) IsLocked(ctx context.Context, username, ip string) (bool, time.Duration, error) {
	acctLocked, acctRetry, err := l.axisLocked(ctx, lockoutAcctPrefix+username)
	if err != nil {
		return false, 0, fmt.Errorf("check account lock: %w", err)
	}
	ipLocked, ipRetry, err := l.axisLocked(ctx, lockoutIPPrefix+hashIP(ip))
	if err != nil {
		return false, 0, fmt.Errorf("check ip lock: %w", err)
	}
	locked := acctLocked || ipLocked
	retry := acctRetry
	if ipRetry > retry {
		retry = ipRetry
	}
	if !locked {
		return false, 0, nil
	}
	return true, retry, nil
}

// axisLocked reports whether a single counter key is at/over the threshold and
// the retry-after for that axis. retry-after is derived from windowFor(count) —
// the policy window for the current failure depth — which the Expire TTL
// mirrors; using the policy window keeps the value deterministic under the
// injected test clock (Redis TTL is wall-clock, independent of the clock seam).
func (l *Lockout) axisLocked(ctx context.Context, key string) (bool, time.Duration, error) {
	count, err := l.redis.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if count < lockoutThreshold {
		return false, 0, nil
	}
	return true, windowFor(count), nil
}

// Reset clears both the account and IP counters (called on a successful login).
// Deleting absent keys is a no-op, so a fresh login that never failed is safe.
func (l *Lockout) Reset(ctx context.Context, username, ip string) error {
	if err := l.redis.Del(ctx, lockoutAcctPrefix+username, lockoutIPPrefix+hashIP(ip)).Err(); err != nil {
		return fmt.Errorf("reset lockout: %w", err)
	}
	return nil
}
