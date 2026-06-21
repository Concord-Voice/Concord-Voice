package age

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Replay-guard sentinel errors. ErrUnavailable is the fail-closed signal (Redis
// error other than the NX-not-set sentinel) — the handler maps it to HTTP 503,
// never admits the claim.
var (
	ErrStaleTimestamp = errors.New("stale timestamp")
	ErrReplayedNonce  = errors.New("replayed nonce")
	ErrUnavailable    = errors.New("replay store unavailable")
)

const (
	// Asymmetric window: accept the full 300s into the past (legitimate client/
	// network latency) but only +30s of forward clock skew. A wide forward window
	// would let a client pre-mint long-lived claims.
	windowPast   = 300 * time.Second
	windowFuture = 30 * time.Second
	// nonceTTL must be >= windowPast+windowFuture so a claim can never outlive its
	// nonce — otherwise the nonce key would expire while the timestamp is still
	// in-window, reopening a replay seam. Locked by TestNonceTTLCoversWindow.
	nonceTTL = 360 * time.Second
)

// CheckTimestamp enforces the asymmetric freshness window. Pure — no I/O.
func CheckTimestamp(now time.Time, unixSecs int64) error {
	t := time.Unix(unixSecs, 0)
	if t.After(now.Add(windowFuture)) || t.Before(now.Add(-windowPast)) {
		return ErrStaleTimestamp
	}
	return nil
}

// ClaimNonce records single-use of (userID, nonce) via SET NX with a TTL.
//
//   - first use            → nil
//   - already claimed      → ErrReplayedNonce (the NX SET returned redis.Nil)
//   - any other redis error → ErrUnavailable  (FAIL CLOSED — a replay check has no
//     backstop, so a degraded Redis must reject, not admit)
//
// The key is scoped by userID so one user's nonce cannot block another's. There is
// deliberately NO delete-on-failure path: deleting a single-use token on an
// attacker-reachable error path would reopen the replay window. A failed downstream
// tx burns the nonce for <= nonceTTL; the client retries with a fresh nonce.
func ClaimNonce(ctx context.Context, rdb *redis.Client, userID, nonce string) error {
	key := "age_claim_nonce:" + userID + ":" + nonce
	_, err := rdb.SetArgs(ctx, key, "1", redis.SetArgs{TTL: nonceTTL, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		return ErrReplayedNonce
	}
	if err != nil {
		return ErrUnavailable
	}
	return nil
}
