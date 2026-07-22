package voice

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func isNATSSubscriberMethod(function *ast.FuncDecl) bool {
	if function == nil || function.Recv == nil || len(function.Recv.List) != 1 {
		return false
	}
	receiverType := function.Recv.List[0].Type
	if pointer, ok := receiverType.(*ast.StarExpr); ok {
		receiverType = pointer.X
	}
	identifier, ok := receiverType.(*ast.Ident)
	return ok && identifier.Name == "NATSSubscriber"
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func declaredStringConstants(file *ast.File) map[string]bool {
	constants := make(map[string]bool)
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			values, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range values.Names {
				if index >= len(values.Values) {
					continue
				}
				if _, literal := stringLiteral(values.Values[index]); literal {
					constants[name.Name] = true
				}
			}
		}
	}
	return constants
}

func safeLogMessage(expression ast.Expr, constants map[string]bool) bool {
	if _, literal := stringLiteral(expression); literal {
		return true
	}
	identifier, ok := expression.(*ast.Ident)
	return ok && constants[identifier.Name]
}

func safeStructuredLogValue(key string, expression ast.Expr) bool {
	switch key {
	case "failure_class":
		_, literal := stringLiteral(expression)
		return literal || isPolicyErrorClassExpression(expression)
	case "action":
		_, literal := stringLiteral(expression)
		return literal
	case "is_dm":
		if identifier, ok := expression.(*ast.Ident); ok {
			return identifier.Name == "true" || identifier.Name == "false"
		}
		selector, ok := expression.(*ast.SelectorExpr)
		return ok && selector.Sel.Name == "isDM"
	case "context":
		_, literal := stringLiteral(expression)
		return literal
	case "count":
		if literal, ok := expression.(*ast.BasicLit); ok {
			return literal.Kind == token.INT
		}
		call, ok := expression.(*ast.CallExpr)
		if !ok {
			return false
		}
		identifier, ok := call.Fun.(*ast.Ident)
		return ok && identifier.Name == "len"
	default:
		return false
	}
}

func isPolicyErrorClassExpression(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	if identifier, ok := call.Fun.(*ast.Ident); ok {
		return identifier.Name == "PolicyErrorClass"
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "PolicyErrorClass"
}

func containsUnsafeDynamicError(expression ast.Expr) bool {
	if isPolicyErrorClassExpression(expression) {
		return false
	}
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			name := strings.ToLower(value.Name)
			if name == "err" || strings.HasSuffix(name, "err") ||
				strings.Contains(name, "error") {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if value.Sel.Name == "Error" {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

func richPresenceLifecycleLogFindings(filename string, source []byte) ([]string, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, filename, source, 0)
	if err != nil {
		return nil, err
	}
	stringConstants := declaredStringConstants(file)
	findings := make([]string, 0)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || !isNATSSubscriberMethod(function) {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Info" && selector.Sel.Name != "Warn" &&
				selector.Sel.Name != "Error" && selector.Sel.Name != "Debug") {
				return true
			}
			if len(call.Args) == 0 {
				findings = append(findings, function.Name.Name+": missing message")
				return true
			}
			if !safeLogMessage(call.Args[0], stringConstants) {
				findings = append(findings, function.Name.Name+": dynamic message")
			}
			structured := call.Args[1:]
			if len(structured)%2 != 0 {
				findings = append(findings, function.Name.Name+": odd structured arguments")
			}
			for index := 0; index+1 < len(structured); index += 2 {
				key, literal := stringLiteral(structured[index])
				if !literal {
					findings = append(findings, function.Name.Name+": dynamic key")
					continue
				}
				if !safeStructuredLogValue(key, structured[index+1]) {
					findings = append(findings, function.Name.Name+": unsafe key/value "+key)
				}
			}
			for _, argument := range call.Args {
				if containsUnsafeDynamicError(argument) {
					findings = append(findings, function.Name.Name+": dynamic error")
				}
			}
			return true
		})
	}
	return findings, nil
}

func TestRichPresenceLifecycleProducerLogsExcludeIdentityAndPayloadFields(t *testing.T) {
	source, err := os.ReadFile("nats.go")
	require.NoError(t, err)
	findings, err := richPresenceLifecycleLogFindings("nats.go", source)
	require.NoError(t, err)
	require.Empty(t, findings)
}

func TestRichPresenceLifecycleLogGuardControls(t *testing.T) {
	positive := []byte(`package voice
func (s *NATSSubscriber) handleJoined() { s.log.Error("failed", "user_id", "secret") }
`)
	findings, err := richPresenceLifecycleLogFindings("positive.go", positive)
	require.NoError(t, err)
	require.NotEmpty(t, findings)
	require.Contains(t, findings, "handleJoined: unsafe key/value user_id")

	aliasedIdentity := []byte(`package voice
func (s *NATSSubscriber) replay() { s.log.Info("x", "thing", userID) }
`)
	findings, err = richPresenceLifecycleLogFindings("alias.go", aliasedIdentity)
	require.NoError(t, err)
	require.Contains(t, findings, "replay: unsafe key/value thing")

	dynamicKey := []byte(`package voice
func (s *NATSSubscriber) replay() { s.log.Info("x", fieldName, "value") }
`)
	findings, err = richPresenceLifecycleLogFindings("dynamic-key.go", dynamicKey)
	require.NoError(t, err)
	require.Contains(t, findings, "replay: dynamic key")

	dynamicContext := []byte(`package voice
func (s *NATSSubscriber) replay() { s.log.Info("x", "context", context) }
`)
	findings, err = richPresenceLifecycleLogFindings("dynamic-context.go", dynamicContext)
	require.NoError(t, err)
	require.Contains(t, findings, "replay: unsafe key/value context")

	countAlias := []byte(`package voice
func (s *NATSSubscriber) replay() { s.log.Info("x", "count", accountID) }
`)
	findings, err = richPresenceLifecycleLogFindings("count-alias.go", countAlias)
	require.NoError(t, err)
	require.Contains(t, findings, "replay: unsafe key/value count")

	dynamicError := []byte(`package voice
func (s *NATSSubscriber) replay() { s.log.Error("failed", "failure_class", replayErr) }
`)
	findings, err = richPresenceLifecycleLogFindings("dynamic.go", dynamicError)
	require.NoError(t, err)
	require.Contains(t, findings, "replay: dynamic error")

	negative := []byte(`package voice
func (s *NATSSubscriber) anyFutureHelper() { s.log.Error("failed", "failure_class", PolicyErrorClass(stateErr)) }
`)
	findings, err = richPresenceLifecycleLogFindings("negative.go", negative)
	require.NoError(t, err)
	require.Empty(t, findings)
}
