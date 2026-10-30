package privacy_test

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Full-router erasure must map its newly introduced family SQL failures only
// after the actual bearer family admission has definitely committed.
func TestFamilyErrorEnvelope_ErasureFamilyDependency(t *testing.T) {
	for _, failure := range []string{"control", "barrier", "capture"} {
		t.Run(failure, func(t *testing.T) {
			hook, routerDB := stmthook.Open(t)
			ts := testhelpers.SetupTestServerWithRouterDB(t, routerDB)
			user := ts.CreateTestUser(t, "erasefamilyenvelope")
			foreign := ts.CreateTestUser(t, "erasefamilyforeign")
			claims := requireLiveErasureFamily(t, ts, user.AccessToken)
			before := erasureFailureState(t, ts, foreign.ID)
			principalBefore := familyEnvelopeErasurePrincipal(t, ts, user.ID)
			requireErasureTokensUnblacklisted(t, ts, user.AccessToken, foreign.AccessToken)
			admitted := false
			require.NoError(t, hook.ArmAdmission(user.ID, claims.SessionID, func() error { admitted = true; return nil }))
			if failure != "control" {
				fragment := `SELECT id FROM login_family_rollout WHERE id = TRUE FOR SHARE`
				if failure == "capture" {
					fragment = `SELECT f.family_id FROM login_families f`
				}
				hook.Arm([]string{fragment}, func() error {
					if !admitted {
						return fmt.Errorf("erasure family fault preceded definite bearer admission")
					}
					return nil
				}, stmthook.ErrInjected)
			}
			response := ts.DoRequest(http.MethodPost, "/api/v1/privacy/erase-account", map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
			admission := hook.AdmissionReport()
			require.Equal(t, 1, admission.Fired)
			require.True(t, admission.Completed)
			require.False(t, admission.Pending || admission.Running)
			require.NoError(t, admission.CallbackErr)
			if failure == "control" {
				require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
				var count int
				require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users WHERE id=$1`, user.ID).Scan(&count))
				require.Zero(t, count, "same real consumer must erase when dependency works")
				return
			}
			seen, callbackErr := hook.Report()
			require.Equal(t, 1, seen, "must reach the new erasure family SQL, beyond admission")
			require.NoError(t, callbackErr)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Equal(t, `{"error":"Authentication temporarily unavailable"}`, response.Body.String())
			assert.Empty(t, response.Header().Values("Set-Cookie"))
			assert.True(t, reflect.DeepEqual(before, erasureFailureState(t, ts, foreign.ID)), "family/audit/foreign state must survive failed erasure")
			assert.True(t, reflect.DeepEqual(principalBefore, familyEnvelopeErasurePrincipal(t, ts, user.ID)), "principal and refresh credentials must survive")
			requireErasureTokensUnblacklisted(t, ts, user.AccessToken, foreign.AccessToken)
			disabled, err := ts.Redis.Exists(t.Context(), "user_disabled:"+user.ID).Result()
			require.NoError(t, err)
			assert.Zero(t, disabled, "precommit failure must not run erasure deauthorization")
		})
	}
}

func familyEnvelopeErasurePrincipal(t *testing.T, ts *testhelpers.TestServer, userID string) []string {
	t.Helper()
	var principal, refresh string
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT to_jsonb(u)::text FROM users u WHERE id=$1`, userID).Scan(&principal))
	require.NoError(t, ts.DB.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id)::text,'[]') FROM refresh_tokens r WHERE user_id=$1`, userID).Scan(&refresh))
	return []string{principal, refresh}
}
