package media

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// errAttachmentOverTier marks a refusal of an attachment larger than the
// uploader's entitlement allows (#1308). Only this refusal is counted: a
// well-behaved client checks the file against its own entitlement before it
// encrypts or uploads anything, so the server seeing one means the check was
// skipped. Generic body-size refusals and the chunk PUT's 413s are NOT tier
// refusals and never carry it -- they measure transport shape, which an honest
// client can get wrong.
var errAttachmentOverTier = errors.New("attachment exceeds the entitlement cap")

// SetViolationRecorder wires #1308's misbehaviour telemetry. Optional, like
// the other setters on this handler: a nil sink records nothing, and the
// refusal itself never depends on it.
func (h *Handler) SetViolationRecorder(sink entitlements.Recorder) {
	h.violations = sink
}

// observeAttachmentRefusal records a tier-cap refusal and ignores every other
// error. It runs on the refusal path AFTER the 413 body is written, and Record
// returns without waiting on Redis, so the response is identical with or
// without it.
func (h *Handler) observeAttachmentRefusal(c *gin.Context, userID string, err error) {
	if errors.Is(err, errAttachmentOverTier) {
		h.recordAttachmentOverTier(c, userID)
	}
}

// recordAttachmentOverTier counts one attachment_over_tier for the caller.
// user_id comes from AuthRequired, which canonicalizes it, so the parse cannot
// fail on a real request; if it ever does, nothing is recorded rather than a
// key being built from an unparsed string.
func (h *Handler) recordAttachmentOverTier(c *gin.Context, userID string) {
	if h.violations == nil {
		return
	}
	id, err := uuid.Parse(userID)
	if err != nil {
		return
	}
	h.violations.Record(c.Request.Context(), id, entitlements.ViolationAttachmentOverTier)
}
