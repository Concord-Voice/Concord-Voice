package authoritycontract

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const moduleInternal = "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/"

// TestImportBoundary pins DoR §3.1's import boundary. authorityrelay imports
// this package (§2.15 P1), so database/sql must never enter its closure, and
// internal/age pulls gin, go-redis, lib/pq and nats. go list reports the whole
// non-test closure in Deps, so an innocent-looking new import that pulls either
// one fails here too.
func TestImportBoundary(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-json", ".").Output()
	require.NoError(t, err, "go list must run for the import guard to mean anything")
	var pkg struct{ Imports, Deps []string }
	require.NoError(t, json.Unmarshal(out, &pkg))
	require.NotEmpty(t, pkg.Deps, "an empty closure would pass vacuously")

	for _, imp := range pkg.Imports {
		// A standard-library path has no dot in its first element.
		if !strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
			continue
		}
		require.Equal(t, "github.com/google/uuid", imp, "unexpected direct import %s", imp)
	}
	for _, dep := range pkg.Deps {
		require.NotEqual(t, "database/sql", dep, "authoritycontract must not depend on database/sql")
		require.False(t, strings.HasPrefix(dep, moduleInternal), "authoritycontract must not depend on %s", dep)
	}
}
