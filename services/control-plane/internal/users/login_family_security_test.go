package users_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func securityUser(t *testing.T, ts *testhelpers.TestServer, name string) testhelpers.TestUser {
	t.Helper()
	user := ts.CreateTestUser(t, name)
	minter := auth.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, nil)
	access, _, _, err := minter.IssueAccessAndRefresh(context.Background(), user.ID)
	require.NoError(t, err)
	user.AccessToken = access
	return user
}

func securityFamily(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) string {
	t.Helper()
	claims, err := auth.ValidateAccessToken(user.AccessToken, testhelpers.TestJWTSecret)
	require.NoError(t, err)
	var family string
	require.NoError(t, ts.DB.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1 AND user_id=$2`, claims.SessionID, user.ID).Scan(&family))
	return family
}

func TestLoginFamilySecurityCredentialWriters(t *testing.T) {
	for _, path := range []string{"/api/v1/users/me/password", "/api/v1/users/me/keys"} {
		for _, rollback := range []bool{false, true} {
			name := "commit"
			if rollback {
				name = "rollback"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				ts := setupTS(t)
				user := securityUser(t, ts, "familysecurity")
				family := securityFamily(t, ts, user)
				claims, err := auth.ValidateAccessToken(user.AccessToken, testhelpers.TestJWTSecret)
				require.NoError(t, err)
				var oldWrapped []byte
				require.NoError(t, ts.DB.QueryRow(`SELECT wrapped_private_key FROM user_keys WHERE user_id=$1`, user.ID).Scan(&oldWrapped))
				// Domain writes may start only after the old frontier is epoch-owned.
				_, err = ts.DB.Exec(`CREATE FUNCTION family_security_key_probe() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
     IF EXISTS(SELECT 1 FROM login_families WHERE user_id=NEW.user_id AND terminal_at IS NULL)
      OR NOT EXISTS(SELECT 1 FROM login_families f JOIN credential_epoch_voice_ejections e ON e.generation=f.epoch_generation AND e.user_id=f.user_id WHERE f.user_id=NEW.user_id)
     THEN RAISE EXCEPTION 'family capture missing before key write'; END IF;
     RETURN NEW; END $$;
     CREATE TRIGGER family_security_key_probe BEFORE UPDATE ON user_keys FOR EACH ROW EXECUTE FUNCTION family_security_key_probe()`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := ts.DB.Exec(`DROP TRIGGER IF EXISTS family_security_key_probe ON user_keys; DROP FUNCTION IF EXISTS family_security_key_probe()`)
					require.NoError(t, err)
				})
				if rollback {
					_, err = ts.DB.Exec(`ALTER TABLE user_keys ADD CONSTRAINT family_security_rollback CHECK(false) NOT VALID`)
					require.NoError(t, err)
					t.Cleanup(func() {
						_, err := ts.DB.Exec(`ALTER TABLE user_keys DROP CONSTRAINT family_security_rollback`)
						require.NoError(t, err)
					})
				}
				pub, wrapped, salt := testhelpers.E2EETestKeys()
				payload := map[string]interface{}{"current_password": user.Password, "wrapped_private_key": wrapped, "key_derivation_salt": salt, "key_derivation_alg": "argon2id"}
				method := http.MethodPost
				if path == "/api/v1/users/me/keys" {
					method = http.MethodPut
					payload["public_key"] = pub
					payload["acknowledge_data_loss"] = true
				} else {
					payload["new_password"] = "AnotherSecurePass456!" // pragma: allowlist secret
				}
				w := ts.DoRequest(method, path, payload, testhelpers.AuthHeaders(user.AccessToken))
				if rollback {
					require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
				} else {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				}
				var terminal bool
				var epochOwner, authOwner sql.NullString
				require.NoError(t, ts.DB.QueryRow(`SELECT terminal_at IS NOT NULL,epoch_generation,auth_operation_id FROM login_families WHERE family_id=$1`, family).Scan(&terminal, &epochOwner, &authOwner))
				require.Equal(t, !rollback, terminal)
				require.False(t, authOwner.Valid)
				if rollback {
					require.False(t, epochOwner.Valid)
					var actual []byte
					require.NoError(t, ts.DB.QueryRow(`SELECT wrapped_private_key FROM user_keys WHERE user_id=$1`, user.ID).Scan(&actual))
					require.Equal(t, oldWrapped, actual)
					w = ts.DoRequest(http.MethodGet, "/api/v1/users/me", nil, testhelpers.AuthHeaders(user.AccessToken))
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				} else {
					var exact bool
					require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM credential_epoch_voice_ejections e JOIN users u ON u.id=e.user_id WHERE e.generation=$1 AND e.user_id=$2 AND e.superseded_credential_epoch=$3 AND e.credential_epoch=u.credential_epoch)`, epochOwner.String, user.ID, claims.CredentialEpoch).Scan(&exact))
					require.True(t, exact)
					var body map[string]interface{}
					testhelpers.ParseJSON(t, w, &body)
					access, ok := body["access_token"].(string)
					require.True(t, ok)
					continuation, err := auth.ValidateAccessToken(access, testhelpers.TestJWTSecret)
					require.NoError(t, err)
					var continuationFamily string
					require.NoError(t, ts.DB.QueryRow(`SELECT f.family_id FROM login_family_sids s JOIN login_families f USING(family_id,user_id) WHERE s.sid=$1 AND f.terminal_at IS NULL`, continuation.SessionID).Scan(&continuationFamily))
					require.NotEqual(t, family, continuationFamily)
					replaySecurityEpochFrontier(t, ts, user.ID, claims.CredentialEpoch, epochOwner.String)
					w = ts.DoRequest(http.MethodGet, "/api/v1/users/me", nil, testhelpers.AuthHeaders(access))
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				}
			})
		}
	}
}

// This adapter observes the actual caller-owned SQL transaction and preserves
// GraphPresenceCapture.Complete as its terminal commit owner.
type familyErasureCapture struct {
	t                   *testing.T
	user                string
	outcome             string
	captures, completes int
}

func (c *familyErasureCapture) WithGatedTx(context.Context, presencecapture.Subject, func(*sql.Tx) error) error {
	return errors.New("unexpected independent erasure transaction")
}
func (c *familyErasureCapture) CaptureInTx(ctx context.Context, tx *sql.Tx, _ presencecapture.Subject) (presencecapture.Plan, error) {
	c.captures++
	var live int
	require.NoError(c.t, tx.QueryRowContext(ctx, `SELECT count(*) FROM login_families WHERE user_id=$1 AND terminal_at IS NULL`, c.user).Scan(&live))
	require.Zero(c.t, live, "capture must precede presence children")
	return nil, nil
}
func (c *familyErasureCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	c.completes++
	if c.outcome == "rollback" {
		return errors.New("family erasure rollback")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if c.outcome == "lost_ack" {
		return presencecapture.ErrCommitUnresolved
	}
	return nil
}
func (*familyErasureCapture) Abandon(presencecapture.Plan, presencecapture.Cause) {}

// erasureFamilyFence records the exact post-commit family fences of an erasure.
type erasureFamilyFence struct{ families []uuid.UUID }

func (f *erasureFamilyFence) DisconnectFamilyContext(_ context.Context, _, family uuid.UUID) error {
	f.families = append(f.families, family)
	return nil
}

func TestLoginFamilySecurityErasureEvidenceSurvivesUser(t *testing.T) {
	for _, outcome := range []string{"commit", "rollback", "lost_ack"} {
		t.Run(outcome, func(t *testing.T) {
			ts := setupTS(t)
			user := securityUser(t, ts, "familyerase")
			survivor := securityUser(t, ts, "familysurvivor")
			family := securityFamily(t, ts, user)
			other := securityFamily(t, ts, survivor)
			capture := &familyErasureCapture{t: t, user: user.ID, outcome: outcome}
			service := users.NewAccountService(ts.DB, logger.New("test"))
			service.SetGraphPresenceCapture(capture)
			fence := &erasureFamilyFence{}
			users.SetErasureFamilyCloserForTest(service, fence)
			err := service.DeleteAccount(testhelpers.ActorContextForDB(context.Background(), t, ts.DB, user.ID), user.ID)
			switch outcome {
			case "commit":
				require.NoError(t, err)
			case "lost_ack":
				require.ErrorIs(t, err, presencecapture.ErrCommitUnresolved)
			default:
				require.ErrorContains(t, err, "family erasure rollback")
			}
			require.Equal(t, 1, capture.captures)
			require.Equal(t, 1, capture.completes)
			if outcome == "rollback" {
				require.Empty(t, fence.families, "a rolled-back erasure fences nothing")
			} else {
				// A committed (or committed-but-unacknowledged) erasure fences the
				// erased user's sockets before returning; the survivor is untouched.
				var erased []uuid.UUID
				rows, err := ts.DB.Query(`SELECT family_id FROM login_families WHERE user_id=$1`, user.ID)
				require.NoError(t, err)
				for rows.Next() {
					var id uuid.UUID
					require.NoError(t, rows.Scan(&id))
					erased = append(erased, id)
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				require.Contains(t, erased, uuid.MustParse(family))
				require.ElementsMatch(t, erased, fence.families)
				require.NotContains(t, fence.families, uuid.MustParse(other))
			}
			var exists, terminal bool
			var operation sql.NullString
			require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, user.ID).Scan(&exists))
			require.Equal(t, outcome == "rollback", exists)
			require.NoError(t, ts.DB.QueryRow(`SELECT terminal_at IS NOT NULL,auth_operation_id FROM login_families WHERE family_id=$1`, family).Scan(&terminal, &operation))
			require.Equal(t, outcome != "rollback", terminal)
			var survivorLive bool
			require.NoError(t, ts.DB.QueryRow(`SELECT terminal_at IS NULL FROM login_families WHERE family_id=$1`, other).Scan(&survivorLive))
			require.True(t, survivorLive)
			if outcome != "rollback" {
				require.True(t, operation.Valid)
				var sealed bool
				require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM login_family_revocations o JOIN login_family_revocation_targets t USING(operation_id,user_id) WHERE o.operation_id=$1 AND o.scope='user' AND o.reason='erasure' AND o.sealed_at IS NOT NULL AND t.family_id=$2)`, operation.String, family).Scan(&sealed))
				require.True(t, sealed, "terminal evidence must survive DELETE users")
			}
		})
	}
}

// The actual handler's epoch owner replays an old frontier, without moving its
// terminal owner or catching families created after that frontier was captured.
func replaySecurityEpochFrontier(t *testing.T, ts *testhelpers.TestServer, userID, oldEpoch, generation string) {
	t.Helper()
	ctx := context.Background()
	uid := uuid.MustParse(userID)
	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	set, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{uid}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := set.ForUser(uid)
	require.NoError(t, err)
	targets, err := loginfamily.CaptureEpochTargetsTx(ctx, tx, user, oldEpoch)
	require.NoError(t, err)
	var current string
	require.NoError(t, tx.QueryRow(`SELECT credential_epoch FROM users WHERE id=$1`, uid).Scan(&current))
	owner := loginfamily.EpochOwner{UserID: uid, Generation: uuid.MustParse(generation), SupersededCredentialEpoch: oldEpoch, CredentialEpoch: current}
	_, err = loginfamily.CreateFamilyTx(ctx, tx, user, current)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		result, err := loginfamily.RevokeFamiliesForEpochTx(ctx, tx, targets, owner)
		require.NoError(t, err)
		require.Zero(t, result.NewlyTerminalFamilies)
	}
	var live int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM login_families WHERE user_id=$1 AND terminal_at IS NULL`, uid).Scan(&live))
	require.Equal(t, 2, live, "continuation and later family survive old-frontier replay")
	require.NoError(t, tx.Commit())
}

type familySecurityDrain struct{}

func (familySecurityDrain) DrainAlreadyGated(context.Context, *sql.Tx, uuid.UUID) ([]presence.Category, error) {
	return nil, nil
}
func (familySecurityDrain) ClearDrained(context.Context, uuid.UUID, []presence.Category) {}
func (familySecurityDrain) CapturePrivateCallPlansAlreadyGated(context.Context, *sql.Tx, []uuid.UUID) error {
	return nil
}
func (familySecurityDrain) CompletePrivateCallPlansAlreadyGated(context.Context, []uuid.UUID) error {
	return nil
}

func securityTryLock(t *testing.T, db *sql.DB, query string, id string, blocked bool) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(query, id)
	if blocked {
		var pgerr *pq.Error
		require.ErrorAs(t, err, &pgerr)
		require.Equal(t, "55P03", string(pgerr.Code))
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, tx.Rollback())
}
func securityWaitBlocker(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE $1=ANY(pg_blocking_pids(a.pid)) AND a.datname=current_database())`, pid).Scan(&blocked)
		require.NoError(t, err)
		return blocked
	}, 5*time.Second, 10*time.Millisecond, "must observe the actual PostgreSQL lock wait")
}

func TestLoginFamilySecurityErasureSortedUsersBeforeFamiliesAndConversations(t *testing.T) {
	ts := setupTS(t)
	subjects := []testhelpers.TestUser{securityUser(t, ts, "familylocka"), securityUser(t, ts, "familylockb"), securityUser(t, ts, "familylockc")}
	slices.SortFunc(subjects, func(a, b testhelpers.TestUser) int { return strings.Compare(a.ID, b.ID) })
	erased := subjects[0]
	last := subjects[2]
	family := securityFamily(t, ts, erased)
	conversation := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO dm_conversations(id,is_group,created_by) VALUES($1,true,$2)`, conversation, erased.ID)
	require.NoError(t, err)
	for _, subject := range subjects[1:] {
		_, err = ts.DB.Exec(`INSERT INTO dm_voice_participants(conversation_id,user_id) VALUES($1,$2)`, conversation, subject.ID)
		require.NoError(t, err)
	}
	handler := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	handler.SetPresenceHistory(ts.PresenceHistory)
	handler.SetActivitySettingsSuppressor(noopActivitySettingsSuppressor{})
	service := users.NewAccountService(ts.DB, logger.New("test"))
	service.SetActivitySettingsCleanupHandler(handler)
	service.SetActivePlanRail(familySecurityDrain{})
	service.SetGraphPresenceCapture(&familyErasureCapture{t: t, user: erased.ID, outcome: "commit"})
	userBlock, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { err := userBlock.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	_, err = userBlock.Exec(`SELECT id FROM users WHERE id=$1 FOR UPDATE`, last.ID)
	require.NoError(t, err)
	var userPID int
	require.NoError(t, userBlock.QueryRow(`SELECT pg_backend_pid()`).Scan(&userPID))
	familyBlock, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { err := familyBlock.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	_, err = familyBlock.Exec(`SELECT family_id FROM login_families WHERE family_id=$1 FOR UPDATE`, family)
	require.NoError(t, err)
	var familyPID int
	require.NoError(t, familyBlock.QueryRow(`SELECT pg_backend_pid()`).Scan(&familyPID))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	actor := testhelpers.WithAccessIdentity(ctx, t, erased)
	go func() { done <- service.DeleteAccount(actor, erased.ID) }()
	securityWaitBlocker(t, ts.DB, userPID)
	// KEY SHARE conflicts with UPDATE, while it permits NO KEY UPDATE.
	for _, subject := range subjects[:2] {
		securityTryLock(t, ts.DB, `SELECT id FROM users WHERE id=$1 FOR KEY SHARE NOWAIT`, subject.ID, true)
	}
	securityTryLock(t, ts.DB, `SELECT id FROM dm_conversations WHERE id=$1 FOR UPDATE NOWAIT`, conversation, false)
	require.NoError(t, userBlock.Rollback())
	securityWaitBlocker(t, ts.DB, familyPID)
	for _, subject := range subjects {
		securityTryLock(t, ts.DB, `SELECT id FROM users WHERE id=$1 FOR KEY SHARE NOWAIT`, subject.ID, true)
	}
	securityTryLock(t, ts.DB, `SELECT id FROM dm_conversations WHERE id=$1 FOR UPDATE NOWAIT`, conversation, false)
	require.NoError(t, familyBlock.Rollback())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("erasure failed after observed barriers released")
	}
	for _, survivor := range subjects[1:] {
		var exists bool
		require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, survivor.ID).Scan(&exists))
		require.True(t, exists)
	}
}

// A login revoked while its erasure request waited must not erase the account
// and its surviving logins: the erasure revalidates the requesting family
// under the users lock.
func TestLoginFamilySecurityRevokedRequesterCannotErase(t *testing.T) {
	ts := setupTS(t)
	stale := securityUser(t, ts, "familyerasestale")
	survivor := stale
	minter := auth.NewHandler(ts.DB, ts.Redis, logger.NewWithWriter(io.Discard), testhelpers.TestJWTSecret, nil)
	access, _, _, err := minter.IssueAccessAndRefresh(context.Background(), stale.ID)
	require.NoError(t, err)
	survivor.AccessToken = access
	survivorFamily := securityFamily(t, ts, survivor)

	identity := testhelpers.AccessIdentityForToken(t, stale.AccessToken)
	ctx := context.Background()
	revoke, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { err := revoke.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, revoke)
	require.NoError(t, err)
	locked, err := loginfamily.LockUsersTx(ctx, revoke, barrier, []uuid.UUID{identity.UserID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	authority, err := locked.ForUser(identity.UserID)
	require.NoError(t, err)
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, revoke, authority, identity.SID)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(ctx, revoke, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonLogout})
	require.NoError(t, err)
	require.NoError(t, revoke.Commit())

	service := users.NewAccountService(ts.DB, logger.New("test"))
	err = service.DeleteAccount(testhelpers.WithAccessIdentity(ctx, t, stale), stale.ID)

	require.ErrorIs(t, err, credepoch.ErrEpochMismatch, "the handler maps this to 401")
	require.ErrorIs(t, err, loginfamily.ErrDenied)
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, stale.ID).Scan(&exists))
	require.True(t, exists, "the account survives a revoked requester")
	var terminal bool
	require.NoError(t, ts.DB.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id=$1`, survivorFamily).Scan(&terminal))
	require.False(t, terminal, "the surviving login stays live")
}

func TestLoginFamilySecurityEraseRequiresRequesterIdentity(t *testing.T) {
	ts := setupTS(t)
	target := securityUser(t, ts, "familyerasetarget")
	other := securityUser(t, ts, "familyeraseother")
	service := users.NewAccountService(ts.DB, logger.New("test"))
	for name, ctx := range map[string]context.Context{
		"no identity":    context.Background(),
		"other identity": testhelpers.WithAccessIdentity(context.Background(), t, other),
	} {
		err := service.DeleteAccount(ctx, target.ID)
		require.ErrorIs(t, err, credepoch.ErrEpochMismatch, name)
		require.ErrorIs(t, err, loginfamily.ErrDenied, name)
	}
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, target.ID).Scan(&exists))
	require.True(t, exists, "the account survives a request without its own identity")
}
