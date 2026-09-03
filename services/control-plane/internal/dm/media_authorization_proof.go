package dm

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

const (
	dmVoiceMediaProofHeader     = "X-Concord-Media-Proof"
	dmVoiceMediaTimestampHeader = "X-Concord-Media-Timestamp"
	dmVoiceMediaProofKeyContext = "concord/dm-voice-media-authorization/v1"
	// Owned by this caller, not the shared package: the two proofs are
	// independent wire formats and must be able to version separately.
	dmVoiceMediaProofVersion = "v1"
)

// validDMVoiceMediaAuthorizationProof authenticates the internal media-plane
// hop independently of the member JWT. A member possesses their bearer token
// but not the shared signing key, so they cannot turn an abandoned /voice/join
// reservation into a protected media handoff by calling /voice/authorize
// directly. The timestamp bounds replay of a proof observed on the service
// network, and the MAC binds the conversation, exact/omitted call ID, and token.
//
// This proof is endpoint-specific and gates the media handoff itself. It is
// deliberately distinct from the service-hop proof in middleware — that one
// carries its own context string and only exempts a hop from the client-facing
// version and attestation gates. Neither can be replayed as the other.
func (h *Handler) validDMVoiceMediaAuthorizationProof(
	c *gin.Context,
	conversationID uuid.UUID,
	requestedCallID uuid.UUID,
) bool {
	if h.cfg == nil {
		h.logProofRejection("no_config")
		return false
	}
	accessToken, found := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !found || accessToken == "" {
		h.logProofRejection("no_bearer")
		return false
	}
	callID := ""
	if requestedCallID != uuid.Nil {
		callID = requestedCallID.String()
	}
	// Passed once; mediaproof both skew-checks and signs it, so the checked
	// value and the bound value cannot diverge.
	timestamp := c.GetHeader(dmVoiceMediaTimestampHeader)
	ok := mediaproof.Verify(
		mediaproof.DeriveKey(h.cfg.JWTSecret, dmVoiceMediaProofKeyContext),
		c.GetHeader(dmVoiceMediaProofHeader),
		dmVoiceMediaProofVersion,
		timestamp,
		c.Request.Method,
		conversationID.String(),
		callID,
		mediaproof.TokenDigest(accessToken),
	)
	if !ok {
		h.logProofRejection("proof_invalid")
	}
	return ok
}

// logProofRejection records that the media handoff was refused, with a fixed
// reason enum and nothing request-supplied.
//
// This gate previously returned a bare bool on all of its rejection paths, so a
// handoff refused for a rotated secret, a stale timestamp or a malformed header
// was as silent as the service hop was before #3088 — and this is the endpoint
// that gates the media handoff itself. Sampling is not needed here: unlike the
// service-hop header, reaching this function requires passing every route gate
// on an authenticated DM voice-authorize call, which is already rate-limited.
func (h *Handler) logProofRejection(reason string) {
	if h.log == nil {
		return
	}
	h.log.Warn("DM voice media-authorization proof rejected",
		"failure_class", "dm_media_proof_rejected",
		"reason", reason,
	)
}
