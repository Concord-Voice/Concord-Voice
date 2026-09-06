package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestMainWiresOneReadinessFlagToBothEnds guards the PRODUCER end of the
// two-instance drain hazard.
//
// The AST test in internal/api guards the consumer end -- NewRouter must pass
// `dependencies.Readiness` to health.NewProber. Nothing guarded this end, and a
// second `health.NewReadiness()` in the RouterDependencies literal divorces the
// two exactly as thoroughly: SIGTERM latches a flag nothing reads, /readyz
// answers 200 through the entire drain, and the deploy stays green.
//
// main.go, router.go and the consumer test all carry comments warning about
// this hazard. Until now, four warnings guarded one of the two ends.
//
// This is a source-level assertion because the wiring is unreachable at
// runtime: NewRouter needs a live database, and both drain tests construct
// their own *health.Readiness and wire it by hand, so neither observes what
// main.go actually writes.
func TestMainWiresOneReadinessFlagToBothEnds(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var constructed int
	var depsField ast.Expr
	var drainArg ast.Expr

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "health" && sel.Sel.Name == "NewReadiness" {
					constructed++
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "runControlPlaneServer" {
					_ = id
				}
			}
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "runControlPlaneServer" && len(node.Args) == 4 {
				drainArg = node.Args[3]
			}
		case *ast.KeyValueExpr:
			if k, ok := node.Key.(*ast.Ident); ok && k.Name == "Readiness" {
				depsField = node.Value
			}
		}
		return true
	})

	if constructed != 1 {
		t.Fatalf("main.go calls health.NewReadiness() %d times, want exactly 1. "+
			"A second flag divorces the drain from the probe: SIGTERM latches "+
			"one and /readyz reads the other, so it reports ready for the whole "+
			"shutdown.", constructed)
	}
	id, ok := depsField.(*ast.Ident)
	if !ok || id.Name != "readiness" {
		t.Fatalf("RouterDependencies.Readiness is %T, want the `readiness` "+
			"identifier constructed at the top of runControlPlane. A fresh "+
			"health.NewReadiness() here is the two-instance hazard.", depsField)
	}
	sel, ok := drainArg.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "MarkDraining" {
		t.Fatalf("runControlPlaneServer's beginDrain argument is %T, want "+
			"readiness.MarkDraining", drainArg)
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != id.Name {
		t.Fatalf("the drain latch marks %q but the router reads %q -- the two "+
			"ends must be the SAME flag", x, id.Name)
	}
}
