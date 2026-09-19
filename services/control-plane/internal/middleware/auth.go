// Package middleware provides HTTP middleware for authentication, rate limiting, and CORS.
package middleware

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
)

const (
	// authError is the single generic message returned for all authentication failures.
	// Detailed diagnostics are intentionally omitted to avoid leaking information.
	authError = "Authentication required"

	// JWTClaimsContextKey stores validated JWT claims on the Gin context for
	// handlers that must act on the current token without reparsing it.
	JWTClaimsContextKey = "jwt_claims"

	// AccessTokenIssuer is the `iss` claim carried by control-plane access tokens —
	// the single source of truth WITHIN THE CONTROL-PLANE, shared by the minter
	// (auth.GenerateAccessToken) and every Go reader so the two cannot drift. Same
	// writer/reader-share discipline as UserDisabledKey below.
	//
	// Deliberately not the only place this string exists in the system:
	// services/media-plane/src/middleware/auth.ts hard-codes the same value in
	// another language, where no Go constant can reach it.
	AccessTokenIssuer = "concordvoice-control-plane"
)

// AuthRequired returns a middleware that validates JWT tokens and checks the
// token blacklist in Redis. Tokens that have been revoked (e.g. on logout)
// are rejected even if they are otherwise valid.
//
// fence enforces the per-user credential epoch (#2201): after a destructive
// password/key recovery rotates users.credential_epoch, tokens minted under
// the prior epoch (or lacking a cred_epoch claim post-rotation) are rejected.
// A nil fence disables the check (tests that construct the middleware
// directly); production always passes one.
func AuthRequired(jwtSecret string, redisClient *redis.Client, fence *credepoch.Fence) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString, ok := extractBearerToken(c.GetHeader("Authorization"))
		if !ok {
			abortUnauthorized(c)
			return
		}

		claims, ok := parseAndValidateJWT(tokenString, jwtSecret)
		if !ok {
			abortUnauthorized(c)
			return
		}

		userID, identityReason, ok := canonicalUserID(claims)
		if !ok {
			// NOT abortUnauthorized: that reports ReasonInvalidCredentials at
			// medium severity, which is the routine bad-password bucket. A
			// malformed identity claim on an HMAC-VALID token cannot be produced
			// by a client — its only causes are a compromised signing secret or a
			// regressed mint path. Same 401, same generic body, distinguishable
			// event (#3362).
			abortMalformedIdentityClaim(c)
			return
		}
		if identityReason != "" {
			MarkNightwatchVerdict(c, NightwatchVerdict{
				EventType: securityevent.EventAuthentication,
				Outcome:   securityevent.OutcomeSuccess,
				Severity:  securityevent.SeverityMedium,
				Reason:    identityReason,
			})
		}

		blacklisted, err := isTokenBlacklisted(c.Request.Context(), redisClient, claims)
		if err != nil {
			abortAuthDependency(c)
			return
		}
		if blacklisted {
			abortUnauthorized(c)
			return
		}

		emailVerified, err := VerifyLiveTokenState(c.Request.Context(), redisClient, userID, claims)
		if err != nil {
			abortLiveTokenStateError(c, err)
			return
		}

		// Credential-epoch fence (#2201): fail closed while a destructive
		// credential operation is blocked-in-flight, on a superseded epoch, or
		// when neither Redis nor the DB can answer.
		if err := credentialEpochError(c, fence, claims, userID); err != nil {
			if errors.Is(err, credepoch.ErrBlocked) || errors.Is(err, credepoch.ErrEpochMismatch) {
				abortUnauthorized(c)
			} else {
				abortAuthDependency(c)
			}
			return
		}

		c.Set("user_id", userID)
		c.Set(JWTClaimsContextKey, claims)
		c.Set("email_verified", emailVerified)
		c.Next()
	}
}

// credentialEpochError runs the fence check for AuthRequired. A nil fence
// disables the check (tests that construct the middleware directly).
func credentialEpochError(c *gin.Context, fence *credepoch.Fence, claims jwt.MapClaims, userID string) error {
	if fence == nil {
		return nil
	}
	tokenEpoch, _ := claims["cred_epoch"].(string)
	return fence.Check(c.Request.Context(), userID, tokenEpoch)
}

func abortLiveTokenStateError(c *gin.Context, err error) {
	if errors.Is(err, errAccountDisabled) {
		abortAccountDisabled(c)
		return
	}
	abortAuthDependency(c)
}

// TokenCredentialEpoch returns the cred_epoch claim AuthRequired stored on the
// context ("" when absent). Handlers pass it to credepoch.GuardTx so sensitive
// encrypted-state transactions can recheck the epoch they were admitted under
// (#2201).
func TokenCredentialEpoch(c *gin.Context) string {
	if claims, ok := c.Get(JWTClaimsContextKey); ok {
		if m, ok := claims.(jwt.MapClaims); ok {
			if e, ok := m["cred_epoch"].(string); ok {
				return e
			}
		}
	}
	return ""
}

// TokenSessionID returns the authenticated access token's `sid` claim (the
// refresh-session id, #2201), or "" for a legacy token minted without one. It
// is the server-verified session identity — unlike a client-supplied
// X-Session-ID header — so a socket registered under it can be evicted by
// DisconnectSession on session revocation.
func TokenSessionID(c *gin.Context) string {
	if claims, ok := c.Get(JWTClaimsContextKey); ok {
		if m, ok := claims.(jwt.MapClaims); ok {
			if s, ok := m["sid"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func abortUnauthorized(c *gin.Context) {
	MarkNightwatchVerdict(c, NightwatchVerdict{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonInvalidCredentials})
	c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
	c.Abort()
}

// abortMalformedIdentityClaim refuses a token whose `user_id` claim is not a
// parseable uuid (#3362). Verdict and body are IDENTICAL to abortUnauthorized —
// 401, generic authError — so nothing an attacker can observe changes; only the
// security event differs, and it is HIGH because this condition requires either
// the signing secret or a broken mint path.
func abortMalformedIdentityClaim(c *gin.Context) {
	MarkNightwatchVerdict(c, NightwatchVerdict{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, Reason: securityevent.ReasonIdentityClaimMalformed})
	c.JSON(http.StatusUnauthorized, gin.H{"error": authError})
	c.Abort()
}

func abortAccountDisabled(c *gin.Context) {
	MarkNightwatchVerdict(c, NightwatchVerdict{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonAccountDisabled})
	c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
	c.Abort()
}

func abortAuthDependency(c *gin.Context) {
	MarkNightwatchVerdict(c, NightwatchVerdict{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, Reason: securityevent.ReasonDependencyUnavailable})
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
	c.Abort()
}

// UserDisabledKey is the immediate-effect denylist key — the SINGLE source of truth for
// the "user_disabled:<id>" prefix. Both the age-claim disable path (the writer) and this
// middleware (the reader) call it, so the writer and reader can never drift apart and
// silently break the denylist.
func UserDisabledKey(userID string) string { return "user_disabled:" + userID }

// canonicalUserID reads the `user_id` claim and pins it to PostgreSQL's canonical
// uuid spelling before anything downstream keys on it. It is the reason
// `c.Get("user_id")` may be relied on as a canonical UUID BY CONSTRUCTION.
//
// WHY IT IS HERE AND NOT AT EACH GATE (#3362). The request identity crosses two
// stores with different equality rules. Every SQL predicate compares it as a
// `uuid`, and PostgreSQL resolves `{braced}`, UPPERCASE and dash-less spellings
// to ONE value — so all three reach the same users row. The two identity-keyed
// Redis gates instead CONCATENATE the raw string into a key: UserDisabledKey
// above, and credepoch.Key for the #2201 fence. Measured, all three spellings
// were admitted 200 where the canonical form was refused: the denylist has no
// database fallback at all, so a disabled account was simply not seen; and a
// `blocked:` epoch marker was missed while the read-through returned the
// still-current DB epoch and admitted. Canonicalizing HERE fixes both for every
// AUTHENTICATED caller and keeps the invariant in one place — the two key
// builders deliberately do NOT re-canonicalize, because a boundary expressed
// twice drifts (see [internal]rules/backend.md § Token-class binding).
//
// THAT ARGUMENT HAS ONE EXCEPTION, and it is not reachable from here.
// credepoch.Begin builds Key(userID) INTERNALLY, so a caller of the FENCE is a
// key-builder call site that no grep for `credepoch.Key(` will show — the first
// draft of this comment claimed "every one of their callers" on exactly that
// incomplete enumeration, and was wrong. The recovery reset routes
// (internal/auth/handlers.go) are UNAUTHENTICATED: AuthRequired never runs on
// them, so they take the id from a recovery JWT claim and must state this
// invariant themselves, which they now do. Any future unauthenticated rail that
// fences a destructive credential operation owes the same.
//
// The WebSocket door already does this: websocket/handler.go's
// parseUserIDFromClaims parses, and its gates key on `userID.String()`.
//
// NORMALIZE RATHER THAN REJECT. uuid.Parse is a parser, not a canonicality
// check, so normalizing covers any future mint path that keys a token on an
// identifier it did not read back from the users table, where a
// reject-on-non-canonical rule would instead 401 a legitimate user with no
// diagnosis. It is also LOOSER than it looks — its 38-byte arm slices s[1:] and
// never validates the delimiters, so `!<canonical>?` parses — and its accepted
// set is NOT a superset of PostgreSQL's: PG takes a hyphen after any group of
// four digits, which uuid.Parse refuses. Those PG-only spellings were live
// variants of this same bypass and are closed here by REFUSAL rather than by
// normalization. None of that is a soundness problem: every accepted input maps
// to one canonical output, so no two distinct identities can collide.
//
// Two consequences, both deliberate: a `urn:uuid:` claim (which PostgreSQL
// rejects, so it used to surface as a 503) is now normalized and gated like any
// other spelling; and non-uuid input is refused here with a 401 instead of
// reaching PostgreSQL and surfacing a malformed request as a dependency fault
// the dependency never had.
//
// The third return value is the OBSERVABILITY seam. Normalizing silently would
// make a regressed mint path work forever while reporting nothing — the defect
// class this function closes would become undetectable rather than harmless —
// and folding a malformed claim into the generic invalid-credentials bucket
// hides an event no client can produce. `(string, bool)` cannot carry either,
// so the caller gets the reason.
func canonicalUserID(claims jwt.MapClaims) (string, securityevent.ReasonCode, bool) {
	raw, ok := claims["user_id"].(string)
	if !ok || raw == "" {
		return "", securityevent.ReasonIdentityClaimMalformed, false
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return "", securityevent.ReasonIdentityClaimMalformed, false
	}
	canonical := parsed.String()
	if canonical != raw {
		// Admitted, not denied — but a mint path emitting a non-canonical
		// spelling is a defect, and this is the only place it is visible.
		return canonical, securityevent.ReasonIdentityClaimNonCanonical, true
	}
	return canonical, "", true
}

var errAccountDisabled = errors.New("account disabled")

// VerifyLiveTokenState evaluates shared post-parse bearer-token state. A present
// disabled-user key or a Redis error rejects fail closed; otherwise it returns
// the token's backward-compatible email-verification value.
func VerifyLiveTokenState(ctx context.Context, redisClient *redis.Client, userID string, claims jwt.MapClaims) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	exists, err := redisClient.Exists(checkCtx, UserDisabledKey(userID)).Result()
	if err != nil {
		return false, fmt.Errorf("check disabled user: %w", err)
	}
	if exists > 0 {
		return false, errAccountDisabled
	}
	return emailVerifiedFromClaims(claims), nil
}

// RebuildDisabledDenylist repopulates the user_disabled:<id> keys from the source of
// truth (users.disabled = TRUE), using the partial index idx_users_disabled. Call at
// process start and on Redis reconnect: a Redis flush degrades enforcement to the
// login/refresh DB gates until the rebuild runs (bounded — a live access token lasts
// <= 15 min), then the rebuild closes the gap. Returns an error; the caller logs it.
func RebuildDisabledDenylist(ctx context.Context, db *sql.DB, redisClient *redis.Client) error {
	rows, err := db.QueryContext(ctx, `SELECT id FROM users WHERE disabled = TRUE`)
	if err != nil {
		return fmt.Errorf("rebuild disabled denylist: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return fmt.Errorf("rebuild disabled denylist: scan: %w", scanErr)
		}
		if setErr := redisClient.Set(ctx, UserDisabledKey(id), "1", 0).Err(); setErr != nil {
			return fmt.Errorf("rebuild disabled denylist: set: %w", setErr)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("rebuild disabled denylist: rows: %w", rowsErr)
	}
	return nil
}

func extractBearerToken(authHeader string) (string, bool) {
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", false
	}
	return parts[1], true
}

func parseAndValidateJWT(tokenString, jwtSecret string) (jwt.MapClaims, bool) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(jwtSecret), nil
	})
	if err != nil || !token.Valid {
		return nil, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !IsAccessToken(claims) {
		return nil, false
	}
	return claims, true
}

// IsAccessTokenClass is the token-class rule, in one place. The two readers differ
// only in HOW they discover the issuer and whether a `purpose` key was present —
// never in what the answer means. A boundary expressed twice is one edit away from
// being enforced differently by surface, and an attacker picks the surface.
func IsAccessTokenClass(issuer string, hasPurpose bool) bool {
	return issuer == AccessTokenIssuer && !hasPurpose
}

// IsAccessToken reports whether already-signature-validated claims describe a
// control-plane access token, rather than some other JWT that merely happens to be
// signed with the same HMAC secret.
//
// A valid signature is NOT an authorization boundary here: mfa.GenerateChallengeToken
// signs the MFA-login, suspicious-refresh, MFA-upgrade and account-recovery families
// with the same cfg.JWTSecret, and every one carries a `user_id` claim. The incident
// and why neither the jti blacklist nor the #2201 epoch fence caught it are recorded
// once in [internal]rules/backend.md § Token-class binding (#2899).
//
// SCOPE — the constant's name overstates it: this is the boundary for the three
// CONTROL-PLANE readers. The media-plane verifies the same secret and checks the
// issuer ONLY (services/media-plane/src/middleware/auth.ts), so the boundary is not
// identical on every reader of this secret.
func IsAccessToken(claims jwt.MapClaims) bool {
	iss, _ := claims["iss"].(string)
	// Test for the KEY, not a value: `claims["purpose"].(string)` yields "" for any
	// non-string, so a numeric / null / object / bool purpose walked straight through
	// the first version of this guard. An attacker picks the encoding.
	_, hasPurpose := claims["purpose"]
	return IsAccessTokenClass(iss, hasPurpose)
}

func isTokenBlacklisted(ctx context.Context, redisClient *redis.Client, claims jwt.MapClaims) (bool, error) {
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	exists, err := redisClient.Exists(ctx, fmt.Sprintf("blacklist:%s", jti)).Result()
	if err != nil {
		return false, fmt.Errorf("check token blacklist: %w", err)
	}
	return exists > 0, nil
}

func emailVerifiedFromClaims(claims jwt.MapClaims) bool {
	if ev, ok := claims["email_verified"].(bool); ok {
		return ev
	}
	return true
}

// RequireVerifiedEmail returns a middleware that blocks requests from users
// whose email has not been verified. Must be applied AFTER AuthRequired.
func RequireVerifiedEmail() gin.HandlerFunc {
	return func(c *gin.Context) {
		verified, exists := c.Get("email_verified")
		v, ok := verified.(bool)
		if !exists || !ok || !v {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Email verification required",
				"code":  "EMAIL_NOT_VERIFIED",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// BlacklistToken adds a JWT's JTI to the Redis blacklist with a TTL matching
// the token's remaining lifetime. After blacklisting, AuthRequired will reject it.
func BlacklistToken(ctx context.Context, redisClient *redis.Client, jti string, remainingTTL time.Duration) error {
	if jti == "" || remainingTTL <= 0 {
		return nil
	}
	return redisClient.Set(ctx, fmt.Sprintf("blacklist:%s", jti), "1", remainingTTL).Err()
}
