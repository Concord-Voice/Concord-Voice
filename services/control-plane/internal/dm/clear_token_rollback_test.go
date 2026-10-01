package dm_test

// Both kinds of step-up token survive a rollback of the own-rule route that
// spent them (#3509, design spec "Developer decisions, 2026-10-01", T-4): DM
// Clear spends the token in its transaction, then fails writing its range,
// and the token must still be there for the retry. A WebAuthn token used to be
// spent by a Redis GETDEL that no rollback reached (Codex P2).

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// failClearRangeInsert makes the actor's next Clear fail AFTER its step-up,
// at the range insert, and returns the function that lifts the fault.
func failClearRangeInsert(t *testing.T, ts *testhelpers.TestServer, actorID string) func() {
	t.Helper()
	name := "test_clear_rollback_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err := ts.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected clear-range fault'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER %[1]s BEFORE INSERT ON dm_message_hidden_ranges
		FOR EACH ROW WHEN (NEW.user_id = '%[2]s') EXECUTE FUNCTION %[1]s()`, name, uuid.MustParse(actorID)))
	require.NoError(t, err)
	lift := func() {
		_, dropErr := ts.DB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON dm_message_hidden_ranges; DROP FUNCTION IF EXISTS %[1]s()`, name))
		require.NoError(t, dropErr)
	}
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON dm_message_hidden_ranges; DROP FUNCTION IF EXISTS %[1]s()`, name))
	})
	return lift
}

func tokenStored(t *testing.T, ts *testhelpers.TestServer, token string) bool {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM step_up_tokens WHERE token_hash = $1`, sum[:]).Scan(&n))
	return n == 1
}

// Mutant killed (either factor): spending the token outside the route's
// transaction — on the pool, or in Redis as WebAuthn tokens were — leaves the
// token gone after the rolled-back Clear.
func TestDMClear_StepUpTokenSurvivesARolledBackClear(t *testing.T) {
	for _, tc := range []struct {
		name   string
		factor stepup.TokenFactor
		field  string
	}{
		{"password token", stepup.FactorPassword, "step_up_token"},
		{"webauthn token", stepup.FactorWebAuthn, "mfa_code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			actor := ts.CreateTestUser(t, "clear_rb_"+string(tc.factor)[:4])
			peer := ts.CreateTestUser(t, "clear_rb_peer_"+string(tc.factor)[:4])
			ts.CreateFriendship(t, actor.ID, peer.ID, statusAccepted)
			convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
			if tc.factor == stepup.FactorWebAuthn {
				// A WebAuthn credential makes the account an MFA account (P1),
				// so Clear's own rule spends the inline token via VerifyCodeTx.
				_, err := ts.DB.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
					VALUES (gen_random_uuid(), $1, $2, 'Key', 'hardware', '\x00', 0, NOW())`, actor.ID, []byte("cred-"+actor.ID))
				require.NoError(t, err)
			}
			token, e := stepup.MintToken(context.Background(), ts.DB, actor.ID, tc.factor, stepup.PurposeDMClear, "")
			require.Nil(t, e)
			clearHistory := func() int {
				return ts.DoRequest("POST", pathDMConversationsPrefix+convID+"/clear",
					map[string]string{tc.field: token}, testhelpers.AuthHeaders(actor.AccessToken)).Code
			}

			lift := failClearRangeInsert(t, ts, actor.ID)
			require.Equal(t, http.StatusInternalServerError, clearHistory(), "precondition: the Clear fails after its step-up")
			assert.True(t, tokenStored(t, ts, token), "the rolled-back Clear must leave the token stored")

			lift()
			assert.Equal(t, http.StatusOK, clearHistory(), "the same token confirms the retry")
			assert.False(t, tokenStored(t, ts, token), "the committed Clear spent it")
		})
	}
}
