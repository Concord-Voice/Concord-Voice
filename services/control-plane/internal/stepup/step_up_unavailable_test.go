package stepup

// Regression for review of #3509 (Codex P2, server half): an own-rule route's
// NoFactors 400 — an account with neither a password nor an inline MFA method
// — carried only its copy, so a client could not tell it from a malformed
// body's 400 and offered a password prompt that can never succeed. Every
// own-rule NoFactors 400 carries step_up_unavailable: true, at the route seam
// and at the password mint, and the spec declares it on each such route.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyOwnRuleTx_NoFactors_CarriesStepUpUnavailable: the route seam's
// NoFactors 400 keeps its copy and gains the discriminator, for every
// own-rule purpose.
func TestVerifyOwnRuleTx_NoFactors_CarriesStepUpUnavailable(t *testing.T) {
	for _, purpose := range OwnRulePurposes() {
		route := OwnRuleRoute{Purpose: purpose, Copy: OwnRuleCopy(purpose)}
		e := VerifyOwnRuleTx(context.Background(), nil, nil, "user", route, Input{StepUpToken: "token"}, Subject{})
		require.NotNil(t, e, "purpose %q", purpose)
		assert.Equal(t, http.StatusBadRequest, e.Status, "purpose %q", purpose)
		assert.Equal(t, OwnRuleCopy(purpose).NoFactors, e.Body["error"], "purpose %q: the copy is unchanged", purpose)
		assert.Equal(t, true, e.Body["step_up_unavailable"], "purpose %q", purpose)
	}
}

// TestMintPasswordToken_NoFactors_CarriesStepUpUnavailable: the mint answers
// the same account with the same body.
func TestMintPasswordToken_NoFactors_CarriesStepUpUnavailable(t *testing.T) {
	m := newMintHarness(t)
	_, err := m.db.Exec(`UPDATE users SET password_hash = '' WHERE id = $1`, m.userID)
	require.NoError(t, err)

	for _, purpose := range OwnRulePurposes() {
		w, body := m.mint(t, mintBody(mintFixturePassword, purpose))
		require.Equal(t, http.StatusBadRequest, w.Code, "purpose %q", purpose)
		assert.Equal(t, OwnRuleCopy(purpose).NoFactors, body["error"], "purpose %q: the copy is unchanged", purpose)
		assert.Equal(t, true, body["step_up_unavailable"], "purpose %q", purpose)
	}
}

// TestStepUpUnavailable_DeclaredByTheOwnRuleBadRequests: each route that can
// answer an own-rule NoFactors 400 must reference, under its 400, a schema
// declaring step_up_unavailable.
func TestStepUpUnavailable_DeclaredByTheOwnRuleBadRequests(t *testing.T) {
	lines := openAPILines(t)
	for _, op := range []struct{ path, method string }{
		{"/auth/step-up/password", "post"},
		{"/channels/{id}/messages", "delete"},
		{"/servers/{id}/messages", "delete"},
		{"/messages/{id}", "delete"},
		{"/dm/conversations/{id}/clear", "post"},
		{"/dm/conversations/{id}/messages/{message_id}", "delete"},
	} {
		operation := block(t, block(t, lines, 0, 2, op.path), 0, 4, op.method)
		badRequest := block(t, block(t, operation, 0, 6, "responses"), 0, 8, "'400'")
		declared := false
		for _, ref := range schemaRef.FindAllStringSubmatch(strings.Join(badRequest, "\n"), -1) {
			declared = declared || schemaDeclaresBoolean(t, lines, ref[1], "step_up_unavailable", 2)
		}
		assert.Truef(t, declared, "%s %s: its 400 must reference a schema declaring step_up_unavailable",
			strings.ToUpper(op.method), op.path)
	}
}
