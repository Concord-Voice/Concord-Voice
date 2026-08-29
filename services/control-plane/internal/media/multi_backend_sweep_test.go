package media

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Multi-backend sweep fan-out (ADR-0038 action item 6).
//
// The sweeper holds ONE store and therefore enumerates ONE bucket. Wiring only
// the legacy client means that, the moment the Wave C write default moves,
// every chunked session opened against the vendor is invisible to the sweep —
// and invisible SILENTLY, because `Attempted` falls to zero and the
// all-aborts-failed alarm cannot fire on a batch nobody listed. These tests
// cover the enumeration decision, which is the part that goes wrong.

// fakeSweepRegistry lets a test choose which backends exist and which of them
// refuse to resolve.
type fakeSweepRegistry struct {
	ids      []storage.BackendID
	broken   map[storage.BackendID]bool
	resolved []storage.BackendID
}

func (f *fakeSweepRegistry) BackendIDs() []storage.BackendID { return f.ids }

func (f *fakeSweepRegistry) Resolve(id storage.BackendID) (*storage.Client, error) {
	f.resolved = append(f.resolved, id)
	if f.broken[id] {
		return nil, errors.New("backend unavailable")
	}
	return &storage.Client{}, nil
}

func sweepTestLogger() *logger.Logger { return logger.NewWithWriter(discardWriter{}) }

// TestResolveSweepTargets_CoversEveryRegisteredBackend is the whole point: one
// target per backend, not one for the legacy client.
func TestResolveSweepTargets_CoversEveryRegisteredBackend(t *testing.T) {
	reg := &fakeSweepRegistry{ids: []storage.BackendID{storage.LegacyBackendID, "r2-useast", "r2-eu"}}

	targets := resolveSweepTargets(reg, "attachment session sweep", sweepTestLogger())

	require.Len(t, targets, 3, "every registered backend must be swept, not just the legacy one")
	assert.Equal(t,
		[]string{string(storage.LegacyBackendID), "r2-useast", "r2-eu"},
		[]string{targets[0].backend, targets[1].backend, targets[2].backend},
		"registration order is preserved so per-backend log lines do not reshuffle every run")
	for _, tgt := range targets {
		assert.NotNil(t, tgt.store)
	}
}

// TestResolveSweepTargets_SkipsUnresolvableButKeepsTheRest — one bad backend
// must not cost the others their sweep. This is the fault-isolation property
// the registry itself is built around, carried through to the sweeper.
func TestResolveSweepTargets_SkipsUnresolvableButKeepsTheRest(t *testing.T) {
	reg := &fakeSweepRegistry{
		ids:    []storage.BackendID{storage.LegacyBackendID, "r2-useast"},
		broken: map[storage.BackendID]bool{"r2-useast": true},
	}

	targets := resolveSweepTargets(reg, "attachment session sweep", sweepTestLogger())

	require.Len(t, targets, 1)
	assert.Equal(t, string(storage.LegacyBackendID), targets[0].backend)
	assert.Equal(t,
		[]storage.BackendID{storage.LegacyBackendID, "r2-useast"}, reg.resolved,
		"an unresolvable backend must still be ATTEMPTED, so its skip is observable")
}

// TestResolveSweepTargets_NilRegistryYieldsNothing — an embedder with no
// registry must not panic; StartSessionSweepWorkers falls back to the single
// legacy worker at the call site.
func TestResolveSweepTargets_NilRegistryYieldsNothing(t *testing.T) {
	assert.Nil(t, resolveSweepTargets(nil, "attachment session sweep", sweepTestLogger()))
	assert.Zero(t, StartSessionSweepWorkers(t.Context(), nil, sweepTestLogger(), 0))
}

// TestSessionSweeperBackendLabel — a sweep that found nothing must say WHICH
// bucket found nothing, or "zero candidates" and "never enumerated" read the
// same in a log.
func TestSessionSweeperBackendLabel(t *testing.T) {
	labelled := NewSessionSweeperForBackend(nil, "r2-useast", sweepTestLogger())
	assert.Equal(t, "r2-useast", labelled.backendLabel())

	plain := NewSessionSweeper(nil, sweepTestLogger())
	assert.NotEmpty(t, plain.backendLabel(), "an unlabelled sweeper still renders something readable")
	assert.NotEqual(t, "r2-useast", plain.backendLabel())
}

// --- disk watermark is legacy-only ------------------------------------------
//
// The watermark statfs's the LOCAL host disk, because the harm it prevents is
// MinIO filling the disk Postgres shares. A write landing at a vendor backend
// consumes none of it, so gating that write on local occupancy would turn a
// full disk into a total attachment outage instead of the migration that
// relieves it. Gitar suggested hoisting the check ABOVE backend resolution for
// efficiency; that is declined, and this test is why.

func TestCheckAttachmentDiskWatermark_RefusesOnlyLegacyWrites(t *testing.T) {
	h := &Handler{log: logger.NewWithWriter(discardWriter{})}
	h.SetDiskWatermark(refusingWatermark(false)) // SaaS, disk at 100%

	// Legacy write (empty backendID = NULL column) -> refused with 507.
	c, rec := writeRoutingContext()
	assert.False(t, h.checkAttachmentDiskWatermark(c, ""),
		"a legacy write must be refused when the shared disk is at capacity")
	assert.Equal(t, http.StatusInsufficientStorage, rec.Code)

	// Vendor write -> allowed, because it consumes none of that disk.
	c, rec = writeRoutingContext()
	assert.True(t, h.checkAttachmentDiskWatermark(c, "r2-useast"),
		"a vendor write must NOT be gated on local MinIO occupancy")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestCheckAttachmentDiskWatermark_AllowsLegacyBelowThreshold is the positive
// control: without it, the vendor assertion above would also pass if the gate
// were simply broken and refusing nothing.
//
// The arguments are (totalBlocks, availableBlocks) and BOTH have to be sane for
// this to control anything. It was written `(10, 90)` — 90 available out of 10
// total — which trips occupancyPercent's "available blocks exceed total" guard,
// and Check FAILS OPEN on any occupancy-read error. The test passed by taking
// the error path, never evaluating an occupancy figure, so it would have gone
// on passing with the threshold comparison deleted outright: a positive control
// that controlled nothing. 90 of 100 available is 10% occupancy against a 75%
// refuse threshold, which reaches the comparison this test exists to pin.
func TestCheckAttachmentDiskWatermark_AllowsLegacyBelowThreshold(t *testing.T) {
	h := &Handler{log: logger.NewWithWriter(discardWriter{})}
	h.SetDiskWatermark(newDiskWatermark("/", false, fixedDiskStatFS(100, 90), logger.NewWithWriter(discardWriter{})))

	c, rec := writeRoutingContext()
	assert.True(t, h.checkAttachmentDiskWatermark(c, ""))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestResolveSweepTargets_UnconfiguredStorageIsQuiet — a deployment with no
// object storage at all is a SUPPORTED configuration, not a fault, and must not
// log an ERROR on every boot.
//
// This is a regression introduced by the multi-backend fix itself and caught in
// review. Before it, `if storageClient != nil` skipped the whole sweep silently
// when storage was unconfigured. After it, the registry is always non-nil, so
// enumeration always runs, resolves the one registered-but-unavailable legacy
// backend, and shouted about it forever. Fixing one silence created a different
// noise; both are wrong for the same reason — the log must distinguish a fault
// from a configuration.
func TestResolveSweepTargets_UnconfiguredStorageIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	reg := &fakeSweepRegistry{
		ids:    []storage.BackendID{storage.LegacyBackendID},
		broken: map[storage.BackendID]bool{storage.LegacyBackendID: true},
	}

	targets := resolveSweepTargets(reg, "attachment session sweep", logger.NewWithWriter(&buf))

	assert.Empty(t, targets, "there is nothing to sweep without object storage")
	assert.NotContains(t, buf.String(), "level=ERROR",
		"an unconfigured deployment must not report a fault on every boot")
	assert.Contains(t, buf.String(), "not configured",
		"it should still say why it swept nothing, rather than being wholly silent")
}

// TestResolveSweepTargets_UnavailableVendorStillErrors is the positive control
// for the test above: the quiet path must be scoped to the unconfigured case
// ONLY, or the fix would have muted the alert this whole fan-out exists to
// raise.
func TestResolveSweepTargets_UnavailableVendorStillErrors(t *testing.T) {
	var buf bytes.Buffer
	reg := &fakeSweepRegistry{
		ids:    []storage.BackendID{storage.LegacyBackendID, "r2-useast"},
		broken: map[storage.BackendID]bool{"r2-useast": true},
	}

	targets := resolveSweepTargets(reg, "attachment session sweep", logger.NewWithWriter(&buf))

	require.Len(t, targets, 1)
	assert.Contains(t, buf.String(), "level=ERROR",
		"a real backend that cannot be enumerated is still a fault worth alerting on")
	assert.Contains(t, buf.String(), "r2-useast")
}

// TestResolveSweepTargets_NamesTheCallingSweep pins the sweepName parameter to
// the log output. Two sweepers share this resolver -- the session sweeper and
// the tier-2 orphan reaper -- and before the parameter existed an orphan-sweep
// enumeration failure reported itself as "attachment session sweep: ... its
// abandoned uploads are unreclaimed", naming the wrong worker AND the wrong
// failure. Without this test the parameter can be threaded, ignored, and never
// missed, because every other assertion here passes either way.
func TestResolveSweepTargets_NamesTheCallingSweep(t *testing.T) {
	var buf bytes.Buffer
	reg := &fakeSweepRegistry{
		ids:    []storage.BackendID{storage.LegacyBackendID, "r2-useast"},
		broken: map[storage.BackendID]bool{"r2-useast": true},
	}

	resolveSweepTargets(reg, "attachment orphan sweep", logger.NewWithWriter(&buf))

	assert.Contains(t, buf.String(), "attachment orphan sweep: backend NOT enumerated")
	assert.NotContains(t, buf.String(), "attachment session sweep",
		"a resolver called by the orphan reaper must not attribute its failure to the session sweeper")
}
