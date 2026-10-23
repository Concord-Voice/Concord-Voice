package managedturn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// cleanupTimeout bounds the row removal after a failed bootstrap probe. It
// runs on a context detached from the caller's, because the probe may have
// failed precisely because that context expired.
const cleanupTimeout = 5 * time.Second

// Coordinator sequences the store, probe, active file and state. See the
// package documentation for the invariants each transition keeps.
type Coordinator struct {
	Store     *Store
	Prober    Prober
	StatePath string
	ActiveDir string
	ActiveGID int
	Realm     string

	// ExpectGeneration, when non-nil, is the operator's compare-and-swap
	// guard: the generation the state must hold (0 for no state) for a
	// mutating transition to proceed. A mismatch is ErrStaleGeneration.
	ExpectGeneration *uint64
}

// Status is the non-secret view printed by the status op.
type Status struct {
	State    State
	HasState bool
	RowCount int
	// ActivePresent reports whether the active-key file exists. An absent
	// file means voice admission is closed.
	ActivePresent bool
	// ActiveFileKeyID is the ID of the key the issuer actually serves ("" when
	// the file is absent). It differs from State.ActiveKeyID only after a crash
	// between publish and state commit; see ErrActiveMismatch.
	ActiveFileKeyID string
}

// transition is one locked read-modify-write of the coordinator's state.
type transition struct {
	lock     *dirLock
	state    State
	has      bool
	activeID string // ID of the key in the active file, "" if absent
}

// mismatched reports whether the published key is not the one the state
// records: a crash between the active-file rename and the state commit, or
// tampering.
func (tr *transition) mismatched() bool {
	if tr.activeID == "" {
		return false
	}
	return !tr.has || tr.activeID != tr.state.ActiveKeyID
}

// begin validates the configuration, takes the transition lock, loads the
// state and the published key's ID, and applies the operator's generation
// guard.
func (c *Coordinator) begin(ctx context.Context, needProber bool) (*transition, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if needProber && c.Prober == nil {
		return nil, fmt.Errorf("%w: no prober", ErrInvalidConfig)
	}
	lock, err := lockDir(ctx, filepath.Dir(c.StatePath))
	if err != nil {
		return nil, err
	}
	tr, err := c.load(lock)
	if err != nil {
		lock.release()
		return nil, err
	}
	return tr, nil
}

func (c *Coordinator) load(lock *dirLock) (*transition, error) {
	st, has, err := LoadState(c.StatePath)
	if err != nil {
		return nil, err
	}
	if st.Generation == math.MaxUint64 {
		// Refused before any publish: no further generation exists.
		return nil, fmt.Errorf("%w: generation exhausted", ErrInvalidState)
	}
	if c.ExpectGeneration != nil && *c.ExpectGeneration != st.Generation {
		return nil, ErrStaleGeneration
	}
	activeID, err := readActiveKeyID(c.ActiveDir)
	if err != nil {
		return nil, err
	}
	return &transition{lock: lock, state: st, has: has, activeID: activeID}, nil
}

// validate checks what every operation, including Status, needs.
func (c *Coordinator) validate() error {
	if c.Store == nil || c.StatePath == "" || c.ActiveDir == "" || c.ActiveGID < 0 {
		return fmt.Errorf("%w: incomplete coordinator", ErrInvalidConfig)
	}
	return validateRealm(c.Realm)
}

// readActiveKeyID returns the ID of the key in dir/turn-key, or "" when the
// file is absent. The key itself never leaves this function. A symlink or a
// non-regular file is ErrUnsafePermission; content that is not a valid key is
// ErrActiveMismatch, since it cannot be any recorded key.
func readActiveKeyID(dir string) (_ string, err error) {
	f, err := os.OpenFile(filepath.Join(dir, ActiveFileName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0) // #nosec G304 -- configured active directory
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case errors.Is(err, syscall.ELOOP):
		return "", ErrUnsafePermission
	case err != nil:
		return "", fmt.Errorf("managedturn: open active file: %w", err)
	}
	defer func() { err = errors.Join(err, closeFile(f)) }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("managedturn: stat active file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", ErrUnsafePermission
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxKeyLen+1))
	defer clear(data)
	if err != nil {
		return "", fmt.Errorf("managedturn: read active file: %w", err)
	}
	k, err := ParseKey(data)
	if err != nil {
		return "", fmt.Errorf("%w: active file does not hold a valid key", ErrActiveMismatch)
	}
	return k.ID(), nil
}

// probe runs the prober and normalises any failure to ErrProbeFailed.
func (c *Coordinator) probe(ctx context.Context, key Key) error {
	err := c.Prober.Probe(ctx, key)
	if err == nil || errors.Is(err, ErrProbeFailed) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrProbeFailed, err)
}

// publish proves key, replaces the active file, then commits next under the
// generation the transition loaded. Nothing is published unless the probe
// passed; nothing is committed unless the publish succeeded.
func (c *Coordinator) publish(ctx context.Context, tr *transition, key Key, next State) error {
	if err := c.probe(ctx, key); err != nil {
		return err
	}
	if err := WriteActiveFile(c.ActiveDir, c.ActiveGID, key); err != nil {
		return err
	}
	return saveStateLocked(c.StatePath, tr.state.Generation, next)
}

// requireRows returns a store check: before the insert the rows must contain
// every id in must and nothing outside must ∪ may. It runs inside the write
// transaction, so the check and the insert are atomic.
func requireRows(must, may []string) func([]string) error {
	return func(rows []string) error {
		for _, id := range must {
			if !slices.Contains(rows, id) {
				return ErrStoreMismatch
			}
		}
		for _, id := range rows {
			if !slices.Contains(must, id) && !slices.Contains(may, id) {
				return ErrStoreMismatch
			}
		}
		return nil
	}
}

// sortedWith returns a sorted copy of ids plus extra.
func sortedWith(ids []string, extra ...string) []string {
	out := append(slices.Clone(ids), extra...)
	slices.Sort(out)
	return out
}

// Bootstrap is the first activation: it refuses any existing state, inserts
// key, proves it and publishes it as generation 1. A database already holding
// key (a retried bootstrap) is accepted; any other row is refused. When the
// probe fails the row is removed again, unless an earlier, interrupted
// bootstrap already published this very key.
func (c *Coordinator) Bootstrap(ctx context.Context, key Key) error {
	if !key.valid() {
		return ErrInvalidKey
	}
	tr, err := c.begin(ctx, true)
	if err != nil {
		return err
	}
	defer tr.lock.release()
	if tr.has {
		return ErrStateExists
	}
	id := key.ID()
	if tr.activeID != "" && tr.activeID != id {
		return ErrActiveMismatch
	}
	if err := c.Store.insertChecked(ctx, c.Realm, []Key{key}, requireRows(nil, []string{id})); err != nil {
		return err
	}
	if err := c.probe(ctx, key); err != nil {
		if tr.activeID == id {
			return err // published by the interrupted run: keep it
		}
		return errors.Join(err, c.removeUnpublished(ctx, id))
	}
	if err := WriteActiveFile(c.ActiveDir, c.ActiveGID, key); err != nil {
		return err
	}
	return saveStateLocked(c.StatePath, 0, State{
		Version:     stateVersion,
		Generation:  1,
		ActiveKeyID: id,
		KeyIDs:      []string{id},
		Phase:       PhaseActive,
	})
}

// removeUnpublished deletes the row for id that this transition inserted and
// never published, on a context detached from the caller's.
func (c *Coordinator) removeUnpublished(ctx context.Context, id string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if _, err := c.Store.deleteRows(cctx, c.Realm, func(rowID string) bool { return rowID == id }); err != nil {
		return fmt.Errorf("%w: %w", ErrUnprovenKeyLeft, err)
	}
	return nil
}

// AddCandidate inserts a second key without publishing it. It refuses when
// the set is full, when the key is already in the set, and when the database
// does not hold the recorded set. The generation is unchanged.
//
// It runs no probe. The only failure after its insert is the state write,
// whose outcome (renamed or not) cannot be known in-process, so it removes
// nothing itself: DiscardUnrecorded later decides from the state on disk.
func (c *Coordinator) AddCandidate(ctx context.Context, key Key) error {
	if !key.valid() {
		return ErrInvalidKey
	}
	tr, err := c.begin(ctx, false)
	if err != nil {
		return err
	}
	defer tr.lock.release()
	if !tr.has {
		return ErrNoState
	}
	if tr.mismatched() {
		return ErrActiveMismatch
	}
	if len(tr.state.KeyIDs) >= MaxKeys {
		return ErrKeySetFull
	}
	id := key.ID()
	if tr.state.hasKey(id) {
		return fmt.Errorf("%w: key already in set", ErrInvalidTransition)
	}
	if err := c.Store.insertChecked(ctx, c.Realm, []Key{key}, requireRows(tr.state.KeyIDs, []string{id})); err != nil {
		return err
	}
	next := tr.state
	next.KeyIDs = sortedWith(tr.state.KeyIDs, id)
	next.Phase = PhaseCandidate
	return saveStateLocked(c.StatePath, tr.state.Generation, next)
}

// Promote publishes a key from the set other than the active one, after a
// fresh proof, as a new generation. Both keys stay in the database.
func (c *Coordinator) Promote(ctx context.Context, key Key) error {
	return c.switchTo(ctx, key, false)
}

// Rollback is Promote restricted to a previously proven key: it is refused
// while the other key is a never-proven candidate.
func (c *Coordinator) Rollback(ctx context.Context, key Key) error {
	return c.switchTo(ctx, key, true)
}

func (c *Coordinator) switchTo(ctx context.Context, key Key, rollback bool) error {
	if !key.valid() {
		return ErrInvalidKey
	}
	tr, err := c.begin(ctx, true)
	if err != nil {
		return err
	}
	defer tr.lock.release()
	if !tr.has {
		return ErrNoState
	}
	id := key.ID()
	replay := tr.mismatched()
	switch {
	case !tr.state.hasKey(id):
		return ErrUnknownKey
	case replay && id != tr.activeID:
		// The issuer serves a key the state does not record as active; only
		// completing that interrupted switch is allowed.
		return ErrActiveMismatch
	case id == tr.state.ActiveKeyID:
		return fmt.Errorf("%w: key is already active", ErrInvalidTransition)
	case rollback && !replay && tr.state.Phase != PhaseActive:
		// A replay is exempt: the file's key was proven when it was published.
		return fmt.Errorf("%w: rollback target was never proven", ErrInvalidTransition)
	}
	// The key is already a row: this write proves the store is writable and
	// that it still holds exactly the recorded set, before any probe runs.
	if err := c.Store.insertChecked(ctx, c.Realm, []Key{key}, requireRows(tr.state.KeyIDs, nil)); err != nil {
		return err
	}
	next := tr.state
	next.Generation++
	next.ActiveKeyID = id
	next.Phase = PhaseActive // both keys have now been proven
	if replay {
		// The file already holds this key: prove it afresh, then commit the
		// state the interrupted run never wrote, without re-publishing.
		if err := c.probe(ctx, key); err != nil {
			return err
		}
		return saveStateLocked(c.StatePath, tr.state.Generation, next)
	}
	return c.publish(ctx, tr, key, next)
}

// Recover is the cold path after RAM loss: keys must be exactly the recorded
// set (order free, no duplicates, nothing extra). It reinserts them, proves
// the recorded active key and reinstalls it as a new generation. It never
// infers a key set from the database or a stale active file.
func (c *Coordinator) Recover(ctx context.Context, keys []Key) error {
	tr, err := c.begin(ctx, true)
	if err != nil {
		return err
	}
	defer tr.lock.release()
	if !tr.has {
		return ErrNoState
	}
	if tr.mismatched() {
		return ErrActiveMismatch
	}
	byID := make(map[string]Key, len(keys))
	for _, k := range keys {
		if k.valid() {
			byID[k.ID()] = k
		}
	}
	if len(keys) != len(tr.state.KeyIDs) || len(byID) != len(keys) {
		return ErrKeySetMismatch
	}
	for _, id := range tr.state.KeyIDs {
		if _, ok := byID[id]; !ok {
			return ErrKeySetMismatch
		}
	}
	if err := c.Store.insertChecked(ctx, c.Realm, keys, requireRows(nil, tr.state.KeyIDs)); err != nil {
		return err
	}
	next := tr.state
	next.Generation++
	key := byID[tr.state.ActiveKeyID]
	if err := c.probe(ctx, key); err != nil {
		return err
	}
	// Recover republishes the key the state already records as active, so it
	// commits the state first: an interrupted run then leaves the new
	// generation with the file absent (not ready) or unchanged, never a
	// republished file under the generation an operator saw before recovery.
	if err := saveStateLocked(c.StatePath, tr.state.Generation, next); err != nil {
		return err
	}
	return WriteActiveFile(c.ActiveDir, c.ActiveGID, key)
}

// DiscardUnrecorded removes turn_secret rows that are neither recorded in the
// state nor published in the active file: the leftovers of a bootstrap or
// add-candidate that inserted a key and then crashed. No credential was ever
// minted from such a row (only the in-process probe credential, which expires
// within MaxProbeTTL), so deleting it revokes nothing anyone holds. With no
// state, the active file must be absent and every row is removed. It never
// touches a recorded or published key, the active file, the state, or the
// generation, and refuses while the published key disagrees with the state.
// It returns the number of rows removed.
func (c *Coordinator) DiscardUnrecorded(ctx context.Context) (int, error) {
	tr, err := c.begin(ctx, false)
	if err != nil {
		return 0, err
	}
	defer tr.lock.release()
	if tr.mismatched() {
		return 0, ErrActiveMismatch
	}
	keep := map[string]bool{}
	if tr.has {
		for _, id := range tr.state.KeyIDs {
			keep[id] = true
		}
	}
	if tr.activeID != "" {
		keep[tr.activeID] = true // already recorded when not mismatched; kept regardless
	}
	return c.Store.deleteRows(ctx, c.Realm, func(id string) bool { return id == "" || !keep[id] })
}

// Status reports the state, the realm's row count and the published key's ID.
// It takes no lock.
func (c *Coordinator) Status(ctx context.Context) (Status, error) {
	if err := c.validate(); err != nil {
		return Status{}, err
	}
	st, has, err := LoadState(c.StatePath)
	if err != nil {
		return Status{}, err
	}
	n, err := c.Store.rowCount(ctx, c.Realm)
	if err != nil {
		return Status{}, err
	}
	activeID, err := readActiveKeyID(c.ActiveDir)
	if err != nil {
		return Status{}, err
	}
	return Status{State: st, HasState: has, RowCount: n, ActivePresent: activeID != "", ActiveFileKeyID: activeID}, nil
}
