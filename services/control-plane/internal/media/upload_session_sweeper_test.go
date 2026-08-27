package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sweeper's whole reason to exist: it must reclaim bytes when the session
// records are GONE. Every other cleanup path -- the client's cancel, the
// unmount keepalive DELETE, the 410 expiry -- reads or writes a Redis session
// record, so all of them fail together the moment Redis does. That is exactly
// when uploads get orphaned, which is why a Redis-derived sweeper would be
// useless precisely when it is needed.
//
// This test therefore FLUSHES REDIS rather than mocking it. Mocking would prove
// the loop iterates; flushing proves the design survives the failure it was
// built for.
func TestSweeper_AbortsOrphansAfterTotalRedisLoss(t *testing.T) {
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sweeporphan")

	sessionID, _ := ss.initOK(t, userID, channelID, 4096)
	ss.uploadAllParts(t, userID, sessionID, sliceParts(t, fakeCiphertext(4096), 4096))
	require.Equal(t, 1, ss.fake.openUploadCount(), "the part should be staged in the store")

	// Total Redis loss. Not a delete of one key -- everything the session layer
	// knows is gone, which is the state the sweeper has to work from.
	//
	// redistest.Reset, NOT FlushDB: concurrent sessions share one Redis server,
	// and a bare flush would wipe a database this process does not own (#2680).
	// Reset refuses to do that, and still gives this test the total loss it
	// needs, because the DB it clears is the one this test was allocated.
	require.NoError(t, redistest.Reset(context.Background(), ss.rdb))

	// VERIFY the precondition rather than assume it. The sweeper never reads
	// Redis, so this test would pass identically if the reset silently did
	// nothing -- and the name would then be claiming a resilience property the
	// test never put the code in a position to demonstrate. Proving the session
	// record is really gone is what makes "after total Redis loss" a fact here
	// rather than a label.
	keys, err := ss.rdb.Keys(context.Background(), "attach_sess*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "the session state must actually be gone")

	// Age it past the hard TTL plus the grace margin.
	for id := range ss.fake.uploads {
		ss.fake.backdateUpload(id, sessionSweepMinAge()+time.Minute)
	}

	res, err := NewSessionSweeper(ss.fake, logger.New("test")).SweepAbandoned(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Swept)
	assert.Equal(t, 0, ss.fake.openUploadCount(),
		"the orphaned multipart upload must be aborted even with no session record")
}

func TestSweeper_LeavesLiveSessionsAlone(t *testing.T) {
	// A session inside its hard TTL is still usable: the user may be on chunk 3
	// of 32 with a slow link. Aborting it would turn a working upload into a
	// failure the client cannot even explain.
	ss := setupSessionTest(t)
	userID, _, channelID := ss.channelContext(t, "sweeplive")

	sessionID, _ := ss.initOK(t, userID, channelID, 4096)
	ss.uploadAllParts(t, userID, sessionID, sliceParts(t, fakeCiphertext(4096), 4096))

	res, err := NewSessionSweeper(ss.fake, logger.New("test")).SweepAbandoned(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Swept)
	assert.Equal(t, 1, ss.fake.openUploadCount())
}

func TestSweeper_MinAgeExceedsTheHardTTL(t *testing.T) {
	// Locks the derivation, not the number. Past the hard TTL every route
	// answers 410, but a request that entered the handler just before it lapsed
	// is still running -- aborting its multipart upload underneath it turns a
	// completing upload into a corrupt one. If someone later "simplifies"
	// sessionSweepMinAge to equal the TTL, that race is armed and this fails.
	assert.Greater(t, sessionSweepMinAge(), uploadSessionHardTTL,
		"the sweep cutoff must sit strictly past the hard TTL, with margin")
}

func TestSweeper_IgnoresUploadsOutsideTheAttachmentPrefix(t *testing.T) {
	// ListMultipartUploads returns every incomplete upload in the BUCKET.
	// Aborting one that belongs to another code path would destroy a stranger's
	// in-flight write -- a data-loss bug dressed as a cleanup job.
	fake := newFakeMultipartStore()
	foreign := fake.startForeignUpload("backups/nightly.tar", sessionSweepMinAge()+time.Hour)

	res, err := NewSessionSweeper(fake, logger.New("test")).SweepAbandoned(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Swept)
	assert.NotContains(t, fake.aborted, foreign)
	assert.Equal(t, 1, fake.openUploadCount(), "a foreign upload must survive the sweep")
}

func TestSweeper_ContinuesPastAFailedAbort(t *testing.T) {
	// One permanently broken key must not block every later reclaim forever.
	//
	// ONLY THE FIRST ABORT FAILS, and that is what makes this test mean anything.
	// The original failed EVERY abort, so "reclaimed 0, still 2 open" held whether
	// the sweeper continued past the error or stopped dead at it -- continue and
	// break produced identical state and the assertion could not tell them apart
	// (found by CodeRabbit on PR 2931). With a single failure the two diverge:
	// continuing reclaims the second upload, breaking leaves both.
	fake := newFakeMultipartStore()
	fake.startForeignUpload("attachments/a", sessionSweepMinAge()+time.Hour)
	fake.startForeignUpload("attachments/b", sessionSweepMinAge()+time.Hour)
	fake.abortFailFirst = 1

	res, err := NewSessionSweeper(fake, logger.New("test")).SweepAbandoned(context.Background())

	require.NoError(t, err, "a failed abort is logged, not returned -- the batch continues")
	assert.Equal(t, 1, res.Swept, "the upload after the failure must still be reclaimed")
	assert.Equal(t, 2, fake.abortAttempts, "the sweeper must ATTEMPT both, not stop at the first error")
	assert.Equal(t, 1, fake.openUploadCount(), "exactly the broken one is left behind")
}

func TestSweeper_ReportsNothingReclaimedWhenEveryAbortFails(t *testing.T) {
	// The blanket-failure case is still worth asserting -- it just cannot carry
	// the continue-vs-break claim, which is why it is now its own test.
	fake := newFakeMultipartStore()
	fake.startForeignUpload("attachments/a", sessionSweepMinAge()+time.Hour)
	fake.startForeignUpload("attachments/b", sessionSweepMinAge()+time.Hour)
	fake.abortErr = errors.New("object store unavailable")

	res, err := NewSessionSweeper(fake, logger.New("test")).SweepAbandoned(context.Background())

	// A sweep in which EVERY abort failed must report an ERROR, not a clean zero.
	// Reporting success here is what let a dead reclaim path -- one lost IAM
	// permission -- look identical to a quiet one, on the component every other
	// best-effort cleanup path in this feature defers to.
	require.Error(t, err, "a totally failed sweep must not report success")
	assert.Contains(t, err.Error(), "reclaim is not working")
	assert.Equal(t, 0, res.Swept)
	assert.Equal(t, 2, res.Attempted, "the attempt count is what makes a dead sweep visible")
	assert.Equal(t, 2, res.Failed)
	assert.Equal(t, 2, fake.abortAttempts, "both were attempted")
	assert.Equal(t, 2, fake.openUploadCount(), "nothing was reclaimed, and nothing was lost")
}

func TestSweeper_SurfacesAListingFailure(t *testing.T) {
	// A listing failure is different from an abort failure: there is no work
	// queue at all, so reporting 0-swept-no-error would read as "nothing to do".
	fake := newFakeMultipartStore()
	fake.listErr = errors.New("object store unavailable")

	_, err := NewSessionSweeper(fake, logger.New("test")).SweepAbandoned(context.Background())
	require.Error(t, err)
}
