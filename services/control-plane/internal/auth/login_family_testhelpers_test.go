package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const familyTestSecret = "logical-family-test-signing-secret"

type familyTestRig struct {
	h    *Handler
	db   *sql.DB
	user string
}

func newFamilyTestRig(t *testing.T) familyTestRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	rdb := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), rdb))
	user := dbtest.CreateUser(t, db).String()
	_, err := db.Exec(`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg) VALUES ($1, $2, $3, 1, 'argon2id')`, user, []byte{1}, []byte{2})
	require.NoError(t, err)
	return familyTestRig{NewHandler(db, rdb, logger.NewWithWriter(io.Discard), familyTestSecret, nil), db, user}
}

func familyTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	c.Request.RemoteAddr = "192.0.2.10:1234"
	c.Request.Header.Set("User-Agent", "logical-family-fixture")
	return c, w
}

type familyPair struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
}

func familyResponse(t *testing.T, w *httptest.ResponseRecorder) familyPair {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var pair familyPair
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &pair))
	require.NotEmpty(t, pair.Access)
	require.NotEmpty(t, pair.Refresh)
	return pair
}

// Compare SQL to the JWT actually delivered, never a separately estimated TTL.
func assertFamilyAccess(t *testing.T, rig familyTestRig, access string) (string, string) {
	t.Helper()
	claims, err := ValidateAccessToken(access, familyTestSecret)
	require.NoError(t, err)
	require.Equal(t, rig.user, claims.UserID)
	require.NotEmpty(t, claims.SessionID)
	var family, owner, epoch string
	var exact bool
	require.NoError(t, rig.db.QueryRow(`SELECT s.family_id, s.user_id, f.credential_epoch, s.access_expires_at = $2 FROM login_family_sids s JOIN login_families f USING (family_id, user_id) WHERE s.sid = $1`, claims.SessionID, claims.ExpiresAt.Time).Scan(&family, &owner, &epoch, &exact))
	require.NotEqual(t, uuid.Nil.String(), family)
	require.Equal(t, rig.user, owner)
	require.Equal(t, claims.CredentialEpoch, epoch)
	require.True(t, exact, "immutable membership must store exact signed NumericDate expiry")
	grant, err := loginfamily.ResolveVerifiedAccess(context.Background(), rig.db, loginfamily.AccessIdentity{UserID: uuid.MustParse(claims.UserID), SID: uuid.MustParse(claims.SessionID), CredentialEpoch: claims.CredentialEpoch, AccessExpiresAt: claims.ExpiresAt.Time})
	require.NoError(t, err, "the delivered JWT must resolve through real live family authority")
	require.Equal(t, family, grant.FamilyID.String())
	var refreshOwner string
	require.NoError(t, rig.db.QueryRow(`SELECT user_id FROM refresh_tokens WHERE id = $1 AND token_hash <> ''`, claims.SessionID).Scan(&refreshOwner))
	require.Equal(t, rig.user, refreshOwner)
	return family, claims.SessionID
}
