package media

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type recordedViolation struct {
	userID uuid.UUID
	reason entitlements.ViolationReason
}

// recordingViolationSink stands in for *entitlements.ViolationRecorder. The
// recorder's own counting, window and escalation are proven in
// internal/entitlements; what these tests own is WHICH refusals reach it.
type recordingViolationSink struct {
	mu    sync.Mutex
	calls []recordedViolation
}

func (s *recordingViolationSink) Record(_ context.Context, userID uuid.UUID, reason entitlements.ViolationReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedViolation{userID: userID, reason: reason})
}

func (s *recordingViolationSink) snapshot() []recordedViolation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedViolation(nil), s.calls...)
}

const freeAttachmentCap = int64(33_554_432)

var legacyUploader = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func freeLegacyHandler(t *testing.T) (*Handler, *recordingViolationSink) {
	t.Helper()
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	h := NewHandler(db, nil, logger.New("test"), &config.Config{UploadMaxSize: 25 * 1024 * 1024}, nil,
		tierStub{entitlements.TierFree})
	sink := &recordingViolationSink{}
	h.SetViolationRecorder(sink)
	return h, sink
}

func requireOneAttachmentViolation(t *testing.T, sink *recordingViolationSink, userID uuid.UUID) {
	t.Helper()
	require.Equal(t, []recordedViolation{{userID: userID, reason: entitlements.ViolationAttachmentOverTier}},
		sink.snapshot(), "exactly one attachment_over_tier for the uploader")
}

// Legacy single-shot path, the body-size arm: the body overflows the
// MaxBytesReader while multipart parsing. The 413 is unchanged, but it is NOT
// counted: the reader's limit is the tier allowance PLUS the multipart overhead
// allowance, so extra form fields or part headers can trip it with a file well
// inside the cap. Only the declared-size arm below proves the file was over tier.
func TestUploadAttachmentDoesNotRecordTheMaxBytesReaderOverflow(t *testing.T) {
	h, sink := freeLegacyHandler(t)
	body, ct := makeMultipartFile(t, 40*1024*1024)
	w := doAttachmentUpload(t, h, body, ct)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "File exceeds maximum upload size")
	assert.Empty(t, sink.snapshot(), "an uncounted 413: the body limit includes multipart overhead")
}

// Legacy single-shot path, the declared-size arm (and the positive control for
// the test above): the body fits the reader's slack, but the
// file's plaintext size is one byte over the allowance.
func TestUploadAttachmentRecordsTheDeclaredSizeTierRefusal(t *testing.T) {
	h, sink := freeLegacyHandler(t)
	body, ct := makeMultipartFile(t, int(freeAttachmentCap+LegacyEnvelopeOverheadBytes+1))
	w := doAttachmentUpload(t, h, body, ct)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "File exceeds maximum upload size")
	requireOneAttachmentViolation(t, sink, legacyUploader)
}

// A refusal that is not the tier cap records nothing.
func TestUploadAttachmentDoesNotRecordANonTierRefusal(t *testing.T) {
	h, sink := freeLegacyHandler(t)
	body, ct := multipartBody(t, "not_the_file_field", "payload.bin", []byte("x"), nil)
	w := doAttachmentUpload(t, h, body, ct)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, sink.snapshot())
}

// Exactly at the allowance is not a refusal at the cap, so it records nothing
// whatever the later stages answer.
func TestUploadAttachmentDoesNotRecordAtTheCap(t *testing.T) {
	h, sink := freeLegacyHandler(t)
	body, ct := makeMultipartFile(t, int(freeAttachmentCap+LegacyEnvelopeOverheadBytes))
	w := doAttachmentUpload(t, h, body, ct)
	require.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Empty(t, sink.snapshot())
}

func sessionSetupWithSink(t *testing.T) (*sessionSetup, *recordingViolationSink) {
	t.Helper()
	ss := setupSessionTest(t)
	sink := &recordingViolationSink{}
	ss.handler.SetViolationRecorder(sink)
	return ss, sink
}

// Chunked path: the init's tier-cap 413 records once; init exactly at the cap
// and every later stage of a successful upload record nothing.
func TestInitUploadSessionRecordsOnlyTheTierCapRefusal(t *testing.T) {
	ss, sink := sessionSetupWithSink(t)
	userID, _, channelID := ss.channelContext(t, "viol_init")

	w := ss.doInit(userID, initBody(channelID, freeAttachmentCap+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	requireOneAttachmentViolation(t, sink, uuid.MustParse(userID))

	w = ss.doInit(userID, initBody(channelID, freeAttachmentCap))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Len(t, sink.snapshot(), 1, "an admitted init records nothing")
}

// The chunk PUT's 413 measures transport shape, not entitlement.
func TestUploadChunk413IsNotATierViolation(t *testing.T) {
	ss, sink := sessionSetupWithSink(t)
	userID, _, channelID := ss.channelContext(t, "viol_chunk")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	oversized := append(append([]byte{}, parts[0]...), make([]byte, 64)...)
	w := ss.doPutChunk(userID, sessionID, 0, oversized, -1)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, sink.snapshot())
}

// The init's generic body cap is not the tier cap.
func TestInitUploadSessionBodyCapIsNotATierViolation(t *testing.T) {
	ss, sink := sessionSetupWithSink(t)
	userID, _, _ := ss.channelContext(t, "viol_body")

	req := httptest.NewRequest(http.MethodPost, pathUploadSession,
		strings.NewReader(`{"mime_type":"`+strings.Repeat("a", initRequestMaxBytes)+`"}`))
	req.Header.Set(headerContentType, "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	ss.handler.InitUploadSession(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, sink.snapshot())
}

func violationTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	return c
}

func TestObserveAttachmentRefusalRecordsOnlyTheTierSentinel(t *testing.T) {
	sink := &recordingViolationSink{}
	h := &Handler{violations: sink}
	user := uuid.New()

	h.observeAttachmentRefusal(violationTestContext(), user.String(), errors.New("missing file"))
	assert.Empty(t, sink.snapshot(), "an unrelated error records nothing")

	wrapped := errors.Join(errors.New("http: request body too large"), errAttachmentOverTier)
	h.observeAttachmentRefusal(violationTestContext(), user.String(), wrapped)
	require.Equal(t, []recordedViolation{{userID: user, reason: entitlements.ViolationAttachmentOverTier}},
		sink.snapshot(), "a wrapped sentinel still records")
}

func TestRecordAttachmentOverTierWithoutASinkOrAValidUserRecordsNothing(t *testing.T) {
	assert.NotPanics(t, func() {
		(&Handler{}).recordAttachmentOverTier(violationTestContext(), uuid.New().String())
	})
	sink := &recordingViolationSink{}
	(&Handler{violations: sink}).recordAttachmentOverTier(violationTestContext(), "not-a-uuid")
	assert.Empty(t, sink.snapshot())
}
