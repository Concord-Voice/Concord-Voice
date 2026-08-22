package presencehook_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// controlPlaneInternal walks up from this test's package directory to
// services/control-plane/internal.
//
// It is a walk from a known anchor rather than a hard-coded path to another
// package's FILE: a rename or split of any single file leaves it working. A
// guard that names another package's file dictates that package's source layout
// and recreates the coupling it was written to sever (#2854 stage C spec 3.2).
func controlPlaneInternal(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // Go sets this to the package directory.
	require.NoError(t, err)
	root := filepath.Dir(wd) // .../internal/presencehook -> .../internal
	require.Equal(t, "internal", filepath.Base(root),
		"guard anchor moved; update controlPlaneInternal")
	return root
}

func nonTestGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	}))
	return out
}

// Guard 3 (C1) — SCOPED, and the scope is the finding, not a weakening.
//
// The spec asked for "exactly one non-test definition of the accepted-edge
// predicate" across internal/. That is NOT implementable and a guard asserting
// it is red on arrival: "are these two users friends?" is a legitimate question
// in internal/dm (DM permission), internal/mfa, internal/presence (audience) and
// internal/websocket, and internal/friends asks it again in RemoveFriend's own
// DELETE. Nine files match the loose form and four match the symmetric-pair
// form. Measured, not assumed.
//
// The invariant that IS true and IS worth enforcing is narrower: the #2446
// capture/probe family shares ONE definition. Concretely, internal/graphpresence
// must carry no friendships SQL of its own — it delegates to
// presencehook.AcceptedEdgeExists. That is what stage C established and what a
// future "inline it back for one less hop" change would undo.
func TestGraphPresenceCarriesNoAcceptedEdgeSQLOfItsOwn(t *testing.T) {
	root := controlPlaneInternal(t)
	dir := filepath.Join(root, "graphpresence")

	for _, path := range nonTestGoFiles(t, dir) {
		// The path comes from nonTestGoFiles' own WalkDir over a directory this
		// test computed from its own package location — never from input.
		src, err := os.ReadFile(path) //nolint:gosec // guard walks a self-derived source tree
		require.NoError(t, err)
		// "FROM friendships" rather than bare "friendships": the bare form trips
		// on an innocent doc comment naming the table, and a guard that fails on
		// prose gets deleted rather than obeyed.
		require.NotContains(t, string(src), "FROM friendships",
			"C1: %s carries its own friendships SQL. The accepted-edge predicate belongs to "+
				"presencehook.AcceptedEdgeExists so the capture gate, RemoveFriend's probe, BlockUser's "+
				"probe and BlockUser's in-transaction read cannot drift apart (#2854 stage C).", path)
	}
}

// Guard 2 (C2) — no POOLED probe anywhere in internal/ takes a row lock.
//
// Scoped to a pooled receiver (`something.db.QueryRowContext`) because a *sql.Tx
// read may legitimately lock. On the pool no SET LOCAL lock_timeout applies, so
// a locking clause there can block indefinitely — the unbounded wait the probe
// contract exists to keep out of the gated path.
func TestNoPooledProbeTakesARowLock(t *testing.T) {
	root := controlPlaneInternal(t)
	lockClauses := []string{"FOR UPDATE", "FOR NO KEY UPDATE", "FOR SHARE", "FOR KEY SHARE"}

	for _, path := range nonTestGoFiles(t, root) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "QueryRowContext" {
				return true
			}
			recv, ok := sel.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "db" { // pooled handle only
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				upper := strings.ToUpper(lit.Value)
				for _, clause := range lockClauses {
					require.NotContains(t, upper, clause,
						"C2: pooled read at %s takes %s — no lock_timeout applies on the pool, "+
							"so it can block indefinitely", fset.Position(lit.Pos()), clause)
				}
			}
			return true
		})
	}
}
