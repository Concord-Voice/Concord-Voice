//nolint:revive // var-naming false positive on "api" in v2.10.1 (relaxed in v2.12+)
package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPermissionCheckerAdapterSupportsTransactionBoundAuthorization is a
// regression test for #2907: message writes require the transaction-bound
// checker so a concurrent permission revoke cannot be bypassed by a cache.
func TestPermissionCheckerAdapterSupportsTransactionBoundAuthorization(t *testing.T) {
	_, ok := any(&permissionCheckerAdapter{}).(interface {
		HasChannelPermissionsUncachedTx(context.Context, *sql.Tx, string, string, string, ...int64) (bool, error)
	})
	require.True(t, ok, "production permission checker must support transaction-bound authorization")
}
