package managedturn_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// modernc.org/sqlite is generated against one exact modernc.org/libc release.
// Go's minimum version selection lets `go get -u` or a dependency bump move
// libc past it, which builds and then misbehaves at run time, so the selected
// libc must equal the version the selected sqlite requires.
func TestSQLiteRunsOnTheLibcItRequires(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	selected := func(mod string) string {
		out, err := exec.Command(gobin, "list", "-m", "-f", "{{.Version}}", mod).Output() // #nosec G204 -- fixed arguments
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	sqlite, libc := selected("modernc.org/sqlite"), selected("modernc.org/libc")
	graph, err := exec.Command(gobin, "mod", "graph").Output() // #nosec G204 -- fixed arguments
	require.NoError(t, err)
	prefix := "modernc.org/sqlite@" + sqlite + " modernc.org/libc@"
	for _, line := range strings.Split(string(graph), "\n") {
		if required, ok := strings.CutPrefix(line, prefix); ok {
			require.Equal(t, required, libc, "modernc.org/libc must stay at the version modernc.org/sqlite %s requires", sqlite)
			return
		}
	}
	t.Fatalf("modernc.org/sqlite %s declares no modernc.org/libc requirement", sqlite)
}
