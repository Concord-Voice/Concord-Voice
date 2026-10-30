package websocket

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
)

// In-band control-socket token renewal. A socket loses authority at the exp of
// the access token that opened it (#3895). The desktop rotates that token
// every 14 minutes, so without renewal every idle socket reconnected on that
// cadence. A renewal frame carries the rotated token over the open socket;
// the deadline moves only when the token passes the same gate as the
// handshake AND belongs to this socket's user, family and credential
// epoch. Any failure leaves the old deadline in force, and the socket then
// closes and reconnects exactly as before. Spec:
// [internal]specs/2026-10-22-control-ws-token-renewal-design.md.

const (
	authRenewMessageType = "auth_renew"

	// tokenRenewalTimeout bounds every round-trip of one renewal (blacklist,
	// live state, epoch fence, family re-check). It runs on the
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

// tokenValidator is the narrow slice of Handler a socket needs to
// renew. nil on a Client disables renewal (fail closed).
type tokenValidator interface {
	validateAccessToken(ctx context.Context, token string) (accessTokenAuth, error)
}

// sessionRebind asks Hub.Run to move a client to its successor refresh
// session. SessionID is read unlocked on Run by DisconnectSession, so only Run
// may write it once the client is registered.
type sessionRebind struct {
	client    *Client
	sessionID string
	done      chan struct{}
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
	if c.securityTerminal.Load() || c.tokenExpiryStopped || !time.Now().Before(c.TokenExpiresAt) || !exp.After(c.TokenExpiresAt) {
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
	return !c.securityTerminal.Load() && !c.tokenExpiryStopped && time.Now().Before(c.TokenExpiresAt) && exp.After(c.TokenExpiresAt)
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
	if c.securityTerminal.Load() || c.tokenExpiryStopped {
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
// belongs to this socket's user, family and credential epoch. It
// returns the token's canonical sid.
func (c *Client) verifyRenewalToken(ctx context.Context, token string) (accessTokenAuth, string, bool) {
	verified, err := c.renewAuthority.validateAccessToken(ctx, token)
	if err != nil {
		c.logRenewalRefused("invalid")
		return accessTokenAuth{}, "", false
	}
	if verified.UserID != c.UserID || c.securityTerminal.Load() {
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
	if c.FamilyID == uuid.Nil || verified.FamilyID != c.FamilyID {
		c.logRenewalRefused("mismatch_family")
		return accessTokenAuth{}, "", false
	}

	return verified, tokenSID, true
}

// rebindRenewedSession moves the socket to another SID in its family on Hub.Run.
// Rebind first, then resolve the exact family again. A terminal commit before
// that check is denied; one after it schedules the same immutable family sweep.
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
		return !c.securityTerminal.Load()
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
	if c.securityTerminal.Load() || c.renewAuthority == nil || c.Hub == nil || !c.admitRenewalAttempt(time.Now()) {
		return false
	}
	// An absent SID cannot establish family authority.
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

	verified, tokenSID, ok := c.verifyRenewalToken(ctx, token)
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
	// Repeat the shared gate after Run's rebind. Both SID paths must still
	// resolve the exact immutable family; no retained refresh row is required.
	final, err := c.renewAuthority.validateAccessToken(ctx, token)
	if err != nil || final.UserID != verified.UserID || final.FamilyID != verified.FamilyID ||
		final.CredEpoch != verified.CredEpoch || canonicalSessionID(final.SessionID) != tokenSID ||
		!final.ExpiresAt.Equal(verified.ExpiresAt) {
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
