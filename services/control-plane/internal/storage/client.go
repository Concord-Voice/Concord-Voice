// Package storage provides an S3-compatible object storage client for media file management.
// It wraps the MinIO SDK and handles bucket initialization, object upload/download,
// presigned URL generation, and object deletion.
package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Client wraps the MinIO SDK client with application-specific operations.
type Client struct {
	minio  *minio.Client
	bucket string
	log    *logger.Logger
}

// New creates a new storage client and ensures the configured bucket exists.
func New(cfg *config.Config, log *logger.Logger) (*Client, error) {
	mc, err := minio.New(cfg.MinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: failed to create MinIO client: %w", err)
	}

	client := &Client{
		minio:  mc,
		bucket: cfg.MinIOBucket,
		log:    log,
	}

	// Ensure bucket exists on startup (fail fast if MinIO is unreachable)
	initCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.ensureBucket(initCtx); err != nil {
		return nil, err
	}

	log.Info("Object storage connected", "endpoint", cfg.MinIOEndpoint, "bucket", cfg.MinIOBucket)
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
		return nil, "", fmt.Errorf("storage: failed to get object %q: %w", key, err)
	}

	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, "", fmt.Errorf("storage: failed to stat object %q: %w", key, err)
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
