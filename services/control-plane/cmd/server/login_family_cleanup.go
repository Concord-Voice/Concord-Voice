package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Counts include confirmed commits only. A failed or exhausted pass must never
// claim completion, including when it committed earlier units before stopping.
func logLoginFamilyCleanup(ctx context.Context, log *logger.Logger, result loginfamily.GCResult, err error) {
	ctxErr := ctx.Err()
	incomplete := result.Incomplete || err != nil || ctxErr != nil
	fields := []any{
		"deleted_sids", result.DeletedSIDs,
		"deleted_families", result.DeletedFamilies,
		"deleted_operations", result.DeletedOperations,
		"deleted_connection_closures", result.DeletedConnectionClosures,
		"deleted_room_closures", result.DeletedRoomClosures,
		"incomplete", incomplete,
		"failed", err != nil || ctxErr != nil,
	}
	level := slog.LevelInfo
	if incomplete {
		level = slog.LevelWarn
	}
	if err != nil || ctxErr != nil {
		level = slog.LevelError
		failureClass := "sweep_failed"
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded):
			failureClass = "deadline_exceeded"
		case errors.Is(err, context.Canceled) || errors.Is(ctxErr, context.Canceled):
			failureClass = "canceled"
		}
		fields = append(fields, "failure_class", failureClass)
	}
	log.Log(ctx, level, "Cleanup: login family evidence sweep finished", fields...)
}
