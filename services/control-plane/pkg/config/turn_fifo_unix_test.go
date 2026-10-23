//go:build unix

package config

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A FIFO with no writer must be refused promptly, not block the admission.
func TestICEServersCheckedManagedFileRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "active-turn-key")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	link := filepath.Join(dir, "linked-key")
	require.NoError(t, syscall.Symlink(fifo, link))

	for path, want := range map[string]string{fifo: "invalid metadata", link: "symlink"} {
		cfg := &Config{TURNServerHost: turnHost, TURNSecretFile: path}
		done := make(chan error, 1)
		go func() {
			servers, err := cfg.ICEServersChecked(turnUser1)
			assert.Empty(t, servers)
			done <- err
		}()
		select {
		case err := <-done:
			require.ErrorContains(t, err, want)
		case <-time.After(2 * time.Second):
			t.Fatalf("ICEServersChecked blocked on FIFO %s", path)
		}
	}
}
