package feedbackscreenshots

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func setup(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	user := testdb.CreateUser(t, db)
	return db, user.String()
}

func storageKeyForTest(id string) string { return storagePrefix + id }
func publicPathForTest(id string) string { return publicPrefix + id }

func stageForTest(t *testing.T, svc *Service, userID, id string, data []byte) string {
	t.Helper()
	fileID, err := svc.Stage(context.Background(), userID, storageKeyForTest(id), data, "image/png")
	require.NoError(t, err)
	return fileID
}

func pinForTest(t *testing.T, svc *Service, userID string, paths []string) string {
	t.Helper()
	receipt, err := svc.Pin(context.Background(), userID, paths)
	require.NoError(t, err)
	require.NotEmpty(t, receipt)
	return receipt
}

func TestStageStoresPendingBytesAndCreatesMetadata(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	data := []byte("staged screenshot bytes")
	fileID := stageForTest(t, svc, userID, id, data)
	require.NotEmpty(t, fileID)
	got, mime, remote, allowed, err := svc.Read(context.Background(), storageKeyForTest(id))
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, remote)
	require.Equal(t, "image/png", mime)
	require.Equal(t, data, got)
	var state string
	var expires time.Time
	var mediaCount int
	require.NoError(t, db.QueryRow(`SELECT state,expires_at FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &expires))
	require.Equal(t, "pending", state)
	require.WithinDuration(t, time.Now().Add(72*time.Hour), expires, 2*time.Minute)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE id=$1 AND storage_key=$2 AND media_tier=1`, fileID, storageKeyForTest(id)).Scan(&mediaCount))
	require.Equal(t, 1, mediaCount)
}

func TestStageValidatesIdentitySizeAndType(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	for _, tc := range []struct {
		id, mime string
		data     []byte
	}{
		{"bad-key", "image/png", []byte("image")},
		{uuid.NewString(), "image/gif", []byte("image")},
		{uuid.NewString(), "image/jpeg", nil},
	} {
		_, err := svc.Stage(context.Background(), userID, storageKeyForTest(tc.id), tc.data, tc.mime)
		require.ErrorIs(t, err, ErrInvalid)
	}
	_, _, _, allowed, err := svc.Read(context.Background(), "feedback-screenshots/not-a-uuid")
	require.ErrorIs(t, err, ErrInvalid)
	require.False(t, allowed)
}

func TestStageMetadataFailureRollsBackBytesAndQuota(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	_, err := db.Exec(`CREATE FUNCTION reject_feedback_screenshot_metadata_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'forced screenshot metadata failure'; END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_feedback_screenshot_metadata_insert BEFORE INSERT ON media_files
		FOR EACH ROW EXECUTE FUNCTION reject_feedback_screenshot_metadata_insert()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS reject_feedback_screenshot_metadata_insert ON media_files`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS reject_feedback_screenshot_metadata_insert()`)
	})
	_, err = svc.Stage(context.Background(), userID, storageKeyForTest(id), []byte("image"), "image/png")
	require.Error(t, err)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&count))
	require.Zero(t, count, "failed metadata cannot leave staged bytes or charged quota")
	_, err = db.Exec(`DROP TRIGGER reject_feedback_screenshot_metadata_insert ON media_files`)
	require.NoError(t, err)
	stageForTest(t, svc, userID, id, []byte("image"))
}

func TestConcurrentStageCallsSerializePendingQuota(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	start := make(chan struct{})
	type result struct{ err error }
	results := make(chan result, maxPending+1)
	var wg sync.WaitGroup
	for i := 0; i < maxPending+1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Stage(ctx, userID, storageKeyForTest(uuid.NewString()), []byte("image"), "image/png")
			results <- result{err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var succeeded, quotaRejected int
	for result := range results {
		switch {
		case result.err == nil:
			succeeded++
		case errors.Is(result.err, ErrQuota):
			quotaRejected++
		default:
			require.NoError(t, result.err)
		}
	}
	require.Equal(t, maxPending, succeeded)
	require.Equal(t, 1, quotaRejected)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM feedback_screenshot_uploads WHERE user_id=$1 AND state='pending'`, userID).Scan(&count))
	require.Equal(t, maxPending, count)
}

func TestPendingByteQuotaCapsAtFortyMiB(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	data := make([]byte, int(maxPendingSize/4))
	for i := 0; i < 4; i++ {
		_, err := svc.Stage(context.Background(), userID, storageKeyForTest(uuid.NewString()), data, "image/png")
		require.NoError(t, err)
	}
	_, err := svc.Stage(context.Background(), userID, storageKeyForTest(uuid.NewString()), []byte("x"), "image/png")
	require.ErrorIs(t, err, ErrQuota, "a fifth image exceeds the 40 MiB pending byte allowance")
}

func TestPerUserByteQuotaIncludesPublishedScreenshots(t *testing.T) {
	db, userID := setup(t)
	_, err := db.Exec(`INSERT INTO feedback_screenshot_uploads
		(object_key,user_id,size_bytes,state,put_completed,published,expires_at)
		VALUES ($1,$2,$3,'pinned',TRUE,TRUE,NOW())`, uuid.NewString(), userID, maxUserSize)
	require.NoError(t, err)
	_, err = New(db).Stage(context.Background(), userID, storageKeyForTest(uuid.NewString()), []byte("x"), "image/png")
	require.ErrorIs(t, err, ErrQuota)
}

func TestGlobalByteQuotaIncludesOtherOwnersPublishedScreenshots(t *testing.T) {
	db, userID := setup(t)
	otherUserID := testdb.CreateUser(t, db).String()
	_, err := db.Exec(`INSERT INTO feedback_screenshot_uploads
		(object_key,user_id,size_bytes,state,put_completed,published,expires_at)
		VALUES ($1,$2,$3,'pinned',TRUE,TRUE,NOW())`, uuid.NewString(), otherUserID, maxGlobalSize)
	require.NoError(t, err)
	_, err = New(db).Stage(context.Background(), userID, storageKeyForTest(uuid.NewString()), []byte("x"), "image/png")
	require.ErrorIs(t, err, ErrQuota)
}

func TestPinPublicationAndRejectedOutcomePreserveConcurrentReferences(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	path := publicPathForTest(id)
	stageForTest(t, svc, userID, id, []byte("screenshot"))
	ctx := context.Background()
	firstReceipt := pinForTest(t, svc, userID, []string{path})
	var firstRetainedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT retained_at FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&firstRetainedAt))
	secondReceipt := pinForTest(t, svc, userID, []string{path})
	var secondRetainedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT retained_at FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&secondRetainedAt))
	require.Equal(t, firstRetainedAt, secondRetainedAt, "a concurrent unresolved report must not extend the retention deadline")
	require.ErrorIs(t, svc.ReleaseRejected(ctx, uuid.NewString(), []string{path}), ErrInvalid,
		"an unrelated receipt must not release another report's pin")
	require.NoError(t, svc.ReleaseRejected(ctx, firstReceipt, []string{path}))
	var state string
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT state,pin_attempts FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &attempts))
	require.Equal(t, "pinned", state)
	require.Equal(t, 1, attempts)
	require.NoError(t, db.QueryRow(`SELECT retained_at FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&secondRetainedAt))
	require.Equal(t, firstRetainedAt, secondRetainedAt, "rejecting one attempt must preserve the remaining attempt's original deadline")
	require.NoError(t, svc.ConfirmPublished(ctx, secondReceipt, []string{path}))
	require.NoError(t, svc.ReleaseRejected(ctx, secondReceipt, []string{path}), "published evidence remains retained")
	var published bool
	require.NoError(t, db.QueryRow(`SELECT state,pin_attempts,published FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &attempts, &published))
	require.Equal(t, "pinned", state)
	require.Zero(t, attempts)
	require.True(t, published)
}

func TestProvisionalPinsContinueCountingAgainstPendingQuota(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	for i := 0; i < maxPending; i++ {
		id := uuid.NewString()
		stageForTest(t, svc, userID, id, []byte("image"))
		pinForTest(t, svc, userID, []string{publicPathForTest(id)})
	}
	_, err := svc.Stage(context.Background(), userID, storageKeyForTest(uuid.NewString()), []byte("ninth"), "image/png")
	require.ErrorIs(t, err, ErrQuota, "provisionally pinned uploads still consume the pending allowance")
}

func TestReceiptReleasesPinAfterAccountDeletion(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	id := uuid.NewString()
	path := publicPathForTest(id)
	stageForTest(t, svc, userID, id, []byte("rejected report image"))
	receipt := pinForTest(t, svc, userID, []string{path})
	_, err := db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	require.NoError(t, err)
	require.NoError(t, svc.ReleaseRejected(ctx, receipt, []string{path}), "receipt is durable across account erasure")
	var owner sql.NullString
	var state string
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT user_id::text,state,pin_attempts FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&owner, &state, &attempts))
	require.False(t, owner.Valid)
	require.Equal(t, "pending", state)
	require.Zero(t, attempts)
	_, err = db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	count, err := svc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count, "definitely rejected image is reclaimed after account erasure")
	require.NoError(t, db.QueryRow(`SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state))
	require.Equal(t, "deleted", state)
}

func TestReceiptConfirmsPublishedPinAfterAccountDeletion(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	path := publicPathForTest(id)
	data := []byte("published report image")
	stageForTest(t, svc, userID, id, data)
	receipt := pinForTest(t, svc, userID, []string{path})
	_, err := db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	require.NoError(t, err)
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{path}))
	var state string
	var published bool
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT state,published,pin_attempts FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &published, &attempts))
	require.Equal(t, "pinned", state)
	require.True(t, published)
	require.Zero(t, attempts)
	got, _, remote, allowed, err := svc.Read(context.Background(), storageKeyForTest(id))
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, remote)
	require.Equal(t, data, got)
}

func TestPinUpdateFailureRollsBackAllPins(t *testing.T) {
	db, userID := setup(t)
	ctx := context.Background()
	svc := New(db)
	keys := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}
	for _, id := range keys {
		stageForTest(t, svc, userID, id, []byte("image"))
	}
	const seq = "test_feedback_screenshot_pin_attempts"
	_, err := db.Exec(`CREATE SEQUENCE ` + seq)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE FUNCTION test_fail_second_feedback_screenshot_pin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.state = 'pinned' THEN
			PERFORM nextval('` + seq + `');
			IF NEW.object_key = '00000000-0000-4000-8000-000000000002'::uuid THEN
				RAISE EXCEPTION 'simulated second feedback screenshot pin failure';
			END IF;
		END IF;
		RETURN NEW;
	END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER test_fail_second_feedback_screenshot_pin BEFORE UPDATE ON feedback_screenshot_uploads
		FOR EACH ROW EXECUTE FUNCTION test_fail_second_feedback_screenshot_pin()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS test_fail_second_feedback_screenshot_pin ON feedback_screenshot_uploads`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS test_fail_second_feedback_screenshot_pin()`)
		_, _ = db.Exec(`DROP SEQUENCE IF EXISTS ` + seq)
	})
	_, err = svc.Pin(ctx, userID, []string{publicPathForTest(keys[1]), publicPathForTest(keys[0])})
	require.Error(t, err)
	var attempts int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT last_value FROM `+seq).Scan(&attempts))
	require.EqualValues(t, 2, attempts, "the second-row trigger proves both sorted updates were attempted")
	for _, id := range keys {
		var state string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state))
		require.Equal(t, "pending", state, "pin transaction must roll back the first update")
	}
}

func TestPinRejectsNoncanonicalPathsAndOtherOwners(t *testing.T) {
	db, ownerID := setup(t)
	other := testdb.CreateUser(t, db).String()
	svc := New(db)
	id := uuid.NewString()
	stageForTest(t, svc, ownerID, id, []byte("image"))
	_, err := svc.Pin(context.Background(), ownerID, []string{publicPrefix + strings.ToUpper(id)})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = svc.Pin(context.Background(), ownerID, []string{storageKeyForTest(id)})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = svc.Pin(context.Background(), other, []string{publicPathForTest(id)})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestPinRejectsExpiredOrMetadataLessPendingImages(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	expiredID := uuid.NewString()
	orphanID := uuid.NewString()
	stageForTest(t, svc, userID, expiredID, []byte("expired"))
	stageForTest(t, svc, userID, orphanID, []byte("orphan"))
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, expiredID)
	require.NoError(t, err)
	_, err = svc.Pin(context.Background(), userID, []string{publicPathForTest(expiredID)})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = db.Exec(`DELETE FROM media_files WHERE storage_key=$1`, storageKeyForTest(orphanID))
	require.NoError(t, err)
	_, err = svc.Pin(context.Background(), userID, []string{publicPathForTest(orphanID)})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestAccountDeletionPreservesStagedChargeUntilSweep(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	stageForTest(t, svc, userID, id, []byte("image"))
	_, err := db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	require.NoError(t, err)
	var retainedOwner sql.NullString
	var state string
	require.NoError(t, db.QueryRow(`SELECT user_id::text,state FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&retainedOwner, &state))
	require.False(t, retainedOwner.Valid)
	require.Equal(t, "pending", state)
	_, err = db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	count, err := svc.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state))
	require.Equal(t, "deleted", state)
}

func TestSweepExpiresPendingBytesAndMetadataAtomically(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	stageForTest(t, svc, userID, id, []byte("screenshot"))
	for i := 0; i < maxPending-1; i++ {
		stageForTest(t, svc, userID, uuid.NewString(), []byte("screenshot"))
	}
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	_, _, _, allowed, err := svc.Read(context.Background(), storageKeyForTest(id))
	require.NoError(t, err)
	require.False(t, allowed, "expired pending bytes are denied before cleanup")
	count, err := svc.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count)
	count, err = svc.Sweep(context.Background())
	require.NoError(t, err)
	require.Zero(t, count)
	var state string
	var staged []byte
	var metadata int
	require.NoError(t, db.QueryRow(`SELECT state,staged_data FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &staged))
	require.Equal(t, "deleted", state)
	require.Nil(t, staged)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key=$1`, storageKeyForTest(id)).Scan(&metadata))
	require.Zero(t, metadata)
	stageForTest(t, svc, userID, uuid.NewString(), []byte("new pending slot"))
}

func TestLateRejectionAfterUploadExpiryReleasesPendingEvidenceForSweep(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	id := uuid.NewString()
	path := publicPathForTest(id)
	stageForTest(t, svc, userID, id, []byte("rejected report image"))
	receipt := pinForTest(t, svc, userID, []string{path})
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	require.NoError(t, svc.ReleaseRejected(ctx, receipt, []string{path}))
	var state string
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT state,pin_attempts FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &attempts))
	require.Equal(t, "pending", state)
	require.Zero(t, attempts)
	count, err := svc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count, "after definitive rejection, expired evidence is reclaimable")
	require.NoError(t, db.QueryRow(`SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state))
	require.Equal(t, "deleted", state)
}

func TestSweepDeleteFailureRollsBackLifecycleTransition(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	otherID := uuid.NewString()
	stageForTest(t, svc, userID, id, []byte("screenshot"))
	stageForTest(t, svc, userID, otherID, []byte("other screenshot"))
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key IN ($1,$2)`, id, otherID)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE test_feedback_screenshot_delete_failure (storage_key TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO test_feedback_screenshot_delete_failure (storage_key) VALUES ($1)`, storageKeyForTest(id))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE FUNCTION fail_feedback_screenshot_metadata_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM test_feedback_screenshot_delete_failure WHERE storage_key=OLD.storage_key) THEN
				RAISE EXCEPTION 'forced metadata delete failure';
			END IF;
			RETURN OLD;
		END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER fail_feedback_screenshot_metadata_delete BEFORE DELETE ON media_files FOR EACH ROW EXECUTE FUNCTION fail_feedback_screenshot_metadata_delete()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS fail_feedback_screenshot_metadata_delete ON media_files`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS fail_feedback_screenshot_metadata_delete()`)
		_, _ = db.Exec(`DROP TABLE IF EXISTS test_feedback_screenshot_delete_failure`)
	})
	count, err := svc.Sweep(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, count, "a failed row must not block cleanup of its sibling")
	var state string
	var staged []byte
	var metadata int
	require.NoError(t, db.QueryRow(`SELECT state,staged_data FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &staged))
	require.Equal(t, "pending", state)
	require.NotEmpty(t, staged)
	var retryAt time.Time
	require.NoError(t, db.QueryRow(`SELECT retry_at FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&retryAt))
	require.True(t, retryAt.After(time.Now()), "failed cleanup must remain charged with a retry lease")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM media_files WHERE storage_key=$1`, storageKeyForTest(id)).Scan(&metadata))
	require.Equal(t, 1, metadata)
	require.NoError(t, db.QueryRow(`SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1`, otherID).Scan(&state))
	require.Equal(t, "deleted", state)
}

func TestCleanupCannotDeleteImagePinnedAfterExpiryClaim(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	data := []byte("report submitted during cleanup")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	pinForTest(t, svc, userID, []string{path})
	require.ErrorIs(t, svc.reclaimOne(context.Background(), uuid.MustParse(id)), ErrInvalid)
	got, _, remote, allowed, err := svc.Read(context.Background(), storageKeyForTest(id))
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, remote)
	require.Equal(t, data, got)
}

func TestOffloadRequiresConfirmedPublicationAndUsesStagedBytes(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	id := uuid.NewString()
	data := []byte("retain and offload these exact bytes")
	stageForTest(t, svc, userID, id, data)
	putter := &fakePutter{}
	checker := &allowChecker{}
	count, err := svc.Offload(ctx, putter, checker)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Empty(t, putter.keys)
	require.Zero(t, checker.calls)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(ctx, receipt, []string{path}))
	count, err = svc.Offload(ctx, putter, checker)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, []string{storageKeyForTest(id)}, putter.keys)
	require.Equal(t, data, putter.data[storageKeyForTest(id)])
	require.Equal(t, 1, checker.calls)
	got, _, remote, allowed, err := svc.Read(ctx, storageKeyForTest(id))
	require.NoError(t, err)
	require.True(t, allowed)
	require.True(t, remote)
	require.Nil(t, got)
}

func TestOffloadFailureKeepsStagedBytesForRetry(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	id := uuid.NewString()
	data := []byte("retryable bytes")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(ctx, receipt, []string{path}))
	putter := &fakePutter{err: errors.New("object store unavailable")}
	count, err := svc.Offload(ctx, putter, &allowChecker{})
	require.Error(t, err)
	require.Zero(t, count)
	got, _, remote, allowed, err := svc.Read(ctx, storageKeyForTest(id))
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, remote)
	require.Equal(t, data, got)
	_, err = db.Exec(`UPDATE feedback_screenshot_uploads SET retry_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	putter.err = nil
	count, err = svc.Offload(ctx, putter, &allowChecker{})
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestOffloadConfirmationFailureKeepsSourceBytesAndAttemptCharge(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	data := []byte("source must survive failed confirmation")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{path}))
	_, err := db.Exec(`CREATE FUNCTION reject_feedback_screenshot_offload_confirmation() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.put_completed AND NOT OLD.put_completed THEN
				RAISE EXCEPTION 'forced offload confirmation failure';
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_feedback_screenshot_offload_confirmation BEFORE UPDATE ON feedback_screenshot_uploads
		FOR EACH ROW EXECUTE FUNCTION reject_feedback_screenshot_offload_confirmation()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS reject_feedback_screenshot_offload_confirmation ON feedback_screenshot_uploads`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS reject_feedback_screenshot_offload_confirmation()`)
	})
	putter := &fakePutter{}
	count, err := svc.Offload(context.Background(), putter, &allowChecker{})
	require.ErrorContains(t, err, "confirm screenshot offload")
	require.Zero(t, count)
	require.Equal(t, data, putter.data[storageKeyForTest(id)])
	var staged []byte
	var attempts int
	var completed bool
	require.NoError(t, db.QueryRow(`SELECT staged_data,put_attempts,put_completed FROM feedback_screenshot_uploads WHERE object_key=$1`, id).
		Scan(&staged, &attempts, &completed))
	require.Equal(t, data, staged, "a remote success without durable confirmation must retain the source")
	require.Equal(t, 1, attempts, "the uncertain remote write must remain charged")
	require.False(t, completed)
	_, err = db.Exec(`DROP TRIGGER reject_feedback_screenshot_offload_confirmation ON feedback_screenshot_uploads`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE feedback_screenshot_uploads SET retry_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	count, err = svc.Offload(context.Background(), putter, &allowChecker{})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT staged_data,put_attempts,put_completed FROM feedback_screenshot_uploads WHERE object_key=$1`, id).
		Scan(&staged, &attempts, &completed))
	require.Nil(t, staged)
	require.Equal(t, 2, attempts)
	require.True(t, completed)
}

func TestOffloadConfirmsRemoteSuccessAfterWorkerCancellation(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	data := []byte("remote write completed before cancellation")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{path}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	putter := &fakePutter{onPut: cancel}
	count, err := svc.Offload(ctx, putter, &allowChecker{})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, data, putter.data[storageKeyForTest(id)])
	var staged []byte
	var completed bool
	require.NoError(t, db.QueryRow(`SELECT staged_data,put_completed FROM feedback_screenshot_uploads WHERE object_key=$1`, id).
		Scan(&staged, &completed))
	require.Nil(t, staged)
	require.True(t, completed)
}

func TestOffloadNeverWritesObjectWithoutDurableAttemptCharge(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	data := []byte("charge before remote write")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{path}))
	_, err := db.Exec(`CREATE FUNCTION reject_feedback_screenshot_attempt_charge() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.put_attempts > OLD.put_attempts THEN
				RAISE EXCEPTION 'forced attempt charge failure';
			END IF;
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_feedback_screenshot_attempt_charge BEFORE UPDATE ON feedback_screenshot_uploads
		FOR EACH ROW EXECUTE FUNCTION reject_feedback_screenshot_attempt_charge()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS reject_feedback_screenshot_attempt_charge ON feedback_screenshot_uploads`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS reject_feedback_screenshot_attempt_charge()`)
	})
	putter := &fakePutter{}
	count, err := svc.Offload(context.Background(), putter, &allowChecker{})
	require.ErrorContains(t, err, "charge screenshot offload attempt")
	require.Zero(t, count)
	require.Empty(t, putter.keys, "no object version is created before the attempt is charged")
	var staged []byte
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT staged_data,put_attempts FROM feedback_screenshot_uploads WHERE object_key=$1`, id).
		Scan(&staged, &attempts))
	require.Equal(t, data, staged)
	require.Zero(t, attempts)
}

func TestOffloadHonorsDiskGateAndCapsObjectStoreAttempts(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	id := uuid.NewString()
	data := []byte("do not discard on disk refusal")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(ctx, receipt, []string{path}))
	putter := &fakePutter{}
	checker := &allowChecker{err: errors.New("disk watermark refused")}
	count, err := svc.Offload(ctx, putter, checker)
	require.Error(t, err)
	require.Zero(t, count)
	require.Empty(t, putter.keys, "the disk check must run before any object-store write")
	var staged []byte
	require.NoError(t, db.QueryRow(`SELECT staged_data FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&staged))
	require.Equal(t, data, staged)
	for attempt := 0; attempt < maxPutAttempts; attempt++ {
		_, err = db.Exec(`UPDATE feedback_screenshot_uploads SET retry_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
		require.NoError(t, err)
		putter.err = errors.New("object store uncertain failure")
		checker.err = nil
		count, err = svc.Offload(ctx, putter, checker)
		require.Error(t, err)
		require.Zero(t, count)
	}
	_, err = db.Exec(`UPDATE feedback_screenshot_uploads SET retry_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	putCount := len(putter.keys)
	count, err = svc.Offload(ctx, putter, checker)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Len(t, putter.keys, putCount, "no fourth remote attempt may be made after the durable cap")
	require.NoError(t, db.QueryRow(`SELECT staged_data FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&staged))
	require.Equal(t, data, staged, "exhausting remote attempts must preserve readable charged evidence")
}

func TestExpiredUnresolvedPinBecomesRetainedAndOffloadEligible(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	ctx := context.Background()
	id := uuid.NewString()
	data := []byte("pending report evidence")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET retained_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, id)
	require.NoError(t, err)
	putter := &fakePutter{}
	count, err := svc.Offload(ctx, putter, &allowChecker{})
	require.NoError(t, err)
	require.Equal(t, 1, count, "the expiry transition makes unresolved evidence permanent and offloadable")
	var published, retained bool
	var state string
	require.NoError(t, db.QueryRow(`SELECT state,published,retained FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &published, &retained))
	require.Equal(t, "pinned", state)
	require.False(t, published, "retention must not claim a report was published")
	require.True(t, retained)
	require.Equal(t, data, putter.data[storageKeyForTest(id)])
	require.NoError(t, svc.ReleaseRejected(ctx, receipt, []string{path}), "a late rejection cannot undo the retention boundary")
	require.NoError(t, db.QueryRow(`SELECT state,published,retained FROM feedback_screenshot_uploads WHERE object_key=$1`, id).Scan(&state, &published, &retained))
	require.Equal(t, "pinned", state)
	require.False(t, published)
	require.True(t, retained)
}

func TestWorkerReclaimsExpiredUploadAndOffloadsPublishedImageAtStartup(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	expiredID := uuid.NewString()
	publishedID := uuid.NewString()
	data := []byte("published screenshot")
	stageForTest(t, svc, userID, expiredID, []byte("abandoned"))
	stageForTest(t, svc, userID, publishedID, data)
	receipt := pinForTest(t, svc, userID, []string{publicPathForTest(publishedID)})
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{publicPathForTest(publishedID)}))
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET expires_at=NOW()-INTERVAL '1 second' WHERE object_key=$1`, expiredID)
	require.NoError(t, err)

	var logs bytes.Buffer
	putter := &fakePutter{}
	ctx, cancel := context.WithCancel(context.Background())
	wait := svc.Start(ctx, putter, &allowChecker{}, logger.NewWithWriter(&logs))
	t.Cleanup(func() { cancel(); wait() })
	require.Eventually(t, func() bool {
		var expiredState string
		var offloaded bool
		err := db.QueryRow(`SELECT state FROM feedback_screenshot_uploads WHERE object_key=$1`, expiredID).Scan(&expiredState)
		if err != nil {
			return false
		}
		err = db.QueryRow(`SELECT put_completed FROM feedback_screenshot_uploads WHERE object_key=$1`, publishedID).Scan(&offloaded)
		return err == nil && expiredState == "deleted" && offloaded
	}, 3*time.Second, 10*time.Millisecond, "startup pass should process both classes without waiting for a tick")
	cancel()
	wait()
	require.Equal(t, data, putter.data[storageKeyForTest(publishedID)])
	require.Contains(t, logs.String(), "feedback screenshot lifecycle pass")
}

func TestWorkerLogsOffloadFailureAndKeepsPublishedBytes(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	data := []byte("must survive failed offload")
	stageForTest(t, svc, userID, id, data)
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{path}))
	var logs bytes.Buffer
	svc.runPass(context.Background(), &fakePutter{err: errors.New("object store unavailable")}, &allowChecker{}, logger.NewWithWriter(&logs))
	require.Contains(t, logs.String(), "feedback screenshot offload failed")
	got, _, remote, allowed, err := svc.Read(context.Background(), storageKeyForTest(id))
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, remote)
	require.Equal(t, data, got)
}

func TestWorkerReportsCappedOffloadsAndStatusFailures(t *testing.T) {
	db, userID := setup(t)
	svc := New(db)
	id := uuid.NewString()
	stageForTest(t, svc, userID, id, []byte("still charged after capped attempts"))
	path := publicPathForTest(id)
	receipt := pinForTest(t, svc, userID, []string{path})
	require.NoError(t, svc.ConfirmPublished(context.Background(), receipt, []string{path}))
	_, err := db.Exec(`UPDATE feedback_screenshot_uploads SET put_attempts=$2 WHERE object_key=$1`, id, maxPutAttempts)
	require.NoError(t, err)
	var logs bytes.Buffer
	log := logger.NewWithWriter(&logs)
	svc.reportCappedOffloads(context.Background(), log)
	require.Contains(t, logs.String(), "feedback screenshot offload needs operator reconciliation")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.reportCappedOffloads(ctx, log)
	require.Contains(t, logs.String(), "feedback screenshot capped-offload check failed")
}

type fakePutter struct {
	keys  []string
	data  map[string][]byte
	err   error
	onPut func()
}

func (f *fakePutter) PutObject(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return f.err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.data == nil {
		f.data = make(map[string][]byte)
	}
	f.data[key] = data
	if f.onPut != nil {
		f.onPut()
	}
	return nil
}

type allowChecker struct {
	calls int
	err   error
}

func (c *allowChecker) Check() error { c.calls++; return c.err }
