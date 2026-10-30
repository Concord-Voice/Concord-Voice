package middleware

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const familyHTTPSecret = "family-http-test-only-signing-secret" // pragma: allowlist secret

type familyHTTPFixture struct {
	db       *sql.DB
	rc       *redis.Client
	identity lf.AccessIdentity
}

func newFamilyHTTPFixture(t *testing.T) familyHTTPFixture {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	rc := redistest.Client(t)
	require.NoError(t, redistest.Reset(t.Context(), rc))
	t.Cleanup(func() {
		_, err := testdb.ExecReplica(db, `TRUNCATE login_family_rollout, login_family_revocations, login_families, login_family_sids CASCADE`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES (true)`)
		require.NoError(t, err)
	})
	identity := lf.AccessIdentity{UserID: testdb.CreateUser(t, db), SID: uuid.New(), AccessExpiresAt: time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	})
	barrier, err := lf.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := lf.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{identity.UserID}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(identity.UserID)
	require.NoError(t, err)
	family, err := lf.CreateFamilyTx(t.Context(), tx, user, "")
	require.NoError(t, err)
	require.NoError(t, lf.BindSIDTx(t.Context(), tx, family, identity.SID, identity.AccessExpiresAt))
	require.NoError(t, tx.Commit())
	return familyHTTPFixture{db, rc, identity}
}

func (f familyHTTPFixture) active(t *testing.T) {
	t.Helper()
	// Explicit disposable SQL fixture, not the operator activation workflow.
	_, err := f.db.Exec(`UPDATE login_family_rollout SET activated_at=NOW(),receipt='{}'::jsonb WHERE id=true`)
	require.NoError(t, err)
}
func (f familyHTTPFixture) claims() jwt.MapClaims {
	return jwt.MapClaims{"iss": AccessTokenIssuer, "user_id": f.identity.UserID.String(), "sid": f.identity.SID.String(), "cred_epoch": "", "email_verified": true, "jti": uuid.NewString(), "exp": f.identity.AccessExpiresAt.Unix()}
}
func (f familyHTTPFixture) request(t *testing.T, claims jwt.MapClaims, db *sql.DB, fence *credepoch.Fence, handlers ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(familyHTTPSecret))
	require.NoError(t, err)
	r := gin.New()
	chain := make([]gin.HandlerFunc, 0, 1+len(handlers))
	chain = append(chain, AuthRequired(familyHTTPSecret, f.rc, db, fence))
	chain = append(chain, handlers...)
	r.GET("/", chain...)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func familyHTTPAllowed(c *gin.Context) { c.Status(http.StatusNoContent) }

func TestLoginFamilyHTTPExactSQLAuthority(t *testing.T) {
	for _, name := range []string{"live", "unknown SID", "different signed expiry", "wrong epoch", "terminal", "unknown user", "missing SID"} {
		t.Run(name, func(t *testing.T) {
			f := newFamilyHTTPFixture(t)
			f.active(t)
			claims := f.claims()
			status := http.StatusUnauthorized
			switch name {
			case "live":
				status = http.StatusNoContent
			case "unknown SID":
				claims["sid"] = uuid.NewString()
			case "missing SID":
				delete(claims, "sid")
			case "different signed expiry":
				claims["exp"] = f.identity.AccessExpiresAt.Add(time.Second).Unix()
			case "wrong epoch":
				claims["cred_epoch"] = "00112233445566778899aabbccddeeff" // pragma: allowlist secret
			case "unknown user":
				claims["user_id"] = uuid.NewString()
			case "terminal":
				f.revoke(t)
			}
			w := f.request(t, claims, f.db, nil, familyHTTPAllowed)
			require.Equal(t, status, w.Code, w.Body.String())
			if status == http.StatusUnauthorized {
				require.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
			}
		})
	}
}
func (f familyHTTPFixture) revoke(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := tx.Rollback()
		if !errors.Is(err, sql.ErrTxDone) {
			require.NoError(t, err)
		}
	}()
	b, err := lf.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	set, err := lf.LockUsersTx(ctx, tx, b, []uuid.UUID{f.identity.UserID}, lf.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := set.ForUser(f.identity.UserID)
	require.NoError(t, err)
	targets, err := lf.ResolveTargetSIDTx(ctx, tx, user, f.identity.SID)
	require.NoError(t, err)
	_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeSession, Reason: lf.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestLoginFamilyHTTPPreservesEarlierRedisAndEpochGates(t *testing.T) {
	for _, name := range []string{"blacklist", "disabled", "theft", "epoch"} {
		t.Run(name, func(t *testing.T) {
			f := newFamilyHTTPFixture(t)
			f.active(t)
			claims := f.claims()
			// SQL authority also denies, but each earlier gate has a distinct external
			// response or a deliberately unavailable later dependency.
			var fence *credepoch.Fence
			status := http.StatusUnauthorized
			body := `{"error":"Authentication required"}`
			switch name {
			case "blacklist":
				jti, ok := claims["jti"].(string)
				require.True(t, ok, "jti claim must be a string")
				require.NoError(t, f.rc.Set(t.Context(), "blacklist:"+jti, "1", time.Minute).Err())
			case "disabled":
				require.NoError(t, f.rc.Set(t.Context(), UserDisabledKey(f.identity.UserID.String()), "1", time.Minute).Err())
				status = http.StatusForbidden
				body = `{"error_code":"account_disabled"}`
			case "theft":
				require.NoError(t, f.rc.Set(t.Context(), TokenTheftBlockedKey(f.identity.UserID.String()), "1", time.Minute).Err())
				status = http.StatusServiceUnavailable
				body = `{"error":"Session revocation pending: retry later","error_code":"session_revocation_pending"}`
			case "epoch":
				require.NoError(t, f.rc.Set(t.Context(), credepoch.Key(f.identity.UserID.String()), "blocked:fixture", time.Minute).Err())
				fence = credepoch.New(f.db, f.rc, nil)
			}
			broken, err := sql.Open("postgres", testdb.DatabaseURL())
			require.NoError(t, err)
			require.NoError(t, broken.Close())
			admissionDB := broken
			if name == "epoch" {
				admissionDB = f.db
				familyHTTPUnavailableTable(t, f.db)
			}
			w := f.request(t, claims, admissionDB, fence, familyHTTPAllowed)
			require.Equal(t, status, w.Code, w.Body.String())
			require.JSONEq(t, body, w.Body.String())
		})
	}
}

func TestLoginFamilyHTTPUnavailableAuthorityIs503(t *testing.T) {
	f := newFamilyHTTPFixture(t)
	f.active(t)
	// Keep the older theft SQL lookup healthy and break only new family authority.
	familyHTTPUnavailableTable(t, f.db)
	w := f.request(t, f.claims(), f.db, nil, familyHTTPAllowed)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, w.Body.String())
}

func TestLoginFamilyHTTPVerifiedContextIntegrity(t *testing.T) {
	f := newFamilyHTTPFixture(t)
	f.active(t)
	raw, _ := gin.CreateTestContext(httptest.NewRecorder())
	raw.Set("user_id", f.identity.UserID.String())
	raw.Set(JWTClaimsContextKey, f.claims())
	_, _, ok := VerifiedAccessFromContext(raw)
	require.False(t, ok, "public context keys cannot manufacture verified family admission")
	for _, name := range []string{"snapshot", "user mutation", "claims user mutation", "SID mutation", "expiry mutation", "epoch mutation"} {
		t.Run(name, func(t *testing.T) {
			w := f.request(t, f.claims(), f.db, nil, func(c *gin.Context) {
				identity, grant, ok := VerifiedAccessFromContext(c)
				require.True(t, ok)
				require.Equal(t, f.identity.UserID, identity.UserID)
				require.Equal(t, f.identity.SID, identity.SID)
				require.Equal(t, f.identity.CredentialEpoch, identity.CredentialEpoch)
				require.True(t, f.identity.AccessExpiresAt.Equal(identity.AccessExpiresAt))
				require.Equal(t, identity.UserID, grant.UserID)
				require.NotEqual(t, uuid.Nil, grant.FamilyID)
				switch name {
				case "snapshot":
					identity.SID = uuid.New()
					grant.FamilyID = uuid.New()
				case "user mutation":
					c.Set("user_id", uuid.NewString())
				default:
					value, exists := c.Get(JWTClaimsContextKey)
					require.True(t, exists)
					claims, valid := value.(jwt.MapClaims)
					require.True(t, valid)
					switch name {
					case "claims user mutation":
						claims["user_id"] = uuid.NewString()
					case "SID mutation":
						claims["sid"] = uuid.NewString()
					case "expiry mutation":
						claims["exp"] = f.identity.AccessExpiresAt.Add(time.Second).Unix()
					case "epoch mutation":
						claims["cred_epoch"] = "00112233445566778899aabbccddeeff" // pragma: allowlist secret
					}
				}
				again, _, valid := VerifiedAccessFromContext(c)
				require.Equal(t, name == "snapshot", valid)
				if valid {
					require.Equal(t, f.identity.SID, again.SID)
					require.True(t, f.identity.AccessExpiresAt.Equal(again.AccessExpiresAt))
				}
				c.Status(http.StatusNoContent)
			})
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		})
	}
}

// Temporarily hide only the new authority table in this exclusive scratch DB.
// Unlike closing the pool, this cannot fail at the older theft SQL gate.
func familyHTTPUnavailableTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`ALTER TABLE login_family_sids RENAME TO family_http_fixture_unavailable`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`ALTER TABLE family_http_fixture_unavailable RENAME TO login_family_sids`)
		require.NoError(t, err)
	})
}

func TestLoginFamilyHTTPPreparedStillRequiresRealFamily(t *testing.T) {
	f := newFamilyHTTPFixture(t)
	require.Equal(t, http.StatusNoContent, f.request(t, f.claims(), f.db, nil, familyHTTPAllowed).Code)
	for _, name := range []string{"missing SID", "unmapped SID"} {
		t.Run(name, func(t *testing.T) {
			claims := f.claims()
			delete(claims, "sid")
			if name == "unmapped SID" {
				claims["sid"] = uuid.NewString()
			}
			w := f.request(t, claims, f.db, nil, familyHTTPAllowed)
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
	}
}

func TestLoginFamilyHTTPRetainsVerifiedEmailPolicy(t *testing.T) {
	f := newFamilyHTTPFixture(t)
	f.active(t)
	claims := f.claims()
	claims["email_verified"] = false
	w := f.request(t, claims, f.db, nil, RequireVerifiedEmail(), familyHTTPAllowed)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.JSONEq(t, `{"error":"Email verification required","code":"EMAIL_NOT_VERIFIED"}`, w.Body.String())
}
