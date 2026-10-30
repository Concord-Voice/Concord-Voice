package sessions

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These regressions spend canonical credentials through AuthRequired and real
// password verification. Only the handler's pool is hooked, so the dependency
// fault cannot be consumed by middleware's earlier admission transaction.
func TestFamilyErrorEnvelope_SessionRoutes(t *testing.T) {
	for _, route := range []string{"single", "all"} {
		for _, failure := range []string{"control", "ownership", "capture", "commit rollback", "commit acknowledged lost"} {
			if route == "all" && failure == "ownership" {
				continue
			}
			t.Run(route+"/"+failure, func(t *testing.T) {
				rig := newSecurityEventFamilyRig(t)
				hook, db := stmthook.Open(t)
				rig.handler.db = db
				path, method := "/api/v1/sessions/"+rig.sid, http.MethodDelete
				body := `{"password":"` + privacyPassword + `"}`
				if route == "all" {
					path, method = "/api/v1/sessions/revoke-all", http.MethodPost
					body = `{"password":"` + privacyPassword + `","include_current":true}`
					rig.router.POST(path, rig.handler.RevokeAllSessions)
				} else {
					rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
				}
				ctx := context.Background()
				require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{Score: float64(time.Now().Unix()), Member: "existing-revoke"}).Err())
				require.NoError(t, rig.redis.Set(ctx, simpleAuthWindowKey(rig.user), "fixture-window", 0).Err())
				before := familyEnvelopeSessionState(t, rig)
				reached := false
				switch failure {
				case "ownership":
					hook.Arm([]string{`SELECT EXISTS(SELECT 1 FROM login_family_sids WHERE sid = $1 AND user_id = $2)`}, func() error { reached = true; return nil }, stmthook.ErrInjected)
				case "capture":
					hook.Arm([]string{`SELECT f.family_id FROM login_families f`}, func() error { reached = true; return nil }, stmthook.ErrInjected)
				case "commit rollback", "commit acknowledged lost":
					// This statement only executes after authentication and complete family
					// capture. The commit fault affects only the handler pool, and the
					// hook report proves the revocation sealed its durable owner first.
					hook.Arm([]string{`UPDATE login_family_revocations SET sealed_at=clock_timestamp()`}, func() error { reached = true; return nil }, nil)
					hook.ArmCommit(failure == "commit acknowledged lost", stmthook.ErrInjected)
				}
				response := rig.request(method, path, body)
				if failure == "control" {
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					rig.requireCommittedOwner(t)
					require.Len(t, rig.recorder.events, 1)
					return
				}
				seen, callbackErr := hook.Report()
				require.Equal(t, 1, seen, "must reach the requested handler-side family fault")
				require.NoError(t, callbackErr)
				require.True(t, reached)
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				assert.Equal(t, `{"error":"Authentication temporarily unavailable"}`, response.Body.String())
				assert.Empty(t, response.Header().Values("Set-Cookie"))
				assert.Empty(t, rig.recorder.events, "unknown commit must not publish a success event")
				assert.Empty(t, rig.disconnector.users)
				assert.Empty(t, rig.disconnector.sessions)
				if failure == "commit acknowledged lost" {
					assert.NotEmpty(t, rig.disconnector.families, "a revocation committed behind a lost ACK must still fence its family")
				} else {
					assert.Empty(t, rig.disconnector.families, "an uncommitted revocation must not fence any family")
				}
				members, err := rig.redis.ZRange(ctx, revokeTrackingKey(rig.user), 0, -1).Result()
				require.NoError(t, err)
				assert.Equal(t, []string{"existing-revoke"}, members)
				window, err := rig.redis.Get(ctx, simpleAuthWindowKey(rig.user)).Result()
				require.NoError(t, err)
				assert.Equal(t, "fixture-window", window)
				if failure == "commit acknowledged lost" {
					rig.requireCommittedOwner(t) // physical commit happened despite the lost ACK
				} else {
					assert.True(t, reflect.DeepEqual(before, familyEnvelopeSessionState(t, rig)), "refusal must roll back durable mutation")
				}
			})
		}
	}
}

func familyEnvelopeSessionState(t *testing.T, rig securityEventFamilyRig) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for _, table := range []string{"users", "refresh_tokens", "login_families", "login_family_sids", "login_family_revocations", "login_family_revocation_targets"} {
		var snapshot string
		column := "user_id"
		if table == "users" {
			column = "id"
		}
		query := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text)::text,'[]') FROM %s s WHERE %s=$1`, table, column)
		require.NoError(t, rig.db.QueryRowContext(t.Context(), query, rig.user).Scan(&snapshot))
		state[table] = snapshot
	}
	return state
}
