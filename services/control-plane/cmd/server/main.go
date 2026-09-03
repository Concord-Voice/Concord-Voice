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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage/probe"
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
	storageProbe    func([]string) int
}

func dispatchControlPlaneSubcommand(args []string) (int, bool) {
	return dispatchControlPlaneSubcommandWithRunners(args, controlPlaneSubcommandRunners{
		admin:           admin.RunAdminCtl,
		activityHistory: presencehistory.RunAdminCtl,
		storageProbe:    probe.Run,
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
	case "storage-probe":
		return runners.storageProbe(args[2:]), true
	case "serve":
		return 0, false
	default:
		if args[1] == "" {
			return 0, false
		}
		fmt.Fprintln(os.Stderr, "usage: control-plane [admin|activity-history|storage-probe] ...")
		return 64, true
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

	// TYPED NIL STOPS HERE. initStorageClient returns a nil *storage.Client when
	// object storage is unconfigured, and NewRouter's parameter is the
	// media.ObjectStore INTERFACE -- so passing the concrete pointer directly
	// produces a NON-NIL interface holding a nil pointer, and every downstream
	// `store == nil` guard silently becomes dead code. That is not theoretical:
	// it made router.go's "media endpoints disabled" branch unreachable and
	// could hand the durable GDPR erasure worker a typed-nil deleter instead of
	// letting it retain the obligation for a configured replica to retry (found
	// by three independent reviewers on PR #3019).
	//
	// Converting once, here at the boundary, is what makes those guards mean
	// what they say. Do NOT pass storageClient directly to an interface
	// parameter again; the compiler cannot catch it.
	mediaStore := mediaObjectStore(storageClient)
	tier1ErasureReclaimer := media.NewTier1ErasureReclaimer(db, mediaStore, log)

	// Boot-time object-storage backend registry (ADR-0038 / #2759). Placement
	// is per object, so reads resolve the store from the media_files row
	// rather than assuming this one client. NewRegistry cannot fail and never
	// aborts boot: a non-legacy backend that will not construct registers as
	// unavailable and fails closed only for the rows that name it.
	storageRegistry := storage.NewRegistry(cfg, storageClient, log)

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
	// #2445: the shared Rich Presence capture executor is built inside NewRouter
	// (it needs the activity service and sender-presence resolver, which live
	// there). The nightly temp-grant sweep constructs its own tempGrantManager
	// out here, so the executor is carried out to wire it.
	var voicePresenceRecheck rbac.PresenceRecheck
	completeExpiredOwnershipTransfers := func(context.Context) {}
	// Closes both presence dispatch workers; ordered before hub.Shutdown so
	// their fail-closed drains can still reach live sockets (#2738).
	closePresenceWorkers := func() {}
	// #2448: the durable active-category reconciler is built inside NewRouter —
	// it needs the hub as its terminal and the activity store as its reader, both
	// of which live there — and carried out here so its startup pass and its
	// ticker join the Activity History runtime below rather than a second one.
	//
	// Both closures below are safe to build before it exists: bindRouter runs
	// first inside initializeActivityHistoryRuntime, so the assignment has
	// happened by the time either is called. A boot that somehow reached
	// reconcileActivePlans with this still nil fails closed — ReconcilePass
	// nil-checks its receiver and returns ErrReconcilerNotWired — and the worker
	// never starts, because a failed startup pass aborts before startWorkers.
	var activePlanReconciler *activepresence.Reconciler
	runtime, startupErr := startActivityHistoryRuntime(activityHistoryRuntimeDependencies{
		startupContext:      context.Background(),
		workerContext:       cleanupCtx,
		reconcileDisclosure: presenceHistoryService.ReconcileStaleDisclosure,
		bindRouter: func() (*gin.Engine, *websocket.Hub, *natsclient.Client, error) {
			router, hub, natsClient, metricsRuntime, permissionEnforcer, presenceRecheck,
				closePresence, activePlans, completeExpiredTransfers, routerErr := api.NewRouter(
				db,
				redisClient,
				mediaStore,
				cfg,
				liveSpa,
				log,
				api.RouterDependencies{
					OpsMetricsReader:   adminMetricsRouterReader,
					PresenceHistory:    presenceHistoryService,
					Tier1ErasureWake:   tier1ErasureReclaimer.Wake,
					MediaStoreResolver: media.NewRegistryStoreResolver(storageRegistry),
					MediaWriteRouter:   media.NewRegistryWriteRouter(storageRegistry),
				},
			)
			if routerErr != nil {
				return nil, nil, nil, routerErr
			}
			opsMetricsRuntime = metricsRuntime
			voicePermissionEnforcer = permissionEnforcer
			voicePresenceRecheck = presenceRecheck
			closePresenceWorkers = closePresence
			activePlanReconciler = activePlans
			completeExpiredOwnershipTransfers = completeExpiredTransfers
			return router, hub, natsClient, nil
		},
		reconcilePending: presenceHistoryService.ReconcilePending,
		reconcileActivePlans: func(ctx context.Context, limit int) (activepresence.PassStats, error) {
			return activePlanReconciler.ReconcilePass(ctx, limit)
		},
		pendingWorker:    presenceHistoryService.RunPendingReconciler,
		activePlanWorker: func(ctx context.Context) { activePlanReconciler.Run(ctx) },
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
	waitTier1ErasureReclaimer := func() {}
	waitBackgroundWorkers := func() {
		waitActivityHistoryWorkers()
		waitTier1ErasureReclaimer()
		if voicePermissionEnforcer != nil {
			voicePermissionEnforcer.Close()
		}
	}

	go runCleanupJob(cleanupCtx, db, redisClient, log, completeExpiredOwnershipTransfers)

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
	voice.StartTempGrantSweepWorker(cleanupCtx, voice.TempGrantSweepDeps{
		DB:              db,
		Log:             log,
		Hub:             hub,
		Resolver:        sweepResolver,
		NATS:            natsClient,
		PresenceRecheck: voicePresenceRecheck,
	}, voice.DefaultTempGrantSweepInterval)

	// Start the subscription-expiry sweeper (#2158). Fixed-duration code grants
	// lapse purely by the clock; without this the passive expiry never fires
	// OnTierChange, so a client keeps premium UI affordances
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

	// Reclaims object-store bytes staged by chunked attachment upload sessions
	// that were never committed or cancelled (#2157 PR 2).
	//
	// This is LOAD-BEARING FOR CORRECTNESS, not defence in depth. Every other
	// cleanup path -- the client's cancel button, the unmount keepalive DELETE,
	// the 410 hard-TTL expiry -- reads or writes a Redis session record, so all
	// of them fail together the moment Redis does, which is exactly when uploads
	// get orphaned. This sweeper derives its work queue from the object store
	// itself, so a total Redis loss cannot strand bytes. That guarantee is what
	// lets the client-side paths be genuinely best-effort.
	//
	// ONE WORKER PER REGISTERED BACKEND, not one over the legacy client
	// (ADR-0038 action item 6). A sweeper holds a single store and therefore
	// enumerates a single bucket; wiring only the legacy one would leave every
	// vendor-resident abandoned upload unswept the moment the Wave C write
	// default moves, and it would do so silently -- `Attempted` falls to zero
	// and the all-aborts-failed alarm cannot fire on a batch nobody listed.
	//
	// No nil guard: storage.NewRegistry ALWAYS returns a non-nil registry (it
	// registers a legacy entry even when there is no legacy client), so a
	// `storageRegistry != nil` branch here would be dead code pretending to be
	// a fallback. Unconfigured storage is handled inside, where the one
	// registered-but-unavailable legacy backend is recognised and skipped
	// quietly rather than logged as a fault.
	media.StartSessionSweepWorkers(cleanupCtx, storageRegistry, log, media.DefaultSessionSweepInterval)
	waitTier1ErasureReclaimer = tier1ErasureReclaimer.Start(cleanupCtx)

	// Tier-2 orphan reclamation (#2759 follow-on). A SIBLING of the sweeper
	// above, not a duplicate: that one aborts INCOMPLETE multipart uploads,
	// this one deletes COMPLETED objects that no media_files row claims.
	//
	// It is the only reclamation path in this service that can start without a
	// row. Every other one -- the purge engine's queue, the straggler sweep,
	// CleanupObject -- begins from media_files, and an account erasure
	// hard-deletes those rows through ON DELETE CASCADE, so historical residue
	// is invisible to all of them at once. The erasure capture in
	// users.deleteAccountTx closes that going forward; this recovers the past.
	//
	// TIER 2 ONLY, permanently. See the header of internal/media/orphan_reaper.go
	// for why widening the prefix to tier-1 profile media would blank live
	// server and group-DM icons across the estate.
	media.StartOrphanSweepWorkers(cleanupCtx, db, storageRegistry, log, media.DefaultOrphanSweepInterval)

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
			func() { closePresenceWorkers() },
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
	waitActivityWorkers func(),
	// closePresenceWorkers drains the #2445 and #2446 presence dispatch queues.
	// It sits BEFORE shutdownHub on purpose: their fail-closed abandons
	// disconnect through the hub, so closing the hub first would discard
	// exactly the work the drain exists to perform (#2738 review).
	closePresenceWorkers func(),
	shutdownHub func(),
	shutdownMetrics func() error,
	shutdownAdminMetricsReader func() error,
	closeNATS func(),
) error {
	stopBackground()
	shutdownErr := shutdownHTTP()
	waitActivityWorkers()
	closePresenceWorkers()
	shutdownHub()
	shutdownErr = errors.Join(shutdownErr, shutdownMetrics())
	shutdownErr = errors.Join(shutdownErr, shutdownAdminMetricsReader())
	closeNATS()
	return shutdownErr
}

type activityHistoryRuntimeDependencies struct {
	startupContext       context.Context
	workerContext        context.Context
	reconcileDisclosure  func(context.Context) (int64, error)
	bindRouter           func() (*gin.Engine, *websocket.Hub, *natsclient.Client, error)
	reconcilePending     func(context.Context, int) (presencehistory.ReconcileStats, error)
	reconcileActivePlans func(context.Context, int) (activepresence.PassStats, error)
	pendingWorker        func(context.Context)
	activePlanWorker     func(context.Context)
	retentionWorker      func(context.Context)
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
		dependencies.reconcilePending == nil || dependencies.reconcileActivePlans == nil ||
		dependencies.pendingWorker == nil || dependencies.activePlanWorker == nil ||
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
			reconcilePending:     dependencies.reconcilePending,
			reconcileActivePlans: dependencies.reconcileActivePlans,
			startWorkers: func() {
				runtime.waitWorkers = startActivityHistoryWorkers(
					dependencies.workerContext,
					dependencies.pendingWorker,
					dependencies.activePlanWorker,
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
	reconcileDisclosure  func(context.Context) (int64, error)
	bindRuntime          func() error
	reconcilePending     func(context.Context, int) (presencehistory.ReconcileStats, error)
	reconcileActivePlans func(context.Context, int) (activepresence.PassStats, error)
	startWorkers         func()
}

func initializeActivityHistoryRuntime(
	ctx context.Context,
	steps activityHistoryStartupSteps,
) (int64, error) {
	if steps.reconcileDisclosure == nil || steps.bindRuntime == nil ||
		steps.reconcilePending == nil || steps.reconcileActivePlans == nil ||
		steps.startWorkers == nil {
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
	// #2448 joins HERE rather than getting its own startup call: this is the one
	// place that already establishes "startup reconciliation completes, under
	// activityHistoryStartupTimeout, before workers start, and boot FAILS if it
	// does not". That contract is the whole reason activepresence.Reconciler.Run
	// may discard its pass errors — a rail that cannot reconcile never serves.
	if _, err := steps.reconcileActivePlans(startupCtx, activityHistoryStartupBatch); err != nil {
		return 0, fmt.Errorf("reconcile pending active-category plans: %w", err)
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

// startActivityHistoryWorkers launches every supplied background loop and
// returns the wait function for all of them.
//
// Variadic rather than one parameter per worker: the previous form hardcoded
// Add(2) beside two named parameters, so #2448's third worker could not be added
// without editing a count that nothing would have failed on if it were missed —
// the wait would have returned while a worker was still running.
func startActivityHistoryWorkers(
	ctx context.Context,
	workers ...func(context.Context),
) func() {
	var group sync.WaitGroup
	group.Add(len(workers))
	for _, worker := range workers {
		go func(run func(context.Context)) {
			defer group.Done()
			run(ctx)
		}(worker)
	}
	return group.Wait
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

// initStorageClient builds the LEGACY object-storage client and only that one.
//
// Its retry-then-fatal-in-production behaviour is scoped to the legacy backend
// by construction, and must stay that way (ADR-0038 / #2759): every deployment
// depends on the legacy store for profile media and for the entire pre-cutover
// attachment corpus, so refusing to boot without it is defensible. It is not
// defensible for a vendor backend — an R2 outage or a credential problem there
// would take down auth, messaging and voice over a store that holds no objects
// this build has been told to read. Non-legacy backends are constructed by
// storage.NewRegistry, which never fails and never aborts boot.
// mediaObjectStore converts the concrete storage client into the interface
// NewRouter takes, WITHOUT producing a typed nil.
//
// Extracted rather than written inline so the conversion is testable: it is one
// `if` whose absence is invisible at the call site and silently re-arms the
// dead-guard class described above. A test that constructs the nil case and
// asserts `got == nil` fails the moment someone "simplifies" this back to a
// direct assignment.
func mediaObjectStore(client *storage.Client) media.ObjectStore {
	if client == nil {
		return nil
	}
	return client
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

// runCleanupJob periodically purges expired tokens, stale sessions, and
// UNEXPIRING Redis presence keys. The server must not rely on clients to clean
// up after themselves — this job is the authoritative backstop. Genuine orphans
// are reaped by their own 120s TTL, not by this job; see cleanupStalePresence.
func runCleanupJob(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, ownershipCleanup func(context.Context)) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Run once at startup to catch anything that accumulated while the server was down
	runCleanup(ctx, db, redisClient, log, ownershipCleanup)

	for {
		select {
		case <-ticker.C:
			runCleanup(ctx, db, redisClient, log, ownershipCleanup)
		case <-ctx.Done():
			log.Info("Cleanup job stopped")
			return
		}
	}
}

func runCleanup(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, ownershipCleanup func(context.Context)) {
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
	cleanupStalePresence(taskCtx, redisClient, log)

	// Task 4: Auto-complete expired ownership transfers through the ownership
	// handler, which owns its capture-bound authority transaction.
	if ownershipCleanup != nil {
		ownershipCleanup(taskCtx)
	}

	log.Debug("Cleanup completed")
}

const (
	// basePresenceScanPattern bounds the cleanup SCAN to the presence keyspace.
	basePresenceScanPattern = "presence:*"
	// presenceScanBatch is the SCAN COUNT hint, and therefore also the number of
	// TTL reads that ride in one pipeline round trip.
	presenceScanBatch = 100
)

type stalePresenceStore interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
	Pipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error)
}

// reapUnexpiringPresenceLua deletes a base presence key only if it has NO
// expiry, atomically, returning the number of keys it removed.
//
// The guard lives in Lua for two independent reasons, and both are load-bearing.
//
// It makes the read and the delete ONE operation. A separate PTTL then DEL has a
// window: PTTL can report -2 (key already gone), a disconnected user can
// reconnect and SET the key with a fresh 120s TTL, and the DEL then destroys a
// live registration -- proven with a passing exploit during review. Batching the
// reads made that window WIDER, not narrower, because it spans the rest of the
// batch rather than a single round trip.
//
// And PTTL's sentinels are raw integers here, so `== -1` means exactly "exists
// with no expiry" with no client-side scaling question at all. That matters
// because go-redis returns -1/-2 UNSCALED as nanoseconds, making the natural
// `ttl == -1*time.Second` permanently false. internal/presence keeps its own
// PTTL guard in Lua for precisely this reason; this is the same decision.
const reapUnexpiringPresenceLua = `
if redis.call('PTTL', KEYS[1]) == -1 then
  return redis.call('DEL', KEYS[1])
end
return 0
`

// isBasePresenceKey reports whether key is exactly the key that
// presence.StatusRedisKey builds for some user ID.
//
// This is an ALLOWLIST, and that is the point. "presence:" is a SHARED prefix:
// rich presence stores "presence:rich:<uuid>:<category>" beneath it, and any
// family added later lands there too. The previous denylist -- delete whatever
// does not parse as presence:<uuid> -- therefore treated every rich-presence key
// as malformed and deleted the lot on every pass.
//
// The round trip through StatusRedisKey is load-bearing, not decoration, and it
// must not be "simplified" back to `err == nil`. uuid.Parse's own documentation
// says it should not be used to validate strings: it is case-insensitive and
// accepts the urn:uuid:, brace-wrapped and bare-32-hex spellings, and its
// 38-byte arm strips the first byte WITHOUT checking that it was a brace -- so a
// bare Parse admits "presence:@<uuid>#" and every other single-delimiter shape.
// Under the old denylist that leniency erred toward KEEPING keys; under an
// allowlist the same leniency errs toward DELETING them, so the predicate is
// pinned to the one spelling the writer actually emits.
func isBasePresenceKey(key string) bool {
	userID, found := strings.CutPrefix(key, "presence:")
	if !found {
		return false
	}
	parsed, err := uuid.Parse(userID)
	return err == nil && key == presence.StatusRedisKey(parsed)
}

// reapUnexpiringPresence deletes every key in the batch that carries no expiry,
// running one atomic guard per key in ONE pipeline round trip.
//
// Batching is not a micro-optimization. A candidate key is roughly "one
// currently-online user", so a guard per candidate un-batched turns a ~N/100
// round-trip pass into ~N, inside the single 30s budget this job shares with the
// ownership-transfer sweep that runs AFTER it -- a large fleet could starve that
// sweep and surface only as a context deadline in an unrelated task.
//
// A key whose guard fails to run is left alone. The entire justification for
// deleting rests on that read, so an unread TTL must never authorize a delete;
// the key is reconsidered next pass and expires on its own regardless.
func reapUnexpiringPresence(
	ctx context.Context,
	redisClient stalePresenceStore,
	keys []string,
) (reaped, failed int, firstErr error) {
	if len(keys) == 0 {
		return 0, 0, nil
	}
	cmds := make([]*redis.Cmd, len(keys))
	_, pipeErr := redisClient.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, key := range keys {
			cmds[i] = pipe.Eval(ctx, reapUnexpiringPresenceLua, []string{key})
		}
		return nil
	})
	for _, cmd := range cmds {
		// A nil command means the pipeline never queued this guard, so pipeErr is
		// the only error there is to report for it. Per-command errors are
		// preferred where they exist: one unrunnable guard must not discard the
		// verdict for the rest of the batch.
		if cmd == nil {
			failed++
			if firstErr == nil {
				firstErr = pipeErr
			}
			continue
		}
		removed, err := cmd.Int()
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// The script returns what DEL actually removed, so a key that expired
		// between the SCAN and the guard contributes 0 and is never counted as
		// reaped.
		reaped += removed
	}
	return reaped, failed, firstErr
}

// cleanupStalePresence removes base presence keys that outlived their expiry.
//
// Every writer of presence:<uuid> sets a 120s TTL (internal/websocket/hub.go
// registration, offline transition, set_status, and the heartbeat repair paths;
// refreshPresenceTTL renews it with EXPIRE). A base key that still exists with
// no expiry therefore cannot have come from any of them, and is the only thing
// this job may delete.
//
// It deliberately does NOT consult the hub's connected-user set. That check
// could only ever reap an orphan Redis expiry removes within 120s anyway, which
// does not buy enough to justify what it cost: runCleanup fires once at startup
// against an empty hub, so every restart deleted every live base presence key in
// the fleet -- deterministically, not as a race it might have lost.
//
// The pass summary is logged UNCONDITIONALLY, and that is a consequence of the
// predicate above rather than a style choice. A healthy fleet now deletes
// nothing, so a summary gated on "something was deleted" would emit no line
// ever; before the fix the accidental rich-presence deletions fired it every
// hour and it doubled as this job's only heartbeat. Losing that would leave a
// panicked goroutine, a cancelled context, a ticker that never fired and a
// Redis ACL denying SCAN all indistinguishable from perfect health, because
// runCleanup's own completion line is Debug and production logs at Info.
func cleanupStalePresence(ctx context.Context, redisClient stalePresenceStore, log *logger.Logger) {
	var (
		scanned     int
		candidates  int
		staleCount  int
		ttlErrors   int
		firstTTLErr error
		partial     bool
		cursor      uint64
	)

	for {
		keys, nextCursor, err := redisClient.Scan(ctx, cursor, basePresenceScanPattern, presenceScanBatch).Result()
		if err != nil {
			// Truncating the pass is right -- the summary below must still run --
			// but the summary has to SAY it was truncated, or a partial pass that
			// reaped two keys reports identically to a complete one.
			log.Error("Cleanup: failed to scan presence keys", "error", err)
			partial = true
			break
		}
		scanned += len(keys)

		batch := make([]string, 0, len(keys))
		for _, key := range keys {
			if isBasePresenceKey(key) {
				batch = append(batch, key)
			}
		}
		candidates += len(batch)

		reaped, failed, guardErr := reapUnexpiringPresence(ctx, redisClient, batch)
		staleCount += reaped
		ttlErrors += failed
		if firstTTLErr == nil {
			firstTTLErr = guardErr
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// One aggregated line, never one per key: a Redis fault that fails every
	// PTTL while SCAN keeps succeeding would otherwise emit one Error per online
	// user per hour, indefinitely. The key itself is never logged -- it embeds a
	// user UUID (observability.md principle 2).
	if firstTTLErr != nil {
		log.Error("Cleanup: failed to reap unexpiring presence keys",
			"error", firstTTLErr, "affected", ttlErrors)
	}
	log.Info("Cleanup: presence pass complete",
		"scanned", scanned,
		"candidates", candidates,
		logKeyCount, staleCount,
		"ttl_errors", ttlErrors,
		"partial", partial,
	)
}
