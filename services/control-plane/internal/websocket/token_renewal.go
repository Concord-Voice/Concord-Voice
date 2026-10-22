package websocket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// In-band control-socket token renewal. A socket loses authority at the exp of
// the access token that opened it (#3895). The desktop rotates that token
// every 14 minutes, so without renewal every idle socket reconnected on that
// cadence. A renewal frame carries the rotated token over the open socket;
// the deadline moves only when the token passes the same gate as the
// handshake AND belongs to this socket's user, credential epoch and refresh
// lineage. Any failure leaves the old deadline in force, and the socket then
// closes and reconnects exactly as before. Spec:
// [internal]specs/2026-10-22-control-ws-token-renewal-design.md.

const (
	authRenewMessageType = "auth_renew"

	// tokenRenewalTimeout bounds every round-trip of one renewal (blacklist,
	// live state, epoch fence, lineage, revocation re-check). It runs on the
	// socket's read goroutine, so this is also the longest a renewal can stall
	// reads; pongWait (60s) and the client's 60s refresh lead absorb it. The
	// Redis calls honour it only because the production client sets
	// ContextTimeoutEnabled (internal/database/redis.go).
	tokenRenewalTimeout = 3 * time.Second

	// tokenRenewalMinInterval caps renewal attempts per socket. Rotation is
	// every 14 minutes, so a legitimate client never comes close.
	tokenRenewalMinInterval = 10 * time.Second
)

// Constant acknowledgement frames. No failure reason is sent: the client
// cannot act on one, and a reason would be an oracle. Keep the shape in sync
// with AuthRenewResultSchema in client/desktop/src/renderer/types/ws-events.ts.
var (
	authRenewAcceptedFrame = []byte(`{"type":"auth_renew_result","data":{"ok":true}}`)
	authRenewRejectedFrame = []byte(`{"type":"auth_renew_result","data":{"ok":false}}`)
)

// tokenRenewalAuthority is the narrow slice of Handler a socket needs to
// renew. nil on a Client disables renewal (fail closed).
type tokenRenewalAuthority interface {
	validateAccessToken(ctx context.Context, token string) (accessTokenAuth, error)
	sessionPredecessor(ctx context.Context, userID uuid.UUID, sessionID string) (string, error)
	sessionRevoked(ctx context.Context, userID uuid.UUID, sessionID string) (bool, error)
}

// sessionRebind asks Hub.Run to move a client to its successor refresh
// session. SessionID is read unlocked on Run by DisconnectSession, so only Run
// may write it once the client is registered.
type sessionRebind struct {
	client    *Client
	sessionID string
	done      chan struct{}
}

var errSessionNotFound = errors.New("session not found")

// sessionPredecessor returns the refresh session a live (unrevoked) session
// rotated from. Every refresh mints a new sid whose predecessor_id is the old
// one, so this is the only link between the socket's sid and a rotated token.
func (h *Handler) sessionPredecessor(ctx context.Context, userID uuid.UUID, sessionID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", errSessionNotFound
	}
	var predecessor sql.NullString
	err = h.db.QueryRowContext(ctx,
		`SELECT predecessor_id FROM refresh_tokens WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sid, userID).Scan(&predecessor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("session lineage lookup: %w", err)
	}
	if !predecessor.Valid {
		return "", nil
	}
	return predecessor.String, nil
}

// sessionRevoked reports whether a refresh session is revoked. A missing row
// counts as revoked; a non-uuid sid is revoked with errSessionNotFound.
func (h *Handler) sessionRevoked(ctx context.Context, userID uuid.UUID, sessionID string) (bool, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return true, errSessionNotFound
	}
	var revoked bool
	err = h.db.QueryRowContext(ctx,
		`SELECT revoked_at IS NOT NULL FROM refresh_tokens WHERE id = $1 AND user_id = $2`,
		sid, userID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("session revocation lookup: %w", err)
	}
	return revoked, nil
}

// tokenDeadline is the socket's current authority deadline.
func (c *Client) tokenDeadline() time.Time {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	return c.TokenExpiresAt
}

// extendDeadline moves the deadline to exp. It refuses to shorten, to revive a
// deadline that has already passed (a close may be under way), and to touch a
// socket whose expiry timer was stopped by teardown.
func (c *Client) extendDeadline(exp time.Time) bool {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.tokenExpiryStopped || !time.Now().Before(c.TokenExpiresAt) || !exp.After(c.TokenExpiresAt) {
		return false
	}
	c.TokenExpiresAt = exp
	return true
}

// deadlineAccepts reports whether extendDeadline(exp) would succeed now. It is
// the cheap refusal before any rebind; extendDeadline stays authoritative.
func (c *Client) deadlineAccepts(exp time.Time) bool {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	return !c.tokenExpiryStopped && time.Now().Before(c.TokenExpiresAt) && exp.After(c.TokenExpiresAt)
}

// stopTokenExpiry stops the expiry timer for teardown. Once stopped, a timer
// that already fired cannot re-arm.
func (c *Client) stopTokenExpiry() {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.tokenExpiryStopped = true
	if c.tokenExpiryTimer != nil {
		c.tokenExpiryTimer.Stop()
	}
}

// rearmTokenExpiry runs when the expiry timer fires. It returns true when the
// socket must NOT close now: either teardown stopped the timer, or a renewal
// moved the deadline and the timer has been re-armed for it.
func (c *Client) rearmTokenExpiry() bool {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.tokenExpiryStopped {
		return true
	}
	remaining := time.Until(c.TokenExpiresAt)
	if remaining <= 0 || c.tokenExpiryTimer == nil {
		return false
	}
	c.tokenExpiryTimer.Reset(remaining)
	return true
}

// onTokenExpiryTimer is the expiry timer's callback.
func (c *Client) onTokenExpiryTimer(registered <-chan struct{}) {
	if c.rearmTokenExpiry() {
		return
	}
	c.closeExpiredToken()
	select {
	case <-registered:
		c.requestUnregister()
	default:
	}
}

// handleTokenRenewal answers one auth_renew frame. It runs on the read
// goroutine, which owns the read deadline it may move.
func (c *Client) handleTokenRenewal(msg IncomingMessage) {
	token, _ := msg.Data["token"].(string)
	accepted := token != "" && c.renewToken(token)
	if accepted {
		if err := c.Conn.SetReadDeadline(c.connectionDeadline(pongWait)); err != nil {
			log.Printf("WebSocket read deadline after renewal failed: %s", describeSocketFailure(err))
		}
	}
	frame := authRenewRejectedFrame
	if accepted {
		frame = authRenewAcceptedFrame
	}
	// A dropped ack is harmless: the client times out and keeps its old deadline
	// expectation, and the server-side deadline is already correct.
	c.enqueueOutbound(frame)
}

func (c *Client) logRenewalRefused(reason string) {
	log.Printf("WebSocket token renewal refused for user %s: %s", sanitizeLogValue(c.UserID.String()), reason)
}

// canonicalSessionID returns the canonical spelling of a session id, or ""
// when it is not a uuid. Comparing and rebinding raw strings would let a
// non-canonical spelling rebind the socket to a sid DisconnectSession never
// matches.
func canonicalSessionID(sid string) string {
	parsed, err := uuid.Parse(sid)
	if err != nil {
		return ""
	}
	return parsed.String()
}

// failRenewalClosed ends a socket whose session can no longer be proven live.
// The transport closes first: unregistering waits on Hub.Run, and the socket
// must lose its authority even while Run is slow.
func (c *Client) failRenewalClosed(reason string) {
	c.logRenewalRefused(reason)
	c.stopTokenExpiry()
	c.closeRevokedConnection()
	c.requestUnregister()
}

// admitRenewalAttempt applies the per-socket throttle. It runs on the read
// goroutine, the only reader and writer of both timestamps.
func (c *Client) admitRenewalAttempt(now time.Time) bool {
	if !c.lastRenewalAttempt.IsZero() && now.Sub(c.lastRenewalAttempt) < tokenRenewalMinInterval {
		// One line per window, not per frame: per-frame refusal logging is an
		// amplification primitive.
		if now.Sub(c.lastThrottleLog) >= tokenRenewalMinInterval {
			c.lastThrottleLog = now
			c.logRenewalRefused("throttled")
		}
		return false
	}
	c.lastRenewalAttempt = now
	return true
}

// verifyRenewalToken checks that the token passes the handshake gate and
// belongs to this socket's user, credential epoch and refresh lineage. It
// returns the token's canonical sid.
func (c *Client) verifyRenewalToken(ctx context.Context, token, ownSID string) (accessTokenAuth, string, bool) {
	verified, err := c.renewAuthority.validateAccessToken(ctx, token)
	if err != nil {
		c.logRenewalRefused("invalid")
		return accessTokenAuth{}, "", false
	}
	if verified.UserID != c.UserID {
		c.logRenewalRefused("mismatch_user")
		return accessTokenAuth{}, "", false
	}
	if verified.CredEpoch != c.CredEpoch {
		c.logRenewalRefused("mismatch_epoch")
		return accessTokenAuth{}, "", false
	}
	tokenSID := canonicalSessionID(verified.SessionID)
	if tokenSID == "" {
		c.logRenewalRefused("mismatch_session")
		return accessTokenAuth{}, "", false
	}
	if tokenSID == ownSID {
		return verified, tokenSID, true
	}
	predecessor, err := c.renewAuthority.sessionPredecessor(ctx, c.UserID, tokenSID)
	if err != nil && !errors.Is(err, errSessionNotFound) {
		c.logRenewalRefused("unavailable")
		return accessTokenAuth{}, "", false
	}
	if err != nil || canonicalSessionID(predecessor) != ownSID {
		c.logRenewalRefused("mismatch_session")
		return accessTokenAuth{}, "", false
	}
	return verified, tokenSID, true
}

// rebindRenewedSession moves the socket to its successor sid on Hub.Run. Rebind
// first, then re-check revocation (the #2414 ordering): a revocation that
// committed before the check is seen there; one that commits after it sends
// DisconnectSession for the new sid, which Run services after this rebind and
// so sweeps this socket.
func (c *Client) rebindRenewedSession(ctx context.Context, sid string) bool {
	rebind := sessionRebind{client: c, sessionID: sid, done: make(chan struct{})}
	select {
	case c.Hub.rebindSession <- rebind:
	case <-ctx.Done():
		// Nothing changed: the old sid and the old deadline stand.
		c.logRenewalRefused("unavailable")
		return false
	}
	select {
	case <-rebind.done:
		return true
	case <-ctx.Done():
		// Run took the rebind but has not applied it; which sid owns the
		// socket is unknown, so it cannot be proven live.
		c.failRenewalClosed("revoked_or_unavailable")
		return false
	}
}

// renewToken applies every renewal guard. It never logs the token. The
// deadline moves last, after the session is rebound and re-checked, so an
// extended deadline never exists for a session whose revocation is unproven.
func (c *Client) renewToken(token string) bool {
	if c.renewAuthority == nil || c.Hub == nil || !c.admitRenewalAttempt(time.Now()) {
		return false
	}
	// A legacy socket with no refresh session has no lineage to prove.
	ownSID := canonicalSessionID(c.SessionID)
	if ownSID == "" {
		c.logRenewalRefused("mismatch_session")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), tokenRenewalTimeout)
	defer cancel()
	go func() {
		select {
		case <-c.Hub.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	verified, tokenSID, ok := c.verifyRenewalToken(ctx, token, ownSID)
	if !ok {
		return false
	}
	if !c.deadlineAccepts(verified.ExpiresAt) {
		c.logRenewalRefused("not_later")
		return false
	}
	if tokenSID != ownSID && !c.rebindRenewedSession(ctx, tokenSID) {
		return false
	}
	// The same-sid path is re-checked too, so acceptance never rests on every
	// mint site issuing a fresh sid.
	revoked, err := c.renewAuthority.sessionRevoked(ctx, c.UserID, tokenSID)
	if err != nil || revoked {
		c.failRenewalClosed("revoked_or_unavailable")
		return false
	}
	if !c.extendDeadline(verified.ExpiresAt) {
		// The old deadline passed during the round trips; the socket closes at
		// it, exactly as an unrenewed one does.
		c.logRenewalRefused("not_later")
		return false
	}
	return true
}
