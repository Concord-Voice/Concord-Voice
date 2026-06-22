// Package oauth implements OAuth 2.0 and OIDC provider integrations for Concord Voice SSO.
// Each provider (Google, Apple, …) exposes a common Provider interface so the
// handler layer can dispatch code-exchange and id-token validation uniformly.
package oauth

import (
	"crypto/ecdsa"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// appleIssuer is Apple's token endpoint used as the audience claim in client-secret JWTs.
// Defined here so Task 2's AppleProvider can reuse it without redeclaration.
const appleIssuer = "https://appleid.apple.com"

// clientSecretReuseGrace is how much remaining TTL must exist before the signer
// skips re-signing and returns the cached JWT. A 60-second window prevents
// returning a JWT that is about to expire to Apple's /auth/token endpoint while
// keeping the ~50µs ES256 sign cost amortised across concurrent requests.
const clientSecretReuseGrace = 60 * time.Second // pragma: allowlist secret -- duration constant, not a credential

// brokerClientSecretTTL is the lifetime of client-secret JWTs minted for the
// client-driven exchange path (epic #971 / #972): the minimum useful window
// for the desktop client to complete its single /auth/token POST to Apple.
// At this TTL the signer's reuse cache never hits (the cache condition needs
// more than clientSecretReuseGrace of remaining life), so every broker call
// re-signs — the desired fresh-JWT-per-call property.
const brokerClientSecretTTL = 60 * time.Second // pragma: allowlist secret -- duration constant, not a credential

// appleClientSecretSigner signs Apple client-secret JWTs using ES256 and caches
// the result for reuse within clientSecretReuseGrace of the token's expiry.
// Safe for concurrent use via an internal mutex.
//
// Apple's /auth/token endpoint requires a freshly-signed ES256 JWT as the
// client_secret form value. The JWT is signed with the developer's P-256 ECDSA
// private key (.p8 file), with Team ID as iss and Services ID as sub.
// See: https://developer.apple.com/documentation/sign_in_with_apple/generate-and-validate-tokens
type appleClientSecretSigner struct {
	teamID   string
	clientID string
	keyID    string
	key      *ecdsa.PrivateKey
	ttl      time.Duration

	mu     sync.Mutex
	cached string
	expiry time.Time
}

// newAppleClientSecretSigner constructs a signer with the given Apple credentials.
// ttl is the lifetime of each signed JWT; Apple caps this at 6 months (180 days).
// Production callers should use 90 days; tests inject shorter values for determinism.
func newAppleClientSecretSigner(teamID, clientID, keyID string, key *ecdsa.PrivateKey, ttl time.Duration) *appleClientSecretSigner {
	return &appleClientSecretSigner{
		teamID:   teamID,
		clientID: clientID,
		keyID:    keyID,
		key:      key,
		ttl:      ttl,
	}
}

// signed returns the cached client-secret JWT if it has more than
// clientSecretReuseGrace remaining; otherwise it re-signs, caches, and returns a
// fresh JWT. The now parameter is accepted explicitly so callers and tests can
// inject a deterministic clock without patching global time.
func (s *appleClientSecretSigner) signed(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cache hit: cached token still has > grace period remaining.
	if s.cached != "" && now.Add(clientSecretReuseGrace).Before(s.expiry) {
		return s.cached, nil
	}

	expiry := now.Add(s.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": s.teamID,
		"iat": now.Unix(),
		"exp": expiry.Unix(),
		"aud": appleIssuer,
		"sub": s.clientID,
	})
	tok.Header["kid"] = s.keyID

	str, err := tok.SignedString(s.key)
	if err != nil {
		// Private key bytes are never included in error messages.
		return "", fmt.Errorf("oauth/apple: sign client_secret: %w", err)
	}

	s.cached = str
	s.expiry = expiry
	return str, nil
}
