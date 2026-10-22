package managedredis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestActivationMarkerFailsClosedForInvalidMarker(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "wrong contents",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", len(activationMarkerContents))), 0o600))
			},
		},
		{
			name: "wrong permissions",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte(activationMarkerContents), 0o644)) // #nosec G306 -- deliberately invalid permissions in a temporary marker fixture.
			},
		},
		{
			name: "directory in place of marker",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Mkdir(path, 0o700))
			},
		},
		{
			name: "symlink in place of marker",
			setup: func(t *testing.T, path string) {
				target := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.WriteFile(target, []byte(activationMarkerContents), 0o600))
				require.NoError(t, os.Symlink(target, path))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, aclPath := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
			path := filepath.Join(filepath.Dir(aclPath), "marker", "activated")
			tt.setup(t, path)

			exists, err := controller.activationMarker()
			require.Error(t, err)
			require.False(t, exists)
			require.Error(t, controller.requireActivationMarker())
		})
	}
}

func TestActivationMarkerMissingRequiresReviewedRecovery(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})

	exists, err := controller.activationMarker()
	require.NoError(t, err)
	require.False(t, exists)
	require.Error(t, controller.requireActivationMarker())
}

func TestActivationMarkerRejectsDirectoryWithSharedPermissions(t *testing.T) {
	controller, aclPath := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	markerDir := filepath.Join(filepath.Dir(aclPath), "marker")
	require.NoError(t, os.Chmod(markerDir, 0o750)) // #nosec G302 -- deliberate unsafe permissions in a temporary fixture.

	exists, err := controller.activationMarker()
	require.Error(t, err)
	require.False(t, exists)
	require.Error(t, controller.createActivationMarker())
}
