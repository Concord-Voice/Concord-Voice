package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestMigrateProfileRowCommitsObjectMetadataAndCanonicalURL(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := uuid.New().String()
	originalURL := "data:image/png;base64,AAAA"
	key := path.Join("avatars", userID)
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "migrate"+userID[:8], "test", originalURL)
	require.NoError(t, err)
	stats := migrationStats{}
	puts := 0
	m := &migrator{db: db, bucket: "test", putObject: func(_ context.Context, _, _ string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
		puts++
		assert.Equal(t, int64(3), size)
		assert.Equal(t, "image/png", opts.ContentType)
		_, err := io.ReadAll(r)
		return minio.UploadInfo{}, err
	}}
	opts := migrateOpts{column: "avatar_url", keyPrefix: "avatars", proxyPath: "/api/v1/media/avatars"}
	require.NoError(t, m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte{1, 2, 3}, "image/png", opts, &stats))
	assert.Equal(t, 1, puts)
	assert.Equal(t, 1, stats.migrated)
	var url string
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, userID).Scan(&url))
	assert.Equal(t, "/api/v1/media/avatars/"+userID, url)
	var mime string
	var size int64
	var backend sql.NullString
	var profileSlot string
	require.NoError(t, db.QueryRow(`SELECT mime_type, file_size, storage_backend, profile_slot FROM media_files WHERE storage_key = $1`, key).Scan(&mime, &size, &backend, &profileSlot))
	assert.Equal(t, "image/png", mime)
	assert.Equal(t, int64(3), size)
	assert.False(t, backend.Valid)
	assert.Equal(t, "avatar", profileSlot)
}

func TestMigrateProfileRowPutFailureRollsBackMetadataAndURL(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := uuid.New().String()
	originalURL := "data:image/png;base64,AAAA"
	key := path.Join("avatars", userID)
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "putfail"+userID[:8], "test", originalURL)
	require.NoError(t, err)
	stats := migrationStats{}
	m := &migrator{db: db, bucket: "test", putObject: func(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error) {
		return minio.UploadInfo{}, errors.New("storage unavailable")
	}}
	opts := migrateOpts{column: "avatar_url", keyPrefix: "avatars", proxyPath: "/api/v1/media/avatars"}
	assert.Error(t, m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte{1, 2, 3}, "image/png", opts, &stats))
	assert.Equal(t, 1, stats.errored)
	var url string
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, userID).Scan(&url))
	assert.Equal(t, originalURL, url)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1`, key).Scan(&count))
	assert.Zero(t, count)
}

func TestMigrateProfileRowURLUpdateFailureRollsBackAndRetries(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := uuid.New().String()
	originalURL := "data:image/png;base64,AAAA"
	key := path.Join("avatars", userID, uuid.New().String())
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "updatefail"+userID[:8], "test", originalURL)
	require.NoError(t, err)

	dropTrigger := func() error {
		_, dropErr := db.Exec(`
			DROP TRIGGER IF EXISTS test_migrate_url_update_failure_trg ON users;
			DROP FUNCTION IF EXISTS test_migrate_url_update_failure()`)
		return dropErr
	}
	t.Cleanup(func() { require.NoError(t, dropTrigger()) })
	_, err = db.Exec(`
		CREATE FUNCTION test_migrate_url_update_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'simulated profile URL update failure';
		END $$;
		CREATE TRIGGER test_migrate_url_update_failure_trg BEFORE UPDATE OF avatar_url ON users
			FOR EACH ROW EXECUTE FUNCTION test_migrate_url_update_failure()`)
	require.NoError(t, err)

	puts := 0
	m := &migrator{db: db, bucket: "test", putObject: func(_ context.Context, _, objectKey string, _ io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
		puts++
		assert.Equal(t, key, objectKey)
		return minio.UploadInfo{}, nil
	}}
	opts := migrateOpts{column: "avatar_url", keyPrefix: "avatars", proxyPath: "/api/v1/media/avatars"}
	stats := migrationStats{}
	err = m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte("data"), "image/png", opts, &stats)
	require.ErrorContains(t, err, "update profile migration URL")
	assert.Equal(t, 1, puts)
	assert.Equal(t, 1, stats.errored)
	assert.Equal(t, originalURL, queryProfileURL(t, db, userID))
	assert.Equal(t, 0, countLiveProfileMetadata(t, db, key))

	require.NoError(t, dropTrigger())
	stats = migrationStats{}
	require.NoError(t, m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte("data"), "image/png", opts, &stats))
	assert.Equal(t, 2, puts)
	assert.Equal(t, 1, stats.migrated)
	assert.Equal(t, "/api/v1/media/avatars/"+userID, queryProfileURL(t, db, userID))
	assert.Equal(t, 1, countLiveProfileMetadata(t, db, key))
}

func TestMigrateProfileRowCommitFailureRollsBackAndRetries(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := uuid.New().String()
	originalURL := "data:image/png;base64,AAAA"
	key := path.Join("avatars", userID, uuid.New().String())
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "commitfail"+userID[:8], "test", originalURL)
	require.NoError(t, err)

	dropTrigger := func() error {
		_, dropErr := db.Exec(`
			DROP TRIGGER IF EXISTS test_migrate_commit_failure_trg ON users;
			DROP FUNCTION IF EXISTS test_migrate_commit_failure()`)
		return dropErr
	}
	t.Cleanup(func() { require.NoError(t, dropTrigger()) })
	_, err = db.Exec(`
		CREATE FUNCTION test_migrate_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'simulated profile migration commit failure';
		END $$;
		CREATE CONSTRAINT TRIGGER test_migrate_commit_failure_trg AFTER UPDATE OF avatar_url ON users
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION test_migrate_commit_failure()`)
	require.NoError(t, err)

	puts := 0
	m := &migrator{db: db, bucket: "test", putObject: func(_ context.Context, _, objectKey string, _ io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
		puts++
		assert.Equal(t, key, objectKey)
		return minio.UploadInfo{}, nil
	}}
	opts := migrateOpts{column: "avatar_url", keyPrefix: "avatars", proxyPath: "/api/v1/media/avatars"}
	stats := migrationStats{}
	err = m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte("data"), "image/png", opts, &stats)
	require.ErrorContains(t, err, "commit profile migration")
	assert.Equal(t, 1, puts)
	assert.Equal(t, 1, stats.errored)
	assert.Equal(t, originalURL, queryProfileURL(t, db, userID))
	assert.Equal(t, 0, countLiveProfileMetadata(t, db, key))

	require.NoError(t, dropTrigger())
	stats = migrationStats{}
	require.NoError(t, m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte("data"), "image/png", opts, &stats))
	assert.Equal(t, 2, puts)
	assert.Equal(t, 1, stats.migrated)
	assert.Equal(t, "/api/v1/media/avatars/"+userID, queryProfileURL(t, db, userID))
	assert.Equal(t, 1, countLiveProfileMetadata(t, db, key))
}

func queryProfileURL(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	var url string
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, userID).Scan(&url))
	return url
}

func countLiveProfileMetadata(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL`, key).Scan(&count))
	return count
}

func TestMigrateProfileRowExistingMetadataRejectsBeforePut(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := uuid.New().String()
	originalURL := "data:image/png;base64,AAAA"
	key := path.Join("avatars", userID)
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "existing"+userID[:8], "test", originalURL)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
		VALUES ($1, $2, 'photo', 1, 'image/jpeg', 9, $3, 'avatar')`, uuid.New().String(), userID, key)
	require.NoError(t, err)
	stats := migrationStats{}
	puts := 0
	m := &migrator{db: db, bucket: "test", putObject: func(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error) {
		puts++
		return minio.UploadInfo{}, nil
	}}
	opts := migrateOpts{column: "avatar_url", keyPrefix: "avatars", proxyPath: "/api/v1/media/avatars"}
	assert.Error(t, m.migrateProfileRow(context.Background(), userID, originalURL, key, []byte{1, 2, 3}, "image/png", opts, &stats))
	assert.Zero(t, puts)
	assert.Equal(t, 1, stats.errored)
	var url string
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, userID).Scan(&url))
	assert.Equal(t, originalURL, url)
	var mime string
	var size int64
	require.NoError(t, db.QueryRow(`SELECT mime_type, file_size FROM media_files WHERE storage_key = $1`, key).Scan(&mime, &size))
	assert.Equal(t, "image/jpeg", mime)
	assert.Equal(t, int64(9), size)
}

func TestMigrateBatchContinuesAfterProfileFailure(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	firstID := "00000000-0000-4000-8000-000000000001"
	secondID := "00000000-0000-4000-8000-000000000002"
	legacyURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	for _, row := range []struct {
		id       string
		username string
	}{
		{firstID, "starvation-first"},
		{secondID, "starvation-second"},
	} {
		_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
			VALUES ($1, $2, $3, $4, true, true, $5)`, row.id, row.id+"@test.local", row.username, "test", legacyURL)
		require.NoError(t, err)
	}
	putCalls := 0
	m := &migrator{
		db:        db,
		bucket:    "test",
		batchSize: 2,
		putObject: func(_ context.Context, _, objectKey string, _ io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
			putCalls++
			if objectKey == "avatars/"+firstID {
				return minio.UploadInfo{}, errors.New("first object unavailable")
			}
			return minio.UploadInfo{}, nil
		},
	}
	opts := migrateOpts{
		table: "users", column: "avatar_url", idColumn: "id", keyPrefix: "avatars",
		proxyPath: "/api/v1/media/avatars", maxW: 512, maxH: 512, outputPNG: true, profile: true,
	}
	stats := migrationStats{}
	_, err := m.migrateBatch(context.Background(), `SELECT id, avatar_url FROM users WHERE avatar_url IS NOT NULL AND avatar_url LIKE 'data:image/%' ORDER BY id LIMIT $1`, opts, &stats)
	assert.NoError(t, err, "later row starvation: one row failure must not abort the migration batch")
	assert.Equal(t, 2, putCalls, "later row starvation: migration must attempt the row after a transactional failure")
	assert.Equal(t, 2, stats.scanned, "later row starvation: both ordered rows must be scanned")
	assert.Equal(t, 1, stats.migrated)
	assert.Equal(t, 1, stats.errored)
	var firstURL, secondURL string
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, firstID).Scan(&firstURL))
	require.NoError(t, db.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, secondID).Scan(&secondURL))
	assert.Equal(t, legacyURL, firstURL)
	assert.Equal(t, "/api/v1/media/avatars/"+secondID, secondURL)
}

func TestMigrateBatchRejectsNonPositiveBatchSize(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()

	query := `SELECT '00000000-0000-4000-8000-000000000001'::uuid, 'not-used' LIMIT $1`
	for _, batchSize := range []int{0, -1} {
		t.Run("batch="+strconv.Itoa(batchSize), func(t *testing.T) {
			stats := migrationStats{}
			m := &migrator{db: db, batchSize: batchSize}
			_, err := m.migrateBatch(context.Background(), query, migrateOpts{}, &stats)
			require.Error(t, err)
			assert.Equal(t, 1, stats.errored)
		})
	}
}

func TestMigrateBatchWithCursorAdvancesAfterFailedRow(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()

	query := `SELECT id, data_url FROM (VALUES
		('00000000-0000-4000-8000-000000000001'::uuid, 'not-a-data-url'),
		('00000000-0000-4000-8000-000000000002'::uuid, 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==')
	) AS candidates(id, data_url)
	WHERE ($1::uuid IS NULL OR id > $1::uuid)
	ORDER BY id LIMIT $2`
	m := &migrator{db: db, batchSize: 1, dryRun: true}
	stats := migrationStats{}
	var cursor *uuid.UUID

	count, err := m.migrateBatchWithCursor(context.Background(), query, migrateOpts{}, &stats, &cursor)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	require.NotNil(t, cursor)
	assert.Equal(t, "00000000-0000-4000-8000-000000000001", cursor.String())

	count, err = m.migrateBatchWithCursor(context.Background(), query, migrateOpts{}, &stats, &cursor)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, "00000000-0000-4000-8000-000000000002", cursor.String())
	assert.Equal(t, 2, stats.scanned)
	assert.Equal(t, 1, stats.errored)
	assert.Equal(t, 1, stats.migrated)
}

func TestReadManifest(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name  string
		input string
		valid bool
	}{
		{"valid avatar and banner with final newline", "avatars/" + id + "\nbanners/" + id + "\n", true},
		{"empty", "", false},
		{"blank", "   \n", false},
		{"comment", "# avatars/" + id + "\n", false},
		{"bad prefix", "photos/" + id + "\n", false},
		{"noncanonical UUID", "avatars/11111111-1111-4111-8111-11111111111A\n", false},
		{"duplicate", "avatars/" + id + "\navatars/" + id + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.txt")
			require.NoError(t, os.WriteFile(path, []byte(tc.input), 0o600))
			entries, err := readManifest(path)
			if tc.valid {
				require.NoError(t, err)
				require.Len(t, entries, 2)
				assert.Equal(t, "avatar_url", entries[0].column)
				assert.Equal(t, "/api/v1/media/banners/"+id, entries[1].proxyURL)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestRepairProfileMetadata(t *testing.T) {
	cases := []struct {
		name          string
		seedURL       bool
		seedMedia     bool
		seedTombstone bool
		dryRun        bool
		statErr       bool
		wantRepaired  bool
		wantStat      int
	}{
		{"matching canonical reference creates metadata", true, false, false, false, false, true, 1},
		{"wrong reference does not stat or write", false, false, false, false, false, false, 0},
		{"tombstone does not stat or write", true, false, true, false, false, false, 0},
		{"existing owned tier one is idempotent", true, true, false, false, false, true, 0},
		{"dry run stats without writing", true, false, false, true, false, true, 1},
		{"missing object does not write", true, false, false, false, true, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := testdb.SetupTestDB(t)
			defer cleanup()
			userID := uuid.New().String()
			key := path.Join("avatars", userID)
			proxyURL := "/api/v1/media/avatars/" + userID
			_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
				VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "manifest"+strings.ReplaceAll(userID[:8], "-", ""), "test", func() interface{} {
				if tc.seedURL {
					return proxyURL
				}
				return nil
			}())
			require.NoError(t, err)
			if tc.seedMedia {
				_, err = db.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot) VALUES ($1, $2, 'photo', 1, 'image/jpeg', 9, $3, 'avatar')`, uuid.New().String(), userID, key)
				require.NoError(t, err)
			}
			if tc.seedTombstone {
				_, err = db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
				require.NoError(t, err)
			}
			statCalls := 0
			m := &migrator{db: db, bucket: "test", dryRun: tc.dryRun, statObject: func(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error) {
				statCalls++
				if tc.statErr {
					return minio.ObjectInfo{}, assert.AnError
				}
				return minio.ObjectInfo{ContentType: "image/png", Size: 42}, nil
			}}
			entry, ok, err := parseManifestLine(key)
			require.NoError(t, err)
			require.True(t, ok)
			repaired, repairErr := m.repairProfileMetadata(context.Background(), entry)
			if tc.wantRepaired {
				assert.NoError(t, repairErr)
				assert.True(t, repaired)
				if tc.name == "matching canonical reference creates metadata" {
					repairedAgain, errAgain := m.repairProfileMetadata(context.Background(), entry)
					assert.NoError(t, errAgain)
					assert.True(t, repairedAgain)
				}
			} else {
				assert.Error(t, repairErr)
				assert.False(t, repaired)
			}
			assert.Equal(t, tc.wantStat, statCalls)
			var count int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key = $1`, key).Scan(&count))
			if tc.seedMedia {
				assert.Equal(t, 1, count)
				var mime string
				var size int64
				require.NoError(t, db.QueryRow(`SELECT mime_type, file_size FROM media_files WHERE storage_key = $1`, key).Scan(&mime, &size))
				assert.Equal(t, "image/jpeg", mime, "existing metadata must not be overwritten")
				assert.Equal(t, int64(9), size, "existing metadata must not be overwritten")
			} else if tc.dryRun || !tc.wantRepaired {
				assert.Zero(t, count)
			} else {
				assert.Equal(t, 1, count)
				var mime string
				var size int64
				var backend sql.NullString
				var profileSlot string
				require.NoError(t, db.QueryRow(`SELECT mime_type, file_size, storage_backend, profile_slot FROM media_files WHERE storage_key = $1`, key).Scan(&mime, &size, &backend, &profileSlot))
				assert.Equal(t, "image/png", mime)
				assert.Equal(t, int64(42), size)
				assert.False(t, backend.Valid)
				assert.Equal(t, "avatar", profileSlot)
			}
		})
	}
}

func TestRepairProfileMetadataBannerUsesStatMIMEAndSize(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := uuid.New().String()
	key := path.Join("banners", userID)
	proxyURL := "/api/v1/media/banners/" + userID
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, header_image_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "banner"+userID[:8], "test", proxyURL)
	require.NoError(t, err)
	statCalls := 0
	m := &migrator{db: db, bucket: "test", statObject: func(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error) {
		statCalls++
		return minio.ObjectInfo{ContentType: "image/jpeg", Size: 73}, nil
	}}
	entry, ok, err := parseManifestLine(key)
	require.NoError(t, err)
	require.True(t, ok)
	repaired, err := m.repairProfileMetadata(context.Background(), entry)
	require.NoError(t, err)
	assert.True(t, repaired)
	assert.Equal(t, 1, statCalls)
	var mime string
	var size int64
	var backend sql.NullString
	var profileSlot string
	require.NoError(t, db.QueryRow(`SELECT mime_type, file_size, storage_backend, profile_slot FROM media_files WHERE storage_key = $1`, key).Scan(&mime, &size, &backend, &profileSlot))
	assert.Equal(t, "image/jpeg", mime)
	assert.Equal(t, int64(73), size)
	assert.False(t, backend.Valid)
	assert.Equal(t, "banner", profileSlot)
}

func TestMigrateProfileRowRejectsUnsupportedPrefixBeforeOpeningTransaction(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	stats := migrationStats{}
	err = (&migrator{db: db}).migrateProfileRow(context.Background(), uuid.NewString(), "old", "files/key", []byte("data"), "image/png", migrateOpts{keyPrefix: "files"}, &stats)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported profile key prefix")
	assert.Equal(t, 1, stats.errored)
}

func TestProfileMigrationOperationsReportClosedDatabase(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	m := &migrator{db: db}
	stats := migrationStats{}
	err = m.migrateProfileRow(context.Background(), uuid.NewString(), "old", "avatars/key", []byte("data"), "image/png", migrateOpts{keyPrefix: "avatars"}, &stats)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "begin profile migration transaction")
	assert.Equal(t, 1, stats.errored)

	repaired, err := m.repairProfileMetadata(context.Background(), manifestEntry{key: "avatars/" + uuid.NewString(), userID: uuid.NewString(), column: "avatar_url", profileSlot: "avatar"})
	assert.False(t, repaired)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "begin profile metadata repair")
}

func TestRepairManifestAggregatesEntryFailures(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	m := &migrator{db: db}
	entries := []manifestEntry{{key: "avatars/" + uuid.NewString()}, {key: "banners/" + uuid.NewString()}}
	stats, err := m.repairManifest(context.Background(), entries)
	require.Error(t, err)
	assert.Equal(t, migrationStats{scanned: 2, errored: 2}, stats)
}

func TestReadManifestMissingFileReturnsOpenError(t *testing.T) {
	entries, err := readManifest(filepath.Join(t.TempDir(), "missing.manifest"))
	assert.Nil(t, entries)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open manifest")
}

func TestRepairProfileMetadataRejectsInvalidObjectMetadata(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()
	userID := uuid.NewString()
	key := path.Join("avatars", userID)
	proxyURL := "/api/v1/media/avatars/" + userID
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified, avatar_url)
		VALUES ($1, $2, $3, $4, true, true, $5)`, userID, userID+"@test.local", "invalidmeta"+userID[:8], "test", proxyURL)
	require.NoError(t, err)
	m := &migrator{db: db, bucket: "test", statObject: func(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error) {
		return minio.ObjectInfo{ContentType: "image/jpeg", Size: 0}, nil
	}}
	entry, ok, err := parseManifestLine(key)
	require.NoError(t, err)
	require.True(t, ok)
	repaired, err := m.repairProfileMetadata(context.Background(), entry)
	assert.False(t, repaired)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid profile object metadata")
}
