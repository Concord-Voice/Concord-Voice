// Package feedbackscreenshots tracks the durable lifecycle of feedback screenshot objects.
package feedbackscreenshots

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

const (
	reservationTTL  = 72 * time.Hour
	failedRetry     = time.Hour
	putTimeout      = 5 * time.Second
	maxSweepBatch   = 100
	maxOffloadBatch = 5
	maxPutAttempts  = 3
	maxPending      = 8
	maxPendingSize  = int64(40 << 20)
	maxUserObjects  = 128
	maxUserSize     = int64(128 << 20)
	maxGlobalObjs   = 10000
	maxGlobalSize   = int64(10 << 30)
	maxStagedSize   = int64(512 << 20)
	advisoryLockID  = int64(0x4653424c494645) // FSB(LIFE)
	storagePrefix   = "feedback-screenshots/"
	publicPrefix    = "/api/v1/media/feedback-screenshots/"
)

var (
	// ErrQuota means the screenshot count or stored-byte allowance is exhausted.
	ErrQuota = errors.New("feedback screenshot quota exceeded")
	// ErrInvalid means the screenshot identity or lifecycle does not permit the operation.
	ErrInvalid = errors.New("invalid feedback screenshot")
)

// ObjectPutter retains only published screenshots in the object store.
type ObjectPutter interface {
	PutObject(context.Context, string, io.Reader, int64, string) error
}

// DiskChecker applies the shared-disk watermark immediately before offload.
type DiskChecker interface{ Check() error }

// Service owns screenshot lifecycle records and quota enforcement.
type Service struct{ db *sql.DB }

// New creates a lifecycle service backed by PostgreSQL.
func New(db *sql.DB) *Service { return &Service{db: db} }

func rollbackScreenshotTx(tx *sql.Tx, label string, retErr *error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		*retErr = errors.Join(*retErr, fmt.Errorf("rollback %s: %w", label, err))
	}
}

type screenshotQuotaUsage struct {
	pendingCount, userCount, globalCount          int
	pendingSize, userSize, globalSize, stagedSize int64
}

func (q screenshotQuotaUsage) exceeded(size int64) bool {
	return q.pendingCount+1 > maxPending || q.pendingSize+size > maxPendingSize ||
		q.userCount+1 > maxUserObjects || q.userSize+size > maxUserSize ||
		q.globalCount+1 > maxGlobalObjs || q.globalSize+size > maxGlobalSize ||
		q.stagedSize+size > maxStagedSize
}

func stageIdentity(userID, key, contentType string, data []byte) (uuid.UUID, uuid.UUID, error) {
	uid, err := uuid.Parse(userID)
	size := int64(len(data))
	if err != nil || uid.String() != userID || size <= 0 || size > maxPendingSize ||
		(contentType != "image/jpeg" && contentType != "image/png") {
		return uuid.Nil, uuid.Nil, ErrInvalid
	}
	objectID, err := parseStorageKey(key)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return uid, objectID, nil
}

// Stage atomically charges quota, stores pending bytes and creates Tier 1 metadata.
// No object-store write starts before a report is positively published.
func (s *Service) Stage(ctx context.Context, userID, key string, data []byte, contentType string) (fileID string, retErr error) {
	uid, objectID, err := stageIdentity(userID, key, contentType, data)
	size := int64(len(data))
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin screenshot staging: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // The next defer reports rollback errors.
	defer rollbackScreenshotTx(tx, "screenshot staging", &retErr)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockID); err != nil {
		return "", fmt.Errorf("lock screenshot quota: %w", err)
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 FOR UPDATE)`, uid).Scan(&exists); err != nil {
		return "", fmt.Errorf("lock screenshot owner: %w", err)
	}
	if !exists {
		return "", ErrInvalid
	}
	var usage screenshotQuotaUsage
	err = tx.QueryRowContext(ctx, `SELECT
        count(*) FILTER (WHERE user_id=$1 AND (state='pending' OR (state='pinned' AND NOT published AND NOT retained))),
        COALESCE(sum(size_bytes) FILTER (WHERE user_id=$1 AND (state='pending' OR (state='pinned' AND NOT published AND NOT retained))),0),
        count(*) FILTER (WHERE user_id=$1),
        COALESCE(sum(size_bytes) FILTER (WHERE user_id=$1),0),
        count(*), COALESCE(sum(size_bytes),0),
        COALESCE(sum(size_bytes) FILTER (WHERE staged_data IS NOT NULL),0)
        FROM feedback_screenshot_uploads WHERE state<>'deleted'`, uid).
		Scan(&usage.pendingCount, &usage.pendingSize, &usage.userCount, &usage.userSize,
			&usage.globalCount, &usage.globalSize, &usage.stagedSize)
	if err != nil {
		return "", fmt.Errorf("read screenshot quota: %w", err)
	}
	if usage.exceeded(size) {
		return "", ErrQuota
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO feedback_screenshot_uploads
        (object_key,user_id,size_bytes,state,put_completed,staged_data,content_type,expires_at)
        VALUES ($1,$2,$3,'pending',FALSE,$4,$5,NOW()+$6::interval)`,
		objectID, uid, size, data, contentType, reservationTTL.String())
	if err != nil {
		return "", fmt.Errorf("insert staged screenshot: %w", err)
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO media_files
        (uploader_id,file_type,media_tier,mime_type,file_size,storage_key)
        VALUES ($1,'photo',1,$2,$3,$4) RETURNING id::text`, uid, contentType, size, key).Scan(&fileID)
	if err != nil {
		return "", fmt.Errorf("insert screenshot metadata: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("commit staged screenshot: %w", err)
	}
	return fileID, nil
}

// Pin atomically preserves submitted screenshots. The random receipt binds a
// publication outcome to this specific attempt even if account erasure clears
// the nullable owner before the external provider responds.
func (s *Service) Pin(ctx context.Context, userID string, paths []string) (receipt string, retErr error) {
	uid, err := uuid.Parse(userID)
	if err != nil || uid.String() != userID {
		return "", ErrInvalid
	}
	ids, err := parsePinPaths(paths)
	if err != nil {
		return "", err
	}
	token := uuid.New()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin screenshot pin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // The next defer reports rollback errors.
	defer rollbackScreenshotTx(tx, "screenshot pin", &retErr)
	for _, id := range ids {
		if err = pinScreenshot(ctx, tx, uid, id, token); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		// COMMIT may have reached PostgreSQL even when its acknowledgement did
		// not reach us. Return the receipt so the caller can release a pin that
		// was never sent to the issue provider.
		return token.String(), fmt.Errorf("commit screenshot pin: %w", err)
	}
	return token.String(), nil
}

func parsePinPaths(paths []string) ([]uuid.UUID, error) {
	if len(paths) == 0 {
		return nil, ErrInvalid
	}
	ids := make([]uuid.UUID, 0, len(paths))
	seen := make(map[uuid.UUID]struct{}, len(paths))
	for _, path := range paths {
		id, err := parsePublicPath(path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids, nil
}

func pinScreenshot(ctx context.Context, tx *sql.Tx, userID, id, token uuid.UUID) error {
	var owner sql.NullString
	var state string
	var active bool
	var completed bool
	var published bool
	var retained bool
	err := tx.QueryRowContext(ctx, `SELECT user_id,state,(expires_at>NOW()),(staged_data IS NOT NULL OR put_completed),published,retained FROM feedback_screenshot_uploads WHERE object_key=$1 FOR UPDATE`, id).Scan(&owner, &state, &active, &completed, &published, &retained)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalid
	}
	if err != nil {
		return fmt.Errorf("lock screenshot for pin: %w", err)
	}
	if !owner.Valid || owner.String != userID.String() || !completed || (state != "pending" && state != "pinned") || (state == "pending" && !active) {
		return ErrInvalid
	}
	key := storageKey(id)
	var mediaExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_files WHERE storage_key=$1 AND uploader_id=$2 AND media_tier=1 AND deleted_at IS NULL)`, key, userID).Scan(&mediaExists); err != nil {
		return fmt.Errorf("verify screenshot ownership: %w", err)
	}
	if !mediaExists {
		return ErrInvalid
	}
	if !published && !retained {
		if _, err = tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
			SET state='pinned',pin_attempts=pin_attempts+1,pin_tokens=array_append(pin_tokens,$2::uuid),
			retained_at=CASE WHEN state='pending' THEN NOW()+$3::interval ELSE retained_at END,
			updated_at=NOW()
			WHERE object_key=$1`, id, token, reservationTTL.String()); err != nil {
			return fmt.Errorf("pin screenshot: %w", err)
		}
	}
	return nil
}

// ConfirmPublished makes a successful report's pins permanent. A failed
// confirmation leaves its pin attempts charged, preserving the already-filed
// issue's screenshots until the outcome can be reconciled.
func (s *Service) ConfirmPublished(ctx context.Context, receipt string, paths []string) error {
	return s.finishPin(ctx, receipt, paths, true)
}

// ReleaseRejected releases only this submission's pins after an explicit
// provider rejection. Concurrent submissions keep their own attempts, and a
// screenshot already confirmed in another report remains pinned.
func (s *Service) ReleaseRejected(ctx context.Context, receipt string, paths []string) error {
	return s.finishPin(ctx, receipt, paths, false)
}

func (s *Service) finishPin(ctx context.Context, receipt string, paths []string, published bool) (retErr error) {
	token, err := uuid.Parse(receipt)
	if err != nil || token.String() != receipt {
		return ErrInvalid
	}
	ids, err := parsePinPaths(paths)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin screenshot pin outcome: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // The next defer reports rollback errors.
	defer rollbackScreenshotTx(tx, "screenshot pin outcome", &retErr)
	for _, id := range ids {
		if err = finishOnePin(ctx, tx, token, id, published); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit screenshot pin outcome: %w", err)
	}
	return nil
}

func finishOnePin(ctx context.Context, tx *sql.Tx, token, id uuid.UUID, published bool) error {
	var state string
	var attempts int64
	var alreadyPublished bool
	var retained bool
	var authorized bool
	err := tx.QueryRowContext(ctx, `SELECT state,pin_attempts,published,retained,$2::uuid=ANY(pin_tokens)
		FROM feedback_screenshot_uploads WHERE object_key=$1 FOR UPDATE`, id, token).
		Scan(&state, &attempts, &alreadyPublished, &retained, &authorized)
	if err != nil {
		return fmt.Errorf("lock screenshot pin outcome: %w", err)
	}
	if state != "pinned" {
		return ErrInvalid
	}
	if alreadyPublished {
		return nil
	}
	if retained {
		if published {
			_, err = tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads SET published=TRUE,updated_at=NOW() WHERE object_key=$1`, id)
			return err
		}
		return nil
	}
	if attempts == 0 || !authorized {
		return ErrInvalid
	}
	if published {
		_, err = tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
			SET published=TRUE,pin_attempts=0,pin_tokens='{}',updated_at=NOW() WHERE object_key=$1`, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
			SET pin_attempts=pin_attempts-1,pin_tokens=array_remove(pin_tokens,$2::uuid),
			state=CASE WHEN pin_attempts=1 THEN 'pending' ELSE 'pinned' END,
			retained_at=CASE WHEN pin_attempts=1 THEN NULL ELSE retained_at END,
			updated_at=NOW() WHERE object_key=$1`, id, token)
	}
	if err != nil {
		return fmt.Errorf("record screenshot pin outcome: %w", err)
	}
	return nil
}

// Read returns staged bytes for pending or not-yet-offloaded pinned images.
// Historical rowless images and positively offloaded images use object storage.
func (s *Service) Read(ctx context.Context, key string) (data []byte, contentType string, remote, allowed bool, err error) {
	id, err := parseStorageKey(key)
	if err != nil {
		return nil, "", false, false, err
	}
	var state string
	var owner sql.NullString
	var active, putCompleted bool
	var mime sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT state,user_id,expires_at>NOW(),put_completed,staged_data,content_type
        FROM feedback_screenshot_uploads WHERE object_key=$1`, id).
		Scan(&state, &owner, &active, &putCompleted, &data, &mime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", true, true, nil
	}
	if err != nil {
		return nil, "", false, false, fmt.Errorf("read screenshot lifecycle: %w", err)
	}
	allowed = state == "pinned" || (state == "pending" && owner.Valid && active)
	if !allowed {
		return nil, "", false, false, nil
	}
	if data != nil {
		return data, mime.String, false, true, nil
	}
	if putCompleted {
		return nil, "", true, true, nil
	}
	return nil, "", false, false, fmt.Errorf("screenshot %s has no readable bytes", id)
}

// Sweep claims a bounded expired batch, then removes each pending image's bytes
// and metadata atomically. Failed rows keep a retry lease so one bad deletion
// cannot block every later abandoned upload.
func (s *Service) Sweep(ctx context.Context) (deletedCount int, retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin screenshot sweep claim: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // The next defer reports rollback errors.
	defer rollbackScreenshotTx(tx, "screenshot sweep claim", &retErr)
	rows, err := tx.QueryContext(ctx, `WITH expired AS (
        SELECT object_key FROM feedback_screenshot_uploads
        WHERE state='pending' AND (expires_at<=NOW() OR user_id IS NULL)
          AND (retry_at IS NULL OR retry_at<=NOW())
        ORDER BY expires_at,object_key FOR UPDATE SKIP LOCKED LIMIT $1
    ) UPDATE feedback_screenshot_uploads fs
      SET retry_at=NOW()+$2::interval,updated_at=NOW()
      FROM expired WHERE fs.object_key=expired.object_key RETURNING fs.object_key`,
		maxSweepBatch, failedRetry.String())
	if err != nil {
		return 0, fmt.Errorf("claim expired screenshots: %w", err)
	}
	ids, err := readScreenshotIDs(rows, maxSweepBatch, "expired screenshots")
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit screenshot sweep claims: %w", err)
	}
	var failures []error
	for _, id := range ids {
		if err = s.reclaimOne(ctx, id); err != nil {
			failures = append(failures, err)
		} else {
			deletedCount++
		}
	}
	return deletedCount, errors.Join(failures...)
}

func (s *Service) reclaimOne(ctx context.Context, id uuid.UUID) (retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin expired screenshot cleanup: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // The next defer reports rollback errors.
	defer rollbackScreenshotTx(tx, "expired screenshot cleanup", &retErr)
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1 FOR UPDATE`, id).Scan(&state); err != nil {
		return fmt.Errorf("lock expired screenshot cleanup: %w", err)
	}
	if state != "pending" {
		return ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM media_files WHERE storage_key=$1 AND media_tier=1`, storageKey(id)); err != nil {
		return fmt.Errorf("delete expired screenshot metadata: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
        SET state='deleted',user_id=NULL,staged_data=NULL,content_type=NULL,retry_at=NULL,updated_at=NOW()
		WHERE object_key=$1 AND state='pending'`, id)
	if err != nil {
		return fmt.Errorf("reclaim expired screenshot bytes: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check expired screenshot cleanup: %w", err)
	}
	if n != 1 {
		return ErrInvalid
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit expired screenshot cleanup: %w", err)
	}
	return nil
}

// Offload writes only permanent screenshots to their immutable key. Unresolved
// pinned submissions become permanently retained after 72 hours, allowing
// offload without falsely recording a confirmed publication.
// A failed or uncertain PUT leaves the PostgreSQL bytes readable and charged;
// attempts are durably capped because versioned stores can retain overwritten
// objects even when each retry uses identical bytes and an immutable key.
func (s *Service) Offload(ctx context.Context, putter ObjectPutter, checker DiskChecker) (completed int, retErr error) {
	if putter == nil || checker == nil {
		return 0, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin screenshot offload: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // The next defer reports rollback errors.
	defer rollbackScreenshotTx(tx, "screenshot offload claims", &retErr)
	if _, err = tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
		SET retained=TRUE,pin_attempts=0,pin_tokens='{}',updated_at=NOW()
		WHERE object_key IN (
			SELECT object_key FROM feedback_screenshot_uploads
			WHERE state='pinned' AND NOT published AND NOT retained AND retained_at<=NOW()
			ORDER BY retained_at,object_key FOR UPDATE SKIP LOCKED LIMIT $1
		)`, maxSweepBatch); err != nil {
		return 0, fmt.Errorf("retain unresolved screenshot submissions: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT object_key FROM feedback_screenshot_uploads
		WHERE state='pinned' AND (published OR retained) AND staged_data IS NOT NULL
		  AND put_attempts<$2 AND (retry_at IS NULL OR retry_at<=NOW())
		ORDER BY retry_at NULLS FIRST,object_key FOR UPDATE SKIP LOCKED LIMIT $1`, maxOffloadBatch, maxPutAttempts)
	if err != nil {
		return 0, fmt.Errorf("select screenshot offload: %w", err)
	}
	ids, err := readScreenshotIDs(rows, maxOffloadBatch, "screenshot offload")
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
            SET retry_at=NOW()+$2::interval,updated_at=NOW() WHERE object_key=$1`, id, failedRetry.String()); err != nil {
			return 0, fmt.Errorf("claim screenshot offload: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit screenshot offload claims: %w", err)
	}
	var failures []error
	for _, id := range ids {
		var data []byte
		var mime string
		if err = s.db.QueryRowContext(ctx, `SELECT staged_data,content_type FROM feedback_screenshot_uploads
			WHERE object_key=$1 AND state='pinned' AND (published OR retained) AND staged_data IS NOT NULL`, id).Scan(&data, &mime); err != nil {
			failures = append(failures, fmt.Errorf("load staged screenshot: %w", err))
			continue
		}
		if err = checker.Check(); err != nil {
			failures = append(failures, fmt.Errorf("check screenshot offload disk: %w", err))
			continue
		}
		claim, claimErr := s.db.ExecContext(ctx, `UPDATE feedback_screenshot_uploads
			SET put_attempts=put_attempts+1,updated_at=NOW()
			WHERE object_key=$1 AND state='pinned' AND (published OR retained)
			  AND staged_data IS NOT NULL AND put_attempts<$2`, id, maxPutAttempts)
		if claimErr != nil {
			failures = append(failures, fmt.Errorf("charge screenshot offload attempt: %w", claimErr))
			continue
		}
		n, affectErr := claim.RowsAffected()
		if affectErr != nil || n != 1 {
			failures = append(failures, fmt.Errorf("charge screenshot offload affected %d rows: %w", n, affectErr))
			continue
		}
		putCtx, cancel := context.WithTimeout(ctx, putTimeout)
		putErr := putter.PutObject(putCtx, storageKey(id), bytes.NewReader(data), int64(len(data)), mime)
		cancel()
		if putErr != nil {
			failures = append(failures, fmt.Errorf("offload screenshot: %w", putErr))
			continue
		}
		confirmCtx, confirmCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		result, updateErr := s.db.ExecContext(confirmCtx, `UPDATE feedback_screenshot_uploads
            SET put_completed=TRUE,staged_data=NULL,content_type=NULL,retry_at=NULL,updated_at=NOW()
			WHERE object_key=$1 AND state='pinned' AND (published OR retained) AND staged_data IS NOT NULL`, id)
		confirmCancel()
		if updateErr != nil {
			failures = append(failures, fmt.Errorf("confirm screenshot offload: %w", updateErr))
			continue
		}
		n, affectErr = result.RowsAffected()
		if affectErr != nil || n != 1 {
			failures = append(failures, fmt.Errorf("confirm screenshot offload affected %d rows: %w", n, affectErr))
			continue
		}
		completed++
	}
	return completed, errors.Join(failures...)
}

func readScreenshotIDs(rows *sql.Rows, capacity int, label string) (ids []uuid.UUID, retErr error) {
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close %s: %w", label, err))
		}
	}()
	ids = make([]uuid.UUID, 0, capacity)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s: %w", label, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", label, err)
	}
	return ids, nil
}

func parseStorageKey(key string) (uuid.UUID, error) {
	return parseUUIDWithPrefix(key, storagePrefix)
}
func parsePublicPath(path string) (uuid.UUID, error) {
	return parseUUIDWithPrefix(path, publicPrefix)
}

func parseUUIDWithPrefix(value, prefix string) (uuid.UUID, error) {
	if !strings.HasPrefix(value, prefix) {
		return uuid.Nil, ErrInvalid
	}
	id, err := uuid.Parse(strings.TrimPrefix(value, prefix))
	if err != nil || prefix+id.String() != value {
		return uuid.Nil, ErrInvalid
	}
	return id, nil
}
func storageKey(id uuid.UUID) string { return storagePrefix + id.String() }

// Start reclaims abandoned bytes and offloads permanent images at startup and
// each minute. The caller cancels ctx and waits with the returned function.
func (s *Service) Start(ctx context.Context, putter ObjectPutter, checker DiskChecker, log *logger.Logger) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var lastCappedReport time.Time
		for {
			s.runPass(ctx, putter, checker, log)
			if log != nil && time.Since(lastCappedReport) >= time.Hour && ctx.Err() == nil {
				lastCappedReport = time.Now()
				s.reportCappedOffloads(ctx, log)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}

func (s *Service) runPass(ctx context.Context, putter ObjectPutter, checker DiskChecker, log *logger.Logger) {
	passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	count, sweepErr := s.Sweep(passCtx)
	offloaded := 0
	var offloadErr error
	if putter != nil && checker != nil && passCtx.Err() == nil {
		offloaded, offloadErr = s.Offload(passCtx, putter, checker)
	}
	if sweepErr != nil && ctx.Err() == nil && log != nil {
		log.Error("feedback screenshot cleanup failed", "operation", "feedback_screenshot_cleanup", "error", sweepErr)
	}
	if offloadErr != nil && ctx.Err() == nil && log != nil {
		log.Error("feedback screenshot offload failed", "operation", "feedback_screenshot_offload", "error", offloadErr)
	}
	if log != nil && (count > 0 || offloaded > 0) {
		log.Info("feedback screenshot lifecycle pass", "reaped", count, "offloaded", offloaded)
	}
}

func (s *Service) reportCappedOffloads(ctx context.Context, log *logger.Logger) {
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var capped int
	err := s.db.QueryRowContext(statusCtx, `SELECT COUNT(*) FROM feedback_screenshot_uploads
		WHERE state='pinned' AND staged_data IS NOT NULL AND put_attempts >= $1`, maxPutAttempts).Scan(&capped)
	if err != nil {
		log.Error("feedback screenshot capped-offload check failed", "error", err)
	} else if capped > 0 {
		log.Warn("feedback screenshot offload needs operator reconciliation", "capped_count", capped)
	}
}
