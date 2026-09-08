package auth_test

// Regression test for the observability discipline rule in
// [internal]rules/observability.md § "Core principles" → "No key material".
//
// Locks the post-#1161 cleanup: no .go file under internal/auth/ may pass
// a token-derived fingerprint (suffix, hash prefix, or full hash) as a
// structured-log field to <receiver>.log.{Info,Warn,Error,Debug}.
//
// If this test breaks, EITHER:
//   (a) Real fix needed: a new log call was added that emits a forbidden
//       field. Remove the field; add `request_id` if correlation is needed
//       (use middleware.RequestIDContextKey).
//   (b) Test calibration: the AST detection patterns need to expand to
//       cover a new variant. Update forbiddenFieldName/forbiddenValueShape
//       below and document the new variant in the spec at
//       [internal]specs/2026-05-27-1161-strip-token-fingerprints-design.md.
//
// Frontend prior art: PR #714's eslint-no-raw-err-console.test.ts uses the
// same shape (discipline rule + AST-walking test that locks the rule).
//
// Spec: [internal]specs/2026-05-27-1161-strip-token-fingerprints-design.md
// Issue: https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/1161

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

	"github.com/stretchr/testify/require"
)

// authPackageDir is the directory the test scans. When `go test` runs the
// auth package, the process CWD is this package directory, so "." resolves
// to the package's source tree.
const authPackageDir = "."

type violation struct {
	pos       token.Position
	method    string // "Info" | "Warn" | "Error" | "Debug"
	fieldName string // forbidden field-name string literal, if matched
	valueExpr string // formatted value expression, for the error message
	reason    string // human-readable why-it's-forbidden
}

func TestNoTokenFingerprintLogFields(t *testing.T) {
	fset := token.NewFileSet()
	var violations []violation

	err := filepath.WalkDir(authPackageDir, func(path string, d fs.DirEntry, err error) error {
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

			// Direct logger call: <chain>.log.<Method>(msg, k1, v1, k2, v2, ...)
			// kvPairs start at arg index 1 (the msg is at index 0).
			if method := loggerMethod(call.Fun); method != "" {
				scanArgPairs(call.Args, 1, fset, method, &violations)
				return true
			}

			// With-chain call: <chain>.log.With(k1, v1, k2, v2, ...)
			// All args are kvPairs (no leading msg). Detected on the With
			// call itself because the outer .Info/.Warn/etc. has the With
			// CallExpr as its receiver (not a SelectorExpr), so the direct
			// matcher above can't see through it. Catches pre-staging
			// bypasses like `h.log.With("token_hash", x).Info("...")`.
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

func TestRecoveryForcedClearLogPrivacy(t *testing.T) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, "handlers.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]bool{
		"validateAndConsumeRecoveryToken": true,
		"prepareRecoveryPassword":         true,
		"recoveryWork":                    true,
		"execRecoveryTx":                  true,
		"executeRecoveryTransaction":      true,
		"respondRecoveryReadinessFailure": true,
		"respondRecoveryPresenceFailure":  true,
		"parseRecoverySenderID":           true,
		"RecoveryResetPassword":           true,
		"RecoveryResetAccount":            true,
	}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !targets[function.Name.Name] || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || loggerMethod(call.Fun) == "" {
				return true
			}
			for _, argument := range call.Args[1:] {
				formatted := strings.ToLower(formatExpr(files, argument))
				for _, forbidden := range []string{
					"claims", "user_id", "userid", "senderid", "operationid",
					"custom_text", "emoji", "payload", "ciphertext", "encrypted",
					"updated_at", "created_at", "timestamp", "accountlinked",
				} {
					if strings.Contains(formatted, forbidden) {
						t.Errorf("%s:%d recovery forced-clear log contains %q in %s",
							files.Position(argument.Pos()).Filename,
							files.Position(argument.Pos()).Line,
							forbidden,
							formatted,
						)
					}
				}
				if formatted == "err" || formatted == "cause" || strings.HasSuffix(formatted, ".err") {
					t.Errorf("%s:%d recovery forced-clear log emits raw error %s",
						files.Position(argument.Pos()).Filename,
						files.Position(argument.Pos()).Line,
						formatted,
					)
				}
			}
			return true
		})
	}
}

// securityEventProducerFiles is the complete Task 3 production surface. Keep
// this explicit rather than walking internal/: a new security-event producer
// must be deliberately added to the privacy review boundary.
var securityEventProducerFiles = []string{
	"handlers.go",
	"oauth_adapter.go",
	"../mfa/handlers.go",
	"../sessions/handlers.go",
	"../credepoch/credepoch.go",
	"../middleware/ratelimit.go",
	"../middleware/authban.go",
	"../middleware/cfaccess.go",
	"../middleware/attestation.go",
	"../api/router.go",
	"../admin/audit.go",
	"../rbac/audit.go",
	"../opsmetrics/receiver.go",
	"../websocket/hub.go",
	"../media/disk_watermark.go",
	"../attestation/cache.go",
	"../attestation/publish_handler.go",
	"../attestation/verify_handler.go",
}

// TestSecurityEventsUseOnlyClosedConstants keeps Nightwatch's producer surface
// free of request, identity, credential, and error data. Every explicitly set
// Event field must be a securityevent constant, except the three closed,
// server-derived identifier tuples below. Their exact use counts prevent either
// a copied or removed exception from silently broadening the boundary.
func TestSecurityEventsUseOnlyClosedConstants(t *testing.T) {
	fset := token.NewFileSet()
	var violations []string
	allowedUses := make(map[closedEventIdentifierAllowance]int)
	literals := 0
	for _, path := range securityEventProducerFiles {
		fileLiterals := 0
		file, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if isNightwatchVerdictLiteral(lit) {
				literals++
				fileLiterals++
				validateClosedEventLiteral(fset, path, lit, &violations, allowedUses)
				return true
			}
			if !isSecurityEventLiteral(lit) {
				return true
			}
			literals++
			fileLiterals++
			validateClosedEventLiteral(fset, path, lit, &violations, allowedUses)
			return true
		})
		scanClosedEventAssignments(fset, path, file, &violations, allowedUses)
		if fileLiterals == 0 {
			t.Errorf("security-event privacy guard scanned no producer literals in %s", path)
		}
	}
	if literals == 0 {
		t.Fatal("security-event privacy guard scanned no producer literals")
	}
	for _, allowance := range closedEventIdentifierAllowances {
		if actual := allowedUses[allowance]; actual != allowance.count {
			violations = append(violations, "closed security-event identifier "+allowance.path+"."+allowance.field+"="+allowance.identifier+" used "+strconv.Itoa(actual)+" times, want "+strconv.Itoa(allowance.count))
		}
	}
	if len(violations) != 0 {
		t.Error(strings.Join(violations, "\n"))
	}
}

func TestSecurityEventPrivacyGuardRejectsRequestDerivedField(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "request_derived.go", `package producer
import "securityevent"
func emit(routeFromRequest func() string) {
	_ = securityevent.Event{ReasonCode: securityevent.ReasonCode(routeFromRequest())}
}`, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok || !isSecurityEventLiteral(lit) {
			return true
		}
		for _, entry := range lit.Elts {
			field, ok := entry.(*ast.KeyValueExpr)
			if ok && !isSecurityEventConstant(field.Value) {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("privacy guard must reject a request-derived Event field")
	}
}

func TestSecurityEventPrivacyGuardRejectsRequestDerivedVerdict(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "request_derived_verdict.go", `package producer
import (
  "middleware"
  "securityevent"
)
func emit(requestValue func() string) {
  _ = middleware.NightwatchVerdict{Reason: securityevent.ReasonCode(requestValue())}
}`, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if ok && isNightwatchVerdictLiteral(lit) {
			validateClosedEventLiteral(fset, "request_derived_verdict.go", lit, &violations, nil)
		}
		return true
	})
	if len(violations) == 0 {
		t.Fatal("privacy guard must reject a request-derived NightwatchVerdict field")
	}
}

func TestSecurityEventPrivacyGuardRejectsAssignmentForm(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "assignment.go", `package producer
import (
  "middleware"
  "securityevent"
)
func emit(requestValue func() string) {
  var event securityevent.Event
  event.ReasonCode = securityevent.ReasonCode(requestValue())
  var verdict middleware.NightwatchVerdict
  verdict.Reason = securityevent.ReasonCode(requestValue())
}`, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	scanClosedEventAssignments(fset, "assignment.go", file, &violations, nil)
	joined := strings.Join(violations, "\n")
	require.Contains(t, joined, "ReasonCode must be a securityevent constant")
	require.Contains(t, joined, "Reason must be a securityevent constant")
}

type closedEventIdentifierAllowance struct {
	path       string
	field      string
	identifier string
	count      int
}

var closedEventIdentifierAllowances = []closedEventIdentifierAllowance{
	{path: "handlers.go", field: "AuthMethod", identifier: "primaryAuthMethod", count: 2},
	{path: "../admin/audit.go", field: "Outcome", identifier: "outcome", count: 5},
	{path: "../rbac/audit.go", field: "EvidenceRef", identifier: "evidenceRef", count: 1},
	// authenticateForRevoke receives only closed route constants from its three callers.
	{path: "../sessions/handlers.go", field: "RouteTemplate", identifier: "route", count: 2},
}

func TestSecurityEventPrivacyGuardRejectsNonExactClosedIdentifier(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		field string
		expr  ast.Expr
	}{
		{name: "renamed", path: "handlers.go", field: "AuthMethod", expr: ast.NewIdent("primaryMethod")},
		{name: "cross field", path: "handlers.go", field: "Outcome", expr: ast.NewIdent("primaryAuthMethod")},
		{name: "cross file", path: "../admin/audit.go", field: "AuthMethod", expr: ast.NewIdent("primaryAuthMethod")},
		{name: "call expression", path: "handlers.go", field: "AuthMethod", expr: &ast.CallExpr{Fun: ast.NewIdent("primaryAuthMethod")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := closedEventIdentifierAllowanceFor(test.path, test.field, test.expr); ok {
				t.Fatal("privacy guard accepted a non-exact identifier")
			}
		})
	}
}

func validateClosedEventLiteral(fset *token.FileSet, path string, lit *ast.CompositeLit, violations *[]string, allowedUses map[closedEventIdentifierAllowance]int) {
	for _, entry := range lit.Elts {
		field, ok := entry.(*ast.KeyValueExpr)
		if !ok {
			*violations = append(*violations, fset.Position(entry.Pos()).String()+": unkeyed security-event composite literal is prohibited")
			continue
		}
		key, _ := field.Key.(*ast.Ident)
		if key != nil {
			validateClosedEventField(fset, path, key.Name, field.Value, violations, allowedUses)
		}
	}
}

// scanClosedEventAssignments extends the composite-literal guard to typed
// Event and NightwatchVerdict variables populated field-by-field. Scanning
// each function independently prevents a same-named local in another function
// from inheriting an Event classification.
func scanClosedEventAssignments(fset *token.FileSet, path string, file *ast.File, violations *[]string, allowedUses map[closedEventIdentifierAllowance]int) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		eventVariables := make(map[string]bool)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.DeclStmt:
				declaration, ok := statement.Decl.(*ast.GenDecl)
				if !ok || declaration.Tok != token.VAR {
					return true
				}
				for _, specification := range declaration.Specs {
					valueSpec, ok := specification.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, name := range valueSpec.Names {
						if isClosedEventType(valueSpec.Type) || isClosedEventLiteralAt(valueSpec.Values, index, len(valueSpec.Names)) {
							eventVariables[name.Name] = true
						}
					}
				}
			case *ast.AssignStmt:
				for index, left := range statement.Lhs {
					right, ok := assignmentValue(statement.Rhs, index, len(statement.Lhs))
					if !ok {
						continue
					}
					if name, ok := left.(*ast.Ident); ok && isClosedEventLiteralExpression(right) {
						eventVariables[name.Name] = true
						continue
					}
					selector, ok := left.(*ast.SelectorExpr)
					if !ok || !eventVariables[identifierName(selector.X)] {
						continue
					}
					validateClosedEventAssignmentField(fset, path, function.Name.Name, selector.Sel.Name, right, violations, allowedUses)
				}
			}
			return true
		})
	}
}

func validateClosedEventAssignmentField(fset *token.FileSet, path, functionName, fieldName string, value ast.Expr, violations *[]string, allowedUses map[closedEventIdentifierAllowance]int) {
	if isClosedNightwatchVerdictTransfer(path, functionName, fieldName, value) {
		return
	}
	validateClosedEventField(fset, path, fieldName, value, violations, allowedUses)
}

// isClosedNightwatchVerdictTransfer permits exactly the typed boundary adapter
// in router.go. It does not exempt the function: a new field or any value that
// is not one of these already-closed verdict fields remains a violation.
func isClosedNightwatchVerdictTransfer(path, functionName, fieldName string, value ast.Expr) bool {
	if path != "../api/router.go" || functionName != "nightwatchVerdictEvent" {
		return false
	}
	if fieldName == "RouteTemplate" {
		identifier, ok := value.(*ast.Ident)
		return ok && identifier.Name == "route"
	}
	selector, ok := value.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "verdict" {
		return false
	}
	return map[string]string{
		"EventType":  "EventType",
		"Outcome":    "Outcome",
		"Severity":   "Severity",
		"ReasonCode": "Reason",
		"AuthMethod": "AuthMethod",
	}[fieldName] == selector.Sel.Name
}

func validateClosedEventField(fset *token.FileSet, path, fieldName string, value ast.Expr, violations *[]string, allowedUses map[closedEventIdentifierAllowance]int) {
	if isSecurityEventConstant(value) {
		return
	}
	if allowance, ok := closedEventIdentifierAllowanceFor(path, fieldName, value); ok {
		if allowedUses != nil {
			allowedUses[allowance]++
		}
		return
	}
	*violations = append(*violations, fset.Position(value.Pos()).String()+": "+fieldName+" must be a securityevent constant")
	ast.Inspect(value, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || !forbiddenSecurityEventIdentifier(identifier.Name) {
			return true
		}
		*violations = append(*violations, fset.Position(identifier.Pos()).String()+": prohibited security-event data identifier "+identifier.Name)
		return true
	})
}

func assignmentValue(values []ast.Expr, index, count int) (ast.Expr, bool) {
	if len(values) != count || index >= len(values) {
		return nil, false
	}
	return values[index], true
}

func isClosedEventLiteralAt(values []ast.Expr, index, count int) bool {
	value, ok := assignmentValue(values, index, count)
	return ok && isClosedEventLiteralExpression(value)
}

func isClosedEventLiteralExpression(expression ast.Expr) bool {
	literal, ok := expression.(*ast.CompositeLit)
	return ok && (isSecurityEventLiteral(literal) || isNightwatchVerdictLiteral(literal))
}

func identifierName(expression ast.Expr) string {
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func TestSecurityEventPrivacyGuardRejectsUnkeyedCompositeLiterals(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "unkeyed.go", `package producer
import ("middleware"; "securityevent")
func emit() {
  _ = securityevent.Event{securityevent.EventAuthentication}
  _ = middleware.NightwatchVerdict{securityevent.EventAuthentication}
}`, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if ok && (isSecurityEventLiteral(lit) || isNightwatchVerdictLiteral(lit)) {
			validateClosedEventLiteral(fset, "unkeyed.go", lit, &violations, nil)
		}
		return true
	})
	require.Contains(t, strings.Join(violations, "\n"), "unkeyed security-event composite literal is prohibited")
}

func closedEventIdentifierAllowanceFor(path, field string, expr ast.Expr) (closedEventIdentifierAllowance, bool) {
	identifier, ok := expr.(*ast.Ident)
	if !ok {
		return closedEventIdentifierAllowance{}, false
	}
	for _, allowance := range closedEventIdentifierAllowances {
		if allowance.path == path && allowance.field == field && allowance.identifier == identifier.Name {
			return allowance, true
		}
	}
	return closedEventIdentifierAllowance{}, false
}

func isSecurityEventLiteral(lit *ast.CompositeLit) bool {
	return isSecurityEventType(lit.Type)
}

func isSecurityEventType(expression ast.Expr) bool {
	typeName, ok := expression.(*ast.SelectorExpr)
	if !ok || typeName.Sel.Name != "Event" {
		return false
	}
	pkg, ok := typeName.X.(*ast.Ident)
	return ok && pkg.Name == "securityevent"
}

func isNightwatchVerdictLiteral(lit *ast.CompositeLit) bool {
	return isNightwatchVerdictType(lit.Type)
}

func isNightwatchVerdictType(expression ast.Expr) bool {
	switch typeName := expression.(type) {
	case *ast.Ident:
		// Middleware producers instantiate their local type directly; auth's
		// synthetic regression fixture qualifies it as middleware.NightwatchVerdict.
		return typeName.Name == "NightwatchVerdict"
	case *ast.SelectorExpr:
		if typeName.Sel.Name != "NightwatchVerdict" {
			return false
		}
		pkg, ok := typeName.X.(*ast.Ident)
		return ok && pkg.Name == "middleware"
	default:
		return false
	}
}

func isClosedEventType(expression ast.Expr) bool {
	return isSecurityEventType(expression) || isNightwatchVerdictType(expression)
}

func isSecurityEventConstant(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "securityevent"
}

func forbiddenSecurityEventIdentifier(name string) bool {
	lower := strings.ToLower(name)
	for _, forbidden := range []string{"user", "email", "ip", "token", "device", "machine", "socket", "server", "channel", "error", "cause", "stack", "code", "credential"} {
		if strings.Contains(lower, forbidden) {
			return true
		}
	}
	return false
}

// loggerMethod returns "Info"/"Warn"/"Error"/"Debug"/"Fatal" if fun has the
// shape <chain>.log.<Method>, else returns "". Matches the structured-logger
// pattern used throughout services/control-plane, including nested-receiver
// chains like h.Handler.log.Info(...). `Fatal` is included because
// pkg/logger/logger.go's wrapper type defines it (and writes a structured
// line before exiting). `LogAttrs` is intentionally OMITTED — different
// arg shape (typed attributes, not kv-pairs); not idiomatic in this
// codebase. If LogAttrs adoption grows, add a separate matcher.
func loggerMethod(fun ast.Expr) string {
	methodSel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	method := methodSel.Sel.Name
	switch method {
	case "Info", "Warn", "Error", "Debug", "Fatal":
		// fall through
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

	// Receiver may be a single identifier (h, handler) OR a nested selector
	// chain (h.Handler, h.Inner.Service). Recursively descend to verify the
	// chain terminates in an *ast.Ident. Closes the bypass class where a
	// future delegating struct emits via h.Handler.log.Info(...).
	if !terminatesInIdent(logSel.X) {
		return ""
	}

	return method
}

// isWithChainCall reports whether fun has the shape <chain>.log.With —
// used to detect logger.With(...) pre-staging bypasses. With() takes
// kvPairs and returns a child logger; if any of those kvPairs is a
// forbidden field, the eventual emission carries it whether or not the
// outer .Info/.Warn call's own arg list contains forbidden fields.
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

// terminatesInIdent reports whether expr resolves to an identifier at the
// end of any selector chain. (x → true; x.y → true; x.y.z → true;
// foo().y → false; x[0].y → false.) Used by loggerMethod and
// isWithChainCall to verify the receiver chain is rooted in a named symbol
// rather than a call/index/literal expression.
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

// scanArgPairs walks an arg list as [key1, val1, key2, val2, ...] starting
// at startIdx, and appends violations for any pair whose key matches a
// forbidden field name OR whose value matches a forbidden token-derived
// shape. Used for both direct logger calls (startIdx=1, skipping msg at 0)
// and With-chain calls (startIdx=0, no leading msg).
func scanArgPairs(args []ast.Expr, startIdx int, fset *token.FileSet, method string, violations *[]violation) {
	for i := startIdx; i+1 < len(args); i += 2 {
		keyExpr := args[i]
		valExpr := args[i+1]

		keyName := stringLitValue(keyExpr)
		if keyName == "" {
			// Skip the field-name check for non-literal keys (we can't
			// audit dynamic field names statically) — but STILL run the
			// value-shape check, since a suspicious value derivation is
			// forbidden regardless of the key shape. Closes the dynamic-
			// key bypass class flagged by Gitar on PR #1216:
			// h.log.Info("msg", dynamicKey(), tokenHash[:16]) would
			// otherwise skip the entire pair. The placeholder field name
			// "<dynamic>" marks the violation as a dynamic-key case in
			// the error output.
			if reason := forbiddenValueShape(valExpr); reason != "" {
				*violations = append(*violations, violation{
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
			*violations = append(*violations, violation{
				pos:       fset.Position(keyExpr.Pos()),
				method:    method,
				fieldName: keyName,
				valueExpr: formatExpr(fset, valExpr),
				reason:    reason,
			})
			continue
		}

		if reason := forbiddenValueShape(valExpr); reason != "" {
			*violations = append(*violations, violation{
				pos:       fset.Position(valExpr.Pos()),
				method:    method,
				fieldName: keyName,
				valueExpr: formatExpr(fset, valExpr),
				reason:    reason,
			})
		}
	}
}

// stringLitValue returns the runtime string value of a string-literal
// expression. Uses strconv.Unquote so it correctly handles BOTH
// double-quoted strings with escape sequences (e.g., `"token_\x73uffix"`
// resolves to "token_suffix") AND backtick raw strings. Returns "" for
// non-string-literal expressions or any literal strconv can't unquote.
//
// The escape-resolution behavior is load-bearing: without it, a leak
// written as `"token_\x73uffix"` would pass the field-name check (the
// raw source slot wouldn't match the substring "suffix") while producing
// "token_suffix" at runtime — a silent bypass.
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

// forbiddenFieldName returns a reason string if the field name matches a
// forbidden pattern from spec decision 5, else "".
func forbiddenFieldName(name string) string {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "suffix") {
		return "field name contains 'suffix' — token-suffix variants are forbidden by observability.md"
	}
	if strings.Contains(lower, "hash_prefix") {
		return "field name contains 'hash_prefix' — hash-prefix variants are forbidden by observability.md"
	}
	if lower == "token_hash" {
		return "field name 'token_hash' emits a refresh-token hash — full hashes are forbidden by observability.md"
	}
	return ""
}

// forbiddenValueShape returns a reason string if the value expression has a
// token-derived shape from spec decision 5 (defense-in-depth against
// renamed-field reintroduction), else "".
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

	// Pattern A: <ident>[:N] where ident name contains "hash". The N may
	// be an integer literal OR a named constant (e.g., hashPrefixLen) —
	// accepting both closes the "extract magic number to a const" bypass.
	// AST shape: Low == nil && High != nil && Max == nil.
	if strings.Contains(identLower, "hash") &&
		slice.Low == nil && slice.High != nil && slice.Max == nil &&
		isIntLitOrIdent(slice.High) {
		return "value is a hash-prefix slice expression on a 'Hash'-named identifier (forbidden by observability.md)"
	}

	// Pattern B: <ident>[len(<ident>)-N:] where ident name contains "token".
	// N may be an integer literal OR a named constant — see Pattern A note.
	// AST shape: Low != nil && High == nil. Low must be (len(ident) - <N|ident>).
	if strings.Contains(identLower, "token") &&
		slice.Low != nil && slice.High == nil {
		if isLenMinusNOf(slice.Low, ident.Name) {
			return "value is a token-suffix slice expression on a 'Token'-named identifier (forbidden by observability.md)"
		}
	}

	return ""
}

// isIntLitOrIdent reports whether expr is an integer literal OR an
// identifier (which would resolve to a typed constant or local var).
// Used by forbiddenValueShape to detect slice bounds that aren't bare
// numeric literals — closes the named-constant bypass class.
func isIntLitOrIdent(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.BasicLit:
		return n.Kind == token.INT
	case *ast.Ident:
		return true
	}
	return false
}

// isLenMinusNOf reports whether expr matches the AST shape
// `len(identName) - <N>` where <N> is an integer literal or an identifier
// (typed constant or var). Accepting identifiers on the RHS closes the
// named-constant bypass: `len(tokenHash) - tokenSuffixLen` would otherwise
// slip past a literal-only check.
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

// formatExpr formats an AST expression back to Go source for error messages.
// Returns the literal "<unformattable>" if formatting fails (rare — only
// happens on AST nodes that go/format cannot serialize back to source).
func formatExpr(fset *token.FileSet, e ast.Expr) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, e); err != nil {
		return "<unformattable>"
	}
	return buf.String()
}
