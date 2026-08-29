package media

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// Placement-predicate and delete-rail coverage (ADR-0038 / #2759 unit B2).
//
// WHY THIS FILE EXISTS. `isLegacyBackend` had no test of any kind, and the gap
// was DIRECTIONAL in the dangerous direction: mutating it to `return true` left
// `go test ./internal/media/... ./internal/purge/... ./internal/storage/...`
// green, while `return false` was caught. So the nil→true arm was pinned by
// incidental use and every false-returning input was unpinned — meaning nothing
// stopped an edit that sends a vendor-resident object to the legacy MinIO
// client.
//
// That matters most at the one call site with NO resolver behind it:
// `CleanupObject` (cleanup.go:74), reached from avatar/banner and server-icon
// replacement, where this predicate is the SOLE placement guard and the
// function's own contract says nothing retries behind it. An S3 DELETE of a key
// absent from the target bucket returns SUCCESS, so a collapsed predicate
// records an erasure that never happened and leaves the object alive at the
// vendor.
//
// The `"legacy"` literal case is not decoration either: dropping that clause
// (`return backend == nil`) also survived, which would let the media rails and
// storage.Registry disagree about a spelling registry_test.go pins as valid.

func TestIsLegacyBackend(t *testing.T) {
	legacy := string(storage.LegacyBackendID)
	for _, tc := range []struct {
		name    string
		backend *string
		want    bool
	}{
		{"NULL column is the legacy backend", nil, true},
		{"the literal legacy spelling", &legacy, true},
		{"a vendor backend is NOT legacy", strPtr("r2-useast"), false},
		{"empty string is a data defect, not NULL", strPtr(""), false},
		{"an unknown identifier is not legacy", strPtr("b2-eu"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isLegacyBackend(tc.backend))
		})
	}
}

func strPtr(s string) *string { return &s }

// --- legacyDeleterResolver: the no-registry arm ----------------------------

// TestLegacyDeleterResolver_ResolvesLegacy is the positive control. Without it
// the refusal test below would also pass if resolution were broken outright.
func TestLegacyDeleterResolver_ResolvesLegacy(t *testing.T) {
	legacy := &fakeObjectDeleter{}
	resolver := NewDeleterResolver(nil, legacy)

	got, err := resolver.ResolveDeleter(nil)
	require.NoError(t, err)
	assert.Same(t, legacy, got)

	got, err = resolver.ResolveDeleter(strPtr(string(storage.LegacyBackendID)))
	require.NoError(t, err)
	assert.Same(t, legacy, got)
}

// TestLegacyDeleterResolver_RefusesNonLegacy — with no registry wired, a row
// naming a vendor backend must NOT be deleted from the legacy store. This is
// the arm whose collapse is silent: the wrong-bucket DELETE would succeed.
func TestLegacyDeleterResolver_RefusesNonLegacy(t *testing.T) {
	legacy := &fakeObjectDeleter{}
	resolver := NewDeleterResolver(nil, legacy)

	got, err := resolver.ResolveDeleter(strPtr("r2-useast"))
	require.Error(t, err)
	assert.Nil(t, got, "a vendor-backed row must resolve to NO deleter without a registry")
	assert.False(t, legacy.called, "the legacy store must not be touched")
}

func TestLegacyDeleterResolver_UnconfiguredStoreErrors(t *testing.T) {
	resolver := NewDeleterResolver(nil, nil)

	got, err := resolver.ResolveDeleter(nil)
	require.Error(t, err)
	assert.Nil(t, got)
}

// --- registryDeleterResolver: the wired arm --------------------------------

// stubDeleteStoreResolver is a media.StoreResolver whose answer the test picks,
// so the delete rail's error propagation is reachable without a real registry.
type stubDeleteStoreResolver struct {
	store ObjectStore
	err   error
}

func (s stubDeleteStoreResolver) ResolveStore(_ *string) (ObjectStore, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.store, nil
}

// TestRegistryDeleterResolver_PropagatesResolutionFailure — when a registry IS
// wired, an unresolvable backend must surface the error rather than degrade to
// the legacy store. reapBlob depends on this to leave the row unmarked.
func TestRegistryDeleterResolver_PropagatesResolutionFailure(t *testing.T) {
	resolver := NewDeleterResolver(stubDeleteStoreResolver{err: errors.New("unknown backend")}, &fakeObjectDeleter{})

	got, err := resolver.ResolveDeleter(strPtr("r2-useast"))
	require.Error(t, err)
	assert.Nil(t, got)
}

// TestRegistryDeleterResolver_PrefersTheRegistryOverTheLegacyFallback locks the
// precedence in NewDeleterResolver: a non-nil StoreResolver wins, so wiring a
// registry cannot be silently bypassed by also passing a legacy deleter.
func TestRegistryDeleterResolver_PrefersTheRegistryOverTheLegacyFallback(t *testing.T) {
	registryStore, legacy := newMockStore(), &fakeObjectDeleter{}
	resolver := NewDeleterResolver(stubDeleteStoreResolver{store: registryStore}, legacy)

	got, err := resolver.ResolveDeleter(strPtr("r2-useast"))
	require.NoError(t, err)
	assert.Same(t, registryStore, got)
	assert.NotSame(t, ObjectDeleter(legacy), got)
}

// fakeObjectDeleter records whether it was asked to delete anything.
type fakeObjectDeleter struct{ called bool }

func (f *fakeObjectDeleter) DeleteObject(_ context.Context, _ string) error {
	f.called = true
	return nil
}
