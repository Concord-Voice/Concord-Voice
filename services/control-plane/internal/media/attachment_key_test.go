package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// attachmentStorageKey is the CONSTRUCTOR: it mints the one correct key for a
// brand-new attachment. These tests lock its output to the current prefix and
// guard against the constant/constructor silently drifting apart.

func TestAttachmentStorageKey_MintsUnderCurrentPrefix(t *testing.T) {
	assert.Equal(t, "attachments/abc-123", attachmentStorageKey("abc-123"))
}

func TestAttachmentStorageKey_DerivesFromThePrefixConstant(t *testing.T) {
	// Locks that the constructor is built FROM attachmentKeyPrefix rather than
	// carrying its own duplicated literal -- if #1608 repoints the constant,
	// this test (and the constructor) move with it for free.
	fileID := "some-file-id"
	assert.Equal(t, attachmentKeyPrefix+fileID, attachmentStorageKey(fileID))
}

func TestAttachmentStorageKey_EmptyFileID(t *testing.T) {
	// No fileID validation happens here -- that is the caller's job (uuid.New()
	// always supplies one in production). The constructor must still behave
	// predictably: it degrades to the bare prefix rather than panicking.
	assert.Equal(t, "attachments/", attachmentStorageKey(""))
}

// isRecognizedAttachmentKey is the PREDICATE: given an arbitrary key read back
// from the object store, it answers whether the attachment upload path could
// have minted it. Wrong in either direction is dangerous -- a false negative
// abandons a live upload's bytes to the sweeper's neighbouring code path
// unnoticed, a false positive lets the sweeper abort a stranger's write.

func TestIsRecognizedAttachmentKey_RecognizesCurrentPrefix(t *testing.T) {
	assert.True(t, isRecognizedAttachmentKey("attachments/file-1"))
}

func TestIsRecognizedAttachmentKey_RejectsForeignPrefixes(t *testing.T) {
	foreign := []string{
		"avatars/u1",
		"server-icons/s1",
		"server-banners/s1",
		"dm-icons/c1",
		"banners/u1",
		"backups/nightly.tar",
	}
	for _, key := range foreign {
		assert.False(t, isRecognizedAttachmentKey(key), "key %q must not be recognized as an attachment key", key)
	}
}

func TestIsRecognizedAttachmentKey_RejectsEmptyKey(t *testing.T) {
	assert.False(t, isRecognizedAttachmentKey(""))
}

func TestIsRecognizedAttachmentKey_ChecksPrefixNotSubstring(t *testing.T) {
	// "attachments/" must anchor at the START of the key. If the predicate ever
	// regressed from strings.HasPrefix to a substring check (strings.Contains),
	// this key would be wrongly recognized and this test would catch it.
	assert.False(t, isRecognizedAttachmentKey("not-attachments/file-1"))
}

func TestIsRecognizedAttachmentKey_MatchesEveryHistoricalPrefix(t *testing.T) {
	// The predicate must walk the WHOLE union, not just the first entry --
	// this is what lets #1608 append a second prefix without a logic change.
	for _, prefix := range recognizedAttachmentKeyPrefixes {
		assert.True(t, isRecognizedAttachmentKey(prefix+"some-file"),
			"prefix %q from recognizedAttachmentKeyPrefixes must be recognized", prefix)
	}
}

func TestRecognizedAttachmentKeyPrefixes_ContainsTheCurrentPrefix(t *testing.T) {
	// If a future rename repoints attachmentKeyPrefix without appending it to
	// recognizedAttachmentKeyPrefixes, every newly minted key becomes
	// unrecognized by the sweeper the instant it is written -- silently
	// abandoning brand-new uploads, not just historical ones.
	assert.Contains(t, recognizedAttachmentKeyPrefixes, attachmentKeyPrefix)
}

func TestAttachmentStorageKey_OutputIsAlwaysRecognized(t *testing.T) {
	// The invariant that actually protects the sweeper: whatever the
	// constructor mints today, the predicate must recognize -- for any fileID,
	// not just a fixed example. This is the test that would fail the day the
	// two artefacts drift apart.
	for _, fileID := range []string{"a", "00000000-0000-0000-0000-000000000000", "file-with-dashes-123"} {
		key := attachmentStorageKey(fileID)
		assert.True(t, isRecognizedAttachmentKey(key),
			"constructor output %q must be recognized by the predicate", key)
	}
}
