package presence

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

var bannedActivityLogImports = map[string]bool{
	"log":      true,
	"log/slog": true,
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger": true,
}

var bannedActivityLogMethods = map[string]bool{
	"Info": true, "Warn": true, "Error": true, "Debug": true,
	"Fatal": true, "Print": true, "Printf": true, "Println": true,
}

func activityLogFindings(filename string, source []byte) ([]string, error) {
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
		if bannedActivityLogImports[path] {
			findings = append(findings, "banned logging import: "+path)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && bannedActivityLogMethods[selector.Sel.Name] {
			findings = append(findings, "banned logging call: "+selector.Sel.Name)
		}
		return true
	})
	return findings, nil
}

func activityProductionLogFindings(root fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, fmt.Errorf("read presence package: %w", err)
	}
	findings := make([]string, 0)
	for _, entry := range entries {
		filename := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") ||
			strings.HasSuffix(filename, "_test.go") {
			continue
		}
		source, err := fs.ReadFile(root, filename)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filename, err)
		}
		fileFindings, err := activityLogFindings(filename, source)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", filename, err)
		}
		for _, finding := range fileFindings {
			findings = append(findings, filename+": "+finding)
		}
	}
	return findings, nil
}

func TestActivityProductionEmitsNoPayloadOrIdentityLogs(t *testing.T) {
	findings, err := activityProductionLogFindings(os.DirFS("."))
	require.NoError(t, err)
	require.Empty(t, findings)
}

func TestActivityProductionLogGuardScansEveryFile(t *testing.T) {
	loggedSource := []byte(`package sample
import "log"
func emit(payload string) { log.Printf("payload=%s", payload) }
`)
	sources := fstest.MapFS{
		"future_activity.go": {Data: loggedSource},
		"policy.go":          {Data: []byte("package sample\n")},
		"ignored_test.go":    {Data: loggedSource},
		"ignored.txt":        {Data: loggedSource},
	}
	findings, err := activityProductionLogFindings(sources)
	require.NoError(t, err)
	require.Contains(t, findings, "future_activity.go: banned logging import: log")
	require.Contains(t, findings, "future_activity.go: banned logging call: Printf")
	require.NotContains(t, strings.Join(findings, "\n"), "ignored_test.go")
	require.NotContains(t, strings.Join(findings, "\n"), "ignored.txt")
}

func TestActivityServiceLogGuardControls(t *testing.T) {
	positive := []byte(`package sample
import "log"
func emit(payload string) { log.Printf("payload=%s", payload) }
`)
	findings, err := activityLogFindings("positive.go", positive)
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	negative := []byte(`package sample
func reconcile(count int) int { return count + 1 }
`)
	findings, err = activityLogFindings("negative.go", negative)
	require.NoError(t, err)
	require.Empty(t, findings)
}
