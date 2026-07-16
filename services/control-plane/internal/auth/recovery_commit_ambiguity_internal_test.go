package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errForcedAmbiguousRecoveryCommit = errors.New("forced ambiguous recovery commit")

type recoveryForcedEventRecorder struct {
	events []string
	plans  []presencehistory.DeliveryPlan
}

func (h *recoveryForcedEventRecorder) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	h.plans = append(h.plans, plan)
	h.events = append(h.events, "delivery_ack")
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func (h *recoveryForcedEventRecorder) DisconnectUser(uuid.UUID) {
	h.events = append(h.events, "disconnect")
}

type ambiguousRecoveryMFAChecker struct {
	claims *RecoveryClaims
}

func (*ambiguousRecoveryMFAChecker) IsEnabled(context.Context, string) bool { return false }

func (*ambiguousRecoveryMFAChecker) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (*ambiguousRecoveryMFAChecker) GetLoginMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (*ambiguousRecoveryMFAChecker) GenerateLoginChallenge(context.Context, string, bool) (string, string, error) {
	return "", "", nil
}

func (*ambiguousRecoveryMFAChecker) GenerateUpgradeChallenge(context.Context, string, bool) (string, string, error) {
	return "", "", nil
}

func (*ambiguousRecoveryMFAChecker) BeginWebAuthnLogin(context.Context, string, string) (interface{}, error) {
	return nil, nil
}

func (*ambiguousRecoveryMFAChecker) GenerateRecoveryToken(string) (string, string, error) {
	return "", "", nil
}

func (c *ambiguousRecoveryMFAChecker) ValidateRecoveryToken(string) (*RecoveryClaims, error) {
	return c.claims, nil
}

type recoveryCommitDelivery struct {
	plans []presencehistory.DeliveryPlan
	err   error
}

func (d *recoveryCommitDelivery) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	d.plans = append(d.plans, plan)
	if d.err != nil {
		return presencehistory.DeliveryAck{}, d.err
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

type recoveryForcedFlow struct {
	name    string
	path    string
	body    string
	handler func(*Handler, *gin.Context)
}

func recoveryForcedFlows() []recoveryForcedFlow {
	return []recoveryForcedFlow{
		{
			name: "password recovery",
			path: "/api/v1/auth/recovery/reset-password",
			body: `{
				"recovery_token":"token",
				"new_password":"NewSecurePassword123!",
				"wrapped_private_key":"a2V5",
				"key_derivation_salt":"c2FsdA==",
				"key_derivation_alg":"argon2id",
				"user_id":"00000000-0000-0000-0000-000000000001"
			}`,
			handler: func(h *Handler, c *gin.Context) { h.RecoveryResetPassword(c) },
		},
		{
			name: "account recovery",
			path: "/api/v1/auth/recovery/reset-account",
			body: `{
				"recovery_token":"token",
				"new_password":"NewSecurePassword123!",
				"wrapped_private_key":"a2V5",
				"key_derivation_salt":"c2FsdA==",
				"key_derivation_alg":"argon2id",
				"public_key":"cHVibGlj",
				"acknowledge_data_loss":true,
				"user_id":"00000000-0000-0000-0000-000000000001"
			}`,
			handler: func(h *Handler, c *gin.Context) { h.RecoveryResetAccount(c) },
		},
	}
}

func TestRecoveryCommitAmbiguityConsumesTokenAndAcknowledgesForcedClear(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, test := range recoveryForcedFlows() {
		t.Run(test.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			senderID := dbtest.CreateUser(t, db)
			seedRecoveryCommitKeyRows(t, db, senderID)
			rdb := setupAuthAttemptRedis(t)
			claims := &RecoveryClaims{UserID: senderID.String(), JTI: uuid.NewString()}
			observer := &recoveryForcedEventRecorder{}
			service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
			require.NoError(t, service.BindDelivery(observer))
			commitCalls := 0
			restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
				Commit: func(tx *sql.Tx) error {
					commitCalls++
					if commitCalls == 1 {
						require.NoError(t, tx.Commit())
						return errForcedAmbiguousRecoveryCommit
					}
					return tx.Commit()
				},
			})
			t.Cleanup(restore)
			handler := &Handler{
				db:              db,
				redis:           rdb,
				log:             logger.NewWithWriter(io.Discard),
				hub:             observer,
				mfaChecker:      &ambiguousRecoveryMFAChecker{claims: claims},
				presenceHistory: service,
			}

			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			c.Request.Header.Set("Content-Type", "application/json")
			test.handler(handler, c)

			assert.Equal(t, http.StatusInternalServerError, response.Code)
			exists, err := rdb.Exists(
				context.Background(), "recovery_token_used:"+claims.JTI,
			).Result()
			require.NoError(t, err)
			assert.Equal(t, int64(1), exists)
			require.Len(t, observer.plans, 1)
			assert.Equal(t, presencehistory.DeliveryConservativeReset, observer.plans[0].Mode)
			assert.Equal(t, senderID, observer.plans[0].SenderID)
			assert.Equal(t, []string{"delivery_ack", "disconnect"}, observer.events)
			var pending int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
			).Scan(&pending))
			assert.Zero(t, pending, "read-back-confirmed acknowledgement must delete the marker")
		})
	}
}

func TestRecoveryCommitAmbiguityClassifiesOutcomesAndConsumesToken(t *testing.T) {
	commitCause := errors.New("recovery commit classification ambiguity")
	tests := []struct {
		name       string
		outcome    presencehistory.ForcedClearOutcome
		deliveries int
		disconnect bool
	}{
		{name: "rollback", outcome: presencehistory.ForcedClearRolledBack},
		{name: "superseded", outcome: presencehistory.ForcedClearSuperseded, disconnect: true},
		{name: "unresolved", outcome: presencehistory.ForcedClearUnresolved, deliveries: 1, disconnect: true},
	}

	for _, flow := range recoveryForcedFlows() {
		for _, test := range tests {
			t.Run(flow.name+"/"+test.name, func(t *testing.T) {
				db, _ := dbtest.SetupTestDB(t)
				senderID := dbtest.CreateUser(t, db)
				seedRecoveryCommitKeyRows(t, db, senderID)
				seedRecoveryForcedPresence(t, db, senderID)
				rdb := setupAuthAttemptRedis(t)
				claims := &RecoveryClaims{UserID: senderID.String(), JTI: uuid.NewString()}
				delivery := &recoveryCommitDelivery{}
				hub := &recoveryForcedEventRecorder{}

				serviceDB := db
				if test.outcome == presencehistory.ForcedClearUnresolved {
					var err error
					serviceDB, err = sql.Open("postgres", dbtest.DatabaseURL())
					require.NoError(t, err)
					require.NoError(t, serviceDB.Ping())
					t.Cleanup(func() { _ = serviceDB.Close() })
				}
				service := newRecoveryForcedService(t, serviceDB, delivery)
				laterService := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
				var laterOperationID uuid.UUID
				restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
					Commit: func(tx *sql.Tx) error {
						switch test.outcome {
						case presencehistory.ForcedClearRolledBack:
							return commitCause
						case presencehistory.ForcedClearSuperseded:
							require.NoError(t, tx.Commit())
							laterTx, err := laterService.BeginTx(context.Background(), nil)
							require.NoError(t, err)
							later, err := laterService.BeginAudienceOperation(
								context.Background(), laterTx, senderID, presencehistory.ForcedSecurityClear,
							)
							require.NoError(t, err)
							laterOperationID = later.ID
							require.NoError(t, laterService.CommitTx(laterTx))
							return commitCause
						case presencehistory.ForcedClearUnresolved:
							require.NoError(t, tx.Commit())
							require.NoError(t, serviceDB.Close())
							return commitCause
						default:
							return errors.New("unexpected recovery outcome")
						}
					},
				})
				t.Cleanup(restore)
				handler := newRecoveryForcedHandler(db, rdb, hub, service, claims)
				var logs bytes.Buffer
				handler.log = logger.NewWithWriter(&logs)

				response := invokeRecoveryForcedFlow(flow, handler)

				assert.Equal(t, http.StatusInternalServerError, response.Code)
				assert.Contains(t, logs.String(), recoveryForcedClearClass(test.outcome))
				exists, err := rdb.Exists(
					context.Background(), "recovery_token_used:"+claims.JTI,
				).Result()
				require.NoError(t, err)
				assert.Equal(t, int64(1), exists, "any attempted ambiguous commit consumes the token")
				assert.Len(t, delivery.plans, test.deliveries)
				if test.disconnect {
					assert.Equal(t, []string{"disconnect"}, hub.events)
				} else {
					assert.Empty(t, hub.events)
				}

				var pendingCount int
				var pendingID uuid.UUID
				err = db.QueryRow(
					`SELECT COUNT(*), COALESCE(MAX(operation_id::text), $2) FROM presence_settings_pending_operations WHERE user_id = $1`,
					senderID, uuid.Nil.String(),
				).Scan(&pendingCount, &pendingID)
				require.NoError(t, err)
				switch test.outcome {
				case presencehistory.ForcedClearRolledBack:
					assert.Zero(t, pendingCount)
				case presencehistory.ForcedClearSuperseded:
					assert.Equal(t, 1, pendingCount)
					assert.Equal(t, laterOperationID, pendingID)
				case presencehistory.ForcedClearUnresolved:
					assert.Equal(t, 1, pendingCount)
					require.Len(t, delivery.plans, 1)
					assert.Nil(t, delivery.plans[0].ClearRecipients)
					assert.Nil(t, delivery.plans[0].UpdateRecipients)
				}
			})
		}
	}
}

func TestRecoveryForcedClearArchivesHistoryBeforeSuccess(t *testing.T) {
	for _, test := range recoveryForcedFlows() {
		t.Run(test.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			senderID := dbtest.CreateUser(t, db)
			seedRecoveryCommitKeyRows(t, db, senderID)
			historyID := seedRecoveryForcedPresence(t, db, senderID)
			rdb := setupAuthAttemptRedis(t)
			claims := &RecoveryClaims{UserID: senderID.String(), JTI: uuid.NewString()}
			delivery := &recoveryCommitDelivery{}
			hub := &recoveryForcedEventRecorder{}
			service := newRecoveryForcedService(t, db, delivery)
			handler := newRecoveryForcedHandler(db, rdb, hub, service, claims)

			response := invokeRecoveryForcedFlow(test, handler)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, delivery.plans, 1)
			assert.Equal(t, presencehistory.DeliveryConservativeReset, delivery.plans[0].Mode)
			assert.Equal(t, senderID, delivery.plans[0].SenderID)
			assert.Equal(t, []string{"disconnect"}, hub.events)
			var endedAt sql.NullTime
			var pending int
			require.NoError(t, db.QueryRow(
				`SELECT ended_at FROM presence_history WHERE id = $1`, historyID,
			).Scan(&endedAt))
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
			).Scan(&pending))
			assert.True(t, endedAt.Valid)
			assert.Zero(t, pending)
		})
	}
}

func TestRecoveryForcedClearRecorderFailureReleasesTokenAndRollsBack(t *testing.T) {
	for _, test := range recoveryForcedFlows() {
		t.Run(test.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			senderID := dbtest.CreateUser(t, db)
			seedRecoveryCommitKeyRows(t, db, senderID)
			seedRecoveryForcedPresence(t, db, senderID)
			rdb := setupAuthAttemptRedis(t)
			claims := &RecoveryClaims{UserID: senderID.String(), JTI: uuid.NewString()}
			delivery := &recoveryCommitDelivery{}
			service := newRecoveryForcedService(t, db, delivery)
			restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
				RecordTransition: func(context.Context, *sql.Tx, uuid.UUID, presencehistory.CustomTextState, presencehistory.CustomTextState) error {
					return errors.New("recovery history recorder failure")
				},
			})
			t.Cleanup(restore)
			handler := newRecoveryForcedHandler(db, rdb, &recoveryForcedEventRecorder{}, service, claims)

			response := invokeRecoveryForcedFlow(test, handler)
			assert.Equal(t, http.StatusInternalServerError, response.Code)
			exists, err := rdb.Exists(
				context.Background(), "recovery_token_used:"+claims.JTI,
			).Result()
			require.NoError(t, err)
			assert.Zero(t, exists, "definite recorder rollback must release the token")
			var tier int
			var text string
			require.NoError(t, db.QueryRow(
				`SELECT custom_text_tier, custom_text FROM user_presence_settings WHERE user_id = $1`, senderID,
			).Scan(&tier, &text))
			assert.Equal(t, 1, tier)
			assert.Equal(t, "recovery visible", text)
			assert.Empty(t, delivery.plans)
		})
	}
}

func TestRecoveryForcedClearDeliveryFailureKeepsTokenAndQuarantine(t *testing.T) {
	for _, test := range recoveryForcedFlows() {
		t.Run(test.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			senderID := dbtest.CreateUser(t, db)
			seedRecoveryCommitKeyRows(t, db, senderID)
			seedRecoveryForcedPresence(t, db, senderID)
			rdb := setupAuthAttemptRedis(t)
			claims := &RecoveryClaims{UserID: senderID.String(), JTI: uuid.NewString()}
			delivery := &recoveryCommitDelivery{err: errors.New("recovery delivery unavailable")}
			hub := &recoveryForcedEventRecorder{}
			service := newRecoveryForcedService(t, db, delivery)
			handler := newRecoveryForcedHandler(db, rdb, hub, service, claims)

			response := invokeRecoveryForcedFlow(test, handler)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			exists, err := rdb.Exists(
				context.Background(), "recovery_token_used:"+claims.JTI,
			).Result()
			require.NoError(t, err)
			assert.Equal(t, int64(1), exists)
			var pending int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
			).Scan(&pending))
			assert.Equal(t, 1, pending)
			assert.Equal(t, []string{"disconnect"}, hub.events)
		})
	}
}

func TestRecoveryForcedClearReadinessAndTrustedIdentityFailures(t *testing.T) {
	flows := recoveryForcedFlows()
	for _, flow := range flows {
		t.Run("invalid validated claim UUID fails closed without panic/"+flow.name, func(t *testing.T) {
			rdb := setupAuthAttemptRedis(t)
			claims := &RecoveryClaims{UserID: "not-a-uuid", JTI: uuid.NewString()}
			handler := newRecoveryForcedHandler(nil, rdb, nil, nil, claims)

			response := invokeRecoveryForcedFlow(flow, handler)
			assert.Equal(t, http.StatusUnauthorized, response.Code)
			exists, err := rdb.Exists(
				context.Background(), "recovery_token_used:"+claims.JTI,
			).Result()
			require.NoError(t, err)
			assert.Zero(t, exists)
		})
	}
	flow := flows[0]

	for _, test := range []struct {
		name    string
		service func(*testing.T, *sql.DB) *presencehistory.Service
		status  int
	}{
		{name: "nil service", status: http.StatusServiceUnavailable},
		{
			name: "unbound service",
			service: func(_ *testing.T, db *sql.DB) *presencehistory.Service {
				return presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
			},
			status: http.StatusServiceUnavailable,
		},
		{
			name: "database readiness failure",
			service: func(t *testing.T, _ *sql.DB) *presencehistory.Service {
				closedDB, err := sql.Open("postgres", dbtest.DatabaseURL())
				require.NoError(t, err)
				require.NoError(t, closedDB.Close())
				service := presencehistory.NewService(closedDB, presencehistory.DisclosureState{}, false)
				require.NoError(t, service.BindDelivery(&recoveryCommitDelivery{}))
				return service
			},
			status: http.StatusInternalServerError,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _ := dbtest.SetupTestDB(t)
			senderID := dbtest.CreateUser(t, db)
			seedRecoveryCommitKeyRows(t, db, senderID)
			rdb := setupAuthAttemptRedis(t)
			claims := &RecoveryClaims{UserID: senderID.String(), JTI: uuid.NewString()}
			var service *presencehistory.Service
			if test.service != nil {
				service = test.service(t, db)
			}
			handler := newRecoveryForcedHandler(db, rdb, nil, service, claims)

			response := invokeRecoveryForcedFlow(flow, handler)
			assert.Equal(t, test.status, response.Code)
			exists, err := rdb.Exists(
				context.Background(), "recovery_token_used:"+claims.JTI,
			).Result()
			require.NoError(t, err)
			assert.Zero(t, exists, "readiness failures occur before mutation and release the token")
		})
	}
}

func TestRecoveryForcedClearTokenAndPasswordInfrastructureFailures(t *testing.T) {
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/recovery", nil)
		return c, response
	}

	t.Run("missing MFA checker fails closed", func(t *testing.T) {
		c, response := newContext()
		handler := &Handler{log: logger.NewWithWriter(io.Discard)}
		claims, usedKey := handler.validateAndConsumeRecoveryToken(c, "token", false)
		assert.Nil(t, claims)
		assert.Empty(t, usedKey)
		assert.Equal(t, http.StatusInternalServerError, response.Code)
	})

	t.Run("token usage backend failure fails closed", func(t *testing.T) {
		rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
		require.NoError(t, rdb.Close())
		c, response := newContext()
		handler := &Handler{
			redis: rdb,
			log:   logger.NewWithWriter(io.Discard),
			mfaChecker: &ambiguousRecoveryMFAChecker{claims: &RecoveryClaims{
				UserID: uuid.NewString(), JTI: uuid.NewString(),
			}},
		}
		claims, usedKey := handler.validateAndConsumeRecoveryToken(c, "token", false)
		assert.Nil(t, claims)
		assert.Empty(t, usedKey)
		assert.Equal(t, http.StatusInternalServerError, response.Code)
	})

}

func TestRecoveryVerifyCodeFailsClosedWithoutTokenIssuer(t *testing.T) {
	rdb := setupAuthAttemptRedis(t)
	email := "forced-clear-token@test.local"
	code := "123456"
	record, err := json.Marshal(recoveryRecord{
		CodeHash: hashRecoveryCode(code), UserID: uuid.NewString(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(
		context.Background(), recoveryRedisKey(email), record, time.Minute,
	).Err())
	handler := &Handler{redis: rdb, log: logger.NewWithWriter(io.Discard)}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/recovery/verify-code",
		strings.NewReader(`{"email":"`+email+`","code":"`+code+`"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	handler.RecoveryVerifyCode(c)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestRecoveryForcedClearTransactionInfrastructureFailures(t *testing.T) {
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/recovery", nil)
		return c, response
	}

	t.Run("begin failure releases token without invoking work", func(t *testing.T) {
		closedDB, err := sql.Open("postgres", dbtest.DatabaseURL())
		require.NoError(t, err)
		require.NoError(t, closedDB.Close())
		service := presencehistory.NewService(closedDB, presencehistory.DisclosureState{}, false)
		rdb := setupAuthAttemptRedis(t)
		usedKey := "recovery_token_used:" + uuid.NewString()
		require.NoError(t, rdb.Set(context.Background(), usedKey, "1", time.Minute).Err())
		handler := &Handler{
			redis: rdb, log: logger.NewWithWriter(io.Discard), presenceHistory: service,
		}
		c, response := newContext()
		workCalled := false

		_, err = handler.execRecoveryTx(
			context.Background(), c, usedKey, errFailedResetPwd,
			func(context.Context, *sql.Tx) (recoveryPresenceResult, error) {
				workCalled = true
				return recoveryPresenceResult{}, nil
			},
		)

		require.Error(t, err)
		assert.False(t, workCalled)
		assert.Equal(t, http.StatusInternalServerError, response.Code)
		exists, redisErr := rdb.Exists(context.Background(), usedKey).Result()
		require.NoError(t, redisErr)
		assert.Zero(t, exists)
	})

	t.Run("missing transaction releases token and returns failure", func(t *testing.T) {
		db, err := sql.Open("postgres", dbtest.DatabaseURL())
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
		restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Begin: func(context.Context, *sql.TxOptions) (*sql.Tx, error) {
				return nil, nil
			},
		})
		t.Cleanup(restore)
		rdb := setupAuthAttemptRedis(t)
		usedKey := "recovery_token_used:" + uuid.NewString()
		require.NoError(t, rdb.Set(context.Background(), usedKey, "1", time.Minute).Err())
		handler := &Handler{
			redis: rdb, log: logger.NewWithWriter(io.Discard), presenceHistory: service,
		}
		c, response := newContext()

		_, err = handler.execRecoveryTx(
			context.Background(), c, usedKey, errFailedResetPwd,
			func(context.Context, *sql.Tx) (recoveryPresenceResult, error) {
				return recoveryPresenceResult{}, errors.New("work must not run")
			},
		)

		require.Error(t, err)
		assert.Equal(t, http.StatusInternalServerError, response.Code)
		exists, redisErr := rdb.Exists(context.Background(), usedKey).Result()
		require.NoError(t, redisErr)
		assert.Zero(t, exists)
	})

	t.Run("rollback failure joins the work error", func(t *testing.T) {
		db, err := sql.Open("postgres", dbtest.DatabaseURL())
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
		workErr := errors.New("recovery work failed")
		rollbackErr := errors.New("recovery rollback failed")
		restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
				return db.BeginTx(ctx, options)
			},
			Rollback: func(*sql.Tx) error { return rollbackErr },
		})
		t.Cleanup(restore)
		rdb := setupAuthAttemptRedis(t)
		usedKey := "recovery_token_used:" + uuid.NewString()
		require.NoError(t, rdb.Set(context.Background(), usedKey, "1", time.Minute).Err())
		handler := &Handler{
			redis: rdb, log: logger.NewWithWriter(io.Discard), presenceHistory: service,
		}
		c, response := newContext()

		_, err = handler.execRecoveryTx(
			context.Background(), c, usedKey, errFailedResetPwd,
			func(context.Context, *sql.Tx) (recoveryPresenceResult, error) {
				return recoveryPresenceResult{}, workErr
			},
		)

		require.ErrorIs(t, err, workErr)
		require.ErrorIs(t, err, rollbackErr)
		assert.Equal(t, http.StatusInternalServerError, response.Code)
	})
}

func TestRecoveryForcedClearReadinessRetryAndOutcomeClasses(t *testing.T) {
	tests := []struct {
		outcome presencehistory.ForcedClearOutcome
		class   string
	}{
		{presencehistory.ForcedClearAcknowledged, "acknowledged"},
		{presencehistory.ForcedClearQuarantined, "quarantined"},
		{presencehistory.ForcedClearRolledBack, "rolled_back"},
		{presencehistory.ForcedClearSuperseded, "superseded"},
		{presencehistory.ForcedClearUnresolved, "unresolved"},
	}
	for _, test := range tests {
		assert.Equal(t, test.class, recoveryForcedClearClass(test.outcome))
	}

	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	handler := &Handler{log: logger.NewWithWriter(io.Discard)}
	handler.respondRecoveryReadinessFailure(c, errFailedResetPwd, &presencehistory.ServiceError{
		Status: http.StatusServiceUnavailable, RetryAfter: 2500 * time.Millisecond,
	})
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Equal(t, "3", response.Header().Get("Retry-After"))
}

func TestRecoveryForcedClearStatementFailureReleasesTokenAndRollsBack(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	senderID := dbtest.CreateUser(t, db)
	seedRecoveryForcedPresence(t, db, senderID)
	rdb := setupAuthAttemptRedis(t)
	usedKey := "recovery_token_used:" + uuid.NewString()
	require.NoError(t, rdb.Set(context.Background(), usedKey, "1", time.Minute).Err())
	service := newRecoveryForcedService(t, db, &recoveryCommitDelivery{})
	handler := &Handler{
		redis: rdb, log: logger.NewWithWriter(io.Discard), presenceHistory: service,
	}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/recovery", nil)

	err := handler.executeRecoveryTransaction(c, senderID, usedKey, errFailedResetPwd, []recoveryTxOp{{
		query: `UPDATE recovery_table_that_does_not_exist SET value = $1`,
		args:  []interface{}{1},
		desc:  "forced statement failure",
	}})

	require.Error(t, err)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	exists, redisErr := rdb.Exists(context.Background(), usedKey).Result()
	require.NoError(t, redisErr)
	assert.Zero(t, exists)
	var tier int
	var text string
	require.NoError(t, db.QueryRow(
		`SELECT custom_text_tier, custom_text FROM user_presence_settings WHERE user_id = $1`, senderID,
	).Scan(&tier, &text))
	assert.Equal(t, 1, tier)
	assert.Equal(t, "recovery visible", text)
}

func newRecoveryForcedService(
	t *testing.T,
	db *sql.DB,
	delivery presencehistory.Delivery,
) *presencehistory.Service {
	t.Helper()
	service := presencehistory.NewService(db, presencehistory.DisclosureState{
		Available: true,
		RequiredConsent: &presencehistory.RequiredConsent{
			Version:  1,
			CopyHash: strings.Repeat("a", 64),
		},
	}, true)
	require.NoError(t, service.BindDelivery(delivery))
	return service
}

func newRecoveryForcedHandler(
	db *sql.DB,
	rdb *redis.Client,
	hub SessionDisconnector,
	service *presencehistory.Service,
	claims *RecoveryClaims,
) *Handler {
	return &Handler{
		db: db, redis: rdb, log: logger.NewWithWriter(io.Discard), hub: hub,
		mfaChecker: &ambiguousRecoveryMFAChecker{claims: claims}, presenceHistory: service,
	}
}

func invokeRecoveryForcedFlow(
	flow recoveryForcedFlow,
	handler *Handler,
) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, flow.path, strings.NewReader(flow.body))
	c.Request.Header.Set("Content-Type", "application/json")
	flow.handler(handler, c)
	return response
}

func seedRecoveryForcedPresence(t *testing.T, db *sql.DB, senderID uuid.UUID) uuid.UUID {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text, custom_text_emoji,
			activity_history_enabled, activity_history_retention_days,
			activity_history_consent_version, activity_history_consent_copy_hash,
			activity_history_consented_at
		) VALUES ($1, 1, 'recovery visible', 'lock', TRUE, 30, 1, $2, clock_timestamp())
	`, senderID, strings.Repeat("a", 64))
	require.NoError(t, err)
	historyID := uuid.New()
	_, err = db.Exec(`
		INSERT INTO presence_history (
			id, sender_id, category, payload_version, payload,
			started_at, recorded_at, expires_at
		) VALUES (
			$1, $2, 'custom_text', 1, '{"text":"recovery visible","emoji":"lock"}',
			clock_timestamp(), clock_timestamp(), clock_timestamp() + INTERVAL '30 days'
		)
	`, historyID, senderID)
	require.NoError(t, err)
	return historyID
}

func seedRecoveryCommitKeyRows(t *testing.T, db *sql.DB, senderID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt)
		 VALUES ($1, 'old-wrapped', 'old-salt')`, senderID,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO public_keys (id, user_id, public_key, key_version)
		 VALUES ($1, $2, 'old-public', 1)`, uuid.New(), senderID,
	)
	require.NoError(t, err)
}
