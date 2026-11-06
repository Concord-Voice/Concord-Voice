package media

import (
	"context"
	"io"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// Cold-attachment tiering (Infrequent Access round trip).
//
// An R2 bucket lifecycle rule moves attachments/ objects to Infrequent Access
// 60 days after upload ([internal]attachment-cold-tier.md). That rule is
// one-way and age-based, so bringing a file that turned hot again back to
// Standard is ours to do: the second read of an Infrequent Access object
// within coldReadWindow copies it back, which also restarts its 60 days.
//
// PRIVACY: the only state is a Redis counter keyed by file ID, with no user
// ID, that expires on its own. Nothing about when a file was read is written
// to Postgres.

// coldReadWindow is how long the first Infrequent Access read is remembered.
const coldReadWindow = 30 * 24 * time.Hour

// coldReadsToPromote is the read count, inside the window, that promotes a file.
const coldReadsToPromote = 2

// coldPromoteTimeout bounds the counter round trip plus the server-side copy.
// The copy is R2-internal (no bytes cross our uplink), but a 1 GiB object is
// not instant.
const coldPromoteTimeout = 2 * time.Minute

// coldReadSlots caps detached cold-read goroutines across the process. A read
// that finds every slot busy is simply not counted.
// ponytail: fixed cap of 64; make it a config value if 64 ever drops real reads.
var coldReadSlots = make(chan struct{}, 64)

func coldReadKey(fileID string) string    { return "attachment:cold-reads:" + fileID }
func coldPromoteKey(fileID string) string { return "attachment:cold-promote:" + fileID }

// coldTierStore is the optional half of a store that reports storage classes.
// *storage.Client implements it; test fakes and MinIO-only stores need not,
// and a store without it simply never promotes.
type coldTierStore interface {
	GetObjectWithClass(ctx context.Context, key string) (io.ReadCloser, string, string, error)
	StorageClass(ctx context.Context, key string) (string, error)
	PromoteToStandard(ctx context.Context, key string) error
	DeleteObject(ctx context.Context, key string) error
}

// goColdRead runs noteColdRead detached, if a slot is free. Returns at once.
func (h *Handler) goColdRead(store coldTierStore, fileID, key string) {
	select {
	case coldReadSlots <- struct{}{}:
	default:
		h.log.Warn("Cold attachment read not counted: too many in flight", "file_id", fileID)
		return
	}
	go func() {
		defer func() { <-coldReadSlots }()
		h.noteColdRead(store, fileID, key)
	}()
}

// getAttachmentObject reads an attachment and reports its storage class when
// the store can.
func getAttachmentObject(ctx context.Context, store ObjectStore, key string) (io.ReadCloser, string, string, error) {
	if ts, ok := store.(coldTierStore); ok {
		return ts.GetObjectWithClass(ctx, key)
	}
	obj, contentType, err := store.GetObject(ctx, key)
	return obj, contentType, "", err
}

// noteColdRead counts one read of an Infrequent Access attachment and promotes
// it on the coldReadsToPromote-th read inside the window. Best-effort: any
// failure leaves the file where it is, still fully readable.
//
// Callers run it through goColdRead, detached from the request, so a client
// that disconnects mid-download still counts and the download is never slowed
// by the copy.
func (h *Handler) noteColdRead(store coldTierStore, fileID, key string) {
	if h.sessionRedis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), coldPromoteTimeout)
	defer cancel()

	counter := coldReadKey(fileID)
	var reads *redis.IntCmd
	// INCR + EXPIRE NX in one transaction: the window starts at the FIRST
	// read and later reads never extend it.
	if _, err := h.sessionRedis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		reads = p.Incr(ctx, counter)
		p.ExpireNX(ctx, counter, coldReadWindow)
		return nil
	}); err != nil {
		h.log.Warn("Cold attachment read not counted", "file_id", fileID, "error", err)
		return
	}
	// >= rather than ==: if a promotion fails, the next read retries it.
	if reads.Val() < coldReadsToPromote {
		return
	}
	// One copy per file at a time: concurrent reads past the threshold would
	// otherwise each pay a full Infrequent Access retrieval and write.
	lock := coldPromoteKey(fileID)
	if got, err := h.sessionRedis.SetNX(ctx, lock, 1, coldPromoteTimeout).Result(); err != nil || !got {
		return
	}
	defer h.sessionRedis.Del(context.Background(), lock)
	if err := store.PromoteToStandard(ctx, key); err != nil {
		h.log.Warn("Cold attachment promotion failed; it stays in Infrequent Access",
			"file_id", fileID, "error", err)
		return
	}
	h.dropPromotedOrphan(ctx, store, fileID, key)
	if err := h.sessionRedis.Del(ctx, counter).Err(); err != nil {
		h.log.Warn("Cold attachment counter not cleared", "file_id", fileID, "error", err)
	}
	h.log.Info("Promoted cold attachment to Standard", "file_id", fileID)
}

// dropPromotedOrphan deletes the object again when the attachment was deleted
// while the copy ran: a copy that commits after DeleteObject recreates the
// blob, and the reaper, which already marked it reaped, would never see it.
// A lookup failure keeps the object; that is the pre-existing orphan risk,
// never data loss.
func (h *Handler) dropPromotedOrphan(ctx context.Context, store coldTierStore, fileID, key string) {
	if h.db == nil {
		return
	}
	var live bool
	if err := h.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM media_files WHERE storage_key = $1 AND deleted_at IS NULL)`,
		key).Scan(&live); err != nil {
		h.log.Warn("Cold attachment liveness check failed", "file_id", fileID, "error", err)
		return
	}
	if live {
		return
	}
	if err := store.DeleteObject(ctx, key); err != nil {
		h.log.Warn("Promoted copy of a deleted attachment not removed", "file_id", fileID, "error", err)
		return
	}
	h.log.Info("Removed promoted copy of a deleted attachment", "file_id", fileID)
}

// recordAttachmentRead counts one read of an attachment whose bytes this
// process does not fetch (a presigned-URL download), looking its class up
// with a HEAD first. Returns at once; the work is detached and slot-bounded
// like goColdRead.
// DownloadAttachment does not use this: it already has the class from its GET.
// Its first caller is the presigned-URL route in #4323, which lands after this.
func (h *Handler) recordAttachmentRead(store ObjectStore, fileID, key string) {
	ts, ok := store.(coldTierStore)
	if !ok || h.sessionRedis == nil {
		return
	}
	select {
	case coldReadSlots <- struct{}{}:
	default:
		h.log.Warn("Cold attachment read not counted: too many in flight", "file_id", fileID)
		return
	}
	go func() {
		defer func() { <-coldReadSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		class, err := ts.StorageClass(ctx, key)
		cancel()
		if err != nil {
			h.log.Warn("Cold attachment read not counted", "file_id", fileID, "error", err)
			return
		}
		if isColdClass(class) {
			h.noteColdRead(ts, fileID, key)
		}
	}()
}

// isColdClass reports whether a storage class is Infrequent Access.
func isColdClass(class string) bool { return class == storage.StorageClassInfrequentAccess }
