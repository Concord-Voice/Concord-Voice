package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// Session lifetimes (#1688 §6). The idle window slides on every Get; the
// absolute cap is fixed at mint and never extended, so a session is capped at 8h
// of wall-clock life regardless of activity.
const (
	// sessionIdleTTL is the sliding inactivity window. A session unused for this
	// long is rejected and its Redis key has expired.
	sessionIdleTTL = 30 * time.Minute
	// sessionAbsoluteTTL is the hard upper bound on a session's lifetime, set at
	// mint. Even a continuously-active session is rejected past this point.
	sessionAbsoluteTTL = 8 * time.Hour
)

// sessionIDBytes is the CSPRNG session-id length (256 bits). The id is opaque —
// it carries no admin data; the binding lives only in Redis.
const sessionIDBytes = 32

// sessionKeyPrefix namespaces admin session keys in Redis.
const sessionKeyPrefix = "admin_session:"

// adminCookieName is the session cookie. The `__Host-` prefix is a browser-
// enforced hardening contract: the cookie is accepted ONLY when sent with
// Secure, Path=/, and NO Domain attribute — preventing subdomain cookie fixation
// and forcing HTTPS (#1688 §6).
const adminCookieName = "__Host-cv_admin_sid"

// ErrSessionInvalid is returned when a session id is unknown, expired (idle or
// absolute), or malformed. A single sentinel keeps the failure uniform — a
// caller cannot distinguish "never existed" from "expired".
var ErrSessionInvalid = errors.New("admin: session is invalid or expired")

// Session is the data bound to a session id in Redis. AbsoluteExpiry is fixed at
// mint; LastSeen slides forward on each Get.
type Session struct {
	AdminID        string    `json:"admin_id"`
	CreatedAt      time.Time `json:"created_at"`
	LastSeen       time.Time `json:"last_seen"`
	AbsoluteExpiry time.Time `json:"absolute_expiry"`
}

// SessionStore mints, resolves, rotates, and revokes opaque admin sessions in
// Redis (#1688 §6). The injected clock makes idle/absolute expiry deterministic
// in tests (no time.Now() in the hot path).
type SessionStore struct {
	redis *redis.Client
	now   func() time.Time
}

// NewSessionStore wires a SessionStore against Redis with an injectable clock.
// A nil clock defaults to time.Now (production).
func NewSessionStore(rdb *redis.Client, now func() time.Time) *SessionStore {
	if now == nil {
		now = time.Now
	}
	return &SessionStore{redis: rdb, now: now}
}

// newSessionID returns a fresh 256-bit CSPRNG session id as hex.
func newSessionID() (string, error) {
	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Mint creates a new session bound to adminID, persisted in Redis with the idle
// TTL. The absolute expiry is stamped at mint (now + 8h) and stored in the value
// so Get can enforce it independently of the key TTL.
func (s *SessionStore) Mint(ctx context.Context, adminID string) (string, error) {
	if adminID == "" {
		return "", errors.New("admin: Mint requires an adminID")
	}
	sid, err := newSessionID()
	if err != nil {
		return "", err
	}
	now := s.now()
	sess := Session{
		AdminID:        adminID,
		CreatedAt:      now,
		LastSeen:       now,
		AbsoluteExpiry: now.Add(sessionAbsoluteTTL),
	}
	if err := s.persist(ctx, sid, sess, now); err != nil {
		return "", err
	}
	return sid, nil
}

// Get resolves a session id, enforcing BOTH the absolute cap and the sliding
// idle window, then refreshes last_seen + the key TTL ("slides" the idle
// window). Any expired/unknown/malformed id returns ErrSessionInvalid.
func (s *SessionStore) Get(ctx context.Context, sid string) (Session, error) {
	if sid == "" {
		return Session{}, ErrSessionInvalid
	}
	key := sessionKeyPrefix + sid
	raw, err := s.redis.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("load session: %w", err)
	}

	var sess Session
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		// A corrupt value is treated as invalid, not a 500 — fail closed.
		return Session{}, ErrSessionInvalid
	}

	now := s.now()
	// Absolute cap: a session past its mint-stamped expiry is dead even if the
	// idle key has not yet TTL'd out (clock injection can outrun Redis TTL).
	if !now.Before(sess.AbsoluteExpiry) {
		// Best-effort eager cleanup; ignore the delete error (TTL will reap it).
		if delErr := s.redis.Del(ctx, key).Err(); delErr != nil {
			return Session{}, fmt.Errorf("evict expired session: %w", delErr)
		}
		return Session{}, ErrSessionInvalid
	}
	// Idle window: if more than the idle TTL elapsed since last_seen, the session
	// is stale. (Redis would normally have reaped the key; the explicit check
	// guards the injected-clock path and any TTL skew.)
	if now.Sub(sess.LastSeen) > sessionIdleTTL {
		if delErr := s.redis.Del(ctx, key).Err(); delErr != nil {
			return Session{}, fmt.Errorf("evict idle session: %w", delErr)
		}
		return Session{}, ErrSessionInvalid
	}

	// Slide the idle window: refresh last_seen + re-persist with the idle TTL,
	// clamped so the key never outlives the absolute cap.
	sess.LastSeen = now
	if err := s.persist(ctx, sid, sess, now); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Rotate issues a fresh session id for the same admin and revokes the old id,
// preserving the original absolute expiry (rotation does not extend the 8h cap).
// Used to defend against session fixation on privilege transitions (login).
func (s *SessionStore) Rotate(ctx context.Context, oldSID string) (string, error) {
	sess, err := s.Get(ctx, oldSID)
	if err != nil {
		return "", err
	}
	newSID, err := newSessionID()
	if err != nil {
		return "", err
	}
	now := s.now()
	sess.LastSeen = now
	if err := s.persist(ctx, newSID, sess, now); err != nil {
		return "", err
	}
	if err := s.Revoke(ctx, oldSID); err != nil {
		return "", err
	}
	return newSID, nil
}

// Revoke deletes a session id (instant logout / forced revocation). Deleting an
// already-absent id is a no-op (idempotent), not an error.
func (s *SessionStore) Revoke(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	if err := s.redis.Del(ctx, sessionKeyPrefix+sid).Err(); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// persist serializes a session and writes it with a key TTL equal to the idle
// window, clamped so the key never outlives the absolute expiry. Clamping keeps
// Redis from holding a key past the hard cap even if the idle window would.
func (s *SessionStore) persist(ctx context.Context, sid string, sess Session, now time.Time) error {
	ttl := sessionIdleTTL
	if remaining := sess.AbsoluteExpiry.Sub(now); remaining < ttl {
		ttl = remaining
	}
	if ttl <= 0 {
		// Already past the absolute cap — do not write a non-positive TTL (which
		// Redis treats as "persist forever").
		return ErrSessionInvalid
	}
	payload, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	if err := s.redis.Set(ctx, sessionKeyPrefix+sid, payload, ttl).Err(); err != nil {
		return fmt.Errorf("persist session: %w", err)
	}
	return nil
}

// SetAdminSessionCookie writes the `__Host-cv_admin_sid` cookie. The `__Host-`
// prefix mandates Secure + Path=/ + empty Domain; SameSite=Strict blocks
// cross-site delivery (CSRF defense). Secure is set unconditionally — the admin
// console runs over HTTPS (incl. https-localhost in dev, see config.go).
func SetAdminSessionCookie(c *gin.Context, sid string) {
	c.SetSameSite(http.SameSiteStrictMode)
	// maxAge in seconds matches the absolute cap; the server-side Redis TTL is
	// the authoritative lifetime, the cookie maxAge is a hint to the browser.
	c.SetCookie(adminCookieName, sid, int(sessionAbsoluteTTL/time.Second), "/", "", true, true)
}

// ClearAdminSessionCookie expires the session cookie (logout). It mirrors the
// __Host- invariants (Secure, Path=/, empty Domain) so the browser accepts the
// deletion, and uses maxAge -1 -> Max-Age=0 to expire immediately.
func ClearAdminSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(adminCookieName, "", -1, "/", "", true, true)
}
