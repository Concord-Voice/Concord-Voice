package media

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Read-rail adapter coverage (ADR-0038 / #2759 unit B1).
//
// write_routing_test.go proves the WRITE rail's production adapter; this is its
// counterpart for the READ rail. `NewRegistryStoreResolver` is what main.go
// actually wires, and it was reached by no test at all — the handler-level
// tests all inject a stub, so the adapter that converts a *storage.Registry
// into a StoreResolver was exercised only in production.

func TestNewRegistryStoreResolver_ResolvesNULLToLegacy(t *testing.T) {
	legacy := &storage.Client{}
	registry := storage.NewRegistry(nil, legacy, logger.NewWithWriter(discardWriter{}))

	got, err := NewRegistryStoreResolver(registry).ResolveStore(nil)

	require.NoError(t, err)
	assert.Same(t, legacy, got, "a NULL column must resolve to the legacy client")
}

// TestNewRegistryStoreResolver_UnknownBackendFailsClosed — the adapter must
// surface the registry's refusal rather than degrade to the legacy store. A
// wrong-bucket read either 404s an object that exists or serves another
// backend's bytes.
func TestNewRegistryStoreResolver_UnknownBackendFailsClosed(t *testing.T) {
	registry := storage.NewRegistry(nil, &storage.Client{}, logger.NewWithWriter(discardWriter{}))

	got, err := NewRegistryStoreResolver(registry).ResolveStore(strPtr("r2-useast"))

	require.Error(t, err)
	assert.Nil(t, got, "an unknown backend must yield NO store, not the legacy one")
}

// TestNewRegistryStoreResolver_UnavailableBackendFailsClosed covers an
// unconfigured legacy backend: the registry registers it UNAVAILABLE and the
// adapter must return an error and no store.
//
// **It does NOT cover the nil-boxing arm, and the distinction is worth
// recording.** The adapter carries an `if client == nil` guard against boxing a
// nil *storage.Client into the ObjectStore interface — a non-nil interface
// holding nil, which defeats every downstream nil check. Gutting that guard
// leaves this test green, because it is unreachable by construction:
// `ResolveRow` returns an error whenever it has no client, so the error branch
// above it always fires first. The guard is defence-in-depth against a future
// `ResolveRow` that returns `(nil, nil)`, and no test can reach it until one
// does. Do not delete it on the strength of a coverage report.
func TestNewRegistryStoreResolver_UnavailableBackendFailsClosed(t *testing.T) {
	registry := storage.NewRegistry(nil, nil, logger.NewWithWriter(discardWriter{}))

	got, err := NewRegistryStoreResolver(registry).ResolveStore(nil)

	require.Error(t, err)
	assert.Nil(t, got, "an unavailable backend must yield no store")
}

// --- setters ---------------------------------------------------------------

func TestSetStoreResolverAndWriteRouter(t *testing.T) {
	h := &Handler{}
	assert.Nil(t, h.backends)
	assert.Nil(t, h.writeRouter)

	resolver := stubDeleteStoreResolver{store: newMockStore()}
	h.SetStoreResolver(resolver)
	assert.Equal(t, resolver, h.backends)

	router := stubWriteRouter{tier1: newMockStore()}
	h.SetWriteRouter(router)
	assert.Equal(t, router, h.writeRouter)
}

// --- BlobRef ---------------------------------------------------------------

// TestNewBlobRef_IndependentPerCall exercises the batch shape: two refs built
// from one reused scan variable, which is then overwritten as the next
// `rows.Next()` iteration would.
//
// **Read what this can and cannot prove.** `NewBlobRef` takes `sql.NullString`
// BY VALUE, so the parameter is already a copy and `&backend.String` would
// point into that copy, not into the caller's variable. Replacing the explicit
// `value := backend.String` with `&backend.String` is therefore an EQUIVALENT
// mutation — verified by running it, and the suite correctly stays green. No
// test can separate the two forms while the signature is by-value.
//
// So this is a tripwire for a SIGNATURE change, not a lock on the aliasing bug
// the constructor's doc comment describes: change the parameter to
// `*sql.NullString` (or build BlobRef literals inline at a scan site) and the
// hazard becomes real and this test becomes able to fail. Keeping it is cheap
// and it documents the batch invariant callers depend on; do not read its
// passing as evidence that aliasing is guarded today — pass-by-value is what
// guards it.
func TestNewBlobRef_IndependentPerCall(t *testing.T) {
	var scanned sql.NullString

	scanned = sql.NullString{String: "r2-useast", Valid: true}
	first := NewBlobRef("attachments/a", scanned)

	scanned = sql.NullString{String: "b2-eu", Valid: true}
	second := NewBlobRef("attachments/b", scanned)

	// Overwrite the shared variable the way the next loop iteration would, and
	// assert it actually moved — otherwise the line is inert decoration, which
	// is precisely what `ineffassign` (and the equivalent-mutation run noted
	// above) reported the first time this was written.
	scanned = sql.NullString{String: "clobbered", Valid: true}
	require.Equal(t, "clobbered", scanned.String, "the scan variable has moved on")

	require.NotNil(t, first.Backend)
	require.NotNil(t, second.Backend)
	assert.Equal(t, "r2-useast", *first.Backend, "each ref keeps the value it was built from")
	assert.Equal(t, "b2-eu", *second.Backend)
	assert.NotSame(t, first.Backend, second.Backend, "each ref must own its own string")
}

func TestNewBlobRef_InvalidIsANULLColumn(t *testing.T) {
	ref := NewBlobRef("attachments/a", sql.NullString{Valid: false})

	assert.Equal(t, "attachments/a", ref.Key)
	assert.Nil(t, ref.Backend, "an invalid NullString is a NULL column, which is the legacy backend")
}

// TestBlobRef_BackendLabel — NULL must stay distinguishable in a log from any
// string a row could legitimately carry, since the two mean different things
// when an operator is deciding whether an erasure actually happened.
func TestBlobRef_BackendLabel(t *testing.T) {
	assert.NotEqual(t, "r2-useast", BlobRef{Key: "k"}.BackendLabel())
	assert.Equal(t, "r2-useast", BlobRef{Key: "k", Backend: strPtr("r2-useast")}.BackendLabel())

	nullLabel := BlobRef{Key: "k"}.BackendLabel()
	assert.NotEmpty(t, nullLabel, "a NULL backend must render as something readable, not an empty string")
}
