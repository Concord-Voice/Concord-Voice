package presencehistory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type logPrivacyViolation struct {
	Position token.Position
	Field    string
	Reason   string
}

func TestLogEmissionPrivacyGuard(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		violations, err := scanPresenceHistoryLogFile(path)
		if err != nil {
			return err
		}
		for _, violation := range violations {
			t.Errorf("%s:%d logger field %q violates Activity History privacy guard: %s",
				violation.Position.Filename,
				violation.Position.Line,
				violation.Field,
				violation.Reason,
			)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan presencehistory logger calls: %v", err)
	}
}

func scanPresenceHistoryLogSource(filename, source string) ([]logPrivacyViolation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, source, parser.AllErrors)
	if err != nil {
		return nil, err
	}
	return scanPresenceHistoryLogAST(fset, file), nil
}

func scanPresenceHistoryLogFile(path string) ([]logPrivacyViolation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		return nil, err
	}
	return scanPresenceHistoryLogAST(fset, file), nil
}

func scanPresenceHistoryLogAST(fset *token.FileSet, file *ast.File) []logPrivacyViolation {
	var violations []logPrivacyViolation
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		start, ok := presenceHistoryLoggerArgs(call.Fun)
		if !ok {
			return true
		}
		if start == 1 && len(call.Args) > 0 {
			if reason := forbiddenLogValue(call.Args[0]); reason != "" {
				violations = append(violations, logPrivacyViolation{
					Position: fset.Position(call.Args[0].Pos()),
					Field:    "<message>",
					Reason:   reason,
				})
			}
		}
		for index := start; index+1 < len(call.Args); index += 2 {
			field := logFieldName(call.Args[index])
			reason := forbiddenLogField(field)
			if reason == "" {
				reason = forbiddenLogValue(call.Args[index+1])
			}
			if reason == "" {
				continue
			}
			if field == "" {
				field = "<dynamic>"
			}
			violations = append(violations, logPrivacyViolation{
				Position: fset.Position(call.Args[index].Pos()),
				Field:    field,
				Reason:   reason,
			})
		}
		return true
	})
	return violations
}

func logFieldName(expression ast.Expr) string {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return ""
	}
	return value
}

func forbiddenLogField(field string) string {
	if field == "" {
		return "dynamic field names cannot be audited"
	}
	if aggregateName(field) {
		return ""
	}
	normalized := strings.ReplaceAll(strings.ToLower(field), "_", "")
	if rawErrorName(normalized) {
		return "raw errors are prohibited; emit a stable error class"
	}
	if term := forbiddenPrivacyTerm(field); term != "" {
		return "field name contains forbidden term " + term
	}
	return ""
}

func forbiddenLogValue(expression ast.Expr) string {
	if allowedAggregateValue(expression) {
		return ""
	}
	var forbidden string
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || aggregateName(identifier.Name) || strings.HasPrefix(identifier.Name, "Category") {
			return true
		}
		if rawErrorName(identifier.Name) {
			forbidden = "raw error"
			return false
		}
		if term := forbiddenPrivacyTerm(identifier.Name); term != "" {
			forbidden = term
			return false
		}
		return true
	})
	if forbidden != "" {
		return "value expression contains forbidden term " + forbidden
	}
	return ""
}

func forbiddenPrivacyTerm(value string) string {
	normalized := strings.ReplaceAll(strings.ToLower(value), "_", "")
	for _, term := range []string{
		"user",
		"sender",
		"history",
		"cursor",
		"payload",
		"text",
		"emoji",
		"consent",
		"retention",
		"startedat",
		"endedat",
		"recordedat",
		"expiresat",
		"createdat",
		"updatedat",
		"operationid",
	} {
		if strings.Contains(normalized, term) {
			return term
		}
	}
	return ""
}

func TestLogEmissionPrivacyGuardRejectsOperationIDButAllowsOperationClass(t *testing.T) {
	const source = `package fixture

func unsafe(log logger, operationID string) {
	log.Info("unsafe", "operation_id", operationID)
}

func safe(log logger) {
	log.Info("safe", "operation", "presence_reconciliation")
}
`
	violations, err := scanPresenceHistoryLogSource("fixture.go", source)
	if err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1: %#v", len(violations), violations)
	}
}

func aggregateName(value string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(value), "_", "")
	return strings.HasSuffix(normalized, "count") || strings.HasSuffix(normalized, "counts")
}

func rawErrorName(value string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(value), "_", "")
	if strings.HasSuffix(normalized, "class") {
		return false
	}
	return strings.HasSuffix(normalized, "err") || strings.HasSuffix(normalized, "error")
}

func allowedAggregateValue(expression ast.Expr) bool {
	if identifier, ok := expression.(*ast.Ident); ok {
		return aggregateName(identifier.Name)
	}
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == "len"
}

func TestLogEmissionPrivacyGuardDetectsIdentifierAndTimestampValues(t *testing.T) {
	const source = `package fixture

func unsafe(log logger, userID string, item historyItem, retentionDays int, historyCount int, err error, queryErr error, dbError error, dynamicKey func() string) {
	log.Info("identifier", "id", userID)
	log.Warn("timestamp", "at", item.RecordedAt)
	log.With(dynamicKey(), item.Payload).Error("dynamic key")
	log.Info("dynamic count", dynamicKey(), historyCount)
	log.Info("nested timestamp", "duration", time.Since(item.RecordedAt))
	log.Info("retention", "days", retentionDays)
	log.Error("failed", "error", err)
	log.Error("query failed", "cause", queryErr)
	log.Error("database failed", "cause", dbError)
	log.Info(fmt.Sprintf("retention=%d", retentionDays))
	log.Error(err.Error())
}

func safe(log logger, historyCount int, errorClass string, runStart time.Time) {
	log.Info("aggregate", "history_count", historyCount)
	log.Info("category", "category", CategoryCustomText)
	log.Warn("classified", "error_class", errorClass)
	log.Info("duration", "duration", time.Since(runStart))
}
`

	violations, err := scanPresenceHistoryLogSource("fixture.go", source)
	if err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(violations) != 11 {
		t.Fatalf("got %d violations, want 11: %#v", len(violations), violations)
	}
}

func presenceHistoryLoggerArgs(function ast.Expr) (int, bool) {
	selector, ok := function.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	switch selector.Sel.Name {
	case "Info", "Warn", "Error", "Debug", "Fatal":
		return 1, true
	case "With":
		return 0, true
	default:
		return 0, false
	}
}
