package managedturn_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// These cases cover the refusals the implementation adds on top of the plan's
// minimum: structural state validation, path safety, schema drift, the
// operator generation guard and the database-agrees-with-state rule.

// ---- Key --------------------------------------------------------------------

func TestKey_StringersAndMarshalersRedact(t *testing.T) {
	k, raw := newKey(t)
	assert.NotContains(t, k.String(), raw)
	assert.NotContains(t, k.GoString(), raw)

	_, err := json.Marshal(struct{ K managedturn.Key }{K: k})
	require.Error(t, err, "a key must refuse JSON serialisation")
	assert.NotContains(t, err.Error(), raw)
	_, err = k.MarshalText()
	require.Error(t, err)
}

func TestKey_ZeroKeyHasNoID(t *testing.T) {
	assert.Empty(t, managedturn.Key{}.ID())
}

func TestReadKeys_RejectsCountOutsideBounds(t *testing.T) {
	for _, n := range []int{0, -1, managedturn.MaxKeys + 1} {
		_, err := managedturn.ReadKeys(strings.NewReader(newKeyString(t)), n)
		requireRejected(t, err, managedturn.ErrInvalidConfig)
	}
}

// ---- State ------------------------------------------------------------------

func TestSaveState_RefusesInvalidRecords(t *testing.T) {
	good := sampleState(t, 1)
	other, _ := newKey(t)
	cases := map[string]func(*managedturn.State){
		"version":               func(s *managedturn.State) { s.Version = 2 },
		"zero generation":       func(s *managedturn.State) { s.Generation = 0 },
		"empty key set":         func(s *managedturn.State) { s.KeyIDs = nil },
		"three keys":            func(s *managedturn.State) { s.KeyIDs = append(s.KeyIDs, other.ID()) },
		"unknown phase":         func(s *managedturn.State) { s.Phase = "retiring" },
		"candidate with one":    func(s *managedturn.State) { s.Phase = managedturn.PhaseCandidate; s.KeyIDs = s.KeyIDs[:1] },
		"malformed id":          func(s *managedturn.State) { s.KeyIDs[1] = "not-hex" },
		"duplicate id":          func(s *managedturn.State) { s.KeyIDs[1] = s.KeyIDs[0] },
		"active not in the set": func(s *managedturn.State) { s.ActiveKeyID = other.ID() },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := newStatePath(t)
			st := good
			st.KeyIDs = append([]string(nil), good.KeyIDs...)
			mutate(&st)
			requireRejected(t, managedturn.SaveState(path, 0, st), managedturn.ErrInvalidState)
			_, ok, err := managedturn.LoadState(path)
			require.NoError(t, err)
			assert.False(t, ok, "an invalid record is never written")
		})
	}
}

func TestSaveState_GenerationMayOnlyStayOrAdvanceByOne(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))
	requireRejected(t, managedturn.SaveState(path, 1, sampleState(t, 3)), managedturn.ErrInvalidState)
	require.NoError(t, managedturn.SaveState(path, 1, sampleState(t, 1)), "same generation is a non-publishing update")
	require.NoError(t, managedturn.SaveState(path, 1, sampleState(t, 2)))
}

func TestLoadState_RefusesUnsafeOrUnexpectedFiles(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		path := newStatePath(t)
		target := filepath.Join(filepath.Dir(path), "target.json")
		require.NoError(t, managedturn.SaveState(target, 0, sampleState(t, 1)))
		require.NoError(t, os.Symlink(target, path))
		_, ok, err := managedturn.LoadState(path)
		requireRejected(t, err, managedturn.ErrUnsafePermission)
		assert.False(t, ok)
	})
	t.Run("group writable", func(t *testing.T) {
		path := newStatePath(t)
		require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))
		require.NoError(t, os.Chmod(path, 0o620)) // #nosec G302 -- deliberate test fixture mode on a temp path.
		_, _, err := managedturn.LoadState(path)
		requireRejected(t, err, managedturn.ErrUnsafePermission)
	})
	t.Run("directory", func(t *testing.T) {
		path := newStatePath(t)
		require.NoError(t, os.Mkdir(path, 0o700))
		_, _, err := managedturn.LoadState(path)
		requireRejected(t, err)
	})
	t.Run("unknown field", func(t *testing.T) {
		path := newStatePath(t)
		st := sampleState(t, 1)
		raw, err := json.Marshal(st)
		require.NoError(t, err)
		withExtra := strings.Replace(string(raw), "{", `{"secret":"x",`, 1)
		require.NoError(t, os.WriteFile(path, []byte(withExtra), 0o600))
		_, _, err = managedturn.LoadState(path)
		requireRejected(t, err, managedturn.ErrInvalidState)
	})
	t.Run("trailing data", func(t *testing.T) {
		path := newStatePath(t)
		raw, err := json.Marshal(sampleState(t, 1))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(raw, []byte(" {}")...), 0o600))
		_, _, err = managedturn.LoadState(path)
		requireRejected(t, err, managedturn.ErrInvalidState)
	})
}

func TestSaveState_RefusesUnsafeDirectory(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o777))       // #nosec G302 -- deliberate test fixture mode on a temp path.
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) }) // #nosec G302 -- deliberate test fixture mode on a temp path.
	requireRejected(t, managedturn.SaveState(path, 0, sampleState(t, 1)), managedturn.ErrUnsafePermission)
}

// ---- Active file ------------------------------------------------------------

func TestWriteActiveFile_RefusesUnsafeInputs(t *testing.T) {
	k, _ := newKey(t)

	t.Run("negative gid", func(t *testing.T) {
		requireRejected(t, managedturn.WriteActiveFile(newActiveDir(t), -1, k), managedturn.ErrInvalidConfig)
	})
	t.Run("world-writable directory", func(t *testing.T) {
		dir := newActiveDir(t)
		require.NoError(t, os.Chmod(dir, 0o777)) // #nosec G302 -- deliberate test fixture mode on a temp path.
		requireRejected(t, managedturn.WriteActiveFile(dir, os.Getgid(), k), managedturn.ErrUnsafePermission)
		assert.Empty(t, dirNames(t, dir))
	})
	t.Run("symlinked directory", func(t *testing.T) {
		dir := newActiveDir(t)
		link := filepath.Join(t.TempDir(), "link")
		require.NoError(t, os.Symlink(dir, link))
		requireRejected(t, managedturn.WriteActiveFile(link, os.Getgid(), k), managedturn.ErrUnsafePermission)
		assert.Empty(t, dirNames(t, dir))
	})
	t.Run("directory at the target", func(t *testing.T) {
		dir := newActiveDir(t)
		require.NoError(t, os.Mkdir(filepath.Join(dir, activeFileName), 0o700))
		requireRejected(t, managedturn.WriteActiveFile(dir, os.Getgid(), k), managedturn.ErrUnsafePermission)
	})
}

// ---- Store ------------------------------------------------------------------

func TestOpenStore_RefusesUnsafePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("symlink database", func(t *testing.T) {
		dir := shareableTempDir(t)
		target := filepath.Join(dir, "target")
		_ = openInitStore(t, target)
		path := filepath.Join(dir, "turndb")
		require.NoError(t, os.Symlink(target, path))
		_, err := managedturn.OpenStore(ctx, path)
		requireRejected(t, err, managedturn.ErrUnsafePermission)
	})
	t.Run("group-writable database", func(t *testing.T) {
		path := newDBPath(t)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Chmod(path, 0o660)) // #nosec G302 -- deliberate test fixture mode on a temp path.
		_, err := managedturn.OpenStore(ctx, path)
		requireRejected(t, err, managedturn.ErrUnsafePermission)
	})
	t.Run("world-writable directory", func(t *testing.T) {
		path := newDBPath(t)
		require.NoError(t, os.Chmod(filepath.Dir(path), 0o777)) // #nosec G302 -- deliberate test fixture mode on a temp path.
		_, err := managedturn.OpenStore(ctx, path)
		requireRejected(t, err, managedturn.ErrUnsafePermission)
		_, serr := os.Stat(path)
		assert.True(t, os.IsNotExist(serr), "no database is created in an unsafe directory")
	})
	t.Run("not a database", func(t *testing.T) {
		path := newDBPath(t)
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("not sqlite ", 100)), 0o600))
		_, err := managedturn.OpenStore(ctx, path)
		requireRejected(t, err)
	})
}

func TestOpenStore_CreatesFileWithCoturnReadableMode(t *testing.T) {
	path := newDBPath(t)
	_ = openInitStore(t, path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestStore_InitSchema_RefusesDriftAndForeignObjects(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		withInit bool // create coturn's schema first
		stmt     string
	}{
		{"altered coturn table", false, `CREATE TABLE turn_secret (realm text, value text)`},
		{"foreign table", true, `CREATE TABLE extra (x int)`},
		{"trigger", true, `CREATE TRIGGER t AFTER INSERT ON turnusers_lt BEGIN SELECT 1; END`},
		{"view", true, `CREATE VIEW v AS SELECT value FROM turn_secret`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newDBPath(t)
			if tc.withInit {
				s := openInitStore(t, path)
				require.NoError(t, s.Close())
			}
			raw := openRaw(t, path, "rwc")
			_, err := raw.Exec(tc.stmt)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			require.NoError(t, os.Chmod(path, 0o640)) // #nosec G302 -- deliberate test fixture mode on a temp path.

			s, err := managedturn.OpenStore(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			requireRejected(t, s.InitSchema(ctx), managedturn.ErrInvalidConfig)
		})
	}
}

func TestStore_KeyIDsRefusesInvalidStoredValue(t *testing.T) {
	ctx := context.Background()
	path := newDBPath(t)
	s := openInitStore(t, path)
	raw := openRaw(t, path, "rw")
	_, err := raw.Exec(`INSERT INTO turn_secret (realm, value) VALUES (?, ?)`, testRealm, "short")
	require.NoError(t, err)

	_, err = s.KeyIDs(ctx, testRealm)
	requireRejected(t, err, managedturn.ErrInvalidKey)
}

func TestStore_InsertRefusesInvalidRealm(t *testing.T) {
	ctx := context.Background()
	s := openInitStore(t, newDBPath(t))
	k, _ := newKey(t)
	for _, realm := range []string{"", "has space", strings.Repeat("r", managedturn.MaxKeyLen+1), "tab\t"} {
		requireRejected(t, s.Insert(ctx, realm, k), managedturn.ErrInvalidConfig)
	}
	ids, err := s.KeyIDs(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// ---- Coordinator ------------------------------------------------------------

func TestCoordinator_ExpectGenerationGuard(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	gen := func(n uint64) *uint64 { return &n }

	f.c.ExpectGeneration = gen(1)
	requireRejected(t, f.c.Bootstrap(ctx5(t), k1), managedturn.ErrStaleGeneration)
	assert.Empty(t, f.prober.callIDs(), "a stale guard refuses before any probe")
	assert.Empty(t, f.rows(), "or any insert")

	f.c.ExpectGeneration = gen(0)
	require.NoError(t, f.c.Bootstrap(ctx5(t), k1))
	f.c.ExpectGeneration = gen(1)
	require.NoError(t, f.c.AddCandidate(ctx5(t), k2), "add-candidate keeps generation 1")
	before := f.snapshot()

	f.c.ExpectGeneration = gen(0)
	requireRejected(t, f.c.Promote(ctx5(t), k2), managedturn.ErrStaleGeneration)
	f.requireUnchanged(before, "a stale operator guard changes nothing")

	f.c.ExpectGeneration = gen(1)
	require.NoError(t, f.c.Promote(ctx5(t), k2))
	assert.Equal(t, uint64(2), f.state().Generation)
}

func TestCoordinator_RefusesMisconfiguration(t *testing.T) {
	k, _ := newKey(t)
	cases := []struct {
		name         string
		mutate       func(*managedturn.Coordinator)
		statusRefuse bool // Status needs everything but the prober
	}{
		{"no store", func(c *managedturn.Coordinator) { c.Store = nil }, true},
		{"no prober", func(c *managedturn.Coordinator) { c.Prober = nil }, false},
		{"no state", func(c *managedturn.Coordinator) { c.StatePath = "" }, true},
		{"no active", func(c *managedturn.Coordinator) { c.ActiveDir = "" }, true},
		{"negative gid", func(c *managedturn.Coordinator) { c.ActiveGID = -1 }, true},
		{"empty realm", func(c *managedturn.Coordinator) { c.Realm = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.mutate(f.c)
			requireRejected(t, f.c.Bootstrap(ctx5(t), k), managedturn.ErrInvalidConfig)
			assert.Empty(t, f.rows(), "a misconfigured coordinator writes nothing")
			_, err := f.c.Status(ctx5(t))
			if tc.statusRefuse {
				requireRejected(t, err, managedturn.ErrInvalidConfig)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCoordinator_RefusesZeroKeys(t *testing.T) {
	f := newFixture(t)
	zero := managedturn.Key{}
	for _, err := range []error{
		f.c.Bootstrap(ctx5(t), zero),
		f.c.AddCandidate(ctx5(t), zero),
		f.c.Promote(ctx5(t), zero),
		f.c.Rollback(ctx5(t), zero),
	} {
		requireRejected(t, err, managedturn.ErrInvalidKey)
	}
	assert.Empty(t, f.prober.callIDs())
}

func TestCoordinator_TransitionsBeforeBootstrapRefused(t *testing.T) {
	f := newFixture(t)
	k, _ := newKey(t)
	requireRejected(t, f.c.AddCandidate(ctx5(t), k), managedturn.ErrNoState)
	requireRejected(t, f.c.Promote(ctx5(t), k), managedturn.ErrNoState)
	requireRejected(t, f.c.Rollback(ctx5(t), k), managedturn.ErrNoState)
	requireRejected(t, f.c.Recover(ctx5(t), []managedturn.Key{k}), managedturn.ErrNoState)
	assert.Empty(t, f.rows())
}

func TestCoordinator_InvalidTransitionsRefused(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrap(k1)

	requireRejected(t, f.c.AddCandidate(ctx5(t), k1), managedturn.ErrInvalidTransition)
	requireRejected(t, f.c.Promote(ctx5(t), k1), managedturn.ErrInvalidTransition)

	require.NoError(t, f.c.AddCandidate(ctx5(t), k2))
	f.prober.reset()
	before := f.snapshot()
	err := f.c.Rollback(ctx5(t), k2)
	requireRejected(t, err, managedturn.ErrInvalidTransition)
	assert.Empty(t, f.prober.callIDs(), "an unproven candidate is never a rollback target")
	f.requireUnchanged(before, "a refused rollback changes nothing")
	assert.Equal(t, managedturn.PhaseCandidate, f.state().Phase)

	require.NoError(t, f.c.Promote(ctx5(t), k2))
	assert.Equal(t, managedturn.PhaseActive, f.state().Phase, "after promotion every key is proven")
	requireRejected(t, f.c.Rollback(ctx5(t), k2), managedturn.ErrInvalidTransition)
}

func TestCoordinator_ColdDatabaseRefusesEverythingButRecover(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrapWithCandidate(k1, k2)
	require.NoError(t, f.c.Promote(ctx5(t), k2))
	f.coldReboot()
	before := f.snapshot()

	requireRejected(t, f.c.Rollback(ctx5(t), k1), managedturn.ErrStoreMismatch)
	assert.Empty(t, f.prober.callIDs(), "an empty database with stale state is never proven")
	f.requireUnchanged(before, "and never republished")

	st, err := f.c.Status(ctx5(t))
	require.NoError(t, err)
	assert.False(t, st.ActivePresent)
	assert.Zero(t, st.RowCount)
}

func TestCoordinator_AddCandidateRefusesDivergedDatabase(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrap(k1)
	f.coldReboot()

	requireRejected(t, f.c.AddCandidate(ctx5(t), k2), managedturn.ErrStoreMismatch)
	assert.Empty(t, f.rows(), "a candidate cannot refill a cold database")
}

func TestCoordinator_AddCandidateNeedsNoProber(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	f.bootstrap(k1)
	f.c.Prober = nil

	require.NoError(t, f.c.AddCandidate(ctx5(t), k2), "add-candidate runs no probe")
	assert.Len(t, f.rows(), 2)
}

func TestCoordinator_BootstrapForeignRowRefusedButRetryAccepted(t *testing.T) {
	t.Run("foreign row", func(t *testing.T) {
		f := newFixture(t)
		stray, _ := newKey(t)
		k1, _ := newKey(t)
		require.NoError(t, f.store.Insert(ctx5(t), testRealm, stray))
		requireRejected(t, f.c.Bootstrap(ctx5(t), k1), managedturn.ErrStoreMismatch)
		assert.Empty(t, f.prober.callIDs())
	})
	t.Run("retry after a failed probe", func(t *testing.T) {
		f := newFixture(t)
		k1, raw1 := newKey(t)
		f.prober.setErr(managedturn.ErrProbeFailed)
		requireRejected(t, f.c.Bootstrap(ctx5(t), k1), managedturn.ErrProbeFailed)
		assert.Empty(t, f.rows(), "an unproven bootstrap key is removed again")

		f.prober.setErr(nil)
		require.NoError(t, f.c.Bootstrap(ctx5(t), k1))
		active, ok := f.activeBytes()
		require.True(t, ok)
		assert.Equal(t, []byte(raw1), active)
		assert.Equal(t, uint64(1), f.state().Generation)
	})
}

func TestCoordinator_ProberErrorsNormaliseToErrProbeFailed(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	f.prober.setErr(errors.New("transport exploded"))
	err := f.c.Bootstrap(ctx5(t), k1)
	requireRejected(t, err, managedturn.ErrProbeFailed)
}

func TestCoordinator_StatusReportsActivePresence(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)
	st, err := f.c.Status(ctx5(t))
	require.NoError(t, err)
	assert.False(t, st.ActivePresent)

	f.bootstrap(k1)
	st, err = f.c.Status(ctx5(t))
	require.NoError(t, err)
	assert.True(t, st.ActivePresent)
	assert.Equal(t, managedturn.PhaseActive, st.State.Phase)
}

func TestCoordinator_TransitionWaitsForLockAndHonoursContext(t *testing.T) {
	f := newFixture(t)
	k1, _ := newKey(t)

	// Hold the transition lock from inside a probe and try a second
	// transition meanwhile: it must not proceed, and must give up with its
	// own context.
	var inner error
	var nested bool
	f.prober.hook = func(managedturn.Key) {
		if nested {
			return // without the lock the inner call would reach here; never recurse
		}
		nested = true
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		inner = f.c.Bootstrap(ctx, k1)
	}
	require.NoError(t, f.c.Bootstrap(ctx5(t), k1))
	require.ErrorIs(t, inner, context.DeadlineExceeded, "a concurrent transition waits on the lock")
	assert.Len(t, f.prober.callIDs(), 1, "the waiting transition never probed")
}

// ---- Prober configuration ---------------------------------------------------

func TestTURNProber_RefusesIncompleteOrInsecureConfig(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	for name, mutate := range map[string]func(*managedturn.TURNProber){
		"no realm":       func(p *managedturn.TURNProber) { p.Realm = "" },
		"no udp":         func(p *managedturn.TURNProber) { p.UDPAddr = "" },
		"no tls config":  func(p *managedturn.TURNProber) { p.TLSConfig = nil },
		"skip verify":    func(p *managedturn.TURNProber) { p.TLSConfig = &tls.Config{InsecureSkipVerify: true} }, // #nosec G402 -- asserting refusal
		"bad udp port":   func(p *managedturn.TURNProber) { p.UDPAddr = "127.0.0.1:0" },
		"bad udp syntax": func(p *managedturn.TURNProber) { p.UDPAddr = "no-port" },
	} {
		t.Run(name, func(t *testing.T) {
			p := proberFor(f)
			mutate(p)
			err := p.Probe(withTimeout(t, 4*time.Second), k)
			requireRejected(t, err, managedturn.ErrProbeFailed)
			assert.Zero(t, f.totalCreated())
		})
	}
	err := proberFor(f).Probe(withTimeout(t, 4*time.Second), managedturn.Key{})
	requireRejected(t, err, managedturn.ErrInvalidKey)
}

func TestTURNProber_DefaultsNowAndTTL(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	p := proberFor(f)
	p.Now = nil
	p.TTL = 0
	p.MaxRetries = 2
	require.NoError(t, p.Probe(withTimeout(t, 15*time.Second), k))
	for _, u := range f.seenUsernames() {
		assert.True(t, strings.HasSuffix(u, ":"+managedturn.ProbeUserTag))
	}
}

func TestTURNProber_RetriesAreBoundedForWrongKey(t *testing.T) {
	wrong, _ := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: newKeyString(t)})
	p := proberFor(f)
	p.MaxRetries = 1
	err := p.Probe(withTimeout(t, 15*time.Second), wrong)
	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.Zero(t, f.totalCreated())
}

func TestMintProbeCredential_ZeroKeyAndClamp(t *testing.T) {
	u, p := managedturn.MintProbeCredential(managedturn.Key{}, time.Second, time.Unix(1, 0))
	assert.Empty(t, u)
	assert.Empty(t, p)

	k, _ := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	for _, ttl := range []time.Duration{0, -time.Second, time.Hour} {
		u, _ := managedturn.MintProbeCredential(k, ttl, now)
		assert.Equal(t, fmt.Sprintf("%d:%s", now.Add(managedturn.MaxProbeTTL).Unix(), managedturn.ProbeUserTag), u)
	}
}
