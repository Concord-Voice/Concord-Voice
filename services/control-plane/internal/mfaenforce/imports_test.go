package mfaenforce_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const moduleInternal = "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/"

// TestLeafImports pins this package's place in the import graph.
//
// The packages that consume the gate must never become its dependencies,
// directly or transitively. The day one of them imports this package that is
// an import cycle; before then it is a gate that can reach the permission
// state it gates. `go list` reports the whole non-test closure in Deps, so a
// new import of an innocent-looking package that itself pulls in rbac fails
// here too.
//
// Direct imports are held to the standard library plus the three the package
// comment names.
func TestLeafImports(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-json", ".").Output()
	require.NoError(t, err, "go list must run for the import guard to mean anything")
	var pkg struct {
		Imports []string
		Deps    []string
	}
	require.NoError(t, json.Unmarshal(out, &pkg))
	require.NotEmpty(t, pkg.Deps, "an empty closure would pass vacuously")

	forbidden := []string{"rbac", "servers", "members", "channels", "api"}
	for _, dep := range pkg.Deps {
		for _, name := range forbidden {
			require.NotEqual(t, moduleInternal+name, dep, "mfaenforce must not depend on internal/%s", name)
		}
	}

	allowed := map[string]bool{
		"github.com/gin-gonic/gin": true,
		"github.com/lib/pq":        true,
		moduleInternal + "stepup":  true,
	}
	for _, imp := range pkg.Imports {
		// A standard-library path has no dot in its first element.
		if !strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
			continue
		}
		require.True(t, allowed[imp], "unexpected direct import %s", imp)
	}
}
