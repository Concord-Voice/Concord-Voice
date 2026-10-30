package testhelpers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const staleBlobGuard = `SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`

// This pins a real handler epoch comparison: deleting MatchEpoch must let the
// write through and fail the response/state assertions, after proven admission.
func TestStaleBoundary_ActualHTTPAdmitsThenRotatesBeforeBlobGuard(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	ts, boundary := SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "staleboundaryblob")
	identity := fixtureIdentity(t, ts.DB, user.AccessToken, true)
	require.NoError(t, boundary.ArmControl(user.AccessToken))
	boundary.Hook.ArmArg([]string{staleBlobGuard}, user.ID, nil, nil)
	w := ts.DoRequest(http.MethodPut, "/api/v1/users/me/preferences", map[string]any{"encrypted_data": "Y29udHJvbA=="}, AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, boundary.Check())
	seen, err := boundary.Hook.Report()
	require.NoError(t, err)
	require.Equal(t, 1, seen)

	require.NoError(t, boundary.Arm(user.AccessToken))
	boundary.Hook.ArmArg([]string{staleBlobGuard}, user.ID, nil, nil)
	w = ts.DoRequest(http.MethodPut, "/api/v1/users/me/preferences", map[string]any{"encrypted_data": "c3RhbGU="}, AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"Authentication required"}`, w.Body.String())
	boundary.RequireRotated(t)
	seen, err = boundary.Hook.Report()
	require.NoError(t, err)
	require.Equal(t, 1, seen, "handler lock/epoch read must run after actual auth commit")
	var data string
	var version int
	require.NoError(t, ts.DB.QueryRow(`SELECT encrypted_data, version FROM user_preferences WHERE user_id=$1`, user.ID).Scan(&data, &version))
	assert.Equal(t, "Y29udHJvbA==", data)
	assert.Equal(t, 1, version)
	_, err = loginfamily.ResolveVerifiedAccess(context.Background(), ts.DB, identity)
	assert.ErrorIs(t, err, loginfamily.ErrDenied, "old actual JWT is now durably stale")
	var expiryUnchanged bool
	require.NoError(t, ts.DB.QueryRow(`SELECT access_expires_at=$2 FROM login_family_sids WHERE sid=$1`, identity.SID, identity.AccessExpiresAt).Scan(&expiryUnchanged))
	assert.True(t, expiryUnchanged)
	var parents int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections WHERE user_id=$1 AND superseded_credential_epoch=$2`, user.ID, identity.CredentialEpoch).Scan(&parents))
	assert.Equal(t, 1, parents, "rotation must use the real public epoch-owner path")
}

func TestStaleBoundary_InvalidHTTPDoesNotConsumeValidArm(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	ts, boundary := SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "staleboundaryinvalid")
	require.NoError(t, boundary.Arm(user.AccessToken))
	require.Error(t, boundary.Check(), "missing admission observation is a fixture failure")
	w := ts.DoRequest(http.MethodPut, "/api/v1/users/me/preferences", map[string]any{"encrypted_data": "YQ=="}, AuthHeaders("invalid-test-token"))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Zero(t, boundary.Hook.AdmissionReport().Fired)
	assert.True(t, boundary.Hook.AdmissionReport().Pending)
	require.Error(t, boundary.Check())
	w = ts.DoRequest(http.MethodPut, "/api/v1/users/me/preferences", map[string]any{"encrypted_data": "YQ=="}, AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	boundary.RequireRotated(t)
}

func TestStaleBoundary_InvalidArmAndCallbackFailureAreObservable(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	ts, boundary := SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "staleboundaryerrors")
	require.Error(t, boundary.Arm("invalid-test-token"))
	raw, err := auth.GenerateAccessToken(user.ID, TestJWTSecret, true, "", "")
	require.NoError(t, err)
	require.Error(t, boundary.Arm(raw), "a correctly signed token without a real SID is not admission authority")
	require.Error(t, boundary.Check())
	require.NoError(t, boundary.Arm(user.AccessToken))
	// Fail only the fixture's rotation, after a real successful auth commit.
	// The returned HTTP success cannot turn this into a passing boundary check.
	fault := errors.New("fixture rotation failed")
	boundary.rotate = func(context.Context, string) (string, error) { return "", fault }
	w := ts.DoRequest(http.MethodPut, "/api/v1/users/me/preferences", map[string]any{"encrypted_data": "YQ=="}, AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.ErrorIs(t, boundary.Hook.AdmissionReport().CallbackErr, fault)
	assert.ErrorIs(t, boundary.Check(), fault)
}

func TestStaleBoundary_RotationRejectsMissingUser(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	ts, _ := SetupStaleBoundaryServer(t)
	_, err := rotateCredentialEpoch(context.Background(), ts, "00000000-0000-0000-0000-000000000123")
	require.Error(t, err)
}

// Later-barrier callers need a rotation primitive without moving their race to
// the HTTP admission seam. The public helper must advance durable authority.
func TestStaleBoundary_StandaloneRotationUsesPublicEpochOwner(t *testing.T) {
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "explicit disposable database required")
	ts, _ := SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "staleboundarystandalone")
	identity := fixtureIdentity(t, ts.DB, user.AccessToken, true)
	epoch := ts.RotateCredentialEpoch(t, user.ID)
	assert.Len(t, epoch, 32)
	assert.NotEqual(t, identity.CredentialEpoch, epoch)
	var stored string
	require.NoError(t, ts.DB.QueryRow(`SELECT credential_epoch FROM users WHERE id=$1`, user.ID).Scan(&stored))
	assert.Equal(t, epoch, stored)
	_, err := loginfamily.ResolveVerifiedAccess(context.Background(), ts.DB, identity)
	assert.ErrorIs(t, err, loginfamily.ErrDenied)
}
