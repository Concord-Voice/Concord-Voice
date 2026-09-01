package media

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Write-rail routing coverage (ADR-0038 / #2759 unit B3).
//
// WHY THIS FILE EXISTS. write_routing.go shipped with no callers and no tests:
// the helpers below were defined, the Handler field they referenced did not
// exist, and the package therefore did not compile — so nothing could have run
// against them. Once wired, the invariant that matters most is the one no
// integration test can currently observe, because the write default still
// points at legacy: a TIER-1 write must resolve the legacy backend even when
// the tier-2 default has moved somewhere else. The fake router below is what
// makes that observable today rather than after the Wave C flip.

// stubWriteRouter lets a test point the two rails at DIFFERENT stores, which is
// the only way to tell "tier-1 is pinned to legacy" apart from "tier-1 happens
// to agree with the write default", the state the real registry is in today.
type stubWriteRouter struct {
	tier1      ObjectStore
	tier1Err   error
	attachment ObjectStore
	backendID  string
	attachErr  error
}

func (s stubWriteRouter) ResolveTier1WriteStore() (ObjectStore, error) {
	if s.tier1Err != nil {
		return nil, s.tier1Err
	}
	return s.tier1, nil
}

func (s stubWriteRouter) ResolveAttachmentWriteStore() (ObjectStore, string, error) {
	if s.attachErr != nil {
		return nil, "", s.attachErr
	}
	return s.attachment, s.backendID, nil
}

func writeRoutingContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, rec
}

func writeRoutingHandler(router WriteRouter, process ObjectStore) *Handler {
	return &Handler{log: logger.NewWithWriter(discardWriter{}), store: process, writeRouter: router}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestRequireTier1WriteStore_IgnoresTheAttachmentWriteDefault is the invariant
// this unit exists for. Profile media lives on the legacy backend permanently;
// if a tier-1 write ever followed the tier-2 default, the day that default
// flips every new avatar would land in a vendor bucket while the download path
// (which resolves tier-1 keys through the legacy store) 404s them.
func TestRequireTier1WriteStore_IgnoresTheAttachmentWriteDefault(t *testing.T) {
	legacy, vendor := newMockStore(), newMockStore()
	h := writeRoutingHandler(stubWriteRouter{
		tier1:      legacy,
		attachment: vendor,
		backendID:  "r2-useast",
	}, nil)

	c, rec := writeRoutingContext()
	got, ok := h.requireTier1WriteStore(c)

	require.True(t, ok)
	assert.Same(t, legacy, got, "a tier-1 write must resolve the legacy backend")
	assert.NotSame(t, vendor, got, "a tier-1 write must never follow the attachment write default")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestRequireAttachmentWriteStore_ReturnsTheBackendIdentifier — the identifier
// is what gets persisted on the row, so it must be the one belonging to the
// store the bytes were actually handed to.
func TestRequireAttachmentWriteStore_ReturnsTheBackendIdentifier(t *testing.T) {
	legacy, vendor := newMockStore(), newMockStore()
	h := writeRoutingHandler(stubWriteRouter{
		tier1:      legacy,
		attachment: vendor,
		backendID:  "r2-useast",
	}, nil)

	c, _ := writeRoutingContext()
	got, backendID, ok := h.requireAttachmentWriteStore(c)

	require.True(t, ok)
	assert.Same(t, vendor, got)
	assert.Equal(t, "r2-useast", backendID,
		"the row must record the backend the object was actually written to")
}

func TestRequireUploadSessionWriteStore_VersionRoutesRails(t *testing.T) {
	legacy, vendor := newMockStore(), newMockStore()
	h := writeRoutingHandler(stubWriteRouter{
		tier1: legacy, attachment: vendor, backendID: "r2-useast",
	}, nil)
	for _, tc := range []struct {
		name    string
		version EnvelopeVersion
		want    ObjectStore
		wantID  string
	}{
		{"v2 uses legacy rail", EnvelopeVersionV2, legacy, ""},
		{"v3 uses attachment rail", EnvelopeVersionV3, vendor, "r2-useast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := writeRoutingContext()
			got, id, ok := h.requireUploadSessionWriteStore(c, tc.version)
			require.True(t, ok)
			assert.Same(t, tc.want, got)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

func TestRequireUploadSessionWriteStore_RejectsV2WhenAttachmentBackendArmed(t *testing.T) {
	h := writeRoutingHandler(stubWriteRouter{
		tier1: newMockStore(), attachment: newMockStore(), backendID: "r2-useast",
	}, nil)
	h.cfg = &config.Config{AttachmentWriteBackend: "r2-useast"}

	c, rec := writeRoutingContext()
	store, backendID, ok := h.requireUploadSessionWriteStore(c, EnvelopeVersionV2)

	assert.False(t, ok)
	assert.Nil(t, store)
	assert.Empty(t, backendID)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"envelope_version must be 3 while the attachment write backend is armed","envelope_versions":[3]}`, rec.Body.String())
}

// TestRequireAttachmentWriteStore_LegacyYieldsAnEmptyIdentifier locks the NULL
// spelling: the column must stay NULL for legacy objects, never carry the
// literal string "legacy". Migration 000114 states NULL is never backfilled, so
// a stray "legacy" value would be a permanently distinct third state.
func TestRequireAttachmentWriteStore_LegacyYieldsAnEmptyIdentifier(t *testing.T) {
	legacy := newMockStore()
	h := writeRoutingHandler(stubWriteRouter{tier1: legacy, attachment: legacy, backendID: ""}, nil)

	c, _ := writeRoutingContext()
	got, backendID, ok := h.requireAttachmentWriteStore(c)

	require.True(t, ok)
	assert.Same(t, legacy, got)
	assert.Empty(t, backendID, "a legacy write must leave storage_backend NULL")
}

// TestRequireWriteStores_UnwiredRouterKeepsPreADRBehaviour — an embedder with no
// registry must behave exactly as it did before ADR-0038: every write goes to
// the single process-wide store and records NULL.
func TestRequireWriteStores_UnwiredRouterKeepsPreADRBehaviour(t *testing.T) {
	process := newMockStore()
	h := writeRoutingHandler(nil, process)

	c, _ := writeRoutingContext()
	tier1, ok := h.requireTier1WriteStore(c)
	require.True(t, ok)
	assert.Same(t, process, tier1)

	c, _ = writeRoutingContext()
	attachment, backendID, ok := h.requireAttachmentWriteStore(c)
	require.True(t, ok)
	assert.Same(t, process, attachment)
	assert.Empty(t, backendID, "an unwired router must not invent a backend identifier")
}

// TestRequireTier1WriteStore_ResolverErrorAnswers503 — a write must be refused,
// not silently retargeted, when the backend cannot be resolved.
func TestRequireTier1WriteStore_ResolverErrorAnswers503(t *testing.T) {
	fallback := newMockStore()
	h := writeRoutingHandler(stubWriteRouter{tier1Err: errors.New("legacy unavailable")}, fallback)

	c, rec := writeRoutingContext()
	got, ok := h.requireTier1WriteStore(c)

	assert.False(t, ok)
	assert.Nil(t, got)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), errMsgStorageUnavailable)
}

// TestRequireAttachmentWriteStore_ResolverErrorAnswers503 — same, and it must
// NOT fall back to the process-wide store, which is the wrong-bucket write.
func TestRequireAttachmentWriteStore_ResolverErrorAnswers503(t *testing.T) {
	h := writeRoutingHandler(stubWriteRouter{attachErr: errors.New("unknown backend")}, newMockStore())

	c, rec := writeRoutingContext()
	got, backendID, ok := h.requireAttachmentWriteStore(c)

	assert.False(t, ok)
	// Nil is the whole claim: the helper must NOT hand back `fallback`, the
	// process-wide store, which would be the wrong-bucket write.
	assert.Nil(t, got, "an unresolvable write backend must not fall back to the process store")
	assert.Empty(t, backendID)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// --- session backend threading --------------------------------------------
//
// A chunked upload opens a multipart upload against ONE backend. Every later
// operation on that session must resolve THAT backend, not the current write
// default — otherwise a flip between open and commit sends the remaining
// chunks to a bucket holding no such upload.

// TestUploadSessionBackendPtr_EmptyIsNULL — the session's empty string and the
// column's NULL are the same state, and must render as a nil *string so
// storeForRow takes its legacy arm rather than treating "" as an identifier.
func TestUploadSessionBackendPtr_EmptyIsNULL(t *testing.T) {
	assert.Nil(t, (&uploadSession{backend: ""}).backendPtr(),
		"an empty session backend must render as a NULL column, not an empty identifier")

	got := (&uploadSession{backend: "r2-useast"}).backendPtr()
	require.NotNil(t, got)
	assert.Equal(t, "r2-useast", *got)
}

// TestDecodeUploadSession_CarriesTheBackend — a record written before this
// change has no "backend" field at all; it must decode as legacy rather than
// failing, because those sessions are live across the deploy.
func TestDecodeUploadSession_CarriesTheBackend(t *testing.T) {
	base := map[string]string{
		"user_id": "u", "file_id": "f", "storage_key": "attachments/k", "upload_id": "up",
		"file_type": "file", "mime_type": "application/octet-stream",
		"key_version": "1", "total_chunks": "2",
		"plaintext_bytes": "10", "ciphertext_bytes": "20", "created_at": "1700000000",
	}

	sess, err := decodeUploadSession("sid", base)
	require.NoError(t, err)
	assert.Empty(t, sess.backend, "a pre-upgrade record with no backend field is legacy")
	assert.Nil(t, sess.backendPtr())

	withBackend := make(map[string]string, len(base)+1)
	for k, v := range base {
		withBackend[k] = v
	}
	withBackend["backend"] = "r2-useast"

	sess, err = decodeUploadSession("sid", withBackend)
	require.NoError(t, err)
	assert.Equal(t, "r2-useast", sess.backend)
	require.NotNil(t, sess.backendPtr())
	assert.Equal(t, "r2-useast", *sess.backendPtr())
}

// --- registryWriteRouter: the PRODUCTION adapter ---------------------------
//
// Everything above proves the Handler helpers against stubWriteRouter. The
// adapter that main.go actually wires (NewRegistryWriteRouter) had no test, so
// the two claims that matter in production — tier-1 resolves the legacy backend
// identifier, and a legacy attachment write yields an EMPTY identifier rather
// than the literal "legacy" — were unverified against a real *storage.Registry.
//
// storage.Client is constructed as a bare composite literal: its fields are all
// unexported, nothing here dials, and the registry only ever hands the pointer
// back.

func TestRegistryWriteRouter_Tier1ResolvesTheLegacyClient(t *testing.T) {
	legacy := &storage.Client{}
	registry := storage.NewRegistry(nil, legacy, logger.NewWithWriter(discardWriter{}))

	router := NewRegistryWriteRouter(registry)
	got, err := router.ResolveTier1WriteStore()

	require.NoError(t, err)
	assert.Same(t, legacy, got)
}

// TestRegistryWriteRouter_LegacyAttachmentWriteYieldsEmptyIdentifier is the
// NULL-spelling guard at the production adapter. Returning "legacy" here would
// write that literal into media_files.storage_backend, creating a permanent
// third state alongside NULL that migration 000114 never backfills.
func TestRegistryWriteRouter_LegacyAttachmentWriteYieldsEmptyIdentifier(t *testing.T) {
	legacy := &storage.Client{}
	registry := storage.NewRegistry(nil, legacy, logger.NewWithWriter(discardWriter{}))

	got, backendID, err := NewRegistryWriteRouter(registry).ResolveAttachmentWriteStore()

	require.NoError(t, err)
	assert.Same(t, legacy, got)
	assert.Empty(t, backendID,
		"the legacy write default must persist as NULL, never the string \"legacy\"")
	assert.NotEqual(t, string(storage.LegacyBackendID), backendID)
}

// TestRegistryWriteRouter_UnconfiguredLegacyFailsClosed — with no legacy client
// the registry registers it UNAVAILABLE, and both rails must surface that
// rather than hand back a nil store boxed in a non-nil interface.
func TestRegistryWriteRouter_UnconfiguredLegacyFailsClosed(t *testing.T) {
	registry := storage.NewRegistry(nil, nil, logger.NewWithWriter(discardWriter{}))
	router := NewRegistryWriteRouter(registry)

	got, err := router.ResolveTier1WriteStore()
	require.Error(t, err)
	assert.Nil(t, got)

	store, backendID, err := router.ResolveAttachmentWriteStore()
	require.Error(t, err)
	assert.Nil(t, store)
	assert.Empty(t, backendID)
}
