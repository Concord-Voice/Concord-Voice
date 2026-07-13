package websocket

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	customTextSensitiveLogIdentifier = regexp.MustCompile(
		`(?i)(sender|viewer|user|account|operation.?id|payload|text|emoji|cursor|timestamp|updated.?at|created.?at|recorded.?at|unix|time.?now)`,
	)
	customTextSensitiveLogFormat = regexp.MustCompile(
		`(?i)\b(sender|viewer|user|account|operation.?id|payload|emoji|cursor|timestamp|updated.?at|created.?at|recorded.?at)\b[^%\n]{0,32}%[+#0 .'0-9\[\]*-]*[svqxXdOobUcFGEe]`,
	)
	customTextSensitiveTextLogFormat = regexp.MustCompile(
		`(?i)(^|[^[:alnum:]-])text\b[^%\n]{0,32}%[+#0 .'0-9\[\]*-]*[svqxXdOobUcFGEe]`,
	)
)

func TestCustomTextLogEmissionsExcludeIdentifiersPayloadsCursorsAndTimestamps(t *testing.T) {
	source, err := os.ReadFile("customtext.go")
	require.NoError(t, err)

	violations, err := customTextLogPrivacyViolations("customtext.go", source)
	require.NoError(t, err)
	assert.Empty(t, violations, strings.Join(violations, "\n"))
}

func TestCustomTextLogPrivacyGuardPositiveAndNegativeControls(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		violations int
	}{
		{
			name:       "sender identifier",
			body:       `log.Printf("delivery failed for sender %s", senderID)`,
			violations: 2,
		},
		{
			name:       "payload expression",
			body:       `log.Printf("delivery failed: %v", payload)`,
			violations: 1,
		},
		{
			name:       "status text expression",
			body:       `log.Printf("delivery failed: %q", statusText)`,
			violations: 1,
		},
		{
			name:       "status text format label",
			body:       `log.Printf("status text %q", value)`,
			violations: 1,
		},
		{
			name:       "cursor field",
			body:       `log.Printf("snapshot cursor %q", request.Cursor)`,
			violations: 2,
		},
		{
			name:       "account linked timestamp",
			body:       `log.Printf("snapshot failed at %d", time.Now().Unix())`,
			violations: 1,
		},
		{
			name:       "aggregate count allowed",
			body:       `log.Printf("custom-text clients disconnected: %d", len(recipients))`,
			violations: 0,
		},
		{
			name:       "error class allowed",
			body:       `log.Printf("custom-text delivery failed: %T", err)`,
			violations: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := []byte("package websocket\nfunc example() { " + test.body + " }")
			violations, err := customTextLogPrivacyViolations(test.name+".go", source)
			require.NoError(t, err)
			assert.Len(t, violations, test.violations, strings.Join(violations, "\n"))
		})
	}
}

func customTextLogPrivacyViolations(filename string, source []byte) ([]string, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, source, 0)
	if err != nil {
		return nil, err
	}

	var violations []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isCustomTextLoggerCall(call.Fun) || len(call.Args) == 0 {
			return true
		}
		position := files.Position(call.Pos())
		if format, ok := stringLiteral(call.Args[0]); ok &&
			(customTextSensitiveLogFormat.MatchString(format) ||
				customTextSensitiveTextLogFormat.MatchString(format)) {
			violations = append(violations, fmt.Sprintf(
				"%s:%d sensitive field label in logger format",
				position.Filename,
				position.Line,
			))
		}
		for _, argument := range call.Args[1:] {
			ast.Inspect(argument, func(value ast.Node) bool {
				switch typed := value.(type) {
				case *ast.Ident:
					if customTextSensitiveLogIdentifier.MatchString(typed.Name) {
						violations = append(violations, fmt.Sprintf(
							"%s:%d sensitive logger value %q",
							position.Filename,
							position.Line,
							typed.Name,
						))
					}
				case *ast.BasicLit:
					if literal, ok := stringLiteral(typed); ok &&
						customTextSensitiveLogIdentifier.MatchString(literal) {
						violations = append(violations, fmt.Sprintf(
							"%s:%d sensitive logger literal",
							position.Filename,
							position.Line,
						))
					}
				}
				return true
			})
		}
		return true
	})
	return violations, nil
}

func isCustomTextLoggerCall(function ast.Expr) bool {
	selector, ok := function.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	logger, ok := selector.X.(*ast.Ident)
	if !ok || logger.Name != "log" {
		return false
	}
	switch selector.Sel.Name {
	case "Print", "Printf", "Println":
		return true
	default:
		return false
	}
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}
