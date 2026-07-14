package dm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	dmVoiceMediaProofHeader       = "X-Concord-Media-Proof"
	dmVoiceMediaTimestampHeader   = "X-Concord-Media-Timestamp"
	dmVoiceMediaProofKeyContext   = "concord/dm-voice-media-authorization/v1"
	dmVoiceMediaProofVersion      = "v1"
	dmVoiceMediaProofMaxClockSkew = 30 * time.Second
)

func deriveDMVoiceMediaProofKey(jwtSecret string) []byte {
	mac := hmac.New(sha256.New, []byte(jwtSecret))
	_, _ = mac.Write([]byte(dmVoiceMediaProofKeyContext))
	return mac.Sum(nil)
}

func dmVoiceMediaProofPayload(
	timestamp, method, conversationID, requestedCallID, accessToken string,
) []byte {
	tokenDigest := sha256.Sum256([]byte(accessToken))
	return []byte(strings.Join([]string{
		dmVoiceMediaProofVersion,
		timestamp,
		method,
		conversationID,
		requestedCallID,
		hex.EncodeToString(tokenDigest[:]),
	}, "\n"))
}

// validDMVoiceMediaAuthorizationProof authenticates the internal media-plane
// hop independently of the member JWT. A member possesses their bearer token
// but not the shared signing key, so they cannot turn an abandoned /voice/join
// reservation into a protected media handoff by calling /voice/authorize
// directly. The timestamp bounds replay of a proof observed on the service
// network, and the MAC binds the conversation, exact/omitted call ID, and token.
func (h *Handler) validDMVoiceMediaAuthorizationProof(
	c *gin.Context,
	conversationID uuid.UUID,
	requestedCallID uuid.UUID,
) bool {
	if h.cfg == nil || h.cfg.JWTSecret == "" {
		return false
	}
	timestamp := c.GetHeader(dmVoiceMediaTimestampHeader)
	unixSeconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	proofTime := time.Unix(unixSeconds, 0)
	skew := time.Since(proofTime)
	if skew < -dmVoiceMediaProofMaxClockSkew || skew > dmVoiceMediaProofMaxClockSkew {
		return false
	}

	providedProof, err := hex.DecodeString(c.GetHeader(dmVoiceMediaProofHeader))
	if err != nil || len(providedProof) != sha256.Size {
		return false
	}
	authorization := c.GetHeader("Authorization")
	accessToken, found := strings.CutPrefix(authorization, "Bearer ")
	if !found || accessToken == "" {
		return false
	}
	callID := ""
	if requestedCallID != uuid.Nil {
		callID = requestedCallID.String()
	}
	mac := hmac.New(sha256.New, deriveDMVoiceMediaProofKey(h.cfg.JWTSecret))
	_, _ = mac.Write(dmVoiceMediaProofPayload(
		timestamp, c.Request.Method, conversationID.String(), callID, accessToken,
	))
	return hmac.Equal(providedProof, mac.Sum(nil))
}
