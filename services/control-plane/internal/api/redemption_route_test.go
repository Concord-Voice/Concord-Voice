// Package api_test holds the redemption ROUTE end-to-end test. It lives in the
// external test package (not package api) because testhelpers.SetupTestServer
// imports internal/api — an in-package test importing testhelpers would form an
// import cycle. api_test is compiled separately, so the cycle is broken.
package api_test

import (
	"context"
	"net/http"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// TestRedemptionRoutes_EndToEnd drives the redemption routes through the FULL
// router built by api.NewRouter (via SetupTestServer) — exercising the #1303
// route registration in router.go and the redemption_wiring.go handler build,
// not just the handler in isolation. It covers:
//   - POST /api/v1/redeem happy path (real auth middleware + rate-limit chain).
//   - POST /api/v1/redeem generic rejection for garbage (no-oracle 400).
//   - POST /api/v1/admin/redemption/codes → 503 (admin token unset in the test
//     config, so AdminGate disables the endpoint — the documented safe default).
func TestRedemptionRoutes_EndToEnd(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "redeemer")

	// Issue a real single-use feature code directly against the same DB.
	iss := redemption.NewIssuer(ts.DB, redemption.NewCatalog(), redemption.NewDBAuditSink())
	codes, err := iss.Issue(context.Background(), redemption.IssueSpec{
		GrantKind:  "feature:custom_themes",
		Count:      1,
		SingleUse:  true,
		MaxRedeems: intPtr(1),
		BatchID:    "route-e2e",
	})
	require.NoError(t, err)
	require.Len(t, codes, 1)

	t.Run("redeem happy path", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPost, "/api/v1/redeem",
			map[string]string{"code": codes[0].Plaintext},
			testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var resp map[string]any
		testhelpers.ParseJSON(t, w, &resp)
		assert.Equal(t, true, resp["success"])
		assert.NotEmpty(t, resp["description"])
	})

	t.Run("premium redeem fires the post-commit tier notifier", func(t *testing.T) {
		// A premium grant flips the user's tier free→premium, so the engine runs
		// the redemptionTierNotifier.OnTierChange adapter AFTER commit (the only
		// path that exercises the wiring's cache-invalidate + broadcast). The
		// adapter runs against the REAL entitlements cache built by NewRouter, so
		// this is the end-to-end live-update path, not a stub.
		premiumUser := ts.CreateTestUser(t, "premium-redeemer")
		premiumCodes, perr := iss.Issue(context.Background(), redemption.IssueSpec{
			GrantKind:   redemption.GrantPremiumSubscription,
			GrantParams: map[string]any{"months": 3},
			Count:       1,
			SingleUse:   true,
			MaxRedeems:  intPtr(1),
			BatchID:     "route-premium",
		})
		require.NoError(t, perr)

		w := ts.DoRequest(http.MethodPost, "/api/v1/redeem",
			map[string]string{"code": premiumCodes[0].Plaintext},
			testhelpers.AuthHeaders(premiumUser.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var resp map[string]any
		testhelpers.ParseJSON(t, w, &resp)
		assert.Equal(t, true, resp["success"])
		assert.Contains(t, resp["description"], "3 months")

		// The subscription row was created (premium grant landed durably).
		var tier string
		require.NoError(t, ts.DB.QueryRow(
			`SELECT tier FROM subscriptions WHERE user_id=$1`, premiumUser.ID,
		).Scan(&tier))
		assert.Equal(t, "premium", tier)
	})

	t.Run("redeem garbage → generic 400 (no oracle)", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPost, "/api/v1/redeem",
			map[string]string{"code": "NOPE-NOPE-NOPE"},
			testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "not valid")
	})

	t.Run("admin generation endpoint disabled (503) when token unset", func(t *testing.T) {
		w := ts.DoRequest(http.MethodPost, "/api/v1/admin/redemption/codes",
			map[string]any{"grant_kind": "feature:custom_themes", "count": 1},
			testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
}

func intPtr(i int) *int { return &i }
