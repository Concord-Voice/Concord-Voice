package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

var (
	// ErrInvalidToken indicates the JWT token is invalid.
	ErrInvalidToken = errors.New("invalid token")
	// ErrExpiredToken indicates the JWT token has expired.
	ErrExpiredToken = errors.New("token has expired")
)

// Claims represents JWT claims
type Claims struct {
	UserID        string `json:"user_id"`
	EmailVerified *bool  `json:"email_verified,omitempty"` // nil in pre-migration tokens → treated as true
	Tier          string `json:"tier,omitempty"`           // "free" | "premium"; omitempty → absent in pre-entitlement tokens, read as free
	// CredentialEpoch is the users.credential_epoch value the token was minted
	// under (#2201). Absent (omitempty) while the user has never rotated; after
	// the first rotation, verification fails closed on an absent claim.
	CredentialEpoch string `json:"cred_epoch,omitempty"`
	// SessionID is the refresh_tokens.id this access token descends from
	// (#2201 — the seam for future per-session revocation; not yet enforced).
	SessionID string `json:"sid,omitempty"`
	// Purpose exists ONLY so this reader can see whether the wire carried a
	// `purpose` key at all (#2899). It is never set at mint: nil marshals away
	// under omitempty, so the emitted token is byte-identical to before.
	//
	// json.RawMessage rather than string is deliberate. A string field cannot
	// distinguish "absent" from "present and null" (both decode to ""), and a
	// non-string value would make ParseWithClaims fail with a decode error rather
	// than a clean auth rejection. RawMessage captures the raw bytes of ANY value,
	// so len(Purpose) > 0 means "the key was present", which is the property
	// middleware.IsAccessToken enforces on the untyped path.
	Purpose json.RawMessage `json:"purpose,omitempty"`
	jwt.RegisteredClaims
}

// AccessTokenTTL is the lifetime of JWT access tokens.
const AccessTokenTTL = 15 * time.Minute

// GenerateAccessToken creates a short-lived JWT access token with a unique JTI.
// emailVerified and the optional entitlement tier are embedded so downstream
// middleware (control-plane) and the media-plane JWT verifier can gate without a
// DB query on every request.
//
// credentialEpoch is the user's current users.credential_epoch ("" while the
// user has never rotated — omitempty drops the claim); sessionID is the
// refresh_tokens.id the pair descends from (#2201). Both are enforced by the
// credepoch fence: after a user's first rotation, tokens without a matching
// cred_epoch claim are rejected on every surface.
//
// tier is variadic for backward compatibility: callers that omit it — or pass an
// empty value — issue a tier-less claim (omitempty drops it from the wire), which
// downstream readers treat as "free" (fail-closed). Only the first value is used.
func GenerateAccessToken(userID, jwtSecret string, emailVerified bool, credentialEpoch, sessionID string, tier ...string) (string, error) {
	now := time.Now()
	t := ""
	if len(tier) > 0 {
		t = tier[0]
	}
	claims := Claims{
		UserID:          userID,
		EmailVerified:   &emailVerified,
		Tier:            t,
		CredentialEpoch: credentialEpoch,
		SessionID:       sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.New().String(),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    middleware.AccessTokenIssuer,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(jwtSecret))
}

// ValidateAccessToken verifies and parses a JWT access token
func ValidateAccessToken(tokenString, jwtSecret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(
		tokenString,
		&Claims{},
		func(token *jwt.Token) (interface{}, error) {
			// Verify signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		},
	)

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}

	// Reject a validly-signed JWT that is not an access token. The MFA challenge and
	// recovery families share this HMAC secret and unmarshal happily into Claims
	// (unknown fields are ignored), populating UserID — so before this check, Logout's
	// blacklistAccessToken accepted a challenge token and let its holder revoke the
	// victim's sessions. Delegates to middleware.IsAccessTokenClass rather than
	// restating the rule, so this reader cannot drift from the untyped one.
	if claims, ok := token.Claims.(*Claims); ok && token.Valid &&
		middleware.IsAccessTokenClass(claims.Issuer, len(claims.Purpose) > 0) {
		return claims, nil
	}

	return nil, ErrInvalidToken
}

// GenerateRefreshToken creates a random refresh token
func GenerateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// HashRefreshToken creates a SHA-256 hash of the refresh token
func HashRefreshToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
