package presence

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

//go:embed policy.go policy_state.go policy_types.go minimize.go
var policySourceFiles embed.FS

var bannedPolicyLogMethods = map[string]bool{
	"Info": true, "Warn": true, "Error": true, "Debug": true,
	"Fatal": true, "Print": true, "Printf": true, "Println": true,
}

var bannedPolicyLogImports = map[string]bool{
	"log":      true,
	"log/slog": true,
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger": true,
}

func policyLogFindings(filename string, source []byte) ([]string, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, filename, source, 0)
	if err != nil {
		return nil, err
	}

	findings := make([]string, 0)
	for _, imported := range file.Imports {
		path, unquoteErr := strconv.Unquote(imported.Path.Value)
		if unquoteErr != nil {
			return nil, unquoteErr
		}
		if bannedPolicyLogImports[path] {
			findings = append(findings, "banned logging import: "+path)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && bannedPolicyLogMethods[selector.Sel.Name] {
			findings = append(findings, "banned logging call: "+selector.Sel.Name)
		}
		return true
	})
	return findings, nil
}

func TestPolicyImplementationEmitsNoLogs(t *testing.T) {
	for _, filename := range []string{"policy.go", "policy_state.go", "policy_types.go", "minimize.go"} {
		source, err := policySourceFiles.ReadFile(filename)
		require.NoError(t, err)
		findings, err := policyLogFindings(filename, source)
		require.NoError(t, err)
		require.Empty(t, findings, "%s must remain log-free", filename)
	}
}

func TestPolicyLogGuardControls(t *testing.T) {
	positive := []byte(`package sample
type logger interface { Error(string, ...any) }
func emit(log logger, channelID string) { log.Error("failed", "channel_id", channelID) }
`)
	findings, err := policyLogFindings("positive.go", positive)
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	negative := []byte(`package sample
func authorize(channelID string) bool { return channelID != "" }
`)
	findings, err = policyLogFindings("negative.go", negative)
	require.NoError(t, err)
	require.Empty(t, findings)
}
