package users

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var presenceWriterSensitiveLogValue = regexp.MustCompile(
	`(?i)(sender|target|recipient|viewer|user.?id|account|operation.?id|custom.?text|emoji|payload|ciphertext|encrypted|excluded|updated.?at|created.?at|timestamp)`)

func TestPresenceWriterLogEmissionsExcludeContentIdentifiersAndAccountTimestamps(t *testing.T) {
	for _, filename := range []string{"presence_settings.go", "presence_overrides.go"} {
		source, err := os.ReadFile(filename) // #nosec G304 -- fixed test-only source filenames above
		require.NoError(t, err)
		violations, err := presenceWriterLogViolations(filename, source)
		require.NoError(t, err)
		assert.Empty(t, violations, strings.Join(violations, "\n"))
	}
}

func TestReplaceMyKeysForcedClearLogsUseStableMetadataOnly(t *testing.T) {
	source, err := os.ReadFile("handlers.go") // #nosec G304 -- fixed test-only source filename
	require.NoError(t, err)
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "handlers.go", source, 0)
	require.NoError(t, err)
	targets := map[string]bool{
		"ReplaceMyKeys": true, "replaceMyKeysCoordinated": true, "forcedClearOutcomeClass": true,
	}
	var violations []string
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !targets[function.Name.Name] || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isPresenceWriterLoggerCall(call.Fun) {
				return true
			}
			position := files.Position(call.Pos())
			for _, argument := range call.Args {
				ast.Inspect(argument, func(value ast.Node) bool {
					if identifier, ok := value.(*ast.Ident); ok {
						lower := strings.ToLower(identifier.Name)
						if presenceWriterSensitiveLogValue.MatchString(lower) ||
							lower == "err" || strings.HasSuffix(lower, "err") || lower == "cause" {
							violations = append(violations, fmt.Sprintf(
								"%s:%d sensitive/raw logger value %q", position.Filename, position.Line, identifier.Name,
							))
						}
					}
					return true
				})
			}
			return true
		})
	}
	assert.Empty(t, violations, strings.Join(violations, "\n"))
}

func TestPresenceWriterLogGuardPositiveAndNegativeControls(t *testing.T) {
	for name, bad := range map[string][]byte{
		"direct": []byte(`package users
			func example() { h.log.Error("failed", "sender_id", senderID, "ciphertext", encryptedData) }`),
		"with chain": []byte(`package users
			func example() { h.log.With("recipient_id", recipientID).Error("failed") }`),
		"fatal viewer": []byte(`package users
			func example() { h.log.Fatal("failed", "viewer_id", viewerID) }`),
	} {
		t.Run(name, func(t *testing.T) {
			violations, err := presenceWriterLogViolations("bad.go", bad)
			require.NoError(t, err)
			assert.NotEmpty(t, violations)
		})
	}

	good := []byte(`package users
	func example() { h.log.Error("presence write failed", "error_class", errorClass, "failure_count", count) }`)
	violations, err := presenceWriterLogViolations("good.go", good)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

func TestLegacyCustomStatusCoordinatorAndVoidBroadcasterSymbolsAreGone(t *testing.T) {
	for _, filename := range []string{
		"handlers.go",
		"presence_settings.go",
		"presence_overrides.go",
		"../websocket/customtext.go",
		"../websocket/hub.go",
	} {
		source, err := os.ReadFile(filename) // #nosec G304 -- fixed repository source filenames above
		require.NoError(t, err)
		text := string(source)
		for _, symbol := range []string{
			"customStatusCoordinator",
			"withCustomStatusSender",
			"presenceOverrideBroadcaster",
			"customTextDeliveryCoordinator",
			"withCustomTextDelivery",
			"BroadcastCustomText(",
			"BroadcastCustomTextAudienceDelta(",
		} {
			assert.NotContains(t, text, symbol, "%s still contains retired %s", filename, symbol)
		}
	}
}

func presenceWriterLogViolations(filename string, source []byte) ([]string, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, source, 0)
	if err != nil {
		return nil, err
	}
	var violations []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isPresenceWriterLoggerCall(call.Fun) {
			return true
		}
		position := files.Position(call.Pos())
		for _, argument := range call.Args {
			ast.Inspect(argument, func(value ast.Node) bool {
				identifier, ok := value.(*ast.Ident)
				if ok && presenceWriterSensitiveLogValue.MatchString(identifier.Name) {
					violations = append(violations, fmt.Sprintf(
						"%s:%d sensitive logger value %q", position.Filename, position.Line, identifier.Name,
					))
				}
				literal, ok := value.(*ast.BasicLit)
				if ok && presenceWriterSensitiveLogValue.MatchString(literal.Value) {
					violations = append(violations, fmt.Sprintf(
						"%s:%d sensitive logger label", position.Filename, position.Line,
					))
				}
				return true
			})
		}
		return true
	})
	return violations, nil
}

func isPresenceWriterLoggerCall(function ast.Expr) bool {
	selector, ok := function.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch selector.Sel.Name {
	case "Debug", "Info", "Warn", "Error", "Fatal", "With":
		return true
	default:
		return false
	}
}
