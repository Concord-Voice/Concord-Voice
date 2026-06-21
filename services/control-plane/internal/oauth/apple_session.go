package oauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// appleSessionRequest is the POST body for AppleSession (#974). The desktop
// main process completes the Apple code exchange itself and posts the
// resulting id_token for server-side re-verification — the server NEVER
// trusts client-side verification (CWE-345 defense in depth).
//
// AppleUserData carries Apple's first-auth "user" JSON under the same wire
// name Callback used: same 64 KiB cap, same treat-as-opaque posture (a UX
// display-name suggestion, never an authoritative identity signal). Never
// logged in any error path.
type appleSessionRequest struct {
	IDToken       string `json:"id_token" binding:"required"`
	State         string `json:"state" binding:"required"`
	AppleUserData string `json:"apple_user_data,omitempty"`
}

// ProviderSession implements POST /api/v1/auth/sso/:provider/session —
// the terminal server step of the client-driven OAuth exchange for supported
// providers (apple, google). Renamed from AppleSession (#975: widened to google).
//
// Flow:
//  1. Provider gate: apple|google only (404 for unknown providers, broker
//     parity). Type-switch on the concrete provider type gates per-provider
//     id_token verification without adding a new interface method.
//  2. State validation — error class `invalid_state` (401): record lookup,
//     GET-then-DEL one-shot (Callback parity; GETDEL is the tracked
//     hardening), constant-time compare against the record-embedded copy
//     (#972 / CWE-385; pre-#972 records decode State as "" and fail closed),
//     provider binding. Runs AFTER the broker consumed its sibling one-shot
//     key earlier in the flow — independent keys, compatible orderings.
//  3. id_token verification — error class `invalid_id_token` (401): the
//     existing #271 verifier (signature via JWKS with RS256-only keyfunc,
//     iss/aud/exp/iat, nonce binding against rec.Nonce, nonce_supported for
//     apple). The two 401 classes exist because the client remediation differs
//     (restart the flow vs never-retry-this-token); WHICH check failed
//     within a class stays opaque.
//  4. Email-verified gate (403, Callback parity).
//  5. Provider-specific UserInfo construction, then classifyCallback + the
//     existing respond* terminal step — byte-identical response shapes, so
//     the renderer's register/link/MFA paths are untouched.
func (h *Handler) ProviderSession(c *gin.Context) {
	providerName := c.Param("provider")
	if providerName != "apple" && providerName != "google" {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}
	provider, err := h.deps.Registry.Get(providerName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}

	var req appleSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.State) > maxStateLength {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "invalid_request"})
		return
	}

	ctx := c.Request.Context()

	stateKey := stateKeyPrefix + req.State
	raw, err := h.deps.Redis.Get(ctx, stateKey).Bytes()
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	// Best-effort delete: the record is already read, so a delete failure
	// cannot enable replay within this request (Callback parity).
	_, _ = h.deps.Redis.Del(ctx, stateKey).Result()

	var rec ssoStateRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.State), []byte(rec.State)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	// Provider names are not secret material — plain equality is correct.
	if rec.Provider != providerName {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}

	// Per-provider id_token verification via concrete type-assertion.
	// This avoids adding Exchange (or a new interface method) for what is
	// purely server-side re-verification of a client-minted token.
	var info *UserInfo
	var ok bool
	switch p := provider.(type) {
	case *AppleProvider:
		info, ok = h.appleSessionUserInfo(ctx, c, p, &req, rec.Nonce)
	case *GoogleProvider:
		info, ok = h.googleSessionUserInfo(ctx, c, p, rec.Nonce, req.IDToken)
	default:
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}
	if !ok {
		return
	}

	switch h.classifyCallback(ctx, info) {
	case branchExistingSSO:
		h.respondExistingSSO(c, info)
	case branchAccountLink:
		h.respondAccountLink(c, info)
	default: // branchNewUser
		h.respondNewUser(c, info)
	}
}

// appleSessionUserInfo verifies the Apple id_token and builds UserInfo, writing
// the error response and returning ok=false on any failure.
func (h *Handler) appleSessionUserInfo(ctx context.Context, c *gin.Context, p *AppleProvider, req *appleSessionRequest, nonce string) (*UserInfo, bool) {
	claims, vErr := p.validateIDToken(ctx, req.IDToken, nonce)
	if vErr != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_id_token"})
		return nil, false
	}
	if !bool(claims.EmailVerified) {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "oauth_email_unverified"})
		return nil, false
	}
	appleUserData := req.AppleUserData
	if len(appleUserData) > maxAppleUserDataBytes {
		appleUserData = ""
	}
	return &UserInfo{
		Provider:       "apple",
		ProviderUserID: claims.Subject,
		Email:          claims.Email,
		EmailVerified:  bool(claims.EmailVerified),
		Name:           parseAppleUserData(appleUserData),
		AvatarURL:      "",
		IsRelayEmail:   isAppleRelayEmail(claims.Email, bool(claims.IsPrivateEmail)),
	}, true
}

// googleSessionUserInfo verifies the Google id_token and builds UserInfo.
func (h *Handler) googleSessionUserInfo(ctx context.Context, c *gin.Context, p *GoogleProvider, nonce, idToken string) (*UserInfo, bool) {
	claims, vErr := p.validateIDToken(ctx, idToken, nonce)
	if vErr != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_id_token"})
		return nil, false
	}
	if !claims.EmailVerified {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "oauth_email_unverified"})
		return nil, false
	}
	return &UserInfo{
		Provider:       "google",
		ProviderUserID: claims.Sub,
		Email:          claims.Email,
		EmailVerified:  claims.EmailVerified,
		Name:           claims.Name,
		AvatarURL:      claims.Picture,
		IsRelayEmail:   false,
	}, true
}
