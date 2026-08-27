package media

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChunkedPlaintextBytes_RoundTripsAtEveryBoundary(t *testing.T) {
	for _, pt := range []int64{
		1, 100,
		AttachmentChunkPlaintextBytes - 1,
		AttachmentChunkPlaintextBytes,
		AttachmentChunkPlaintextBytes + 1,
		33_554_432,  // free tier
		268_435_456, // premium tier
	} {
		ct := ChunkedCiphertextBytes(pt)
		got, err := ChunkedPlaintextBytes(ct, TotalChunksFor(pt))
		require.NoError(t, err, "plaintext=%d", pt)
		require.Equal(t, pt, got, "round trip failed for plaintext=%d", pt)
	}
}

// The test that would have caught the live 28-byte dead band: a file exactly at
// the entitlement must be ACCEPTED, not 413'd. Before this, the server compared
// a CIPHERTEXT length against a PLAINTEXT entitlement.
func TestSizing_FileExactlyAtTheEntitlementIsAccepted(t *testing.T) {
	const freeFloor int64 = 33_554_432

	ct := ChunkedCiphertextBytes(freeFloor)
	require.Greater(t, ct, freeFloor, "ciphertext must exceed plaintext")

	pt, err := ChunkedPlaintextBytes(ct, TotalChunksFor(freeFloor))
	require.NoError(t, err)
	require.LessOrEqual(t, pt, freeFloor,
		"a file exactly at the entitlement must pass the server's own comparison")
}

func TestChunkedPlaintextBytes_RejectsInconsistentChunkCount(t *testing.T) {
	ct := ChunkedCiphertextBytes(AttachmentChunkPlaintextBytes * 2)
	// totalChunks is client-supplied and therefore untrusted: it is verified
	// against the arithmetic rather than believed.
	_, err := ChunkedPlaintextBytes(ct, 5)
	require.Error(t, err, "a chunk count that disagrees with the arithmetic must be rejected")
}

func TestChunkedPlaintextBytes_RejectsNonPositivePlaintext(t *testing.T) {
	// require.Error alone was VACUOUS here: the redundant arithmetic-disagreement
	// check below fires for these inputs too, so deleting the plaintext floor
	// changed only which message came back. Pin the message.
	_, err := ChunkedPlaintextBytes(AttachmentEnvelopeHeaderBytes+AttachmentChunkOverheadBytes, 1)
	require.ErrorContains(t, err, "too small for",
		"the plaintext floor must be what rejects this, not the arithmetic check behind it")
}

func TestChunkedPlaintextBytes_RejectsNonPositiveChunkCount(t *testing.T) {
	for _, n := range []int64{0, -1} {
		_, err := ChunkedPlaintextBytes(1_000_000, n)
		require.ErrorContains(t, err, "totalChunks must be >= 1", "totalChunks=%d", n)
	}
}

func TestChunkedPlaintextBytes_NeverReturnsANonPositivePlaintext(t *testing.T) {
	// THE HOLE THE ARITHMETIC CHECK CANNOT SEE. TotalChunksFor returns 0 for a
	// non-positive plaintext, so `got != totalChunks` is FALSE when totalChunks
	// is also 0 -- the check agrees with itself and, with both guards deleted, a
	// NEGATIVE plaintext returns with a nil error:
	//
	//   ChunkedPlaintextBytes(ct=28,    n=0) -> 0     err=<nil>
	//   ChunkedPlaintextBytes(ct=0,     n=0) -> -28   err=<nil>
	//   ChunkedPlaintextBytes(ct=-1000, n=0) -> -1028 err=<nil>
	//
	// Both guards are load-bearing and nothing tested them.
	for _, ct := range []int64{28, 0, -1000, -1} {
		got, err := ChunkedPlaintextBytes(ct, 0)
		require.Error(t, err, "ciphertext=%d with 0 chunks must not return a plaintext", ct)
		require.Zero(t, got, "a rejected call must not hand back a size")
	}
}

func TestPremiumCeilingOverheadIs924Bytes(t *testing.T) {
	const premium int64 = 268_435_456
	require.Equal(t, int64(32), TotalChunksFor(premium))
	// 28 header + 28*32 per-chunk = 924. The client's
	// MAX_DECRYPTABLE_ATTACHMENT_BYTES derives the same number; if these two
	// ever disagree, a maximum-size premium file becomes undownloadable.
	require.Equal(t, premium+924, ChunkedCiphertextBytes(premium))
}

func TestLegacyEnvelopeOverheadMatchesTheClientFormat(t *testing.T) {
	// IV 12 + GCM tag 16. The single-shot path's ciphertext is exactly
	// plaintext + this, which is what the legacy sizing fix subtracts.
	require.Equal(t, int64(28), LegacyEnvelopeOverheadBytes)
}

// --- legacy single-shot path: the unit conversion, end to end through the parser ---

func attachmentRequest(t *testing.T, ciphertextLen int) *gin.Context {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "a.bin")
	require.NoError(t, err)
	_, err = part.Write(make([]byte, ciphertextLen))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/media/upload/attachment", &body)
	c.Request.Header.Set("Content-Type", w.FormDataContentType())
	return c
}

// The regression this whole task exists for: a file whose PLAINTEXT is exactly
// the entitlement puts plaintext+28 on the wire, and must still be accepted.
func TestParseAttachmentFile_AcceptsPlaintextExactlyAtTheLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const maxPlaintext int64 = 1024

	c := attachmentRequest(t, int(maxPlaintext+LegacyEnvelopeOverheadBytes))
	file, header, err := parseAttachmentFile(c, maxPlaintext)
	require.NoError(t, err, "a file exactly at the plaintext entitlement must be accepted")
	require.NotNil(t, header)
	if file != nil {
		_ = file.Close()
	}
}

func TestParseAttachmentFile_RejectsPlaintextOverTheLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const maxPlaintext int64 = 1024

	// One plaintext byte too many, once the envelope is accounted for.
	c := attachmentRequest(t, int(maxPlaintext+LegacyEnvelopeOverheadBytes+1))
	file, _, err := parseAttachmentFile(c, maxPlaintext)
	require.Error(t, err, "a file over the plaintext entitlement must still be rejected")
	if file != nil {
		_ = file.Close()
	}
}

// Tier 1 (icons, avatars, banners) uploads PLAINTEXT images — there is no E2EE
// envelope on that path, so its size check must NOT be converted. Pinning this
// so the two parsers are not "harmonised" into agreeing.
func TestParseMultipartFile_TierOneComparesTheWireSizeDirectly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const maxSize int64 = 1024

	// Exactly at the limit: accepted.
	c := attachmentRequest(t, int(maxSize))
	file, _, err := parseMultipartFile(c, maxSize)
	require.NoError(t, err)
	if file != nil {
		_ = file.Close()
	}

	// One byte over: rejected, with no envelope allowance.
	c2 := attachmentRequest(t, int(maxSize+1))
	file2, _, err2 := parseMultipartFile(c2, maxSize)
	require.Error(t, err2, "tier 1 must not grant an envelope allowance it never carries")
	if file2 != nil {
		_ = file2.Close()
	}
}
