package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// pathUploadSession is the session route prefix, used to build realistic
// request URLs in the handler-level tests.
const pathUploadSession = "/api/v1/media/upload/attachment/session"

// errFakeStore is the injected object-store fault. It is deliberately NOT
// wrapped in any package sentinel: a backend fault must map to 502/503 on its
// own shape, not because the handler recognised a specific error value.
var errFakeStore = errors.New("fake store: backend unavailable")

// =====================================================================
// Chunked attachment upload session (#2157 PR 2, spec §4.2/§4.3)
// =====================================================================

// fakeMultipartStore is an ObjectStore with REAL multipart semantics, kept
// separate from mockStore (whose multipart methods fail loudly on purpose).
//
// The design constraint these tests exist to protect is "the object store is
// authoritative at commit", so this fake keeps its own part ledger and never
// consults the caller's: a test can delete a part behind the handler's back and
// the handler must notice. An in-memory fake that agreed with the caller by
// construction would prove nothing — this one can, and does, disagree.
type fakeMultipartStore struct {
	*mockStore

	mu      sync.Mutex
	uploads map[string]*fakeMultipartUpload
	nextID  int

	// Injected failures, one per verb, so each object-store error path gets its
	// own status assertion rather than sharing one.
	newErr      error
	putPartErr  error
	listErr     error
	completeErr error
	abortErr    error
	// See AbortMultipartUpload: a bounded prefix of failures, plus the attempt
	// count, is what makes continue-vs-break observable.
	abortFailFirst int
	abortAttempts  int

	aborted []string
}

type fakeMultipartUpload struct {
	key         string
	contentType string
	// When the multipart upload was started. The sweeper filters on this, so a
	// fake that ignored it would make every age assertion vacuous -- the sweep
	// would pass identically whether or not the filter existed.
	initiated time.Time
	parts     map[int]storage.ObjectPartInfo
	data      map[int][]byte
}

func newFakeMultipartStore() *fakeMultipartStore {
	return &fakeMultipartStore{
		mockStore: newMockStore(),
		uploads:   make(map[string]*fakeMultipartUpload),
	}
}

func (f *fakeMultipartStore) NewMultipartUpload(_ context.Context, key, contentType string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newErr != nil {
		return "", f.newErr
	}
	f.nextID++
	uploadID := fmt.Sprintf("upload-%d", f.nextID)
	f.uploads[uploadID] = &fakeMultipartUpload{
		key:         key,
		contentType: contentType,
		initiated:   time.Now(),
		parts:       make(map[int]storage.ObjectPartInfo),
		data:        make(map[int][]byte),
	}
	return uploadID, nil
}

func (f *fakeMultipartStore) PutObjectPart(
	_ context.Context, key, uploadID string, partNumber int, r io.Reader, size int64,
) (storage.ObjectPartInfo, error) {
	// Read OUTSIDE the lock, mirroring a real backend that streams: the handler
	// hands us the (MaxBytesReader-wrapped) request body.
	data, readErr := io.ReadAll(r)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putPartErr != nil {
		return storage.ObjectPartInfo{}, f.putPartErr
	}
	up, ok := f.uploads[uploadID]
	if !ok || up.key != key {
		return storage.ObjectPartInfo{}, fmt.Errorf("fake store: no such upload for %q", key)
	}
	if readErr != nil {
		return storage.ObjectPartInfo{}, fmt.Errorf("fake store: part read failed: %w", readErr)
	}
	// A real backend rejects a part whose body is shorter than the declared
	// length. Keeping that behaviour is what lets the truncated-body test reach
	// the handler's short-read classification instead of silently succeeding.
	if int64(len(data)) != size {
		return storage.ObjectPartInfo{}, fmt.Errorf(
			"fake store: part %d declared %d bytes, body carried %d", partNumber, size, len(data))
	}
	info := storage.ObjectPartInfo{
		PartNumber: partNumber,
		Size:       int64(len(data)),
		ETag:       fmt.Sprintf("etag-%s-%d-%d", uploadID, partNumber, len(data)),
	}
	up.parts[partNumber] = info
	up.data[partNumber] = data
	return info, nil
}

func (f *fakeMultipartStore) ListObjectParts(_ context.Context, key, uploadID string) ([]storage.ObjectPartInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	up, ok := f.uploads[uploadID]
	if !ok || up.key != key {
		return nil, fmt.Errorf("fake store: no such upload for %q", key)
	}
	out := make([]storage.ObjectPartInfo, 0, len(up.parts))
	for _, p := range up.parts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out, nil
}

func (f *fakeMultipartStore) CompleteMultipartUpload(
	_ context.Context, key, uploadID string, parts []storage.ObjectPartInfo,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeErr != nil {
		return f.completeErr
	}
	up, ok := f.uploads[uploadID]
	if !ok || up.key != key {
		return fmt.Errorf("fake store: no such upload for %q", key)
	}
	ordered := make([]storage.ObjectPartInfo, len(parts))
	copy(ordered, parts)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PartNumber < ordered[j].PartNumber })

	var buf bytes.Buffer
	for _, p := range ordered {
		held, present := up.parts[p.PartNumber]
		if !present {
			return fmt.Errorf("fake store: part %d was never uploaded", p.PartNumber)
		}
		if held.ETag != p.ETag {
			return fmt.Errorf("fake store: part %d ETag mismatch", p.PartNumber)
		}
		buf.Write(up.data[p.PartNumber])
	}
	delete(f.uploads, uploadID)
	f.mockStore.mu.Lock()
	f.objects[key] = &mockObject{data: buf.Bytes(), contentType: up.contentType}
	f.mockStore.mu.Unlock()
	return nil
}

func (f *fakeMultipartStore) AbortMultipartUpload(_ context.Context, _, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortAttempts++
	// abortFailFirst lets a bounded PREFIX of aborts fail. Failing them all makes
	// "the batch continued" and "the batch stopped" produce identical state, so a
	// test written against a blanket error cannot see the difference (#2931).
	if f.abortFailFirst > 0 && f.abortAttempts <= f.abortFailFirst {
		return errFakeStore
	}
	if f.abortErr != nil {
		return f.abortErr
	}
	delete(f.uploads, uploadID)
	f.aborted = append(f.aborted, uploadID)
	return nil
}

func (f *fakeMultipartStore) ListIncompleteUploads(
	_ context.Context, olderThan time.Time,
) ([]storage.IncompleteUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]storage.IncompleteUpload, 0, len(f.uploads))
	for id, up := range f.uploads {
		// Honours the cutoff exactly as the real store does. Returning
		// everything regardless would hand the sweeper a work queue it never
		// asked for and hide any missing age filter.
		if !up.initiated.Before(olderThan) {
			continue
		}
		out = append(out, storage.IncompleteUpload{
			Key: up.key, UploadID: id, Initiated: up.initiated,
		})
	}
	return out, nil
}

// backdateUpload ages one multipart upload so the sweeper's cutoff selects it.
func (f *fakeMultipartStore) backdateUpload(uploadID string, age time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if up, ok := f.uploads[uploadID]; ok {
		up.initiated = time.Now().Add(-age)
	}
}

// startForeignUpload stages an incomplete multipart upload under a key that is
// NOT an attachment, to prove the sweep is prefix-scoped.
func (f *fakeMultipartStore) startForeignUpload(key string, age time.Duration) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("foreign-%d", f.nextID)
	f.uploads[id] = &fakeMultipartUpload{
		key:       key,
		parts:     make(map[int]storage.ObjectPartInfo),
		data:      make(map[int][]byte),
		initiated: time.Now().Add(-age),
	}
	return id
}

// shrinkStoredPart leaves a part PRESENT but at the wrong size -- the half of
// "the object store is the authority" that deleting a part cannot exercise.
func (f *fakeMultipartStore) shrinkStoredPart(_ string, partNumber int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, up := range f.uploads {
		if p, ok := up.parts[partNumber]; ok {
			p.Size--
			up.parts[partNumber] = p
			return
		}
	}
}

// dropStoredPart removes a part from the store's ledger WITHOUT touching the
// session's own record in Redis — the exact disagreement the commit path has to
// resolve in the store's favour.
func (f *fakeMultipartStore) dropStoredPart(uploadID string, partNumber int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if up, ok := f.uploads[uploadID]; ok {
		delete(up.parts, partNumber)
		delete(up.data, partNumber)
	}
}

func (f *fakeMultipartStore) openUploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// --- test harness ---------------------------------------------------------

type sessionSetup struct {
	*testSetup
	fake *fakeMultipartStore
	rdb  *redis.Client
}

func setupSessionTest(t *testing.T) *sessionSetup {
	t.Helper()

	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	fake := newFakeMultipartStore()
	cfg := &config.Config{UploadMaxSize: 25 * 1024 * 1024}
	h := NewHandler(db, fake, logger.New("test"), cfg, nil, freeTierStub{})
	rdb := redistest.Client(t)
	h.SetSessionRedis(rdb)

	return &sessionSetup{
		testSetup: &testSetup{handler: h, store: fake.mockStore, db: db},
		fake:      fake,
		rdb:       rdb,
	}
}

// channelContext creates a server + channel the user may upload into.
func (ss *sessionSetup) channelContext(t *testing.T, username string) (userID, serverID, channelID string) {
	t.Helper()
	userID = ss.createTestUser(t, username)
	serverID = ss.createTestServer(t, userID, "sess-"+username)
	channelID = ss.createTestChannel(t, serverID, "sess-uploads")
	return userID, serverID, channelID
}

func (ss *sessionSetup) doInit(userID string, body map[string]any) *httptest.ResponseRecorder {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err) // impossible for a map of scalars; panicking keeps the helper signature small
	}
	req := httptest.NewRequest(http.MethodPost, pathUploadSession, bytes.NewReader(raw))
	req.Header.Set(headerContentType, "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	ss.handler.InitUploadSession(c)
	return w
}

// initBody is the default well-formed init payload for a plaintext of the given size.
func initBody(channelID string, plaintextBytes int64) map[string]any {
	total := TotalChunksFor(plaintextBytes)
	return map[string]any{
		"channel_id":                channelID,
		"key_version":               1,
		"file_type":                 "file",
		"mime_type":                 "application/octet-stream",
		"chunk_size":                AttachmentChunkPlaintextBytes,
		"total_chunks":              total,
		"declared_ciphertext_bytes": ChunkedCiphertextBytes(plaintextBytes),
	}
}

func (ss *sessionSetup) doPutChunk(
	userID, sessionID string, index int, data []byte, contentLength int64,
) *httptest.ResponseRecorder {
	path := fmt.Sprintf("%s/%s/chunk/%d", pathUploadSession, sessionID, index)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(data))
	req.Header.Set(headerContentType, mimeOctetStream)
	// contentLength sentinels:
	//   >= 0  declare exactly this many bytes
	//   -1    leave the length httptest computed from the body (the normal case)
	//   -2    declare the length UNKNOWN, which is Go's ContentLength == -1
	//
	// The -2 case has to be set explicitly. httptest.NewRequest fills
	// ContentLength in from a *bytes.Reader, so simply declining to override it
	// yields a perfectly well-formed request -- which is why the unknown-length
	// assertion could never reach the handler's guard.
	switch {
	case contentLength >= 0:
		req.ContentLength = contentLength
	case contentLength == -2:
		req.ContentLength = -1
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	c.Params = gin.Params{
		{Key: "session_id", Value: sessionID},
		{Key: "index", Value: strconv.Itoa(index)},
	}
	ss.handler.PutUploadChunk(c)
	return w
}

func (ss *sessionSetup) getAttachment(t *testing.T, userID, fileID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/media/attachments/"+fileID, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	c.Params = gin.Params{{Key: "file_id", Value: fileID}}
	ss.handler.DownloadAttachment(c)
	return w
}

func (ss *sessionSetup) doCommit(userID, sessionID string) *httptest.ResponseRecorder {
	path := fmt.Sprintf("%s/%s/commit", pathUploadSession, sessionID)
	req := httptest.NewRequest(http.MethodPost, path, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	c.Params = gin.Params{{Key: "session_id", Value: sessionID}}
	ss.handler.CommitUploadSession(c)
	return w
}

func (ss *sessionSetup) doCancel(userID, sessionID string) *httptest.ResponseRecorder {
	path := fmt.Sprintf("%s/%s", pathUploadSession, sessionID)
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("user_id", userID)
	c.Params = gin.Params{{Key: "session_id", Value: sessionID}}
	ss.handler.CancelUploadSession(c)
	return w
}

// initOK runs a well-formed init and returns (sessionID, fileID).
func (ss *sessionSetup) initOK(t *testing.T, userID, channelID string, plaintextBytes int64) (string, string) {
	t.Helper()
	w := ss.doInit(userID, initBody(channelID, plaintextBytes))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	sessionID, _ := resp["session_id"].(string)
	fileID, _ := resp["file_id"].(string)
	require.NotEmpty(t, sessionID)
	require.NotEmpty(t, fileID)
	assert.Equal(t, float64(AttachmentChunkPlaintextBytes), resp["chunk_size"])
	assert.NotEmpty(t, resp["expires_at"])
	return sessionID, fileID
}

// fakeCiphertext builds a blob shaped exactly like a v2 envelope, WITHOUT being
// one: the server must never parse it, so the tests deliberately do not feed it
// a real header. Only the lengths are real.
func fakeCiphertext(plaintextBytes int64) []byte {
	blob := make([]byte, ChunkedCiphertextBytes(plaintextBytes))
	for i := range blob {
		blob[i] = byte(i % 251)
	}
	return blob
}

// sliceParts cuts a whole-blob byte slice into its per-index upload parts.
func sliceParts(t *testing.T, blob []byte, plaintextBytes int64) [][]byte {
	t.Helper()
	total := TotalChunksFor(plaintextBytes)
	parts := make([][]byte, 0, total)
	off := int64(0)
	for i := int64(0); i < total; i++ {
		size := attachmentPartSize(i, total, plaintextBytes)
		parts = append(parts, blob[off:off+size])
		off += size
	}
	require.Equal(t, int64(len(blob)), off, "parts must tile the blob exactly")
	return parts
}

func (ss *sessionSetup) uploadAllParts(t *testing.T, userID, sessionID string, parts [][]byte) {
	t.Helper()
	for i, p := range parts {
		w := ss.doPutChunk(userID, sessionID, i, p, -1)
		require.Equal(t, http.StatusOK, w.Code, "chunk %d body: %s", i, w.Body.String())
	}
}

// =====================================================================
// The three that are easiest to get wrong
// =====================================================================

func TestUploadSession_RoundTripsByteIdentical(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_roundtrip")

	const plaintext = int64(4096)
	blob := fakeCiphertext(plaintext)
	parts := sliceParts(t, blob, plaintext)
	require.Len(t, parts, 1)

	sessionID, fileID := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	w := ss.doCommit(userID, sessionID)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	// The commit response is byte-identical to today's UploadAttachment response
	// so the client's existing parsing is unchanged.
	assert.JSONEq(t, fmt.Sprintf(
		`{"file_id":%q,"storage_key":"attachments/%s","file_type":"file","file_size":%d}`,
		fileID, fileID, len(blob)), w.Body.String())

	// Download the completed object through the normal proxy path and compare bytes.
	rc, _, err := ss.fake.GetObject(t.Context(), "attachments/"+fileID)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, blob, got, "the stored object must be byte-identical to what was uploaded")

	// The media_files row records the CIPHERTEXT size, matching the legacy path.
	var size int64
	var tier, keyVersion int
	require.NoError(t, ss.db.QueryRow(
		`SELECT file_size, media_tier, key_version FROM media_files WHERE id = $1`, fileID,
	).Scan(&size, &tier, &keyVersion))
	assert.Equal(t, int64(len(blob)), size)
	assert.Equal(t, MediaTierE2EE, tier)
	assert.Equal(t, 1, keyVersion)
}

func TestInitUploadSession_PersistsEnvelopeVersionV3(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "v3-version")
	body := initBody(channelID, 4096)
	body["envelope_version"] = 3
	w := ss.doInit(userID, body)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	sessionID, ok := parseBody(t, w)["session_id"].(string)
	require.True(t, ok)
	stored := ss.rdb.HGet(t.Context(), attachSessionKey(sessionID), "envelope_version").Val()
	assert.Equal(t, "3", stored)
}

func TestInitUploadSession_VersionSelectsWriteBackend(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    *int
		wantID     string
		wantLegacy bool
	}{
		{"omitted envelope version uses legacy", nil, "", true},
		{"envelope version 2 uses legacy", func() *int { v := 2; return &v }(), "", true},
		{"envelope version 3 uses vendor backend", func() *int { v := 3; return &v }(), "r2-useast", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := setupSessionTest(t)
			vendor := newFakeMultipartStore()
			ss.handler.SetWriteRouter(stubWriteRouter{
				tier1: ss.fake, attachment: vendor, backendID: "r2-useast",
			})
			userID, _, channelID := ss.channelContext(t, "version-backend")
			body := initBody(channelID, 4096)
			if tc.version != nil {
				body["envelope_version"] = *tc.version
			}
			w := ss.doInit(userID, body)
			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
			sessionID, ok := parseBody(t, w)["session_id"].(string)
			require.True(t, ok)
			assert.Equal(t, tc.wantID, ss.rdb.HGet(t.Context(), attachSessionKey(sessionID), "backend").Val())
			if tc.wantLegacy {
				assert.Equal(t, 1, ss.fake.openUploadCount())
				assert.Equal(t, 0, vendor.openUploadCount())
			} else {
				assert.Equal(t, 0, ss.fake.openUploadCount())
				assert.Equal(t, 1, vendor.openUploadCount())
			}
		})
	}
}

func TestInitUploadSession_EnvelopeVersionCompatibilityMatrix(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	tests := []struct {
		name     string
		declared *int
		wantCode int
		wantHash string
	}{
		{name: "omitted means v2", wantCode: http.StatusCreated, wantHash: "2"},
		{name: "zero means v2", declared: intPtr(0), wantCode: http.StatusCreated, wantHash: "2"},
		{name: "explicit v2", declared: intPtr(2), wantCode: http.StatusCreated, wantHash: "2"},
		{name: "explicit v3", declared: intPtr(3), wantCode: http.StatusCreated, wantHash: "3"},
		{name: "unsupported version", declared: intPtr(4), wantCode: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ss := setupSessionTest(t)
			userID, _, channelID := ss.channelContext(t, "version-matrix")
			body := initBody(channelID, 4096)
			if tc.declared != nil {
				body["envelope_version"] = *tc.declared
			}
			w := ss.doInit(userID, body)
			require.Equal(t, tc.wantCode, w.Code, "body: %s", w.Body.String())
			if tc.wantHash == "" {
				return
			}
			sessionID, ok := parseBody(t, w)["session_id"].(string)
			require.True(t, ok)
			assert.Equal(t, tc.wantHash,
				ss.rdb.HGet(t.Context(), attachSessionKey(sessionID), "envelope_version").Val())
		})
	}
}

func TestUploadSession_V3VersionDrivesUniformPartGeometry(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "v3-route")
	const plaintext = 2*AttachmentChunkPlaintextBytes + 1
	const totalChunks = 3
	body := initBody(channelID, plaintext)
	body["total_chunks"] = totalChunks
	body["declared_ciphertext_bytes"] = AttachmentEnvelopeHeaderBytes +
		AttachmentChunkOverheadBytes*totalChunks + plaintext
	body["envelope_version"] = 3
	w := ss.doInit(userID, body)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	sessionID, ok := parseBody(t, w)["session_id"].(string)
	require.True(t, ok)
	part0 := make([]byte, 8_388_636)
	w = ss.doPutChunk(userID, sessionID, 0, part0, int64(len(part0)))
	assert.Equal(t, http.StatusOK, w.Code,
		"v3 part 0 must exclude the 28-byte envelope header from its plaintext budget")
}

func TestInitUploadSession_V3FirstChunkCapacityBoundaries(t *testing.T) {
	const first = AttachmentChunkPlaintextBytes - AttachmentEnvelopeHeaderBytes
	for _, tc := range []struct {
		name  string
		pt    int64
		total int64
	}{
		{name: "exact capacity", pt: first, total: 1},
		{name: "one byte over", pt: first + 1, total: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := setupSessionTest(t)
			userID, _, channelID := ss.channelContext(t, "v3-boundary")
			body := initBody(channelID, tc.pt)
			body["total_chunks"] = tc.total
			body["declared_ciphertext_bytes"] = AttachmentEnvelopeHeaderBytes +
				AttachmentChunkOverheadBytes*tc.total + tc.pt
			body["envelope_version"] = 3
			w := ss.doInit(userID, body)
			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
			sessionID, ok := parseBody(t, w)["session_id"].(string)
			require.True(t, ok)
			assert.Equal(t, "3", ss.rdb.HGet(t.Context(),
				attachSessionKey(sessionID), "envelope_version").Val())
		})
	}
}

func TestInitUploadSession_V3PremiumCeiling(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "v3-ceiling")
	ss.handler.serverTiers = serverTierStub{tier: "mach3"}
	const plaintext = 512 * 1024 * 1024
	const totalChunks = 65
	body := initBody(channelID, plaintext)
	body["total_chunks"] = totalChunks
	body["declared_ciphertext_bytes"] = AttachmentEnvelopeHeaderBytes +
		AttachmentChunkOverheadBytes*totalChunks + plaintext
	body["envelope_version"] = 3
	w := ss.doInit(userID, body)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	sessionID, ok := parseBody(t, w)["session_id"].(string)
	require.True(t, ok)
	assert.Equal(t, "3", ss.rdb.HGet(t.Context(),
		attachSessionKey(sessionID), "envelope_version").Val())
}

func TestUploadSession_MultiChunkRoundTrip(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_multichunk")

	plaintext := AttachmentChunkPlaintextBytes + 100
	blob := fakeCiphertext(plaintext)
	parts := sliceParts(t, blob, plaintext)
	require.Len(t, parts, 2)
	// Part 0 carries the envelope header; part 1 does not.
	assert.Equal(t, int(AttachmentEnvelopeHeaderBytes+AttachmentChunkOverheadBytes+AttachmentChunkPlaintextBytes), len(parts[0]))
	assert.Equal(t, int(AttachmentChunkOverheadBytes+100), len(parts[1]))

	sessionID, fileID := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	w := ss.doCommit(userID, sessionID)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	rc, _, err := ss.fake.GetObject(t.Context(), "attachments/"+fileID)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, blob, got)
}

// A 403 confirms the session exists. 404 does not. This distinction is the test.
func TestUploadSession_NonOwnerGets404Not403(t *testing.T) {
	ss := setupSessionTest(t)
	owner, serverID, channelID := ss.channelContext(t, "sess_owner")
	stranger := ss.createTestUser(t, "sess_stranger")
	// The stranger is a fully-entitled member of the same server, so the ONLY
	// thing separating them from the session is ownership.
	_, err := ss.db.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
		serverID, stranger)
	require.NoError(t, err)

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, owner, channelID, plaintext)

	for name, w := range map[string]*httptest.ResponseRecorder{
		"chunk":  ss.doPutChunk(stranger, sessionID, 0, parts[0], -1),
		"commit": ss.doCommit(stranger, sessionID),
	} {
		assert.Equal(t, http.StatusNotFound, w.Code, "%s must 404, never 403: %s", name, w.Body.String())
		assert.NotContains(t, w.Body.String(), "denied",
			"%s: the body must not hint that the session exists", name)
	}

	// The owner's session is untouched by the stranger's probing.
	ss.uploadAllParts(t, owner, sessionID, parts)
	require.Equal(t, http.StatusCreated, ss.doCommit(owner, sessionID).Code)
}

// ListObjectParts is authoritative at commit — NOT the client's own record.
func TestUploadSession_CommitWithMissingPartsReturns409WithIndices(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_missing")

	plaintext := AttachmentChunkPlaintextBytes + 100
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, fileID := ss.initOK(t, userID, channelID, plaintext)

	// Upload BOTH parts, so the session's own record says the upload is complete...
	ss.uploadAllParts(t, userID, sessionID, parts)
	require.Equal(t, int64(2), ss.rdb.SCard(t.Context(), attachSessionPartsKey(sessionID)).Val())

	// ...then take part 2 away behind the handler's back. The session record and
	// the store now disagree, and the store is the one that counts.
	uploadID := ss.rdb.HGet(t.Context(), attachSessionKey(sessionID), "upload_id").Val()
	require.NotEmpty(t, uploadID)
	ss.fake.dropStoredPart(uploadID, 2)

	w := ss.doCommit(userID, sessionID)
	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	assert.Equal(t, []any{float64(1)}, resp["missing"],
		"missing must carry the WIRE index (0-based), not the S3 part number")

	// Nothing was completed: a 409 must not leave a half-object readable. The key
	// queried is THIS SESSION'S -- "attachments/whatever" was never a storage key
	// in this test, so its absence was trivially true and proved nothing (#2931).
	_, _, err := ss.fake.GetObject(t.Context(), "attachments/"+fileID)
	assert.Error(t, err, "a 409 commit must not have produced a readable object")
	assert.Equal(t, 1, ss.fake.openUploadCount(), "the multipart upload stays open for a retry")

	// Re-PUT the missing index and the same commit now succeeds.
	require.Equal(t, http.StatusOK, ss.doPutChunk(userID, sessionID, 1, parts[1], -1).Code)
	require.Equal(t, http.StatusCreated, ss.doCommit(userID, sessionID).Code)
}

func TestUploadSession_CommitWithNoPartsAtAllReturns409(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_noparts")

	plaintext := AttachmentChunkPlaintextBytes + 100
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)

	w := ss.doCommit(userID, sessionID)
	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []any{float64(0), float64(1)}, parseBody(t, w)["missing"])
}

// =====================================================================
// Failure taxonomy — one test per status
// =====================================================================

func TestUploadSession_Init400OnArithmeticAndContextErrors(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_400")
	conversationID := ss.createTestDMConversation(t, userID, ss.createTestUser(t, "sess_400_peer"))

	mutate := func(f func(map[string]any)) map[string]any {
		b := initBody(channelID, 4096)
		f(b)
		return b
	}

	cases := map[string]map[string]any{
		"no context":         mutate(func(b map[string]any) { delete(b, "channel_id") }),
		"both contexts":      mutate(func(b map[string]any) { b["conversation_id"] = conversationID }),
		"channel not uuid":   mutate(func(b map[string]any) { b["channel_id"] = valueNotUUID }),
		"key_version zero":   mutate(func(b map[string]any) { b["key_version"] = 0 }),
		"key_version absent": mutate(func(b map[string]any) { delete(b, "key_version") }),
		"chunk_size wrong":   mutate(func(b map[string]any) { b["chunk_size"] = 4194304 }),
		"total_chunks zero":  mutate(func(b map[string]any) { b["total_chunks"] = 0 }),
		"total_chunks disagrees with the arithmetic": mutate(func(b map[string]any) {
			b["total_chunks"] = 7
		}),
		"declared bytes too small for the chunk count": mutate(func(b map[string]any) {
			b["declared_ciphertext_bytes"] = AttachmentEnvelopeHeaderBytes + AttachmentChunkOverheadBytes
		}),
		"declared bytes negative": mutate(func(b map[string]any) { b["declared_ciphertext_bytes"] = -1 }),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := ss.doInit(userID, body)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		})
	}
	assert.Equal(t, 0, ss.fake.openUploadCount(), "no rejected init may open a multipart upload")
}

func TestUploadSession_Chunk400OnBadIndex(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_badindex")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)

	for _, idx := range []int{-1, 1, 99} {
		w := ss.doPutChunk(userID, sessionID, idx, parts[0], -1)
		assert.Equal(t, http.StatusBadRequest, w.Code, "index %d body: %s", idx, w.Body.String())
	}
}

func TestUploadSession_Chunk400OnTruncatedBody(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_truncated")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)

	// Content-Length claims the full part; the body stops early. A backend error
	// caused by the CLIENT's truncation is a 400, not a 502 blamed on the store.
	full := int64(len(parts[0]))
	w := ss.doPutChunk(userID, sessionID, 0, parts[0][:full-10], full)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

func TestUploadSession_Chunk413OnOversizePart(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_oversize")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)

	oversized := append(append([]byte{}, parts[0]...), make([]byte, 64)...)
	w := ss.doPutChunk(userID, sessionID, 0, oversized, -1)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())

	// And a body of unknown length is refused rather than read blind.
	//
	// Assert the MESSAGE, not just the 400: the `declared != expected` case
	// below it also yields 400, so a status-only assertion cannot tell which
	// guard fired and stays green if the unknown-length case is deleted. The
	// status is what matters for safety; the message is what makes the test
	// specific to the guard it names.
	w = ss.doPutChunk(userID, sessionID, 0, parts[0], -2)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "Content-Length is required",
		"an unknown-length body must be refused by the Content-Length guard, "+
			"not incidentally by the arithmetic mismatch")
}

func TestUploadSession_Init413OverEntitlementCap(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_413")

	// One byte over the free-tier PLAINTEXT allowance, expressed in ciphertext —
	// the unit conversion is the whole point of the fix this route carries.
	over := int64(33_554_432) + 1
	w := ss.doInit(userID, initBody(channelID, over))
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, float64(33_554_432), parseBody(t, w)["max_size"])

	// Exactly at the cap is admitted: the dead band is closed, not merely narrowed.
	w = ss.doInit(userID, initBody(channelID, 33_554_432))
	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}

func TestUploadSession_Commit401OnCredentialEpochAdvance(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_401")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	// The uploader's credential epoch advances mid-session (#2201). The request
	// carries no cred_epoch claim, so the fence no longer matches.
	_, err := ss.db.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, "epoch-2", userID)
	require.NoError(t, err)

	w := ss.doCommit(userID, sessionID)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())

	var rows int
	require.NoError(t, ss.db.QueryRow(
		`SELECT COUNT(*) FROM media_files WHERE uploader_id = $1`, userID).Scan(&rows))
	assert.Zero(t, rows, "a fenced commit must not leave a media_files row")
}

func TestUploadSession_Commit403WhenMembershipLost(t *testing.T) {
	ss := setupSessionTest(t)
	userID, serverID, channelID := ss.channelContext(t, "sess_403")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	// Kicked between init and commit.
	_, err := ss.db.Exec(
		`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, userID)
	require.NoError(t, err)

	w := ss.doCommit(userID, sessionID)
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

func TestUploadSession_410OnHardExpiry(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_410")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)

	// A client that keeps PUTting refreshes the 30-minute sliding window forever;
	// the 2-hour hard cap is what actually ends the session.
	backdated := time.Now().Add(-3 * time.Hour).Unix()
	require.NoError(t, ss.rdb.HSet(
		t.Context(), attachSessionKey(sessionID), "created_at", backdated).Err())

	w := ss.doPutChunk(userID, sessionID, 0, parts[0], -1)
	assert.Equal(t, http.StatusGone, w.Code, "chunk body: %s", w.Body.String())

	// Expiry reclaims the bytes rather than stranding them.
	assert.Zero(t, ss.fake.openUploadCount())
	assert.Zero(t, ss.rdb.Exists(t.Context(), attachSessionKey(sessionID)).Val())

	// The FIRST request after the hard cap gets 410 and consumes the session.
	// Every later one gets 404, because the session is genuinely gone -- and
	// that is deliberate, not a gap.
	//
	// Returning 410 here instead would need a tombstone, and a tombstone that
	// answers 410 to anyone tells a non-owner that a session once existed at
	// that id. That is exactly what the 404-not-403 rule on the ownership check
	// exists to prevent, so buying a nicer status code costs a real disclosure
	// boundary. An owner-aware tombstone could avoid the leak, but it adds a
	// key, a TTL and a second lookup on every 404 path to improve a flow no
	// client performs: a 410 on a chunk PUT makes the client restart with a
	// fresh fileNonce, so it never goes on to commit that session.
	w = ss.doCommit(userID, sessionID)
	assert.Equal(t, http.StatusNotFound, w.Code, "commit body: %s", w.Body.String())
}

// The flow a client actually reaches: the last chunk lands, the client waits
// out the two-hour cap, then commits. Commit is the FIRST request to observe
// expiry, so it must be the one that returns 410 and reclaims the bytes. The
// chunk-then-commit test above never exercised this, because its PUT consumed
// the session first.
func TestUploadSession_CommitIsThe410WhenItObservesExpiryFirst(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_commit_expiry")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	backdated := time.Now().Add(-3 * time.Hour).Unix()
	require.NoError(t, ss.rdb.HSet(
		t.Context(), attachSessionKey(sessionID), "created_at", backdated).Err())

	w := ss.doCommit(userID, sessionID)
	assert.Equal(t, http.StatusGone, w.Code, "commit body: %s", w.Body.String())
	assert.Zero(t, ss.fake.openUploadCount(), "expiry must reclaim the parts")
	assert.Zero(t, ss.rdb.Exists(t.Context(), attachSessionKey(sessionID)).Val())
}

func TestUploadSession_404OnUnknownSession(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, _ := ss.channelContext(t, "sess_404")

	for _, id := range []string{"nope", "", "../../etc/passwd", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		w := ss.doCommit(userID, id)
		assert.Equal(t, http.StatusNotFound, w.Code, "session %q body: %s", id, w.Body.String())
	}
}

func TestUploadSession_Init429OnOpenSessionCap(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_429_open")

	for i := 0; i < maxOpenUploadSessions; i++ {
		w := ss.doInit(userID, initBody(channelID, 2048))
		require.Equal(t, http.StatusCreated, w.Code, "session %d body: %s", i, w.Body.String())
	}
	w := ss.doInit(userID, initBody(channelID, 2048))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, maxOpenUploadSessions, ss.fake.openUploadCount(),
		"the refused init must not open a multipart upload")
}

func TestUploadSession_Init429OnByteBudget(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_429_budget")

	// Burn the budget directly rather than by uploading a gigabyte.
	require.NoError(t, ss.rdb.IncrBy(
		t.Context(), attachBudgetKey(userID), uploadBudgetBytesPerWindow).Err())

	w := ss.doInit(userID, initBody(channelID, 2048))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "body: %s", w.Body.String())
	assert.Zero(t, ss.fake.openUploadCount())
}

func TestUploadSession_502OnObjectStoreError(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_502")

	ss.fake.newErr = errFakeStore
	w := ss.doInit(userID, initBody(channelID, 2048))
	assert.Equal(t, http.StatusBadGateway, w.Code, "init body: %s", w.Body.String())
	ss.fake.newErr = nil

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)

	ss.fake.putPartErr = errFakeStore
	w = ss.doPutChunk(userID, sessionID, 0, parts[0], -1)
	assert.Equal(t, http.StatusBadGateway, w.Code, "chunk body: %s", w.Body.String())
	ss.fake.putPartErr = nil

	ss.uploadAllParts(t, userID, sessionID, parts)

	ss.fake.listErr = errFakeStore
	w = ss.doCommit(userID, sessionID)
	assert.Equal(t, http.StatusBadGateway, w.Code, "list body: %s", w.Body.String())
	ss.fake.listErr = nil

	ss.fake.completeErr = errFakeStore
	w = ss.doCommit(userID, sessionID)
	assert.Equal(t, http.StatusBadGateway, w.Code, "complete body: %s", w.Body.String())
}

func TestUploadSession_503WhenStoreOrRedisUnavailable(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_503")

	t.Run("object store not configured", func(t *testing.T) {
		ss.handler.store = nil
		defer func() { ss.handler.store = ss.fake }()
		w := ss.doInit(userID, initBody(channelID, 2048))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	})

	t.Run("redis not configured", func(t *testing.T) {
		ss.handler.SetSessionRedis(nil)
		defer ss.handler.SetSessionRedis(ss.rdb)
		w := ss.doInit(userID, initBody(channelID, 2048))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	})

	t.Run("redis unreachable", func(t *testing.T) {
		dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
		t.Cleanup(func() { _ = dead.Close() })
		ss.handler.SetSessionRedis(dead)
		defer ss.handler.SetSessionRedis(ss.rdb)

		w := ss.doInit(userID, initBody(channelID, 2048))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, "init body: %s", w.Body.String())
		w = ss.doCommit(userID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, "commit body: %s", w.Body.String())
	})
}

// =====================================================================
// Cancel
// =====================================================================

func TestUploadSession_CancelAbortsAndIsIdempotent(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sess_cancel")

	const plaintext = int64(2048)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	w := ss.doCancel(userID, sessionID)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Zero(t, ss.fake.openUploadCount(), "cancel must abort the multipart upload")
	assert.Zero(t, ss.rdb.Exists(t.Context(), attachSessionKey(sessionID)).Val())
	assert.Zero(t, ss.rdb.Exists(t.Context(), attachSessionPartsKey(sessionID)).Val())
	assert.Zero(t, ss.rdb.SCard(t.Context(), attachUserSessionsKey(userID)).Val())

	// Repeating it is a no-op, not an error.
	assert.Equal(t, http.StatusNoContent, ss.doCancel(userID, sessionID).Code)

	// The cancelled session is gone for its owner too.
	assert.Equal(t, http.StatusNotFound, ss.doCommit(userID, sessionID).Code)
}

// A non-owner's DELETE gets the SAME answer as a DELETE for a session that
// never existed. Returning 404 here (and 204 for "never existed") would rebuild
// the existence oracle the 404-not-403 rule exists to close.
func TestUploadSession_CancelByNonOwnerLeaksNothingAndDeletesNothing(t *testing.T) {
	ss := setupSessionTest(t)
	owner, _, channelID := ss.channelContext(t, "sess_cancel_owner")
	stranger := ss.createTestUser(t, "sess_cancel_stranger")

	const plaintext = int64(2048)
	sessionID, _ := ss.initOK(t, owner, channelID, plaintext)

	ownerResp := ss.doCancel(stranger, sessionID)
	unknown := ss.doCancel(stranger, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.Equal(t, unknown.Code, ownerResp.Code, "a non-owner must not be able to tell the two apart")
	assert.Equal(t, unknown.Body.String(), ownerResp.Body.String())

	assert.Equal(t, int64(1), ss.rdb.Exists(t.Context(), attachSessionKey(sessionID)).Val(),
		"a stranger's DELETE must not destroy the owner's session")
	assert.Equal(t, 1, ss.fake.openUploadCount())
}

// =====================================================================
// Arithmetic + route shape
// =====================================================================

func TestAttachmentPartSizesTileTheBlobExactly(t *testing.T) {
	for _, plaintext := range []int64{
		1,
		4096,
		AttachmentChunkPlaintextBytes,
		AttachmentChunkPlaintextBytes + 1,
		AttachmentChunkPlaintextBytes * 3,
		268_435_456,
	} {
		total := TotalChunksFor(plaintext)
		var sum int64
		for i := int64(0); i < total; i++ {
			size := attachmentPartSize(i, total, plaintext)
			assert.Positive(t, size)
			sum += size
		}
		assert.Equal(t, ChunkedCiphertextBytes(plaintext), sum,
			"part sizes must sum to the declared ciphertext length for plaintext %d", plaintext)
	}
}

// Gin's 301 for a trailing slash fires during the router tree walk, BEFORE the
// handler chain — so a trailing-slash registration silently skips ALL
// middleware, including auth and the rate limiter. Register every path bare.
func TestUploadSessionRouteShapes(t *testing.T) {
	engine := gin.New()
	group := engine.Group("/api/v1/media")
	h := &Handler{}
	// Mirrors router.go, INCLUDING the pre-existing DELETE /media/:file_id, whose
	// wildcard shares a tree level with the new static "upload" segment.
	group.DELETE("/:file_id", func(*gin.Context) {})
	RegisterUploadSessionRoutes(group, h, nil)

	paths := make(map[string]string, 8)
	for _, r := range engine.Routes() {
		paths[r.Method+" "+r.Path] = r.Handler
		assert.False(t, len(r.Path) > 1 && r.Path[len(r.Path)-1] == '/',
			"route %s %s is registered with a trailing slash", r.Method, r.Path)
	}

	for _, want := range []string{
		"POST /api/v1/media/upload/attachment/session",
		"PUT /api/v1/media/upload/attachment/session/:session_id/chunk/:index",
		"POST /api/v1/media/upload/attachment/session/:session_id/commit",
		"DELETE /api/v1/media/upload/attachment/session/:session_id",
	} {
		assert.Contains(t, paths, want)
	}
}

// --- rate-limit posture ----------------------------------------------------
//
// A posture test that only checks the happy path is VACUOUS: it passes
// identically whether the route fails open or closed, because neither posture
// does anything until the backend errors. Both directions, or the test is
// worthless.
//
// The limiter and the handler are given DIFFERENT Redis clients on purpose. If
// both saw the dead one, a fail-open route would still answer 503 -- from the
// handler's own session lookup -- and the test could not tell "the limiter let
// it through" from "the limiter blocked it". Splitting them makes the middleware
// the only thing under test.
func postureEngine(t *testing.T, ss *sessionSetup, userID string) *gin.Engine {
	t.Helper()
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = dead.Close() })

	engine := gin.New()
	group := engine.Group("/api/v1/media")
	group.Use(func(c *gin.Context) { c.Set("user_id", userID) })
	RegisterUploadSessionRoutes(group, ss.handler, dead)
	return engine
}

func TestRateLimit_InitFailsClosedOnRedisError(t *testing.T) {
	// Init is where the byte budget is reserved. A budget that cannot be read is
	// a budget that cannot be enforced, so admitting sessions blind would let one
	// client turn a Redis blip into an unbounded object-store bill.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "postureinit")

	raw, err := json.Marshal(initBody(channelID, 4096))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, pathUploadSession, bytes.NewReader(raw))
	req.Header.Set(headerContentType, "application/json")
	w := httptest.NewRecorder()
	postureEngine(t, ss, userID).ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"init must refuse when the limiter backend is unreachable; body: %s", w.Body.String())
	// And nothing was staged: the handler never ran.
	assert.Equal(t, 0, ss.fake.openUploadCount())
}

func TestRateLimit_ChunkPutFailsOpenOnRedisError(t *testing.T) {
	// Fail-open here is deliberate. A 256 MiB upload is 32 chunk requests over
	// many minutes; failing closed would let a momentary blip destroy a
	// 20-minute upload that is 90% done, for no security gain -- the session
	// exists, its bytes are budgeted, and its ceiling is fixed.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "postureput")
	sessionID, _ := ss.initOK(t, userID, channelID, 4096)

	parts := sliceParts(t, fakeCiphertext(4096), 4096)
	path := fmt.Sprintf("%s/%s/chunk/0", pathUploadSession, sessionID)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(parts[0]))
	req.Header.Set(headerContentType, mimeOctetStream)
	w := httptest.NewRecorder()
	postureEngine(t, ss, userID).ServeHTTP(w, req)

	// The chunk was actually accepted -- the limiter did not stand in the way.
	assert.Equal(t, http.StatusOK, w.Code,
		"chunk PUT must survive a limiter-backend outage; body: %s", w.Body.String())
}

func TestRateLimit_CommitFailsClosedAndCancelFailsOpen(t *testing.T) {
	// Commit is the expensive operation and is cheap to retry, so refusing under
	// uncertainty costs one button press. Cancel is the opposite: refusing it
	// STRANDS bytes, which is what every limiter here exists to prevent.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "posturecommit")
	sessionID, _ := ss.initOK(t, userID, channelID, 4096)
	ss.uploadAllParts(t, userID, sessionID, sliceParts(t, fakeCiphertext(4096), 4096))

	engine := postureEngine(t, ss, userID)

	commitPath := fmt.Sprintf("%s/%s/commit", pathUploadSession, sessionID)
	wc := httptest.NewRecorder()
	engine.ServeHTTP(wc, httptest.NewRequest(http.MethodPost, commitPath, nil))
	assert.Equal(t, http.StatusServiceUnavailable, wc.Code, "commit body: %s", wc.Body.String())

	// The session survived the refused commit, so cancel still has work to do.
	wd := httptest.NewRecorder()
	engine.ServeHTTP(wd, httptest.NewRequest(
		http.MethodDelete, pathUploadSession+"/"+sessionID, nil))
	assert.Equal(t, http.StatusNoContent, wd.Code, "cancel body: %s", wd.Body.String())
	assert.Equal(t, 0, ss.fake.openUploadCount(), "cancel must have reclaimed the staged part")
}

// --- A10'-b: structural proof that the chunk route never multipart-parses ----
//
// gin's MaxMultipartMemory caps how much of a multipart body is held in RAM
// before ParseMultipartForm SPILLS THE REST TO DISK. That spill is the whole
// reason the legacy attachment path cannot carry 256 MiB: it writes a temp file
// per upload and holds the parsed form in memory besides.
//
// Setting it to 1 byte makes any multipart parse on this route fail or spill
// immediately. The chunk PUT still succeeding is therefore not a behavioural
// assertion but a STRUCTURAL one: the handler reads the raw request body and
// streams it to the object store, and no amount of later refactoring can
// reintroduce ParseMultipartForm here without turning this red.
//
// An assertion about the response alone could not prove this -- a handler that
// multipart-parsed and then ignored the result would answer 200 too. The cap is
// what makes the absence of the parse observable.
func TestUploadSession_ChunkRouteNeverMultipartParses(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "nomultipart")
	sessionID, _ := ss.initOK(t, userID, channelID, 4096)

	engine := gin.New()
	// One byte. Any ParseMultipartForm on this engine spills to disk at once.
	engine.MaxMultipartMemory = 1
	group := engine.Group("/api/v1/media")
	group.Use(func(c *gin.Context) { c.Set("user_id", userID) })
	RegisterUploadSessionRoutes(group, ss.handler, ss.rdb)

	parts := sliceParts(t, fakeCiphertext(4096), 4096)
	path := fmt.Sprintf("%s/%s/chunk/0", pathUploadSession, sessionID)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(parts[0]))
	req.Header.Set(headerContentType, mimeOctetStream)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	// And the bytes really landed: a handler that skipped the body entirely
	// would also avoid the parse, and would also answer 200.
	require.Equal(t, 1, ss.fake.openUploadCount())
	require.Equal(t, []int64{int64(len(parts[0]))}, ss.fake.storedPartSizes(),
		"the chunk body must have reached the object store intact")
}

// storedPartSizes returns the sizes of every part staged across all open
// uploads, in part order. A handler that skipped the body entirely would also
// avoid a multipart parse, and would also answer 200 -- this is what separates
// "never parsed" from "never read".
func (f *fakeMultipartStore) storedPartSizes() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sizes []int64
	for _, up := range f.uploads {
		for n := 1; n <= len(up.parts); n++ {
			if p, ok := up.parts[n]; ok {
				sizes = append(sizes, p.Size)
			}
		}
	}
	return sizes
}

// S3 hard-limits a multipart upload to 10,000 parts, so a session declaring
// more can never complete on ANY backend ADR-0024 commits to. Rejecting at init
// turns a store failure at part 10,001 -- after 10,000 have already been
// uploaded -- into a diagnosable 400 before any bytes move.
//
// It also bounds total_chunks independently of the entitlement cap, which is
// what makes the int(index) narrowing in PutUploadChunk provably safe: the
// index is validated against total_chunks, so a bound here is a bound there.
func TestUploadSession_Init400OnAbsurdChunkCount(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "chunkbound")

	// The declared size must be CONSISTENT with the absurd chunk count, or the
	// pre-existing arithmetic check rejects the request first and this proves
	// nothing about the new bound. A guard placed behind another guard is
	// vacuous unless the test satisfies the one in front of it -- the first
	// version of this test did not, and stayed green with the bound deleted.
	consistent := func(total int64) map[string]any {
		body := initBody(channelID, 4096)
		body["total_chunks"] = total
		body["declared_ciphertext_bytes"] = ChunkedCiphertextBytes(total * AttachmentChunkPlaintextBytes)
		return body
	}

	for _, tc := range []struct {
		name  string
		total int64
	}{
		{"just past the S3 ceiling", maxMultipartParts + 1},
		{"far past it", maxMultipartParts * 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ss.doInit(userID, consistent(tc.total))
			assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), "total_chunks must be between",
				"the refusal must name the chunk bound, not some other check that fired first")
			assert.Equal(t, 0, ss.fake.openUploadCount(),
				"no multipart upload may be started for a rejected session")
		})
	}

	// Zero and negative counts cannot be made arithmetically consistent, so they
	// are asserted only as "rejected" -- which guard catches them is not this
	// test's claim.
	for _, total := range []int64{0, -1} {
		body := initBody(channelID, 4096)
		body["total_chunks"] = total
		w := ss.doInit(userID, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "total=%d body: %s", total, w.Body.String())
	}
}

func TestUploadSession_InitAcceptsTheMaximumPartCount(t *testing.T) {
	// The bound must be inclusive: a session at exactly the ceiling is legal, and
	// an off-by-one here would refuse the largest upload the format allows.
	// Rejected for SIZE (413), not for the chunk count -- which is what proves
	// the count itself passed validation.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "chunkboundmax")

	body := initBody(channelID, 4096)
	body["total_chunks"] = maxMultipartParts
	body["declared_ciphertext_bytes"] = ChunkedCiphertextBytes(
		int64(maxMultipartParts) * AttachmentChunkPlaintextBytes)
	w := ss.doInit(userID, body)

	assert.NotEqual(t, http.StatusBadRequest, w.Code,
		"the ceiling itself must not be rejected as a bad chunk count; body: %s", w.Body.String())
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
}

// --- byte-budget refund on init failure (CodeRabbit, PR 2931) --------------

func budgetUsed(t *testing.T, ss *sessionSetup, userID string) int64 {
	t.Helper()
	v, err := ss.rdb.Get(context.Background(), "attach_budget:"+userID).Int64()
	if err == redis.Nil {
		return 0
	}
	require.NoError(t, err)
	return v
}

func TestUploadSession_RefundsBudgetWhenTheConcurrencyCapRejects(t *testing.T) {
	// The budget is charged BEFORE the slot is claimed. Without a refund, a user
	// who is merely at their concurrency limit burns ingress budget for a full
	// window on a session that never existed -- and then starts collecting 429s
	// for uploads that should have been allowed.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "budgetrefund")

	for i := 0; i < maxOpenUploadSessions; i++ {
		ss.initOK(t, userID, channelID, 4096)
	}
	atCap := budgetUsed(t, ss, userID)

	w := ss.doInit(userID, initBody(channelID, 4096))
	require.Equal(t, http.StatusTooManyRequests, w.Code, "body: %s", w.Body.String())

	assert.Equal(t, atCap, budgetUsed(t, ss, userID),
		"a session refused for concurrency must not consume ingress budget")
}

func TestUploadSession_RefundsBudgetWhenTheObjectStoreRefuses(t *testing.T) {
	// Nothing was uploaded and no session exists, so the reservation is pure loss
	// if it is not returned.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "budgetstore")

	before := budgetUsed(t, ss, userID)
	ss.fake.newErr = errFakeStore
	w := ss.doInit(userID, initBody(channelID, 4096))
	ss.fake.newErr = nil

	require.Equal(t, http.StatusBadGateway, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, before, budgetUsed(t, ss, userID),
		"a session the object store refused must not consume ingress budget")
}

func TestUploadSession_DoesNotRefundOnCancel(t *testing.T) {
	// The asymmetry is deliberate and is the security-relevant half of this fix.
	// The budget bounds INGRESS BYTES PER WINDOW, so bytes the client actually
	// sent are spent whatever the session's outcome. Refunding on cancel would
	// make "init 1 GiB, send 900 MiB, cancel, repeat" an unbounded ingress
	// channel -- defeating the very control it appears to maintain.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "budgetcancel")

	sessionID, _ := ss.initOK(t, userID, channelID, 4096)
	charged := budgetUsed(t, ss, userID)
	require.Greater(t, charged, int64(0), "init must charge the budget")

	require.Equal(t, http.StatusNoContent, ss.doCancel(userID, sessionID).Code)

	assert.Equal(t, charged, budgetUsed(t, ss, userID),
		"cancelling must NOT hand back ingress budget; a user who cancels has still used the pipe")
}

func TestUploadSession_ChunkIndexIsBoundedByTheSessionChunkCount(t *testing.T) {
	// The point-of-use maxMultipartParts check in PutUploadChunk is UNREACHABLE
	// once decodeUploadSession bounds total_chunks: index is compared against
	// total_chunks first, and total_chunks can no longer exceed the ceiling. It
	// is retained as the locally-provable form CodeQL can see (alert 1305 lives
	// behind a struct field the analysis does not follow), and this test asserts
	// the bound that IS reachable -- the chunk count itself.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "indexbound")
	sessionID, _ := ss.initOK(t, userID, channelID, 4096)

	chunk := fakeCiphertext(4096)
	// A 4096-byte file is one chunk, so index 1 is the first invalid one.
	w := ss.doPutChunk(userID, sessionID, 1, chunk, int64(len(chunk)))

	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid chunk index",
		"the refusal must come from the INDEX guard, not from some other 400 that fires first")
	assert.Empty(t, ss.fake.storedPartSizes(), "no part may be written for an out-of-range index")
}

func TestUploadSession_RejectsACorruptSessionRecordAtDecode(t *testing.T) {
	// The init-time bound only covers records THIS init path wrote.
	// decodeUploadSession parses whatever the Redis hash holds, so bounding at
	// decode makes the constraint a property of every reader of the record --
	// commit and cancel included -- rather than of the one handler that was
	// patched. A corrupt or externally written record must not decode at all.
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{"absurd chunk count", "total_chunks", "1099511627776"},
		{"zero chunk count", "total_chunks", "0"},
		{"negative chunk count", "total_chunks", "-1"},
		{"negative plaintext", "plaintext_bytes", "-1"},
		{"negative ciphertext", "ciphertext_bytes", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := setupSessionTest(t)
			userID, _, channelID := ss.channelContext(t, "decode"+tc.field+tc.value)
			sessionID, _ := ss.initOK(t, userID, channelID, 4096)

			require.NoError(t, ss.rdb.HSet(context.Background(),
				"attach_sess:"+sessionID, tc.field, tc.value).Err())

			// Every route that decodes the record must refuse it, not just the
			// chunk handler.
			chunk := fakeCiphertext(4096)
			put := ss.doPutChunk(userID, sessionID, 0, chunk, int64(len(chunk)))
			assert.NotEqual(t, http.StatusOK, put.Code, "chunk PUT body: %s", put.Body.String())

			commit := ss.doCommit(userID, sessionID)
			assert.NotEqual(t, http.StatusCreated, commit.Code, "commit body: %s", commit.Body.String())

			assert.Empty(t, ss.fake.storedPartSizes(), "a corrupt record must move no bytes")
		})
	}
}

func TestUploadSession_RePuttingOneIndexIsMetered(t *testing.T) {
	// THE HOLE A RED-TEAM PoC CONFIRMED: the byte budget is charged once at init
	// on DECLARED bytes and never consulted again, and PutObjectPart OVERWRITES,
	// so re-PUTting index 0 forever pushed unlimited real bytes for free.
	// Measured 32.5x over the reservation in one minute; projected ~281 GiB from
	// a 32 MiB reservation over one session's 2h TTL. Storage never grows, so no
	// storage-side backstop could see it.
	//
	// The refund comment reasoned carefully about "init, send, cancel, repeat"
	// and genuinely closed that loop. This one is cheaper and was wide open --
	// the door I was not looking at.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "ingressmeter")

	const plaintext = int64(4096)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	part := sliceParts(t, fakeCiphertext(plaintext), plaintext)[0]

	accepted, refused := 0, 0
	for i := 0; i < 12; i++ {
		w := ss.doPutChunk(userID, sessionID, 0, part, int64(len(part)))
		switch w.Code {
		case http.StatusOK:
			accepted++
		case http.StatusTooManyRequests:
			refused++
			assert.Contains(t, w.Body.String(), "ingress allowance",
				"the refusal must name the ingress meter, not some other 429")
		default:
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}

	assert.Equal(t, uploadSessionIngressFactor, accepted,
		"exactly the allowance should be accepted -- one whole upload plus repair headroom")
	assert.Positive(t, refused, "the flood must be refused, not silently absorbed")
}

func TestUploadSession_IngressAllowanceStillPermitsRepair(t *testing.T) {
	// The meter must not break the DESIGNED repair path: a 409 names missing
	// indices and the client re-sends them. A factor of 1 would refuse the first
	// legitimate repair, which is why the allowance is a multiple.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "ingressrepair")

	const plaintext = int64(4096)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)

	// Upload, then re-upload the whole file once more as a repair would.
	ss.uploadAllParts(t, userID, sessionID, parts)
	ss.uploadAllParts(t, userID, sessionID, parts)

	// And commit still succeeds afterwards.
	assert.Equal(t, http.StatusCreated, ss.doCommit(userID, sessionID).Code)
}

func TestUploadSession_ChunkPutRefusedAfterMembershipRevoked(t *testing.T) {
	// A red-team PoC accepted 5 chunk PUTs after the uploader was removed from
	// the server. Commit correctly refused, so nothing could land -- but the
	// INGRESS channel outlived the authorization that opened it for up to the
	// session's 2h TTL, which is precisely what the ingress ledger exists to
	// bound. Authorization is re-checked at the source now, not only at commit.
	ss := setupSessionTest(t)
	userID, serverID, channelID := ss.channelContext(t, "revokedchunk")

	const plaintext = int64(4096)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	part := sliceParts(t, fakeCiphertext(plaintext), plaintext)[0]

	// One good PUT proves the session works before revocation.
	require.Equal(t, http.StatusOK, ss.doPutChunk(userID, sessionID, 0, part, int64(len(part))).Code)

	_, err := ss.db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID)
	require.NoError(t, err)

	w := ss.doPutChunk(userID, sessionID, 0, part, int64(len(part)))
	assert.Equal(t, http.StatusForbidden, w.Code,
		"a revoked member must not keep pushing bytes; body: %s", w.Body.String())
}

func TestUploadSession_MaxBytesReaderStopsAnOverLongBody(t *testing.T) {
	// PROVEN UNCOVERED: deleting http.MaxBytesReader entirely left the whole
	// chunk + commit suite green, because every existing case is decided by the
	// Content-Length switch BEFORE the body is read -- the 413 test declares more
	// than expected, the truncated test declares exactly and sends less. Nothing
	// sent a body LONGER than a truthful Content-Length, which is the lying-length
	// case the guard's own comment names and the only path that reaches
	// errors.As(body.err, &tooLarge).
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "maxbytes")

	const plaintext = int64(4096)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	part := sliceParts(t, fakeCiphertext(plaintext), plaintext)[0]

	// Truthful-looking Content-Length, body 4 KiB longer than it claims.
	oversized := append(append([]byte{}, part...), make([]byte, 4096)...)
	w := ss.doPutChunk(userID, sessionID, 0, oversized, int64(len(part)))

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, ss.fake.storedPartSizes(),
		"no part may be written from a body that outran its declared length")
}

func TestUploadSession_CommitRejectsAPartStoredAtTheWrongSize(t *testing.T) {
	// The store-authoritative claim has two halves: a part that is MISSING, and a
	// part that is PRESENT but wrong. Only the first was tested -- replacing
	// `!present || part.Size != want` with `!present` kept every commit test
	// green, so the size half of "the object store is the authority" was unproven.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "wrongsize")

	const plaintext = AttachmentChunkPlaintextBytes + 100
	parts := sliceParts(t, fakeCiphertext(plaintext), plaintext)
	sessionID, _ := ss.initOK(t, userID, channelID, plaintext)
	ss.uploadAllParts(t, userID, sessionID, parts)

	// Shrink part 2 in the store without touching the session's own record.
	ss.fake.shrinkStoredPart(sessionID, 2)

	w := ss.doCommit(userID, sessionID)
	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	assert.Equal(t, []any{float64(1)}, resp["missing"],
		"a part present at the wrong size must be named missing, exactly like an absent one")
}

// seedChannelKeyEpoch advances the channel to `epoch` by ISSUING the rotation
// that produced it, which is what makes an epoch current.
//
// Not by inserting a channel_keys row: the distribution triggers (migrations
// 000105-000109) reject a wrap whose epoch was never issued, which is exactly
// the invariant that makes the issuance ledger the authority here.
func seedChannelKeyEpoch(t *testing.T, db *sql.DB, channelID, userID string, epoch int) {
	t.Helper()
	require.Greater(t, epoch, 1, "epoch 1 is the initial epoch and is never issued by a rotation")
	_, err := db.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, $2, $3, 'test seed', $4)`,
		channelID, epoch-1, epoch, userID)
	require.NoError(t, err)
}

func TestUploadSession_RejectsAnEpochThatNeverExisted(t *testing.T) {
	// The epoch is sender-attested by design (#2843), but "not invented by the
	// server" had been implemented as "accepted unchecked" -- >= 1 was the only
	// bound. That was inert while nothing read the value back. It stopped being
	// inert when the download began reflecting it as X-File-Key-Version, because
	// a viewer's client then uses it to SELECT A KEY: a fabricated 2147483647 is
	// cached as a real epoch and drives a monotonic rotation watermark, after
	// which every genuine rotation compares <= and is dropped.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "epochgate")
	seedChannelKeyEpoch(t, ss.db, channelID, userID, 3)

	const plaintext = int64(4096)

	for _, epoch := range []int{4, 99, 2147483647} {
		body := initBody(channelID, plaintext)
		body["key_version"] = epoch
		w := ss.doInit(userID, body)
		require.Equal(t, http.StatusBadRequest, w.Code,
			"epoch %d is above the context max and must be refused; body: %s", epoch, w.Body.String())
		assert.Equal(t, "unknown_epoch", parseBody(t, w)["code"])
	}

	// POSITIVE CONTROL: every HISTORICAL epoch still uploads. An upper bound
	// that also rejected real epochs would pass the loop above while breaking
	// the feature -- and an upload racing a rotation is legitimate.
	for _, epoch := range []int{1, 2, 3} {
		body := initBody(channelID, plaintext)
		body["key_version"] = epoch
		w := ss.doInit(userID, body)
		require.Equal(t, http.StatusCreated, w.Code,
			"epoch %d exists and must be accepted; body: %s", epoch, w.Body.String())
	}
}

func TestUploadSession_AcceptsEpochOneBeforeKeysAreDistributed(t *testing.T) {
	// A channel exists before its keys do: the initial wrap is hardcoded to
	// epoch 1 (storeWrappedKeys) but distribution is client-driven. Rejecting an
	// upload in that window would be a new outage for a real user, and epoch 1
	// cannot poison anything -- it is the floor of every real context, so it can
	// never exceed a monotonic watermark.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "epochfloor")
	// Deliberately NO seedChannelKeyEpoch.

	body := initBody(channelID, int64(4096))
	body["key_version"] = 1
	require.Equal(t, http.StatusCreated, ss.doInit(userID, body).Code)

	// ...but the floor is a floor, not an opening.
	body2 := initBody(channelID, int64(4096))
	body2["key_version"] = 2
	assert.Equal(t, http.StatusBadRequest, ss.doInit(userID, body2).Code)
}

func TestUploadSession_DownloadCarriesTheKeyEpoch(t *testing.T) {
	// The upload has attested key_version since #2832 and the column has always
	// held it -- it just never reached the client, so both decrypt sites used the
	// LATEST channel key. Every revocation rotates the CSK, so each rotation
	// permanently orphaned every attachment uploaded before it, and the failure
	// surfaced to the user as "may be damaged or altered": a rotation reported as
	// tampering.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "keyepoch")
	// Epoch 7 has to actually EXIST -- the attested epoch is now bounded by the
	// context's real max, so a channel six rotations in is what this describes.
	seedChannelKeyEpoch(t, ss.db, channelID, userID, 7)

	const plaintext = int64(4096)
	body := initBody(channelID, plaintext)
	body["key_version"] = 7
	w := ss.doInit(userID, body)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	resp := parseBody(t, w)
	sessionID, _ := resp["session_id"].(string)
	fileID, _ := resp["file_id"].(string)

	ss.uploadAllParts(t, userID, sessionID, sliceParts(t, fakeCiphertext(plaintext), plaintext))
	require.Equal(t, http.StatusCreated, ss.doCommit(userID, sessionID).Code)

	dl := ss.getAttachment(t, userID, fileID)
	require.Equal(t, http.StatusOK, dl.Code, "body: %s", dl.Body.String())
	assert.Equal(t, "7", dl.Header().Get("X-File-Key-Version"),
		"the client cannot select the right key without the epoch it was sealed under")
}

func TestUploadSession_RejectedInitLeavesNoImmortalKey(t *testing.T) {
	// The placeholder stakes the session key BEFORE the budget and slot checks,
	// and both reject paths return without deleting it. That is fine only
	// because the key expires. When the TTL was a separate Expire whose failure
	// merely logged, a failed Expire left a hash with no expiry that no path
	// would ever remove -- one permanent Redis key per rejected init.
	//
	// The reject path is the ONLY window where this is observable: on the happy
	// path persistSession overwrites the key with the real record and its own
	// TTL, so a test that inits successfully measures the wrong thing and stays
	// green with the Expire deleted. (It did. That is why this one exists.)
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "rejectttl")
	ctx := context.Background()

	for i := 0; i < maxOpenUploadSessions; i++ {
		w := ss.doInit(userID, initBody(channelID, int64(4096)))
		require.Equal(t, http.StatusCreated, w.Code, "setup init %d: %s", i, w.Body.String())
	}

	before, err := ss.rdb.Keys(ctx, attachSessionPrefix+"*").Result()
	require.NoError(t, err)
	seen := make(map[string]struct{}, len(before))
	for _, k := range before {
		seen[k] = struct{}{}
	}

	// One past the concurrency cap: rejected AFTER the placeholder is staked.
	w := ss.doInit(userID, initBody(channelID, int64(4096)))
	require.Equal(t, http.StatusTooManyRequests, w.Code, "body: %s", w.Body.String())

	after, err := ss.rdb.Keys(ctx, attachSessionPrefix+"*").Result()
	require.NoError(t, err)

	var orphans []string
	for _, k := range after {
		if _, ok := seen[k]; !ok {
			orphans = append(orphans, k)
		}
	}

	// POSITIVE CONTROL: the rejected init really did leave a key behind. Without
	// this the TTL loop below would pass vacuously on an empty list.
	require.Len(t, orphans, 1, "the rejected init should have staked exactly one placeholder")

	for _, k := range orphans {
		ttl, ttlErr := ss.rdb.TTL(ctx, k).Result()
		require.NoError(t, ttlErr)
		// go-redis reports "exists, no expiry" as -1 and "missing" as -2, both
		// as negative durations. Either would mean the key outlives the request.
		assert.Positive(t, ttl, "placeholder %s has no expiry and nothing will ever delete it", k)
		assert.LessOrEqual(t, ttl, uploadSessionSlidingTTL)
	}
}
