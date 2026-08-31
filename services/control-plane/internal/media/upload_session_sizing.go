package media

import "fmt"

// Attachment envelope arithmetic for the chunked wire format (#2157 PR 2).
//
// These constants MUST match
// client/desktop/src/renderer/utils/crypto/attachmentChunkedCrypto.ts. They are
// compile-time constants on both sides deliberately: the chunk size is bound
// into every chunk's GCM AAD, so an operator-tunable value would silently
// invalidate stored ciphertext.
const (
	// AttachmentChunkPlaintextBytes is 8 MiB — the smallest power of two above
	// S3 multipart's 5 MiB minimum part size (last part exempt).
	AttachmentChunkPlaintextBytes int64 = 8_388_608

	// AttachmentEnvelopeHeaderBytes is the per-file header, identical in v2 and
	// v3: magic 2 + version 1 + flags 1 + chunkSize 4 + totalChunks 4 +
	// fileNonce 16.
	AttachmentEnvelopeHeaderBytes int64 = 28

	// AttachmentChunkOverheadBytes is IV 12 + GCM tag 16, per chunk.
	AttachmentChunkOverheadBytes int64 = 28

	// LegacyEnvelopeOverheadBytes is the v1 single-shot overhead: IV 12 + tag 16.
	LegacyEnvelopeOverheadBytes int64 = 28
)

// EnvelopeVersion is the attachment wire format an upload session carries.
//
// A named type rather than a bare int on purpose: uploadSession already holds a
// `keyVersion int`, and the two are semantically unrelated integers that would
// otherwise be transposable at every call site without a compiler complaint.
// Same reasoning as mfa.CredEpoch ([internal]rules/backend.md § Issuance binding).
type EnvelopeVersion int

const (
	// EnvelopeVersionV2 is the original chunked format. Chunk 0 carries a full
	// AttachmentChunkPlaintextBytes of plaintext AND the 28-byte header, so part
	// 0 is 28 bytes larger than every other non-trailing part.
	//
	// STILL READABLE FOREVER: every chunked attachment already in the field is a
	// v2 blob. This server never parses one, but its arithmetic must keep
	// describing one exactly, or an in-flight session opened before a deploy
	// starts rejecting its own chunks.
	EnvelopeVersionV2 EnvelopeVersion = 2

	// EnvelopeVersionV3 shrinks chunk 0's PLAINTEXT budget by the header size, so
	// every non-trailing PART is byte-identical.
	//
	// Cloudflare R2 refuses a multipart upload whose non-trailing parts differ in
	// size (error 10048, S3 code InvalidPart: "All non-trailing parts must have
	// the same size"). S3 and MinIO both permit non-uniform parts, which is why
	// v2's asymmetry survived every test the feature shipped with — and why any
	// attachment needing >= 3 parts would have failed the moment a session
	// opened against R2.
	EnvelopeVersionV3 EnvelopeVersion = 3

	// EnvelopeVersionDefault is what an ABSENT or zero `envelope_version` means.
	//
	// v2, not v3: a client predating this change sends no field at all and is
	// still producing v2 geometry. Defaulting to the NEW format would silently
	// re-describe its parts and reject every chunk it sends.
	EnvelopeVersionDefault = EnvelopeVersionV2
)

// NormalizeEnvelopeVersion maps a client-declared envelope_version onto the
// closed accepted set, and reports whether it is in it.
//
// Zero means absent — Go's JSON decoder leaves an omitted int at zero and this
// field is optional by contract. Anything else outside {2, 3} is rejected
// rather than coerced: an unrecognised version is a client this server cannot
// describe the geometry of, and guessing would mean sizing its parts wrong.
func NormalizeEnvelopeVersion(raw int) (EnvelopeVersion, bool) {
	if raw == 0 {
		return EnvelopeVersionDefault, true
	}
	if v := EnvelopeVersion(raw); v.Valid() {
		return v, true
	}
	// Zero, NOT the rejected value and NOT the default: a caller that drops the
	// bool gets a version whose geometry is obviously unusable rather than one
	// that silently reads as v2.
	return 0, false
}

// Valid reports whether v is a version this server knows the geometry of.
func (v EnvelopeVersion) Valid() bool {
	return v == EnvelopeVersionV2 || v == EnvelopeVersionV3
}

// firstChunkPlaintextBytes is the plaintext budget of chunk 0.
//
// THIS ONE FUNCTION IS THE WHOLE DIFFERENCE BETWEEN v2 AND v3. Everything else
// below — the chunk count, the length identity, the part sizes — is the same
// arithmetic reading a different first-chunk budget.
func firstChunkPlaintextBytes(version EnvelopeVersion) int64 {
	if version == EnvelopeVersionV3 {
		return AttachmentChunkPlaintextBytes - AttachmentEnvelopeHeaderBytes
	}
	return AttachmentChunkPlaintextBytes
}

// chunkPlaintextAt returns the plaintext carried by chunk `index`.
//
// Callers must have validated `index` against `totalChunks` and `totalChunks`
// against `plaintextBytes` (ChunkedPlaintextBytes does the latter); this is
// pure arithmetic and asserts neither.
func chunkPlaintextAt(version EnvelopeVersion, index, totalChunks, plaintextBytes int64) int64 {
	first := firstChunkPlaintextBytes(version)
	if index == 0 {
		if totalChunks == 1 {
			// The only chunk is also the last, so it holds the whole file.
			return plaintextBytes
		}
		return first
	}
	if index == totalChunks-1 {
		return plaintextBytes - first - (totalChunks-2)*AttachmentChunkPlaintextBytes
	}
	return AttachmentChunkPlaintextBytes
}

// TotalChunksFor returns the number of chunks a plaintext of this size occupies
// under `version`. Returns 0 for a non-positive size, which every caller treats
// as invalid.
func TotalChunksFor(plaintextBytes int64, versions ...EnvelopeVersion) int64 {
	if plaintextBytes < 1 {
		return 0
	}
	version := envelopeVersionOrDefault(versions)
	if !version.Valid() {
		return 0
	}
	first := firstChunkPlaintextBytes(version)
	if plaintextBytes <= first {
		return 1
	}
	// Chunk 0 takes `first`; the rest are full chunks. For v2 `first` IS a full
	// chunk, so this collapses to the ceil(plaintext / chunkSize) it always was.
	return 1 + (plaintextBytes-first+AttachmentChunkPlaintextBytes-1)/AttachmentChunkPlaintextBytes
}

// ChunkedCiphertextBytes returns the on-the-wire length of an envelope carrying
// the given plaintext.
//
// The identity is version-independent — header + n*overhead + plaintext — and
// only `n` moves. v3 therefore costs at most one extra chunk's 28 bytes over v2
// for the same file, never a re-derivation.
func ChunkedCiphertextBytes(plaintextBytes int64, versions ...EnvelopeVersion) int64 {
	version := envelopeVersionOrDefault(versions)
	return AttachmentEnvelopeHeaderBytes +
		AttachmentChunkOverheadBytes*TotalChunksFor(plaintextBytes, version) +
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
// the arithmetic rather than believed. That property is what bounds every later
// part-size computation, so it survives the version parameter unchanged —
// TotalChunksFor is still monotonic in plaintextBytes under both versions, so
// the check still has a unique solution.
func ChunkedPlaintextBytes(ciphertextBytes, totalChunks int64, versions ...EnvelopeVersion) (int64, error) {
	version := envelopeVersionOrDefault(versions)
	if !version.Valid() {
		return 0, fmt.Errorf("unsupported envelope version %d", version)
	}
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
	if got := TotalChunksFor(plaintext, version); got != totalChunks {
		return 0, fmt.Errorf(
			"chunk count %d disagrees with the arithmetic %d", totalChunks, got)
	}
	return plaintext, nil
}

// envelopeVersionOrDefault keeps pre-v3 internal callers on v2 geometry.
// Session-init validation passes the persisted version explicitly; accepting a
// missing argument is only source compatibility for the v2-only helper API.
func envelopeVersionOrDefault(versions []EnvelopeVersion) EnvelopeVersion {
	if len(versions) == 0 {
		return EnvelopeVersionDefault
	}
	if len(versions) == 1 {
		return versions[0]
	}
	return 0
}
