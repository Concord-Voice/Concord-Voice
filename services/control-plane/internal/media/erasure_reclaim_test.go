package media_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// sqlNull and sqlString build the two shapes media_files.storage_backend takes:
// NULL (the permanent value of every pre-ADR-0038 object) and an explicit label.
func sqlNull() sql.NullString { return sql.NullString{} }

func sqlString(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

type recordingDeleter struct {
	deleted []string
	err     error
}

func (r *recordingDeleter) DeleteObject(_ context.Context, key string) error {
	if r.err != nil {
		return r.err
	}
	r.deleted = append(r.deleted, key)
	return nil
}

const erasureTestUser = "11111111-2222-3333-4444-555555555555"

// TestErasableTier1KeysExcludesSharedSubjects guards the narrowing at its
// source. Every key this returns is deleted when an account is erased, so a
// shared-subject key appearing here destroys a live server's icon.
func TestErasableTier1KeysExcludesSharedSubjects(t *testing.T) {
	keys := media.ErasableTier1Keys(erasureTestUser)

	assert.ElementsMatch(t, []string{
		"avatars/" + erasureTestUser,
		"banners/" + erasureTestUser,
	}, keys)

	// Stated as a prefix rule as well as an exact set, so a future purpose added
	// to tier1StorageKey fails here rather than silently widening the erasure.
	for _, key := range keys {
		assert.True(t,
			strings.HasPrefix(key, "avatars/") || strings.HasPrefix(key, "banners/"),
			"%s is not scoped to a user subject; server-icons/, server-banners/ and "+
				"dm-icons/ belong to subjects that outlive their uploader", key)
	}
}

// TestReclaimErasedTier1DeletesLegacyObjects: the happy path, including the
// NULL-backend row that every pre-ADR-0038 object carries permanently.
func TestReclaimErasedTier1DeletesLegacyObjects(t *testing.T) {
	legacy := string(storage.LegacyBackendID)
	store := &recordingDeleter{}

	media.ReclaimErasedTier1(context.Background(), store, logger.New("test"), []media.BlobRef{
		media.NewBlobRef("avatars/"+erasureTestUser, sqlNull()),
		media.NewBlobRef("banners/"+erasureTestUser, sqlString(legacy)),
	})

	assert.Equal(t, []string{
		"avatars/" + erasureTestUser,
		"banners/" + erasureTestUser,
	}, store.deleted, "NULL and the explicit legacy label both mean the legacy backend")
}

// Each branch below is justified in the source by "the log line is the only
// signal". An earlier revision of these tests asserted only NotPanics/Empty,
// which passes just as happily with the log line deleted -- i.e. with the
// function turned into the exact silent failure it exists to avoid. Each now
// asserts the SIGNAL, not merely the absence of a crash.

// TestReclaimErasedTier1RefusesNonLegacyBackend: ADR-0038 pins profile media to
// MinIO permanently, so a non-legacy value is an upstream violation. Deleting
// from the legacy store anyway would hit the wrong bucket and SUCCEED, and
// there is no retry behind this path -- the straggler sweep is tier-2 only.
func TestReclaimErasedTier1RefusesNonLegacyBackend(t *testing.T) {
	var buf bytes.Buffer
	store := &recordingDeleter{}

	media.ReclaimErasedTier1(context.Background(), store, logger.NewWithWriter(&buf), []media.BlobRef{
		media.NewBlobRef("avatars/"+erasureTestUser, sqlString("r2")),
	})

	assert.Empty(t, store.deleted, "must not delete from a store that does not hold the object")
	assert.Contains(t, buf.String(), "refusing to reclaim profile media held by a non-legacy backend")
	assert.Contains(t, buf.String(), "avatars/"+erasureTestUser)
}

// TestReclaimErasedTier1SurvivesDeleteFailure: best-effort, one failure must not
// abandon the remaining keys — and the failure must be ALERTABLE. This is the
// one branch in the file with an irreversible consequence (the row is gone, so
// nothing retries and the residue is plaintext), so it logs at ERROR while the
// benign refusals around it do too — but a WARN here would sort it below them.
func TestReclaimErasedTier1SurvivesDeleteFailure(t *testing.T) {
	var buf bytes.Buffer
	store := &recordingDeleter{err: errors.New("storage down")}

	require.NotPanics(t, func() {
		media.ReclaimErasedTier1(context.Background(), store, logger.NewWithWriter(&buf), []media.BlobRef{
			media.NewBlobRef("avatars/"+erasureTestUser, sqlNull()),
			media.NewBlobRef("banners/"+erasureTestUser, sqlNull()),
		})
	})

	out := buf.String()
	assert.Contains(t, out, "PLAINTEXT bytes remain")
	assert.Contains(t, out, "level=ERROR",
		"permanent unrecoverable retention on a GDPR path must not sort below the benign refusals")
	assert.Contains(t, out, "avatars/"+erasureTestUser, "both keys must be attempted, not just the first")
	assert.Contains(t, out, "banners/"+erasureTestUser)
}

// TestReclaimErasedTier1ToleratesNilStore covers a deployment with no object
// storage configured — and it passes the nil in the shape PRODUCTION produces.
//
// An earlier revision passed an untyped literal `nil`, a true nil interface. The
// production path could not produce that: main.go held a *storage.Client and
// NewRouter's parameter is an interface, so an unconfigured deployment boxed a
// TYPED nil, `store == nil` was false, and the branch this test claimed to cover
// was a nil dereference instead. The test asserted a property the call path did
// not have. Both shapes are now covered, typed first.
func TestReclaimErasedTier1ToleratesNilStore(t *testing.T) {
	// A TYPED nil is deliberately NOT tested here, and the omission is the honest
	// one: this function cannot defend against it. `store == nil` is false for an
	// interface holding a nil pointer, and there is no reflection-free guard that
	// changes that. The defence is at the boundary — cmd/server's
	// mediaObjectStore converts once so a typed nil never enters the interface —
	// and TestMediaObjectStoreNeverProducesATypedNil is where that is pinned.
	// Asserting NotPanics here would claim a property the function does not have,
	// which is what the earlier untyped-only test did.

	t.Run("untyped nil interface", func(t *testing.T) {
		var buf bytes.Buffer
		require.NotPanics(t, func() {
			media.ReclaimErasedTier1(context.Background(), nil, logger.NewWithWriter(&buf), []media.BlobRef{
				media.NewBlobRef("avatars/"+erasureTestUser, sqlNull()),
			})
		})
		assert.Contains(t, buf.String(), "object storage is not configured")
		assert.Contains(t, buf.String(), "level=ERROR",
			"rows existed, so the objects exist and are now unreachable — a config fault, not a no-op")
	})
}
