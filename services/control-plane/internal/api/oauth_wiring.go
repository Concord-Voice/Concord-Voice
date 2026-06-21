//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope.
package api

import (
	"crypto/sha256"
	"database/sql"
	"io"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/cfkv"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/hkdf"
)

// buildOAuthHandler constructs the SSO endpoints handler. When
// cfg.GoogleSSO.Enabled is true, registers a GoogleProvider built from the
// configured client credentials. When disabled, returns a handler whose
// registry is empty — every /sso/:provider route then 404s with
// unknown_provider, which is the correct behaviour for an SSO-disabled
// deployment.
//
// Production guard: cfg.Load already errors out at startup if
// GOOGLE_SSO_ENABLED=true but credentials are missing. By the time we get
// here, the credential pair is either present (Enabled=true) or both empty
// (Enabled=false). A NewGoogleProvider failure here is fatal because it
// means a previously-validated config has somehow regressed — keep loud.
//
// The seed RedirectURI is a fallback — each /sso/:provider initiate request
// supplies the real loopback URI (via redirect_uri query param), stored in the
// sso_state record and included in the auth URL per RFC 6749 §4.1.3. This
// lets concurrent OAuth attempts on different ephemeral loopback ports coexist
// without server-side reconfiguration.
func buildOAuthHandler(
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
	authHandler *auth.Handler,
	log *logger.Logger,
) *oauth.Handler {
	registry := oauth.NewRegistry()
	if cfg.GoogleSSO.Enabled {
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
		log.Info("Google SSO disabled (GOOGLE_SSO_ENABLED=false)")
	}

	if cfg.AppleSSO.Enabled {
		provider, err := oauth.NewAppleProvider(oauth.AppleConfig{
			ClientID:   cfg.AppleSSO.ClientID,
			TeamID:     cfg.AppleSSO.TeamID,
			KeyID:      cfg.AppleSSO.KeyID,
			PrivateKey: cfg.AppleSSO.PrivateKey,
			// Fallback only — same rationale as Google above. Each /sso/apple
			// initiate request supplies the real loopback URI via the
			// redirect_uri query param, stored in sso_state, replayed at
			// exchange time per RFC 6749 §4.1.3.
			RedirectURI: "http://127.0.0.1:0/oauth/callback",
		})
		if err != nil {
			log.Fatal("Failed to construct Apple OAuth provider", "error", err)
		}
		registry.Register(provider)
		log.Info("Apple SSO enabled", "client_id", cfg.AppleSSO.ClientID, "team_id", cfg.AppleSSO.TeamID)
	} else {
		log.Info("Apple SSO disabled (APPLE_SSO_ENABLED=false)")
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
