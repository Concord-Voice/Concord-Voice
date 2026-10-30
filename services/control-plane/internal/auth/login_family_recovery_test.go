package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

// Exercise both public recovery handlers with real recovery-factor verification,
// family-aware signed access and the presence service that owns their commit.
func TestLoginFamilyRecoveryEpochOwnership(t *testing.T) {
	for _, path := range []string{pathRecoveryResetPwd, pathRecoveryResetAcct} {
		for _, outcome := range []string{"commit", "rollback", "lost_ack"} {
			t.Run(path+"/"+outcome, func(t *testing.T) {
				ts := setupTS(t)
				user := ts.CreateTestUser(t, "familyrecovery")
				minter := auth.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, nil)
				var err error
				user.AccessToken, _, _, err = minter.IssueAccessAndRefresh(context.Background(), user.ID)
				require.NoError(t, err)
				claims, err := auth.ValidateAccessToken(user.AccessToken, testhelpers.TestJWTSecret)
				require.NoError(t, err)
				var family string
				require.NoError(t, ts.DB.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1 AND user_id=$2`, claims.SessionID, user.ID).Scan(&family))
				var oldWrapped []byte
				require.NoError(t, ts.DB.QueryRow(`SELECT wrapped_private_key FROM user_keys WHERE user_id=$1`, user.ID).Scan(&oldWrapped))
				// A SQL observer at the actual forced-clear boundary rejects late capture.
				_, err = ts.DB.Exec(`CREATE FUNCTION family_recovery_order_probe() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
      IF EXISTS (SELECT 1 FROM login_families WHERE user_id=NEW.user_id AND terminal_at IS NULL)
       OR NOT EXISTS (SELECT 1 FROM login_families f JOIN credential_epoch_voice_ejections e ON e.generation=f.epoch_generation AND e.user_id=f.user_id WHERE f.user_id=NEW.user_id)
      THEN RAISE EXCEPTION 'family capture missing before forced clear'; END IF; RETURN NEW; END $$;
      CREATE TRIGGER family_recovery_order_probe BEFORE INSERT OR UPDATE ON presence_settings_pending_operations FOR EACH ROW EXECUTE FUNCTION family_recovery_order_probe()`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := ts.DB.Exec(`DROP TRIGGER IF EXISTS family_recovery_order_probe ON presence_settings_pending_operations; DROP FUNCTION IF EXISTS family_recovery_order_probe()`)
					require.NoError(t, err)
				})
				token := recoveryTokenFor(t, ts, user)
				pub, wrapped, salt := testhelpers.E2EETestKeys()
				payload := map[string]interface{}{"recovery_token": token, "new_password": credEpochNewPass, "wrapped_private_key": wrapped, "key_derivation_salt": salt, "key_derivation_alg": "argon2id"}
				if path == pathRecoveryResetAcct {
					payload["public_key"] = pub
					payload["acknowledge_data_loss"] = true
				}
				commits := 0
				restore := ts.PresenceHistory.SetTransactionTestHooks(presencehistory.TransactionTestHooks{Commit: func(tx *sql.Tx) error {
					commits++
					if commits == 1 && outcome != "commit" {
						if outcome == "lost_ack" {
							require.NoError(t, tx.Commit())
						}
						return errors.New("family recovery commit acknowledgement unavailable")
					}
					return tx.Commit()
				}})
				defer restore()
				w := ts.DoRequest(http.MethodPost, path, payload, nil)
				if outcome == "commit" {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				} else {
					require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
					require.NotContains(t, w.Body.String(), `"access_token"`)
				}
				require.Positive(t, commits, "valid fixture must reach the real presence-owned commit")
				var terminal bool
				var epochOwner, authOwner sql.NullString
				require.NoError(t, ts.DB.QueryRow(`SELECT terminal_at IS NOT NULL,epoch_generation,auth_operation_id FROM login_families WHERE family_id=$1`, family).Scan(&terminal, &epochOwner, &authOwner))
				require.Equal(t, outcome != "rollback", terminal)
				require.False(t, authOwner.Valid, "epoch flow must never create a duplicate auth owner")
				var duplicate int
				require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, user.ID).Scan(&duplicate))
				require.Zero(t, duplicate)
				if outcome == "rollback" {
					require.False(t, epochOwner.Valid)
					var actual []byte
					require.NoError(t, ts.DB.QueryRow(`SELECT wrapped_private_key FROM user_keys WHERE user_id=$1`, user.ID).Scan(&actual))
					require.Equal(t, oldWrapped, actual)
					assertTokenStillLive := ts.DoRequest(http.MethodGet, pathUsersMeAuth, nil, testhelpers.AuthHeaders(user.AccessToken))
					require.Equal(t, http.StatusOK, assertTokenStillLive.Code, assertTokenStillLive.Body.String())
				} else {
					require.True(t, epochOwner.Valid)
					var exact bool
					require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM credential_epoch_voice_ejections e JOIN users u ON u.id=e.user_id WHERE e.generation=$1 AND e.user_id=$2 AND e.superseded_credential_epoch=$3 AND e.credential_epoch=u.credential_epoch)`, epochOwner.String, user.ID, claims.CredentialEpoch).Scan(&exact))
					require.True(t, exact)
					marker, err := ts.Redis.Get(context.Background(), credepoch.Key(user.ID)).Result()
					require.NoError(t, err)
					if outcome == "lost_ack" {
						require.True(t, strings.HasPrefix(marker, "blocked:"), marker)
					} else {
						require.True(t, strings.HasPrefix(marker, "active:"), marker)
					}
					assertTokenDead(t, ts, user.AccessToken)
				}
			})
		}
	}
}
