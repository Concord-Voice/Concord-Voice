package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVerifyAttachmentAccess exercises the three mismatch branches of
// verifyAttachmentAccess (the sanitized log sites at hub.go:1117/1121/1125,
// #1365) plus the matching paths. verifyAttachmentAccess only compares its
// arguments, so a zero-value Hub is sufficient — no DB/Redis needed.
func TestVerifyAttachmentAccess(t *testing.T) {
	h := &Hub{}

	t.Run("not owner -> false", func(t *testing.T) {
		assert.False(t, h.verifyAttachmentAccess("file-1", "user-A",
			attachmentLinkCtx{userID: "user-B"}, nil, nil))
	})

	t.Run("wrong channel -> false", func(t *testing.T) {
		assert.False(t, h.verifyAttachmentAccess("file-1", "user-A",
			attachmentLinkCtx{userID: "user-A", channelID: "chan-1"}, nil, nil))
	})

	t.Run("wrong conversation -> false", func(t *testing.T) {
		assert.False(t, h.verifyAttachmentAccess("file-1", "user-A",
			attachmentLinkCtx{userID: "user-A", conversationID: "conv-1"}, nil, nil))
	})

	t.Run("matching channel -> true", func(t *testing.T) {
		ch := "chan-1"
		assert.True(t, h.verifyAttachmentAccess("file-1", "user-A",
			attachmentLinkCtx{userID: "user-A", channelID: "chan-1"}, &ch, nil))
	})

	t.Run("matching conversation -> true", func(t *testing.T) {
		conv := "conv-1"
		assert.True(t, h.verifyAttachmentAccess("file-1", "user-A",
			attachmentLinkCtx{userID: "user-A", conversationID: "conv-1"}, nil, &conv))
	})
}
