package media

import "strings"

// Attachment storage-key construction and recognition.
//
// TWO artefacts, not one, and the split is deliberate. attachmentStorageKey is a
// CONSTRUCTOR: given a fileID it mints the one correct key for a brand-new
// attachment object. isRecognizedAttachmentKey is a PREDICATE: given an
// ARBITRARY key the sweeper did not write -- read back from
// ListIncompleteUploads, which returns every incomplete multipart upload in the
// bucket -- it answers whether that key belongs to the attachment upload path at
// all. A constructor has no way to express that question, so collapsing the two
// into one value would leave the sweeper with nothing to call.
//
// It also has to survive a prefix rename (#1608) without a logic change. When
// that lands, the bucket still holds in-flight multipart uploads keyed under the
// OLD prefix, and the sweeper must keep matching them or it silently abandons a
// stranger's upload to the bucket lifecycle rule while reporting a clean sweep
// (see upload_session_sweeper.go's SweepAbandoned/SweepResult doc comments). A
// predicate that only recognizes the new prefix reintroduces exactly that bug.
// recognizedAttachmentKeyPrefixes is therefore the historical UNION of every
// prefix attachment keys have ever been minted under; #1608 appends the new
// value to that slice and repoints attachmentKeyPrefix at it -- a one-line
// change, not a logic edit.

// attachmentKeyPrefix is the prefix used to mint NEW attachment storage keys.
// Every constructor call site uses this and only this.
const attachmentKeyPrefix = "attachments/"

// recognizedAttachmentKeyPrefixes is every prefix an attachment key has ever
// been minted under, oldest compatibility retained indefinitely. The sweeper
// must keep recognizing an old prefix for as long as the bucket can still hold
// an incomplete multipart upload started under it.
var recognizedAttachmentKeyPrefixes = []string{attachmentKeyPrefix}

// attachmentStorageKey mints the storage key for a newly uploaded attachment.
func attachmentStorageKey(fileID string) string {
	return attachmentKeyPrefix + fileID
}

// isRecognizedAttachmentKey reports whether key was minted by the attachment
// upload path, under the current prefix or any historical one. Used by the
// session sweeper to scope ListIncompleteUploads results before aborting
// them -- see the sweeper's own comments on why mis-scoping this is dangerous.
func isRecognizedAttachmentKey(key string) bool {
	for _, prefix := range recognizedAttachmentKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
