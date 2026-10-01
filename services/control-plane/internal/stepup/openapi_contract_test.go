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
