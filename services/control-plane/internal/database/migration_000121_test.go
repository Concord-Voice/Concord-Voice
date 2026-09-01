package database_test

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000121_ProfileStorageShape(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	insert := func(userID, slot, key string, tier int) error {
		_, err := db.Exec(`
			INSERT INTO media_files
				(uploader_id, file_type, media_tier, mime_type, file_size,
				 storage_key, profile_slot)
			VALUES ($1, 'photo', $2, 'image/png', 1, $3, $4)`,
			userID, tier, key, slot)
		return err
	}

	validLegacyUser := testhelpers.CreateUser(t, db).String()
	require.NoError(t, insert(validLegacyUser, "avatar", "avatars/"+validLegacyUser, 1))
	validGenerationUser := testhelpers.CreateUser(t, db).String()
	require.NoError(t, insert(validGenerationUser, "banner",
		"banners/"+validGenerationUser+"/01234567-89ab-cdef-0123-456789abcdef", 1))
	_, err := db.Exec(`
		INSERT INTO media_files
			(uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		VALUES ($1, 'photo', 1, 'image/png', 1, $2)`,
		validGenerationUser, "avatars/"+validGenerationUser+"/01234567-89ab-cdef-0123-456789abcdef")
	require.Error(t, err, "profile-shaped keys must carry their matching profile_slot")
	_, err = db.Exec(`INSERT INTO tier1_profile_upload_intents (storage_key, user_id, profile_slot)
		VALUES ($1, $2, 'avatar')`,
		"avatars/"+validGenerationUser+"/01234567-89ab-cdef-0123-456789abcdef", validGenerationUser)
	require.NoError(t, err)
	invalidIntent := func(userID, slot, key string) error {
		_, err := db.Exec(`INSERT INTO tier1_profile_upload_intents (storage_key, user_id, profile_slot)
			VALUES ($1, $2, $3)`, key, userID, slot)
		return err
	}
	require.Error(t, invalidIntent(validGenerationUser, "avatar",
		"banners/"+validGenerationUser+"/01234567-89ab-cdef-0123-456789abcdef"))
	require.Error(t, invalidIntent(validGenerationUser, "banner",
		"banners/"+validGenerationUser+"/01234567-89ab-cdef-0123-456789abcde"))

	for name, tc := range map[string]struct {
		key  string
		slot string
		tier int
	}{
		"wrong prefix":       {"avatars/" + validGenerationUser + "/01234567-89ab-cdef-0123-456789abcdef", "banner", 1},
		"missing generation": {"avatars/" + validGenerationUser + "/", "avatar", 1},
		"wrong tier":         {"avatars/" + validGenerationUser + "/01234567-89ab-cdef-0123-456789abcdef", "avatar", 2},
	} {
		t.Run(name, func(t *testing.T) {
			userID := testhelpers.CreateUser(t, db).String()
			require.Error(t, insert(userID, tc.slot, tc.key, tc.tier))
		})
	}
}

func TestMigration000122_DownRestoresUnvalidatedGuards(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000122_validate_tier1_profile_storage_shape.down.sql"))
	require.NoError(t, err)
	for _, constraint := range []string{
		"media_files_tier1_profile_storage_shape_check",
		"tier1_profile_upload_intents_immutable_key_shape_check",
	} {
		var validated bool
		require.NoError(t, tx.QueryRow(`
			SELECT convalidated FROM pg_constraint WHERE conname = $1`, constraint).Scan(&validated))
		require.False(t, validated, "%s must be NOT VALID after 000122 down", constraint)
	}
}

func TestMigration000126_BackfillsSoftDeletedLegacyProfileRows(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db).String()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// Recreate the pre-125 state: 000123's NULL expression is deliberately
	// compatible with this historical row, while 000125 is not yet installed.
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000124_validate_profile_key_slot_backlink.down.sql"))
	require.NoError(t, err)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000125_reject_unclassified_profile_keys.down.sql"))
	require.NoError(t, err)

	key := "avatars/" + userID
	_, err = tx.Exec(`
		INSERT INTO media_files
			(uploader_id, file_type, media_tier, mime_type, file_size,
			 storage_key, deleted_at)
		VALUES ($1, 'photo', 1, 'image/png', 1, $2, NOW())`, userID, key)
	require.NoError(t, err)

	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000125_reject_unclassified_profile_keys.up.sql"))
	require.NoError(t, err)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000126_validate_profile_key_requires_slot.up.sql"))
	require.NoError(t, err)

	var slot string
	require.NoError(t, tx.QueryRow(`SELECT profile_slot FROM media_files WHERE storage_key = $1`, key).Scan(&slot))
	require.Equal(t, "avatar", slot)
	var validated bool
	require.NoError(t, tx.QueryRow(`
		SELECT convalidated FROM pg_constraint
		WHERE conname = 'media_files_profile_key_requires_slot_check'`).Scan(&validated))
	require.True(t, validated)
}

func TestMigration000119DownRefusesPendingEvidence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE tier1_profile_clear_delete_obligations (
		user_id UUID NOT NULL, storage_key VARCHAR(500) NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		reconcile_after TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
		created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
		PRIMARY KEY (user_id, storage_key))`)
	require.NoError(t, err)
	userID := testhelpers.CreateUser(t, db).String()
	_, err = tx.Exec(`INSERT INTO tier1_profile_clear_delete_obligations (user_id, storage_key) VALUES ($1, $2)`, userID, "avatars/"+userID)
	require.NoError(t, err)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000119_tier1_profile_clear_delete_obligations.down.sql"))
	require.Error(t, err, "000119 down must retain unresolved profile-clear evidence")
	assert.Contains(t, err.Error(), "unresolved profile-clear obligations")
}

func TestProfileStorageRollbackChainGuardsAndReapplies(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	userID := testhelpers.CreateUser(t, db).String()
	key := "avatars/" + userID + "/01234567-89ab-cdef-0123-456789abcdef"
	_, err = tx.Exec(`INSERT INTO tier1_profile_upload_intents (storage_key, user_id, profile_slot) VALUES ($1, $2, 'avatar')`, key, userID)
	require.NoError(t, err)
	// Walk the additive guards back to 000120, then prove its refusal path.
	for _, migration := range []string{
		"000126_validate_profile_key_requires_slot.down.sql",
		"000125_reject_unclassified_profile_keys.down.sql",
		"000124_validate_profile_key_slot_backlink.down.sql",
		"000123_guard_profile_key_slot_backlink.down.sql",
		"000122_validate_tier1_profile_storage_shape.down.sql",
		"000121_guard_tier1_profile_storage_shape.down.sql",
	} {
		_, err = tx.Exec(migrationReadFile(t, "../../migrations/"+migration))
		require.NoError(t, err, migration)
	}
	_, err = tx.Exec(`SAVEPOINT rollback_guard`)
	require.NoError(t, err)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000120_tier1_profile_upload_intents.down.sql"))
	require.Error(t, err, "000120 down must refuse a live upload intent")
	assert.Contains(t, err.Error(), "profile upload intents")
	_, err = tx.Exec(`ROLLBACK TO SAVEPOINT rollback_guard`)
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM tier1_profile_upload_intents WHERE storage_key = $1`, key)
	require.NoError(t, err)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000120_tier1_profile_upload_intents.down.sql"))
	require.NoError(t, err)
	var exists bool
	require.NoError(t, tx.QueryRow(`SELECT to_regclass('public.tier1_profile_clear_delete_obligations') IS NOT NULL`).Scan(&exists))
	assert.True(t, exists, "clean 000120 down must restore the 000119 table")
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000120_tier1_profile_upload_intents.up.sql"))
	require.NoError(t, err, "the guarded rollback state must reapply 000120")
}
