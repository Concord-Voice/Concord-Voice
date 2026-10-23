//go:build linux

package managedturn_test

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// dropPrivileges switches the effective IDs of every thread to nobody, keeping
// root as the saved ID so the returned restore can switch back. It skips the
// test when the drop is refused.
func dropPrivileges(t testing.TB) (restore func()) {
	t.Helper()
	if err := syscall.Setresgid(-1, nobodyID, 0); err != nil {
		t.Skipf("cannot drop group privileges to enforce a read-only database: %v", err)
	}
	if err := syscall.Setresuid(-1, nobodyID, 0); err != nil {
		_ = syscall.Setresgid(-1, 0, -1) // best effort before skipping; the skip is the report
		t.Skipf("cannot drop user privileges to enforce a read-only database: %v", err)
	}
	return func() {
		require.NoError(t, syscall.Setresuid(-1, 0, -1))
		require.NoError(t, syscall.Setresgid(-1, 0, -1))
	}
}
