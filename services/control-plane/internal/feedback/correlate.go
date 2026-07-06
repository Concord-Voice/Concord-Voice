package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// DeriveCorrelationToken returns a short, non-reversible, deterministic handle
// for a reporter so triage can tie a user's multiple reports together WITHOUT
// exposing the raw internal UUID in the (public) feedback repo. `key` is a
// dedicated correlation key HKDF-derived from the JWT secret at wiring time
// (buildFeedbackHandler) — never the raw signing key — so the correlation
// purpose is cryptographically separated from JWT signing (mirrors the
// auditIPKey seam in internal/api/oauth_wiring.go). 16 hex chars (64 bits)
// keeps cross-user collision negligible at scale.
func DeriveCorrelationToken(key []byte, userID string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(userID))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}
