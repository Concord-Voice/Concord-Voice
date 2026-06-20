// Package main provides a one-time migration tool to move image assets
// from PostgreSQL base64 data URLs to MinIO object storage.
//
// Usage:
//
//	migrate-media [-dry-run] [-batch=100]
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
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	_ "github.com/joho/godotenv/autoload" // auto-load .env into process environment on import
	_ "github.com/lib/pq"                 // register postgres driver for database/sql

	"github.com/markdrogersjr/Concord/services/control-plane/internal/media"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

type migrationStats struct {
	scanned  int
	migrated int
	errored  int
}

func main() {
	dryRun := flag.Bool("dry-run", false, "Preview what would be migrated without making changes")
	batchSize := flag.Int("batch", 100, "Number of rows to process per query batch")
	flag.Parse()

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

	mc, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL,
	})
	if err != nil {
		log.Fatalf("Failed to create MinIO client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Ensure bucket exists
	exists, err := mc.BucketExists(ctx, cfg.MinIOBucket)
	if err != nil {
		log.Fatalf("Failed to check bucket: %v", err)
	}
	if !exists {
		if err := mc.MakeBucket(ctx, cfg.MinIOBucket, minio.MakeBucketOptions{}); err != nil {
			log.Fatalf("Failed to create bucket: %v", err)
		}
		log.Printf("Created bucket: %s", cfg.MinIOBucket)
	}

	mode := "LIVE"
	if *dryRun {
		mode = "DRY RUN"
	}
	log.Printf("=== Media Migration (%s) ===", mode)
	log.Printf("Database: %s", maskDSN(cfg.DatabaseURL))
	log.Printf("MinIO:    %s/%s", cfg.MinIOEndpoint, cfg.MinIOBucket)
	log.Println()

	total := &migrationStats{}

	// Migrate user avatars
	log.Println("--- User Avatars ---")
	stats := migrateColumn(ctx, db, mc, cfg.MinIOBucket, *dryRun, *batchSize, migrateOpts{
		table:     "users",
		column:    "avatar_url",
		idColumn:  "id",
		keyPrefix: "avatars",
		proxyPath: "/api/v1/media/avatars",
		maxW:      media.AvatarMaxDim,
		maxH:      media.AvatarMaxDim,
		outputPNG: true,
	})
	total.add(stats)

	// Migrate user banners
	log.Println("--- User Banners ---")
	stats = migrateColumn(ctx, db, mc, cfg.MinIOBucket, *dryRun, *batchSize, migrateOpts{
		table:     "users",
		column:    "header_image_url",
		idColumn:  "id",
		keyPrefix: "banners",
		proxyPath: "/api/v1/media/banners",
		maxW:      media.BannerMaxW,
		maxH:      media.BannerMaxH,
		outputPNG: false,
	})
	total.add(stats)

	// Migrate server icons
	log.Println("--- Server Icons ---")
	stats = migrateColumn(ctx, db, mc, cfg.MinIOBucket, *dryRun, *batchSize, migrateOpts{
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
	stats = migrateColumn(ctx, db, mc, cfg.MinIOBucket, *dryRun, *batchSize, migrateOpts{
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
	log.Printf("=== Migration Complete (%s) ===", mode)
	log.Printf("  Scanned:  %d", total.scanned)
	log.Printf("  Migrated: %d", total.migrated)
	log.Printf("  Errors:   %d", total.errored)
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
}

type migrator struct {
	db        *sql.DB
	mc        *minio.Client
	bucket    string
	dryRun    bool
	batchSize int
}

func migrateColumn(ctx context.Context, db *sql.DB, mc *minio.Client, bucket string, dryRun bool, batchSize int, opts migrateOpts) migrationStats {
	m := &migrator{db: db, mc: mc, bucket: bucket, dryRun: dryRun, batchSize: batchSize}
	var stats migrationStats

	// Query rows with base64 data URLs (skip already-migrated and NULL)
	// #nosec G201 -- table/column names are hardcoded constants, not user input
	query := fmt.Sprintf( // nosemgrep:concord-go-sql-sprintf
		`SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND %s LIKE 'data:image/%%' ORDER BY %s LIMIT $1`,
		opts.idColumn, opts.column, opts.table, opts.column, opts.column, opts.idColumn,
	)

	for {
		batchCount, err := m.migrateBatch(ctx, query, opts, &stats)
		if err != nil {
			return stats
		}

		if batchCount < batchSize {
			break
		}
	}

	log.Printf("  Subtotal: %d scanned, %d migrated, %d errors", stats.scanned, stats.migrated, stats.errored)
	return stats
}

func (m *migrator) migrateBatch(ctx context.Context, query string, opts migrateOpts, stats *migrationStats) (int, error) {
	rows, err := m.db.QueryContext(ctx, query, m.batchSize)
	if err != nil {
		log.Printf("  ERROR: query failed: %v", err)
		stats.errored++
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	batchCount := 0
	for rows.Next() {
		var id, dataURL string
		if err := rows.Scan(&id, &dataURL); err != nil {
			log.Printf("  ERROR: scan failed: %v", err)
			stats.errored++
			continue
		}
		stats.scanned++
		batchCount++

		m.migrateRow(ctx, id, dataURL, opts, stats)
	}
	if err := rows.Err(); err != nil {
		log.Printf("  ERROR: row iteration failed: %v", err)
		stats.errored++
	}

	return batchCount, nil
}

func (m *migrator) migrateRow(ctx context.Context, id, dataURL string, opts migrateOpts, stats *migrationStats) {
	imgBytes, contentType, err := decodeDataURL(dataURL)
	if err != nil {
		log.Printf("  ERROR [%s %s]: failed to decode data URL: %v", opts.table, id, err)
		stats.errored++
		return
	}

	if m.dryRun {
		log.Printf("  WOULD MIGRATE [%s %s]: %s (%d bytes base64 → ~%d bytes raw)",
			opts.table, id, contentType, len(dataURL), len(imgBytes))
		stats.migrated++
		return
	}

	processedData, processedCT, procErr := processForMigration(imgBytes, opts.maxW, opts.maxH, opts.outputPNG)
	if procErr != nil {
		log.Printf("  ERROR [%s %s]: failed to process image: %v", opts.table, id, procErr)
		stats.errored++
		return
	}

	objectKey := fmt.Sprintf("%s/%s", opts.keyPrefix, id)
	_, err = m.mc.PutObject(ctx, m.bucket, objectKey, bytes.NewReader(processedData),
		int64(len(processedData)), minio.PutObjectOptions{ContentType: processedCT})
	if err != nil {
		log.Printf("  ERROR [%s %s]: failed to upload to MinIO: %v", opts.table, id, err)
		stats.errored++
		return
	}

	newURL := fmt.Sprintf("%s/%s", opts.proxyPath, id)
	// #nosec G201 -- table/column names are hardcoded constants
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query — table/column identifiers come from hardcoded migration opts, values are parameterized
	updateQuery := fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE %s = $2`, opts.table, opts.column, opts.idColumn) // nosemgrep:concord-go-sql-sprintf
	if _, err := m.db.ExecContext(ctx, updateQuery, newURL, id); err != nil {
		log.Printf("  ERROR [%s %s]: failed to update DB: %v", opts.table, id, err)
		stats.errored++
		return
	}

	log.Printf("  MIGRATED [%s %s]: %d bytes base64 → %d bytes %s → %s",
		opts.table, id, len(dataURL), len(processedData), processedCT, newURL)
	stats.migrated++
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
