package managedturn_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// testRealm is a fictional realm; the writer must use it exactly as given.
const testRealm = "turn.test.invalid"

// activeFileName is the fixed file name WriteActiveFile publishes inside dir.
const activeFileName = "turn-key"

// newKeyString returns a fresh random fictional key: 43 URL-safe base64
// characters (printable, no space), the shortest length ParseKey accepts.
func newKeyString(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	s := base64.RawURLEncoding.EncodeToString(b)
	require.Len(t, s, managedturn.MinKeyLen)
	return s
}

// newKey parses a fresh random fictional key and returns it with its raw text.
func newKey(t testing.TB) (managedturn.Key, string) {
	t.Helper()
	raw := newKeyString(t)
	k, err := managedturn.ParseKey([]byte(raw))
	require.NoError(t, err, "a 43-char printable key must parse")
	return k, raw
}

// requireRejected asserts err is a real refusal: non-nil, not the stub's
// ErrNotImplemented, and (when allowed is non-empty) one of the allowed
// sentinels. The ErrNotImplemented exclusion is what keeps refusal cases red
// against the stub instead of passing vacuously.
func requireRejected(t testing.TB, err error, allowed ...error) {
	t.Helper()
	require.Error(t, err)
	require.NotErrorIs(t, err, managedturn.ErrNotImplemented, "stub error is not a refusal")
	if len(allowed) == 0 {
		return
	}
	for _, a := range allowed {
		if errors.Is(err, a) {
			return
		}
	}
	require.Failf(t, "unexpected refusal", "got %v, want one of %v", err, allowed)
}

// shareableTempDir returns a temp directory the unprivileged test user can
// traverse. It is deliberately not t.TempDir: that parent is 0700, which would
// lock the read-only fixtures out of their own directory when the tests run as
// root and drop to an unprivileged user.
func shareableTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "managedturn-test-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o755)) // #nosec G302 -- traversable by the unprivileged identity the read-only tests drop to.
	t.Cleanup(func() {
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(p, 0o755) // #nosec G122 G302 -- test-owned temp tree; cleanup restores traversal so RemoveAll succeeds.
			}
			return nil
		})
		_ = os.RemoveAll(dir)
	})
	return dir
}

const nobodyID = 65534

// readOnlyEnv describes a store made read-only for the current process.
type readOnlyEnv struct {
	once    sync.Once
	restore func()
}

// Restore returns the process to its original identity and permissions. It is
// idempotent and also runs from t.Cleanup.
func (e *readOnlyEnv) Restore() { e.once.Do(e.restore) }

// makeStoreReadOnly strips write permission from the database file and its
// directory. Root bypasses permission bits, so when running as root the
// process drops to an unprivileged identity (all threads) for the duration;
// ownedPaths are chowned to that identity so the rest of the coordinator's
// files stay usable. Call after the store handle is closed. Returns a restore
// that the caller invokes before asserting on files.
func makeStoreReadOnly(t testing.TB, dbPath string, ownedPaths ...string) *readOnlyEnv {
	t.Helper()
	dir := filepath.Dir(dbPath)
	require.NoError(t, os.Chmod(dbPath, 0o444)) // #nosec G302 -- read-only, never writable: the fixture under test.
	require.NoError(t, os.Chmod(dir, 0o555))    // #nosec G302 -- read-only fixture directory, never writable.

	env := &readOnlyEnv{}
	undo := []func(){func() {
		_ = os.Chmod(dir, 0o755)    // #nosec G302 -- restores the fixture for cleanup.
		_ = os.Chmod(dbPath, 0o644) // #nosec G302 -- restores the fixture for cleanup.
	}}

	if os.Geteuid() == 0 {
		for _, p := range ownedPaths {
			require.NoError(t, filepath.Walk(p, func(path string, _ os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				return os.Lchown(path, nobodyID, nobodyID) // #nosec G122 -- test-owned temp tree; Lchown never follows a symlink.
			}))
		}
		undo = append([]func(){dropPrivileges(t)}, undo...)
	}

	env.restore = func() {
		for _, u := range undo {
			u()
		}
	}
	t.Cleanup(env.Restore)

	// Prove the environment really is read-only before relying on it; a test
	// that silently runs with write access would pass for the wrong reason.
	if f, err := os.OpenFile(dbPath, os.O_WRONLY, 0); err == nil { // #nosec G304 -- test fixture path.
		_ = f.Close()
		env.Restore()
		t.Skip("permission bits are not enforced in this environment; cannot make the database read-only")
	}
	return env
}

// openInitStore opens a store at path, creates the schema and registers Close.
func openInitStore(t testing.TB, path string) *managedturn.Store {
	t.Helper()
	ctx := context.Background()
	s, err := managedturn.OpenStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema(ctx))
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }
