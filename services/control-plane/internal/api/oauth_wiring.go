//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope.
package api

import (
	"crypto/sha256"
	"database/sql"
	"io"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/cfkv"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/oauth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/hkdf"
)

// requireSSODeployment rejects hosted-only SSO before any route handler runs.
func requireSSODeployment(cfg *config.Config) gin.HandlerFunc {
	selfHosted := config.IsSelfHostedInstance(cfg.InstanceType)
	return func(c *gin.Context) {
		if selfHosted {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error_code": "sso_disabled_self_hosted"})
			return
		}
		c.Next()
	}
}

// buildOAuthHandler constructs the SSO endpoints handler. Google and Apple
// providers are registered only when their raw enable flags and deployment
// policy allow them. Self-hosted deployments always have an empty registry;
// the SSO route group independently rejects requests before handlers run.
//
// Config.Load still validates the credentials of every raw-enabled provider
// at startup, including on self-hosted deployments. Provider constructor
// failures remain fatal because previously validated configuration has
// unexpectedly regressed.
//
// Google's seed RedirectURI is a fallback — each /sso/google initiate request
// supplies the real loopback URI (via redirect_uri query param), stored in the
// sso_state record and included in the auth URL per RFC 6749 §4.1.3, letting
// concurrent OAuth attempts on different ephemeral loopback ports coexist.
// Apple takes no redirect config at all (#2306): its provider-facing redirect
// is the fixed Worker-bridge callback registered on the Apple Services ID; the
// desktop loopback is private relay metadata (Redis state + KV port) only.
func buildOAuthHandler(
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
	authHandler *auth.Handler,
	log *logger.Logger,
) *oauth.Handler {
	registry := oauth.NewRegistry()
	selfHosted := config.IsSelfHostedInstance(cfg.InstanceType)
	if cfg.GoogleSSO.Enabled && !selfHosted {
		provider, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
			ClientID: cfg.GoogleSSO.ClientID,
			// Fallback only — the constructor requires a non-empty RedirectURI,
			// but every production request supplies the real loopback URI via
			// redirect_uri query param (stored in sso_state). See doc comment above.
			RedirectURI: "http://127.0.0.1:0/oauth/callback",
		})
		if err != nil {
			log.Fatal("Failed to construct Google OAuth provider", "error", err)
		}
		registry.Register(provider)
		log.Info("Google SSO enabled", "client_id", cfg.GoogleSSO.ClientID)
	} else {
		log.Info("Google SSO disabled by configuration or deployment policy")
	}

	if cfg.AppleSSO.Enabled && !selfHosted {
		provider, err := oauth.NewAppleProvider(oauth.AppleConfig{
			ClientID:   cfg.AppleSSO.ClientID,
			TeamID:     cfg.AppleSSO.TeamID,
			KeyID:      cfg.AppleSSO.KeyID,
			PrivateKey: cfg.AppleSSO.PrivateKey,
		})
		if err != nil {
			log.Fatal("Failed to construct Apple OAuth provider", "error", err)
		}
		registry.Register(provider)
		log.Info("Apple SSO enabled", "client_id", cfg.AppleSSO.ClientID, "team_id", cfg.AppleSSO.TeamID)
	} else {
		log.Info("Apple SSO disabled by configuration or deployment policy")
	}

	// Cloudflare KV bridge (#973): publishes apple state→loopback-port
	// mappings for the apple-sso-bridge Worker. Typed-nil trap: only assign
	// through the interface variable inside the Enabled branch — a bare
	// `var kvBridge oauth.StatePortPutter` is a true nil interface, whereas
	// assigning `(*cfkv.Client)(nil)` would defeat the handler's nil check.
	var kvBridge oauth.StatePortPutter
	if cfg.CloudflareKVBridge.Enabled {
		kvBridge = cfkv.New(
			cfg.CloudflareKVBridge.AccountID,
			cfg.CloudflareKVBridge.NamespaceID,
			cfg.CloudflareKVBridge.APIToken,
		)
		log.Info("Cloudflare KV bridge enabled (apple-sso-bridge)")
	} else {
		log.Info("Cloudflare KV bridge disabled (CLOUDFLARE_KV_BRIDGE_ENABLED=false)")
	}

	// auditIPKey pseudonymizes client IPs in SSO audit events. Derived (never
	// reused raw) from JWTSecret via HKDF with a distinct info string so the
	// audit purpose is cryptographically separated from token signing — and no
	// new env var / deployment surface is introduced (#972 spec, F5).
	auditIPKey := make([]byte, 32)
	if _, err := io.ReadFull(
		hkdf.New(sha256.New, []byte(cfg.JWTSecret), nil, []byte("concord/audit-ip-pseudonym/v1")),
		auditIPKey,
	); err != nil {
		log.Fatal("Failed to derive SSO audit IP key", "error", err)
	}

	return oauth.NewHandler(oauth.HandlerDeps{
		Registry:    registry,
		Redis:       redisClient,
		DB:          db,
		AuthHandler: authHandler,
		CFKV:        kvBridge,
		Log:         log,
		AuditIPKey:  auditIPKey,
	})
}
