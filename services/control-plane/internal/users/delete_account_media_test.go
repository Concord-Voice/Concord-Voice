package users_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// recordingReclaimer captures what the erasure handed to the object-storage
// half, so a test can assert on the SET rather than on a delete side effect.
type recordingReclaimer struct {
	mu     sync.Mutex
	called bool
	tier1  []media.BlobRef
	tier2  []media.BlobRef
}

func (r *recordingReclaimer) reclaim(_ context.Context, tier1, tier2 []media.BlobRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	r.tier1 = append(r.tier1, tier1...)
	r.tier2 = append(r.tier2, tier2...)
}

func (r *recordingReclaimer) keys(refs []media.BlobRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Key)
	}
	return out
}

// insertTier1Row inserts one tier-1 media_files row, so a test can construct the
// exact ownership shapes the narrowing has to distinguish between.
func insertTier1Row(t *testing.T, ts *testhelpers.TestServer, uploaderID, key string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, mime_type, file_size, storage_key)
		VALUES ($1, 'photo', 1, 'image/webp', 1024, $2)`,
		uploaderID, key)
	require.NoError(t, err)
}

// insertTier2Row inserts one tier-2 attachment row. The valid_media_context
// CHECK (000042, re-expressed by 000062) demands key_version plus exactly one of
// channel_id / conversation_id for that tier.
func insertTier2Row(t *testing.T, ts *testhelpers.TestServer, uploaderID, channelID, key string) {
	t.Helper()
	_, err := ts.DB.Exec(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version, channel_id,
		                         mime_type, file_size, storage_key)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1024, $3)`,
		uploaderID, channelID, key)
	require.NoError(t, err)
}

// TestDeleteAccountCapturesOwnMediaOnly is THE regression test for this change,
// and the assertion that matters is the NEGATIVE one.
//
// A bare `WHERE uploader_id = $1` capture passes every positive assertion below
// and still ships a data-destruction bug: `server-icons/<serverID>` and
// `dm-icons/<conversationID>` rows carry the uploader's id (and
// insertTier1Record's ON CONFLICT REBINDS them to whoever changed the icon
// last), but the objects belong to a server and a conversation that survive
// this erasure. proxyTier1Media serves them by key without ever reading
// media_files, so deleting them blanks a live server's icon and nothing else in
// the system notices.
func TestDeleteAccountCapturesOwnMediaOnly(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "erasemediaowner")
	uploader := ts.CreateTestUser(t, "erasemediauploader")

	// A server owned by SOMEBODY ELSE, so the erasure cannot cascade it away.
	serverID := ts.CreateTestServer(t, owner.ID, "icon-bearing server")
	channelID := ts.CreateTestChannel(t, serverID, "attachments")
	conversationID := ts.CreateGroupDMConversation(t, owner.ID, uploader.ID)

	attachmentKey := "attachments/" + uploader.ID
	avatarKey := "avatars/" + uploader.ID
	bannerKey := "banners/" + uploader.ID
	serverIconKey := "server-icons/" + serverID
	serverBannerKey := "server-banners/" + serverID
	dmIconKey := "dm-icons/" + conversationID

	insertTier2Row(t, ts, uploader.ID, channelID, attachmentKey)
	insertTier1Row(t, ts, uploader.ID, avatarKey)
	insertTier1Row(t, ts, uploader.ID, bannerKey)
	// Uploaded BY the erased user, owned by subjects that outlive them.
	insertTier1Row(t, ts, uploader.ID, serverIconKey)
	insertTier1Row(t, ts, uploader.ID, serverBannerKey)
	insertTier1Row(t, ts, uploader.ID, dmIconKey)

	rec := &recordingReclaimer{}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetErasedMediaReclaimer(rec.reclaim)

	require.NoError(t, svc.DeleteAccount(context.Background(), uploader.ID))
	require.True(t, rec.called, "the reclaimer must run for a user who uploaded media")

	assert.ElementsMatch(t, []string{avatarKey, bannerKey}, rec.keys(rec.tier1),
		"tier-1 capture must be exactly the two keys whose SUBJECT is the erased user")
	assert.ElementsMatch(t, []string{attachmentKey}, rec.keys(rec.tier2),
		"tier-2 capture must be the user's own attachments")

	// The negative assertion, stated separately so a failure names the defect.
	for _, shared := range []string{serverIconKey, serverBannerKey, dmIconKey} {
		assert.NotContains(t, rec.keys(rec.tier1), shared,
			"capture must NOT include %s: the object belongs to a subject that survives this erasure", shared)
		assert.NotContains(t, rec.keys(rec.tier2), shared,
			"capture must NOT include %s on the tier-2 leg either", shared)
	}
}

// TestDeleteAccountCaptureCarriesBackend proves the capture keeps the PAIR, not
// just the key. A ref that reached the delete rail with a nil backend when its
// row named one would be deleted from the wrong bucket -- and an S3 DELETE of
// an absent key returns SUCCESS, so that failure is silent.
func TestDeleteAccountCaptureCarriesBackend(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasemediabackend")

	channelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, user.ID, "backend"), "general")
	_, err := ts.DB.Exec(`
		INSERT INTO media_files (uploader_id, file_type, media_tier, key_version, channel_id,
		                         mime_type, file_size, storage_key, storage_backend)
		VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 10, $3, 'r2')`,
		user.ID, channelID, "attachments/"+user.ID)
	require.NoError(t, err)

	rec := &recordingReclaimer{}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetErasedMediaReclaimer(rec.reclaim)

	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))
	require.Len(t, rec.tier2, 1)
	require.NotNil(t, rec.tier2[0].Backend, "a row naming a backend must not capture as NULL")
	assert.Equal(t, "r2", *rec.tier2[0].Backend)
}

// TestDeleteAccountWithoutReclaimerStillErases pins the deliberate degrade: an
// unwired reclaimer leaves the pre-existing leak but must never fail a GDPR
// Article 17 erasure.
func TestDeleteAccountWithoutReclaimerStillErases(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasemedianowire")
	channelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, user.ID, "nowire"), "general")
	insertTier2Row(t, ts, user.ID, channelID, "attachments/"+user.ID)

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))

	var rows int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID).Scan(&rows))
	assert.Equal(t, 0, rows)
}

// TestDeleteAccountSkipsAlreadySoftDeletedRows pins the handoff to the orphan
// reaper. A soft-deleted row is deliberately outside the capture (it does not
// ride idx_media_files_uploader, which is partial on `deleted_at IS NULL`, so
// admitting it would mean a sequential scan under the erasure's user-row lock).
// If this ever starts passing the key through, the orphan reaper's justification
// changes and this comment is the place to notice.
func TestDeleteAccountSkipsAlreadySoftDeletedRows(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasemediasoftdel")

	channelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, user.ID, "softdel"), "general")
	softDeleted := "attachments/soft-" + user.ID
	insertTier2Row(t, ts, user.ID, channelID, softDeleted)
	_, err := ts.DB.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE storage_key = $1`, softDeleted)
	require.NoError(t, err)

	live := "attachments/live-" + user.ID
	insertTier2Row(t, ts, user.ID, channelID, live)

	rec := &recordingReclaimer{}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetErasedMediaReclaimer(rec.reclaim)

	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))
	assert.ElementsMatch(t, []string{live}, rec.keys(rec.tier2),
		"soft-deleted rows are the orphan reaper's input, not the capture's")
}

// TestReclaimErasedMediaDetachesRequestContext proves the reclamation survives
// request-context cancellation.
//
// THE WINDOW IS REAL. DeleteAccount is handed c.Request.Context()
// (privacy/handler.go), which cancels the instant the client disconnects, and
// account deletion is exactly the flow where someone confirms and then closes
// the app while the erasure is still doing its sender-gated suppression,
// presence capture and cascade. By the time the reclaimer runs the users row is
// GONE, so a live cancelled context would strand the objects with nothing able
// to retry -- and on the tier-1 leg that residue is plaintext, permanent, and
// outside the orphan reaper's tier-2 scope.
//
// Driven at the unit rather than through DeleteAccount because the window is
// unreachable from outside: cancelling before the call fails BeginTx (no
// erasure, nothing to reclaim), and cancelling during it means racing a commit.
func TestReclaimErasedMediaDetachesRequestContext(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, ctx.Err(), "precondition: the parent context is already cancelled")

	var reclaimErr error
	var deadline bool
	var reclaimed int
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetErasedMediaReclaimer(func(reclaimCtx context.Context, tier1, tier2 []media.BlobRef) {
		reclaimErr = reclaimCtx.Err()
		_, deadline = reclaimCtx.Deadline()
		reclaimed = len(tier1) + len(tier2)
	})

	users.ReclaimErasedMediaForTest(ctx, svc,
		[]media.BlobRef{media.NewBlobRef("avatars/x", sql.NullString{})},
		[]media.BlobRef{media.NewBlobRef("attachments/y", sql.NullString{})})

	require.Equal(t, 2, reclaimed, "both legs must reach the reclaimer")
	assert.NoError(t, reclaimErr,
		"the reclaimer got a cancelled context: its object deletes would fail with the "+
			"users row already gone and no sweep able to retry the tier-1 leg")
	assert.True(t, deadline,
		"detached but unbounded would let an unresponsive backend hold the request goroutine open")
}

// TestDeleteAccountCapturesAttachmentsInItsOwnIncompleteChannels pins the
// capture's POSITION in the transaction, which nothing else does.
//
// deleteAccountTx captures BEFORE deleteIncompleteChannelsTx, not merely before
// the users DELETE, because media_files.channel_id is ALSO ON DELETE CASCADE
// (000042) — dropping the erased user's incomplete E2EE channels already
// destroys the attachment rows inside them. Every other test in this file hangs
// its tier-2 rows off a channel created by CreateTestChannel, which never writes
// channel_initial_key_distributions, so deleteIncompleteChannelsTx deletes
// nothing and the ordering is unexercised: move the capture three lines down and
// the whole suite stays green while permanently leaking exactly these objects.
//
// Seeding that table is what makes the ordering observable.
func TestDeleteAccountCapturesAttachmentsInItsOwnIncompleteChannels(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "eraseincompleteowner")
	creator := ts.CreateTestUser(t, "eraseincompletecreator")

	serverID := ts.CreateTestServer(t, owner.ID, "incomplete-channel server")
	ts.AddMemberToServer(t, serverID, creator.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "incomplete-channel")

	// This row is what makes the channel "incomplete" and therefore deleted by
	// deleteIncompleteChannelsTx during the erasure.
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`,
		channelID, creator.ID)
	require.NoError(t, err)

	attachmentKey := "attachments/incomplete-" + creator.ID
	insertTier2Row(t, ts, creator.ID, channelID, attachmentKey)

	rec := &recordingReclaimer{}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetErasedMediaReclaimer(rec.reclaim)

	require.NoError(t, svc.DeleteAccount(context.Background(), creator.ID))

	assert.Contains(t, rec.keys(rec.tier2), attachmentKey,
		"the capture must run BEFORE deleteIncompleteChannelsTx: that call cascades "+
			"media_files.channel_id, so a capture placed after it never sees this row "+
			"and the object leaks with nothing able to find it")
}

// TestDeleteAccountCapturesSoftDeletedProfileMedia is the regression for the
// tier-1 half of erasedMediaQuery's split filter.
//
// media.CleanupObject soft-deletes a tier-1 row even when its DeleteObject
// FAILED, so a soft-deleted `avatars/<uid>` row can still have a live object.
// Applying `deleted_at IS NULL` to the tier-1 arm — as an earlier revision did —
// strands it permanently: the straggler sweep and the orphan reaper are both
// media_tier = 2, and the residue is server-decoded PLAINTEXT.
func TestDeleteAccountCapturesSoftDeletedProfileMedia(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasesoftdeletedavatar")

	avatarKey := "avatars/" + user.ID
	insertTier1Row(t, ts, user.ID, avatarKey)
	_, err := ts.DB.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE storage_key = $1`, avatarKey)
	require.NoError(t, err)

	rec := &recordingReclaimer{}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetErasedMediaReclaimer(rec.reclaim)

	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))

	assert.Contains(t, rec.keys(rec.tier1), avatarKey,
		"a soft-deleted profile-media row may still have a LIVE object (CleanupObject "+
			"soft-deletes even when the delete failed), and no tier-2 sweep can reach it")
}
