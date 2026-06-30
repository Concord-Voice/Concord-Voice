// Package api provides HTTP routing and middleware setup for the Control Plane REST API.
package api // revive:disable-line:var-naming

import (
	"context"
	"database/sql"
	"encoding/hex"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/channels"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/clientconfig"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/feedback"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/friends"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/invites"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/klipy"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/media"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/members"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/messages"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/mfa"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/notifications"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/ownership"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/servercapabilities"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/servers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/sessions"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/updates"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
	"github.com/redis/go-redis/v9"
)

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

func (a *permissionCheckerAdapter) HasMentionPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error) {
	return a.resolver.HasPermission(ctx, serverID, userID, channelID, rbac.Permission(permBit))
}

func (a *permissionCheckerAdapter) HasChannelPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error) {
	return a.resolver.HasPermission(ctx, serverID, userID, channelID, rbac.Permission(permBit))
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

// NewRouter creates a new API router and returns the WebSocket hub and NATS client for lifecycle management.
func NewRouter(db *sql.DB, redis *redis.Client, store media.ObjectStore, cfg *config.Config, liveSpa *config.LiveSpaConfig, log *logger.Logger) (*gin.Engine, *websocket.Hub, *natsclient.Client) {
	router := gin.New()
	configureTrustedProxies(router, cfg, log)

	// Middleware
	router.Use(gin.Recovery())
	router.Use(middleware.RequestID())
	router.Use(middleware.Logger(log))
	router.Use(middleware.SecurityHeaders(cfg.Environment))
	router.Use(middleware.CORS(cfg.AllowedOrigins))
	router.Use(middleware.ValidateCustomHeaders())

	// Health check (#882: GET + HEAD both 200, so CF probes and monitoring
	// tools that default to HEAD don't false-negative). Handler is at package
	// scope (healthHandler below) so router_test.go can call it directly.
	router.GET("/health", healthHandler)
	router.HEAD("/health", healthHandler)

	// Initialize WebSocket hub
	hub := websocket.NewHub(db, redis)

	// Initialize NATS (inter-service messaging with media plane)
	var natsClient *natsclient.Client
	nc, err := natsclient.Connect(cfg.NATSUrl)
	if err != nil {
		log.Warn("NATS connection failed — voice state sync disabled", "error", err)
	} else {
		natsClient = nc
	}

	// Initialize MFA handler
	mfaEncKey, err := hex.DecodeString(cfg.MFAEncryptionKey)
	if err != nil || len(mfaEncKey) != 32 {
		log.Fatal("Invalid MFA_ENCRYPTION_KEY: must be 64 hex chars (32 bytes)")
	}
	webauthnSvc, err := mfa.NewWebAuthnService(cfg.WebAuthnRPID, "ConcordVoice", cfg.WebAuthnRPOrigins)
	if err != nil {
		log.Fatal("Failed to create WebAuthn service", "error", err)
	}
	mfaHandler := mfa.NewHandler(db, redis, log, mfaEncKey, cfg.JWTSecret, webauthnSvc, cfg.Environment)

	// Initialize RBAC components (before handlers that depend on resolver)
	permCache := rbac.NewPermissionCache(redis)
	rbacResolver := rbac.NewResolver(db, permCache, log)
	permissionChecker := &permissionCheckerAdapter{resolver: rbacResolver}
	hub.SetMentionChecker(permissionChecker)
	hub.SetChannelPermissionChecker(permissionChecker)

	// Start hub AFTER all dependencies are injected (avoids data races on checkers)
	go hub.Run()
	auditWriter := rbac.NewAuditWriter(db, log)
	rbacHandler := rbac.NewHandler(db, log, redis, hub, rbacResolver, permCache, auditWriter)

	// Initialize email service
	emailSvc := email.NewService(cfg, log)

	// Initialize handlers
	authHandler := auth.NewHandlerForInstance(db, redis, log, cfg.JWTSecret, hub, cfg.InstanceType)
	authHandler.SetEmailService(emailSvc)
	// Wire cross-references (breaks circular init dependency)
	authHandler.SetMFAChecker(mfaHandler)
	mfaHandler.SetLoginCompleter(authHandler)
	mfaHandler.SetEmailService(emailSvc)
	entCache := entitlements.NewCacheForInstance(redis, db, cfg.InstanceType)
	serverEntCache := entitlements.NewServerCacheForInstance(redis, db, cfg.InstanceType)
	sessionsHandler := sessions.NewHandler(db, redis, log, hub, mfaHandler)
	usersHandler := users.NewHandler(db, log, hub, mfaHandler, entCache)
	serversHandler := servers.NewHandler(db, log, hub, rbacResolver, serverEntCache)
	channelsHandler := channels.NewHandler(db, log, hub, rbacResolver, redis, serverEntCache)
	membersHandler := members.NewHandler(db, log, redis, hub, rbacResolver, auditWriter)
	messagesHandler := messages.NewHandler(db, log, hub, rbacResolver)
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
	dmHandler := dm.NewHandler(db, log, hub, cfg, natsClient, redis, entCache)
	// Wire DM voice ring cleanup-on-disconnect (#1209 plan task B7 Part 2).
	// When a user's last WS connection drops, the hub invokes
	// HandleUserDisconnect to cancel any rings they initiated.
	hub.SetDMRingCanceller(dmHandler.HandleUserDisconnect)
	friendsHandler := friends.NewHandler(db, log, hub)
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
	var mediaHandler *media.Handler
	if store != nil {
		mediaHandler = media.NewHandler(db, store, log, cfg, rbacResolver, entCache)
		usersHandler.SetMediaStore(store)
		serversHandler.SetMediaStore(store)
	}
	wsHandler := websocket.NewHandler(hub, db, redis, cfg.JWTSecret, cfg.AllowedOrigins)
	wsTicketHandler := auth.NewWSTicketHandler(redis, cfg.JWTSecret)
	clientConfigHandler := clientconfig.NewHandler(cfg, liveSpa, log)
	serverCapabilitiesHandler := servercapabilities.NewHandler(cfg)
	updatesHandler := updates.NewHandler(cfg, log)
	privacyHandler := buildPrivacyHandler(db, redis, log)
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
		voiceSub := voice.NewNATSSubscriber(db, log, hub, natsClient, rbacResolver)
		if subErr := voiceSub.Subscribe(); subErr != nil {
			log.Error("Failed to subscribe to voice NATS events", "error", subErr)
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
			middleware.AuthRequired(cfg.JWTSecret, redis),
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
		authRequired.Use(middleware.AuthRequired(cfg.JWTSecret, redis))
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
			// TEMPORARY (preflight testing window, Sat 2026-06-20 → Mon 2026-06-22):
			// per-user cap raised 10 → 1000/hour so active preflight testers are not
			// throttled while filing tickets. 1000 is STRICTLY above the global
			// 500/hour aggregate (GlobalRateLimit below), so the per-user layer is
			// provably never the binding constraint and the GLOBAL flood guard on
			// this privileged-PAT public-repo write is UNCHANGED — worst case at the
			// aggregate stays 500 issues/hour. REVERT to `10, 1*time.Hour` on
			// Mon 2026-06-22. The 10/hour steady-state rationale in the comment block
			// above is the design to restore.
			protected.POST("/feedback",
				middleware.RateLimitByUserFailClosed(redis, 1000, 1*time.Hour),
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

				// Validate cached key epochs on reconnect (pull-based catch-up for missed revocations)
				e2eeRoutes.POST("/validate-epochs",
					middleware.RateLimitByUser(redis, 10, 1*time.Minute),
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
				// G7 defense-in-depth: media-plane calls this to re-check
				// DM auth at the SFU boundary. Higher rate limit (60/min)
				// because the media-plane calls it on every SFU
				// reconnection (transport renegotiation, ICE restart, etc.)
				// and a single legitimate call can produce several reconnects.
				dmRoutes.POST("/:id/voice/authorize",
					middleware.RateLimitByUser(redis, 60, 1*time.Minute),
					dmHandler.AuthorizeDMVoiceForMediaPlane,
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
	wireAdminRoutes(router, db, redis, cfg, log)

	return router, hub, natsClient
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
