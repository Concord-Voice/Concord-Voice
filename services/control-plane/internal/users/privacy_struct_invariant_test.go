package users_test

// Structural regression locks for the #2765 step-up gate.
//
// buildPrivacyClauses turns EVERY non-nil field of updatePrivacyRequest into a
// SQL `SET` clause. That is why the step-up credentials live in a separate
// struct (updatePrivacyStepUp) that it cannot see — the isolation is meant to
// be structural, not a matter of remembering. These two tests are what make
// that guarantee load-bearing rather than aspirational:
//
//  1. updatePrivacyRequest may only hold pointer-to-scalar column fields. A
//     `string` field on it is exactly how a credential would reach the SQL
//     builder.
//  2. privacySettingsResponse may not gain a credential-shaped field. That
//     would put a submitted password back on the wire in the 200 body — the
//     same class of leak the OpenAPI request/response split closes.
//
// If one of these breaks, do NOT relax the test. Move the offending field into
// updatePrivacyStepUp (case 1) or off the response struct entirely (case 2).
//
// AST-walking rather than compile-time because Go has no way to express "this
// struct admits only these type shapes". Prior art for the technique:
// internal/auth/log_emissions_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const privacyHandlersFile = "handlers.go"

// credentialFieldFragments are name fragments that can only mean a submitted
// credential.
//
// Deliberately narrow. A bare "code" fragment matches AutoAcceptFriendCodes,
// and a bare "mfa" matches a legitimate MFAEnabled status field — neither is a
// credential, and a lock that cries wolf gets relaxed by the next person who
// trips it. Match the concatenated Go spelling ("mfacode") and the json
// spelling ("mfa_code") instead, so MFACode is caught and MFAEnabled is not.
var credentialFieldFragments = []string{
	"password", "mfacode", "mfa_code", "credential", "secret", "passphrase",
}

// findStruct returns the named struct's field list from the given file.
func findStruct(t *testing.T, filename, structName string) *ast.FieldList {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
	require.NoError(t, err, "parse %s", filename)

	var fields *ast.FieldList
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name == nil || ts.Name.Name != structName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		fields = st.Fields
		return false
	})

	require.NotNilf(t, fields, "struct %s not found in %s — was it renamed? "+
		"Do not delete this test; retarget it.", structName, filename)
	return fields
}

// TestUpdatePrivacyRequestHoldsOnlyPointerScalars locks invariant 1.
//
// Every field of updatePrivacyRequest is a privacy_settings COLUMN and must be
// a pointer to a scalar, so absent / true / false stay distinguishable AND so
// no string-shaped value can ride into buildPrivacyClauses.
func TestUpdatePrivacyRequestHoldsOnlyPointerScalars(t *testing.T) {
	fields := findStruct(t, privacyHandlersFile, "updatePrivacyRequest")
	require.NotEmpty(t, fields.List, "updatePrivacyRequest unexpectedly has no fields")

	for _, field := range fields.List {
		star, ok := field.Type.(*ast.StarExpr)
		require.Truef(t, ok,
			"field %s on updatePrivacyRequest is not a pointer. Every field here becomes a "+
				"SQL SET clause via buildPrivacyClauses; a non-pointer also destroys the "+
				"absent-vs-false distinction the partial update depends on. If this is a "+
				"step-up credential, it belongs on updatePrivacyStepUp (#2765).",
			fieldNames(field))

		ident, ok := star.X.(*ast.Ident)
		require.Truef(t, ok,
			"field %s on updatePrivacyRequest points at a non-basic type. Only *bool and "+
				"*int are permitted (#2765).", fieldNames(field))

		require.Containsf(t, []string{"bool", "int"}, ident.Name,
			"field %s on updatePrivacyRequest is *%s. Only *bool and *int are permitted — "+
				"a *string here is precisely how a credential would reach the SQL builder. "+
				"Put it on updatePrivacyStepUp instead (#2765).",
			fieldNames(field), ident.Name)
	}
}

// TestPrivacySettingsResponseCarriesNoCredential locks invariant 2.
//
// The 200 body must never echo a submitted credential. This catches an
// accidental RETURNING widening or a copy-paste from the request struct.
func TestPrivacySettingsResponseCarriesNoCredential(t *testing.T) {
	fields := findStruct(t, privacyHandlersFile, "privacySettingsResponse")
	require.NotEmpty(t, fields.List, "privacySettingsResponse unexpectedly has no fields")

	for _, field := range fields.List {
		name := strings.ToLower(fieldNames(field))
		tag := ""
		if field.Tag != nil {
			tag = strings.ToLower(field.Tag.Value)
		}
		for _, fragment := range credentialFieldFragments {
			require.NotContainsf(t, name, fragment,
				"field %s on privacySettingsResponse looks credential-shaped (%q). The "+
					"response must never echo a submitted credential (#2765).",
				fieldNames(field), fragment)
			require.NotContainsf(t, tag, fragment,
				"json tag on privacySettingsResponse field %s looks credential-shaped (%q). "+
					"The response must never echo a submitted credential (#2765).",
				fieldNames(field), fragment)
		}
	}
}

// TestUpdatePrivacyStepUpIsSeparateFromColumns proves the two halves are
// genuinely distinct types. If someone merges them back together the isolation
// argument collapses silently, because buildPrivacyClauses would once again be
// able to see a credential.
func TestUpdatePrivacyStepUpIsSeparateFromColumns(t *testing.T) {
	stepUp := findStruct(t, privacyHandlersFile, "updatePrivacyStepUp")

	got := make([]string, 0, len(stepUp.List))
	for _, field := range stepUp.List {
		got = append(got, fieldNames(field))
	}
	require.ElementsMatch(t, []string{"CurrentPassword", "MFACode"}, got,
		"updatePrivacyStepUp should carry exactly the two step-up factors. Adding a "+
			"privacy COLUMN here would mean it never reaches buildPrivacyClauses and so "+
			"silently stops being persisted (#2765).")
}

func fieldNames(field *ast.Field) string {
	names := make([]string, 0, len(field.Names))
	for _, n := range field.Names {
		names = append(names, n.Name)
	}
	if len(names) == 0 {
		return "<embedded>"
	}
	return strings.Join(names, ",")
}
