package admin

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminCtlSecurityEventLoggerUsesConfiguredMode(t *testing.T) {
	ctx := context.Background()
	require.False(t, adminCtlSecurityEventLogger("production").Enabled(ctx, slog.LevelDebug))
	require.True(t, adminCtlSecurityEventLogger("development").Enabled(ctx, slog.LevelDebug))
}
