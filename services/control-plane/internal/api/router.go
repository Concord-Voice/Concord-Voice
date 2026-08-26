// Package api provides HTTP routing and middleware setup for the Control Plane REST API.
package api // revive:disable-line:var-naming

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/clientconfig"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/feedback"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/graphpresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/klipy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/notifications"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ownership"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servercapabilities"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/sessions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/updates"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// richPresenceSuppressTimeout bounds one hidden-sender suppression: acquiring the
// sender gate, a bounded recipient resolve, the deletes, and the clear fan-out.
// It runs off the hub Run goroutine, so this bounds the worker, not the loop.
const richPresenceSuppressTimeout = 5 * time.Second

// Route path constants — extracted to satisfy go:S1192 (no duplicate string literals).
const (
	routeTransferOwnership = "/:id/transfer-ownership"
	routeRecoveryKey       = "/recovery-key"
	routeRecoveryCircle    = "/recovery-circle"
	pathIDMembers          = "/:id/members"
	pathIDMembersUserID    = "/:id/members/:user_id"
	pathIDMembersTimeout   = "/:id/members/:user_id/timeout"
	pathIDRead             = "/:id/read"
	pathIDKeys             = "/:id/keys"
	pathIDOverrides        = "/:id/overrides"
	routeVoiceMute         = "/:id/voice/:userId/mute"
	routeVoiceDeafen       = "/:id/voice/:userId/deafen"
	routeVoiceMove         = "/:id/voice/:userId/move"
	routeVoiceDisconnect   = "/:id/voice/:userId/disconnect"
	routeVoiceTempAccess   = "/:id/voice/:userId/temp-access"
)

// permissionCheckerAdapter adapts an rbac.Resolver to websocket checker interfaces
// without making the websocket package import rbac.
type permissionCheckerAdapter struct {
	resolver *rbac.Resolver
}

type dmVoiceCallLeaseVerifier struct {
	redis *redis.Client
}

func (v dmVoiceCallLeaseVerifier) Matches(
	ctx context.Context,
	conversationID, callID uuid.UUID,
) (bool, error) {
	lease, found, err := dm.LookupDMVoiceCallLease(ctx, v.redis, conversationID)
	if err != nil {
		return false, err
	}
	return found && lease.CallID == callID, nil
}

func (a *permissionCheckerAdapter) HasMentionPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error) {
	return a.resolver.HasPermission(ctx, serverID, userID, channelID, rbac.Permission(permBit))
}

func (a *permissionCheckerAdapter) HasChannelPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error) {
	return a.resolver.HasPermission(ctx, serverID, userID, channelID, rbac.Permission(permBit))
}

func (a *permissionCheckerAdapter) HasChannelPermissionsUncached(ctx context.Context, serverID, userID, channelID string, permBits ...int64) (bool, error) {
	permissions, err := a.resolver.ResolveEffectivePermissionsUncached(ctx, serverID, userID, channelID)
	if errors.Is(err, rbac.ErrNotMember) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, permBit := range permBits {
		if !permissions.Has(rbac.Permission(permBit)) {
			return false, nil
		}
	}
	return true, nil
}

// configureTrustedProxies applies the CIDR allowlist (validated + defaulted at
// config Load time, production-guarded) and emits a startup audit log. When a
// request arrives from a trusted peer, Gin iterates RemoteIPHeaders
// [X-Forwarded-For, X-Real-IP] in order: for X-Forwarded-For it walks
// right-to-left skipping trusted hops and returns the first untrusted address;
// X-Real-IP is tried next if XFF is absent or yields no valid IP. When the
// peer is untrusted, both headers are ignored and RemoteAddr is returned —
// preserving anti-spoof semantics.
func configureTrustedProxies(router *gin.Engine, cfg *config.Config, log *logger.Logger) {
	if err := router.SetTrustedProxies(cfg.TrustedProxyCIDRs); err != nil {
		log.Fatal("Failed to configure trusted proxies", "error", err)
	}
	log.Info("Trusted proxies configured",
		"cidrs", cfg.TrustedProxyCIDRs,
		"count", len(cfg.TrustedProxyCIDRs))
}

// testEnvAuthFlowCap is the per-IP auth-flow rate-limit cap applied when the
// control-plane runs under the e2e/integration test profile (CONCORD_ENV=test).
// Set far above any plausible e2e suite size — the Playwright suite drives ~18
// UI registrations AND a login per backend spec from a single CI IP (see #1274)
// — while staying bounded (not unlimited) to retain a sanity ceiling against a
// runaway test loop.
const testEnvAuthFlowCap = 10000

// Fallback purge rate-limit values (#1352), matching the config.Load() defaults for
// PURGE_RATE_LIMIT / PURGE_RATE_WINDOW_SEC. Applied when a hand-built Config leaves
// them at Go's zero value, which RateLimitByUserFailClosed would otherwise read as
// "allow 0 requests" and reject every purge with 429.
const (
	defaultPurgeRateLimit  = 5
	defaultPurgeRateWindow = time.Hour
)

// NOTE: the ":id"-scoped "/:id/messages" sub-path is registered as a repeated string
// literal under the server, channel, and DM groups rather than extracted to a shared
// constant. scripts/api/check-openapi-coverage.sh parses route registrations
// textually and cannot resolve a path constant — extracting one makes the scanner
// report every affected route (including the pre-existing GETs) as missing from the
// router, breaking the OpenAPI coverage gate. The duplication is required by that
// tooling; do not "fix" it. See the SonarQube FP register (go:S1192, router.go).

// resolvePurgeRateLimit returns the per-actor purge rate limit and window, falling
// back to the Load() defaults when the Config was hand-built without them (tests,
// embedders). A zero limit or window is NOT "unlimited" — the fail-closed limiter
// reads it as "allow 0 requests" and rejects every purge with 429, bricking the
// endpoints. Mirrors the purge.NewEngine maxBatch guard.
func resolvePurgeRateLimit(cfg *config.Config) (int, time.Duration) {
	limit := cfg.PurgeRateLimit
	if limit <= 0 {
		limit = defaultPurgeRateLimit
	}
	window := time.Duration(cfg.PurgeRateWindowSec) * time.Second
	if window <= 0 {
		window = defaultPurgeRateWindow
	}
	return limit, window
}

// authFlowTestRateLimit returns the per-IP request cap for an auth-flow route
// (POST /register, /register/confirm, /login). Under CONCORD_ENV=test it relaxes
// the cap to testEnvAuthFlowCap; otherwise it returns prodCap unchanged, so
// production rate limits are untouched.
//
// Why this exists: the Playwright e2e suite cannot share one authenticated
// session across specs — the access token is held in-memory only and the
// refresh token never enters the renderer (see client/desktop/src/renderer/
// services/apiClient.ts), and e2eeService initializes during the real LOGIN
// flow (registration does NOT init it) — so each backend spec registers a fresh
// user AND logs in. From one CI IP this exceeds the production caps (5/15min
// register, 10/15min login) keyed by ratelimit:ip:<ip>:<method>:<path>. Relaxing
// only these auth-flow routes under the existing test gate is the least-invasive
// fix. Rate-limit behavior itself is owned by middleware unit tests, not the
// e2e suite. See #1274 and [internal]0011-playwright-e2e-rate-limit-test-gate.md.
func authFlowTestRateLimit(prodCap int) int {
	if isE2ETestEnv() {
		return testEnvAuthFlowCap
	}
	return prodCap
}

// isE2ETestEnv reports whether the control-plane runs under the e2e/integration
// test profile (CONCORD_ENV=test). It mirrors the gate used by auth.isTestEnv()
// (which writes plaintext verification codes to Redis under test_only:<id> for
// test recovery). Kept as a local mirror rather than exporting the auth helper,
// to leave the security-sensitive auth package's API surface unchanged.
// CONCORD_ENV is never "test" in production, so anything gated on this is a
// structural no-op in prod.
func isE2ETestEnv() bool {
	return os.Getenv("CONCORD_ENV") == "test"
}

func publicInviteIconHandler(invitesHandler *invites.Handler, mediaHandler *media.Handler) gin.HandlerFunc {
	if mediaHandler != nil {
		return mediaHandler.ProxyInviteServerIcon
	}
	return invitesHandler.GetPublicInviteIconFallback
}

func publicFriendAvatarHandler(friendsHandler *friends.Handler, mediaHandler *media.Handler) gin.HandlerFunc {
	if mediaHandler != nil {
		return mediaHandler.ProxyFriendCodeAvatar
	}
	return friendsHandler.GetPublicFriendAvatarFallback
}

func bindPresenceHistoryRuntime(
	hub *websocket.Hub,
	presenceHistoryService *presencehistory.Service,
) error {
	if presenceHistoryService == nil {
		return fmt.Errorf("presence history service unavailable")
	}
	hub.SetPresenceHistoryService(presenceHistoryService)
	if err := presenceHistoryService.BindDelivery(hub); err != nil {
		return fmt.Errorf("bind presence history delivery: %w", err)
	}
	return nil
}

func configureOpsMetricsAndRecovery(router *gin.Engine, enabled bool) *opsmetrics.Counters {
	var counters *opsmetrics.Counters
	if enabled {
		counters = opsmetrics.NewCounters()
		router.Use(opsmetrics.RequestMetricsMiddleware(counters))
	}
	// Metrics must wrap Recovery so recovered panics are observed after the
	// response status has been changed to 500.
	router.Use(gin.Recovery())
	return counters
}

// RouterDependencies groups runtime services that are injected into NewRouter.
type RouterDependencies struct {
	OpsMetricsReader opsmetrics.Reader
	PresenceHistory  *presencehistory.Service
}

// requirePresenceRecheckWired fails startup when the #2445 Rich Presence capture
// executor is missing while the activity service is present.
//
// A missing wiring line is the one fail-OPEN path in that design: RBAC mutations
// would commit with no capture and no clear, silently restoring the disclosure
// the issue closed. Converting it into a boot failure matches the fail-closed
// posture, and it cannot misfire where the activity service itself is absent.
//
// It interrogates the HANDLER, not the executor value the caller happens to
// hold. Taking `presenceRecheck rbac.PresenceRecheck` made the guard a
// tautology: voicepresence.NewExecutor always returns a non-nil *Executor, and
// boxing a non-nil pointer into an interface always yields a non-nil interface,
// so the condition was unreachable. Worse, it inspected a local variable rather
// than handler state — deleting the SetPresenceRecheck call, the exact
// fail-OPEN path named above, still booted cleanly. Asking the handler whether
// it was wired is the only formulation that catches that (#2445 review).
//
// Extracted from NewRouter rather than inlined: the guard's two-condition check
// is two points of cognitive complexity in a function already at the go:S3776
// limit.
func requirePresenceRecheckWired(
	log *logger.Logger,
	activityService *presence.ActivityService,
	rbacHandler *rbac.Handler,
) {
	if activityService != nil && !rbacHandler.HasPresenceRecheck() {
		log.Fatal("Rich Presence recheck executor is required when the activity service is wired")
	}
}

// graphPresenceWiringComplete reports whether every graph-presence consumer was
// wired. It asks the CONSUMERS, never the constructed reconciler:
// graphpresence.New always returns a non-nil pointer, so a check on that value
// is a tautology that still boots with a SetGraphPresenceCapture line deleted —
// the one fail-OPEN path this guard exists to catch.
//
// Seven consumers as of #2448: the two #2446 graph consumers plus membership,
// invite join, server delete and account erasure — and, for erasure, its
// cross-replica clear publisher too, because that publish is the only mechanism
// that retracts an erased user's Custom Status anywhere. #2448 adds the seventh,
// DM group deletion, and a third erasure arm, the durable active-category drain.
// An unwired consumer is a SILENT hazard — the handler serves traffic and skips
// reconciliation with no error anywhere — which is why this stays a log.Fatal
// rather than a warning.
//
// The #2448 arms are the sharpest case yet for that posture.
// activepresence.Reconciler.Run discards every pass error by design, so an
// unwired DM rail deletes a group, commits the cascade, records NO obligation,
// and destroys the C3 evidence — dm_voice_participants and the Redis lease — in
// that same commit. There is no later recovery, and no error is raised anywhere.
// An unwired erasure drain is louder but still wrong: the erasure hits migration
// 000111's RESTRICT and fails with an opaque 23503.
func graphPresenceWiringComplete(c graphPresenceConsumers) bool {
	return c.friends.HasGraphPresenceCapture() &&
		c.users.HasGraphPresenceCapture() &&
		c.members.HasGraphPresenceCapture() &&
		c.invites.HasGraphPresenceCapture() &&
		c.servers.HasGraphPresenceCapture() &&
		c.erasure.HasGraphPresenceCapture() &&
		c.erasure.HasErasureClearPublisher() &&
		c.dm.HasActivePlanRail() &&
		c.erasure.HasActivePlanDrain()
}

// graphPresenceConsumers groups the seven consumers the boot guard interrogates.
// A struct rather than seven positional parameters: threading them individually
// pushed requireGraphPresenceCaptureWired past the parameter limit, and seven
// same-shaped pointers in a row is exactly the signature where a transposed
// argument compiles and silently checks the wrong handler twice.
type graphPresenceConsumers struct {
	friends *friends.Handler
	users   *users.Handler
	members *members.Handler
	invites *invites.Handler
	servers *servers.Handler
	dm      *dm.Handler
	erasure *users.AccountService
}

// familyGaps is presencecapture.UnregisteredFamilies behind a package var, so
// a test can drive graphPresenceFamilyRegistryComplete's FALSE branch. This
// declaration is its only assignment outside a test, so every boot still reads
// the real registry.
//
// The seam has to sit here rather than in presencecapture. That package's own
// registry_test produces a gap by swapping the unexported familyRegistry, which
// an internal/api test cannot reach, and UnregisteredFamilies takes no
// argument — so without this var nothing could make the zero-arg guard below
// answer false.
var familyGaps = presencecapture.UnregisteredFamilies

// graphPresenceFamilyRegistryComplete reports whether every declared
// presencecapture.Family carries a registry entry. It is separate from the
// wiring guard because it is a property of the CODE, not of this boot's
// dependency graph, so it must hold even on a replica with no activity service.
//
// This zero-arg form is what the guard calls, so the guard interrogates the
// registry and never a value a caller constructed: familyGaps is package state,
// not a parameter, and no boot path can substitute one.
//
// Two seams, two mutations. familyRegistryComplete isolates the verdict so a
// table can drive it over a supplied gap list; familyGaps isolates the lookup
// so a test can drive THIS composition false. Neither covers the other — the
// table never calls this function, so rewriting its body to `return true` would
// leave the suite green and the boot guard inert, which is the same fail-OPEN
// class the comment above graphPresenceWiringComplete names.
func graphPresenceFamilyRegistryComplete() bool {
	return familyRegistryComplete(familyGaps())
}

// familyRegistryComplete is the verdict over an already-collected gap list.
// Complete means the list is empty; any missing family is a boot failure.
func familyRegistryComplete(missing []presencecapture.Family) bool {
	return len(missing) == 0
}

// requireGraphPresenceFamilyRegistry fatal-exits on an unregistered family.
// The count is logged, never the family values: a fixed integer is enough to
// act on and keeps the line free of anything derived from an enum a future
// append may name after a customer-visible feature.
func requireGraphPresenceFamilyRegistry(log *logger.Logger) {
	if graphPresenceFamilyRegistryComplete() {
		return
	}
	log.Fatal("presence capture family registry is incomplete",
		"missing_count", len(familyGaps()))
}

// graphPresenceRailWired reports whether the durable Custom Status leg is wired
// onto the constructed reconciler.
//
// This is the ONE place a #2446 guard legitimately interrogates the reconciler
// rather than a handler. The rule it appears to break — "ask the HANDLERS,
// never the constructed reconciler" — exists because graphpresence.New always
// returns a non-nil pointer, so a nil check on that VALUE is a tautology that
// still boots with a SetGraphPresenceCapture line deleted. HasTopologyRail is
// not that: it reports whether SetTopologyRail actually ran, which is exactly
// the fail-open a deleted wiring line would produce.
func graphPresenceRailWired(capture *graphpresence.Reconciler) bool {
	return capture.HasTopologyRail()
}

// activePlanRailWired reports whether the #2448 durable active-category rail can
// deliver. Like graphPresenceRailWired it legitimately interrogates the
// constructed value, because HasReconciler reports whether the wiring actually
// RAN rather than whether a pointer is non-nil: activepresence.NewRail and
// NewReconciler both always return non-nil, so `rail != nil` would be the exact
// tautology that boots with the terminal never attached.
//
// It is separate from graphPresenceWiringComplete's consumer clauses because it
// asks a different question. Those ask whether each handler received A rail;
// this asks whether the one rail they all received can reach a client.
func activePlanRailWired(rail *activepresence.Rail) bool {
	return rail.HasReconciler()
}

// buildActivePlanRail constructs the #2448 rail and hands back both halves: the
// rail its three consumers hold, and the reconciler cmd/server drives from the
// Activity History startup pass and its ticker.
//
// It is a helper rather than two lines inside NewRouter for a measured reason —
// NewRouter sits at the go:S3776 cognitive-complexity limit of 15, so every
// addition there has to be branch-free, and keeping the construction out of it
// leaves the budget for the wiring the guard checks.
//
// store is passed twice on purpose: ActivityStore is both the StateReader the
// resolver reads through and the GenerationDeleter the terminal deletes through.
// The deleter is CompareAndDelete, never Delete — Delete ignores the generation
// and would destroy a newer one that raced in.
func buildActivePlanRail(
	db *sql.DB,
	gate activepresence.Gate,
	store *presence.ActivityStore,
	deliverer activepresence.Deliverer,
	log *logger.Logger,
) (*activepresence.Rail, *activepresence.Reconciler) {
	reconciler := activepresence.NewReconciler(db, gate, store, store, deliverer, log)
	return activepresence.NewRail(db, gate, reconciler, log), reconciler
}

// requireGraphPresenceCaptureWired fatal-exits when the activity service exists
// and any #2446 consumer is unwired, or when the durable Custom Status rail is
// unwired. Its scope stops at the handlers it names: adding a third consumer
// without adding it here is a silent hazard, exactly as the three temp-grant
// owners remain one for #2445.
//
// An unwired rail is FATAL rather than a degrade. Without it every hooked write
// silently reverts to PR 1's in-memory-only behaviour, and Custom Status is the
// one leg with no level arm to fall back on: a viewer who never reconnects
// holds the sender's text indefinitely.
//
// The activityService == nil early return stays, though not for the reason it
// looks like: SetGraphPresenceCapture below is unconditional, so such a replica
// DOES run the #2446 consumers — what it loses is the C1 refresh leg, which
// fails closed on its own. The return exists for parity with
// requirePresenceRecheckWired and for direct unit-test calls. On a real boot it
// is unreachable: presence.NewActivityService returns a struct-literal pointer
// and never nil.
func requireGraphPresenceCaptureWired(
	log *logger.Logger,
	activityService *presence.ActivityService,
	capture *graphpresence.Reconciler,
	activePlanRail *activepresence.Rail,
	consumers graphPresenceConsumers,
) {
	if activityService == nil {
		return
	}
	if !graphPresenceWiringComplete(consumers) {
		log.Fatal("graph presence capture is not wired on every consumer handler")
	}
	if !graphPresenceRailWired(capture) {
		log.Fatal("graph presence durable custom status rail is not wired")
	}
	if !activePlanRailWired(activePlanRail) {
		log.Fatal("durable active-category presence rail is not wired")
	}
}

// requireStepUpBudgetWired fatal-exits when the users handler has no Redis
// client for the #2765 purge-fence step-up budget.
//
// The budget fails CLOSED on a nil client, which is the right direction but a
// silent one: an unwired handler would answer every attempt to disable
// require_auth_before_purge with a 429 that no user or operator could explain.
// Boot is where that should surface. Same reasoning, and same handler-not-value
// interrogation, as requireGraphPresenceCaptureWired above.
func requireStepUpBudgetWired(log *logger.Logger, u *users.Handler) {
	if u == nil || !u.HasRedis() {
		log.Fatal("users handler has no Redis client: the step-up attempt budget would deny every request")
	}
}

// NewRouter creates a new API router and returns its background runtime dependencies.
func NewRouter(
	db *sql.DB,
	redis *redis.Client,
	store media.ObjectStore,
	cfg *config.Config,
	liveSpa *config.LiveSpaConfig,
	log *logger.Logger,
	dependencies RouterDependencies,
) (*gin.Engine, *websocket.Hub, *natsclient.Client, *OpsMetricsRuntime, *voice.PermissionEnforcer, rbac.PresenceRecheck, func(), *activepresence.Reconciler, error) {
	metricsReader := dependencies.OpsMetricsReader
	presenceHistoryService := dependencies.PresenceHistory
	router := gin.New()
	configureTrustedProxies(router, cfg, log)

	// Middleware
	opsCounters := configureOpsMetricsAndRecovery(router, cfg.OpsMetrics.Enabled)
	router.Use(middleware.RequestID())
	router.Use(middleware.Logger(log))
	router.Use(middleware.SecurityHeaders(cfg.Environment, cfg.HSTSHeaderValue))
	router.Use(middleware.CORS(cfg.AllowedOrigins))
	router.Use(middleware.ValidateCustomHeaders())

	// Health check (#882: GET + HEAD both 200, so CF probes and monitoring
	// tools that default to HEAD don't false-negative). Handler is at package
	// scope (healthHandler below) so router_test.go can call it directly.
	router.GET("/health", healthHandler)
	router.HEAD("/health", healthHandler)

	// Initialize WebSocket hub
	hub := websocket.NewHub(db, redis, opsCounters)
	if err := bindPresenceHistoryRuntime(hub, presenceHistoryService); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}

	// Initialize NATS (inter-service messaging with media plane)
	var natsClient *natsclient.Client
	nc, err := natsclient.Connect(cfg.NATSUrl)
	if err != nil {
		// Reaching here means a CONFIGURATION fault, not an outage.
		// RetryOnFailedConnect (pkg/nats.Connect) makes an unreachable bus
		// return a reconnecting client rather than an error (#2854 finding A),
		// so what is left is an unparseable URL or bad credentials -- a
		// deterministic deploy defect. The boot guard fatals shortly after; this
		// is the line that says why.
		//
		// Deliberately NOT a list of affected features (#2875). The old text
		// named only voice-state sync while the bus had since gained the
		// erasure-clear leg, graph presence and voice permission enforcement, so
		// an operator chasing "why is a deleted account's presence still
		// visible" got no signal from it. An enumeration is exactly what goes
		// stale when a subject is added, and nothing about adding one prompts
		// anyone to revisit this string.
		//
		// The raw error is NOT logged: natsclient.Connect wraps nats.Connect,
		// whose *url.Error formats the raw URL, so a nats://<user>:<pass>@host
		// misconfiguration would write the credential into the log (CWE-532).
		log.Warn("NATS configuration is invalid — every bus-dependent feature "+
			"would be degraded; boot will fail", "failure_class", "nats_config")
	} else {
		natsClient = nc
	}

	// Initialize MFA handler — versioned keyring (#2307). ParseKeyring errors
	// never contain key material, only version numbers.
	mfaKeyring, err := mfa.ParseKeyring(cfg.MFAEncryptionKey, cfg.MFAEncryptionKeyVersion, cfg.MFAEncryptionKeysRetired)
	if err != nil {
		log.Fatal("Invalid MFA encryption keyring", "error", err)
	}
	webauthnSvc, err := mfa.NewWebAuthnService(cfg.WebAuthnRPID, "ConcordVoice", cfg.WebAuthnRPOrigins)
	if err != nil {
		log.Fatal("Failed to create WebAuthn service", "error", err)
	}
	mfaHandler := mfa.NewHandler(db, redis, log, mfaKeyring, cfg.JWTSecret, webauthnSvc, cfg.Environment)

	// Initialize RBAC components (before handlers that depend on resolver)
	permCache := rbac.NewPermissionCache(redis)
	rbacResolver := rbac.NewResolver(db, permCache, log)
	permissionChecker := &permissionCheckerAdapter{resolver: rbacResolver}
	hub.SetMentionChecker(permissionChecker)
	hub.SetChannelPermissionChecker(permissionChecker)
	activityStore := presence.NewActivityStore(redis)
	activityBuilder := presence.NewActivityBuilder(
		db, dmVoiceCallLeaseVerifier{redis: redis}, activityStore,
	)
	// The base-presence gate for Rich Presence emission (#2444). Both the
	// lifecycle service and the bootstrap snapshot reader share one resolver so
	// they cannot disagree about whether a sender may publish.
	senderPresence := websocket.NewSenderPresenceResolver(redis, db, hub)
	activityService := presence.NewActivityService(
		presenceHistoryService,
		activityBuilder,
		activityStore,
		db,
		rbacResolver,
		hub,
		senderPresence,
	)
	activitySnapshotService := presence.NewActivitySnapshotService(
		db,
		activityBuilder,
		activityStore,
		rbacResolver,
		presenceHistoryService,
		senderPresence,
	)
	hub.SetActivitySnapshotService(activitySnapshotService)
	// The edge arm. The sender gate is acquired HERE, at the injection site,
	// because SuppressHiddenSenderActivityAlreadyGated is an AlreadyGated method
	// by contract -- matching how account erasure calls its sibling. The hub
	// dispatches this off its Run goroutine, so blocking on the gate is safe.
	hub.SetRichPresenceHiddenSuppressor(
		newRichPresenceHiddenSuppressor(activityService, presenceHistoryService, log, false),
		newRichPresenceHiddenSuppressor(activityService, presenceHistoryService, log, true),
	)

	auditWriter := rbac.NewAuditWriter(db, log)
	rbacHandler := rbac.NewHandler(db, log, redis, hub, rbacResolver, permCache, auditWriter)
	// Mid-session voice permission push (CV-CAN-007 review P1): permission
	// mutations re-resolve and publish voice.enforce.permissions for
	// voice-connected members. One shared enforcer instance backs the RBAC
	// handler, the ownership handler, and the voice.joined bridge — its
	// internal publish serialization only holds within one instance. Safe with
	// a nil natsClient (the enforcer no-ops without NATS).
	voicePermEnforcer := voice.NewPermissionEnforcer(db, log, rbacResolver, natsClient)
	rbacHandler.SetVoiceEnforcer(voicePermEnforcer)

	// Post-mutation Rich Presence reconciliation (#2445). One shared executor
	// backs the RBAC handler and the temporary-SBAC revoke path; it owns both
	// halves — the in-transaction pre-mutation capture and the post-commit
	// dispatch. It imports rbac and presence; rbac imports neither.
	presenceRecheckExecutor := voicepresence.NewExecutor(
		db, activityService, rbacResolver, senderPresence, hub, log,
	)
	rbacHandler.SetPresenceRecheck(presenceRecheckExecutor)
	requirePresenceRecheckWired(log, activityService, rbacHandler)

	// Initialize email service
	emailSvc := email.NewService(cfg, log)

	// Credential-epoch fence (#2201): the single process-wide verifier/mutator
	// for users.credential_epoch. Injected into AuthRequired, the WS surfaces,
	// and the destructive credential flows.
	credFence := credepoch.New(db, redis, log)

	// Initialize handlers
	authHandler := auth.NewHandlerForInstance(db, redis, log, cfg.JWTSecret, hub, cfg.InstanceType)
	authHandler.SetPresenceHistory(presenceHistoryService)
	authHandler.SetEmailService(emailSvc)
	authHandler.SetCredentialFence(credFence)
	authHandler.SetKeyRevocationBroadcaster(websocket.KeyRevocationBroadcaster(hub))
	authHandler.SetInitialDistributorChecker(rbacResolver.CanDistributeChannelKeyTx)
	// Wire cross-references (breaks circular init dependency)
	authHandler.SetMFAChecker(mfaHandler)
	mfaHandler.SetLoginCompleter(authHandler)
	mfaHandler.SetEmailService(emailSvc)
	entCache := entitlements.NewCacheForInstance(redis, db, cfg.InstanceType)
	serverEntCache := entitlements.NewServerCacheForInstance(redis, db, cfg.InstanceType)
	sessionsHandler := sessions.NewHandler(db, redis, log, hub, mfaHandler)
	usersHandler := users.NewHandler(db, log, hub, mfaHandler, entCache, credFence, authHandler)
	usersHandler.SetRedis(redis)
	requireStepUpBudgetWired(log, usersHandler)
	usersHandler.SetPresenceHistory(presenceHistoryService)
	usersHandler.SetActivitySettingsSuppressor(activityService)
	presenceHistoryHandler := presencehistory.NewHandler(presenceHistoryService)
	serversHandler := servers.NewHandler(db, log, hub, rbacResolver, entCache, serverEntCache)
	channelsHandler := channels.NewHandler(db, log, hub, rbacResolver, redis, serverEntCache)
	membersHandler := members.NewHandler(db, log, redis, hub, rbacResolver, auditWriter)
	// A kick/leave/ban deletes membership but leaves any voice participant on its
	// join-time snapshot — recheck evicts them from the room (CV-CAN-007 P1).
	membersHandler.SetVoiceEnforcer(voicePermEnforcer)
	// Message-purge engine (#1352): batched bulk delete + attachment reaper.
	// The reaper's worker + straggler sweeper are process-lifetime background
	// loops, started here like hub.Run above.
	purgeReaper := purge.NewReaper(db, log, store)
	purgeEngine := purge.NewEngine(db, log, purgeReaper, cfg.PurgeMaxBatch)
	go purgeReaper.StartWorker(context.Background())
	go purgeReaper.SweepStragglers(context.Background())

	purgeRateLimit, purgeRateWindow := resolvePurgeRateLimit(cfg)
	messagesHandler := messages.NewHandler(db, log, hub, rbacResolver, entCache, purgeEngine, opsCounters)
	// #1353: give the members handler the purge capability for optional purge-on-ban/kick,
	// sharing the standalone purge endpoint's fail-closed rate-limit config (Codex P2 review).
	membersHandler.SetServerMessagePurger(messagesHandler, purgeRateLimit, purgeRateWindow)
	invitesHandler := invites.NewHandler(db, log, hub, rbacResolver)
	voiceHandler := voice.NewHandler(voice.HandlerDeps{
		DB:          db,
		Log:         log,
		Hub:         hub,
		Cfg:         cfg,
		Resolver:    rbacResolver,
		NATS:        natsClient,
		Audit:       auditWriter,
		EntCache:    entCache,
		ServerTiers: serverEntCache,
	})
	// #2445: the REST RevokeTempAccess path owns its own tempGrantManager, so it
	// needs the shared executor forwarded explicitly. revokeTemporaryChannelAccess
	// is one function but three manager instances reach it (this handler, the NATS
	// subscriber, and the nightly sweep); an unwired owner revokes with no capture
	// and no clear, which is the disclosure this issue closes.
	voiceHandler.SetPresenceRecheck(presenceRecheckExecutor)
	// The #2448 durable active-category rail. ONE instance: the reconciler owns
	// the claim/ack loop, so a second would have two pass loops racing for the
	// same rows. presenceHistoryService is its gate — both rails share ONE gate
	// array, because a duplicated array would not be a gate — and it is provably
	// non-nil here for the same reason graphPresenceCapture's rail is:
	// bindPresenceHistoryRuntime above returns an error on a nil service.
	activePlanRail, activePlanReconciler := buildActivePlanRail(
		db, presenceHistoryService, activityStore, hub, log)

	// mfaHandler implements mfa.Verifier — the DM purge step-up gate (#1352).
	dmHandler := dm.NewHandler(dm.HandlerDeps{
		DB:          db,
		Log:         log,
		Hub:         hub,
		Cfg:         cfg,
		NATS:        natsClient,
		Redis:       redis,
		EntCache:    entCache,
		PurgeEngine: purgeEngine,
		MFAVerifier: mfaHandler,
		ActivePlans: activePlanRail,
	})
	// Wire DM voice ring cleanup-on-disconnect (#1209 plan task B7 Part 2).
	// When a user's last WS connection drops, the hub invokes
	// HandleUserDisconnect to cancel any rings they initiated.
	hub.SetDMRingCanceller(dmHandler.HandleUserDisconnect)
	friendsHandler := friends.NewHandler(db, log, hub)

	// Pre-mutation Rich Presence capture for graph-destroying writes (#2446). It
	// imports presence; friends and users import neither, via the presencecapture
	// leaf. Separate from the #2445 executor by design: that family's durability
	// belongs to #2635 and must not be conflated with this one.
	graphPresenceCapture := graphpresence.New(db, activityService, hub, senderPresence, log)

	// The durable Custom Status (C2) leg. It rides the #2419 topology rail that
	// presenceHistoryService already owns — no second outbox, table, stream, or
	// dispatcher. Wiring it here, at ONE construction site, is what lets the
	// durable terminal be swapped in without rewriting a single hook.
	//
	// presenceHistoryService is provably non-nil by this line:
	// bindPresenceHistoryRuntime above returns an error on a nil service and
	// NewRouter returns it. That matters more than it looks — a typed nil
	// *Service would still satisfy the TopologyRail interface, so
	// HasTopologyRail would answer true and the guard below would pass on a
	// dead rail. Belt and braces: all three TopologyRail methods now fail
	// closed on a nil receiver, so such a rail errors on first use rather than
	// panicking, and the proof above is no longer the only thing standing
	// between a refactor and a nil dereference.
	graphPresenceCapture.SetTopologyRail(presenceHistoryService)

	friendsHandler.SetGraphPresenceCapture(graphPresenceCapture)
	usersHandler.SetGraphPresenceCapture(graphPresenceCapture)

	// #2447 membership and account-lifecycle consumers. The erasure consumer
	// (users.AccountService) is constructed later, inside buildPrivacyHandler, so
	// requireGraphPresenceCaptureWired has moved to just after that call — a
	// guard that runs before its last consumer exists would pass on a nil.
	membersHandler.SetGraphPresenceCapture(graphPresenceCapture)
	invitesHandler.SetGraphPresenceCapture(graphPresenceCapture)
	serversHandler.SetGraphPresenceCapture(graphPresenceCapture)

	// The additive (hydrate) direction. Deliberately NOT the capture: hydration
	// sits outside the presencecapture contract because it has no minuend.
	membersHandler.SetActivitySnapshots(activitySnapshotService)
	invitesHandler.SetActivitySnapshots(activitySnapshotService)

	requireGraphPresenceFamilyRegistry(log)

	// Both presence workers own a goroutine and a queue, and NOTHING called
	// either Close: a graceful shutdown left queued plans undrained, so the
	// fail-closed abandons that exist to clear revoked state never ran. The
	// #2445 executor had the identical gap, so one closer covers both (PR
	// #2738 review, CodeRabbit).
	//
	// The caller must run this BEFORE hub.Shutdown and after the HTTP drain:
	// the drain's abandons disconnect through the hub, so closing the hub
	// first would silently discard exactly the work the drain exists to do.
	closePresenceWorkers := func() {
		graphPresenceCapture.Close()
		presenceRecheckExecutor.Close()
	}

	feedbackHandler := buildFeedbackHandler(cfg, log)
	notificationsHandler := notifications.NewHandler(db, log)
	ownershipHandler := ownership.NewHandler(ownership.HandlerDeps{
		DB:          db,
		Log:         log,
		Hub:         hub,
		Redis:       redis,
		Cache:       permCache,
		Audit:       auditWriter,
		EmailSvc:    emailSvc,
		MFAVerifier: mfaHandler,
	})
	// Ownership changes are the largest single permission delta (owner
	// short-circuit) — they must push voice rechecks too (CV-CAN-007 P1).
	ownershipHandler.SetVoiceEnforcer(voicePermEnforcer)
	var mediaHandler *media.Handler
	if store != nil {
		mediaHandler = media.NewHandler(db, store, log, cfg, rbacResolver, entCache, serverEntCache)
		mediaHandler.SetOpsCounter(opsCounters)
		usersHandler.SetMediaStore(store)
		serversHandler.SetMediaStore(store)
	}
	wsHandler := websocket.NewHandler(hub, db, redis, cfg.JWTSecret, cfg.AllowedOrigins,
		credFence, middleware.SecurityHeaderSet(cfg.Environment, cfg.HSTSHeaderValue))
	wsTicketHandler := auth.NewWSTicketHandler(redis, cfg.JWTSecret)
	clientConfigHandler := clientconfig.NewHandler(cfg, liveSpa, log)
	serverCapabilitiesHandler := servercapabilities.NewHandler(cfg)
	updatesHandler := updates.NewHandler(cfg, log)
	privacyHandler, accountService := buildPrivacyHandler(
		db, redis, log, usersHandler, hub, graphPresenceCapture, natsClient)

	// The erasure drain (#2448). Wired here rather than beside the DM rail for
	// the same reason the guard below runs here: accountService does not exist
	// until buildPrivacyHandler returns on the line above.
	accountService.SetActivePlanRail(activePlanRail)

	// Runs here, not beside the other wiring: accountService is the last #2447
	// consumer and is constructed on the line above.
	requireGraphPresenceCaptureWired(
		log, activityService, graphPresenceCapture, activePlanRail,
		graphPresenceConsumers{
			friends: friendsHandler,
			users:   usersHandler,
			members: membersHandler,
			invites: invitesHandler,
			servers: serversHandler,
			dm:      dmHandler,
			erasure: accountService,
		})
	oauthHandler := buildOAuthHandler(db, redis, cfg, authHandler, log)

	// Client attestation (#677, ADR-0010). When REQUIRE_CLIENT_ATTESTATION=false
	// (self-hosted default), we still wire the surface so the verify endpoint
	// is callable but the RequireAttestation middleware is a pass-through.
	// When true, the OIDC verifier is constructed eagerly and a failed
	// discovery (network down at startup) is treated as fatal — that matches
	// the fail-closed posture required by D2.
	attestationHandler := buildAttestationHandler(db, redis, natsClient, cfg, log)

	// Age-verification claim handler (#1623). hub satisfies age.SessionDisconnector
	// for the terminal-disable live-session kick on valid_age=false.
	ageHandler := buildAgeHandler(db, redis, hub, log)

	// Entitlement capability set handler (#1297). Owns its own read-through Cache
	// (NOT borrowed from auth.Handler — internal/auth is a protected path).
	entitlementsHandler := entitlements.NewHTTPHandlerForInstance(db, redis, log, cfg.InstanceType)

	// Subscription-status read handler (#1304). Read-only companion to the
	// entitlement set: exposes the live subscription's status/source/expiry that
	// the Settings subscription page renders. Fails closed to the free default on
	// any DB error (never a fabricated premium).
	subscriptionsHandler := subscriptions.NewHandler(db, log)

	// Redemption engine + issuer (#1303). The first LIVE caller of the
	// entitlements.OnTierChange convergence point: a premium code grant
	// invalidates the user's tier cache and pushes entitlements_changed via the
	// EntitlementNotifier (built here from the hub + the shared entCache). The
	// admin generation endpoint is gated by REDEMPTION_ADMIN_TOKEN; empty
	// disables it (503).
	redemptionEntNotifier := NewEntitlementNotifier(hub, log)
	redemptionHandler := buildRedemptionHandler(db, entCache, redemptionEntNotifier, cfg, log)

	// Start NATS voice event subscriber
	if natsClient != nil {
		voiceSub := voice.NewNATSSubscriber(db, log, hub, natsClient, redis, rbacResolver, activityService)
		// Close the join-vs-mutation race: re-push fresh permissions when a
		// voice.joined lands (CV-CAN-007 P1).
		voiceSub.SetPermissionEnforcer(voicePermEnforcer)
		// Temporary-SBAC revoke shares the one #2445 executor with the RBAC
		// handler, so a presence-triggered revoke captures the same way an
		// authority write does.
		voiceSub.SetPresenceRecheck(presenceRecheckExecutor)
		if subErr := voiceSub.Subscribe(); subErr != nil {
			log.Error("Failed to subscribe to voice NATS events", "error", subErr)
		} else {
			voicePermEnforcer.AddCloseHook(voiceSub.Close)
		}
	}
	// API v1 routes
	v1 := router.Group("/api/v1")
	{
		// Auth routes with rate limiting + IP-based auth failure ban check
		authRoutes := v1.Group("/auth")
		authRoutes.Use(middleware.AuthBanCheck(redis))
		{
			// Register: 5 attempts per 15 minutes in production (prevent spam
			// account creation). Relaxed under CONCORD_ENV=test so the Playwright
			// e2e suite's ~18 single-IP registrations don't trip the cap. See
			// #1274 and authFlowTestRateLimit().
			authRoutes.POST("/register",
				middleware.RateLimitByIP(redis, authFlowTestRateLimit(5), 15*time.Minute),
				authHandler.Register,
			)

			// Register confirm: 10 attempts per 15 minutes (verify email code,
			// promote pending_registration -> user). See #621. Relaxed under
			// CONCORD_ENV=test alongside /register — the e2e suite also drives
			// ~18 confirmations. See #1274.
			authRoutes.POST("/register/confirm",
				middleware.RateLimitByIP(redis, authFlowTestRateLimit(10), 15*time.Minute),
				authHandler.ConfirmRegistration,
			)

			// Resend verification code: 20 attempts per 15 minutes. See #621.
			authRoutes.POST("/register/resend",
				middleware.RateLimitByIP(redis, 20, 15*time.Minute),
				authHandler.ResendRegistrationCode,
			)

			// Change email mid-registration: 10 attempts per 15 minutes. See #621.
			authRoutes.POST("/register/change-email",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				authHandler.ChangeRegistrationEmail,
			)

			// Abandon pending registration: 20 attempts per 15 minutes. See #621.
			authRoutes.DELETE("/register/:pending_id",
				middleware.RateLimitByIP(redis, 20, 15*time.Minute),
				authHandler.AbandonRegistration,
			)

			// Login: 10 attempts per 15 minutes in production (prevent brute force).
			// Relaxed under CONCORD_ENV=test — the e2e suite's registerAndLogin
			// helper logs in once per backend spec (to initialize e2eeService,
			// which registration does not), exceeding 10/15min from one CI IP.
			// See #1274.
			authRoutes.POST("/login",
				middleware.RateLimitByIP(redis, authFlowTestRateLimit(10), 15*time.Minute),
				authHandler.Login,
			)

			// Refresh: 30 attempts per minute (normal usage, but prevent abuse)
			authRoutes.POST("/refresh",
				middleware.RateLimitByIP(redis, 30, 1*time.Minute),
				authHandler.Refresh,
			)

			// Logout: 10 attempts per minute
			authRoutes.POST("/logout",
				middleware.RateLimitByIP(redis, 10, 1*time.Minute),
				authHandler.Logout,
			)

			// MFA verify (unauthenticated — uses challenge token from login)
			authRoutes.POST("/mfa/verify",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				mfaHandler.Verify,
			)

			// MFA email code delivery (unauthenticated — uses challenge token)
			authRoutes.POST("/mfa/email/send",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				mfaHandler.SendEmailMFACode,
			)

			// Account recovery (unauthenticated — uses email verification + recovery tokens)
			authRoutes.POST("/recovery/begin",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.RecoveryBegin,
			)
			authRoutes.POST("/recovery/verify-code",
				middleware.RateLimitByIP(redis, 5, 15*time.Minute),
				authHandler.RecoveryVerifyCode,
			)
			authRoutes.POST("/recovery/reset-password",
				middleware.RateLimitByIP(redis, 5, 15*time.Minute),
				authHandler.RecoveryResetPassword,
			)
			authRoutes.POST("/recovery/reset-account",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.RecoveryResetAccount,
			)

			// Trusted device recovery (unauthenticated — uses recovery token)
			authRoutes.POST("/recovery/device-request",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.CreateDeviceRecoveryRequest,
			)
			authRoutes.GET("/recovery/device-request/:id",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				authHandler.PollDeviceRecoveryRequest,
			)

			// Social recovery (unauthenticated — uses recovery token)
			authRoutes.POST("/recovery/social-request",
				middleware.RateLimitByIP(redis, 3, 15*time.Minute),
				authHandler.CreateSocialRecoveryRequest,
			)
			authRoutes.GET("/recovery/social-request/:id",
				middleware.RateLimitByIP(redis, 10, 15*time.Minute),
				authHandler.PollSocialRecoveryRequest,
			)

			// SSO endpoints (#270). All four are unauthenticated — the user is
			// proving identity via the provider, not via an existing Concord
			// session. Lockout posture matches /auth/login: per-IP rate limits
			// here, plus AuthBanCheck inherited from the parent group.
			//
			// Initiate (10/15min): one click per attempt. Higher than 5 to
			//   accommodate "user closes the browser tab and retries".
			// CompleteRegistration (5/15min): user has just chosen a username
			//   and passphrase; abuse vector is account-spam, so match
			//   /auth/register's 5/15min ceiling.
			// CompleteLink (5/15min): password verification for the
			//   account-link path. Shares the EMAIL-keyed lockout counter via
			//   AuthAdapter.VerifyPassword, but the per-IP cap prevents a
			//   distributed-IP attacker from bypassing the email gate.
			// SignAppleClientSecret (5/1min): per-mint broker for the
			//   client-driven Apple exchange (#971/#972). Tighter window —
			//   each legitimate login needs exactly one mint; 5/min absorbs
			//   retries while capping signing-oracle abuse (CWE-307).
			// ProviderSession (10/15min): terminal step of the client-driven
			//   exchange for all providers (#974 apple, #975 google). Callback
			//   route removed in #975 — all providers are now client-driven.
			ssoRoutes := authRoutes.Group("/sso")
			{
				ssoRoutes.GET("/:provider",
					middleware.RateLimitByIP(redis, 10, 15*time.Minute),
					oauthHandler.Initiate,
				)
				ssoRoutes.POST("/:provider/complete-registration",
					middleware.RateLimitByIP(redis, 5, 15*time.Minute),
					oauthHandler.CompleteRegistration,
				)
				ssoRoutes.POST("/:provider/complete-link",
					middleware.RateLimitByIP(redis, 5, 15*time.Minute),
					oauthHandler.CompleteLink,
				)
				ssoRoutes.POST("/:provider/sign-client-secret",
					middleware.RateLimitByIP(redis, 5, time.Minute),
					oauthHandler.SignAppleClientSecret,
				)
				ssoRoutes.POST("/:provider/session",
					middleware.RateLimitByIP(redis, 10, 15*time.Minute),
					oauthHandler.ProviderSession,
				)
			}
		}

		// Client config (public — pre-auth, clients need this before login)
		v1.GET("/client/config",
			middleware.RateLimitByIP(redis, 30, 1*time.Minute),
			clientConfigHandler.GetConfig,
		)

		// Server capabilities (public — pre-auth discovery; clients clamp their
		// feature surface to this before/at login). Rate-limited at 30/min/IP to
		// match the sibling /client/config: this is the FIRST pre-auth request a
		// client makes, and self-hosted/corporate deployments (this endpoint's
		// primary driver, #1615) commonly egress many clients through one NAT IP,
		// so a tighter budget would 429 the (N>cap)th simultaneous launcher and
		// block login. The descriptor is constant and auth-state-independent, so
		// there is nothing to enumerate; the limit is pure abuse/DoS throttling,
		// for which parity with the more-sensitive, less-cacheable /client/config
		// is the right calibration (#662).
		v1.GET("/server/capabilities",
			middleware.RateLimitByIP(redis, 30, 1*time.Minute),
			serverCapabilitiesHandler.GetCapabilities,
		)

		// Desktop update assets (public — electron-updater needs this pre-auth)
		v1.GET("/updates/*filename",
			middleware.RateLimitByIP(redis, 30, 1*time.Minute),
			updatesHandler.ServeUpdateAsset,
		)

		// Public invite preview/card routes for invite.concordvoice.chat. The
		// preview route is intentionally privacy-trimmed and returns a uniform
		// invalid shape for missing, malformed, expired, revoked, and maxed invites.
		v1.GET("/invites/:code/preview",
			middleware.RateLimitByIP(redis, 20, 1*time.Minute),
			middleware.RateLimitGlobal(redis, "ratelimit:global:invite-preview", 2000, 1*time.Minute),
			invitesHandler.GetPublicInvitePreview,
		)
		v1.GET("/invites/:code/icon",
			middleware.RateLimitByIP(redis, 60, 1*time.Minute),
			publicInviteIconHandler(invitesHandler, mediaHandler),
		)

		// Public friend-code preview/avatar routes for invite.concordvoice.chat.
		// Same privacy posture as the invite pair above: a uniform invalid shape
		// for every failure class, and no user_id anywhere — which is why the
		// avatar is keyed by code rather than by the owner's UUID (#945).
		v1.GET("/friends/codes/:code/preview",
			middleware.RateLimitByIP(redis, 20, 1*time.Minute),
			middleware.RateLimitGlobal(redis, "ratelimit:global:friend-code-preview", 2000, 1*time.Minute),
			friendsHandler.GetPublicFriendCodePreview,
		)
		v1.GET("/friends/codes/:code/avatar",
			middleware.RateLimitByIP(redis, 60, 1*time.Minute),
			publicFriendAvatarHandler(friendsHandler, mediaHandler),
		)

		// Tier 1 user image proxy GETs (public — required so <img> tags can
		// render without an Authorization header). The opaque user UUID is the
		// only identifier; avatars/banners are intentionally shareable, so the
		// previous auth gate added no real protection while breaking rendering.
		// Uploads + deletes remain authenticated. Rate-limited per IP since
		// there is no user context.
		//
		// server-icons, server-banners, and dm-icons are also public Tier 1
		// media: the unguessable UUID is the only identifier and they need to
		// render via plain <img> tags without an Authorization header.
		// Membership checks have been removed from their handlers.
		if mediaHandler != nil {
			v1.GET("/media/avatars/:user_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyAvatar,
			)
			v1.GET("/media/banners/:user_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyBanner,
			)
			v1.GET("/media/server-icons/:server_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyServerIcon,
			)
			v1.GET("/media/server-banners/:server_id",
				middleware.RateLimitByIP(redis, 120, 1*time.Minute),
				mediaHandler.ProxyServerBanner,
			)
			v1.GET("/media/dm-icons/:conversationId",
				middleware.RateLimitByIP(redis, 60, 1*time.Minute),
				mediaHandler.ProxyDMIcon,
			)
		} else {
			// Match the protected fallback below so misconfiguration fails
			// with a clear 503 instead of a 404 for all public Tier 1 routes.
			mediaUnavailable := func(c *gin.Context) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "media storage not configured"})
			}
			v1.GET("/media/avatars/:user_id", mediaUnavailable)
			v1.GET("/media/banners/:user_id", mediaUnavailable)
			v1.GET("/media/server-icons/:server_id", mediaUnavailable)
			v1.GET("/media/server-banners/:server_id", mediaUnavailable)
			v1.GET("/media/dm-icons/:conversationId", mediaUnavailable)
		}

		// Client attestation routes (#677). Verify takes the configured TTL via
		// closure since gin.HandlerFunc has a fixed signature.
		v1.POST("/attestation/verify",
			middleware.AuthRequired(cfg.JWTSecret, redis, credFence),
			func(c *gin.Context) { attestationHandler.Verify(c, cfg.AttestationTokenTTL) },
		)
		// Internal CI publish endpoints (Y1 split — #677 R3 reconciliation).
		// Two axes, two endpoints, each with its own OIDC-authed shape:
		//
		//   /publish/spa     — body {spa_version, html_hash}, posted by main-cd.yml
		//   /publish/binary  — body {version, platform, cert_hash}, posted by build-desktop.yml
		//
		// Both share the same OIDC verifier (workflow ref / audience / subject).
		// Authentication is delegated to each handler — no upstream auth middleware.
		//
		// NOTE: revoke endpoint is intentionally deferred until an admin-auth
		// middleware exists; the handler is built and tested but not wired
		// (operators can revoke via direct DB + Redis until then).
		v1.POST("/internal/attestation/publish/spa", attestationHandler.PublishSPA)
		v1.POST("/internal/attestation/publish/binary", attestationHandler.PublishBinary)

		// Protected routes — split into two tiers:
		// pendingOK: authenticated but email may be unverified (verification, logout, basic profile)
		// verified:  authenticated AND email verified (everything else)
		//
		// RequireAttestation is layered AFTER AuthRequired so the user_id is in
		// context when the attestation gate runs (per the middleware's contract
		// at internal/middleware/attestation.go). When cfg.RequireClientAttestation
		// is false (self-hosted default) the gate is a pass-through no-op, so
		// existing routes are unaffected. When true (hosted concordvoice.chat
		// deployment), every authenticated route is gated on a valid signed
		// attestation token bound to (session_id, machine_id). Per ADR-0010 and
		// finding #BLOCK-1 of the #1264 review (the middleware was previously
		// defined and unit-tested but never registered on any route).
		authRequired := v1.Group("/")
		authRequired.Use(middleware.AuthRequired(cfg.JWTSecret, redis, credFence))
		authRequired.Use(middleware.RequireAttestation(cfg.RequireClientAttestation, redis, log))

		// ── Pending-OK routes (unverified email allowed) ──────────────
		{
			// Note: logout is in the public authRoutes group above (uses refresh token, not Bearer)

			// Basic profile read (needed by frontend to check verification status)
			authRequired.GET("/users/me",
				middleware.RateLimitByUser(redis, 30, 1*time.Minute),
				usersHandler.GetMe,
			)

			// Entitlement capability set (client UX source; #1297). Auth-required;
			// fails closed to the free set on any resolve error.
			authRequired.GET("/entitlements", entitlementsHandler.Get)

			// Subscription status (Settings subscription page; #1304). Read-only,
			// auth-required, pending-OK (mirrors /entitlements so the page renders
			// before email verification). Always 200; fails closed to the free
			// default on any DB error.
			authRequired.GET("/subscriptions/me",
				middleware.RateLimitByUser(redis, 30, 1*time.Minute),
				subscriptionsHandler.GetMe,
			)

			// Privacy endpoints — GDPR Article 17 erasure.
			// Mounted in the pending-OK tier so users can erase their data
			// even before email verification completes.
			privacyRoutes := authRequired.Group("/privacy")
			{
				privacyRoutes.POST("/erase-account",
					middleware.RateLimitByUser(redis, 3, 24*time.Hour),
					privacyHandler.EraseAccount,
				)
			}
		}

		// ── Verified routes (email must be verified) ──────────────────
		protected := authRequired.Group("/")
		protected.Use(middleware.RequireVerifiedEmail())
		{
			// WebSocket ticket (short-lived, single-use)
			protected.POST("/auth/ws-ticket",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				wsTicketHandler.IssueTicket,
			)

			// Age-verification claim ingest (#1623). Identity-blind: stores only
			// booleans + a jurisdiction integer, verifies the client RSA-PSS
			// signature, and terminally disables the account on valid_age=false.
			protected.PUT("/age/claim",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				ageHandler.SubmitClaim,
			)

			// Age-verification status read (#1763). Read-back companion to the
			// claim ingest so the client rehydrates the verified state on mount
			// instead of re-prompting for DOB. JWT-scoped single-row lookup,
			// returns only the eligibility booleans (identity-blind). Generous
			// per-user limit — a settings panel mounts rarely, but the cap bounds
			// abuse of an authenticated read.
			protected.GET("/age/status",
				middleware.RateLimitByUser(redis, 30, 1*time.Minute),
				ageHandler.GetStatus,
			)

			// Feedback (#158): bug report / feature request submission. This
			// is the one privileged-PAT write to a PUBLIC GitHub repo, so the
			// rate limiters are the SOLE velocity controls — they fail CLOSED
			// (a Redis blip must not remove the flood cap). Two layers:
			//   1. RateLimitByUserFailClosed — per-user 10/hour. Fail-closed
			//      variant of RateLimitByUser; 503 (not allow) on Redis error.
			//      Runs FIRST so a single account's over-quota requests are
			//      rejected here BEFORE they can touch the global counter,
			//      bounding any one account's contribution to the aggregate to
			//      its own 10/hour.
			//   2. GlobalRateLimit — aggregate cap across ALL users
			//      (ratelimit:global:feedback) so N Sybil accounts can't
			//      multiply the public-tracker flood ceiling by N. 429 over
			//      cap, 503 on Redis error. Runs SECOND, counting only requests
			//      that already passed the per-user gate.
			// ORDER IS LOAD-BEARING: with the global limiter first, one
			// account's per-user-REJECTED requests would still INCR the global
			// counter and could drive it to the cap, locking out every other
			// user — a single-account DoS (Gitar + security-reviewer finding on
			// PR #1591). Per-user-first closes that while preserving the
			// Sybil-defense intent (N accounts each add at most 10). Do NOT
			// swap these back.
			// 10/hour/user was bumped from the original 3/hour per a Gitar
			// finding; the counter increments BEFORE the handler runs, so
			// failed validations also burn quota; 10/hour leaves slack for
			// honest users to recover from client-side typos while still
			// capping spam. Bug + feature share the same bucket on purpose.
			protected.POST("/feedback",
				middleware.RateLimitByUserFailClosed(redis, 10, 1*time.Hour),
				feedback.GlobalRateLimit(redis),
				feedbackHandler.Submit,
			)

			// Redemption (#1303). POST /api/v1/redeem — generic code redemption
			// for the authenticated user. Two rate-limit layers bound abuse +
			// enumeration: per-user (10/min) AND per-IP (20/min). The per-IP
			// layer caps a single host enumerating across many accounts; the
			// 130-bit code entropy makes guessing infeasible even unthrottled,
			// so these are anti-abuse, not the primary defense. Failed attempts
			// are logged PII-safe by the handler (outcome category + sanitized
			// user_id only — never the code value/hash).
			protected.POST("/redeem",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				middleware.RateLimitByIP(redis, 20, 1*time.Minute),
				redemptionHandler.Redeem,
			)

			// Admin code generation (#1303). POST /api/v1/admin/redemption/codes.
			// Gated by AdminGate (X-Admin-Token shared secret, constant-time
			// compared) BEFORE the handler runs — the INTERIM issuer-authz
			// primitive (no platform-admin RBAC role exists; see
			// redemption.Handler.AdminGate + the PR description's flagged gap).
			// REDEMPTION_ADMIN_TOKEN empty → AdminGate returns 503 (endpoint
			// disabled; CLI issuance still works). Rate-limited per-user as a
			// belt-and-suspenders cap on the privileged surface.
			adminRedemption := protected.Group("/admin/redemption")
			adminRedemption.Use(redemptionHandler.AdminGate())
			{
				adminRedemption.POST("/codes",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					redemptionHandler.Generate,
				)
			}

			// MFA management routes (authenticated)
			mfaRoutes := protected.Group("/mfa")
			{
				mfaRoutes.GET("/status",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetStatus,
				)

				// TOTP enrollment
				mfaRoutes.POST("/totp/setup",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.TOTPSetup,
				)
				mfaRoutes.POST("/totp/verify-setup",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.TOTPVerifySetup,
				)
				mfaRoutes.POST("/totp/confirm-setup",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.TOTPConfirmSetup,
				)
				mfaRoutes.POST("/totp/disable",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.TOTPDisable,
				)

				// Backup codes
				mfaRoutes.POST("/backup-codes/regenerate",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.RegenerateBackupCodes,
				)

				// WebAuthn credential management
				mfaRoutes.POST("/webauthn/register/begin",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.WebAuthnRegisterBegin,
				)
				mfaRoutes.POST("/webauthn/register/finish",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.WebAuthnRegisterFinish,
				)
				mfaRoutes.GET("/webauthn/credentials",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.WebAuthnListCredentials,
				)
				mfaRoutes.DELETE("/webauthn/credentials/:id",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.WebAuthnDeleteCredential,
				)

				// WebAuthn inline verify (for MFA verification on protected operations)
				mfaRoutes.POST("/webauthn/verify-inline/begin",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.WebAuthnVerifyInlineBegin,
				)
				mfaRoutes.POST("/webauthn/verify-inline/finish",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.WebAuthnVerifyInlineFinish,
				)

				// Recovery-only method settings
				mfaRoutes.PUT("/recovery-only",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.SetRecoveryOnly,
				)

				// Hardened recovery mode (dual-channel Email+SMS)
				mfaRoutes.PUT("/recovery-hardened",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.SetRecoveryHardened,
				)

				// Email MFA setup, with SMS blocked in production until provider integration
				mfaRoutes.POST("/email-sms/setup",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.EmailSmsSetup,
				)
				mfaRoutes.POST("/email-sms/verify",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.EmailSmsVerify,
				)
				mfaRoutes.POST("/email-sms/disable",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.EmailSmsDisable,
				)
				mfaRoutes.GET("/email-sms/status",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.EmailSmsStatus,
				)

				// Backup email for recovery
				mfaRoutes.GET("/backup-email",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetBackupEmail,
				)
				mfaRoutes.PUT("/backup-email",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.SetBackupEmail,
				)

				// Recovery key management
				mfaRoutes.PUT(routeRecoveryKey,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.StoreRecoveryKey,
				)
				mfaRoutes.GET(routeRecoveryKey,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetRecoveryKeyStatus,
				)
				mfaRoutes.DELETE(routeRecoveryKey,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.DeleteRecoveryKey,
				)

				// Trusted device management
				mfaRoutes.GET("/trusted-devices",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.ListTrustedDevices,
				)
				mfaRoutes.POST("/trusted-devices",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.DesignateTrustedDevice,
				)
				mfaRoutes.DELETE("/trusted-devices/:id",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.RemoveTrustedDevice,
				)

				// Recovery request management (authenticated user responds)
				mfaRoutes.GET("/recovery-requests",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.ListRecoveryRequests,
				)
				mfaRoutes.POST("/recovery-requests/:id/respond",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.RespondToRecoveryRequest,
				)

				// Recovery circle management (Shamir's Secret Sharing)
				mfaRoutes.GET(routeRecoveryCircle,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetRecoveryCircle,
				)
				mfaRoutes.PUT(routeRecoveryCircle,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.UpsertRecoveryCircle,
				)
				mfaRoutes.DELETE(routeRecoveryCircle,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					mfaHandler.DeleteRecoveryCircle,
				)
				mfaRoutes.GET("/recovery-circle/shares",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.GetMyRecoveryShares,
				)

				// Social recovery requests (contacts respond)
				mfaRoutes.GET("/recovery-requests/social",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					mfaHandler.ListSocialRecoveryRequests,
				)
				mfaRoutes.POST("/recovery-requests/social/:id/respond",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					mfaHandler.RespondToSocialRecovery,
				)
			}

			// User routes (GET /users/me is in the pendingOK group above)
			userRoutes := protected.Group("/users")
			{
				// Update profile (10 requests per minute)
				userRoutes.PATCH("/me",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdateMe,
				)

				// Get E2EE keys (for password change re-wrapping)
				userRoutes.GET("/me/keys",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					usersHandler.GetMyKeys,
				)

				// Replace E2EE keys (for key recovery)
				userRoutes.PUT("/me/keys",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					usersHandler.ReplaceMyKeys,
				)

				// Change password (5 requests per minute - sensitive)
				userRoutes.POST("/me/password",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					usersHandler.ChangePassword,
				)

				// Get user's public key (for E2EE key wrapping)
				userRoutes.GET("/:user_id/public-key",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPublicKey,
				)

				// Get user's public profile (for viewing other users)
				userRoutes.GET("/:user_id/profile",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPublicProfile,
				)

				// #1240: would a friend request from the caller be accepted?
				// Registered on userRoutes for the :user_id path position but
				// handled by friendsHandler, so this buys the position without
				// creating a users -> friends import edge (precedent:
				// friendsHandler.GetPublicFriendCodePreview on the v1 group).
				//
				// 10/min, not the 30/min user-scoped-read convention: a read
				// ABOUT a third party's privacy preference must not be cheaper
				// than POST /friends/request, the action it predicts. The key is
				// per-FullPath, so this never consumes the send budget.
				userRoutes.GET("/:user_id/friend-request-eligibility",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					friendsHandler.GetFriendRequestEligibility,
				)

				// Encrypted user preferences (cross-device sync)
				userRoutes.GET("/me/preferences",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPreferences,
				)
				userRoutes.PUT("/me/preferences",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.UpdatePreferences,
				)

				// Encrypted saved GIFs (cross-device sync)
				userRoutes.GET("/me/saved-gifs",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetSavedGifs,
				)
				userRoutes.PUT("/me/saved-gifs",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdateSavedGifs,
				)

				// Encrypted friend organization (categories, zero-knowledge, cross-device sync) — #324
				userRoutes.GET("/me/friend-organization",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetFriendOrganization,
				)
				userRoutes.PUT("/me/friend-organization",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdateFriendOrganization,
				)

				// Search users by username/display name
				userRoutes.GET("/search",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.SearchUsers,
				)

				// Privacy settings
				userRoutes.GET("/me/privacy",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPrivacySettings,
				)
				userRoutes.PATCH("/me/privacy",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdatePrivacySettings,
				)

				// Presence settings — custom text status (issue #1233)
				userRoutes.GET("/me/presence-settings",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPresenceSettings,
				)
				userRoutes.PATCH("/me/presence-settings",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.UpdatePresenceSettings,
				)
				userRoutes.GET("/me/presence-overrides/:category",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetPresenceOverrides,
				)
				userRoutes.PUT("/me/presence-overrides/:category",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					usersHandler.ReplacePresenceOverrides,
				)
				presenceHistoryHandler.RegisterRoutes(
					userRoutes,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				)

				// SSO settings (issue #270)
				userRoutes.GET("/me/sso-identities",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.ListSSOIdentities,
				)
				userRoutes.GET("/me/security",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					usersHandler.GetSecurity,
				)
				userRoutes.PATCH("/me/security",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					usersHandler.PatchSecurity,
				)
				userRoutes.DELETE("/me/sso-identities/:provider",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					usersHandler.DeleteSSOIdentity,
				)
			}

			// Session management routes
			sessionRoutes := protected.Group("/sessions")
			{
				// List all active sessions (10 requests per minute)
				sessionRoutes.GET("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					sessionsHandler.ListSessions,
				)

				// Revoke a specific session (10 requests per minute)
				sessionRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					sessionsHandler.RevokeSession,
				)

				// Revoke all sessions (5 requests per minute - requires password re-verification)
				sessionRoutes.POST("/revoke-all",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					sessionsHandler.RevokeAllSessions,
				)

				// Toggle revocation mode (5 requests per minute - requires password re-verification)
				sessionRoutes.PUT("/revocation-mode",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					sessionsHandler.UpdateRevocationMode,
				)
			}

			// Server routes
			serverRoutes := protected.Group("/servers")
			{
				// List user's servers (30 requests per minute)
				serverRoutes.GET("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					serversHandler.ListServers,
				)

				// Server-wide bulk message purge (#1352) — destructive; fail-CLOSED
				// rate limit. Authorization is re-resolved per channel inside the
				// handler (channel-scoped denies are honored — review finding M1).
				serverRoutes.DELETE("/:id/messages",
					middleware.RateLimitByUserFailClosed(redis, purgeRateLimit, purgeRateWindow),
					messagesHandler.PurgeServer,
				)

				// Get unread status across all user's servers (30 requests per minute)
				serverRoutes.GET("/unread-status",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetServerUnreadStatus,
				)

				// Create server (10 requests per minute)
				serverRoutes.POST("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					serversHandler.CreateServer,
				)

				// Get specific server (30 requests per minute)
				serverRoutes.GET("/:id",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					serversHandler.GetServer,
				)

				// Get a server's entitlement set (server-axis, #1521; 30/min)
				serverRoutes.GET("/:id/entitlements",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					serversHandler.GetServerEntitlements,
				)

				// Update server (10 requests per minute)
				serverRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					serversHandler.UpdateServer,
				)

				// Delete server (5 requests per minute - destructive action)
				serverRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					serversHandler.DeleteServer,
				)

				// List channels in a server (30 requests per minute)
				serverRoutes.GET("/:id/channels",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.ListChannels,
				)

				// List members in a server (30 requests per minute)
				serverRoutes.GET(pathIDMembers,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					membersHandler.ListMembers,
				)

				// List member public keys for E2EE channel wrapping (30 requests per minute)
				serverRoutes.GET("/:id/member-public-keys",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					membersHandler.ListMemberPublicKeys,
				)

				// Add member to server (10 requests per minute)
				serverRoutes.POST(pathIDMembers,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					membersHandler.AddMember,
				)

				// Update member role (5 requests per minute - sensitive operation)
				serverRoutes.PATCH(pathIDMembersUserID,
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.UpdateMember,
				)

				// Remove member from server (5 requests per minute - sensitive operation)
				serverRoutes.DELETE(pathIDMembersUserID,
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.RemoveMember,
				)

				// Timeout member (5 requests per minute - sensitive moderation action)
				serverRoutes.POST(pathIDMembersTimeout,
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.TimeoutMember,
				)
				serverRoutes.DELETE(pathIDMembersTimeout,
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.RemoveTimeout,
				)

				// Ban management
				serverRoutes.POST("/:id/bans/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.BanMember,
				)
				serverRoutes.DELETE("/:id/bans/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					membersHandler.UnbanMember,
				)
				serverRoutes.GET("/:id/bans",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					membersHandler.ListBans,
				)

				// Create invite for server (10 requests per minute)
				serverRoutes.POST("/:id/invites",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					invitesHandler.CreateInvite,
				)

				// List invites for server (30 requests per minute)
				serverRoutes.GET("/:id/invites",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					invitesHandler.ListInvites,
				)

				// Revoke invite (10 requests per minute)
				serverRoutes.DELETE("/:id/invites/:invite_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					invitesHandler.RevokeInvite,
				)

				// Channel group routes
				serverRoutes.GET("/:id/channel-groups",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.ListChannelGroups,
				)
				serverRoutes.POST("/:id/channel-groups",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.CreateChannelGroup,
				)
				serverRoutes.PATCH("/:id/channel-groups/:group_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.UpdateChannelGroup,
				)
				serverRoutes.DELETE("/:id/channel-groups/:group_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					channelsHandler.DeleteChannelGroup,
				)

				// Bulk reorder channels (drag-and-drop between groups)
				serverRoutes.PUT("/:id/channels/reorder",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.ReorderChannels,
				)

				// Get unread counts for all channels in a server (30 requests per minute)
				serverRoutes.GET("/:id/unread",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetUnreadCounts,
				)

				// Mark all channels in a server as read (30 requests per minute)
				serverRoutes.POST(pathIDRead,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.MarkServerRead,
				)

				// Get the caller's mute preferences for this server (the server
				// itself plus any channel-level prefs for channels in it).
				// 30 requests per minute matches the unread-states sibling.
				serverRoutes.GET("/:id/mute-states",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					notificationsHandler.GetServerMuteStates,
				)

				// RBAC: Role management
				serverRoutes.GET("/:id/roles",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbac.RequireMembership(rbacResolver),
					rbacHandler.ListRoles,
				)
				serverRoutes.POST("/:id/roles",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.CreateRole,
				)
				// Register /reorder BEFORE /:role_id to prevent Gin matching "reorder" as a wildcard
				serverRoutes.PATCH("/:id/roles/reorder",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.ReorderRoles,
				)
				serverRoutes.PATCH("/:id/roles/:role_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.UpdateRole,
				)
				serverRoutes.DELETE("/:id/roles/:role_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRoles, ""),
					rbacHandler.DeleteRole,
				)

				// RBAC: Role assignment
				serverRoutes.POST("/:id/members/:user_id/roles",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRolesAssign, ""),
					rbacHandler.AssignRole,
				)
				serverRoutes.DELETE("/:id/members/:user_id/roles/:role_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermManageRolesAssign, ""),
					rbacHandler.UnassignRole,
				)

				// RBAC: Computed server permissions
				serverRoutes.GET("/:id/permissions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbac.RequireMembership(rbacResolver),
					rbacHandler.GetMyServerPermissions,
				)

				// RBAC: Audit log
				serverRoutes.GET("/:id/audit-log",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbac.RequirePermission(rbacResolver, rbac.PermViewAuditLog, ""),
					rbacHandler.GetAuditLog,
				)

				// Ownership transfer
				serverRoutes.POST(routeTransferOwnership,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					ownershipHandler.InitiateTransfer,
				)
				serverRoutes.GET(routeTransferOwnership,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					ownershipHandler.GetTransferStatus,
				)
				serverRoutes.DELETE(routeTransferOwnership,
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					ownershipHandler.CancelTransfer,
				)
				// Register /confirm BEFORE transfer-ownership could match as wildcard
				serverRoutes.POST("/:id/transfer-ownership/confirm",
					middleware.RateLimitByUser(redis, 3, 1*time.Minute),
					ownershipHandler.ConfirmTransfer,
				)

				// Server-enforced voice moderation (#488)
				serverRoutes.POST(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerMute,
				)
				serverRoutes.DELETE(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerUnmute,
				)
				serverRoutes.POST(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerDeafen,
				)
				serverRoutes.DELETE(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerUndeafen,
				)
				serverRoutes.POST("/:id/voice/:userId/user-mute",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.UserMute,
				)
				serverRoutes.POST("/:id/voice/:userId/user-deafen",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.UserDeafen,
				)
				// Move a member to another voice channel (#487 Scope B). Same
				// per-user rate limit as the mute/deafen moderation routes.
				serverRoutes.POST(routeVoiceMove,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerMove,
				)
				// Force-disconnect a member from voice (#487 P3). Same per-user
				// rate limit as the mute/deafen/move moderation routes.
				serverRoutes.POST(routeVoiceDisconnect,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.ServerDisconnect,
				)
				// Revoke a move-granted temporary SBAC grant while the target is
				// still in the VC (#487 Scope C). Same per-user rate limit.
				serverRoutes.DELETE(routeVoiceTempAccess,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.RevokeTempAccess,
				)
			}

			// Ownership transfer reversal (token-based, outside server routes)
			protected.POST("/ownership/reverse/:token",
				middleware.RateLimitByUser(redis, 3, 15*time.Minute),
				ownershipHandler.ReverseTransfer,
			)

			// Channel routes
			channelRoutes := protected.Group("/channels")
			{
				// Create channel (10 requests per minute)
				channelRoutes.POST("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.CreateChannel,
				)

				// Bulk message purge (#1352) — destructive; fail-CLOSED rate limit
				// (Redis error denies rather than allows).
				channelRoutes.DELETE("/:id/messages",
					middleware.RateLimitByUserFailClosed(redis, purgeRateLimit, purgeRateWindow),
					messagesHandler.PurgeChannel,
				)

				// Get specific channel (30 requests per minute)
				channelRoutes.GET("/:id",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetChannel,
				)

				// Update channel (10 requests per minute)
				channelRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.UpdateChannel,
				)

				// Delete channel (5 requests per minute - destructive action)
				channelRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					channelsHandler.DeleteChannel,
				)

				// Get message history for a channel (30 requests per minute)
				channelRoutes.GET("/:id/messages",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.GetMessages,
				)

				// Get pinned messages for a channel (30 requests per minute)
				channelRoutes.GET("/:id/pins",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.GetChannelPins,
				)

				// Bulk message fetch for search backfill (20 requests per minute)
				channelRoutes.GET("/:id/messages/bulk",
					middleware.RateLimitByUser(redis, 20, 1*time.Minute),
					messagesHandler.GetMessagesBulk,
				)

				// Mark channel as read (30 requests per minute)
				channelRoutes.POST(pathIDRead,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.MarkChannelRead,
				)

				// Get channel encryption keys (for E2EE channels)
				channelRoutes.GET(pathIDKeys,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetChannelKeys,
				)

				// Distribute channel keys to new members (E2EE key distribution)
				channelRoutes.POST(pathIDKeys,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.DistributeChannelKeys,
				)

				// Voice channel routes
				channelRoutes.GET("/:id/voice/participants",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					voiceHandler.GetParticipants,
				)
				channelRoutes.POST("/:id/voice/join",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.AuthorizeJoin,
				)
				channelRoutes.POST("/:id/voice/authorize-action",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					voiceHandler.AuthorizeVoiceAction,
				)

				// Rotate channel encryption key (admin/owner only)
				channelRoutes.POST("/:id/rotate-key",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.RotateKey,
				)

				// RBAC: Channel permission overrides
				channelRoutes.GET(pathIDOverrides,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbacHandler.ListChannelOverrides,
				)
				channelRoutes.PUT(pathIDOverrides,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.UpsertChannelOverride,
				)
				channelRoutes.DELETE("/:id/overrides/:override_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.DeleteChannelOverride,
				)

				// RBAC: Computed channel permissions
				channelRoutes.GET("/:id/permissions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbacHandler.GetMyChannelPermissions,
				)

				// RBAC: Channel permission sync (inherit from parent category)
				channelRoutes.PUT("/:id/permission-sync",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.SetChannelPermissionSync,
				)
			}

			// Category permission override routes
			categoryRoutes := protected.Group("/categories")
			{
				categoryRoutes.GET(pathIDOverrides,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					rbacHandler.ListCategoryOverrides,
				)
				categoryRoutes.PUT(pathIDOverrides,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.UpsertCategoryOverride,
				)
				categoryRoutes.DELETE("/:id/overrides/:override_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					rbacHandler.DeleteCategoryOverride,
				)
			}

			// E2EE key management routes (separate group to avoid gin /:id wildcard conflict)
			e2eeRoutes := protected.Group("/e2ee")
			{
				// Get pending key requests (for E2EE key distribution)
				e2eeRoutes.GET("/pending-keys",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					channelsHandler.GetPendingKeyRequests,
				)

				// Unified key endpoints (resolves server channel vs DM conversation)
				// GET needs a generous limit — every message decryption requires a key fetch
				// when the 5-minute client cache expires or on channel switch
				e2eeRoutes.GET("/keys/:context_id",
					middleware.RateLimitByUser(redis, 120, 1*time.Minute),
					channelsHandler.GetUnifiedKeys,
				)
				e2eeRoutes.POST("/keys/:context_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.DistributeUnifiedKeys,
				)
				// Re-enrollment trigger for missing-wrap recovery (#1023). Inserts a pending
				// row idempotently so peers can fulfill via DistributeUnifiedKeys.
				e2eeRoutes.POST("/keys/:context_id/rewrap",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					channelsHandler.RequestRewrap,
				)

				// Validate cached key epochs on reconnect (pull-based catch-up for missed revocations).
				// Desktop sends sequential 500-entry batches, so permit up to 30,000
				// bounded entries per user per minute rather than stopping a large
				// reconnect reconciliation after its first ten requests.
				e2eeRoutes.POST("/validate-epochs",
					middleware.RateLimitByUser(redis, 60, 1*time.Minute),
					channelsHandler.ValidateEpochs,
				)
			}

			// Message routes
			messageRoutes := protected.Group("/messages")
			{
				// Send message (30 requests per minute)
				messageRoutes.POST("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.SendMessage,
				)

				// Update message (10 requests per minute)
				messageRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.UpdateMessage,
				)

				// Delete message (10 requests per minute)
				messageRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.DeleteMessage,
				)

				// Suppress embeds on a message (moderator one-way ratchet)
				messageRoutes.POST("/:id/suppress-embeds",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.SuppressEmbeds,
				)

				// Toggle reaction on a message (30 requests per minute)
				messageRoutes.PUT("/:id/reactions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.ToggleReaction,
				)

				// Get reactions for a message (30 requests per minute)
				messageRoutes.GET("/:id/reactions",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					messagesHandler.GetReactions,
				)

				// Pin a message (10 requests per minute)
				messageRoutes.POST("/:id/pin",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.PinMessage,
				)

				// Unpin a message (10 requests per minute)
				messageRoutes.DELETE("/:id/pin",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					messagesHandler.UnpinMessage,
				)
			}

			// KLIPY GIF proxy routes (for Privacy Mode).
			// Routes only exist when KLIPY_API_KEY is set in the environment;
			// when the key is empty the entire integration is disabled and these
			// paths return 404 instead of forwarding to upstream.
			if cfg.KlipyAPIKey != "" {
				klipyHandler := klipy.NewHandler(cfg, log)
				klipyRoutes := protected.Group("/klipy")
				{
					// 30/min on the API endpoints (debounced on the client)
					apiLimiter := middleware.RateLimitByUser(redis, 30, 1*time.Minute)
					// GIF list/search/metadata endpoints — nested under /gifs to
					// match upstream KLIPY API conventions and the client paths
					// in klipyClient.ts (e.g. `/gifs/trending`, `/gifs/items`).
					gifRoutes := klipyRoutes.Group("/gifs")
					gifRoutes.GET("/trending", apiLimiter, klipyHandler.Trending)
					gifRoutes.GET("/search", apiLimiter, klipyHandler.Search)
					gifRoutes.GET("/categories", apiLimiter, klipyHandler.Categories)
					gifRoutes.GET("/recent/:customerID", apiLimiter, klipyHandler.Recent)
					gifRoutes.DELETE("/recent/:customerID", apiLimiter, klipyHandler.HideRecent)
					gifRoutes.GET("/items", apiLimiter, klipyHandler.Items)
					gifRoutes.POST("/share/:slug", apiLimiter, klipyHandler.Share)
					gifRoutes.POST("/report/:slug", apiLimiter, klipyHandler.Report)
					klipyRoutes.GET("/randomid", apiLimiter, klipyHandler.RandomID) //nolint:staticcheck // deprecated but kept for backward compatibility
					klipyRoutes.POST("/customer-id", apiLimiter, klipyHandler.CustomerID)

					// 300/min on the media proxy (chat scrollback bursts + GIF picker fresh scroll).
					// See #804 for the analysis of scroll-page burst patterns.
					klipyRoutes.GET("/media",
						middleware.RateLimitByUser(redis, 300, 1*time.Minute),
						klipyHandler.Media,
					)
				}
			}

			// Invite routes (join + preview)
			inviteRoutes := protected.Group("/invites")
			{
				// Join server via invite code (10 requests per minute)
				inviteRoutes.POST("/join",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					invitesHandler.JoinServer,
				)

				// Get invite info / preview (30 requests per minute)
				inviteRoutes.GET("/:code",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					invitesHandler.GetInviteInfo,
				)
			}

			// DM conversation routes
			dmRoutes := protected.Group("/dm/conversations")
			{
				// List user's DM conversations (30 requests per minute)
				dmRoutes.GET("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.ListConversations,
				)

				// Bulk DM/group purge (#1352) — destructive; fail-CLOSED rate limit.
				// Step-up auth (password/MFA) enforced in-handler per the actor's
				// require_auth_before_purge privacy setting.
				dmRoutes.DELETE("/:id/messages",
					middleware.RateLimitByUserFailClosed(redis, purgeRateLimit, purgeRateWindow),
					dmHandler.PurgeConversation,
				)

				// Open/get-or-create 1:1 DM conversation (10 requests per minute)
				dmRoutes.POST("",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.OpenConversation,
				)

				// Create group DM (10 requests per minute)
				dmRoutes.POST("/group",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.CreateGroup,
				)

				// Get or create personal thread (10 requests per minute)
				dmRoutes.POST("/personal",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.GetOrCreatePersonalThread,
				)

				// Get specific DM conversation (30 requests per minute)
				dmRoutes.GET("/:id",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetConversation,
				)

				// Update DM conversation (group name, etc.) (10 requests per minute)
				dmRoutes.PATCH("/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.UpdateConversation,
				)

				// Group member management (10 requests per minute)
				dmRoutes.POST(pathIDMembers,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.AddMember,
				)
				dmRoutes.DELETE(pathIDMembers+"/:userId",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.RemoveMember,
				)

				// Group member role management (10 requests per minute)
				dmRoutes.PATCH(pathIDMembers+"/:userId",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.UpdateMemberRole,
				)

				// Delete group DM (5 requests per minute)
				dmRoutes.DELETE("/:id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					dmHandler.DeleteGroup,
				)

				// Get DM message history (30 requests per minute)
				dmRoutes.GET("/:id/messages",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetMessages,
				)

				// Edit DM message (10 requests per minute)
				dmRoutes.PATCH("/:id/messages/:message_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.UpdateMessage,
				)

				// Delete DM message (10 requests per minute)
				dmRoutes.DELETE("/:id/messages/:message_id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DeleteMessage,
				)

				// Mark DM conversation as read (30 requests per minute)
				dmRoutes.POST(pathIDRead,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.MarkRead,
				)

				// DM E2EE key management
				dmRoutes.GET(pathIDKeys,
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetKeys,
				)
				dmRoutes.POST(pathIDKeys,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DistributeKeys,
				)

				// DM voice call
				dmRoutes.POST("/:id/voice/join",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.AuthorizeVoiceJoin,
				)
				// DM voice call ring (#1209) — server-authoritative
				// signaling per spec §6.1. Rate-limit matches /voice/join
				// (10/min/user) to bound any ring-spam attack surface.
				dmRoutes.POST("/:id/voice/ring",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.RingDMCall,
				)
				// Callee declines a ringing call. Higher rate limit (30/min)
				// because legitimate group calls (#1219) may emit several
				// declines in quick succession when multiple callees decline.
				dmRoutes.POST("/:id/voice/decline",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.DeclineDMCall,
				)
				// Caller cancels their own ring before any callee accepts.
				// Same rate limit as /ring (10/min/user) since a single
				// user can only initiate one ring per conversation at a time.
				dmRoutes.POST("/:id/voice/cancel",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.CancelDMCall,
				)
				// G7 defense-in-depth: media-plane calls this with a service
				// HMAC proof to re-check DM auth at the SFU boundary. Higher rate limit (60/min)
				// because the media-plane calls it on every SFU
				// reconnection (transport renegotiation, ICE restart, etc.)
				// and a single legitimate call can produce several reconnects.
				dmRoutes.POST("/:id/voice/authorize",
					middleware.RateLimitByUser(redis, 60, 1*time.Minute),
					dmHandler.AuthorizeDMVoiceForMediaPlane,
				)
				dmRoutes.DELETE("/:id/voice/authorize",
					middleware.RateLimitByUser(redis, 60, 1*time.Minute),
					dmHandler.AbortDMVoiceMediaAuthorization,
				)
				dmRoutes.GET("/:id/voice/participants",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					dmHandler.GetVoiceParticipants,
				)

				// DM voice enforcement (#488)
				dmRoutes.POST("/:id/voice/:userId/user-mute",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMUserMute,
				)
				dmRoutes.POST(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardMute,
				)
				dmRoutes.DELETE(routeVoiceMute,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardUnmute,
				)
				dmRoutes.POST(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardDeafen,
				)
				dmRoutes.DELETE(routeVoiceDeafen,
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					dmHandler.DMHardUndeafen,
				)

				// Manual seal & rotate DM encryption key (5 per day per conversation)
				dmRoutes.POST("/:id/rotate-key",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					dmHandler.RotateKey,
				)
			}

			// Friends routes
			friendRoutes := protected.Group("/friends")
			{
				// List accepted friends (30 requests per minute)
				friendRoutes.GET("",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					friendsHandler.ListFriends,
				)

				// List pending friend requests (30 requests per minute)
				friendRoutes.GET("/requests",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					friendsHandler.ListRequests,
				)

				// Send friend request (10 requests per minute)
				friendRoutes.POST("/request",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					friendsHandler.SendRequest,
				)

				// Accept/decline friend request (10 requests per minute)
				friendRoutes.PATCH("/request/:id",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
					friendsHandler.RespondRequest,
				)

				// Remove friend (5 requests per minute)
				friendRoutes.DELETE("/:user_id",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					friendsHandler.RemoveFriend,
				)

				// Block user (5 requests per minute)
				friendRoutes.POST("/:user_id/block",
					middleware.RateLimitByUser(redis, 5, 1*time.Minute),
					friendsHandler.BlockUser,
				)

				// Friend code routes
				codeRoutes := friendRoutes.Group("/codes")
				{
					// Create friend code (10 requests per minute)
					codeRoutes.POST("",
						middleware.RateLimitByUser(redis, 10, 1*time.Minute),
						friendsHandler.CreateFriendCode,
					)

					// List friend codes (30 requests per minute)
					codeRoutes.GET("",
						middleware.RateLimitByUser(redis, 30, 1*time.Minute),
						friendsHandler.ListFriendCodes,
					)

					// Revoke friend code (10 requests per minute)
					codeRoutes.DELETE("/:id",
						middleware.RateLimitByUser(redis, 10, 1*time.Minute),
						friendsHandler.RevokeFriendCode,
					)

					// Preview friend code (30 requests per minute)
					codeRoutes.GET("/:code",
						middleware.RateLimitByUser(redis, 30, 1*time.Minute),
						friendsHandler.PreviewFriendCode,
					)

					// Claim friend code (10 requests per minute)
					codeRoutes.POST("/:code/claim",
						middleware.RateLimitByUser(redis, 10, 1*time.Minute),
						friendsHandler.ClaimFriendCode,
					)
				}
			}

			// Notification preferences (per-server / per-channel / per-DM mutes)
			notificationRoutes := protected.Group("/notifications")
			{
				// Hydrate every mute pref for the caller (called once on app
				// boot to populate the renderer's notificationPrefsStore).
				notificationRoutes.GET("/preferences",
					middleware.RateLimitByUser(redis, 30, 1*time.Minute),
					notificationsHandler.ListPreferences,
				)

				// Upsert a single mute pref. Body: {target_type, target_id,
				// muted, muted_until?}. Rate limit matches the per-action
				// mute UI rhythm — bulk toggles are rare.
				notificationRoutes.PUT("/mute",
					middleware.RateLimitByUser(redis, 60, 1*time.Minute),
					notificationsHandler.SetMute,
				)
			}
		}

		// Media routes (object storage uploads, proxy, presigned URLs)
		// Only registered when storage is configured (nil in tests without MinIO).
		// The misconfig fallback lives on the public v1 group above so a single
		// `/api/v1/media/*path` wildcard handles every method/path consistently
		// (registering it on both v1 and protected would collide on the engine
		// route tree).
		mediaRoutes := protected.Group("/media")
		if mediaHandler != nil {
			// Tier 1 uploads (authenticated, server-processed)
			mediaRoutes.POST("/upload/avatar",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadAvatar,
			)
			mediaRoutes.POST("/upload/banner",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadBanner,
			)
			mediaRoutes.POST("/upload/server-icon",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadServerIcon,
			)
			mediaRoutes.POST("/upload/server-banner",
				middleware.RateLimitByUser(redis, 5, 1*time.Minute),
				mediaHandler.UploadServerBanner,
			)
			mediaRoutes.POST("/upload/dm-icon",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				mediaHandler.UploadDMIcon,
			)

			// Tier 2 upload (E2EE attachments — ciphertext stored as-is)
			mediaRoutes.POST("/upload/attachment",
				middleware.RateLimitByUser(redis, 30, 1*time.Minute),
				mediaHandler.UploadAttachment,
			)

			// Tier 2 proxy download (E2EE attachment — proxied, not presigned)
			mediaRoutes.GET("/attachments/:file_id",
				middleware.RateLimitByUser(redis, 60, 1*time.Minute),
				mediaHandler.DownloadAttachment,
			)

			// Delete media (uploader only)
			mediaRoutes.DELETE("/:file_id",
				middleware.RateLimitByUser(redis, 10, 1*time.Minute),
				mediaHandler.DeleteMedia,
			)
		} else {
			mediaUnavailable503 := func(c *gin.Context) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "media storage not configured"})
			}
			mediaRoutes.POST("/upload/avatar", mediaUnavailable503)
			mediaRoutes.POST("/upload/banner", mediaUnavailable503)
			mediaRoutes.POST("/upload/server-icon", mediaUnavailable503)
			mediaRoutes.POST("/upload/server-banner", mediaUnavailable503)
			mediaRoutes.POST("/upload/dm-icon", mediaUnavailable503)
			mediaRoutes.POST("/upload/attachment", mediaUnavailable503)
			mediaRoutes.GET("/attachments/:file_id", mediaUnavailable503)
			mediaRoutes.DELETE("/:file_id", mediaUnavailable503)
		}

		// WebSocket endpoint (requires JWT authentication via query parameter or header)
		v1.GET("/ws", wsHandler.HandleWebSocket)
	}

	// Platform-admin auth surface (#1688) — mounted at the top-level `/admin`
	// group, fully isolated from the user `/api/v1` JWT path (separate WebAuthn
	// RP, opaque Redis sessions, append-only audit, AdminAuthRequired middleware).
	// Host/path gating of this surface is #1692/#1693.
	wireAdminRoutes(router, db, redis, metricsReader, cfg, log)

	opsRuntime := wireOpsMetricsRuntime(db, natsClient, hub, opsCounters, cfg.OpsMetrics, log)
	// Start only after every dependency, observer, and route has been injected.
	go hub.Run()
	return router, hub, natsClient, opsRuntime, voicePermEnforcer, presenceRecheckExecutor, closePresenceWorkers, activePlanReconciler, nil
}

// healthHandler responds with 200 + control-plane health JSON. Registered
// for both GET and HEAD methods on /health by NewRouter (#882: HEAD support
// so CF Health Check probes and monitoring tools that default to HEAD don't
// false-negative on a healthy service).
//
// Per RFC 7231 §4.3.2, HEAD responses MUST NOT carry a body. gin's c.JSON
// writes the body unconditionally, so we skip it for HEAD: c.Status returns
// the status code without setting Content-Type or any other body-derived
// headers. RFC 7231 says HEAD "is otherwise identical to GET" but treats
// that as guidance, not a hard requirement — for a health-check endpoint
// where clients only consume the status code, the minimal status-only
// response is sufficient and correct.
//
// Package scope (not a closure) so router_test.go can drive it directly.
func healthHandler(c *gin.Context) {
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": "control-plane",
	})
}

// newRichPresenceHiddenSuppressor builds the hub callback that clears a sender's
// active Rich Presence when their base presence transitions to invisible or
// offline (#2444).
//
// Extracted from NewRouter rather than inlined: the nested closure plus its error
// branch pushed NewRouter's cognitive complexity past the SonarQube threshold, and
// the rule is not tunable on AI-authored code.
//
// The sender gate is acquired HERE, at the injection site, because
// SuppressHiddenSenderActivityAlreadyGated is an AlreadyGated method by contract --
// matching how account erasure calls its sibling. The hub dispatches this off its
// Run goroutine, so blocking on the gate is safe.
func newRichPresenceHiddenSuppressor(
	activityService *presence.ActivityService,
	presenceHistoryService *presencehistory.Service,
	log *logger.Logger,
	forced bool,
) func(uuid.UUID) {
	return func(userID uuid.UUID) {
		ctx, cancel := context.WithTimeout(
			context.Background(), richPresenceSuppressTimeout,
		)
		defer cancel()
		if err := presenceHistoryService.WithSender(ctx, userID, func() error {
			if forced {
				return activityService.ForceSuppressHiddenSenderActivityAlreadyGated(ctx, userID)
			}
			return activityService.SuppressHiddenSenderActivityAlreadyGated(ctx, userID)
		}); err != nil {
			// PII-safe: operation class only, never recipients or channel identity.
			log.Warn("rich-presence hidden-sender suppression failed", "error", err)
		}
	}
}
