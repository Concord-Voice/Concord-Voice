package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunExpirationPreflight_SignalCancelsChildAndReturnsCancellation(t *testing.T) {
	quit := make(chan os.Signal, 1)
	started := make(chan struct{})
	quit <- syscall.SIGTERM
	result := make(chan error, 1)
	go func() {
		result <- runExpirationPreflight(context.Background(), quit, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("preflight callback was not started")
	}
	select {
	case err := <-result:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("signal-canceled preflight did not return")
	}
}

func TestRunExpirationPreflight_ReturnsRunErrorWithoutSignal(t *testing.T) {
	want := errors.New("preflight failed")
	quit := make(chan os.Signal, 1)
	assert.ErrorIs(t, runExpirationPreflight(context.Background(), quit, func(context.Context) error { return want }), want)
}

func TestMainExpirationPreflightErrorIsCheckedBeforeListenAndServe(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	require.NoError(t, err)
	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if candidate, ok := n.(*ast.FuncDecl); ok && candidate.Name.Name == "runControlPlane" {
			fn = candidate
			return false
		}
		return true
	})
	require.NotNil(t, fn)
	preflight, listen, cleanup := -1, -1, -1
	checked := false
	for i, stmt := range fn.Body.List {
		if deferStmt, ok := stmt.(*ast.DeferStmt); ok {
			if closure, ok := deferStmt.Call.Fun.(*ast.FuncLit); ok && closureCalls(closure, "cleanupRuntime") {
				cleanup = i
			}
		}
		if ifStmt, ok := stmt.(*ast.IfStmt); ok {
			if errorName, found := assignedCallResult(ifStmt.Init, "runExpirationPreflight"); found {
				preflight = i
				checked = conditionChecksError(ifStmt.Cond, errorName) && blockReturns(ifStmt.Body)
			}
		}
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callName(call) {
			case "runExpirationPreflight":
				preflight = i
			case "ListenAndServe":
				listen = i
			}
			return true
		})
	}
	assert.GreaterOrEqual(t, preflight, 0, "runControlPlane must run expiration preflight")
	assert.True(t, checked, "preflight error must be checked before serving")
	assert.GreaterOrEqual(t, cleanup, 0, "cleanup runtime defer must be installed")
	assert.Less(t, cleanup, preflight, "preflight failure must use the established cleanup defer")
	assert.Greater(t, listen, preflight, "preflight must finish before binding")
}

func closureCalls(closure *ast.FuncLit, name string) bool {
	found := false
	ast.Inspect(closure.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && callName(call) == name {
			found = true
		}
		return true
	})
	return found
}

func assignedCallResult(stmt ast.Stmt, name string) (string, bool) {
	assignment, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assignment.Lhs) == 0 {
		return "", false
	}
	for _, rhs := range assignment.Rhs {
		call, ok := rhs.(*ast.CallExpr)
		if !ok || callName(call) != name {
			continue
		}
		identifier, ok := assignment.Lhs[len(assignment.Lhs)-1].(*ast.Ident)
		return identifier.Name, ok
	}
	return "", false
}

func conditionChecksError(expr ast.Expr, name string) bool {
	binary, ok := expr.(*ast.BinaryExpr)
	if !ok || binary.Op != token.NEQ {
		return false
	}
	identifier, ok := binary.X.(*ast.Ident)
	return ok && identifier.Name == name
}

func blockReturns(block *ast.BlockStmt) bool {
	for _, stmt := range block.List {
		if _, ok := stmt.(*ast.ReturnStmt); ok {
			return true
		}
	}
	return false
}

func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}
