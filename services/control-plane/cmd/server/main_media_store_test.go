package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// TestMediaObjectStoreNeverProducesATypedNil is the regression for a defect that
// made two documented guards into dead code and put a nil dereference on the
// GDPR erasure path.
//
// initStorageClient returns a nil *storage.Client when object storage is
// unconfigured. NewRouter's parameter is the media.ObjectStore INTERFACE, so
// assigning the concrete pointer directly yields a NON-NIL interface holding a
// nil pointer. Every downstream `store == nil` check then silently evaluates
// false — including the durable Tier-1 erasure worker's nil-store branch. That
// would turn a retryable retained obligation into a typed-nil DeleteObject
// dereference after the account is already gone.
//
// `assert.Nil` is NOT sufficient here: testify's Nil uses reflection and reports
// a typed nil as nil, so it passes against the very bug this pins. The bare
// `got == nil` comparison is the assertion that discriminates.
func TestMediaObjectStoreNeverProducesATypedNil(t *testing.T) {
	t.Run("nil client yields a TRUE nil interface", func(t *testing.T) {
		got := mediaObjectStore(nil)

		//nolint:staticcheck // SA4023 is the point: a direct == nil on the interface
		// is exactly what a typed nil defeats, which is what this test exists to catch.
		assert.True(t, got == nil,
			"a typed nil here re-arms the dead-guard class: `store == nil` goes false "+
				"downstream and the durable GDPR erasure worker dereferences it")
	})

	t.Run("non-nil client is passed through", func(t *testing.T) {
		client := &storage.Client{}
		got := mediaObjectStore(client)

		require.NotNil(t, got)
		assert.Same(t, client, got, "the real client must reach the router unwrapped")
	})
}
