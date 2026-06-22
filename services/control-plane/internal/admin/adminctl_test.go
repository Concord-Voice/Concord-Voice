package admin_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

const testEnrollBaseURL = "https://admin.example.org"

func TestAdminCtl_Bootstrap_CreatesPendingAdminAndPrintsToken(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	username := uniqueAdminUsername("bootstrap")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin_users WHERE username = $1`, username)
	})

	var stdout bytes.Buffer
	stdin := strings.NewReader("Str0ng-P@ssw0rd-123\n")

	code := admin.RunAdminCtlForTest(ctx, db, rdb, stdin, &stdout, testEnrollBaseURL,
		[]string{"bootstrap", "--username", username, "--password-stdin"})
	require.Equal(t, 0, code, "stdout: %s", stdout.String())

	// A pending admin exists with the given handle.
	repo := admin.NewAdminRepo(db)
	created, err := repo.GetByUsername(ctx, username)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusPending, created.Status)
	assert.NotEmpty(t, created.PasswordHash)
	assert.NotEqual(t, "Str0ng-P@ssw0rd-123", created.PasswordHash, "password must be hashed, not stored plaintext")

	// The enrollment URL + token print to STDOUT (the operator's terminal).
	out := stdout.String()
	assert.Contains(t, out, "Enroll URL:")
	assert.Contains(t, out, testEnrollBaseURL+"/admin/enroll")
	assert.Contains(t, out, "Token:")

	// The printed token is consumable exactly once (proving it was minted +
	// hashed in Redis, not just echoed).
	token := extractToken(t, out)
	require.NotEmpty(t, token)
	enroll := admin.NewEnrollmentStore(rdb)
	gotAdminID, err := enroll.ConsumeEnrollmentToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, created.ID, gotAdminID)
}

func TestAdminCtl_Bootstrap_NeverPrintsPasswordToStdout(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	username := uniqueAdminUsername("bootstrap-nopw")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin_users WHERE username = $1`, username)
	})

	const password = "Str0ng-P@ssw0rd-XYZ" //nolint:gosec // pragma: allowlist secret -- test fixture asserting it is NOT echoed
	var stdout bytes.Buffer
	stdin := strings.NewReader(password + "\n")

	code := admin.RunAdminCtlForTest(ctx, db, rdb, stdin, &stdout, testEnrollBaseURL,
		[]string{"bootstrap", "--username", username, "--password-stdin"})
	require.Equal(t, 0, code)

	assert.NotContains(t, stdout.String(), password, "the password must never reach stdout")
}

func TestAdminCtl_Bootstrap_RejectsWeakPassword(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	username := uniqueAdminUsername("bootstrap-weak")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin_users WHERE username = $1`, username)
	})

	var stdout bytes.Buffer
	stdin := strings.NewReader("short\n") // < 12 chars, fails ValidatePasswordStrength

	code := admin.RunAdminCtlForTest(ctx, db, rdb, stdin, &stdout, testEnrollBaseURL,
		[]string{"bootstrap", "--username", username, "--password-stdin"})
	require.Equal(t, 1, code)

	// No admin row was created.
	repo := admin.NewAdminRepo(db)
	_, err := repo.GetByUsername(ctx, username)
	assert.ErrorIs(t, err, admin.ErrAdminNotFound)
}

func TestAdminCtl_Bootstrap_RequiresUsername(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	var stdout bytes.Buffer
	code := admin.RunAdminCtlForTest(ctx, db, rdb, strings.NewReader("Str0ng-P@ssw0rd-123\n"), &stdout, testEnrollBaseURL,
		[]string{"bootstrap", "--password-stdin"})
	require.Equal(t, 1, code)
	assert.Contains(t, stdout.String(), "--username is required")
}

func TestAdminCtl_UnknownVerb(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	var stdout bytes.Buffer
	code := admin.RunAdminCtlForTest(ctx, db, rdb, strings.NewReader(""), &stdout, testEnrollBaseURL,
		[]string{"frobnicate"})
	require.Equal(t, 2, code)
	assert.Contains(t, stdout.String(), "unknown verb")
}

func TestAdminCtl_ResetEnrollment_RevokesKeysAndMintsToken(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	repo := admin.NewAdminRepo(db)
	username := uniqueAdminUsername("reset")
	created, err := repo.CreatePending(ctx, username, "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)

	// Make the admin active with a credential, simulating a fully enrolled admin.
	require.NoError(t, repo.SetStatus(ctx, created.ID, admin.StatusActive))
	_, err = repo.AddCredential(ctx, admin.AdminCredential{ // #nosec G101 -- test fixture WebAuthn credential, not a secret
		AdminID:      created.ID,
		CredentialID: []byte(uniqueAdminUsername("reset-cred")),
		PublicKey:    []byte{0x01, 0x02},
		AAGUID:       []byte{0xee, 0x88, 0x28, 0x79, 0x72, 0x1c, 0x49, 0x13, 0x97, 0x75, 0x3d, 0xfc, 0xce, 0x97, 0x07, 0x2a},
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	code := admin.RunAdminCtlForTest(ctx, db, rdb, strings.NewReader(""), &stdout, testEnrollBaseURL,
		[]string{"reset-enrollment", "--username", username})
	require.Equal(t, 0, code, "stdout: %s", stdout.String())

	// Existing credentials were revoked and status returned to pending.
	count, err := repo.CountActiveCredentials(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	got, err := repo.GetByUsername(ctx, username)
	require.NoError(t, err)
	assert.Equal(t, admin.StatusPending, got.Status)

	// A fresh enrollment token was minted and is consumable.
	token := extractToken(t, stdout.String())
	require.NotEmpty(t, token)
	enroll := admin.NewEnrollmentStore(rdb)
	gotAdminID, err := enroll.ConsumeEnrollmentToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, created.ID, gotAdminID)
}

func TestAdminCtl_ResetEnrollment_UnknownAdmin(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)
	ctx := context.Background()

	var stdout bytes.Buffer
	code := admin.RunAdminCtlForTest(ctx, db, rdb, strings.NewReader(""), &stdout, testEnrollBaseURL,
		[]string{"reset-enrollment", "--username", uniqueAdminUsername("nope")})
	require.Equal(t, 1, code)
}

// extractToken pulls the "Token:      <value>" line value out of stdout.
func extractToken(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Token:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "Token:"))
		}
	}
	return ""
}
