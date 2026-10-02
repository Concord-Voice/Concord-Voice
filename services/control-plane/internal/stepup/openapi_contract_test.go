package stepup

// Drift tests between what this package puts on the wire and what
// docs/api/openapi.yaml declares, from review of #3509: four #3455 purposes
// reached the server and the desktop but not the WebAuthn inline enum, and
// the budget and lock-conflict flags a client routes on appeared only in
// examples, under a schema that declared `error` alone. The module carries no
// YAML parser as a direct dependency, so these read the spec by its
// indentation, which is two spaces per level throughout the file.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openAPILines(t *testing.T) []string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "..", "docs", "api", "openapi.yaml")
	contents, err := os.ReadFile(path) // #nosec G304 -- path is derived from this fixed test source file.
	require.NoError(t, err)
	return strings.Split(string(contents), "\n")
}

// indentOf is the number of leading spaces, or -1 for a blank line.
func indentOf(line string) int {
	if strings.TrimSpace(line) == "" {
		return -1
	}
	return len(line) - len(strings.TrimLeft(line, " "))
}

// block returns the lines under the first key line equal to header at the
// given indent, from start, up to the next line indented no deeper. A compact
// sequence's items sit at the key's own indent, so those belong to it too.
func block(t *testing.T, lines []string, start int, indent int, key string) []string {
	t.Helper()
	header := strings.Repeat(" ", indent) + key + ":"
	for i := start; i < len(lines); i++ {
		if lines[i] != header {
			continue
		}
		end := i + 1
		for end < len(lines) && belongsUnder(lines[end], indent) {
			end++
		}
		return lines[i+1 : end]
	}
	require.Failf(t, "openapi.yaml", "%q not found", header)
	return nil
}

func belongsUnder(line string, indent int) bool {
	n := indentOf(line)
	return n == -1 || n > indent || (n == indent && strings.HasPrefix(strings.TrimSpace(line), "- "))
}

// component returns a components section's entry, e.g. ("schemas", "Error").
func component(t *testing.T, lines []string, section, name string) []string {
	t.Helper()
	components := block(t, lines, 0, 0, "components")
	return block(t, block(t, components, 0, 2, section), 0, 4, name)
}

// declaresBoolean reports whether a schema block declares flag as a boolean
// property. An example's `flag: true` is not a declaration.
func declaresBoolean(lines []string, flag string) bool {
	for i, line := range lines {
		if strings.TrimSpace(line) == flag+":" && i+1 < len(lines) &&
			strings.TrimSpace(lines[i+1]) == "type: boolean" {
			return true
		}
	}
	return false
}

var schemaRef = regexp.MustCompile(`\$ref: '#/components/schemas/([A-Za-z0-9]+)'`)

// TestPurposes_MatchTheOpenAPIEnum: a client asks for a WebAuthn inline token
// by purpose, so the begin route's enum must be exactly the purposes the
// server accepts. A purpose missing from it cannot be requested by a
// generated or validating client; one extra would be refused.
func TestPurposes_MatchTheOpenAPIEnum(t *testing.T) {
	lines := openAPILines(t)
	route := block(t, lines, 0, 2, "/mfa/webauthn/verify-inline/begin")
	purpose := block(t, route, 0, 16, "purpose")
	enum := block(t, purpose, 0, 18, "enum")
	var got []string
	for _, line := range enum {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
			got = append(got, v)
		}
	}
	want := make([]string, 0, len(allPurposes))
	for _, p := range Purposes() {
		want = append(want, string(p))
	}
	assert.ElementsMatch(t, want, got)
}

// TestOwnRulePurposes_MatchTheMintEnum: the password step-up mint (#3509)
// accepts exactly the own-rule purposes, so its documented enum must be
// exactly that set: a missing value is a route whose password prompt a
// generated client cannot complete, an extra one is refused with a 400.
func TestOwnRulePurposes_MatchTheMintEnum(t *testing.T) {
	lines := openAPILines(t)
	route := block(t, lines, 0, 2, "/auth/step-up/password")
	require.Contains(t, strings.Join(route, "\n"), "#/components/schemas/PasswordStepUpTokenRequest")
	purpose := block(t, component(t, lines, "schemas", "PasswordStepUpTokenRequest"), 0, 8, "purpose")
	var got []string
	for _, line := range block(t, purpose, 0, 10, "enum") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
			got = append(got, v)
		}
	}
	want := make([]string, 0, len(ownRulePurposes))
	for _, p := range OwnRulePurposes() {
		want = append(want, string(p))
	}
	assert.ElementsMatch(t, want, got)
}

// schemaDeclaresBoolean reports whether the named schema, or one it
// references through allOf/oneOf/anyOf (to a bounded depth), declares flag.
func schemaDeclaresBoolean(t *testing.T, lines []string, name, flag string, depth int) bool {
	t.Helper()
	schema := component(t, lines, "schemas", name)
	if declaresBoolean(schema, flag) {
		return true
	}
	if depth == 0 {
		return false
	}
	for _, ref := range schemaRef.FindAllStringSubmatch(strings.Join(schema, "\n"), -1) {
		if schemaDeclaresBoolean(t, lines, ref[1], flag, depth-1) {
			return true
		}
	}
	return false
}

// TestRefusalFlags_DeclaredByTheResponseSchemas: each response a budget or
// lock-conflict body is sent under must reference a schema that declares the
// flag a client tells that body apart by. The flags are
// step_up_budget_exhausted and step_up_budget_unavailable (budget.go),
// lock_conflict (mfaenforce.WriteBusy), and step_up_token_invalid (the
// own-rule password arm, #3509), which a schema may declare through a schema
// it composes.
func TestRefusalFlags_DeclaredByTheResponseSchemas(t *testing.T) {
	lines := openAPILines(t)
	for response, flags := range map[string][]string{
		"StepUpAttemptsExhausted":   {"step_up_budget_exhausted"},
		"DeleteRateOrBudgetLimited": {"step_up_budget_exhausted"},
		"StepUpBudgetUnavailable":   {"step_up_budget_unavailable"},
		"DeleteSoftLockUnavailable": {"step_up_budget_unavailable", "lock_conflict"},
		"DeleteSoftLockForbidden":   {"step_up_token_invalid", "delete_rate_limited"},
	} {
		refs := schemaRef.FindAllStringSubmatch(strings.Join(component(t, lines, "responses", response), "\n"), -1)
		for _, flag := range flags {
			declared := false
			for _, ref := range refs {
				declared = declared || schemaDeclaresBoolean(t, lines, ref[1], flag, 2)
			}
			assert.Truef(t, declared, "%s must reference a schema declaring %s", response, flag)
		}
	}
}

// TestStepUpRequirements_SchemaMatchesTheHandler: GET /mfa/step-up's documented
// body must be exactly the fields requirementsResponse writes, with
// default_method nullable (an account with no inline factor gets null, which a
// validating client would otherwise reject) and no 404 documented, because a
// 404 on this path is how a client learns the server predates it.
func TestStepUpRequirements_SchemaMatchesTheHandler(t *testing.T) {
	lines := openAPILines(t)
	route := strings.Join(block(t, lines, 0, 2, "/mfa/step-up"), "\n")
	require.Contains(t, route, "#/components/schemas/StepUpRequirements")
	require.NotContains(t, route, "'404':")
	// The shared authentication chain answers these before the handler runs,
	// and the picker must tell a terminal refusal from a retryable outage.
	for _, status := range []string{"'400':", "'403':", "'503':"} {
		require.Contains(t, route, status, "the auth chain's %s must be documented", status)
	}
	require.Contains(t, route, "#/components/schemas/AccountDisabledError")

	schema := component(t, lines, "schemas", "StepUpRequirements")
	var documented []string
	for _, line := range block(t, schema, 0, 6, "properties") {
		if indentOf(line) == 8 {
			documented = append(documented, strings.TrimSuffix(strings.TrimSpace(line), ":"))
		}
	}
	var written []string
	rt := reflect.TypeOf(requirementsResponse{})
	for i := 0; i < rt.NumField(); i++ {
		written = append(written, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	assert.ElementsMatch(t, written, documented)

	var required []string
	for _, line := range block(t, schema, 0, 6, "required") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
			required = append(required, v)
		}
	}
	assert.ElementsMatch(t, written, required, "every field is always present")

	defaultMethod := strings.Join(block(t, schema, 0, 8, "default_method"), "\n")
	assert.Contains(t, defaultMethod, "nullable: true")
	assert.True(t, declaresBoolean(schema, "backup_code_available"))
}

// TestChallengeDefaultMethod_DocumentedOnEveryChallenge: the login, refresh
// and SSO MFA challenges carry the advisory default_method (MFA picker spec
// §2). Login and refresh share the MfaChallenge schema; the SSO session
// response declares its challenge variant inline, so it can drift on its own
// and is pinned separately (rev 3.3, C14).
func TestChallengeDefaultMethod_DocumentedOnEveryChallenge(t *testing.T) {
	lines := openAPILines(t)
	for _, route := range []string{"/auth/login", "/auth/refresh"} {
		assert.Contains(t, strings.Join(block(t, lines, 0, 2, route), "\n"),
			"#/components/schemas/MfaChallenge", "%s must document its MFA challenge", route)
	}
	// Refresh's 403 is also a disabled account (auth/handlers.go, both rotation
	// paths), which carries no challenge token; one schema for both would make
	// a validator reject that response.
	assert.Contains(t, strings.Join(block(t, lines, 0, 2, "/auth/refresh"), "\n"),
		"#/components/schemas/AccountDisabledError", "/auth/refresh's 403 must admit the disabled-account body")

	// Both declarations carry the exact enum the server can send: a member
	// missing from either makes a validating client reject a valid challenge.
	challenge := component(t, lines, "schemas", "MfaChallenge")
	assert.ElementsMatch(t, []string{MethodTOTP, MethodWebAuthn}, enumValues(block(t, challenge, 0, 8, "default_method")),
		"MfaChallenge default_method")

	sso := block(t, lines, 0, 2, "/auth/sso/{provider}/session")
	var ssoMethod []string
	for i, line := range sso {
		if strings.TrimSpace(line) == "default_method:" {
			ssoMethod = block(t, sso, i, indentOf(line), "default_method")
			break
		}
	}
	require.NotEmpty(t, ssoMethod, "the SSO session response's MFA challenge must declare default_method")
	assert.ElementsMatch(t, []string{MethodTOTP, MethodWebAuthn}, enumValues(ssoMethod), "SSO session default_method")
}

// enumValues returns the "- value" items of a property block: its enum.
func enumValues(property []string) []string {
	var values []string
	for _, line := range property {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
			values = append(values, v)
		}
	}
	return values
}
