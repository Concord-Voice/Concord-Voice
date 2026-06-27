package storage

import (
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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
