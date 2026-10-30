package presencehook_test

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// Guard 2 (C2) forbids locking pooled probes, including SKIP LOCKED reads.
// The one reviewed owner mutation is recognized by its complete function AST,
// not its filename, package, statement prefix or presence of SKIP LOCKED.
// Its row-contention avoidance does not prevent DDL, pool or transport waits;
// the independently checked ten-second context bounds those via cancellation.
func TestNoPooledProbeTakesARowLock(t *testing.T) {
	root := controlPlaneInternal(t)
	recognized, callers, boundedRounds, timeoutConstants := 0, 0, 0, 0
	for _, path := range nonTestGoFiles(t, root) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		violations, accepted := pooledLockViolations(t, fset, file)
		recognized += accepted
		require.Empty(t, violations, "C2: locking pooled probe(s) in %s: %v", path, violations)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "claimAuthRevocation" {
					return true
				}
				callers++
				require.Equal(t, "drainDueAuthRevocations", fn.Name.Name)
				require.Equal(t, "r.claimAuthRevocation(ctx, candidate.OperationID)", astText(t, call))
				return true
			})
			if file.Name.Name == "dmblock" && fn.Name.Name == "reconcileSecurityRound" {
				boundedRounds++
				body := astText(t, fn)
				for _, fragment := range []string{
					"roundCtx, cancel := context.WithTimeout(ctx, securityEjectionDrainTimeout)",
					"defer cancel()", "r.drainDueAuthRevocations", "drain(roundCtx, limit)",
				} {
					require.Contains(t, body, fragment, "reviewed claim must retain bounded caller context")
				}
			}
		}
		if file.Name.Name == "dmblock" {
			ast.Inspect(file, func(n ast.Node) bool {
				spec, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range spec.Names {
					if name.Name == "securityEjectionDrainTimeout" {
						timeoutConstants++
						require.Len(t, spec.Values, len(spec.Names))
						require.Equal(t, "10 * time.Second", astText(t, spec.Values[i]))
					}
				}
				return true
			})
		}
	}
	require.Equal(t, 1, boundedRounds, "bounded round must remain present")
	require.Equal(t, 1, timeoutConstants, "actual ten-second timeout must remain present")
	require.Equal(t, 1, recognized, "exact reviewed atomic claim must remain present")
	require.Equal(t, 1, callers, "claim must retain its sole bounded production caller")
}

func astText(t *testing.T, n ast.Node) string {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, format.Node(&out, token.NewFileSet(), n))
	return strings.Join(strings.Fields(out.String()), " ")
}

func pooledLockViolations(t *testing.T, fset *token.FileSet, file *ast.File) ([]string, int) {
	t.Helper()
	expected, err := parser.ParseFile(token.NewFileSet(), "reviewed.go", "package dmblock\n"+reviewedAtomicClaim, 0)
	require.NoError(t, err)
	var violations []string
	accepted := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		exactClaim := ok && file.Name.Name == "dmblock" && astText(t, fn) == astText(t, expected.Decls[0])
		if exactClaim {
			accepted++
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "QueryRowContext" {
				return true
			}
			recv, ok := sel.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "db" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				query, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				upper := strings.ToUpper(strings.Join(strings.Fields(query), " "))
				for _, clause := range []string{"FOR UPDATE", "FOR NO KEY UPDATE", "FOR SHARE", "FOR KEY SHARE"} {
					if strings.Contains(upper, clause) && !exactClaim {
						violations = append(violations, fmt.Sprintf("%s takes %s", fset.Position(lit.Pos()), clause))
					}
				}
			}
			return true
		})
	}
	return violations, accepted
}

func TestPooledLockGuardRecognizesOnlyReviewedAtomicClaim(t *testing.T) {
	for _, tc := range []struct {
		name, source         string
		accepted, violations int
	}{
		{"reviewed claim", reviewedAtomicClaim, 1, 0},
		{"claim without skip locked", strings.Replace(reviewedAtomicClaim, " SKIP LOCKED", "", 1), 0, 1},
		{"claim without due predicate", strings.Replace(reviewedAtomicClaim, "reconcile_after<=clock_timestamp()", "TRUE", 1), 0, 1},
		{"claim without sealed predicate", strings.Replace(reviewedAtomicClaim, "sealed_at IS NOT NULL", "TRUE", 1), 0, 1},
		{"claim without incomplete predicate", strings.Replace(reviewedAtomicClaim, "completed_at IS NULL", "TRUE", 1), 0, 1},
		{"claim without exact owner", strings.Replace(reviewedAtomicClaim, "operation_id=$1", "TRUE", 1), 0, 1},
		{"claim updates different row", strings.Replace(reviewedAtomicClaim, "o.operation_id=due.operation_id", "TRUE", 1), 0, 1},
		{"claim renamed receiver", strings.ReplaceAll(reviewedAtomicClaim, "r.", "other."), 0, 1},
		{"ordinary pooled share probe", "func probe() { p.db.QueryRowContext(ctx, \"SELECT 1 FOR SHARE\") }", 0, 1},
		{"ordinary pooled key share probe", "func probe() { p.db.QueryRowContext(ctx, \"SELECT 1 FOR KEY SHARE\") }", 0, 1},
		{"ordinary pooled no key update probe", "func probe() { p.db.QueryRowContext(ctx, \"SELECT 1 FOR NO KEY UPDATE\") }", 0, 1},
		{"claim with extra locking read", strings.Replace(reviewedAtomicClaim, "var o AuthRevocation", "r.db.QueryRowContext(ctx, \"SELECT 1 FOR SHARE\")\nvar o AuthRevocation", 1), 0, 2},
		{"ordinary pooled skip locked probe", "func (r *Reconciler) probe(ctx context.Context) { r.db.QueryRowContext(ctx, \"SELECT EXISTS(SELECT 1 FROM login_family_revocations FOR UPDATE SKIP LOCKED)\") }", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", "package dmblock\n"+tc.source, 0)
			require.NoError(t, err)
			violations, accepted := pooledLockViolations(t, fset, file)
			require.Equal(t, tc.accepted, accepted)
			require.Len(t, violations, tc.violations)
		})
	}
}

// Pinned to the task6a reviewed owner claim. Any body/receiver/query change
// requires renewed review; no other query in this function is silently exempt.
const reviewedAtomicClaim = `func (r *Reconciler) claimAuthRevocation(ctx context.Context, operationID uuid.UUID) (AuthRevocation, error) {
	var o AuthRevocation
	err := r.db.QueryRowContext(ctx, ` + "`" + `WITH due AS (
 SELECT operation_id FROM login_family_revocations
 WHERE operation_id=$1 AND completed_at IS NULL AND sealed_at IS NOT NULL
 AND reconcile_after<=clock_timestamp() FOR UPDATE SKIP LOCKED
 ) UPDATE login_family_revocations o SET attempts=LEAST(o.attempts,2147483646)+1,
 reconcile_after=clock_timestamp()+INTERVAL '10 seconds',updated_at=clock_timestamp()
 FROM due WHERE o.operation_id=due.operation_id
 RETURNING o.operation_id,o.user_id,o.scope,o.reason,o.attempts,o.created_at` + "`" + `, operationID).Scan(
		&o.OperationID, &o.UserID, &o.Scope, &o.Reason, &o.Attempts, &o.CreatedAt)
	if err != nil {
		return AuthRevocation{}, fmt.Errorf("claim auth revocation: %w", err)
	}
	return o, nil
}`
