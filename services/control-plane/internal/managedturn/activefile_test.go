package managedturn_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

func isErr(err, target error) bool { return errors.Is(err, target) }

func newActiveDir(t testing.TB) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "active")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	return dir
}

func dirNames(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteActiveFile_ContentIsKeyBytesWithNoNewline(t *testing.T) {
	dir := newActiveDir(t)
	k, raw := newKey(t)

	require.NoError(t, managedturn.WriteActiveFile(dir, os.Getgid(), k))

	got, err := os.ReadFile(filepath.Join(dir, activeFileName)) // #nosec G304 -- fixed file name in a test temp dir.
	require.NoError(t, err)
	assert.Equal(t, []byte(raw), got, "exactly the key bytes")
	assert.False(t, bytes.HasSuffix(got, []byte("\n")), "no trailing newline")
	assert.Len(t, got, len(raw))
}

func TestWriteActiveFile_ModeIs0440AndGroupApplied(t *testing.T) {
	dir := newActiveDir(t)
	k, _ := newKey(t)
	gid := os.Getgid()

	require.NoError(t, managedturn.WriteActiveFile(dir, gid, k))

	info, err := os.Stat(filepath.Join(dir, activeFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o440), info.Mode().Perm())
	assert.True(t, info.Mode().IsRegular())
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(gid), st.Gid, "group ownership is the requested gid") // #nosec G115 -- os.Getgid() is a non-negative uint32 kernel id.
	// The control-plane issuer rejects mode bits 0o027 (group-write, other-any).
	assert.Zero(t, info.Mode().Perm()&0o027)
}

func TestWriteActiveFile_ReplacementAndNoTempFilesLeft(t *testing.T) {
	dir := newActiveDir(t)
	k1, raw1 := newKey(t)
	k2, raw2 := newKey(t)

	require.NoError(t, managedturn.WriteActiveFile(dir, os.Getgid(), k1))
	got, err := os.ReadFile(filepath.Join(dir, activeFileName)) // #nosec G304 -- fixed file name in a test temp dir.
	require.NoError(t, err)
	require.Equal(t, []byte(raw1), got)

	require.NoError(t, managedturn.WriteActiveFile(dir, os.Getgid(), k2))
	got, err = os.ReadFile(filepath.Join(dir, activeFileName)) // #nosec G304 -- fixed file name in a test temp dir.
	require.NoError(t, err)
	assert.Equal(t, []byte(raw2), got, "second write replaces the first")

	assert.Equal(t, []string{activeFileName}, dirNames(t, dir), "no temp files left behind")
	info, err := os.Stat(filepath.Join(dir, activeFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o440), info.Mode().Perm(), "replacement keeps the mode")
}

func TestWriteActiveFile_ConcurrentReaderNeverSeesPartialContent(t *testing.T) {
	dir := newActiveDir(t)
	target := filepath.Join(dir, activeFileName)
	kA, rawA := newKey(t)
	kB, rawB := newKey(t)
	require.NoError(t, managedturn.WriteActiveFile(dir, os.Getgid(), kA))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad []string
	var mu sync.Mutex
	var reads int
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(target) // #nosec G304 -- fixed file name in a test temp dir.
			mu.Lock()
			reads++
			if err != nil {
				bad = append(bad, "read error: "+err.Error())
			} else if string(data) != rawA && string(data) != rawB {
				bad = append(bad, "partial or foreign content of length "+itoa(len(data)))
			}
			mu.Unlock()
		}
	}()

	for i := 0; i < 150; i++ {
		k := kA
		if i%2 == 0 {
			k = kB
		}
		require.NoError(t, managedturn.WriteActiveFile(dir, os.Getgid(), k))
	}
	// Give the reader a chance to observe the last state, then stop it.
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, bad, "a reader must see the old key or the new key, never a partial file or a missing file")
	assert.Positive(t, reads)
	assert.Equal(t, []string{activeFileName}, dirNames(t, dir))
}

func TestWriteActiveFile_SymlinkAtTargetIsNotFollowed(t *testing.T) {
	dir := newActiveDir(t)
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
	require.NoError(t, os.Symlink(victim, filepath.Join(dir, activeFileName)))
	k, raw := newKey(t)

	err := managedturn.WriteActiveFile(dir, os.Getgid(), k)

	got, rerr := os.ReadFile(victim) // #nosec G304 -- test-owned symlink victim in a temp dir.
	require.NoError(t, rerr)
	assert.Equal(t, "untouched", string(got), "the symlink target must never be written through")
	vinfo, serr := os.Stat(victim)
	require.NoError(t, serr)
	assert.Equal(t, os.FileMode(0o600), vinfo.Mode().Perm(), "the symlink target's mode must not change")

	linfo, lerr := os.Lstat(filepath.Join(dir, activeFileName))
	require.NoError(t, lerr)
	if err != nil {
		requireRejected(t, err, managedturn.ErrUnsafePermission)
		return
	}
	assert.True(t, linfo.Mode().IsRegular(), "if the write is allowed it must replace the link with a regular file")
	data, derr := os.ReadFile(filepath.Join(dir, activeFileName)) // #nosec G304 -- fixed file name in a test temp dir.
	require.NoError(t, derr)
	assert.Equal(t, []byte(raw), data)
}

func TestWriteActiveFile_RejectsZeroKeyAndLeavesExistingFile(t *testing.T) {
	dir := newActiveDir(t)
	k, raw := newKey(t)
	require.NoError(t, managedturn.WriteActiveFile(dir, os.Getgid(), k))

	err := managedturn.WriteActiveFile(dir, os.Getgid(), managedturn.Key{})
	requireRejected(t, err, managedturn.ErrInvalidKey)

	got, rerr := os.ReadFile(filepath.Join(dir, activeFileName)) // #nosec G304 -- fixed file name in a test temp dir.
	require.NoError(t, rerr)
	assert.Equal(t, []byte(raw), got, "a refused write leaves the proven key in place")
}

func TestWriteActiveFile_MissingDirectoryFails(t *testing.T) {
	k, _ := newKey(t)
	err := managedturn.WriteActiveFile(filepath.Join(t.TempDir(), "does-not-exist"), os.Getgid(), k)
	requireRejected(t, err)
}
