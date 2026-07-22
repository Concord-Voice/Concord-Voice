// Package main is the entry point for the Control Plane server.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	logKeyCount                   = "count"
	logKeyServerID                = "server_id"
	activityHistoryStartupBatch   = 100
	activityHistoryStartupTimeout = 30 * time.Second
)

type controlPlaneSubcommandRunners struct {
	admin           func([]string) int
	activityHistory func([]string) int
}

func dispatchControlPlaneSubcommand(args []string) (int, bool) {
	return dispatchControlPlaneSubcommandWithRunners(args, controlPlaneSubcommandRunners{
		admin:           admin.RunAdminCtl,
		activityHistory: presencehistory.RunAdminCtl,
	})
}

func dispatchControlPlaneSubcommandWithRunners(
	args []string,
	runners controlPlaneSubcommandRunners,
) (int, bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case "admin":
		return runners.admin(args[2:]), true
	case "activity-history":
		return runners.activityHistory(args[2:]), true
	default:
		return 0, false
	}
}

func main() {
	// Maintenance subcommands are dispatched before ordinary configuration or
	// serving dependencies are initialized.
	if code, handled := dispatchControlPlaneSubcommand(os.Args); handled {
		os.Exit(code)
	}
	if err := runControlPlane(); err != nil {
		log.Printf("Control Plane failed: %v", err)
		os.Exit(1)
	}
}

func runControlPlane() (runErr error) {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// Initialize logger
	log := logger.New(cfg.Environment)

	// Initialize database
	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer closeDatabase(db, log)

	// Run migrations
	if err := database.RunMigrations(db); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	presenceHistoryService := presencehistory.NewService(
		db,
		buildActivityHistoryDisclosure(cfg),
		cfg.ActivityHistoryClusterEnabled,
	)

	// Revoke any credential left by an unclean prior exit before initializing
	// dependencies that may still terminate the process during construction.
	readerSetupCtx, readerSetupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = database.EnsureOpsMetricsReaderLoginDisabled(readerSetupCtx, db)
	readerSetupCancel()
	if err != nil {
		return fmt.Errorf("reconcile restricted admin operations metrics reader: %w", err)
	}

	// Initialize Redis
	redisClient, err := database.NewRedisClient(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("connect to Redis: %w", err)
	}
	defer closeRedisClient(redisClient, log)

	// Repopulate the user-disabled denylist from the DB source of truth (#1623):
	// closes the window after a Redis flush where the immediate-effect mid-session
	// gate would otherwise miss already-disabled accounts until their next
	// login/refresh. Non-fatal — the login/refresh DB gates still hold if it errors.
	if rebuildErr := middleware.RebuildDisabledDenylist(context.Background(), db, redisClient); rebuildErr != nil {
		log.Error("Failed to rebuild user-disabled denylist", "error", rebuildErr)
	}

	storageClient, err := initStorageClient(cfg, log)
	if err != nil {
		return err
	}

	// Set Gin mode
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Initialize hot-reloadable SPA config (reads mounted spa.env when SPA_CONFIG_FILE is set)
	liveSpa := config.NewLiveSpaConfig(cfg, cfg.SpaConfigFile, 30*time.Second)

	// Start background cleanup job (reaps expired tokens, stale sessions, orphaned presence)
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	retentionWorker := presencehistory.NewRetentionWorker(db, log)
	var deferredAdminMetricsReader *deferredAdminOpsMetricsReader
	var adminMetricsRouterReader opsmetrics.Reader
	if cfg.AdminConsoleEnabled && cfg.OpsMetrics.Enabled {
		deferredAdminMetricsReader = &deferredAdminOpsMetricsReader{}
		adminMetricsRouterReader = deferredAdminMetricsReader
	}
	var opsMetricsRuntime *api.OpsMetricsRuntime
	var voicePermissionEnforcer *voice.PermissionEnforcer
	runtime, startupErr := startActivityHistoryRuntime(activityHistoryRuntimeDependencies{
		startupContext:      context.Background(),
		workerContext:       cleanupCtx,
		reconcileDisclosure: presenceHistoryService.ReconcileStaleDisclosure,
		bindRouter: func() (*gin.Engine, *websocket.Hub, *natsclient.Client, error) {
			router, hub, natsClient, metricsRuntime, permissionEnforcer, routerErr := api.NewRouter(
				db,
				redisClient,
				storageClient,
				cfg,
				liveSpa,
				log,
				api.RouterDependencies{
					OpsMetricsReader: adminMetricsRouterReader,
					PresenceHistory:  presenceHistoryService,
				},
			)
			if routerErr != nil {
				return nil, nil, nil, routerErr
			}
			opsMetricsRuntime = metricsRuntime
			voicePermissionEnforcer = permissionEnforcer
			return router, hub, natsClient, nil
		},
		reconcilePending: presenceHistoryService.ReconcilePending,
		pendingWorker:    presenceHistoryService.RunPendingReconciler,
		retentionWorker:  retentionWorker.Run,
	})
	if startupErr != nil {
		return fmt.Errorf("initialize Activity History runtime: %w", startupErr)
	}
	log.Info("Activity History startup reconciliation complete", logKeyCount, runtime.paused)
	router := runtime.router
	hub := runtime.hub
	natsClient := runtime.natsClient
	waitActivityHistoryWorkers := runtime.waitWorkers
	waitBackgroundWorkers := func() {
		waitActivityHistoryWorkers()
		if voicePermissionEnforcer != nil {
			voicePermissionEnforcer.Close()
		}
	}

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

	// Start the subscription-expiry sweeper (#2158). Fixed-duration code grants
	// (Kickstarter, Beta) lapse purely by the clock; without this the passive
	// expiry never fires OnTierChange, so a client keeps premium UI affordances
	// until it reconnects while the server already rejects them. The sweep flips
	// each lapsed subscription to 'expired' and runs the SAME OnTierChange
	// convergence point (cache invalidate + entitlements_changed push + downgrade
	// disconnect) the redemption grant uses. Its cache + WS notifier are built
	// fresh here, matching the temp-grant sweeper's own-deps pattern above (the
	// entitlement Cache is Redis-backed and stateless, so a second instance is
	// equivalent to the router's).
	expiryEntCache := entitlements.NewCacheForInstance(redisClient, db, cfg.InstanceType)
	expiryNotifier := api.NewEntitlementNotifier(hub, log)
	subscriptions.StartExpirySweepWorker(cleanupCtx, db, log, expiryEntCache, expiryNotifier, subscriptions.DefaultExpirySweepInterval)

	// Create HTTP server
	srv := &http.Server{
		Addr:           fmt.Sprintf(":%s", cfg.Port),
		Handler:        router,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	var adminMetricsReaderRuntime *adminOpsMetricsReaderRuntime
	cleanupStarted := false
	cleanupRuntime := func() error {
		cleanupStarted = true
		log.Info("Shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Stop accepting and drain HTTP handlers before closing the dependencies
		// they may still use. net/http does not wait for hijacked WebSocket
		// connections; hub.Shutdown closes those after ordinary handlers finish.
		return shutdownControlPlane(
			func() {
				cleanupCancel()
				liveSpa.Stop()
			},
			func() error { return srv.Shutdown(ctx) },
			waitBackgroundWorkers,
			func() { hub.Shutdown() },
			func() error { return opsMetricsRuntime.Stop(ctx) },
			func() error {
				readerCleanupCtx, readerCleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer readerCleanupCancel()
				return adminMetricsReaderRuntime.Stop(readerCleanupCtx)
			},
			func() {
				if natsClient != nil {
					natsClient.Close()
				}
			},
		)
	}
	defer func() {
		if !cleanupStarted {
			runErr = errors.Join(runErr, cleanupRuntime())
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	// Every constructor that can still fatal-exit has completed. Only now rotate
	// the process-ephemeral reader credential, bind it into the already-built
	// admin route surface, and make the listener reachable.
	readerSetupCtx, readerSetupCancel = context.WithTimeout(context.Background(), 15*time.Second)
	adminMetricsReaderRuntime, err = configureAdminOpsMetricsReader(
		readerSetupCtx,
		cfg,
		db,
		openAdminOpsMetricsReader,
		database.EnsureOpsMetricsReaderLoginDisabled,
	)
	readerSetupCancel()
	if err != nil {
		return fmt.Errorf("configure restricted admin operations metrics reader: %w", err)
	}
	defer func() {
		readerCleanupCtx, readerCleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer readerCleanupCancel()
		if cleanupErr := adminMetricsReaderRuntime.Stop(readerCleanupCtx); cleanupErr != nil {
			log.Error("Failed to clean up restricted admin operations metrics reader", "error", cleanupErr)
		}
	}()
	if deferredAdminMetricsReader != nil {
		if err := deferredAdminMetricsReader.Bind(adminMetricsReaderRuntime.Reader()); err != nil {
			return fmt.Errorf("bind restricted admin operations metrics reader: %w", err)
		}
	}

	log.Info("Starting Control Plane server", "port", cfg.Port, "env", cfg.Environment)
	if err := runControlPlaneServer(func() error { return srv.ListenAndServe() }, quit, cleanupRuntime); err != nil {
		return err
	}
	log.Info("Server exited")
	return nil
}

func runControlPlaneServer(
	serve func() error,
	stop <-chan os.Signal,
	shutdown func() error,
) (runErr error) {
	if serve == nil || stop == nil || shutdown == nil {
		return errors.New("control plane server lifecycle is incomplete")
	}
	defer func() {
		runErr = errors.Join(runErr, shutdown())
	}()

	serveResult := make(chan error, 1)
	go func() { serveResult <- serve() }()
	select {
	case <-stop:
		return nil
	case err := <-serveResult:
		if err == nil {
			return errors.New("control plane server stopped unexpectedly")
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve control plane: %w", err)
	}
}

func shutdownControlPlane(
	stopBackground func(),
	shutdownHTTP func() error,
	waitActivityWorkers, shutdownHub func(),
	shutdownMetrics func() error,
	shutdownAdminMetricsReader func() error,
	closeNATS func(),
) error {
	stopBackground()
	shutdownErr := shutdownHTTP()
	waitActivityWorkers()
	shutdownHub()
	shutdownErr = errors.Join(shutdownErr, shutdownMetrics())
	shutdownErr = errors.Join(shutdownErr, shutdownAdminMetricsReader())
	closeNATS()
	return shutdownErr
}

type activityHistoryRuntimeDependencies struct {
	startupContext      context.Context
	workerContext       context.Context
	reconcileDisclosure func(context.Context) (int64, error)
	bindRouter          func() (*gin.Engine, *websocket.Hub, *natsclient.Client, error)
	reconcilePending    func(context.Context, int) (presencehistory.ReconcileStats, error)
	pendingWorker       func(context.Context)
	retentionWorker     func(context.Context)
}

type activityHistoryRuntime struct {
	router      *gin.Engine
	hub         *websocket.Hub
	natsClient  *natsclient.Client
	waitWorkers func()
	paused      int64
}

func startActivityHistoryRuntime(
	dependencies activityHistoryRuntimeDependencies,
) (activityHistoryRuntime, error) {
	if dependencies.startupContext == nil || dependencies.workerContext == nil ||
		dependencies.reconcileDisclosure == nil || dependencies.bindRouter == nil ||
		dependencies.reconcilePending == nil || dependencies.pendingWorker == nil ||
		dependencies.retentionWorker == nil {
		return activityHistoryRuntime{}, errors.New("activity history runtime dependency unavailable")
	}

	runtime := activityHistoryRuntime{waitWorkers: func() {}}
	paused, err := initializeActivityHistoryRuntime(
		dependencies.startupContext,
		activityHistoryStartupSteps{
			reconcileDisclosure: dependencies.reconcileDisclosure,
			bindRuntime: func() error {
				var bindErr error
				runtime.router, runtime.hub, runtime.natsClient, bindErr = dependencies.bindRouter()
				return bindErr
			},
			reconcilePending: dependencies.reconcilePending,
			startWorkers: func() {
				runtime.waitWorkers = startActivityHistoryWorkers(
					dependencies.workerContext,
					dependencies.pendingWorker,
					dependencies.retentionWorker,
				)
			},
		},
	)
	if err != nil {
		return activityHistoryRuntime{}, err
	}
	runtime.paused = paused
	return runtime, nil
}

type activityHistoryStartupSteps struct {
	reconcileDisclosure func(context.Context) (int64, error)
	bindRuntime         func() error
	reconcilePending    func(context.Context, int) (presencehistory.ReconcileStats, error)
	startWorkers        func()
}

func initializeActivityHistoryRuntime(
	ctx context.Context,
	steps activityHistoryStartupSteps,
) (int64, error) {
	if steps.reconcileDisclosure == nil || steps.bindRuntime == nil ||
		steps.reconcilePending == nil || steps.startWorkers == nil {
		return 0, errors.New("activity history startup dependency unavailable")
	}
	startupCtx, cancel := context.WithTimeout(ctx, activityHistoryStartupTimeout)
	defer cancel()

	paused, err := steps.reconcileDisclosure(startupCtx)
	if err != nil {
		return 0, fmt.Errorf("reconcile activity history disclosure: %w", err)
	}
	if err := steps.bindRuntime(); err != nil {
		return 0, fmt.Errorf("bind activity history runtime: %w", err)
	}
	if _, err := steps.reconcilePending(startupCtx, activityHistoryStartupBatch); err != nil {
		return 0, fmt.Errorf("reconcile pending activity history operations: %w", err)
	}
	steps.startWorkers()
	return paused, nil
}

func buildActivityHistoryDisclosure(cfg *config.Config) presencehistory.DisclosureState {
	if cfg == nil {
		return presencehistory.DisclosureState{}
	}
	development := cfg.Environment == "development" || cfg.Environment == "test"
	return presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{
		InstanceType:     cfg.InstanceType,
		OperatorName:     cfg.ActivityHistoryOperatorName,
		PrivacyPolicyURL: cfg.ActivityHistoryPrivacyPolicyURL,
		Development:      development,
	})
}

func startActivityHistoryWorkers(
	ctx context.Context,
	pendingWorker func(context.Context),
	retentionWorker func(context.Context),
) func() {
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		pendingWorker(ctx)
	}()
	go func() {
		defer workers.Done()
		retentionWorker(ctx)
	}()
	return workers.Wait
}

func closeDatabase(db *sql.DB, log *logger.Logger) {
	if err := db.Close(); err != nil {
		log.Error("Error closing database", "error", err)
	}
}

func closeRedisClient(redisClient *redis.Client, log *logger.Logger) {
	if err := redisClient.Close(); err != nil {
		log.Error("Error closing Redis client", "error", err)
	}
}

func initStorageClient(cfg *config.Config, log *logger.Logger) (*storage.Client, error) {
	if cfg.StorageEndpoint == "" {
		log.Info("Object storage not configured (STORAGE_ENDPOINT/MINIO_ENDPOINT empty) — media endpoints disabled")
		return nil, nil
	}

	const maxRetries = 5
	var client *storage.Client
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		client, err = storage.New(cfg, log)
		if err == nil {
			return client, nil
		}
		if attempt < maxRetries {
			backoff := time.Duration(attempt) * 2 * time.Second
			log.Warn("Object storage not ready, retrying", "error", err, "attempt", attempt, "backoff", backoff)
			time.Sleep(backoff)
			continue
		}
		if cfg.Environment == "production" {
			return nil, fmt.Errorf("connect to object storage after %d attempts: %w", maxRetries, err)
		}
		log.Warn("Object storage unavailable — media endpoints will return 503", "error", err)
	}
	return nil, nil
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

type stalePresenceStore interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// cleanupStalePresence compares presence:* keys against the authoritative set
// of connected users from the hub and removes stale entries.
func cleanupStalePresence(ctx context.Context, redisClient stalePresenceStore, hub *websocket.Hub, log *logger.Logger) {
	connectedUsers := hub.GetConnectedUsers()
	var staleCount int
	var cursor uint64
	deleteStaleKey := func(key string) {
		if err := redisClient.Del(ctx, key).Err(); err != nil {
			log.Error("Cleanup: failed to delete stale presence key", "error", err)
			return
		}
		staleCount++
	}
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
				deleteStaleKey(key)
				continue
			}
			if !connectedUsers[uid] {
				deleteStaleKey(key)
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
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Error("Cleanup: failed to close expired transfer rows", "error", closeErr)
		}
	}()

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

func executeTransferTx(ctx context.Context, db *sql.DB, xfer pendingTransfer) (completed bool, retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		retErr = joinCleanupError("rollback transfer transaction", retErr, rollbackErr)
	}()

	res, err := tx.ExecContext(ctx, `
		UPDATE ownership_transfers SET status = 'completed', completed_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, xfer.id)
	if err != nil {
		return false, fmt.Errorf("mark transfer completed: %w", err)
	}
	n, err := checkedRowsAffected(res, "mark transfer completed")
	if err != nil {
		return false, err
	}
	if n == 0 {
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
	n, err := checkedRowsAffected(resFrom, "read from_user role update result")
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("from_user %s is no longer a member", xfer.fromUserID)
	}
	resTo, err := tx.ExecContext(ctx, `UPDATE server_members SET role = 'owner' WHERE server_id = $1 AND user_id = $2`, xfer.serverID, xfer.toUserID)
	if err != nil {
		return fmt.Errorf("update to_user role: %w", err)
	}
	n, err = checkedRowsAffected(resTo, "read to_user role update result")
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("to_user %s is no longer a member", xfer.toUserID)
	}
	return nil
}

func checkedRowsAffected(result sql.Result, operation string) (int64, error) {
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: rows affected: %w", operation, err)
	}
	return n, nil
}

func joinCleanupError(operation string, primaryErr, cleanupErr error) error {
	if cleanupErr == nil {
		return primaryErr
	}
	wrappedCleanupErr := fmt.Errorf("%s: %w", operation, cleanupErr)
	if primaryErr == nil {
		return wrappedCleanupErr
	}
	return errors.Join(primaryErr, wrappedCleanupErr)
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
