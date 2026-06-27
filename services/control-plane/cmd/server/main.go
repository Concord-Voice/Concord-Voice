// Package main is the entry point for the Control Plane server.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/api"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/database"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/storage"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	logKeyCount    = "count"
	logKeyServerID = "server_id"
)

func main() {
	// Admin CLI subcommand dispatch (#1688): `control-plane admin <verb>` runs the
	// adminctl provisioning tooling (bootstrap / enroll / reset-enrollment) instead
	// of booting the server. Invoked out-of-band via `docker exec` for first-admin
	// provisioning and break-glass recovery — see the admin-auth design spec.
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(admin.RunAdminCtl(os.Args[2:]))
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize logger
	log := logger.New(cfg.Environment)

	// Initialize database
	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		log.Fatal("Failed to connect to database", "error", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("Error closing database", "error", err)
		}
	}()

	// Run migrations
	if err := database.RunMigrations(db); err != nil {
		log.Fatal("Failed to run migrations", "error", err)
	}

	// Initialize Redis
	redisClient, err := database.NewRedisClient(cfg.RedisURL)
	if err != nil {
		log.Fatal("Failed to connect to Redis", "error", err)
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Error("Error closing Redis client", "error", err)
		}
	}()

	// Repopulate the user-disabled denylist from the DB source of truth (#1623):
	// closes the window after a Redis flush where the immediate-effect mid-session
	// gate would otherwise miss already-disabled accounts until their next
	// login/refresh. Non-fatal — the login/refresh DB gates still hold if it errors.
	if rebuildErr := middleware.RebuildDisabledDenylist(context.Background(), db, redisClient); rebuildErr != nil {
		log.Error("Failed to rebuild user-disabled denylist", "error", rebuildErr)
	}

	storageClient := initStorageClient(cfg, log)

	// Set Gin mode
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Initialize hot-reloadable SPA config (reads mounted spa.env when SPA_CONFIG_FILE is set)
	liveSpa := config.NewLiveSpaConfig(cfg, cfg.SpaConfigFile, 30*time.Second)

	// Initialize router (starts hub, connects NATS, subscribes to voice events)
	router, hub, natsClient := api.NewRouter(db, redisClient, storageClient, cfg, liveSpa, log)

	// Start background cleanup job (reaps expired tokens, stale sessions, orphaned presence)
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	go runCleanupJob(cleanupCtx, db, redisClient, hub, log)

	pendingRepo := auth.NewPendingRepo(db)
	auth.StartPendingCleanupWorker(cleanupCtx, pendingRepo, log, auth.PendingCleanupInterval)

	// Start attestation registry retention pruner (#677, ADR-0010 D9).
	// Periodically prunes release_binaries (keeps current MAJOR.MINOR + last
	// two patches of prior MINOR) and release_spas (60-day window) so the
	// registry doesn't grow unbounded. Interval is configurable via
	// ATTESTATION_PRUNE_INTERVAL (default 6h, range 1h-24h).
	attestRepo := attestation.NewRepository(db)
	attestCleanup := attestation.NewCleanup(attestRepo, log)
	go attestCleanup.Run(cleanupCtx, cfg.AttestationPruneInterval)

	// Start the temporary-SBAC orphan-sweep backstop (#487 D3). The presence-bound
	// grant lifetime is primarily enforced by the voice.left + heartbeat triggers;
	// this daily sweep revokes any temp grant whose holder is no longer in the
	// channel but whose override survived a missed trigger (e.g., a restart between
	// leave and revoke). Its resolver shares the same Redis-backed permission cache.
	sweepResolver := rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), log)
	voice.StartTempGrantSweepWorker(cleanupCtx, db, log, hub, sweepResolver, natsClient, voice.DefaultTempGrantSweepInterval)

	// Create HTTP server
	srv := &http.Server{
		Addr:           fmt.Sprintf(":%s", cfg.Port),
		Handler:        router,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// Start server in a goroutine
	go func() {
		log.Info("Starting Control Plane server", "port", cfg.Port, "env", cfg.Environment)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("Server failed to start", "error", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down server...")

	// Stop background cleanup job
	cleanupCancel()

	// Stop SPA config file watcher
	liveSpa.Stop()

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shut down WebSocket hub (close all client connections)
	hub.Shutdown()

	// Close NATS connection
	if natsClient != nil {
		natsClient.Close()
	}

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown", "error", err)
	}

	log.Info("Server exited")
}

func initStorageClient(cfg *config.Config, log *logger.Logger) *storage.Client {
	if cfg.StorageEndpoint == "" {
		log.Info("Object storage not configured (STORAGE_ENDPOINT/MINIO_ENDPOINT empty) — media endpoints disabled")
		return nil
	}

	const maxRetries = 5
	var client *storage.Client
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		client, err = storage.New(cfg, log)
		if err == nil {
			return client
		}
		if attempt < maxRetries {
			backoff := time.Duration(attempt) * 2 * time.Second
			log.Warn("Object storage not ready, retrying", "error", err, "attempt", attempt, "backoff", backoff)
			time.Sleep(backoff)
			continue
		}
		if cfg.Environment == "production" {
			log.Fatal("Failed to connect to object storage after retries", "error", err, "attempts", maxRetries)
		}
		log.Warn("Object storage unavailable — media endpoints will return 503", "error", err)
	}
	return nil
}

// runCleanupJob periodically purges expired tokens, stale sessions, and orphaned
// Redis presence keys. The server must not rely on clients to clean up after
// themselves — this job is the authoritative backstop.
func runCleanupJob(ctx context.Context, db *sql.DB, redisClient *redis.Client, hub *websocket.Hub, log *logger.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Run once at startup to catch anything that accumulated while the server was down
	runCleanup(ctx, db, redisClient, hub, log)

	for {
		select {
		case <-ticker.C:
			runCleanup(ctx, db, redisClient, hub, log)
		case <-ctx.Done():
			log.Info("Cleanup job stopped")
			return
		}
	}
}

func runCleanup(ctx context.Context, db *sql.DB, redisClient *redis.Client, hub *websocket.Hub, log *logger.Logger) {
	taskCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Task 1: Purge naturally expired refresh tokens that were never cleaned up
	// (e.g., user never refreshed, so the per-user opportunistic cleanup never ran)
	res1, err := db.ExecContext(taskCtx,
		`DELETE FROM refresh_tokens WHERE expires_at < NOW() AND revoked_at IS NULL`)
	if err != nil {
		log.Error("Cleanup: failed to purge expired tokens", "error", err)
	} else if n, raErr := res1.RowsAffected(); raErr != nil {
		log.Error("Cleanup: RowsAffected error", "task", "purge_expired_tokens", "error", raErr)
	} else if n > 0 {
		log.Info("Cleanup: purged expired tokens", logKeyCount, n)
	}

	// Task 2: Purge revoked sessions older than 90 days
	// (global version of the per-user cleanup that runs during token refresh)
	res2, err := db.ExecContext(taskCtx,
		`DELETE FROM refresh_tokens WHERE revoked_at IS NOT NULL AND revoked_at < NOW() - INTERVAL '90 days'`)
	if err != nil {
		log.Error("Cleanup: failed to purge old revoked sessions", "error", err)
	} else if n, raErr := res2.RowsAffected(); raErr != nil {
		log.Error("Cleanup: RowsAffected error", "task", "purge_revoked_sessions", "error", raErr)
	} else if n > 0 {
		log.Info("Cleanup: purged old revoked sessions", logKeyCount, n)
	}

	// Task 3: Clean stale Redis presence keys
	cleanupStalePresence(taskCtx, redisClient, hub, log)

	// Task 4: Auto-complete expired ownership transfers
	cleanupExpiredTransfers(taskCtx, db, redisClient, hub, log)

	log.Debug("Cleanup completed")
}

// cleanupStalePresence compares presence:* keys against the authoritative set
// of connected users from the hub and removes stale entries.
func cleanupStalePresence(ctx context.Context, redisClient *redis.Client, hub *websocket.Hub, log *logger.Logger) {
	connectedUsers := hub.GetConnectedUsers()
	var staleCount int
	var cursor uint64
	for {
		keys, nextCursor, err := redisClient.Scan(ctx, cursor, "presence:*", 100).Result()
		if err != nil {
			log.Error("Cleanup: failed to scan presence keys", "error", err)
			break
		}
		for _, key := range keys {
			uidStr := strings.TrimPrefix(key, "presence:")
			uid, parseErr := uuid.Parse(uidStr)
			if parseErr != nil {
				redisClient.Del(ctx, key)
				staleCount++
				continue
			}
			if !connectedUsers[uid] {
				redisClient.Del(ctx, key)
				staleCount++
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	if staleCount > 0 {
		log.Info("Cleanup: removed stale presence keys", logKeyCount, staleCount)
	}
}

// cleanupExpiredTransfers finds pending ownership transfers past their 24h window
// and completes them atomically (owner_id + server_members.role swap).
func cleanupExpiredTransfers(ctx context.Context, db *sql.DB, redisClient *redis.Client, hub *websocket.Hub, log *logger.Logger) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, server_id, from_user_id, to_user_id
		FROM ownership_transfers
		WHERE status = 'pending' AND expires_at <= NOW()
	`)
	if err != nil {
		log.Error("Cleanup: failed to query expired transfers", "error", err)
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var xfer pendingTransfer
		if err := rows.Scan(&xfer.id, &xfer.serverID, &xfer.fromUserID, &xfer.toUserID); err != nil {
			log.Error("Cleanup: failed to scan expired transfer", "error", err)
			continue
		}
		if err := completeOwnershipTransfer(ctx, db, redisClient, hub, xfer); err != nil {
			log.Error("Cleanup: failed to auto-complete transfer", "error", err,
				"transfer_id", xfer.id, logKeyServerID, xfer.serverID)
		} else {
			log.Info("Cleanup: auto-completed ownership transfer",
				"transfer_id", xfer.id, logKeyServerID, xfer.serverID,
				"from_user_id", xfer.fromUserID, "to_user_id", xfer.toUserID)
		}
	}
	if err := rows.Err(); err != nil {
		log.Error("Cleanup: error during expired transfers iteration", "error", err)
	}
}

// pendingTransfer holds the fields needed to complete an expired transfer.
type pendingTransfer struct {
	id, serverID, fromUserID, toUserID string
}

// completeOwnershipTransfer atomically transfers server ownership and invalidates caches.
func completeOwnershipTransfer(ctx context.Context, db *sql.DB, redisClient *redis.Client, hub *websocket.Hub, xfer pendingTransfer) error {
	completed, err := executeTransferTx(ctx, db, xfer)
	if err != nil {
		return err
	}
	if !completed {
		return nil
	}

	for _, uid := range []string{xfer.fromUserID, xfer.toUserID} {
		if err := invalidatePermissionCache(ctx, redisClient, xfer.serverID, uid); err != nil {
			return err
		}
	}

	broadcastOwnershipChange(hub, xfer)

	return nil
}

func executeTransferTx(ctx context.Context, db *sql.DB, xfer pendingTransfer) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE ownership_transfers SET status = 'completed', completed_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, xfer.id)
	if err != nil {
		return false, fmt.Errorf("mark transfer completed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE servers SET owner_id = $1 WHERE id = $2`, xfer.toUserID, xfer.serverID); err != nil {
		return false, fmt.Errorf("update server owner: %w", err)
	}
	if err := swapMemberRoles(ctx, tx, xfer); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

func swapMemberRoles(ctx context.Context, tx *sql.Tx, xfer pendingTransfer) error {
	resFrom, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'member' WHERE server_id = $1 AND user_id = $2`, xfer.serverID, xfer.fromUserID)
	if err != nil {
		return fmt.Errorf("update from_user role: %w", err)
	}
	if n, _ := resFrom.RowsAffected(); n == 0 {
		return fmt.Errorf("from_user %s is no longer a member", xfer.fromUserID)
	}
	resTo, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, xfer.serverID, xfer.toUserID)
	if err != nil {
		return fmt.Errorf("update to_user role: %w", err)
	}
	if n, _ := resTo.RowsAffected(); n == 0 {
		return fmt.Errorf("to_user %s is no longer a member", xfer.toUserID)
	}
	return nil
}

func invalidatePermissionCache(ctx context.Context, redisClient *redis.Client, serverID, userID string) error {
	pattern := fmt.Sprintf("perm:%s:%s*", serverID, userID)
	iter := redisClient.Scan(ctx, 0, pattern, 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("scan permission cache keys for user %s: %w", userID, err)
	}
	if len(keys) > 0 {
		if err := redisClient.Unlink(ctx, keys...).Err(); err != nil {
			return fmt.Errorf("unlink permission cache keys for user %s: %w", userID, err)
		}
	}
	return nil
}

func broadcastOwnershipChange(hub *websocket.Hub, xfer pendingTransfer) {
	serverUUID, err := uuid.Parse(xfer.serverID)
	if err != nil {
		return
	}
	hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "ownership_transferred",
		Data: map[string]interface{}{
			logKeyServerID: xfer.serverID,
			"old_owner_id": xfer.fromUserID,
			"new_owner_id": xfer.toUserID,
		},
	})
}
