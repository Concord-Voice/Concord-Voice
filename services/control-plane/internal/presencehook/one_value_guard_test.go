package presencehook_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestNoProbeVerdictCrossesTheTransactionBoundary enforces #2854 stage C's C3:
// "the probe's result must never cross the transaction boundary."
//
// THIS IS THE HIGHEST-VALUE ARTIFACT IN STAGE C, and it is the only mechanism
// that can catch the defect it guards.
//
// The trap: an engineer reads the handler, sees the probe has already computed
// whether the target is a member (or whether an accepted edge exists), passes
// that boolean into the WithGatedTx closure, and deletes the in-transaction read
// as redundant. That converts ErrProbeStale from a detected 503 into a SILENT
// wrong write — a revocation applied over a live audience with no reconciliation.
//
// Why nothing else catches it:
//   - grep cannot: the identifier is arbitrary.
//   - review-by-diff cannot: it REMOVES a line rather than adding one.
//   - no behavioural test can: the HTTP response and the durable state come out
//     byte-identical. Only which locks were held differs, and only in a race.
//
// The GATED CAPTURE value (`gated`) legitimately crosses into the closure — it
// IS the capture, and one value must feed WithGatedTx, Capture, Abandon and
// Complete. The probe's BOOLEAN VERDICT must not.
func TestNoProbeVerdictCrossesTheTransactionBoundary(t *testing.T) {
	root := controlPlaneInternal(t)

	// The verdict identifiers stage C introduces. Keeping this list explicit
	// (rather than inferring "any bool in scope") is what makes the guard a
	// stable contract: a new probe site must add its name here deliberately.
	verdictNames := map[string]bool{
		"probedMember":  true,
		"probedEdge":    true,
		"probeIsMember": true,
		"alreadyMember": true, // AddMember's probe binding
	}

	for _, pkg := range []string{"friends", "members"} {
		dir := filepath.Join(root, pkg)
		for _, path := range nonTestGoFiles(t, dir) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}

			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WithGatedTx" {
					return true
				}
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.FuncLit)
					if !ok {
						continue
					}
					// Names DECLARED inside the closure are the closure's own
					// and are fine — only a capture from the enclosing scope
					// carries the probe's answer across the boundary.
					declared := map[string]bool{}
					ast.Inspect(lit, func(inner ast.Node) bool {
						switch d := inner.(type) {
						case *ast.AssignStmt:
							if d.Tok == token.DEFINE {
								for _, lhs := range d.Lhs {
									if id, ok := lhs.(*ast.Ident); ok {
										declared[id.Name] = true
									}
								}
							}
						case *ast.ValueSpec:
							for _, id := range d.Names {
								declared[id.Name] = true
							}
						}
						return true
					})

					ast.Inspect(lit, func(inner ast.Node) bool {
						id, ok := inner.(*ast.Ident)
						if !ok || !verdictNames[id.Name] || declared[id.Name] {
							return true
						}
						t.Errorf(
							"C3 VIOLATION at %s: the probe verdict %q is used inside a WithGatedTx "+
								"closure. The probe is NON-AUTHORITATIVE; only an in-transaction read "+
								"may answer that question. Substituting it turns probe_stale from a "+
								"detected 503 into a silent revocation over a live audience (#2854).",
							fset.Position(id.Pos()), id.Name)
						return true
					})
				}
				return true
			})
		}
	}
}
