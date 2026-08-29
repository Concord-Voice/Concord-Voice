package media

// Shared-disk occupancy watermark for MinIO-backed attachment writes
// (#2759 unit A1).
//
// SaaS runs MinIO single-node, single-drive, command `server /data`, on a
// plain Docker volume that shares its host's NVMe with Postgres (+ its WAL
// archive), Redis and NATS -- no erasure coding, no separate volume. If
// MinIO fills that disk, Postgres stops accepting writes: an unbounded,
// unquota-ed, user-driven upload path can cause a database outage. This
// watermark is the acute mitigation. It is independent of, and does not
// attempt, the real per-tier storage-pool quota
// (entitlements.MaxServerStoragePoolBytes, currently
// entitlements.ServerStoragePoolUnset on every SaaS row) -- that needs the
// server_storage_usage accounting tracked by #1523.
//
// The two thresholds are CONSTANTS, not config fields: pkg/config/config.go
// is owned by a parallel workstream this wave, and the ADR that introduces
// this gate fixes these numbers deliberately.
import (
	"errors"
	"fmt"
	"syscall"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// diskWatermarkWarnPercent is the occupancy at which every deployment
	// shape (SaaS, self-hosted, dev, air-gapped) logs a warning. Crossing
	// it alone never refuses a write.
	diskWatermarkWarnPercent = 60.0

	// diskWatermarkRefusePercent is the occupancy at which SaaS refuses NEW
	// attachment writes. Self-hosted / dev / air-gapped deployments only
	// ever warn at this threshold too -- MinIO is their sole, permanent
	// object-storage backend (ADR-0038) with nowhere to fail over to, so
	// refusing there would stop the product from accepting attachments
	// during ordinary growth. See DiskWatermark.Check.
	diskWatermarkRefusePercent = 75.0

	// diskWatermarkPath is the path statfs targets. Control-plane has no
	// bind mount onto MinIO's own volume, but on the single-node deployment
	// this gate addresses, MinIO's named Docker volume and the
	// control-plane container's own writable layer share the same backing
	// host disk under Docker's default overlay2 storage driver -- statfs
	// from inside ANY container on that host reports the underlying host
	// filesystem's block counts (the same thing `docker exec <ctr> df -h /`
	// shows), so this observes the same occupancy MinIO would fill.
	diskWatermarkPath = "/"
)

// ErrAttachmentStorageAtCapacity is returned by DiskWatermark.Check when a
// SaaS deployment's shared disk is at or above the refuse threshold. It is
// never returned for a self-hosted deployment -- see scoping rule (a) above.
var ErrAttachmentStorageAtCapacity = errors.New("media: attachment storage at capacity")

// diskStatFSFunc mirrors the injectable seam in
// internal/opsmetrics/host_reader.go (statFSFunc there) so this gate is
// unit-testable without touching a real filesystem. Kept local rather than
// imported: the opsmetrics seam type and readDiskPercent are unexported, and
// this package must not reach into opsmetrics internals to get them.
type diskStatFSFunc func(path string, stat *syscall.Statfs_t) error

// DiskWatermark gates NEW attachment writes on shared-disk occupancy.
//
// It deliberately has no opinion on tier-1 profile media (avatars/,
// server-icons/, dm-icons/) -- those stay writable at any occupancy under
// ADR-0038. The exemption is structural: only the attachment-write call
// sites (UploadAttachment, InitUploadSession) consult a DiskWatermark: it
// is not a parameter a tier-1 call site could accidentally pass.
type DiskWatermark struct {
	path       string
	statFS     diskStatFSFunc
	selfHosted bool
	log        *logger.Logger
}

// NewDiskWatermark builds a watermark check for the shared MinIO disk using
// the real syscall.Statfs. selfHosted should be
// config.IsSelfHostedInstance(cfg.InstanceType) at the call site -- see
// scoping rule (a): self-hosted / dev / air-gapped deployments only warn,
// they never refuse.
func NewDiskWatermark(selfHosted bool, log *logger.Logger) *DiskWatermark {
	return newDiskWatermark(diskWatermarkPath, selfHosted, syscall.Statfs, log)
}

// newDiskWatermark is the injectable-seam constructor for tests.
func newDiskWatermark(path string, selfHosted bool, statFS diskStatFSFunc, log *logger.Logger) *DiskWatermark {
	return &DiskWatermark{path: path, statFS: statFS, selfHosted: selfHosted, log: log}
}

// occupancyPercent reads current disk occupancy as a 0-100 value, using the
// identical blocks/blocks-available computation as
// opsmetrics.readDiskPercent.
func (w *DiskWatermark) occupancyPercent() (float64, error) {
	var stat syscall.Statfs_t
	if err := w.statFS(w.path, &stat); err != nil {
		return 0, fmt.Errorf("disk watermark: read filesystem usage: %w", err)
	}
	if stat.Blocks == 0 {
		return 0, errors.New("disk watermark: filesystem has zero blocks")
	}
	if stat.Bavail > stat.Blocks {
		return 0, errors.New("disk watermark: available filesystem blocks exceed total")
	}
	return float64(stat.Blocks-stat.Bavail) / float64(stat.Blocks) * 100, nil
}

// Check evaluates whether a NEW attachment write should proceed. A nil
// receiver is treated as "no watermark configured" and always allows the
// write, so callers can hold an unset *DiskWatermark exactly like the
// existing optional opsCounter/sessionRedis fields on Handler.
//
// A statfs failure FAILS OPEN (logs and allows the write): treating a
// monitoring failure as a hard refusal would turn an observability outage
// into an availability outage for every attachment upload, which is a worse
// failure mode than temporarily missing this guard.
func (w *DiskWatermark) Check() error {
	if w == nil {
		return nil
	}

	percent, err := w.occupancyPercent()
	if err != nil {
		if w.log != nil {
			w.log.Warn("Could not read shared-disk occupancy; allowing the attachment write", "error", err)
		}
		return nil
	}

	if percent >= diskWatermarkRefusePercent {
		if w.selfHosted {
			if w.log != nil {
				w.log.Warn("Shared-disk occupancy at or above the refuse threshold; self-hosted deployments never refuse attachment writes",
					"occupancy_percent", percent, "threshold_percent", diskWatermarkRefusePercent)
			}
			return nil
		}
		if w.log != nil {
			w.log.Warn("Refusing attachment write: shared-disk occupancy at or above the refuse threshold",
				"occupancy_percent", percent, "threshold_percent", diskWatermarkRefusePercent)
		}
		return ErrAttachmentStorageAtCapacity
	}

	if percent >= diskWatermarkWarnPercent && w.log != nil {
		w.log.Warn("Shared-disk occupancy at or above the warn threshold",
			"occupancy_percent", percent, "threshold_percent", diskWatermarkWarnPercent)
	}
	return nil
}
