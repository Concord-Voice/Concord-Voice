package testdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModuleDir_FindsModuleRootFromPackageDir(t *testing.T) {
	dir, err := ModuleDir("migrations")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(dir, filepath.Join("services", "control-plane", "migrations")), dir)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, moduleMigrationsDir, dir, "the init-time value must match")
}

func TestModuleDir_NoGoModAboveFails(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := ModuleDir("migrations")
	require.Error(t, err)
}
