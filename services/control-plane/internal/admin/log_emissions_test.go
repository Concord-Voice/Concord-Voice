package admin_test

// Regression test for the observability discipline rule in
// [internal]rules/observability.md § "Core principles" → "No key material" / "No
// PII", extended to the admin auth package (#1688 spec §10).
//
// Locks the discipline: no .go file under internal/admin/ may pass a forbidden
// field — a token-derived fingerprint (suffix, hash prefix, full hash) OR a
// password / assertion value — as a structured-log field to
// <receiver>.log.{Info,Warn,Error,Debug,Fatal} or a .log.With(...) pre-stage.
//
// This is the internal/admin/ sibling of internal/auth/log_emissions_test.go;
// the only delta is the password/assertion field-name additions in
// forbiddenFieldName (admin login handles passwords + WebAuthn assertions).
//
// If this test breaks, EITHER:
//   (a) Real fix needed: a new log call emits a forbidden field. Remove the
//       field (audit outcomes via admin.AuditLog, not structured logs).
//   (b) Test calibration: the AST patterns need to expand for a new variant.
//       Update forbiddenFieldName / forbiddenValueShape below.

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// adminPackageDir is the directory the test scans. When `go test` runs the
// admin package, the process CWD is this package directory, so "." resolves to
// the package's source tree.
const adminPackageDir = "."

type logViolation struct {
	pos       token.Position
	method    string
	fieldName string
	valueExpr string
	reason    string
}

func TestNoForbiddenAdminLogFields(t *testing.T) {
	fset := token.NewFileSet()
	var violations []logViolation

	err := filepath.WalkDir(adminPackageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			return parseErr
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if method := loggerMethod(call.Fun); method != "" {
				scanArgPairs(call.Args, 1, fset, method, &violations)
				return true
			}
			if isWithChainCall(call.Fun) {
				scanArgPairs(call.Args, 0, fset, "With", &violations)
				return true
			}
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("walk error: %v", err)
	}

	for _, v := range violations {
		t.Errorf("%s:%d — %s call emits forbidden field %q with value %s (reason: %s)",
			v.pos.Filename, v.pos.Line, v.method, v.fieldName, v.valueExpr, v.reason)
	}
}

// loggerMethod returns "Info"/"Warn"/"Error"/"Debug"/"Fatal" if fun has the
// shape <chain>.log.<Method>, else "".
func loggerMethod(fun ast.Expr) string {
	methodSel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	method := methodSel.Sel.Name
	switch method {
	case "Info", "Warn", "Error", "Debug", "Fatal":
	default:
		return ""
	}
	logSel, ok := methodSel.X.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if logSel.Sel.Name != "log" {
		return ""
	}
	if !terminatesInIdent(logSel.X) {
		return ""
	}
	return method
}

// isWithChainCall reports whether fun has the shape <chain>.log.With.
func isWithChainCall(fun ast.Expr) bool {
	methodSel, ok := fun.(*ast.SelectorExpr)
	if !ok || methodSel.Sel.Name != "With" {
		return false
	}
	logSel, ok := methodSel.X.(*ast.SelectorExpr)
	if !ok || logSel.Sel.Name != "log" {
		return false
	}
	return terminatesInIdent(logSel.X)
}

// terminatesInIdent reports whether expr resolves to an identifier at the end
// of any selector chain.
func terminatesInIdent(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return terminatesInIdent(n.X)
	default:
		return false
	}
}

// scanArgPairs walks an arg list as [key1, val1, key2, val2, ...] from startIdx
// and appends violations for any pair whose key matches a forbidden field name
// OR whose value matches a forbidden token-derived shape.
func scanArgPairs(args []ast.Expr, startIdx int, fset *token.FileSet, method string, violations *[]logViolation) {
	for i := startIdx; i+1 < len(args); i += 2 {
		keyExpr := args[i]
		valExpr := args[i+1]

		keyName := stringLitValue(keyExpr)
		if keyName == "" {
			if reason := forbiddenValueShape(valExpr); reason != "" {
				*violations = append(*violations, logViolation{
					pos:       fset.Position(valExpr.Pos()),
					method:    method,
					fieldName: "<dynamic>",
					valueExpr: formatExpr(fset, valExpr),
					reason:    reason,
				})
			}
			continue
		}

		if reason := forbiddenFieldName(keyName); reason != "" {
			*violations = append(*violations, logViolation{
				pos:       fset.Position(keyExpr.Pos()),
				method:    method,
				fieldName: keyName,
				valueExpr: formatExpr(fset, valExpr),
				reason:    reason,
			})
			continue
		}

		if reason := forbiddenValueShape(valExpr); reason != "" {
			*violations = append(*violations, logViolation{
				pos:       fset.Position(valExpr.Pos()),
				method:    method,
				fieldName: keyName,
				valueExpr: formatExpr(fset, valExpr),
				reason:    reason,
			})
		}
	}
}

// stringLitValue returns the runtime string value of a string-literal expr,
// resolving escapes (so `"token_\x73uffix"` → "token_suffix").
func stringLitValue(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return unquoted
}

// forbiddenFieldName returns a reason if the field name matches a forbidden
// pattern. Extends the internal/auth set with `password` and `assertion`
// (admin login carries both) per #1688 spec §10.
func forbiddenFieldName(name string) string {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "suffix") {
		return "field name contains 'suffix' — token-suffix variants are forbidden by observability.md"
	}
	if strings.Contains(lower, "hash_prefix") {
		return "field name contains 'hash_prefix' — hash-prefix variants are forbidden by observability.md"
	}
	if lower == "token_hash" {
		return "field name 'token_hash' emits a token hash — full hashes are forbidden by observability.md"
	}
	if strings.Contains(lower, "password") {
		return "field name contains 'password' — passwords must never be logged (#1688 spec §10)"
	}
	if strings.Contains(lower, "assertion") {
		return "field name contains 'assertion' — WebAuthn assertion bytes must never be logged (#1688 spec §10)"
	}
	return ""
}

// forbiddenValueShape returns a reason if the value expression has a
// token-derived slice shape (defense-in-depth against renamed-field reintro).
func forbiddenValueShape(e ast.Expr) string {
	slice, ok := e.(*ast.SliceExpr)
	if !ok {
		return ""
	}
	ident, ok := slice.X.(*ast.Ident)
	if !ok {
		return ""
	}
	identLower := strings.ToLower(ident.Name)

	if strings.Contains(identLower, "hash") &&
		slice.Low == nil && slice.High != nil && slice.Max == nil &&
		isIntLitOrIdent(slice.High) {
		return "value is a hash-prefix slice expression on a 'Hash'-named identifier (forbidden by observability.md)"
	}

	if strings.Contains(identLower, "token") &&
		slice.Low != nil && slice.High == nil {
		if isLenMinusNOf(slice.Low, ident.Name) {
			return "value is a token-suffix slice expression on a 'Token'-named identifier (forbidden by observability.md)"
		}
	}
	return ""
}

func isIntLitOrIdent(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.BasicLit:
		return n.Kind == token.INT
	case *ast.Ident:
		return true
	}
	return false
}

func isLenMinusNOf(expr ast.Expr, identName string) bool {
	binary, ok := expr.(*ast.BinaryExpr)
	if !ok || binary.Op != token.SUB {
		return false
	}
	lenCall, ok := binary.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	lenIdent, ok := lenCall.Fun.(*ast.Ident)
	if !ok || lenIdent.Name != "len" {
		return false
	}
	if len(lenCall.Args) != 1 {
		return false
	}
	argIdent, ok := lenCall.Args[0].(*ast.Ident)
	if !ok || argIdent.Name != identName {
		return false
	}
	return isIntLitOrIdent(binary.Y)
}

func formatExpr(fset *token.FileSet, e ast.Expr) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, e); err != nil {
		return "<unformattable>"
	}
	return buf.String()
}
