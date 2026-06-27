// Package storage provides an S3-compatible object storage client for media file management.
// It wraps the MinIO SDK and handles bucket initialization, object upload/download,
// presigned URL generation, and object deletion.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// ErrObjectNotFound is returned by read operations when the object does not exist.
// Consumers classify with errors.Is(err, ErrObjectNotFound) — the minio SDK error
// shape stays confined to this package (#1611).
var ErrObjectNotFound = errors.New("storage: object not found")

// mapNotFound converts a minio NoSuchKey error into ErrObjectNotFound; nil stays nil
// and any other error passes through unchanged. Uses the structured SDK error code
// (matching ObjectExists), not a fragile err.Error() substring match.
func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return ErrObjectNotFound
	}
	return err
}

// Client wraps the MinIO SDK client with application-specific operations.
type Client struct {
	minio  *minio.Client
	bucket string
	log    *logger.Logger
}

// New selects the storage backend by cfg.StorageBackend and returns a ready client.
// All currently-supported backends are S3-compatible, so there is exactly one
// constructor and NO backend branch in the hot path — the switch is construction-time
// only (#1611 / ADR-0024).
func New(cfg *config.Config, log *logger.Logger) (*Client, error) {
	switch cfg.StorageBackend {
	case "minio", "s3", "r2", "b2":
		return newS3Client(cfg, log)
	default:
		return nil, fmt.Errorf("storage: unknown STORAGE_BACKEND %q (want one of: minio, s3, r2, b2)", cfg.StorageBackend)
	}
}

// s3Options maps the resolved STORAGE_* config onto minio-go client options.
// Pure (no network) so the credential/region/TLS wiring is unit-testable.
func s3Options(cfg *config.Config) *minio.Options {
	return &minio.Options{
		// NewStaticV4's 3rd arg is the STS session token, NOT the region — pass "".
		// The region is supplied only via Options.Region below. (#1611 Gitar review.)
		Creds:  credentials.NewStaticV4(cfg.StorageAccessKey, cfg.StorageSecretKey, ""),
		Secure: cfg.StorageUseSSL,
		Region: cfg.StorageRegion,
	}
}

// newS3Client builds the single S3-compatible (minio-go) client and ensures the bucket.
func newS3Client(cfg *config.Config, log *logger.Logger) (*Client, error) {
	mc, err := minio.New(cfg.StorageEndpoint, s3Options(cfg))
	if err != nil {
		return nil, fmt.Errorf("storage: failed to create S3 client: %w", err)
	}

	client := &Client{
		minio:  mc,
		bucket: cfg.StorageBucket,
		log:    log,
	}

	// Ensure bucket exists on startup (fail fast if the backend is unreachable)
	initCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.ensureBucket(initCtx); err != nil {
		return nil, err
	}

	log.Info("Object storage connected", "backend", cfg.StorageBackend, "endpoint", cfg.StorageEndpoint, "bucket", cfg.StorageBucket)
	return client, nil
}

// ensureBucket creates the media bucket if it does not already exist.
// The bucket is created with no public access — all access goes through
// the application layer (proxy endpoints or presigned URLs).
func (c *Client) ensureBucket(ctx context.Context) error {
	exists, err := c.minio.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("storage: failed to check bucket existence: %w", err)
	}
	if exists {
		return nil
	}

	if err := c.minio.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("storage: failed to create bucket %q: %w", c.bucket, err)
	}

	c.log.Info("Created storage bucket", "bucket", c.bucket)
	return nil
}

// PutObject uploads an object to the configured bucket.
// The key is the full object path (e.g. "avatars/user-uuid.webp").
func (c *Client) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	_, err := c.minio.PutObject(ctx, c.bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("storage: failed to put object %q: %w", key, err)
	}
	return nil
}

// GetObject retrieves an object from the configured bucket.
// The caller is responsible for closing the returned reader.
func (c *Client) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	obj, err := c.minio.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("storage: failed to get object %q: %w", key, mapNotFound(err))
	}

	// minio-go's GetObject is lazy — a missing object surfaces NoSuchKey at Stat().
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, "", fmt.Errorf("storage: failed to stat object %q: %w", key, mapNotFound(err))
	}

	return obj, info.ContentType, nil
}

// PresignedGetURL generates a time-limited download URL for an object.
// The URL is signed with the service credentials and expires after the given duration.
func (c *Client) PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	url, err := c.minio.PresignedGetObject(ctx, c.bucket, key, expires, nil)
	if err != nil {
		return "", fmt.Errorf("storage: failed to generate presigned URL for %q: %w", key, err)
	}
	return url.String(), nil
}

// DeleteObject removes an object from the configured bucket.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	err := c.minio.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("storage: failed to delete object %q: %w", key, err)
	}
	return nil
}

// ObjectExists checks whether an object exists in the configured bucket.
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.minio.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, fmt.Errorf("storage: failed to stat object %q: %w", key, err)
	}
	return true, nil
}
