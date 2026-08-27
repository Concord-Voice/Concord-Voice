package media

import "fmt"

// Attachment envelope arithmetic for the chunked wire format (#2157 PR 2).
//
// These constants MUST match
// client/desktop/src/renderer/utils/attachmentChunkedCrypto.ts. They are
// compile-time constants on both sides deliberately: the chunk size is bound
// into every chunk's GCM AAD, so an operator-tunable value would silently
// invalidate stored ciphertext.
const (
	// AttachmentChunkPlaintextBytes is 8 MiB — the smallest power of two above
	// S3 multipart's 5 MiB minimum part size (last part exempt).
	AttachmentChunkPlaintextBytes int64 = 8_388_608

	// AttachmentEnvelopeHeaderBytes is the v2 per-file header:
	// magic 2 + version 1 + flags 1 + chunkSize 4 + totalChunks 4 + fileNonce 16.
	AttachmentEnvelopeHeaderBytes int64 = 28

	// AttachmentChunkOverheadBytes is IV 12 + GCM tag 16, per chunk.
	AttachmentChunkOverheadBytes int64 = 28

	// LegacyEnvelopeOverheadBytes is the v1 single-shot overhead: IV 12 + tag 16.
	LegacyEnvelopeOverheadBytes int64 = 28
)

// TotalChunksFor returns the number of chunks a plaintext of this size occupies.
// Returns 0 for a non-positive size, which every caller treats as invalid.
func TotalChunksFor(plaintextBytes int64) int64 {
	if plaintextBytes < 1 {
		return 0
	}
	return (plaintextBytes + AttachmentChunkPlaintextBytes - 1) / AttachmentChunkPlaintextBytes
}

// ChunkedCiphertextBytes returns the on-the-wire length of a v2 envelope
// carrying the given plaintext.
func ChunkedCiphertextBytes(plaintextBytes int64) int64 {
	return AttachmentEnvelopeHeaderBytes +
		AttachmentChunkOverheadBytes*TotalChunksFor(plaintextBytes) +
		plaintextBytes
}

// ChunkedPlaintextBytes converts a declared ciphertext length back to plaintext,
// so the server compares in the SAME UNIT the client capped in.
//
// This is the fix for a live defect: the server capped CIPHERTEXT against a
// PLAINTEXT entitlement, so a file in the top 28 bytes of a user's allowance
// passed the client's check and then 413'd. Exact arithmetic closes that band
// to zero rather than papering over it with a slack constant — a slack constant
// only narrows the mismatch and permanently widens the enforced ceiling.
//
// totalChunks is client-supplied and therefore untrusted: it is verified against
// the arithmetic rather than believed.
func ChunkedPlaintextBytes(ciphertextBytes, totalChunks int64) (int64, error) {
	if totalChunks < 1 {
		return 0, fmt.Errorf("totalChunks must be >= 1, got %d", totalChunks)
	}
	plaintext := ciphertextBytes -
		AttachmentEnvelopeHeaderBytes -
		AttachmentChunkOverheadBytes*totalChunks
	if plaintext < 1 {
		return 0, fmt.Errorf(
			"declared ciphertext %d is too small for %d chunks", ciphertextBytes, totalChunks)
	}
	if got := TotalChunksFor(plaintext); got != totalChunks {
		return 0, fmt.Errorf(
			"chunk count %d disagrees with the arithmetic %d", totalChunks, got)
	}
	return plaintext, nil
}
