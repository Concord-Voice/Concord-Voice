package storage

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Registry coverage (ADR-0038 / #2759 unit B1).
//
// WHY THIS FILE EXISTS. The registry shipped with no test file at all, and the
// gap was invisible to the suite: mutating ResolveRow's whole body to
// `return r.Resolve(LegacyBackendID)` — which sends EVERY unrecognized
// storage_backend value to the legacy store, the exact wrong-bucket read the
// doc comment says must never happen — left `go test ./internal/storage/...`
// green. A guard nothing can falsify is not a guard.
//
// Each fail-closed assertion below is paired with a positive control that
// resolves successfully through the same call. Without the pair, a mutation
// that broke resolution outright would satisfy every "must error" assertion
// and read as a pass.

func testRegistryLogger() *logger.Logger { return logger.NewWithWriter(io.Discard) }

// legacyOnlyRegistry builds a registry holding just a sentinel legacy client.
// Client is constructed directly rather than through New: New dials, and
// nothing here needs a reachable store to answer a resolution question.
func legacyOnlyRegistry() (*Registry, *Client) {
	legacy := &Client{bucket: "legacy-bucket"}
	return NewRegistry(nil, legacy, testRegistryLogger()), legacy
}

func strptr(s string) *string { return &s }

// TestRegistry_NULLResolvesToLegacy is the positive control for every
// fail-closed assertion below: it proves ResolveRow can succeed at all.
func TestRegistry_NULLResolvesToLegacy(t *testing.T) {
	registry, legacy := legacyOnlyRegistry()

	got, err := registry.ResolveRow(nil)
	require.NoError(t, err)
	assert.Same(t, legacy, got, "a NULL storage_backend must resolve to the legacy client")

	// The literal spelling resolves identically — both are "legacy".
	got, err = registry.ResolveRow(strptr(string(LegacyBackendID)))
	require.NoError(t, err)
	assert.Same(t, legacy, got)
}

// TestRegistry_UnknownBackendFailsClosed is the mutation this file was written
// for. An unrecognized identifier must NOT fall back to legacy: the row points
// at a store this build cannot reach, so serving it from a different bucket
// either 404s an object that exists or returns another backend's bytes.
func TestRegistry_UnknownBackendFailsClosed(t *testing.T) {
	registry, legacy := legacyOnlyRegistry()

	got, err := registry.ResolveRow(strptr("r2-useast"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendUnknown)
	assert.Nil(t, got, "an unknown backend must yield NO client")
	assert.NotSame(t, legacy, got, "an unknown backend must never fall back to the legacy store")
}

// TestRegistry_EmptyStringIsNotNULL locks the documented distinction: a
// non-NULL column naming no backend is a data defect, not a NULL.
func TestRegistry_EmptyStringIsNotNULL(t *testing.T) {
	registry, _ := legacyOnlyRegistry()

	got, err := registry.ResolveRow(strptr(""))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendUnknown)
	assert.Nil(t, got)
}

// TestRegistry_UnconfiguredLegacyIsUnavailableNotUnknown keeps the two failure
// states distinct. "legacy is not a backend" and "the legacy backend is not
// configured" are materially different things to read in an incident log.
func TestRegistry_UnconfiguredLegacyIsUnavailableNotUnknown(t *testing.T) {
	registry := NewRegistry(nil, nil, testRegistryLogger())

	got, err := registry.ResolveRow(nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendUnavailable)
	assert.NotErrorIs(t, err, ErrBackendUnknown)
	assert.Nil(t, got)
}

// TestRegistry_NilRegistryFailsClosed — an embedder that never built one must
// get an error, not a panic on the download path.
func TestRegistry_NilRegistryFailsClosed(t *testing.T) {
	var registry *Registry

	got, err := registry.ResolveRow(nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendUnavailable)
	assert.Nil(t, got)
	assert.Nil(t, registry.BackendIDs())
}

// TestRegistry_UnconstructableBackendDoesNotAbortBoot is the fault-isolation
// claim: an R2 destination problem must not stop the control plane from
// starting, because that would take down auth, messaging, voice and the whole
// MinIO-resident corpus over a store holding no objects at all.
//
// The bad backend must still be REGISTERED (so it reports unavailable rather
// than unknown) and must still fail closed.
func TestRegistry_UnconstructableBackendDoesNotAbortBoot(t *testing.T) {
	legacy := &Client{bucket: "legacy-bucket"}
	cfg := &config.Config{CloudflareR2: config.CloudflareR2Config{
		Endpoint:        "https://accountid.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "", // no bucket -> newVendorClient refuses, fast and offline
		AccessKeyID:     "test-access-key-id",
		SecretAccessKey: "test-secret-access-key",
	}}

	registry := NewRegistry(cfg, legacy, testRegistryLogger())
	require.NotNil(t, registry, "a bad backend must not abort registry construction")

	// Legacy is untouched by its neighbour's failure.
	got, err := registry.ResolveRow(nil)
	require.NoError(t, err)
	assert.Same(t, legacy, got)

	// The bad one is registered-but-unavailable, NOT unknown, and yields no client.
	got, err = registry.ResolveRow(strptr(config.AttachmentBackendR2USEast))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendUnavailable)
	assert.NotErrorIs(t, err, ErrBackendUnknown)
	assert.Nil(t, got)

	// Registration order is stable and legacy-first: the session sweeper walks
	// this list, and Go's randomized map order would reshuffle its per-backend
	// log lines every run.
	assert.Equal(t,
		[]BackendID{LegacyBackendID, BackendID(config.AttachmentBackendR2USEast)},
		registry.BackendIDs())
}

// TestRegistry_CredentiallessBackendIsNotRegistered — AttachmentBackends drops
// a backend with no credentials, so it is UNKNOWN rather than unavailable.
// This is the state of the dormant EU/Asia buckets.
func TestRegistry_CredentiallessBackendIsNotRegistered(t *testing.T) {
	cfg := &config.Config{CloudflareR2: config.CloudflareR2Config{
		Endpoint: "https://accountid.r2.cloudflarestorage.com",
		Bucket:   "concord-voice-r2-us-east",
	}}

	registry := NewRegistry(cfg, &Client{}, testRegistryLogger())

	_, err := registry.ResolveRow(strptr(config.AttachmentBackendR2USEast))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBackendUnknown)
	assert.Equal(t, []BackendID{LegacyBackendID}, registry.BackendIDs())
}

// TestRegistry_ConfigCannotClaimTheLegacyIdentifier — a config entry spelling
// itself "legacy" would re-point every NULL row (the entire pre-cutover corpus
// plus all profile media) at a vendor bucket. The existing entry must win.
func TestRegistry_ConfigCannotClaimTheLegacyIdentifier(t *testing.T) {
	registry, legacy := legacyOnlyRegistry()

	registry.register(config.ObjectBackend{
		ID:              string(LegacyBackendID),
		Endpoint:        "https://accountid.r2.cloudflarestorage.com",
		Bucket:          "attacker-bucket",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
	}, testRegistryLogger())

	got, err := registry.ResolveRow(nil)
	require.NoError(t, err)
	assert.Same(t, legacy, got, "the legacy identifier must not be reassignable from config")
	assert.Equal(t, []BackendID{LegacyBackendID}, registry.BackendIDs())
}

func TestRegistry_AttachmentWriteBackendID_ConfiguredSelectorIsVerbatim(t *testing.T) {
	legacy := &Client{bucket: "legacy-bucket"}
	cfg := &config.Config{AttachmentWriteBackend: "r2-useast"}
	registry := NewRegistry(cfg, legacy, testRegistryLogger())

	assert.Equal(t, BackendID("r2-useast"), registry.AttachmentWriteBackendID(),
		"the boot snapshot must preserve the configured selector even when its client is unavailable")
}

func TestRegistry_AttachmentWriteBackendID_NilConfigUsesLegacy(t *testing.T) {
	registry, _ := legacyOnlyRegistry()
	assert.Equal(t, LegacyBackendID, registry.AttachmentWriteBackendID())
}

func TestRegistry_AttachmentWriteBackendID_ZeroValueConfigUsesLegacy(t *testing.T) {
	legacy := &Client{bucket: "legacy-bucket"}
	registry := NewRegistry(&config.Config{}, legacy, testRegistryLogger())
	assert.Equal(t, LegacyBackendID, registry.AttachmentWriteBackendID())
}

func TestRegistry_LegacyAttachmentBackendConstantsMatch(t *testing.T) {
	assert.Equal(t, "legacy", string(LegacyBackendID))
	assert.Equal(t, "legacy", config.LegacyAttachmentBackendID)
}

// --- vendorEndpointHost ----------------------------------------------------
//
// 0.0% covered before this. It is the only validation between a configured
// endpoint string and a live S3 client, and every rejection it performs is a
// fail-closed one — an http:// endpoint reaching minio.NewCore with Secure:true
// would silently produce a client pointed at the wrong scheme's port, and a
// path-carrying endpoint would prefix every object key.

func TestVendorEndpointHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"plain host", "https://acct.r2.cloudflarestorage.com", "acct.r2.cloudflarestorage.com"},
		{"explicit port is preserved", "https://minio.internal:9000", "minio.internal:9000"},
		{"a bare trailing slash is not a path", "https://acct.r2.cloudflarestorage.com/", "acct.r2.cloudflarestorage.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vendorEndpointHost(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVendorEndpointHost_Rejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"http is refused — TLS is mandatory, there is no *_USE_SSL escape hatch", "http://acct.r2.cloudflarestorage.com"},
		{"scheme-less host does not parse as https", "acct.r2.cloudflarestorage.com"},
		{"no host", "https://"},
		{"a path would prefix every object key", "https://acct.r2.cloudflarestorage.com/bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vendorEndpointHost(tc.raw)
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}
