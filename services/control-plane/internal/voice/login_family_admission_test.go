package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	rawnats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// Removing the final family guard makes the committed-revoke arm issue a final
// revision. Keeping PREPARE inside SQL makes the independent revoke time out.
func TestLoginFamilyA2FinalAuthorityAfterPrepare(t *testing.T) {
	for _, mode := range []string{"revoke_committed", "revoke_rolled_back", "expires_during_final_reservation_wait"} {
		rollback := mode == "revoke_rolled_back"
		expires := mode == "expires_during_final_reservation_wait"
		t.Run(mode, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			t.Cleanup(func() {
				_, err := dbtest.ExecReplica(db, `TRUNCATE login_family_revocation_targets,login_family_sids,login_families,login_family_revocations,login_family_rollout CASCADE`)
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(true)`)
				require.NoError(t, err)
			})
			_, err := db.Exec(`UPDATE login_family_rollout SET activated_at=clock_timestamp(),receipt='{}'::jsonb WHERE id`)
			require.NoError(t, err)
			rc := redistest.Client(t)
			owner, member := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
			server, channel, role := uuid.New(), uuid.New(), uuid.New()
			require.NoError(t, insertPendingAdmissionJoinFixture(db, owner, member, server, channel, role))
			secret := uuid.NewString()
			mintContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			mintContext.Request = httptest.NewRequest(http.MethodPost, "/mint", nil)
			token, _, sid, err := auth.NewHandler(db, rc, logger.New("test"), secret, nil).IssueTokenPair(mintContext, member.String(), "")
			require.NoError(t, err)
			var expiry time.Time
			if expires {
				// Production preparation/signing and family binding share the
				// exact second-precision deadline. No forged carrier or JWT.
				sid = uuid.NewString()
				preparedToken, prepErr := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: member, SID: uuid.MustParse(sid), EmailVerified: true}, time.Now().Add(-auth.AccessTokenTTL+8*time.Second))
				require.NoError(t, prepErr)
				expiry = preparedToken.ExpiresAt()
				mintTx, mintErr := db.Begin()
				require.NoError(t, mintErr)
				defer rollbackFamilyVoiceTx(t, mintTx)
				b, e := lf.AcquireWriterBarrierTx(context.Background(), mintTx)
				require.NoError(t, e)
				set, e := lf.LockUsersTx(context.Background(), mintTx, b, []uuid.UUID{member}, lf.UserNoKeyUpdate)
				require.NoError(t, e)
				user, e := set.ForUser(member)
				require.NoError(t, e)
				family, e := lf.CreateFamilyTx(context.Background(), mintTx, user, "")
				require.NoError(t, e)
				require.NoError(t, lf.BindSIDTx(context.Background(), mintTx, family, uuid.MustParse(sid), expiry))
				require.NoError(t, mintTx.Commit())
				token, e = preparedToken.Sign(secret)
				require.NoError(t, e)
			}
			admission := uuid.NewString()
			_, err = db.Exec(`INSERT INTO voice_pending_admissions(channel_id,user_id,admission_id,socket_id,expires_at) VALUES($1,$2,$3,'family-socket',clock_timestamp()+INTERVAL '30 seconds')`, channel, member, admission)
			require.NoError(t, err)
			bus, err := rawnats.Connect(admissionNATSTestURL())
			require.NoError(t, err, "A2 requires real NATS; configure NATS_URL")
			t.Cleanup(bus.Close)
			control, err := natsclient.Connect(admissionNATSTestURL())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, control.Close()) })
			prepared := make(chan *rawnats.Msg, 1)
			sub, err := bus.Subscribe(voiceAdmissionActivationSubject, func(msg *rawnats.Msg) { prepared <- msg })
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })
			require.NoError(t, bus.Flush())
			h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test"), Cfg: &config.Config{JWTSecret: secret}, NATS: control, Resolver: rbac.NewResolver(db, rbac.NewPermissionCache(rc), logger.New("test")), EntCache: entitlements.NewCache(rc, db)})
			r := gin.New()
			r.Use(middleware.AuthRequired(secret, rc, db, credepoch.New(db, rc, logger.New("test"))))
			r.POST("/channels/:id/voice/join", func(c *gin.Context) { c.Set("concord_service_hop", true); h.AuthorizeJoin(c) })
			req := httptest.NewRequest(http.MethodPost, "/channels/"+channel.String()+"/voice/join", bytes.NewBufferString(fmt.Sprintf(`{"admission_id":%q,"socket_id":"family-socket","activate":true}`, admission)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); r.ServeHTTP(w, req) }()
			var msg *rawnats.Msg
			select {
			case msg = <-prepared:
			case <-done:
				t.Fatalf("request ended before PREPARE: %d %s", w.Code, w.Body.String())
			case <-time.After(5 * time.Second):
				t.Fatal("no PREPARE")
			}
			var reservation *sql.Tx
			var blockerPID int
			if expires {
				reservation, err = db.Begin()
				require.NoError(t, err)
				defer rollbackFamilyVoiceTx(t, reservation)
				require.NoError(t, reservation.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
				require.NoError(t, reservation.QueryRow(`SELECT admission_id FROM voice_pending_admissions WHERE admission_id=$1 FOR UPDATE`, admission).Scan(new(uuid.UUID)))
			} else {
				// Acquire all initial authority independently while the remote acknowledgement
				// is withheld. SQL authority must be released throughout the network wait.
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer rollbackFamilyVoiceTx(t, tx)
				barrier, err := lf.AcquireWriterBarrierTx(ctx, tx)
				require.NoError(t, err)
				users, err := lf.LockUsersTx(ctx, tx, barrier, []uuid.UUID{member}, lf.UserNoKeyUpdate)
				require.NoError(t, err)
				user, err := users.ForUser(member)
				require.NoError(t, err)
				targets, err := lf.ResolveTargetSIDTx(ctx, tx, user, uuid.MustParse(sid))
				require.NoError(t, err)
				_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke})
				require.NoError(t, err)
				require.NoError(t, tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id=$1 FOR UPDATE NOWAIT`, server).Scan(new(uuid.UUID)))
				if rollback {
					require.NoError(t, tx.Rollback())
				} else {
					require.NoError(t, tx.Commit())
				}
			}
			var response voiceAdmissionActivationResponse
			require.NoError(t, json.Unmarshal(msg.Data, &response))
			response.Result = "ok"
			response.Proof = mediaproof.Sign(mediaproof.DeriveKey(secret, voiceAdmissionActivationResponsePurpose), voiceAdmissionActivationVersion, response.Timestamp, "activate", response.ChannelID, response.UserID, response.AdmissionID, response.SocketID, response.Revision, response.Nonce, response.Result)
			payload, err := json.Marshal(response)
			require.NoError(t, err)
			require.NoError(t, msg.Respond(payload))
			if expires {
				// Observe the exact final SQL waiter, rather than assuming a
				// scheduling delay crossed the guard. The initial tx already
				// committed to send PREPARE before we locked this row.
				require.Eventually(t, func() bool {
					var waiting bool
					e := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE $1=ANY(pg_blocking_pids(a.pid)) AND a.wait_event_type='Lock' AND a.query LIKE '%voice_pending_admissions%')`, blockerPID).Scan(&waiting)
					return e == nil && waiting
				}, 3*time.Second, 10*time.Millisecond, "final reservation SELECT must really wait")
				require.Eventually(t, func() bool {
					var expired bool
					e := db.QueryRow(`SELECT clock_timestamp()>$1::timestamptz`, expiry).Scan(&expired)
					return e == nil && expired
				}, 10*time.Second, 20*time.Millisecond)
				require.NoError(t, reservation.Rollback())
			}

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("A2 did not finish")
			}
			if rollback {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"allowed":true`)
			} else {
				require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
				require.NotContains(t, w.Body.String(), `"allowed":true`)
			}
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_pending_admissions WHERE admission_id=$1`, admission).Scan(&count))
			require.Equal(t, 1, count, "initial reservation remains committed; revoked A2 cannot erase unrelated state")
		})
	}
}

func rollbackFamilyVoiceTx(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		require.NoError(t, err)
	}
}

// The registry must reuse the exact user capability established before channel
// locks. Returning a snapshot or reconstructing authority after that prefix
// would fail these same-transaction and single-constructor checks.
func TestLoginFamilyChannelRegistryRetainsOriginalAuthority(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("resource_denied_%v", denied), func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			t.Cleanup(func() {
				_, err := dbtest.ExecReplica(db, `TRUNCATE login_family_revocation_targets,login_family_sids,login_families,login_family_revocations,login_family_rollout CASCADE`)
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(true)`)
				require.NoError(t, err)
			})
			_, err := db.Exec(`UPDATE login_family_rollout SET activated_at=clock_timestamp(),receipt='{}'::jsonb WHERE id`)
			require.NoError(t, err)
			rc := redistest.Client(t)
			owner, member := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
			server, channel, role := uuid.New(), uuid.New(), uuid.New()
			require.NoError(t, insertPendingAdmissionJoinFixture(db, owner, member, server, channel, role))
			secret := uuid.NewString()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/mint", nil)
			token, _, _, err := auth.NewHandler(db, rc, logger.New("test"), secret, nil).IssueTokenPair(c, member.String(), "")
			require.NoError(t, err)
			claims, err := auth.ValidateAccessToken(token, secret)
			require.NoError(t, err)
			identity := lf.AccessIdentity{UserID: member, SID: uuid.MustParse(claims.SessionID), CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time}
			if denied {
				channel = uuid.New()
			}
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer rollbackFamilyVoiceTx(t, tx)
			user, status, err := CheckChannelVoiceFamilyAuthorityTx(ctx, tx, rbac.NewResolver(db, rbac.NewPermissionCache(rc), logger.New("test")), server.String(), channel.String(), identity)
			if denied {
				require.Error(t, err)
				require.Equal(t, http.StatusForbidden, status)
				_, guardErr := lf.GuardAfterUserLockTx(ctx, tx, user, identity, lf.FamilyShare)
				require.ErrorIs(t, guardErr, lf.ErrContract, "failed channel authority cannot leak the earlier family capability")
				return
			}
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, status)
			_, err = lf.GuardAfterUserLockTx(ctx, tx, user, identity, lf.FamilyShare)
			require.NoError(t, err, "original authority remains usable after channel locks")
			otherTx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer rollbackFamilyVoiceTx(t, otherTx)
			_, err = lf.GuardAfterUserLockTx(ctx, otherTx, user, identity, lf.FamilyShare)
			require.ErrorIs(t, err, lf.ErrContract, "returned capability cannot authorize another transaction")
			require.NoError(t, otherTx.Rollback())
			_, err = lf.AcquireWriterBarrierTx(ctx, tx)
			require.ErrorIs(t, err, lf.ErrContract, "registry reuses authority; a second constructor is refused")
			_, err = lf.GuardAfterUserLockTx(ctx, tx, user, identity, lf.FamilyShare)
			require.NoError(t, err, "refused second constructor does not replace the original capability")
			require.NoError(t, tx.Commit())
		})
	}
}
