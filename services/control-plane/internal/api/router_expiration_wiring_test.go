package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterUsesInjectedPurgeEngineAndDoesNotStartReaper(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "router.go", nil, 0)
	require.NoError(t, err)
	var newRouter *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "NewRouter" {
			newRouter = fn
			return false
		}
		return true
	})
	require.NotNil(t, newRouter)
	var injected, startsWorker, sweepsStragglers bool
	ast.Inspect(newRouter.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := node.X.(*ast.Ident); ok && id.Name == "dependencies" && node.Sel.Name == "PurgeEngine" {
				injected = true
			}
			if node.Sel.Name == "StartWorker" {
				startsWorker = true
			}
			if node.Sel.Name == "SweepStragglers" {
				sweepsStragglers = true
			}
		}
		return true
	})
	assert.True(t, injected, "NewRouter must consume the main-owned PurgeEngine")
	assert.False(t, startsWorker, "router construction must not start a background reaper")
	assert.False(t, sweepsStragglers, "router construction must not start straggler sweeping")
}
