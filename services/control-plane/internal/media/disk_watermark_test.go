package media

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fixedDiskStatFS mirrors opsmetrics' fixedStatFS test seam (host_reader_test.go)
// so DiskWatermark's occupancy math can be driven deterministically without a
// real filesystem.
func fixedDiskStatFS(blocks, available uint64) diskStatFSFunc {
	return func(_ string, stat *syscall.Statfs_t) error {
		stat.Blocks = blocks
		stat.Bavail = available
		return nil
	}
}

var errFakeDiskStatFS = errors.New("disk watermark test: statfs failed")

func failingDiskStatFS(_ string, _ *syscall.Statfs_t) error {
	return errFakeDiskStatFS
}

// --- nil-receiver safety ---------------------------------------------------

func TestDiskWatermark_NilReceiver_AlwaysAllows(t *testing.T) {
	var w *DiskWatermark

	err := w.Check()

	assert.NoError(t, err, "a nil *DiskWatermark must behave as 'no watermark configured'")
}

// --- occupancyPercent arithmetic (mirrors opsmetrics.readDiskPercent) ------

func TestDiskWatermark_OccupancyPercent_ComputesFromBlocksAndAvailable(t *testing.T) {
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 25), logger.NewWithWriter(&bytes.Buffer{}))

	percent, err := w.occupancyPercent()

	require.NoError(t, err)
	assert.InDelta(t, 75.0, percent, 0.0001)
}

func TestDiskWatermark_OccupancyPercent_ZeroBlocksErrors(t *testing.T) {
	w := newDiskWatermark("/", false, fixedDiskStatFS(0, 0), logger.NewWithWriter(&bytes.Buffer{}))

	_, err := w.occupancyPercent()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero blocks")
}

func TestDiskWatermark_OccupancyPercent_AvailableExceedsTotalErrors(t *testing.T) {
	// A statfs result claiming more available blocks than total blocks is
	// nonsensical and must be treated as a read failure (fail-open via Check),
	// never as a negative or wrapped-around percentage.
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 150), logger.NewWithWriter(&bytes.Buffer{}))

	_, err := w.occupancyPercent()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceed")
}

func TestDiskWatermark_OccupancyPercent_PropagatesStatFSError(t *testing.T) {
	w := newDiskWatermark("/", false, failingDiskStatFS, logger.NewWithWriter(&bytes.Buffer{}))

	_, err := w.occupancyPercent()

	require.Error(t, err)
	assert.ErrorIs(t, err, errFakeDiskStatFS)
}

// --- Check(): below warn threshold -----------------------------------------

func TestDiskWatermark_Check_BelowWarnThreshold_AllowsSilently(t *testing.T) {
	var logBuf bytes.Buffer
	// 59% occupied: strictly below diskWatermarkWarnPercent (60).
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 41), logger.NewWithWriter(&logBuf))

	err := w.Check()

	assert.NoError(t, err)
	assert.Empty(t, logBuf.String(), "occupancy below the warn threshold must not log anything")
}

// --- Check(): warn threshold (SaaS and self-hosted behave identically) ----

func TestDiskWatermark_Check_AtWarnThreshold_WarnsButAllows(t *testing.T) {
	var logBuf bytes.Buffer
	// Exactly 60%: diskWatermarkWarnPercent boundary, inclusive per Check's >=.
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 40), logger.NewWithWriter(&logBuf))

	err := w.Check()

	assert.NoError(t, err)
	assert.Contains(t, logBuf.String(), "warn threshold")
}

func TestDiskWatermark_Check_JustBelowRefuseThreshold_WarnsButAllows(t *testing.T) {
	var logBuf bytes.Buffer
	// 74%: above warn, strictly below diskWatermarkRefusePercent (75).
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 26), logger.NewWithWriter(&logBuf))

	err := w.Check()

	assert.NoError(t, err, "occupancy below the refuse threshold must never refuse, even on SaaS")
	assert.Contains(t, logBuf.String(), "warn threshold")
}

// --- Check(): refuse threshold, SaaS (selfHosted=false) --------------------

func TestDiskWatermark_Check_AtRefuseThreshold_SaaS_Refuses(t *testing.T) {
	var logBuf bytes.Buffer
	// Exactly 75%: diskWatermarkRefusePercent boundary, inclusive per Check's >=.
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 25), logger.NewWithWriter(&logBuf))

	err := w.Check()

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAttachmentStorageAtCapacity)
	assert.Contains(t, logBuf.String(), "Refusing attachment write")
}

func TestDiskWatermark_Check_AboveRefuseThreshold_SaaS_Refuses(t *testing.T) {
	// 90%: well past the refuse threshold.
	w := newDiskWatermark("/", false, fixedDiskStatFS(100, 10), logger.NewWithWriter(&bytes.Buffer{}))

	err := w.Check()

	assert.ErrorIs(t, err, ErrAttachmentStorageAtCapacity)
}

// --- Check(): refuse threshold, self-hosted (selfHosted=true) --------------
//
// Self-hosted / dev / air-gapped deployments have nowhere to fail over to
// (MinIO is their sole, permanent backend under ADR-0038), so they only ever
// warn at the refuse threshold -- they must never receive
// ErrAttachmentStorageAtCapacity. This is the scoping rule (a) branch.

func TestDiskWatermark_Check_AtRefuseThreshold_SelfHosted_AllowsButWarns(t *testing.T) {
	var logBuf bytes.Buffer
	w := newDiskWatermark("/", true, fixedDiskStatFS(100, 25), logger.NewWithWriter(&logBuf))

	err := w.Check()

	assert.NoError(t, err, "self-hosted deployments must never be refused a write")
	assert.Contains(t, logBuf.String(), "self-hosted deployments never refuse")
	assert.NotContains(t, strings.ToLower(logBuf.String()), "refusing",
		"a self-hosted allow must not be logged with SaaS's refusal message")
}

func TestDiskWatermark_Check_AboveRefuseThreshold_SelfHosted_AllowsButWarns(t *testing.T) {
	// 99%: far past the refuse threshold -- self-hosted still must not refuse.
	w := newDiskWatermark("/", true, fixedDiskStatFS(100, 1), logger.NewWithWriter(&bytes.Buffer{}))

	err := w.Check()

	assert.NoError(t, err)
}

// --- Check(): statfs failure fails open -------------------------------------

func TestDiskWatermark_Check_StatFSError_FailsOpen(t *testing.T) {
	var logBuf bytes.Buffer
	w := newDiskWatermark("/", false, failingDiskStatFS, logger.NewWithWriter(&logBuf))

	err := w.Check()

	assert.NoError(t, err, "a monitoring failure must never become a hard refusal")
	assert.Contains(t, logBuf.String(), "allowing the attachment write")
}

func TestDiskWatermark_Check_StatFSError_FailsOpenEvenWhenSelfHosted(t *testing.T) {
	w := newDiskWatermark("/", true, failingDiskStatFS, logger.NewWithWriter(&bytes.Buffer{}))

	err := w.Check()

	assert.NoError(t, err)
}

func TestDiskWatermark_Check_StatFSError_NilLoggerDoesNotPanic(t *testing.T) {
	w := newDiskWatermark("/", false, failingDiskStatFS, nil)

	assert.NotPanics(t, func() {
		err := w.Check()
		assert.NoError(t, err)
	})
}

// --- NewDiskWatermark: real wiring ------------------------------------------

func TestNewDiskWatermark_UsesRealStatfsOnRootPath(t *testing.T) {
	// Proves NewDiskWatermark actually wires syscall.Statfs against "/" rather
	// than a stub -- occupancy on any live filesystem is a value in [0, 100].
	w := NewDiskWatermark(false, logger.NewWithWriter(&bytes.Buffer{}))

	percent, err := w.occupancyPercent()

	require.NoError(t, err)
	assert.GreaterOrEqual(t, percent, 0.0)
	assert.LessOrEqual(t, percent, 100.0)
}

func TestNewDiskWatermark_CheckDoesNotPanicOnRealDisk(t *testing.T) {
	w := NewDiskWatermark(false, logger.NewWithWriter(&bytes.Buffer{}))

	assert.NotPanics(t, func() {
		_ = w.Check()
	})
}
