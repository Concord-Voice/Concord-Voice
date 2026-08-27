package storage

import (
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// New rejects an unknown backend at construction time (the factory's only branch).
func TestNew_UnknownBackendRejected(t *testing.T) {
	cfg := &config.Config{StorageBackend: "gdrive", StorageEndpoint: "x:9000"}
	_, err := New(cfg, logger.New("test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown STORAGE_BACKEND")
}

// New with a known (S3-compatible) backend but a malformed endpoint fails fast at
// client construction — covers the factory's S3 branch + newS3Client's minio.New path
// without a live backend.
func TestNew_MalformedEndpoint(t *testing.T) {
	cfg := &config.Config{StorageBackend: "minio", StorageEndpoint: "http://bad-endpoint"}
	_, err := New(cfg, logger.New("test"))
	require.Error(t, err)
}

// New against an unreachable (but format-valid) endpoint constructs the client then
// fails in ensureBucket — covers newS3Client's bucket-ensure path.
func TestNew_UnreachableBackendFailsBucketEnsure(t *testing.T) {
	cfg := &config.Config{StorageBackend: "s3", StorageEndpoint: "127.0.0.1:1", StorageBucket: "b"}
	_, err := New(cfg, logger.New("test"))
	require.Error(t, err)
}

// s3Options maps STORAGE_* config onto minio-go client options (the new credential/
// region/TLS wiring — the actual #1611 logic), with no network dependency.
func TestS3Options(t *testing.T) {
	cfg := &config.Config{
		StorageAccessKey: "ak",
		StorageSecretKey: "sk",
		StorageRegion:    "us-east-1",
		StorageUseSSL:    true,
	}
	opts := s3Options(cfg)
	require.True(t, opts.Secure)
	require.Equal(t, "us-east-1", opts.Region)
	require.NotNil(t, opts.Creds)

	// Regression guard (#1611 Gitar review): region must be wired to Options.Region
	// ONLY — it must NOT leak into NewStaticV4's 3rd arg (the STS session token).
	v, err := opts.Creds.GetWithContext(&credentials.CredContext{})
	require.NoError(t, err)
	require.Equal(t, "ak", v.AccessKeyID)
	require.Empty(t, v.SessionToken, "region must not be passed as the STS session token")
}

// mapNotFound maps a minio NoSuchKey error to the package sentinel, passes other
// errors through, and leaves nil as nil.
func TestMapNotFound(t *testing.T) {
	notFound := minio.ErrorResponse{Code: "NoSuchKey"}
	require.ErrorIs(t, mapNotFound(notFound), ErrObjectNotFound)

	other := errors.New("connection refused")
	require.False(t, errors.Is(mapNotFound(other), ErrObjectNotFound))

	require.NoError(t, mapNotFound(nil))
}

// A listing loop that trusts IsTruncated without checking that the marker moved
// spins forever on SUCCESSFUL calls: nothing to return, nothing to cancel, and
// the accumulator growing until the process dies.
func TestListingMarkersMustAdvance(t *testing.T) {
	t.Run("part marker", func(t *testing.T) {
		next, err := nextPartMarker(0, 1000)
		require.NoError(t, err)
		assert.Equal(t, 1000, next)

		for _, stuck := range []int{1000, 999, 0, -1} {
			_, err := nextPartMarker(1000, stuck)
			require.Error(t, err, "marker %d does not advance past 1000", stuck)
			assert.Contains(t, err.Error(), "did not advance")
		}
	})

	t.Run("upload marker pair", func(t *testing.T) {
		// Many uploads can share one key, so a repeated keyMarker with a moving
		// uploadIDMarker IS progress. Only the pair standing still is not.
		k, id, err := nextUploadMarkers("k", "a", "k", "b")
		require.NoError(t, err)
		assert.Equal(t, "k", k)
		assert.Equal(t, "b", id)

		_, _, err = nextUploadMarkers("k", "a", "k", "a")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not advance")

		// BACKWARDS is not progress either, though the pair did change. A
		// backend cycling A -> B -> A satisfies "different" on every hop and
		// loops forever -- the exact non-termination this guard exists to stop.
		for _, tc := range []struct{ curKey, curID, nextKey, nextID string }{
			{"k2", "b", "k1", "z"}, // key regressed
			{"k", "b", "k", "a"},   // same key, id regressed
			{"k", "b", "", ""},     // reset to the first-page markers
		} {
			_, _, err = nextUploadMarkers(tc.curKey, tc.curID, tc.nextKey, tc.nextID)
			require.Error(t, err, "%q/%q -> %q/%q is not forward progress",
				tc.curKey, tc.curID, tc.nextKey, tc.nextID)
		}

		// POSITIVE CONTROL: forward IS accepted, on both axes.
		_, _, err = nextUploadMarkers("k", "b", "k2", "a")
		require.NoError(t, err, "a later key is progress even with an earlier id")

		// The first page starts from the empty pair, so a backend that answers
		// "truncated" with empty markers is stuck too, not starting.
		_, _, err = nextUploadMarkers("", "", "", "")
		require.Error(t, err)
	})
}
