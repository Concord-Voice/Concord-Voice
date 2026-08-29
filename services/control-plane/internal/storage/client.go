// Package storage provides an S3-compatible object storage client for media file management.
// It wraps the MinIO SDK and handles bucket initialization, object upload/download,
// presigned URL generation, and object deletion.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ObjectPartInfo describes one uploaded part of a multipart upload.
//
// It carries the ETag as well as the size because CompleteMultipartUpload
// requires the ETags, and the commit path takes them from the STORE's part
// listing rather than from its own session record -- so the object store stays
// authoritative about what was actually stored, and a client that misreports
// its progress cannot talk the server into completing an upload that is not
// there.
//
// These types live here rather than in internal/media because media already
// imports storage; defining them there and implementing them here would be an
// import cycle.
type ObjectPartInfo struct {
	PartNumber int
	Size       int64
	ETag       string
}

// IncompleteUpload is an abandoned multipart upload, as reported by the store.
//
// The attachment-session sweeper derives its work queue from this rather than
// from Redis on purpose: a Redis-derived sweeper fails exactly when Redis does,
// which is precisely when sessions get orphaned.
type IncompleteUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

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
	minio  *minio.Core
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

// newS3Client builds the LEGACY S3-compatible (minio-go) client and ensures the bucket.
//
// It is no longer the only construction site: newVendorClient in registry.go
// builds the non-legacy backends (ADR-0038 / #2759). That one deliberately
// does NOT call ensureBucket — the bucket-scoped vendor credential cannot
// create a bucket, and a boot-time round trip to a vendor would couple the
// whole control plane's startup to that vendor's availability. Read its doc
// comment before adding anything network-shaped to this constructor.
func newS3Client(cfg *config.Config, log *logger.Logger) (*Client, error) {
	// NewCore rather than New: Core embeds *Client (minio-go core.go:29-31), so
	// every existing method here is promoted and compiles unchanged, while the
	// low-level multipart primitives become available. Swapping the constructor
	// is the entire cost of the change.
	mc, err := minio.NewCore(cfg.StorageEndpoint, s3Options(cfg))
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
	if !exists {
		if err := c.minio.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("storage: failed to create bucket %q: %w", c.bucket, err)
		}
		c.log.Info("Created storage bucket", "bucket", c.bucket)
	}

	c.applyMultipartLifecycle(ctx)
	return nil
}

// multipartLifecycleRuleID identifies our rule inside a bucket lifecycle
// document that may contain rules this service did not write.
const multipartLifecycleRuleID = "abort-incomplete-multipart"

// applyMultipartLifecycle asks the backend to abort multipart uploads left
// incomplete for a day, UPSERTING the rule into whatever document already
// exists rather than replacing it.
//
// Applied UNCONDITIONALLY, not only when the bucket is created: every existing
// deployment already has its bucket, so a create-time-only rule would never
// reach any of them -- and a backstop nobody has is not a backstop.
//
// Best-effort by design. This is defence in depth behind the attachment-session
// sweeper, which is what the design calls load-bearing for correctness because
// it derives its work queue from the object store and therefore survives a total
// Redis loss. ADR-0024 commits to four backends (minio|s3|r2|b2) whose lifecycle
// support varies, so a backend that refuses this must not take the control plane
// down at boot. Warn and continue.
func (c *Client) applyMultipartLifecycle(ctx context.Context) {
	rule := lifecycle.Rule{
		ID:     multipartLifecycleRuleID,
		Status: "Enabled",
		AbortIncompleteMultipartUpload: lifecycle.AbortIncompleteMultipartUpload{
			DaysAfterInitiation: lifecycle.ExpirationDays(1),
		},
	}

	// UPSERT, never replace. SetBucketLifecycle writes the supplied document as
	// THE bucket lifecycle -- it is a PUT, not a PATCH -- and this runs on every
	// startup, so sending a single-rule config would silently delete any expiry,
	// transition, or retention rules an operator had configured. That is
	// unrecoverable data policy loss caused by a restart, which is a far worse
	// outcome than the missing backstop this rule provides.
	cfg, err := c.minio.GetBucketLifecycle(ctx, c.bucket)
	if err != nil {
		// ONLY "there is no document" may be treated as an empty one. Every other
		// error -- throttle, 5xx, transport -- leaves us ignorant of what the
		// bucket currently holds, and a full PUT from ignorance destroys exactly
		// the operator rules this upsert exists to preserve.
		//
		// The first version of this fix collapsed both cases into "start empty",
		// which repaired the overwrite on the happy path and RE-ARMED it on the
		// error path: one throttled GET at startup and the rules are gone. A fix
		// has to be red-teamed like the bug it replaces.
		if minio.ToErrorResponse(err).Code != "NoSuchLifecycleConfiguration" {
			c.log.Warn("Could not read the bucket lifecycle; leaving it untouched rather than "+
				"overwriting rules we cannot see. The attachment-session sweeper remains the "+
				"primary cleanup path",
				"bucket", c.bucket, "error", err)
			return
		}
		cfg = lifecycle.NewConfiguration()
	}
	if cfg == nil {
		cfg = lifecycle.NewConfiguration()
	}
	replaced := false
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == multipartLifecycleRuleID {
			cfg.Rules[i] = rule
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Rules = append(cfg.Rules, rule)
	}

	if err := c.minio.SetBucketLifecycle(ctx, c.bucket, cfg); err != nil {
		c.log.Warn("Could not set the incomplete-multipart lifecycle rule; "+
			"the attachment-session sweeper remains the primary cleanup path",
			"bucket", c.bucket, "error", err)
		return
	}
	c.log.Info("Applied incomplete-multipart lifecycle rule",
		"bucket", c.bucket, "days", 1, "rules_total", len(cfg.Rules))
}

// NOTE on c.minio.Client.X below: Core EMBEDS *Client, but it also defines 14
// methods of its own -- including PutObject, GetObject, ListObjects and
// CopyObject -- and an outer type's own methods SHADOW the promoted ones. Those
// low-level forms take different arguments (Core.PutObject wants md5Base64 and
// sha256Hex; Core.GetObject returns four values), so a bare c.minio.PutObject
// resolves to the wrong method. The explicit .Client hop selects the high-level
// API deliberately. Do not "simplify" it away.
//
// The compiler catches this, so the trap is loud rather than silent -- but the
// design note claiming the swap compiles unchanged was wrong, and this comment
// exists so the next reader does not re-derive it.

// PutObject uploads an object to the configured bucket.
// The key is the full object path (e.g. "avatars/user-uuid.webp").
func (c *Client) PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	_, err := c.minio.Client.PutObject(ctx, c.bucket, key, reader, size, minio.PutObjectOptions{
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
	obj, err := c.minio.Client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
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

// --- Multipart upload (chunked attachment format, #2157 PR 2) -------------
//
// These use the Core-only low-level S3 verbs. ComposeObject was rejected for
// this job: it is built on UploadPartCopy, the least portable S3 verb, and
// ADR-0024 commits to four backends (minio|s3|r2|b2). Object-per-chunk was also
// rejected -- it needs a chunk_count column, changes the storage_key contract,
// and makes delete and cleanup N-object.

// NewMultipartUpload begins a multipart upload and returns its upload ID.
func (c *Client) NewMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	uploadID, err := c.minio.NewMultipartUpload(ctx, c.bucket, key, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return "", fmt.Errorf("storage: failed to start multipart upload for %q: %w", key, err)
	}
	return uploadID, nil
}

// PutObjectPart streams one part directly to the backend.
//
// The reader is consumed as it is written, so the caller never has to hold the
// part in memory -- which is the point of the chunked format on the server side
// as well as the client's.
func (c *Client) PutObjectPart(
	ctx context.Context, key, uploadID string, partNumber int, r io.Reader, size int64,
) (ObjectPartInfo, error) {
	part, err := c.minio.PutObjectPart(
		ctx, c.bucket, key, uploadID, partNumber, r, size, minio.PutObjectPartOptions{})
	if err != nil {
		return ObjectPartInfo{}, fmt.Errorf(
			"storage: failed to upload part %d of %q: %w", partNumber, key, err)
	}
	return ObjectPartInfo{PartNumber: part.PartNumber, Size: part.Size, ETag: part.ETag}, nil
}

// ListObjectParts returns every part the backend actually holds.
//
// Paginated deliberately rather than assuming one page: the current format tops
// out at 32 parts, well under the 1000-per-page default, but an assumption that
// silently truncates a listing would make the commit path under-count parts and
// complete an upload that is missing bytes.
func (c *Client) ListObjectParts(ctx context.Context, key, uploadID string) ([]ObjectPartInfo, error) {
	var out []ObjectPartInfo
	marker := 0
	for {
		res, err := c.minio.ListObjectParts(ctx, c.bucket, key, uploadID, marker, 1000)
		if err != nil {
			return nil, fmt.Errorf("storage: failed to list parts of %q: %w", key, err)
		}
		for _, p := range res.ObjectParts {
			out = append(out, ObjectPartInfo{PartNumber: p.PartNumber, Size: p.Size, ETag: p.ETag})
		}
		if !res.IsTruncated {
			return out, nil
		}
		if marker, err = nextPartMarker(marker, res.NextPartNumberMarker); err != nil {
			return nil, err
		}
	}
}

// nextPartMarker advances a part listing, refusing to loop on a marker that did
// not move.
//
// A backend that answers IsTruncated with an unchanged marker spins this loop
// forever on SUCCESSFUL calls -- no error to return, no cancellation to observe
// (the context reaches every request, so a real cancellation already surfaces
// as an error from minio), and `out` growing without bound until the process
// dies. Part numbers are monotonic by contract, so refusing to go backwards or
// stand still costs nothing a correct backend would want to do.
func nextPartMarker(current, next int) (int, error) {
	if next <= current {
		return 0, fmt.Errorf(
			"storage: part listing reported more results but did not advance past marker %d", current)
	}
	return next, nil
}

// CompleteMultipartUpload concatenates the parts into the final object.
//
// The object becomes readable only at this point -- an incomplete multipart
// upload is not retrievable via GetObject -- so there is no window in which a
// partial attachment could be downloaded.
func (c *Client) CompleteMultipartUpload(
	ctx context.Context, key, uploadID string, parts []ObjectPartInfo,
) error {
	complete := make([]minio.CompletePart, 0, len(parts))
	for _, p := range parts {
		complete = append(complete, minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag})
	}
	// S3 requires ascending part numbers; the caller's slice order is not a
	// contract we want to depend on.
	sort.Slice(complete, func(i, j int) bool { return complete[i].PartNumber < complete[j].PartNumber })

	if _, err := c.minio.CompleteMultipartUpload(
		ctx, c.bucket, key, uploadID, complete, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("storage: failed to complete multipart upload for %q: %w", key, err)
	}
	return nil
}

// AbortMultipartUpload discards an incomplete upload and its stored parts.
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	if err := c.minio.AbortMultipartUpload(ctx, c.bucket, key, uploadID); err != nil {
		return fmt.Errorf("storage: failed to abort multipart upload for %q: %w", key, err)
	}
	return nil
}

// ListIncompleteUploads reports multipart uploads started before olderThan.
//
// This is the sweeper's work queue, and it comes from the object store rather
// than from Redis on purpose: a Redis-derived sweeper fails exactly when Redis
// does, which is precisely when sessions get orphaned. A total Redis loss
// therefore cannot strand bytes.
func (c *Client) ListIncompleteUploads(
	ctx context.Context, olderThan time.Time,
) ([]IncompleteUpload, error) {
	var out []IncompleteUpload
	keyMarker, uploadIDMarker := "", ""
	for {
		res, err := c.minio.ListMultipartUploads(
			ctx, c.bucket, "", keyMarker, uploadIDMarker, "", 1000)
		if err != nil {
			return nil, fmt.Errorf("storage: failed to list incomplete uploads: %w", err)
		}
		for _, u := range res.Uploads {
			if u.Initiated.Before(olderThan) {
				out = append(out, IncompleteUpload{
					Key: u.Key, UploadID: u.UploadID, Initiated: u.Initiated,
				})
			}
		}
		if !res.IsTruncated {
			return out, nil
		}
		keyMarker, uploadIDMarker, err = nextUploadMarkers(
			keyMarker, uploadIDMarker, res.NextKeyMarker, res.NextUploadIDMarker)
		if err != nil {
			return nil, err
		}
	}
}

// nextUploadMarkers advances a multipart-upload listing, refusing to loop on a
// marker PAIR that did not move. Same hazard as nextPartMarker.
//
// The pair, not the key alone: many uploads can share one key, so a truncated
// page legitimately repeats keyMarker while uploadIDMarker moves.
func nextUploadMarkers(curKey, curID, nextKey, nextID string) (string, string, error) {
	// STRICTLY forward, not merely different. Refusing only an unchanged pair
	// left a backend that cycles A -> B -> A looping forever, because each hop
	// "changed" the pair -- the same non-termination the guard exists to stop,
	// one step wider. S3 orders multipart listings by (key, upload-id)
	// ascending, so forward progress is the contract and anything else is a
	// broken response.
	if nextKey < curKey || (nextKey == curKey && nextID <= curID) {
		return "", "", fmt.Errorf(
			"storage: upload listing reported more results but did not advance past %q/%q "+
				"(got %q/%q)", curKey, curID, nextKey, nextID)
	}
	return nextKey, nextID, nil
}

// StoredObject is one object present in the bucket, as reported by a listing.
//
// LastModified is the field the orphan reaper's write-race margin is measured
// against. Both attachment write paths put the OBJECT down before they insert
// its media_files ROW, so an object younger than the margin may simply be one
// whose row is still moments away.
type StoredObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListObjects reports every object under prefix that was last modified before
// olderThan.
//
// The age filter is applied HERE rather than left to the caller so that no
// consumer can accidentally receive a freshly written object. It is the only
// thing standing between the orphan reaper and the put-then-insert window in
// both attachment write paths, and a filter a caller must remember to apply is
// one a caller will eventually forget.
//
// `c.minio.Client.ListObjects` (not `c.minio.ListObjects`) for the reason the
// NOTE above PutObject gives: Core shadows the promoted high-level method with
// a lower-level form. The SDK paginates the returned channel internally.
func (c *Client) ListObjects(
	ctx context.Context, prefix string, olderThan time.Time,
) ([]StoredObject, error) {
	var out []StoredObject
	for info := range c.minio.Client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if info.Err != nil {
			// Return rather than continue. A partial listing presented as a
			// complete one is the dangerous shape here: the orphan reaper's
			// whole premise is "the bucket holds this and the database does
			// not", and a truncated bucket side turns that into a wrong answer
			// in the safe direction today and an unnoticed blind spot forever.
			return nil, fmt.Errorf("storage: failed to list objects under %q: %w", prefix, info.Err)
		}
		obj, ok := storedObjectBefore(info.Key, info.Size, info.LastModified, olderThan)
		if !ok {
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// storedObjectBefore decides whether one listed object is old enough to be a
// candidate, and converts it.
//
// Split out of ListObjects because it is the only part of that method that
// makes a DECISION -- and the decision is the orphan reaper's write-race
// margin, the single thing standing between it and the put-then-insert window
// in both attachment write paths. The SDK call wrapped around it is thin
// plumbing, exercised at the consumer through fakes like every other method
// here; this is not, and a bucket should not be required to test it.
//
// The boundary is EXCLUSIVE and deliberately so: an object whose timestamp
// equals the cutoff is kept, not swept. At the margin the conservative answer
// is to wait another interval, because being early here deletes an object whose
// row is still moments away.
//
// A ZERO timestamp is REJECTED rather than treated as ancient. `time.Time{}`
// predates every cutoff, so a bare `Before` test admits an object carrying no
// LastModified at all -- bypassing the margin entirely, on a delete path, for
// the one input that tells us least. That is the wrong direction for a guard
// whose entire job is failing closed, so a missing timestamp waits for a
// listing that supplies one (PR #3019 review).
func storedObjectBefore(key string, size int64, lastModified, olderThan time.Time) (StoredObject, bool) {
	if lastModified.IsZero() || !lastModified.Before(olderThan) {
		return StoredObject{}, false
	}
	return StoredObject{Key: key, Size: size, LastModified: lastModified}, true
}
