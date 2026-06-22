package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// stateSecretIssuedKeyPrefix namespaces the one-shot replay guard for the
	// client-secret broker (#972). A sibling key — NOT a mutation of the
	// sso_state record — because the record's nonce must survive minting for
	// #974's /session id_token binding (spec F2). TTL uses stateTTL as a safe
	// upper bound: replay after record expiry already fails on record-miss.
	stateSecretIssuedKeyPrefix = "sso_state_secret_issued:" // #nosec G101 -- key prefix, not a credential
	// maxStateLength bounds request-supplied state before it becomes a Redis
	// key suffix. Legitimate states are 43 bytes (32 CSPRNG bytes, base64url).
	maxStateLength = 256
)

// signClientSecretRequest is the POST body for SignAppleClientSecret.
type signClientSecretRequest struct {
	State string `json:"state" binding:"required"`
}

// clientSecretBroker is the capability the broker endpoint requires of a
// provider. Only *AppleProvider implements it — Apple alone demands a signed
// JWT client_secret at its token endpoint.
type clientSecretBroker interface {
	BrokerClientSecret(now time.Time) (string, error)
}

// SignAppleClientSecret implements POST /auth/sso/:provider/sign-client-secret
// (#972): the control-plane's only role in the client-driven Apple OAuth path.
// Authorization is a valid, unconsumed sso_state token; every validation
// failure returns a uniform 401 invalid_state so the response never leaks
// which check failed (same philosophy as errSSOTokenInvalid).
//
// Registered under /:provider (not a static /apple/ segment) to avoid Gin
// radix-tree wildcard conflicts; the in-handler gate below 404s non-apple
// providers (spec F6).
func (h *Handler) SignAppleClientSecret(c *gin.Context) {
	if c.Param("provider") != "apple" {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}
	provider, err := h.deps.Registry.Get("apple")
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}
	broker, ok := provider.(clientSecretBroker)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}

	var req signClientSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.State == "" || len(req.State) > maxStateLength {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}

	ctx := c.Request.Context()
	raw, err := h.deps.Redis.Get(ctx, stateKeyPrefix+req.State).Bytes()
	if err != nil {
		// Covers never-existed AND TTL-expired states: Redis eviction is the
		// expiry mechanism, no separate CreatedAt check (spec, handler flow 3).
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	var rec ssoStateRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	// Constant-time equality against the record-embedded copy (spec F3,
	// CWE-385). Pre-#972 records decode State as "" and fail closed.
	if subtle.ConstantTimeCompare([]byte(req.State), []byte(rec.State)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	// Provider names are not secret material — plain equality is correct.
	if rec.Provider != "apple" {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}

	// One-shot guard: atomic SETNX sibling key. The loser of a concurrent
	// duplicate observes ok==false and is rejected as a replay.
	issued, err := h.deps.Redis.SetNX(ctx, stateSecretIssuedKeyPrefix+req.State, "1", stateTTL).Result()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "redis_unavailable"})
		return
	}
	if !issued {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}

	secret, err := broker.BrokerClientSecret(time.Now())
	if err != nil {
		// Roll back the one-shot guard so a retry isn't wrongly rejected as a
		// replay after a server-side signing failure (Gitar finding, PR #1445).
		// Between the SETNX and this DEL a concurrent duplicate still sees the
		// guard and is rejected — same outcome as the success path. If the DEL
		// itself fails, the state stays burned until TTL: acceptable degraded
		// mode, surfaced via the warn log rather than silently discarded.
		if delErr := h.deps.Redis.Del(ctx, stateSecretIssuedKeyPrefix+req.State).Err(); delErr != nil && h.deps.Log != nil {
			h.deps.Log.Warn("sso broker: one-shot guard rollback failed; state burned until TTL",
				"state_digest", digestState(req.State),
				"error", delErr.Error(),
			)
		}
		// Never wrap the signing error into the response; key material and
		// signer internals stay server-side.
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "internal_error"})
		return
	}

	h.auditClientSecretMinted(req.State, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"client_secret": secret,
		"expires_in":    int(brokerClientSecretTTL.Seconds()),
	})
}

// auditClientSecretMinted emits the structured audit event for a successful
// mint — exactly once per success, never on error paths (#972 AC). Identifiers
// are digested per [internal]rules/observability.md: the state digest is unkeyed
// (256-bit input entropy resists brute force); the IP digest is keyed because
// IPs are low-entropy.
func (h *Handler) auditClientSecretMinted(state, ip string) {
	if h.deps.Log == nil {
		return // test rigs may omit the logger; production wiring always sets it
	}
	h.deps.Log.Info("sso audit",
		"event", "apple_client_secret_minted",
		"state_digest", digestState(state),
		"ip_digest", digestIP(h.deps.AuditIPKey, ip),
	)
}

// digestState returns the first 16 hex chars of SHA-256(state) — a stable
// correlation handle that cannot be inverted (state carries 256 bits of
// CSPRNG entropy).
func digestState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])[:16]
}

// digestIP returns the first 16 hex chars of HMAC-SHA256(key, ip). Keyed
// because the IPv4 space is trivially enumerable — an unkeyed hash would be
// reversible by brute force. The key is HKDF-derived at wiring time.
func digestIP(key []byte, ip string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ip))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}
