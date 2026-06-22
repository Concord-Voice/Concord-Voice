package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// uniqueAdminUsername returns a per-call unique handle. The admin_* tables are
// NOT in testhelpers.TruncateAllTables (out of this package's change scope), so
// each test self-isolates via a unique username + ON DELETE CASCADE cleanup.
var adminUsernameSeq atomic.Int64

func uniqueAdminUsername(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), adminUsernameSeq.Add(1))
}

// registerAdminCleanup arranges for the admin row (and, via CASCADE, its
// credentials) to be deleted at test end so the shared test DB does not
// accumulate rows across runs. Both this delete and testhelpers' DB-close are
// registered via t.Cleanup (LIFO): registering the delete AFTER the close means
// it runs BEFORE the close, so the DELETE still has a live connection.
func registerAdminCleanup(t *testing.T, db *sql.DB, adminID string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_users WHERE id = $1`, adminID)
		assert.NoError(t, err)
	})
}

func TestAdminRepo_CreatePending(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)

	t.Run("creates a pending admin", func(t *testing.T) {
		username := uniqueAdminUsername("create")
		u, err := repo.CreatePending(ctx, username, "$argon2id$v=19$m=65536,t=3,p=4$abc$def")
		require.NoError(t, err)
		registerAdminCleanup(t, db, u.ID)

		assert.NotEmpty(t, u.ID)
		assert.Equal(t, username, u.Username)
		assert.Equal(t, admin.StatusPending, u.Status)
		assert.False(t, u.CreatedAt.IsZero())
	})

	t.Run("rejects a duplicate username", func(t *testing.T) {
		username := uniqueAdminUsername("dup")
		u, err := repo.CreatePending(ctx, username, "hash1")
		require.NoError(t, err)
		registerAdminCleanup(t, db, u.ID)

		_, err = repo.CreatePending(ctx, username, "hash2")
		require.Error(t, err)
		assert.ErrorIs(t, err, admin.ErrDuplicateUsername)
	})
}

func TestAdminRepo_GetByUsername(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)

	t.Run("returns the stored admin", func(t *testing.T) {
		username := uniqueAdminUsername("get")
		created, err := repo.CreatePending(ctx, username, "stored-hash")
		require.NoError(t, err)
		registerAdminCleanup(t, db, created.ID)

		got, err := repo.GetByUsername(ctx, username)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, created.ID, got.ID)
		assert.Equal(t, "stored-hash", got.PasswordHash)
		assert.Equal(t, admin.StatusPending, got.Status)
	})

	t.Run("returns ErrAdminNotFound for an unknown username", func(t *testing.T) {
		got, err := repo.GetByUsername(ctx, uniqueAdminUsername("missing"))
		assert.Nil(t, got)
		assert.ErrorIs(t, err, admin.ErrAdminNotFound)
	})
}

func TestAdminRepo_SetStatusAndDisable(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)

	t.Run("SetStatus flips pending to active", func(t *testing.T) {
		u, err := repo.CreatePending(ctx, uniqueAdminUsername("setstatus"), "h")
		require.NoError(t, err)
		registerAdminCleanup(t, db, u.ID)

		require.NoError(t, repo.SetStatus(ctx, u.ID, admin.StatusActive))
		got, err := repo.GetByUsername(ctx, u.Username)
		require.NoError(t, err)
		assert.Equal(t, admin.StatusActive, got.Status)
	})

	t.Run("Disable sets status disabled and disabled_at", func(t *testing.T) {
		u, err := repo.CreatePending(ctx, uniqueAdminUsername("disable"), "h")
		require.NoError(t, err)
		registerAdminCleanup(t, db, u.ID)

		require.NoError(t, repo.Disable(ctx, u.ID))
		got, err := repo.GetByUsername(ctx, u.Username)
		require.NoError(t, err)
		assert.Equal(t, admin.StatusDisabled, got.Status)
		require.NotNil(t, got.DisabledAt)
		assert.False(t, got.DisabledAt.IsZero())
	})

	t.Run("SetStatus on an unknown id returns ErrAdminNotFound", func(t *testing.T) {
		err := repo.SetStatus(ctx, "00000000-0000-0000-0000-000000000000", admin.StatusActive)
		assert.ErrorIs(t, err, admin.ErrAdminNotFound)
	})
}

func TestAdminRepo_Credentials(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	repo := admin.NewAdminRepo(db)

	u, err := repo.CreatePending(ctx, uniqueAdminUsername("creds"), "h")
	require.NoError(t, err)
	registerAdminCleanup(t, db, u.ID)

	// credential_id is globally UNIQUE; the admin_* tables are not truncated
	// between runs (out of scope), so derive a per-run-unique credential_id to
	// avoid colliding with a leftover row from a prior run.
	credID := []byte(uniqueAdminUsername("cred-id"))
	cred := admin.AdminCredential{ // #nosec G101 -- test fixture WebAuthn credential, not a secret
		AdminID:        u.ID,
		CredentialID:   credID,
		PublicKey:      []byte{0x10, 0x11},
		AAGUID:         []byte{0xee, 0x88, 0x28, 0x79},
		SignCount:      0,
		CredentialName: "yubikey-primary",
		Transports:     []string{"usb", "nfc"},
	}

	t.Run("AddCredential then ListCredentials round-trips", func(t *testing.T) {
		stored, err := repo.AddCredential(ctx, cred)
		require.NoError(t, err)
		assert.NotEmpty(t, stored.ID)

		list, err := repo.ListCredentials(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, cred.CredentialID, list[0].CredentialID)
		assert.Equal(t, cred.PublicKey, list[0].PublicKey)
		assert.Equal(t, cred.AAGUID, list[0].AAGUID)
		assert.Equal(t, "yubikey-primary", list[0].CredentialName)
		assert.Equal(t, []string{"usb", "nfc"}, list[0].Transports)
	})

	t.Run("CountActiveCredentials reflects the stored count", func(t *testing.T) {
		n, err := repo.CountActiveCredentials(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})

	t.Run("UpdateCredentialSignCount advances the stored counter (clone-detection)", func(t *testing.T) {
		require.NoError(t, repo.UpdateCredentialSignCount(ctx, credID, 42))
		var got int64
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT sign_count FROM admin_webauthn_credentials WHERE credential_id = $1`, credID).Scan(&got))
		assert.Equal(t, int64(42), got)
	})

	t.Run("UpdateCredentialSignCount errors on an unknown credential", func(t *testing.T) {
		err := repo.UpdateCredentialSignCount(ctx, []byte(uniqueAdminUsername("missing-cred")), 7)
		require.Error(t, err)
	})

	t.Run("a duplicate credential_id is rejected", func(t *testing.T) {
		_, err := repo.AddCredential(ctx, cred)
		require.Error(t, err)
	})
}
