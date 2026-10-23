package managedturn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A deleted key must not survive in the file's free pages, where coturn's
// group could still read it (secure_delete).
func TestDeleteRows_ScrubsTheDeletedKeyFromTheFile(t *testing.T) {
	ctx := context.Background()
	buf := make([]byte, 48)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	raw := base64.RawURLEncoding.EncodeToString(buf)
	key, err := ParseKey([]byte(raw))
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "turndb")
	s, err := OpenStore(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.InitSchema(ctx))
	require.NoError(t, s.Insert(ctx, "r", key))
	n, err := s.deleteRows(ctx, "r", func(string) bool { return true })
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, s.Close())

	data, err := os.ReadFile(path) // #nosec G304 -- test temp file
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, []byte(raw)), "deleted key bytes remain in the database file")
}
