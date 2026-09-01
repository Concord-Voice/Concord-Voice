// Package main provides a one-time migration tool to move image assets
// from PostgreSQL base64 data URLs to MinIO object storage.
//
// Usage:
//
//	migrate-media [-dry-run] [-batch=100]
//	migrate-media --manifest legacy-profile-media.txt
//
// The tool extracts base64-encoded images from users.avatar_url,
// users.header_image_url, servers.icon_url, and servers.banner_url,
// decodes and processes them (resize + re-encode), uploads to MinIO,
// and updates the database column to the new proxy URL.
//
// It is idempotent: rows already migrated (URLs starting with /api/v1/media/)
// are skipped. Safe to run multiple times.
//
// Environment variables: DATABASE_URL, MINIO_ENDPOINT, MINIO_ACCESS_KEY,
// MINIO_SECRET_KEY, MINIO_BUCKET (same as control-plane).
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	_ "github.com/joho/godotenv/autoload" // auto-load .env into process environment on import
	_ "github.com/lib/pq"                 // register postgres driver for database/sql

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

type migrationStats struct {
	scanned  int
	migrated int
	errored  int
}

func main() {
	dryRun := flag.Bool("dry-run", false, "Preview what would be migrated without making changes")
	batchSize := flag.Int("batch", 100, "Number of rows to process per query batch")
	manifestPath := flag.String("manifest", "", "Operator-attested legacy avatar/banner keys to repair")
	flag.Parse()
	if *batchSize <= 0 {
		log.Fatalf("Batch size must be greater than zero")
	}
	var manifest []manifestEntry
	if *manifestPath != "" {
		var manifestErr error
		manifest, manifestErr = readManifest(*manifestPath)
		if manifestErr != nil {
			log.Fatalf("Invalid legacy profile metadata manifest")
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	mc, err := minio.New(cfg.StorageEndpoint, &minio.Options{
		// NewStaticV4's 3rd arg is the STS session token, not the region — pass "".
		// Region is supplied via Options.Region only (#1611 Gitar review).
		Creds:  credentials.NewStaticV4(cfg.StorageAccessKey, cfg.StorageSecretKey, ""),
		Secure: cfg.StorageUseSSL,
		Region: cfg.StorageRegion,
	})
	if err != nil {
		log.Fatalf("Failed to create MinIO client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	mode := "LIVE"
	if *dryRun {
		mode = "DRY RUN"
	}
	log.Printf("=== Media Migration (%s) ===", mode)
	log.Printf("Database: %s", maskDSN(cfg.DatabaseURL))
	log.Printf("MinIO:    %s/%s", cfg.StorageEndpoint, cfg.StorageBucket)
	log.Println()
	if *manifestPath != "" {
		exists, bucketErr := mc.BucketExists(ctx, cfg.StorageBucket)
		if bucketErr != nil {
			log.Fatalf("Failed to check legacy profile metadata bucket")
		}
		if !exists {
			log.Fatalf("Legacy profile metadata bucket does not exist")
		}
		m := &migrator{
			db:         db,
			mc:         mc,
			bucket:     cfg.StorageBucket,
			dryRun:     *dryRun,
			statObject: mc.StatObject,
			putObject:  mc.PutObject,
		}
		stats, repairErr := m.repairManifest(ctx, manifest)
		if repairErr != nil {
			log.Fatalf("Legacy profile metadata repair failed")
		}
		log.Printf("=== Legacy Profile Metadata Repair Complete (%s): %d scanned, %d repaired, %d errors ===", mode, stats.scanned, stats.migrated, stats.errored)
		return
	}

	// The regular data-URL migration can create the bucket. Manifest repair is
	// deliberately storage-read-only apart from database metadata and must not.
	exists, err := mc.BucketExists(ctx, cfg.StorageBucket)
	if err != nil {
		log.Fatalf("Failed to check bucket: %v", err)
	}
	if !exists {
		if err := mc.MakeBucket(ctx, cfg.StorageBucket, minio.MakeBucketOptions{}); err != nil {
			log.Fatalf("Failed to create bucket: %v", err)
		}
		log.Printf("Created bucket: %s", cfg.StorageBucket)
	}

	total := &migrationStats{}

	// Migrate user avatars
	log.Println("--- User Avatars ---")
	stats := migrateColumn(ctx, db, mc, cfg.StorageBucket, *dryRun, *batchSize, migrateOpts{
		table:     "users",
		column:    "avatar_url",
		idColumn:  "id",
		keyPrefix: "avatars",
		proxyPath: "/api/v1/media/avatars",
		maxW:      media.AvatarMaxDim,
		maxH:      media.AvatarMaxDim,
		outputPNG: true,
		profile:   true,
	})
	total.add(stats)

	// Migrate user banners
	log.Println("--- User Banners ---")
	stats = migrateColumn(ctx, db, mc, cfg.StorageBucket, *dryRun, *batchSize, migrateOpts{
		table:     "users",
		column:    "header_image_url",
		idColumn:  "id",
		keyPrefix: "banners",
		proxyPath: "/api/v1/media/banners",
		maxW:      media.BannerMaxW,
		maxH:      media.BannerMaxH,
		outputPNG: false,
		profile:   true,
	})
	total.add(stats)

	// Migrate server icons
	log.Println("--- Server Icons ---")
	stats = migrateColumn(ctx, db, mc, cfg.StorageBucket, *dryRun, *batchSize, migrateOpts{
		table:     "servers",
		column:    "icon_url",
		idColumn:  "id",
		keyPrefix: "server-icons",
		proxyPath: "/api/v1/media/server-icons",
		maxW:      media.IconMaxDim,
		maxH:      media.IconMaxDim,
		outputPNG: true,
	})
	total.add(stats)

	// Migrate server banners
	log.Println("--- Server Banners ---")
	stats = migrateColumn(ctx, db, mc, cfg.StorageBucket, *dryRun, *batchSize, migrateOpts{
		table:     "servers",
		column:    "banner_url",
		idColumn:  "id",
		keyPrefix: "server-banners",
		proxyPath: "/api/v1/media/server-banners",
		maxW:      media.BannerMaxW,
		maxH:      media.BannerMaxH,
		outputPNG: false,
	})
	total.add(stats)

	log.Println()
	log.Printf("  Scanned:  %d", total.scanned)
	log.Printf("  Migrated: %d", total.migrated)
	log.Printf("  Errors:   %d", total.errored)
	if total.errored > 0 {
		log.Fatalf("=== Migration Incomplete (%s) ===", mode)
	}
	log.Printf("=== Migration Complete (%s) ===", mode)
}

type migrateOpts struct {
	table     string // "users" or "servers"
	column    string // "avatar_url", "header_image_url", etc.
	idColumn  string // "id"
	keyPrefix string // "avatars", "banners", etc.
	proxyPath string // "/api/v1/media/avatars", etc.
	maxW      int
	maxH      int
	outputPNG bool // true for avatars/icons (transparency), false for banners (JPEG)
	profile   bool // user avatar/banner: metadata, PUT, and profile URL commit together
}

type statObjectFunc func(context.Context, string, string, minio.StatObjectOptions) (minio.ObjectInfo, error)
type putObjectFunc func(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error)

type migrator struct {
	db         *sql.DB
	mc         *minio.Client
	bucket     string
	dryRun     bool
	batchSize  int
	statObject statObjectFunc
	putObject  putObjectFunc
}

func migrateColumn(ctx context.Context, db *sql.DB, mc *minio.Client, bucket string, dryRun bool, batchSize int, opts migrateOpts) migrationStats {
	m := &migrator{db: db, mc: mc, bucket: bucket, dryRun: dryRun, batchSize: batchSize, statObject: mc.StatObject, putObject: mc.PutObject}
	var stats migrationStats
	var cursor *uuid.UUID

	// Query rows with base64 data URLs (skip already-migrated and NULL)
	// #nosec G201 -- table/column names are hardcoded constants, not user input
	query := fmt.Sprintf( // nosemgrep:concord-go-sql-sprintf
		`SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND %s LIKE 'data:image/%%' AND ($1::uuid IS NULL OR %s > $1::uuid) ORDER BY %s LIMIT $2`,
		opts.idColumn, opts.column, opts.table, opts.column, opts.column, opts.idColumn,
		opts.idColumn,
	)

	for {
		batchCount, err := m.migrateBatchWithCursor(ctx, query, opts, &stats, &cursor)
		if err != nil {
			return stats
		}

		if batchCount == 0 {
			break
		}
	}

	log.Printf("  Subtotal: %d scanned, %d migrated, %d errors", stats.scanned, stats.migrated, stats.errored)
	return stats
}

func (m *migrator) migrateBatch(ctx context.Context, query string, opts migrateOpts, stats *migrationStats) (int, error) {
	return m.migrateBatchWithCursor(ctx, query, opts, stats, nil)
}

func (m *migrator) migrateBatchWithCursor(ctx context.Context, query string, opts migrateOpts, stats *migrationStats, cursor **uuid.UUID) (int, error) {
	if m.batchSize <= 0 {
		stats.errored++
		return 0, errors.New("migration batch size must be greater than zero")
	}
	args := []any{m.batchSize}
	if cursor != nil {
		var after any
		if *cursor != nil {
			after = **cursor
		}
		args = []any{after, m.batchSize}
	}
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("  ERROR: query failed: %v", err)
		stats.errored++
		return 0, fmt.Errorf("query migration batch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	batchCount := 0
	for rows.Next() {
		var id uuid.UUID
		var dataURL string
		if err := rows.Scan(&id, &dataURL); err != nil {
			log.Printf("  ERROR: scan failed: %v", err)
			stats.errored++
			return batchCount, fmt.Errorf("scan migration row: %w", err)
		}
		if cursor != nil {
			next := id
			*cursor = &next
		}
		stats.scanned++
		batchCount++

		if rowErr := m.migrateRow(ctx, id.String(), dataURL, opts, stats); rowErr != nil {
			continue
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("  ERROR: row iteration failed: %v", err)
		stats.errored++
		return batchCount, fmt.Errorf("iterate migration rows: %w", err)
	}

	return batchCount, nil
}

func (m *migrator) migrateRow(ctx context.Context, id, dataURL string, opts migrateOpts, stats *migrationStats) error {
	imgBytes, contentType, err := decodeDataURL(dataURL)
	if err != nil {
		log.Printf("  ERROR [%s %s]: failed to decode data URL: %v", opts.table, id, err)
		stats.errored++
		return nil
	}

	if m.dryRun {
		log.Printf("  WOULD MIGRATE [%s %s]: %s (%d bytes base64 → ~%d bytes raw)",
			opts.table, id, contentType, len(dataURL), len(imgBytes))
		stats.migrated++
		return nil
	}

	processedData, processedCT, procErr := processForMigration(imgBytes, opts.maxW, opts.maxH, opts.outputPNG)
	if procErr != nil {
		log.Printf("  ERROR [%s %s]: failed to process image: %v", opts.table, id, procErr)
		stats.errored++
		return nil
	}

	objectKey := fmt.Sprintf("%s/%s", opts.keyPrefix, id)
	if opts.profile {
		return m.migrateProfileRow(ctx, id, dataURL, objectKey, processedData, processedCT, opts, stats)
	}
	_, err = m.mc.PutObject(ctx, m.bucket, objectKey, bytes.NewReader(processedData),
		int64(len(processedData)), minio.PutObjectOptions{ContentType: processedCT})
	if err != nil {
		log.Printf("  ERROR [%s %s]: failed to upload to MinIO: %v", opts.table, id, err)
		stats.errored++
		return nil
	}

	newURL := fmt.Sprintf("%s/%s", opts.proxyPath, id)
	// #nosec G201 -- table/column names are hardcoded constants
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query — table/column identifiers come from hardcoded migration opts, values are parameterized
	updateQuery := fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE %s = $2`, opts.table, opts.column, opts.idColumn) // nosemgrep:concord-go-sql-sprintf
	if _, err := m.db.ExecContext(ctx, updateQuery, newURL, id); err != nil {
		log.Printf("  ERROR [%s %s]: failed to update DB: %v", opts.table, id, err)
		stats.errored++
		return nil
	}

	log.Printf("  MIGRATED [%s %s]: %d bytes base64 → %d bytes %s → %s",
		opts.table, id, len(dataURL), len(processedData), processedCT, newURL)
	stats.migrated++
	return nil
}

const profileTier1TombstoneExistsQuery = `
	SELECT EXISTS (
		SELECT 1 FROM tier1_erasure_delete_obligations WHERE storage_key = $1
	)
`

const insertProfileTier1MetadataQuery = `
	INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
	VALUES ($1, $2, 'photo', 1, $3, $4, $5, $6)
	ON CONFLICT (storage_key) WHERE deleted_at IS NULL DO NOTHING
`

const profileMigrationTimeout = 10 * time.Second

func profileSlotForPrefix(prefix string) (string, error) {
	switch prefix {
	case "avatars":
		return "avatar", nil
	case "banners":
		return "banner", nil
	default:
		return "", fmt.Errorf("unsupported profile key prefix %q", prefix)
	}
}

// migrateProfileRow keeps the object, Tier 1 metadata, and canonical URL in
// one transaction. A failed PUT therefore cannot leave a publishable row.
func (m *migrator) migrateProfileRow(ctx context.Context, id, originalURL, objectKey string, processedData []byte, processedCT string, opts migrateOpts, stats *migrationStats) error {
	profileSlot, err := profileSlotForPrefix(opts.keyPrefix)
	if err != nil {
		log.Printf("  ERROR: unsupported profile migration key prefix")
		stats.errored++
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, profileMigrationTimeout)
	defer cancel()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("  ERROR: failed to begin profile migration transaction")
		stats.errored++
		return errors.New("begin profile migration transaction")
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				log.Printf("  ERROR: failed to roll back profile migration transaction")
			}
		}
	}()

	// opts.column is selected only from the two hard-coded profile migration
	// options above; every value remains a parameter.
	// #nosec G201 -- opts.column is selected from hard-coded profile migration options.
	lockQuery := fmt.Sprintf( // nosemgrep:concord-go-sql-sprintf
		`SELECT id FROM users WHERE id = $1 AND %s = $2 FOR NO KEY UPDATE`, opts.column,
	)
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, lockQuery, id, originalURL).Scan(&lockedUserID); err != nil {
		log.Printf("  ERROR: failed to lock profile migration user")
		stats.errored++
		return errors.New("profile migration URL no longer matches selected data URL")
	}

	var tombstoned bool
	if err := tx.QueryRowContext(ctx, profileTier1TombstoneExistsQuery, objectKey).Scan(&tombstoned); err != nil {
		log.Printf("  ERROR: failed to check profile erasure tombstone")
		stats.errored++
		return errors.New("check profile migration erasure tombstone")
	}
	if tombstoned {
		log.Printf("  ERROR: profile migration key is pending erasure")
		stats.errored++
		return errors.New("profile migration key is pending erasure")
	}

	result, err := tx.ExecContext(ctx, insertProfileTier1MetadataQuery,
		uuid.New().String(), lockedUserID, processedCT, len(processedData), objectKey, profileSlot,
	)
	if err != nil {
		log.Printf("  ERROR: failed to insert profile migration metadata")
		stats.errored++
		return errors.New("insert profile migration metadata")
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		log.Printf("  ERROR: failed to confirm profile migration metadata")
		stats.errored++
		return errors.New("confirm profile migration metadata")
	}
	if inserted != 1 {
		log.Printf("  ERROR: profile migration key has conflicting live metadata")
		stats.errored++
		return errors.New("conflicting live profile migration metadata")
	}

	putObject := m.putObject
	if putObject == nil {
		putObject = m.mc.PutObject
	}
	if _, err := putObject(ctx, m.bucket, objectKey, bytes.NewReader(processedData),
		int64(len(processedData)), minio.PutObjectOptions{ContentType: processedCT}); err != nil {
		log.Printf("  ERROR: failed to store profile migration image")
		stats.errored++
		return errors.New("store profile migration image")
	}

	newURL := fmt.Sprintf("%s/%s", opts.proxyPath, id)
	// #nosec G201 -- opts.column is selected from hard-coded profile migration options.
	updateQuery := fmt.Sprintf( // nosemgrep:concord-go-sql-sprintf
		`UPDATE users SET %s = $1 WHERE id = $2 AND %s = $3`, opts.column, opts.column,
	)
	result, err = tx.ExecContext(ctx, updateQuery, newURL, id, originalURL)
	if err != nil {
		log.Printf("  ERROR: failed to update profile migration URL")
		stats.errored++
		return errors.New("update profile migration URL")
	}
	updated, err := result.RowsAffected()
	if err != nil {
		log.Printf("  ERROR: failed to confirm profile migration URL")
		stats.errored++
		return errors.New("confirm profile migration URL")
	}
	if updated != 1 {
		log.Printf("  ERROR: profile migration URL changed before update")
		stats.errored++
		return errors.New("profile migration URL changed before update")
	}
	if err := tx.Commit(); err != nil {
		log.Printf("  ERROR: failed to commit profile migration")
		stats.errored++
		return errors.New("commit profile migration")
	}
	committed = true
	stats.migrated++
	return nil
}

type manifestEntry struct {
	key          string
	userID       string
	column       string
	profileSlot  string
	proxyURL     string
	expectedMIME string
}

// repairManifest repairs only operator-attested legacy objects. A profile URL
// or a storage object alone is deliberately never publication authority.
func (m *migrator) repairManifest(ctx context.Context, entries []manifestEntry) (migrationStats, error) {
	var stats migrationStats
	failed := false
	for _, entry := range entries {
		stats.scanned++
		repaired, repairErr := m.repairProfileMetadata(ctx, entry)
		if repairErr != nil {
			log.Printf("  ERROR: failed to repair manifest metadata")
			stats.errored++
			failed = true
			continue
		}
		if repaired {
			stats.migrated++
		}
	}
	if failed {
		return stats, errors.New("one or more manifest entries could not be repaired")
	}
	return stats, nil
}

func readManifest(path string) (entries []manifestEntry, err error) {
	// #nosec G304 -- operator explicitly supplies the manifest path.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close manifest: %w", closeErr)
		}
	}()

	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		entry, ok, parseErr := parseManifestLine(scanner.Text())
		if parseErr != nil {
			return nil, fmt.Errorf("manifest line %d: %w", line, parseErr)
		}
		if !ok {
			continue
		}
		if _, exists := seen[entry.key]; exists {
			return nil, fmt.Errorf("manifest line %d: duplicate entry", line)
		}
		seen[entry.key] = struct{}{}
		entries = append(entries, entry)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, fmt.Errorf("read manifest: %w", scanErr)
	}
	if len(entries) == 0 {
		return nil, errors.New("manifest must contain at least one entry")
	}
	return entries, nil
}

func parseManifestLine(line string) (manifestEntry, bool, error) {
	if line == "" || strings.TrimSpace(line) != line {
		return manifestEntry{}, false, errors.New("entry must be exact avatars/<uuid> or banners/<uuid>")
	}
	parts := strings.Split(line, "/")
	if len(parts) != 2 || (parts[0] != "avatars" && parts[0] != "banners") {
		return manifestEntry{}, false, errors.New("entry must be avatars/<uuid> or banners/<uuid>")
	}
	id, err := uuid.Parse(parts[1])
	if err != nil || id.String() != parts[1] {
		return manifestEntry{}, false, errors.New("entry must contain a canonical UUID")
	}
	profileSlot, err := profileSlotForPrefix(parts[0])
	if err != nil {
		return manifestEntry{}, false, err
	}
	entry := manifestEntry{key: line, userID: id.String(), profileSlot: profileSlot}
	if parts[0] == "avatars" {
		entry.column = "avatar_url"
		entry.proxyURL = "/api/v1/media/avatars/" + entry.userID
		entry.expectedMIME = "image/png"
	} else {
		entry.column = "header_image_url"
		entry.proxyURL = "/api/v1/media/banners/" + entry.userID
		entry.expectedMIME = "image/jpeg"
	}
	return entry, true, nil
}

func (m *migrator) repairProfileMetadata(ctx context.Context, entry manifestEntry) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, profileMigrationTimeout)
	defer cancel()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errors.New("begin profile metadata repair")
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				log.Printf("  ERROR: failed to roll back profile metadata repair")
			}
		}
	}()

	// #nosec G201 -- entry.column derives only from strict manifest prefixes.
	lockQuery := fmt.Sprintf( // nosemgrep:concord-go-sql-sprintf
		`SELECT id FROM users WHERE id = $1 AND %s = $2 FOR NO KEY UPDATE`, entry.column,
	)
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, lockQuery, entry.userID, entry.proxyURL).Scan(&lockedUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, errors.New("profile URL no longer matches manifest")
		}
		return false, errors.New("lock profile metadata repair user")
	}
	var tombstoned bool
	if err := tx.QueryRowContext(ctx, profileTier1TombstoneExistsQuery, entry.key).Scan(&tombstoned); err != nil {
		return false, errors.New("check profile metadata repair tombstone")
	}
	if tombstoned {
		return false, errors.New("profile metadata is pending erasure")
	}
	var existingUploaderID string
	var existingTier int
	err = tx.QueryRowContext(ctx,
		`SELECT uploader_id, media_tier FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL`,
		entry.key,
	).Scan(&existingUploaderID, &existingTier)
	switch {
	case err == nil:
		if existingUploaderID == lockedUserID && existingTier == 1 {
			return true, nil
		}
		return false, errors.New("conflicting live profile metadata")
	case !errors.Is(err, sql.ErrNoRows):
		return false, errors.New("check existing profile metadata repair")
	}
	statObject := m.statObject
	if statObject == nil {
		statObject = m.mc.StatObject
	}
	info, err := statObject(ctx, m.bucket, entry.key, minio.StatObjectOptions{})
	if err != nil {
		return false, errors.New("stat profile object")
	}
	mimeType := strings.TrimSpace(info.ContentType)
	if mimeType != entry.expectedMIME || info.Size <= 0 {
		return false, errors.New("invalid profile object metadata")
	}
	if m.dryRun {
		return true, nil
	}
	result, err := tx.ExecContext(ctx, insertProfileTier1MetadataQuery,
		uuid.New().String(), lockedUserID, mimeType, info.Size, entry.key, entry.profileSlot,
	)
	if err != nil {
		return false, errors.New("insert profile metadata repair")
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, errors.New("confirm profile metadata repair")
	}
	if inserted != 1 {
		return false, errors.New("profile metadata repair conflict")
	}
	if err := tx.Commit(); err != nil {
		return false, errors.New("commit profile metadata repair")
	}
	committed = true
	return true, nil
}

// processForMigration delegates to the media package's processing functions
// to ensure uploads and migrations produce identical output.
func processForMigration(imgBytes []byte, maxW, maxH int, outputPNG bool) (data []byte, contentType string, err error) {
	r := bytes.NewReader(imgBytes)
	var result *media.ProcessedImage
	if outputPNG {
		result, err = media.ProcessImagePNG(r, maxW, maxH)
	} else {
		result, err = media.ProcessImage(r, maxW, maxH)
	}
	if err != nil {
		return nil, "", err
	}
	return result.Data, result.ContentType, nil
}

// decodeDataURL extracts the raw bytes and MIME type from a data URL.
// Format: data:image/png;base64,iVBORw0KGgo...
func decodeDataURL(dataURL string) ([]byte, string, error) {
	// Split on comma to get the base64 payload
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return nil, "", fmt.Errorf("invalid data URL format (no comma separator)")
	}

	// Extract MIME type from header (data:image/png;base64)
	header := parts[0]
	contentType := "application/octet-stream"
	if strings.HasPrefix(header, "data:") {
		meta := strings.TrimPrefix(header, "data:")
		meta = strings.TrimSuffix(meta, ";base64")
		if meta != "" {
			contentType = meta
		}
	}

	// Decode base64 payload
	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		// Try without padding as fallback
		decoded, err = base64.RawStdEncoding.DecodeString(parts[1])
		if err != nil {
			// Try URL-safe encoding as final fallback
			decoded, err = base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				return nil, "", fmt.Errorf("base64 decode failed: %w", err)
			}
		}
	}

	return decoded, contentType, nil
}

func (s *migrationStats) add(other migrationStats) {
	s.scanned += other.scanned
	s.migrated += other.migrated
	s.errored += other.errored
}

// maskDSN hides the password in a database URL for logging.
func maskDSN(dsn string) string {
	if idx := strings.Index(dsn, "://"); idx >= 0 {
		rest := dsn[idx+3:]
		if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
			userPart := rest[:atIdx]
			if colonIdx := strings.Index(userPart, ":"); colonIdx >= 0 {
				return dsn[:idx+3] + userPart[:colonIdx] + ":****@" + rest[atIdx+1:]
			}
		}
	}
	return dsn
}
