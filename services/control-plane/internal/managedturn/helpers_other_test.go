//go:build !linux

package managedturn_test

import "testing"

// dropPrivileges needs setresuid, which only Linux provides; as root elsewhere
// the read-only fixture cannot be enforced, so the test is skipped.
func dropPrivileges(t testing.TB) (restore func()) {
	t.Helper()
	t.Skip("dropping root privileges for the read-only fixture needs Linux setresuid")
	return func() {}
}
