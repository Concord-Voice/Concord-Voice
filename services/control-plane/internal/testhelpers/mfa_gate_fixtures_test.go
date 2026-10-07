package testhelpers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

func insertFixtureUserAndServer(t *testing.T, db *sql.DB) (userID, serverID string) {
	t.Helper()
	userID, serverID = uuid.NewString(), uuid.NewString()
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, 'stored-hash-fixture', true, true)`, userID, userID+"@mfagate.test", "mg"+userID[:8])
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'mfa gate fixture', $2)`, serverID, userID)
	require.NoError(t, err)
	return userID, serverID
}

// The enrolled row satisfies P1 through the same reader the gates use, and
// the toggle helper moves the flag both ways.
func TestMFAGateFixtures_EnrollAndEnforce(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	userID, serverID := insertFixtureUserAndServer(t, db)

	methods, err := stepup.InlineMFAMethods(context.Background(), db, userID)
	require.NoError(t, err)
	require.Empty(t, methods, "control: a fresh user has no inline factor")

	EnrollInlineTOTP(t, db, userID)
	methods, err = stepup.InlineMFAMethods(context.Background(), db, userID)
	require.NoError(t, err)
	assert.Equal(t, []string{"totp"}, methods)

	for _, want := range []bool{true, false} {
		SetServerMFAEnforcement(t, db, serverID, want)
		var got bool
		require.NoError(t, db.QueryRow(`SELECT enforce_mfa_dangerous_actions FROM servers WHERE id = $1`, serverID).Scan(&got))
		assert.Equal(t, want, got)
	}
}

// The fake accepts only its code, counts every call with its purpose, and
// surfaces Err as the verifier fault stepup turns into a 500.
func TestFakeMFAVerifier_CountsAndJudges(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	v := &FakeMFAVerifier{AcceptCode: "123456"}
	methods := []string{"totp"}
	purpose := stepup.PurposeServerMFAEnforcementOff

	assert.Nil(t, stepup.VerifyMFAFactorTx(ctx, tx, v, "u", purpose, "123456", methods))
	wrong := stepup.VerifyMFAFactorTx(ctx, tx, v, "u", purpose, "654321", methods)
	require.NotNil(t, wrong)
	assert.Equal(t, http.StatusForbidden, wrong.Status)
	missing := stepup.VerifyMFAFactorTx(ctx, tx, v, "u", purpose, "", methods)
	require.NotNil(t, missing)
	assert.Equal(t, true, missing.Body["mfa_required"])

	assert.Equal(t, 2, v.Calls(), "a missing code must not reach the verifier")
	assert.Equal(t, []stepup.Purpose{purpose, purpose}, v.Purposes())

	v.Err = errors.New("verifier down")
	fault := stepup.VerifyMFAFactorTx(ctx, tx, v, "u", purpose, "123456", methods)
	require.NotNil(t, fault)
	assert.Equal(t, http.StatusInternalServerError, fault.Status)
	assert.Equal(t, 3, v.Calls())

	got, err := (&FakeMFAVerifier{}).GetEnabledMethods(ctx, "u")
	require.NoError(t, err)
	assert.Equal(t, []string{"totp"}, got, "nil Methods answers totp")
	ok, err := (&FakeMFAVerifier{}).VerifyCodeTx(ctx, tx, "u", purpose, "")
	require.NoError(t, err)
	assert.False(t, ok, "an empty AcceptCode accepts nothing, not the empty code")
}
