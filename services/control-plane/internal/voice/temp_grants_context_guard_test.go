package voice

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A confirmed database commit keeps best-effort delivery detached from request
// cancellation without extending an existing whole-attempt deadline.
func TestCompleteTemporaryGrantRevocationDetachesConfirmedPostCommitContext(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "temp_grants.go")) // #nosec G304 -- fixed sibling source
	require.NoError(t, err)

	start := strings.Index(string(source), "func (m *tempGrantManager) completeTemporaryGrantRevocation(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(string(source[start:]), "// deleteTemporaryGrantWithCapture")
	require.Greater(t, end, 0)
	completion := string(source[start : start+end])
	require.Contains(t, completion, "detachedTempGrantContext(ctx)")
	require.Contains(t, completion, "m.resolver.InvalidateChannel(safeCtx")
	require.Contains(t, completion, "m.rotator.BroadcastContext(safeCtx")
	require.Contains(t, completion, "m.hub.BroadcastToUserContext(safeCtx")
}

func TestDetachedTempGrantContextPreservesEarlierDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelParent()
	parentDeadline, ok := parent.Deadline()
	require.True(t, ok)

	detached, cancelDetached := detachedTempGrantContext(parent)
	defer cancelDetached()
	detachedDeadline, ok := detached.Deadline()
	require.True(t, ok)
	require.Equal(t, parentDeadline, detachedDeadline)
}

func TestDetachedTempGrantContextIgnoresCancellationWithinBound(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	detached, cancelDetached := detachedTempGrantContext(parent)
	defer cancelDetached()
	require.NoError(t, detached.Err())
	deadline, ok := detached.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(tempGrantEffectTimeout), deadline, time.Second)
}
