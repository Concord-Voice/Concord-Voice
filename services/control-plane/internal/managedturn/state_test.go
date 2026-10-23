package managedturn_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

func newStatePath(t testing.TB) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return filepath.Join(dir, "state.json")
}

func sampleState(t testing.TB, gen uint64) managedturn.State {
	t.Helper()
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	return managedturn.State{
		Version:     1,
		Generation:  gen,
		ActiveKeyID: k1.ID(),
		KeyIDs:      []string{k1.ID(), k2.ID()},
		Phase:       "active",
	}
}

func TestLoadState_MissingFileIsNotAnError(t *testing.T) {
	st, ok, err := managedturn.LoadState(newStatePath(t))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, managedturn.State{}, st)
}

func TestState_SaveLoadRoundTrip(t *testing.T) {
	path := newStatePath(t)
	want := sampleState(t, 1)

	require.NoError(t, managedturn.SaveState(path, 0, want))

	got, ok, err := managedturn.LoadState(path)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestState_SaveAdvancesGenerationChain(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))
	second := sampleState(t, 2)
	require.NoError(t, managedturn.SaveState(path, 1, second))

	got, ok, err := managedturn.LoadState(path)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, second, got)
	assert.Equal(t, uint64(2), got.Generation)
}

func TestState_FileModeIs0600(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, managedturn.SaveState(path, 1, sampleState(t, 2)))
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "replacement keeps the restrictive mode")
}

func TestState_NoTemporaryFilesRemain(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))
	require.NoError(t, managedturn.SaveState(path, 1, sampleState(t, 2)))
	require.Error(t, managedturn.SaveState(path, 99, sampleState(t, 100)), "stale write also must not leave a temp file")

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"state.json"}, names)
}

func TestState_StaleGenerationRefusedAndFileUnchanged(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))
	before, err := os.ReadFile(path) // #nosec G304 -- test temp-dir path.
	require.NoError(t, err)

	for _, prev := range []uint64{0, 2, 7} {
		err := managedturn.SaveState(path, prev, sampleState(t, prev+1))
		requireRejected(t, err, managedturn.ErrStaleGeneration)
	}

	after, err := os.ReadFile(path) // #nosec G304 -- test temp-dir path.
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused compare-and-swap must not change the file")
}

func TestState_MissingFileStalePrevRefused(t *testing.T) {
	path := newStatePath(t)
	err := managedturn.SaveState(path, 5, sampleState(t, 6))
	requireRejected(t, err, managedturn.ErrStaleGeneration)

	_, ok, lerr := managedturn.LoadState(path)
	require.NoError(t, lerr)
	assert.False(t, ok, "no file may appear from a refused write")
}

func TestState_ConcurrentSavesHaveExactlyOneWinner(t *testing.T) {
	path := newStatePath(t)
	require.NoError(t, managedturn.SaveState(path, 0, sampleState(t, 1)))

	const writers = 8
	var wins, stale atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		next := sampleState(t, 2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch err := managedturn.SaveState(path, 1, next); {
			case err == nil:
				wins.Add(1)
			case isErr(err, managedturn.ErrStaleGeneration):
				stale.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), wins.Load(), "compare-and-swap on generation admits exactly one writer")
	assert.Equal(t, int32(writers-1), stale.Load())
	got, ok, err := managedturn.LoadState(path)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(2), got.Generation)
}

func TestLoadState_CorruptFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"not json", "this is not json"},
		{"truncated json", `{"version":1,"generation":3,"active_key_id":"ab`},
		{"empty file", ""},
		{"json array", `[1,2,3]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := newStatePath(t)
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			st, ok, err := managedturn.LoadState(path)
			requireRejected(t, err)
			assert.False(t, ok)
			assert.Equal(t, managedturn.State{}, st)
		})
	}
}

func TestLoadState_OversizedFileErrors(t *testing.T) {
	path := newStatePath(t)
	// A syntactically valid record padded far past any sane state size; the
	// reader must refuse rather than slurp it.
	payload := `{"version":1,"generation":1,"active_key_id":"` + strings.Repeat("a", 4<<20) + `","key_ids":[],"phase":"active"}`
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))

	_, ok, err := managedturn.LoadState(path)
	requireRejected(t, err)
	assert.False(t, ok)
}

func TestState_DoesNotPersistKeyMaterial(t *testing.T) {
	path := newStatePath(t)
	k1, raw1 := newKey(t)
	k2, raw2 := newKey(t)
	st := managedturn.State{Version: 1, Generation: 1, ActiveKeyID: k1.ID(), KeyIDs: []string{k1.ID(), k2.ID()}, Phase: "active"}
	require.NoError(t, managedturn.SaveState(path, 0, st))

	data, err := os.ReadFile(path) // #nosec G304 -- test temp-dir path.
	require.NoError(t, err)
	assert.NotContains(t, string(data), raw1)
	assert.NotContains(t, string(data), raw2)
}
