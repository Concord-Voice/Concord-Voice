package managedturn_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// fakeProber records every probe and can fail on demand. hook runs inside
// Probe so a test can snapshot the world at the instant of proof.
type fakeProber struct {
	mu    sync.Mutex
	calls []string // key IDs, in call order
	err   error
	hook  func(managedturn.Key)
}

func (p *fakeProber) Probe(_ context.Context, key managedturn.Key) error {
	p.mu.Lock()
	p.calls = append(p.calls, key.ID())
	hook, err := p.hook, p.err
	p.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return err
}

func (p *fakeProber) callIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *fakeProber) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *fakeProber) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = nil
	p.hook = nil
}

type fixture struct {
	t         testing.TB
	root      string
	dbPath    string
	stateDir  string
	statePath string
	activeDir string
	store     *managedturn.Store
	prober    *fakeProber
	c         *managedturn.Coordinator
}

func ctx5(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	root := shareableTempDir(t)
	f := &fixture{
		t:         t,
		root:      root,
		dbPath:    filepath.Join(root, "db", "turndb"),
		stateDir:  filepath.Join(root, "state"),
		statePath: filepath.Join(root, "state", "state.json"),
		activeDir: filepath.Join(root, "active"),
		prober:    &fakeProber{},
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(f.dbPath), 0o755)) // #nosec G301 -- fixture: the read-only tests drop to nobody and must traverse it.
	require.NoError(t, os.MkdirAll(f.stateDir, 0o755))             // #nosec G301 -- fixture: the read-only tests drop to nobody and must traverse it.
	require.NoError(t, os.MkdirAll(f.activeDir, 0o755))            // #nosec G301 -- fixture: the read-only tests drop to nobody and must traverse it.
	f.store = openInitStore(t, f.dbPath)
	f.c = &managedturn.Coordinator{
		Store:     f.store,
		Prober:    f.prober,
		StatePath: f.statePath,
		ActiveDir: f.activeDir,
		ActiveGID: os.Getgid(),
		Realm:     testRealm,
	}
	return f
}

func (f *fixture) activeBytes() ([]byte, bool) {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.activeDir, activeFileName))
	if os.IsNotExist(err) {
		return nil, false
	}
	require.NoError(f.t, err)
	return b, true
}

func (f *fixture) stateBytes() ([]byte, bool) {
	f.t.Helper()
	b, err := os.ReadFile(f.statePath)
	if os.IsNotExist(err) {
		return nil, false
	}
	require.NoError(f.t, err)
	return b, true
}

func (f *fixture) state() managedturn.State {
	f.t.Helper()
	st, ok, err := managedturn.LoadState(f.statePath)
	require.NoError(f.t, err)
	require.True(f.t, ok, "state must exist")
	return st
}

func (f *fixture) rows() []string {
	f.t.Helper()
	ids, err := f.store.KeyIDs(context.Background(), testRealm)
	require.NoError(f.t, err)
	return ids
}

// bootstrap activates k1 (and, when two is true, adds k2) and clears the
// prober log so later assertions only see what they provoke.
func (f *fixture) bootstrap(k1 managedturn.Key) {
	f.t.Helper()
	require.NoError(f.t, f.c.Bootstrap(ctx5(f.t), k1))
	f.prober.reset()
}

func (f *fixture) bootstrapWithCandidate(k1, k2 managedturn.Key) {
	f.t.Helper()
	f.bootstrap(k1)
	require.NoError(f.t, f.c.AddCandidate(ctx5(f.t), k2))
	f.prober.reset()
}

// coldReboot simulates host RAM loss: the database and the active file vanish,
// the durable state file survives. The store is reopened and re-initialised
// empty, exactly as managed `up` does before coturn starts.
func (f *fixture) coldReboot() {
	f.t.Helper()
	require.NoError(f.t, f.store.Close())
	require.NoError(f.t, os.Remove(f.dbPath))
	require.NoError(f.t, os.Remove(filepath.Join(f.activeDir, activeFileName)))
	f.store = openInitStore(f.t, f.dbPath)
	f.c.Store = f.store
	f.prober.reset()
	require.Empty(f.t, f.rows(), "cold RAM starts with zero rows")
	_, present := f.activeBytes()
	require.False(f.t, present, "cold RAM starts without an active file")
}

type unchanged struct {
	active, state []byte
	hasActive     bool
	hasState      bool
	rows          []string
}

func (f *fixture) snapshot() unchanged {
	f.t.Helper()
	a, ha := f.activeBytes()
	s, hs := f.stateBytes()
	return unchanged{active: a, hasActive: ha, state: s, hasState: hs, rows: f.rows()}
}

func (f *fixture) requireUnchanged(before unchanged, msg string) {
	f.t.Helper()
	after := f.snapshot()
	assert.Equal(f.t, before, after, msg)
}

// ---- Bootstrap --------------------------------------------------------------

func TestCoordinator_Bootstrap_InsertsThenProbesThenPublishes(t *testing.T) {
	f := newFixture(t)
	k1, raw1 := newKey(t)

	var rowsAtProbe []string
	var activeAtProbe bool
	f.prober.hook = func(managedturn.Key) {
		rowsAtProbe = f.rows()
		_, activeAtProbe = f.activeBytes()
	}

	require.NoError(t, f.c.Bootstrap(ctx5(t), k1))

	assert.Equal(t, []string{k1.ID()}, rowsAtProbe, "the key must be in coturn's database before it is probed")
	assert.False(t, activeAtProbe, "the active file must not exist until the probe has passed")
	assert.Equal(t, []string{k1.ID()}, f.prober.callIDs(), "probed exactly once, with the bootstrap key")

	active, ok := f.activeBytes()
	require.True(t, ok)
	assert.Equal(t, []byte(raw1), active)
	info, err := os.Stat(filepath.Join(f.activeDir, activeFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o440), info.Mode().Perm())

	st := f.state()
	assert.Equal(t, uint64(1), st.Generation, "first activation is generation 1")
	assert.Equal(t, k1.ID(), st.ActiveKeyID)
	assert.Equal(t, []string{k1.ID()}, st.KeyIDs)
	assert.NotZero(t, st.Version)
	assert.NotEmpty(t, st.Phase)
	assert.Equal(t, []string{k1.ID()}, f.rows())

	stateRaw, _ := f.stateBytes()
	assert.NotContains(t, string(stateRaw), raw1, "durable state is non-secret")
}

func TestCoordinator_Bootstrap_RefusesSecondTime(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	other, _ := newKey(t)
	f.bootstrap(k1)
	before := f.snapshot()

	for name, key := range map[string]managedturn.Key{"a different key": other, "the same key": k1} {
		t.Run(name, func(t *testing.T) {
			err := f.c.Bootstrap(ctx5(t), key)
			requireRejected(t, err, managedturn.ErrStateExists)
			assert.Empty(t, f.prober.callIDs(), "a refused bootstrap must not probe")
			f.requireUnchanged(before, "a refused bootstrap changes nothing")
		})
	}
}

func TestCoordinator_Bootstrap_ProbeFailurePublishesNothing(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	f.prober.setErr(managedturn.ErrProbeFailed)

	err := f.c.Bootstrap(ctx5(t), k1)

	requireRejected(t, err, managedturn.ErrProbeFailed)
	_, present := f.activeBytes()
	assert.False(t, present, "an unproven key must never become the active key")
}

// ---- AddCandidate -----------------------------------------------------------

func TestCoordinator_AddCandidate_AddsSecondKeyWithoutChangingActive(t *testing.T) {
	f := newFixture(t)
	k1, raw1 := newKey(t)
	k2, _ := newKey(t)
	f.bootstrap(k1)

	require.NoError(t, f.c.AddCandidate(ctx5(t), k2))

	assert.Equal(t, sortedIDs(k1, k2), f.rows())
	st := f.state()
	assert.ElementsMatch(t, []string{k1.ID(), k2.ID()}, st.KeyIDs)
	assert.Equal(t, k1.ID(), st.ActiveKeyID, "adding a candidate must not activate it")
	active, ok := f.activeBytes()
	require.True(t, ok)
	assert.Equal(t, []byte(raw1), active)
}

func TestCoordinator_AddCandidate_RefusesThirdKey(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	k3, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	before := f.snapshot()

	err := f.c.AddCandidate(ctx5(t), k3)

	requireRejected(t, err, managedturn.ErrKeySetFull)
	f.requireUnchanged(before, "a refused third key changes nothing")
	assert.Equal(t, sortedIDs(k1, k2), f.rows(), "the third key must not reach coturn's database")
}

func TestCoordinator_AddCandidate_RefusedBeforeBootstrap(t *testing.T) {
	f := newFixture(t)
	k, _ := newKey(t)

	err := f.c.AddCandidate(ctx5(t), k)

	requireRejected(t, err)
	assert.Empty(t, f.rows(), "no key may enter the database without a bootstrapped state")
}

// ---- Promote / Rollback -----------------------------------------------------

func TestCoordinator_Promote_UnknownKeyRefused(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	stranger, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	before := f.snapshot()

	err := f.c.Promote(ctx5(t), stranger)

	requireRejected(t, err, managedturn.ErrUnknownKey)
	assert.Empty(t, f.prober.callIDs(), "an unknown key is never probed")
	f.requireUnchanged(before, "an unknown key changes nothing")
	assert.Equal(t, uint64(1), f.state().Generation)
}

func TestCoordinator_Promote_ProbeFailureLeavesEverythingUnchanged(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	before := f.snapshot()
	f.prober.setErr(managedturn.ErrProbeFailed)

	err := f.c.Promote(ctx5(t), k2)

	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.Equal(t, []string{k2.ID()}, f.prober.callIDs(), "the candidate was actually probed")
	f.requireUnchanged(before, "a failed probe must leave the active file, state and rows untouched")
	st := f.state()
	assert.Equal(t, uint64(1), st.Generation, "generation does not advance on failure")
	assert.Equal(t, k1.ID(), st.ActiveKeyID)
}

func TestCoordinator_Promote_SuccessSwapsActiveFileAndBumpsGeneration(t *testing.T) {
	f := newFixture(t)
	k1, raw1 := newKey(t)
	k2, raw2 := newKey(t)
	f.bootstrapWithCandidate(k1, k2)

	var activeAtProbe []byte
	f.prober.hook = func(managedturn.Key) { activeAtProbe, _ = f.activeBytes() }

	require.NoError(t, f.c.Promote(ctx5(t), k2))

	assert.Equal(t, []byte(raw1), activeAtProbe, "the old key stays published until the new key is proven")
	assert.Equal(t, []string{k2.ID()}, f.prober.callIDs(), "the key being promoted is the one probed")
	active, ok := f.activeBytes()
	require.True(t, ok)
	assert.Equal(t, []byte(raw2), active)
	st := f.state()
	assert.Equal(t, uint64(2), st.Generation, "generation is previous+1")
	assert.Equal(t, k2.ID(), st.ActiveKeyID)
	assert.ElementsMatch(t, []string{k1.ID(), k2.ID()}, st.KeyIDs, "promotion retires nothing")
	assert.Equal(t, sortedIDs(k1, k2), f.rows(), "both keys stay in the database during overlap")
}

func TestCoordinator_Rollback_ReturnsToPreviousKeyWithNewGeneration(t *testing.T) {
	f := newFixture(t)
	k1, raw1 := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	require.NoError(t, f.c.Promote(ctx5(t), k2))
	f.prober.reset()

	require.NoError(t, f.c.Rollback(ctx5(t), k1))

	assert.Equal(t, []string{k1.ID()}, f.prober.callIDs(), "rollback re-proves the key before publishing it")
	active, ok := f.activeBytes()
	require.True(t, ok)
	assert.Equal(t, []byte(raw1), active)
	st := f.state()
	assert.Equal(t, k1.ID(), st.ActiveKeyID)
	assert.Equal(t, uint64(3), st.Generation, "rollback is a new generation, never a rewind")
}

func TestCoordinator_Rollback_UnknownKeyRefused(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	stranger, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	require.NoError(t, f.c.Promote(ctx5(t), k2))
	f.prober.reset()
	before := f.snapshot()

	err := f.c.Rollback(ctx5(t), stranger)

	requireRejected(t, err, managedturn.ErrUnknownKey)
	assert.Empty(t, f.prober.callIDs())
	f.requireUnchanged(before, "an unknown rollback target changes nothing")
}

func TestCoordinator_Rollback_ProbeFailureLeavesEverythingUnchanged(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	require.NoError(t, f.c.Promote(ctx5(t), k2))
	f.prober.reset()
	before := f.snapshot()
	f.prober.setErr(managedturn.ErrProbeFailed)

	err := f.c.Rollback(ctx5(t), k1)

	requireRejected(t, err, managedturn.ErrProbeFailed)
	f.requireUnchanged(before, "a failed rollback proof keeps the current key published")
	assert.Equal(t, uint64(2), f.state().Generation)
}

// ---- Recover ----------------------------------------------------------------

func TestCoordinator_Recover_ExactSetReinstallsActiveKeyAfterProbe(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, raw2 := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	require.NoError(t, f.c.Promote(ctx5(t), k2)) // active is the second key
	genBefore := f.state().Generation
	f.coldReboot()

	var rowsAtProbe []string
	var activeAtProbe bool
	f.prober.hook = func(managedturn.Key) {
		rowsAtProbe = f.rows()
		_, activeAtProbe = f.activeBytes()
	}

	// Supplied in the opposite order to prove the set, not the sequence, is compared.
	require.NoError(t, f.c.Recover(ctx5(t), []managedturn.Key{k2, k1}))

	assert.Equal(t, sortedIDs(k1, k2), rowsAtProbe, "the full recorded set is reinserted before the probe")
	assert.False(t, activeAtProbe, "nothing is published until the probe passes")
	assert.Equal(t, []string{k2.ID()}, f.prober.callIDs(), "the recorded ACTIVE key is probed")
	active, ok := f.activeBytes()
	require.True(t, ok)
	assert.Equal(t, []byte(raw2), active, "the recorded active key, not the first one supplied, is reinstalled")
	st := f.state()
	assert.Equal(t, k2.ID(), st.ActiveKeyID)
	assert.ElementsMatch(t, []string{k1.ID(), k2.ID()}, st.KeyIDs)
	assert.GreaterOrEqual(t, st.Generation, genBefore)
	assert.Equal(t, sortedIDs(k1, k2), f.rows())
}

func TestCoordinator_Recover_InterruptedPublishStillAdvancesTheGeneration(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	genBefore := f.state().Generation
	f.coldReboot()
	// A non-empty directory where the active file goes makes the publish fail
	// after the probe has passed.
	blocker := filepath.Join(f.activeDir, managedturn.ActiveFileName)
	f.prober.hook = func(managedturn.Key) { require.NoError(t, os.MkdirAll(filepath.Join(blocker, "x"), 0o700)) }

	require.Error(t, f.c.Recover(ctx5(t), []managedturn.Key{k1, k2}))
	assert.Equal(t, genBefore+1, f.state().Generation,
		"a CAS decision made before recovery must not survive an interrupted recover")

	require.NoError(t, os.RemoveAll(blocker))
	f.prober.hook = nil
	require.NoError(t, f.c.Recover(ctx5(t), []managedturn.Key{k1, k2}), "the interrupted recover can be replayed")
	_, ok := f.activeBytes()
	assert.True(t, ok)
	assert.Equal(t, genBefore+2, f.state().Generation)
}

func TestCoordinator_Recover_RefusesMismatchedKeySets(t *testing.T) {
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	k3, _ := newKey(t)

	cases := []struct {
		name string
		keys []managedturn.Key
	}{
		{"subset: one of two", []managedturn.Key{k1}},
		{"different second key", []managedturn.Key{k1, k3}},
		{"entirely different keys", []managedturn.Key{k3}},
		{"duplicate of one key", []managedturn.Key{k1, k1}},
		{"superset: three keys", []managedturn.Key{k1, k2, k3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.bootstrapWithCandidate(k1, k2)
			f.coldReboot()
			before := f.snapshot()

			err := f.c.Recover(ctx5(t), tc.keys)

			requireRejected(t, err, managedturn.ErrKeySetMismatch, managedturn.ErrKeySetFull)
			assert.Empty(t, f.prober.callIDs(), "a mismatched set is never probed")
			assert.Empty(t, f.rows(), "a mismatched set must not be inserted")
			_, present := f.activeBytes()
			assert.False(t, present, "a mismatched set must not restore readiness")
			f.requireUnchanged(before, "a refused recovery changes nothing")
		})
	}
}

func TestCoordinator_Recover_EmptyDatabaseAndStaleStateWithoutKeysIsNeverReady(t *testing.T) {
	for name, keys := range map[string][]managedturn.Key{"nil keys": nil, "empty keys": {}} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			k1, _ := newKey(t)
			k2, _ := newKey(t)
			f.bootstrapWithCandidate(k1, k2)
			f.coldReboot()
			before := f.snapshot()

			err := f.c.Recover(ctx5(t), keys)

			requireRejected(t, err, managedturn.ErrKeySetMismatch)
			assert.Empty(t, f.prober.callIDs())
			f.requireUnchanged(before, "stale state plus empty RAM stays closed")

			status, serr := f.c.Status(ctx5(t))
			require.NoError(t, serr)
			assert.True(t, status.HasState, "the stale state is still reported")
			assert.Zero(t, status.RowCount, "and the database is still empty")
			_, present := f.activeBytes()
			assert.False(t, present, "voice admission stays closed")
		})
	}
}

func TestCoordinator_Recover_RefusedWithoutAnyState(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)

	err := f.c.Recover(ctx5(t), []managedturn.Key{k1})

	requireRejected(t, err)
	assert.Empty(t, f.prober.callIDs())
	assert.Empty(t, f.rows())
	_, present := f.activeBytes()
	assert.False(t, present, "recover is not a bootstrap")
}

func TestCoordinator_Recover_ProbeFailureDoesNotReinstallActiveKey(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	f.coldReboot()
	f.prober.setErr(managedturn.ErrProbeFailed)

	err := f.c.Recover(ctx5(t), []managedturn.Key{k1, k2})

	requireRejected(t, err, managedturn.ErrProbeFailed)
	_, present := f.activeBytes()
	assert.False(t, present, "an unproven recovery must not reopen voice admission")
}

// ---- False success: a write that did not happen must not count --------------

func TestCoordinator_FalseSuccess_ReadOnlyStoreBlocksPromoteBeforeProbe(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	before := f.snapshot()

	require.NoError(t, f.store.Close())
	env := makeStoreReadOnly(t, f.dbPath, f.stateDir, f.activeDir)
	ro, openErr := managedturn.OpenStore(context.Background(), f.dbPath)
	if openErr != nil {
		env.Restore()
		requireRejected(t, openErr) // refusing a read-only database at open is also fail-closed
		return
	}
	t.Cleanup(func() { _ = ro.Close() })
	f.c.Store = ro
	if os.Geteuid() == nobodyID {
		f.c.ActiveGID = nobodyID // the dropped identity may only chown to its own group
	}

	err := f.c.Promote(ctx5(t), k2)
	env.Restore()

	requireRejected(t, err)
	assert.Empty(t, f.prober.callIDs(), "the probe must never run against a store that cannot be written")
	f.store = nil
	f.c.Store = nil
	after := unchangedFromDisk(t, f)
	assert.Equal(t, before.active, after.active, "the active file must not be touched")
	assert.Equal(t, before.state, after.state, "the state must not be touched")
	assert.Equal(t, uint64(1), stateGeneration(t, f))
}

func TestCoordinator_FalseSuccess_ReadOnlyStoreBlocksBootstrapBeforeProbe(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)

	require.NoError(t, f.store.Close())
	env := makeStoreReadOnly(t, f.dbPath, f.stateDir, f.activeDir)
	ro, openErr := managedturn.OpenStore(context.Background(), f.dbPath)
	if openErr != nil {
		env.Restore()
		requireRejected(t, openErr)
		return
	}
	t.Cleanup(func() { _ = ro.Close() })
	f.c.Store = ro
	if os.Geteuid() == nobodyID {
		f.c.ActiveGID = nobodyID
	}

	err := f.c.Bootstrap(ctx5(t), k1)
	env.Restore()

	requireRejected(t, err)
	assert.Empty(t, f.prober.callIDs(), "no probe may run when the key never reached the database")
	f.store, f.c.Store = nil, nil
	_, present := readActive(t, f)
	assert.False(t, present, "no active file may be published for a key coturn cannot see")
}

func TestCoordinator_FalseSuccess_LockedStoreBlocksPromoteBeforeProbe(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	before := f.snapshot()

	blocker := openRaw(t, f.dbPath, "rwc")
	conn, err := blocker.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`)
	require.NoError(t, err)

	short, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	perr := f.c.Promote(short, k2)

	requireRejected(t, perr)
	assert.Empty(t, f.prober.callIDs(), "the probe must never run while the store cannot be used")

	_, err = conn.ExecContext(context.Background(), `ROLLBACK`)
	require.NoError(t, err)
	f.requireUnchanged(before, "a failed promote leaves the published key and state alone")
}

// ---- Status -----------------------------------------------------------------

func TestCoordinator_Status_NoState(t *testing.T) {
	f := newFixture(t)

	st, err := f.c.Status(ctx5(t))

	require.NoError(t, err)
	assert.False(t, st.HasState)
	assert.Zero(t, st.RowCount)
}

func TestCoordinator_Status_ReportsStateAndRowCount(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrap(k1)

	st, err := f.c.Status(ctx5(t))
	require.NoError(t, err)
	assert.True(t, st.HasState)
	assert.Equal(t, 1, st.RowCount)
	assert.Equal(t, uint64(1), st.State.Generation)
	assert.Equal(t, k1.ID(), st.State.ActiveKeyID)

	require.NoError(t, f.c.AddCandidate(ctx5(t), k2))
	require.NoError(t, f.c.Promote(ctx5(t), k2))
	st, err = f.c.Status(ctx5(t))
	require.NoError(t, err)
	assert.Equal(t, 2, st.RowCount)
	assert.Equal(t, uint64(2), st.State.Generation)
	assert.Equal(t, k2.ID(), st.State.ActiveKeyID)
}

func TestCoordinator_Status_NeverIncludesKeyMaterial(t *testing.T) {
	f := newFixture(t)
	k1, raw1 := newKey(t)
	k2, raw2 := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	require.NoError(t, f.c.Promote(ctx5(t), k2))

	st, err := f.c.Status(ctx5(t))
	require.NoError(t, err)

	asJSON, jerr := json.Marshal(st)
	require.NoError(t, jerr)
	asJSONIndent, jerr := json.MarshalIndent(st, "", "  ")
	require.NoError(t, jerr)
	renderings := map[string]string{
		"%v":          fmt.Sprintf("%v", st),
		"%+v":         fmt.Sprintf("%+v", st),
		"%#v":         fmt.Sprintf("%#v", st),
		"json":        string(asJSON),
		"json indent": string(asJSONIndent),
		"state %+v":   fmt.Sprintf("%+v", st.State),
		"pointer %+v": fmt.Sprintf("%+v", &st),
	}
	names := make([]string, 0, len(renderings))
	for name := range renderings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out := renderings[name]
		assert.NotContains(t, out, raw1, "%s rendering leaked key 1", name)
		assert.NotContains(t, out, raw2, "%s rendering leaked key 2", name)
		assert.NotContains(t, out, raw1[:20], "%s rendering leaked a key prefix", name)
		assert.NotContains(t, out, raw2[:20], "%s rendering leaked a key prefix", name)
	}
	assert.Contains(t, string(asJSON), k2.ID(), "the non-secret key ID is what status reports")
}

func TestCoordinator_ErrorsNeverIncludeKeyMaterial(t *testing.T) {
	f := newFixture(t)
	k1, raw1 := newKey(t)
	k2, raw2 := newKey(t)
	k3, raw3 := newKey(t)
	f.bootstrapWithCandidate(k1, k2)

	errs := []error{
		f.c.Bootstrap(ctx5(t), k3),
		f.c.AddCandidate(ctx5(t), k3),
		f.c.Promote(ctx5(t), k3),
		f.c.Rollback(ctx5(t), k3),
		f.c.Recover(ctx5(t), []managedturn.Key{k3}),
	}
	for i, err := range errs {
		requireRejected(t, err)
		for _, raw := range []string{raw1, raw2, raw3} {
			assert.NotContains(t, err.Error(), raw, "error %d leaked key material", i)
		}
	}
}

// ---- helpers local to this file ---------------------------------------------

func readActive(t testing.TB, f *fixture) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.activeDir, activeFileName))
	if os.IsNotExist(err) {
		return nil, false
	}
	require.NoError(t, err)
	return b, true
}

type diskView struct{ active, state []byte }

func unchangedFromDisk(t testing.TB, f *fixture) diskView {
	t.Helper()
	a, _ := readActive(t, f)
	s, err := os.ReadFile(f.statePath)
	require.NoError(t, err)
	return diskView{active: a, state: s}
}

func stateGeneration(t testing.TB, f *fixture) uint64 {
	t.Helper()
	st, ok, err := managedturn.LoadState(f.statePath)
	require.NoError(t, err)
	require.True(t, ok)
	return st.Generation
}
