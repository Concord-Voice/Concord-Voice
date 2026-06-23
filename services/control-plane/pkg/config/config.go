// Package config provides configuration management for the Control Plane service.
package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const devJWTSecret = "dev_jwt_secret_change_in_production"                              // #nosec G101 -- Not a real secret, just the default placeholder
const devMFAEncKey = "0000000000000000000000000000000000000000000000000000000000000000" // #nosec G101 -- 32-byte zero key, dev only
const devMinIOSecretKey = "concord_dev_minio"                                           // #nosec G101 -- dev default, guarded by validate()

// Development-only connection values. NOT secrets — match docker-compose.yml.
// validate() rejects these in production.
var (
	devDBVal    = "concord_dev_password" //nolint:gosec // dev-only default
	devRedisVal = "concord_dev_redis"    //nolint:gosec // dev-only default
)

// defaultDevDatabaseURL builds the local development PostgreSQL connection string.
// Assembled from parts to satisfy static credential analysis (SonarQube S6698/S2068).
// validate() rejects this value in production.
func defaultDevDatabaseURL() string {
	return "postgres://concord:" + devDBVal + "@localhost:5432/concord?sslmode=disable"
}

// defaultDevRedisURL builds the local development Redis connection string.
func defaultDevRedisURL() string {
	return "redis://:" + devRedisVal + "@localhost:6379"
}

// GoogleSSOConfig holds Google Sign-In configuration. Empty when SSO is
// disabled. ClientSecret was removed in #975: the desktop client drives the
// code exchange directly (client-driven PKCE). Only ClientID is required.
type GoogleSSOConfig struct {
	Enabled  bool
	ClientID string
}

// String renders the config suitable for logs.
func (g GoogleSSOConfig) String() string {
	return fmt.Sprintf("GoogleSSOConfig{Enabled:%t ClientID:%q}", g.Enabled, g.ClientID)
}

// AppleSSOConfig holds Apple Sign-In credentials. Empty when SSO is disabled.
// PrivateKey is the PEM-encoded contents of the .p8 key file from Apple
// Developer. Two acceptable env-var formats:
//
//	APPLE_PRIVATE_KEY="<inline PEM with BEGIN/END PRIVATE KEY markers, newlines escaped as \n>"
//	APPLE_PRIVATE_KEY="@/etc/concord/apple_signin.p8"     (file path prefixed with @)
//
// String() redacts PrivateKey to never log key material.
type AppleSSOConfig struct {
	Enabled  bool
	ClientID string // Services ID, e.g. "chat.concordvoice.signin"
	TeamID   string // 10-char Team ID
	KeyID    string // 10-char Key ID
	// PrivateKey holds PEM bytes (resolved at load time if @-prefixed).
	PrivateKey []byte // #nosec G117 G101 -- False positive: config field name, actual key loaded from env
}

// String renders the config with the private key redacted, suitable for logs.
// The byte count is included so operators can distinguish empty-vs-populated
// keys in logs without exposing the key material itself.
func (a AppleSSOConfig) String() string {
	if !a.Enabled {
		return "AppleSSOConfig{Enabled:false}"
	}
	return fmt.Sprintf("AppleSSOConfig{Enabled:true ClientID:%q TeamID:%q KeyID:%q PrivateKey:[REDACTED %d bytes]}",
		a.ClientID, a.TeamID, a.KeyID, len(a.PrivateKey))
}

// GitHubFeedbackConfig holds the feedback handler's GitHub REST credentials
// (#158). Bundled so the Token field can be redacted via String().
type GitHubFeedbackConfig struct {
	// Token is a fine-scoped PAT with `issues:write` on the feedback-only
	// repo. Never logged — redacted by String() below. #nosec G101 -- type field name, not a secret.
	Token string
	// Repo is the `owner/repo` slug (e.g. "Concord-Voice/Concord-Voice-Feedback").
	Repo string
}

// String redacts the PAT. Mirrors the AppleSSO / CloudflareKVBridge
// pattern: any `%+v` dump of Config in a log line, error wrap, or debug
// pass-through prints the byte length of the token, never the token bytes.
func (c GitHubFeedbackConfig) String() string {
	if c.Token == "" {
		return fmt.Sprintf("GitHubFeedbackConfig{Token:[unset] Repo:%q}", c.Repo)
	}
	return fmt.Sprintf("GitHubFeedbackConfig{Token:[REDACTED %d bytes] Repo:%q}", len(c.Token), c.Repo)
}

// CloudflareKVBridgeConfig holds the Workers KV write credentials for the
// apple-sso-bridge state→port mapping (#973). Disabled by default; the
// feature ships dark until the client-driven Apple flow (#974) consumes it.
type CloudflareKVBridgeConfig struct {
	Enabled     bool
	AccountID   string
	NamespaceID string
	APIToken    string // #nosec G117 -- False positive: config field name, not a secret literal; struct has no JSON tags (never serialized) and String() redacts it.
}

// String redacts the API token.
func (c CloudflareKVBridgeConfig) String() string {
	if !c.Enabled {
		return "CloudflareKVBridgeConfig{Enabled:false}"
	}
	return fmt.Sprintf("CloudflareKVBridgeConfig{Enabled:true AccountID:%q NamespaceID:%q APIToken:[REDACTED %d bytes]}",
		c.AccountID, c.NamespaceID, len(c.APIToken))
}

// Config holds all configuration for the application
type Config struct {
	Environment string
	Port        string
	DatabaseURL string
	RedisURL    string
	JWTSecret   string // #nosec G117 -- False positive: config field name, actual secret loaded from env
	NATSUrl     string

	// CORS settings
	AllowedOrigins []string

	// Media plane
	MediaPlaneURL string

	// TURN/STUN (coturn)
	TURNServerHost string // Hostname or IP of the TURN server (e.g. "turn.concordvoice.chat")
	TURNSecret     string // Shared secret for HMAC ephemeral credentials
	TURNRealm      string // TURN realm (e.g. "concordvoice.chat")

	// Licensing
	LicensingAuthorityURL string

	// Client config (#155 Tier 2 + Tier 3)
	ClientMinVersion string // Minimum client version allowed (e.g. "0.1.3"); empty = no enforcement
	SpaURL           string // Remote SPA URL for Tier 3 hot updates (e.g. "https://app.concordvoice.chat/v1/"); empty = use bundled
	SpaIpcContract   int    // Minimum IPC contract version required by the remote SPA; 0 = no remote SPA
	SpaConfigFile    string // Path to mounted spa.env for hot-reload; empty = static config from env vars

	// Desktop update assets — local directory populated by deploy.sh
	ReleasesDir string // Path to directory containing release assets; empty disables update endpoint

	// SMTP (email verification)
	SMTPHost     string // SMTP server hostname (e.g. "smtp.protonmail.ch"); empty = dev mode (log codes to stdout)
	SMTPPort     int    // SMTP server port (default 587 for STARTTLS submission)
	SMTPUsername string // SMTP authentication username
	SMTPPassword string // SMTP authentication password // #nosec G101 -- config field, loaded from env
	SMTPFrom     string // Sender address (e.g. "noreply@concordvoice.chat")

	// MFA (TOTP + WebAuthn)
	MFAEncryptionKey  string   // 32-byte hex-encoded AES key for TOTP secret encryption at rest
	WebAuthnRPID      string   // Relying Party ID (domain, e.g. "concordvoice.chat")
	WebAuthnRPOrigins []string // Allowed origins for WebAuthn (e.g. "https://concordvoice.chat")

	// Admin console (#1688). AdminConsoleEnabled gates the entire platform-admin
	// auth surface: when false (the default), the /admin routes are NOT mounted
	// and the admin WebAuthn config is not required. #1688 ships the auth backend
	// DORMANT — the console UI (#1691) and network gating (#1692/#1693) flip this
	// on once the surface is ready, at which point validate() requires a real RP
	// config in production. This keeps an unexposed feature from either breaking
	// prod deploys (a hard-required var the FEEDBACK_PAT #1547 outage class) or
	// exposing inert public auth endpoints before the console exists.
	AdminConsoleEnabled bool

	// Admin WebAuthn — the platform-admin console's DEDICATED relying party,
	// deliberately separate from the user-facing WebAuthnRP* above so an admin
	// hardware key and a user passkey can never cross-validate (a shared RP would
	// let one act for the other). Non-secret: these are public RP identifiers and
	// an AAGUID allow-list, so no String() redaction is needed. Required in
	// production ONLY when AdminConsoleEnabled is true (validate() fatal-exits if
	// enabled with an unset/localhost RP_ID, empty origins, OR an empty AAGUID
	// allow-list — checkAAGUID is fail-closed, so empty would brick enrollment).
	AdminWebAuthnRPID           string   // Admin Relying Party ID (e.g. "admin.concordvoice.chat")
	AdminWebAuthnRPOrigins      []string // Allowed origins for the admin RP
	AdminWebAuthnAllowedAAGUIDs []string // Allow-listed authenticator AAGUIDs (e.g. approved YubiKey models)
	CFAccessAUD                 string   // Cloudflare Access audience tag verified by the origin
	CFAccessTeamDomain          string   // Cloudflare Access team domain (issuer + JWKS base URL)

	// Object Storage (MinIO / S3-compatible)
	MinIOEndpoint  string // MinIO server endpoint (e.g. "minio:9000")
	MinIOAccessKey string // MinIO access key (root user)
	MinIOSecretKey string // MinIO secret key // #nosec G101 -- config field, loaded from env
	MinIOUseSSL    bool   // Use SSL for MinIO connection
	MinIOBucket    string // Bucket name for media storage (e.g. "concord-media")
	UploadMaxSize  int64  // Global max upload size in bytes (default 25 MB)

	// KLIPY GIF integration (optional — empty key disables the feature).
	// SECURITY: this key is server-side ONLY. It is never delivered to clients.
	// All KLIPY traffic — API calls and GIF media — is proxied through
	// /api/v1/klipy/* routes in internal/klipy/handlers.go. The renderer rewrites
	// media URLs through /api/v1/klipy/media so KLIPY's CDN never sees per-user IPs.
	KlipyAPIKey string

	// Client attestation (#677)
	RequireClientAttestation bool          // Gate authenticated routes on signed-client attestation
	AttestationTokenTTL      time.Duration // Lifetime of issued attestation tokens; range 30m-24h
	AttestationPruneInterval time.Duration // Cadence for retention pruning (release_binaries + release_spas); range 1h-24h
	OIDCIssuer               string        // GitHub Actions OIDC issuer (typically token.actions.githubusercontent.com)
	OIDCAudience             string        // Expected OIDC audience claim
	OIDCSubjectPrefix        string        // Required OIDC sub prefix (e.g. "repo:markdrogersjr/Concord:")
	// Per-axis OIDC binding (W1, #677 reconciliation). The SPA publish path
	// and binary publish path are issued from DIFFERENT GitHub Actions
	// workflows (main-cd.yml vs build-desktop.yml). Each axis is bound to
	// its own (Workflow, Ref) pair so an OIDC token minted by one workflow
	// cannot satisfy the other axis's publish handler.
	OIDCSPAWorkflow    string // Required workflow path for /publish/spa (e.g. "main-cd.yml")
	OIDCSPARef         string // Required ref for /publish/spa (e.g. "refs/heads/main")
	OIDCBinaryWorkflow string // Required workflow path for /publish/binary (e.g. "build-desktop.yml")
	OIDCBinaryRef      string // Required ref for /publish/binary (e.g. "refs/heads/main")

	// TrustedProxyCIDRs is the allowlist of reverse-proxy CIDRs passed to
	// gin's SetTrustedProxies so c.ClientIP() resolves the real client IP via
	// X-Forwarded-For / X-Real-IP. Production MUST set TRUSTED_PROXY_CIDRS —
	// validate() rejects an empty list in production to prevent silent regression
	// to returning the proxy address instead of the real client IP.
	TrustedProxyCIDRs []string

	// GoogleSSO holds Google Sign-In credentials (#270). Disabled by default.
	// Load() returns an error if Enabled=true but credentials are unset, so
	// misconfiguration is caught at startup in any environment.
	GoogleSSO GoogleSSOConfig

	// AppleSSO holds Apple Sign-In credentials (#271). Disabled by default.
	// Load() returns an error if Enabled=true but any of the four required
	// fields (ClientID, TeamID, KeyID, PrivateKey) is unset, so misconfiguration
	// is caught at startup in any environment. The error message lists ALL
	// missing fields at once rather than failing on the first.
	AppleSSO AppleSSOConfig

	// CloudflareKVBridge holds the Workers KV write credentials for the
	// apple-sso-bridge state→port mapping (#973). Disabled by default;
	// Load() returns an error if Enabled=true but any required field is
	// unset, listing ALL missing fields at once.
	CloudflareKVBridge CloudflareKVBridgeConfig

	// GitHubFeedback holds the feedback handler's GitHub REST credentials
	// (#158). Token is a fine-scoped PAT with `issues:write` on the
	// feedback-only repo named by Repo. Empty fields disable the GitHub
	// call — handlers.go falls back to a log-only stub (dev / self-hosted
	// convenience). validate() rejects empty either field in production.
	// Bundled as a struct (mirroring AppleSSO / CloudflareKVBridge) so the
	// String() method below redacts the PAT — preventing accidental
	// leakage via %+v dumps anywhere in the codebase. Per the security
	// review on PR #1547.
	GitHubFeedback GitHubFeedbackConfig

	// RedemptionAdminToken gates the admin code-generation HTTP endpoint
	// (POST /api/v1/admin/redemption/codes, #1303). It is a config-provisioned
	// shared secret — the INTERIM issuer-authz primitive, because no
	// platform-admin RBAC role exists in the codebase (Role is per-server only:
	// server_members.role ∈ {owner,admin,member}). The handler compares the
	// X-Admin-Token header against this value with crypto/subtle.
	// ConstantTimeCompare. Empty DISABLES the HTTP generation endpoint (503) —
	// a safe default for dev / self-hosted; the CLI issuer path (direct DB
	// access) is unaffected. validate() fatal-exits ONLY when the token is SET
	// but weak (<32 chars); an UNSET token is allowed (incl. in production — it
	// just disables the HTTP endpoint, CLI issuance still works). Never logged.
	// FLAGGED for review: replace with a real platform-admin role + portal
	// (deferred follow-on epic, spec §10). #nosec G117 -- config field name.
	RedemptionAdminToken string // #nosec G117 -- config field name, secret loaded from env
}

// Load reads configuration from environment variables
func Load() (*Config, error) {
	// Load .env file if it exists (development)
	_ = godotenv.Load()

	cfg := &Config{
		Environment:           getEnv("ENVIRONMENT", "development"),
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           getEnv("DATABASE_URL", defaultDevDatabaseURL()),
		RedisURL:              getEnv("REDIS_URL", defaultDevRedisURL()),
		JWTSecret:             getEnv("JWT_SECRET", devJWTSecret),
		NATSUrl:               getEnv("NATS_URL", "nats://localhost:4222"),
		MediaPlaneURL:         getEnv("MEDIA_PLANE_URL", "http://localhost:3000"),
		TURNServerHost:        getEnv("TURN_SERVER_HOST", ""),
		TURNSecret:            getEnv("TURN_SECRET", ""),
		TURNRealm:             getEnv("TURN_REALM", "localhost"),
		LicensingAuthorityURL: getEnv("LICENSING_AUTHORITY_URL", "http://localhost:8082"),
		AllowedOrigins:        parseOrigins(getEnv("ALLOWED_ORIGINS", "http://localhost:3001,http://localhost:3002,app://concord")),
		ClientMinVersion:      getEnv("CLIENT_MIN_VERSION", ""),
		SpaURL:                getEnv("SPA_URL", ""),
		SpaIpcContract:        getEnvInt("SPA_IPC_CONTRACT", 0),
		SpaConfigFile:         getEnv("SPA_CONFIG_FILE", ""),
		ReleasesDir:           getEnv("RELEASES_DIR", ""),
		SMTPHost:              getEnv("SMTP_HOST", ""),
		SMTPPort:              getEnvInt("SMTP_PORT", 587),
		SMTPUsername:          getEnv("SMTP_USERNAME", ""),
		SMTPPassword:          getEnv("SMTP_PASSWORD", ""), // #nosec G101 -- env var name, not a secret
		SMTPFrom:              getEnv("SMTP_FROM", "Concord Voice <noreply@example.com>"),
		MFAEncryptionKey:      getEnv("MFA_ENCRYPTION_KEY", devMFAEncKey),
		WebAuthnRPID:          getEnv("WEBAUTHN_RP_ID", "localhost"),
		WebAuthnRPOrigins:     parseOrigins(getEnv("WEBAUTHN_RP_ORIGINS", "http://localhost:3001")),
		// Admin console (#1688) — dormant by default; the dev RP defaults point at
		// https-localhost (the console requires Secure for the __Host- session
		// cookie); the allowed-AAGUIDs default is empty (operator opts in to the gate).
		AdminConsoleEnabled:         getEnv("ADMIN_CONSOLE_ENABLED", "false") == "true",
		AdminWebAuthnRPID:           getEnv("ADMIN_WEBAUTHN_RP_ID", "localhost"),
		AdminWebAuthnRPOrigins:      parseOrigins(getEnv("ADMIN_WEBAUTHN_RP_ORIGINS", "https://localhost:8443")),
		AdminWebAuthnAllowedAAGUIDs: parseOrigins(getEnv("ADMIN_WEBAUTHN_ALLOWED_AAGUIDS", "")),
		CFAccessAUD:                 getEnv("CF_ACCESS_AUD", ""),
		CFAccessTeamDomain:          getEnv("CF_ACCESS_TEAM_DOMAIN", ""),
		MinIOEndpoint:               getEnv("MINIO_ENDPOINT", ""),
		MinIOAccessKey:              getEnv("MINIO_ACCESS_KEY", "concord"),
		MinIOSecretKey:              getEnv("MINIO_SECRET_KEY", devMinIOSecretKey), // #nosec G101 -- env var name, not a secret
		MinIOUseSSL:                 getEnv("MINIO_USE_SSL", "false") == "true",
		MinIOBucket:                 getEnv("MINIO_BUCKET", "concord-media"),
		UploadMaxSize:               getEnvInt64("UPLOAD_MAX_SIZE", 25*1024*1024), // 25 MB default
		KlipyAPIKey:                 getEnv("KLIPY_API_KEY", ""),
		GitHubFeedback: GitHubFeedbackConfig{
			// TrimSpace both fields: a trailing newline on the PAT (the common
			// copy-paste / secret-store artifact) corrupts the
			// "Bearer <token>" Authorization header and a stray-whitespace repo
			// slug yields per-request 502s from GitHub. (#158 review, PR #1547.)
			Token: strings.TrimSpace(getEnv("FEEDBACK_PAT", "")), // #nosec G101 -- env var name, not a secret
			Repo:  strings.TrimSpace(getEnv("FEEDBACK_REPO", "")),
		},
		// Redemption issuer-authz shared secret (#1303). TrimSpace defends the
		// common trailing-newline secret-store artifact (a stray byte would make
		// the constant-time compare always fail). Empty disables the HTTP gen
		// endpoint; production guard enforces it when generation is enabled.
		RedemptionAdminToken:     strings.TrimSpace(getEnv("REDEMPTION_ADMIN_TOKEN", "")), // #nosec G101 -- env var name, not a secret
		RequireClientAttestation: getEnv("REQUIRE_CLIENT_ATTESTATION", "false") == "true",
		AttestationTokenTTL:      parseAttestationTokenTTL(getEnv("ATTESTATION_TOKEN_TTL", "2h")),
		AttestationPruneInterval: parseAttestationPruneInterval(getEnv("ATTESTATION_PRUNE_INTERVAL", "6h")),
		OIDCIssuer:               getEnv("ATTESTATION_OIDC_ISSUER", "https://token.actions.githubusercontent.com"),
		OIDCAudience:             getEnv("ATTESTATION_OIDC_AUDIENCE", ""),
		OIDCSubjectPrefix:        getEnv("ATTESTATION_OIDC_SUBJECT_PREFIX", "repo:markdrogersjr/Concord:"),
		OIDCSPAWorkflow:          getEnv("ATTESTATION_OIDC_SPA_WORKFLOW", "main-cd.yml"),
		OIDCSPARef:               getEnv("ATTESTATION_OIDC_SPA_REF", "refs/heads/main"),
		OIDCBinaryWorkflow:       getEnv("ATTESTATION_OIDC_BINARY_WORKFLOW", "build-desktop.yml"),
		OIDCBinaryRef:            getEnv("ATTESTATION_OIDC_BINARY_REF", "refs/heads/main"),
	}

	cidrs, err := computeTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"), cfg.Environment)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	cfg.TrustedProxyCIDRs = cidrs

	// Google SSO (#270). Disabled by default. When enabled, only the client
	// ID is required — the client_secret was removed in #975 (client-driven
	// PKCE; the server no longer exchanges codes). A stale Google client-secret
	// env var is silently ignored so deployments migrating from the old config
	// do not need an immediate env-var removal pass. (The literal env-var name
	// is omitted here so the pr-ci re-introduction guard does not match this
	// comment.)
	cfg.GoogleSSO.Enabled = getEnv("GOOGLE_SSO_ENABLED", "false") == "true"
	cfg.GoogleSSO.ClientID = os.Getenv("GOOGLE_CLIENT_ID")
	if cfg.GoogleSSO.Enabled && cfg.GoogleSSO.ClientID == "" {
		return nil, fmt.Errorf("GoogleSSO enabled but GOOGLE_CLIENT_ID unset")
	}

	// Apple SSO (#271). Disabled by default. When enabled, all four fields
	// (ClientID, TeamID, KeyID, PrivateKey) must be set. The production guard
	// accumulates ALL missing fields rather than failing on the first, so
	// operators see every problem in one error.
	if err := loadAppleSSOConfig(cfg); err != nil {
		return nil, err
	}

	// Cloudflare KV bridge (#973). Disabled by default. When enabled, all
	// three credential fields must be set; the error lists every missing
	// field at once (mirrors the AppleSSO gate).
	if err := loadCloudflareKVBridgeConfig(cfg); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}

	return cfg, nil
}

// loadAppleSSOConfig populates cfg.AppleSSO from environment variables and
// enforces the production guard. Extracted from Load() to keep that function's
// cognitive complexity below the 15 SonarQube threshold.
//
// PrivateKey accepts two transport formats:
//   - Literal PEM with \n escapes (.env files / K8s ConfigMaps store
//     multiline values as single lines; we restore real newlines)
//   - @/path/to/file.p8 — the contents of the file are read at load time
//     (preferred for production: file is mounted via secrets manager /
//     systemd LoadCredential)
//
// When Enabled=true, all four fields (ClientID, TeamID, KeyID, PrivateKey)
// must be set. The production guard accumulates ALL missing fields rather
// than failing on the first, so operators see every problem in one error.
//
// Disabled deployments early-return BEFORE touching the filesystem so that
// stale APPLE_PRIVATE_KEY=@/path env vars from a prior enabled deployment
// don't cause startup to fail when SSO has been turned off.
func loadAppleSSOConfig(cfg *Config) error {
	cfg.AppleSSO.Enabled = getEnv("APPLE_SSO_ENABLED", "false") == "true"
	if !cfg.AppleSSO.Enabled {
		return nil
	}

	cfg.AppleSSO.ClientID = os.Getenv("APPLE_CLIENT_ID")
	cfg.AppleSSO.TeamID = os.Getenv("APPLE_TEAM_ID")
	cfg.AppleSSO.KeyID = os.Getenv("APPLE_KEY_ID")

	rawAppleKey := os.Getenv("APPLE_PRIVATE_KEY")
	if strings.HasPrefix(rawAppleKey, "@") {
		// TrimSpace on the after-`@` portion: APPLE_PRIVATE_KEY="@" or
		// "@   " is treated as missing-PrivateKey and falls through to the
		// production-guard accumulation below — symmetric with the
		// whitespace gate on the literal-PEM branch and avoids a confusing
		// "no such file or directory: \"\"" error from os.ReadFile.
		path := strings.TrimSpace(strings.TrimPrefix(rawAppleKey, "@"))
		if path != "" {
			b, err := os.ReadFile(path) // #nosec G304 G703 -- path is operator-supplied via env var, by design
			if err != nil {
				return fmt.Errorf("APPLE_PRIVATE_KEY file: %w", err)
			}
			cfg.AppleSSO.PrivateKey = b
		}
	} else if strings.TrimSpace(rawAppleKey) != "" {
		// Literal PEM; allow newline escapes from .env files.
		// strings.TrimSpace gate ensures whitespace-only env values
		// (e.g., APPLE_PRIVATE_KEY="   ") are treated as missing rather
		// than passing through to NewAppleProvider, which would surface
		// as a confusing PEM-parse error far from the actual misconfig.
		cfg.AppleSSO.PrivateKey = []byte(strings.ReplaceAll(rawAppleKey, `\n`, "\n"))
	}

	var missing []string
	if cfg.AppleSSO.ClientID == "" {
		missing = append(missing, "APPLE_CLIENT_ID")
	}
	if cfg.AppleSSO.TeamID == "" {
		missing = append(missing, "APPLE_TEAM_ID")
	}
	if cfg.AppleSSO.KeyID == "" {
		missing = append(missing, "APPLE_KEY_ID")
	}
	if len(cfg.AppleSSO.PrivateKey) == 0 {
		missing = append(missing, "APPLE_PRIVATE_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("AppleSSO enabled but %s unset", strings.Join(missing, ", "))
	}
	return nil
}

// loadCloudflareKVBridgeConfig populates cfg.CloudflareKVBridge. Mirrors the
// AppleSSO gate: disabled => zero-value group; enabled => every field
// required, all missing fields reported at once.
func loadCloudflareKVBridgeConfig(cfg *Config) error {
	cfg.CloudflareKVBridge.Enabled = getEnv("CLOUDFLARE_KV_BRIDGE_ENABLED", "false") == "true"
	if !cfg.CloudflareKVBridge.Enabled {
		return nil
	}
	cfg.CloudflareKVBridge.AccountID = getEnv("CLOUDFLARE_ACCOUNT_ID", "")
	cfg.CloudflareKVBridge.NamespaceID = getEnv("CLOUDFLARE_KV_NAMESPACE_ID", "")
	cfg.CloudflareKVBridge.APIToken = getEnv("CLOUDFLARE_KV_API_TOKEN", "")

	var missing []string
	if cfg.CloudflareKVBridge.AccountID == "" {
		missing = append(missing, "CLOUDFLARE_ACCOUNT_ID")
	}
	if cfg.CloudflareKVBridge.NamespaceID == "" {
		missing = append(missing, "CLOUDFLARE_KV_NAMESPACE_ID")
	}
	if cfg.CloudflareKVBridge.APIToken == "" {
		missing = append(missing, "CLOUDFLARE_KV_API_TOKEN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("CLOUDFLARE_KV_BRIDGE_ENABLED=true but %s unset", strings.Join(missing, ", "))
	}
	return nil
}

// validate checks for dangerous configuration in production environments.
// Returns an error if required secrets are missing or still set to dev defaults.
func (c *Config) validate() error {
	if c.Environment != "production" {
		return nil
	}

	// Table-driven production guards — each entry is a condition that must NOT be true.
	guards := []struct {
		bad bool   // true when the config value is dangerous for production
		msg string // error message explaining the problem
	}{
		{c.JWTSecret == devJWTSecret,
			"JWT_SECRET must be set to a secure value in production. The default dev secret is not allowed."},
		{c.DatabaseURL == defaultDevDatabaseURL(),
			"DATABASE_URL must be set to a secure value in production. The default dev connection string is not allowed."},
		{c.RedisURL == defaultDevRedisURL(),
			"REDIS_URL must be set to a secure value in production. The default dev connection string is not allowed."},
		{c.MFAEncryptionKey == devMFAEncKey,
			"MFA_ENCRYPTION_KEY must be set to a secure value in production. The default dev key is not allowed. Generate with: openssl rand -hex 32"},
		{c.MinIOSecretKey == devMinIOSecretKey, // #nosec G101 -- dev default comparison
			"MINIO_SECRET_KEY must be set to a secure value in production. The default dev credential is not allowed."},
		{c.SMTPHost == "",
			"SMTP_HOST must be set in production. Email verification cannot fall back to dev mode (logging codes to stdout) in production."},
		{c.MinIOAccessKey == "",
			"MINIO_ACCESS_KEY must be set in production."},
		{c.MinIOSecretKey == "", // #nosec G101 -- env var validation
			"MINIO_SECRET_KEY must be set in production."},
		{c.MinIOEndpoint == "",
			"MINIO_ENDPOINT must be set in production."},
		{c.MinIOBucket == "",
			"MINIO_BUCKET must be set in production."},
		{len(c.TrustedProxyCIDRs) == 0,
			"TRUSTED_PROXY_CIDRS must be set in production. Without it, c.ClientIP() returns the reverse-proxy address instead of the real client IP, breaking rate limiting and session audit logs."},
		// #725 review: MEDIA_PLANE_URL guard parity with other dev-default /
		// unset checks. Rejects empty, dev-default, and root-zone variants.
		{c.MediaPlaneURL == "",
			"MEDIA_PLANE_URL must be set in production. Leaving it unset breaks WebRTC signaling."},
		{c.MediaPlaneURL == "http://localhost:3000",
			"MEDIA_PLANE_URL must be set to a production value. The dev default http://localhost:3000 is not allowed."},
		{mediaPlaneURLIsRoot(c.MediaPlaneURL),
			"MEDIA_PLANE_URL must point to a dedicated media subdomain (e.g. https://media.example.com), not the bare apex domain. This guard catches variants including trailing slash, explicit port, path suffix, uppercase, and wss://."},
		// #158 — feedback handler. The dev-stub path (empty token) is fine
		// for self-hosted / dev. In production we require BOTH the token
		// AND the repo slug; either alone is a misconfiguration that
		// silently degrades to the log-only stub.
		{c.GitHubFeedback.Token == "",
			"FEEDBACK_PAT must be set in production. Without it the feedback handler falls back to a log-only stub and no GitHub issues are filed. Generate a fine-scoped PAT with `issues:write` on the feedback repo only."},
		{c.GitHubFeedback.Repo == "",
			"FEEDBACK_REPO must be set in production. Format: 'owner/repo' (e.g. 'Concord-Voice/Concord-Voice-Feedback')."},
		// Shape-validate the slug — a malformed FEEDBACK_REPO (missing slash,
		// empty segment, embedded whitespace, extra path component) builds a
		// bad GitHub issues URL and yields per-request 502s rather than a
		// fatal-exit at startup. Mirrors the rigor of the MEDIA_PLANE_URL
		// guard above (parse-then-validate, not just an empty check).
		// (#158 review, PR #1547.) The empty case is caught by the row above,
		// so this only fires for non-empty-but-malformed values.
		{c.GitHubFeedback.Repo != "" && !feedbackRepoHasValidShape(c.GitHubFeedback.Repo),
			"FEEDBACK_REPO must be a valid 'owner/repo' slug (exactly one '/', both segments non-empty, no whitespace). Example: 'Concord-Voice/Concord-Voice-Feedback'."},
		// #1688 — admin WebAuthn relying party. Guarded ONLY when the admin console
		// is enabled (ADMIN_CONSOLE_ENABLED=true): #1688 ships the surface dormant,
		// so an un-enabled console requires no admin config (avoids breaking prod
		// deploys with a not-yet-needed var). When enabled, the console is a browser
		// RP; an empty OR still-default-"localhost" RP ID, or an empty origin set,
		// silently mis-scopes every admin ceremony — so both are required and the
		// dev default must be overridden. ADMIN_WEBAUTHN_ALLOWED_AAGUIDS is ALSO
		// required when enabled: checkAAGUID is fail-closed (an empty list rejects
		// every authenticator), so an enabled console with no AAGUIDs would silently
		// brick enrollment — guard it loudly at startup instead (Gitar #1703).
		{c.AdminConsoleEnabled && (c.AdminWebAuthnRPID == "" || c.AdminWebAuthnRPID == "localhost"),
			"ADMIN_WEBAUTHN_RP_ID must be set to a real admin host in production when ADMIN_CONSOLE_ENABLED=true (not the 'localhost' dev default). The admin console's WebAuthn relying party must not share the user-facing WEBAUTHN_RP_ID."},
		{c.AdminConsoleEnabled && len(c.AdminWebAuthnRPOrigins) == 0,
			"ADMIN_WEBAUTHN_RP_ORIGINS must be set in production when ADMIN_CONSOLE_ENABLED=true. Without it the admin WebAuthn ceremony rejects every origin. Example: 'https://admin.concordvoice.chat'."},
		{c.AdminConsoleEnabled && len(c.AdminWebAuthnAllowedAAGUIDs) == 0,
			"ADMIN_WEBAUTHN_ALLOWED_AAGUIDS must list at least one authenticator AAGUID in production when ADMIN_CONSOLE_ENABLED=true. checkAAGUID is fail-closed — an empty list rejects every enrollment, bricking the console. Configure the approved YubiKey AAGUIDs (coordinate with #558)."},
		{c.AdminConsoleEnabled && c.CFAccessAUD == "",
			"CF_ACCESS_AUD must be set when ADMIN_CONSOLE_ENABLED=true — it is the Cloudflare Access audience tag the origin verifies; an empty AUD would accept any Access token (CWE-287)."},
		{c.AdminConsoleEnabled && !strings.HasPrefix(c.CFAccessTeamDomain, "https://"),
			"CF_ACCESS_TEAM_DOMAIN must be the https Cloudflare Access team domain (JWKS + issuer source) when ADMIN_CONSOLE_ENABLED=true."},
		// #1303 — redemption admin token. An UNSET token is allowed in
		// production (it simply disables the HTTP code-generation endpoint;
		// codes can still be issued via the CLI). But a SET-yet-weak token is a
		// foot-gun on a privileged paid-entitlement-minting surface, so we
		// require ≥32 chars when present. Generate with: openssl rand -hex 32.
		{c.RedemptionAdminToken != "" && len(c.RedemptionAdminToken) < 32,
			"REDEMPTION_ADMIN_TOKEN must be ≥32 chars when set (it gates the admin code-generation endpoint). Generate with: openssl rand -hex 32. Leave it unset to disable the HTTP generation endpoint (CLI issuance still works)."},
	}

	for _, g := range guards {
		if g.bad {
			return fmt.Errorf("FATAL: %s", g.msg)
		}
	}

	// Non-fatal: warn when production is using the broad RFC1918 fallback
	// for trusted proxies (the provision-secrets.yml heredoc default). This
	// is functional but permissive — see broadRFC1918CIDRs() for context.
	if equalCIDRs(c.TrustedProxyCIDRs, broadRFC1918CIDRs()) {
		log.Printf("WARNING: TRUSTED_PROXY_CIDRS is the broad RFC1918 fallback (%s). Tighten the TRUSTED_PROXY_CIDRS allowlist for stricter X-Forwarded-For trust.", strings.Join(c.TrustedProxyCIDRs, ","))
	}
	if !containsCloudflareProxyCIDR(c.TrustedProxyCIDRs) {
		log.Printf("WARNING: TRUSTED_PROXY_CIDRS does not include Cloudflare published proxy CIDRs. Public invite preview rate limiting may bucket by Cloudflare edge IP until vars.TRUSTED_PROXY_CIDRS is refreshed and provisioned.")
	}

	if err := c.validateAttestation(); err != nil {
		return err
	}

	return nil
}

// validateAttestation checks attestation-specific production guards.
// Extracted from validate() to keep cognitive complexity below the SonarQube
// threshold and to allow isolated unit testing of the new block.
//
// W1 (per-axis OIDC config): the four OIDC*Workflow / OIDC*Ref fields are
// individually validated. Each axis MUST have both a Workflow and a Ref
// configured when attestation is required — operator may have overridden the
// non-empty defaults with empty env values, which would silently disable the
// axis-binding check at the OIDC layer.
func (c *Config) validateAttestation() error {
	if !c.RequireClientAttestation {
		return nil
	}
	if c.OIDCAudience == "" {
		return fmt.Errorf("ATTESTATION_OIDC_AUDIENCE must be set when REQUIRE_CLIENT_ATTESTATION=true")
	}
	if c.Environment == "production" && c.OIDCIssuer != "https://token.actions.githubusercontent.com" {
		return fmt.Errorf("ATTESTATION_OIDC_ISSUER must be the canonical GitHub issuer in production")
	}
	if c.OIDCSPAWorkflow == "" {
		return fmt.Errorf("ATTESTATION_OIDC_SPA_WORKFLOW must be set when REQUIRE_CLIENT_ATTESTATION=true")
	}
	if c.OIDCSPARef == "" {
		return fmt.Errorf("ATTESTATION_OIDC_SPA_REF must be set when REQUIRE_CLIENT_ATTESTATION=true")
	}
	if c.OIDCBinaryWorkflow == "" {
		return fmt.Errorf("ATTESTATION_OIDC_BINARY_WORKFLOW must be set when REQUIRE_CLIENT_ATTESTATION=true")
	}
	if c.OIDCBinaryRef == "" {
		return fmt.Errorf("ATTESTATION_OIDC_BINARY_REF must be set when REQUIRE_CLIENT_ATTESTATION=true")
	}
	return nil
}

// mediaPlaneURLIsRoot returns true when the given MediaPlaneURL resolves
// to the apex zone host (with or without www.). Uses net/url parsing +
// case-folded host comparison so the following are all caught: trailing
// slash, explicit :443, path suffix (/api, etc.), uppercase variants,
// wss:// scheme. The apex host is not a valid media-plane endpoint —
// the media plane must be reached on its own dedicated subdomain — so
// none of these variants are valid production values.
//
// Does NOT flag a media subdomain or any other subdomain. Does NOT flag
// empty-string — that's the responsibility of a separate "unset" guard
// in validate(). Does NOT flag parse-failure — malformed URLs are a
// different class of bug and will fail downstream; here we only want to
// catch the specific apex-host misconfiguration.
func mediaPlaneURLIsRoot(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "concordvoice.chat" || host == "www.concordvoice.chat"
}

// feedbackRepoHasValidShape reports whether raw is a well-formed GitHub
// `owner/repo` slug: exactly one '/', both segments non-empty, and no
// whitespace anywhere. The control-plane interpolates this into the issues
// REST URL (`/repos/<slug>/issues`); a malformed value would 404/502 on every
// submission instead of failing loudly at startup. Caller is expected to TrimSpace
// the value at Load time, but we also reject any interior whitespace here so a
// slug like "owner /repo" cannot slip through. (#158 review, PR #1547.)
func feedbackRepoHasValidShape(raw string) bool {
	if strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	owner, repo, found := strings.Cut(raw, "/")
	if !found {
		return false
	}
	// strings.Cut splits on the FIRST '/'; a second '/' would land in repo,
	// so reject any remaining slash to enforce "exactly one".
	if owner == "" || repo == "" || strings.Contains(repo, "/") {
		return false
	}
	return true
}

// parseOrigins splits a comma-separated list of origins into a slice.
func parseOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	}
	return defaultValue
}

// defaultTrustedProxyCIDRs returns the dev/CI default for TRUSTED_PROXY_CIDRS.
// Production MUST override via env var — validate() rejects an empty list in
// production. The RFC1918 Docker private range (172.16.0.0/12) is used so
// compose-recreated bridge CIDRs (172.17.0.0/16, 172.19.0.0/16, etc.) remain
// trusted without re-config. The trust boundary in dev is the docker host
// itself — an attacker on the same bridge can already do worse than XFF spoof.
func defaultTrustedProxyCIDRs() string {
	return "172.16.0.0/12"
}

// broadRFC1918CIDRs returns the conservative fallback used by the
// provision-secrets workflow when TRUSTED_PROXY_CIDRS is unset. Trusting
// this entire range in production is operationally safer than failing
// startup, but it permits any RFC1918 source (including Docker bridge
// neighbors) to spoof X-Forwarded-For. validate() emits a warning when
// production lands on this fallback so operators are aware to tighten it
// via `gh variable set TRUSTED_PROXY_CIDRS --env production --body '<CIDR>'`.
func broadRFC1918CIDRs() []string {
	return []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
}

// cloudflareProxyCIDRs is a fallback snapshot of Cloudflare's published proxy
// ranges. Refresh TRUSTED_PROXY_CIDRS via the invite landing runbook.
var cloudflareProxyCIDRs = map[string]struct{}{
	"173.245.48.0/20":  {},
	"103.21.244.0/22":  {},
	"103.22.200.0/22":  {},
	"103.31.4.0/22":    {},
	"141.101.64.0/18":  {},
	"108.162.192.0/18": {},
	"190.93.240.0/20":  {},
	"188.114.96.0/20":  {},
	"197.234.240.0/22": {},
	"198.41.128.0/17":  {},
	"162.158.0.0/15":   {},
	"104.16.0.0/13":    {},
	"104.24.0.0/14":    {},
	"172.64.0.0/13":    {},
	"131.0.72.0/22":    {},
	"2400:cb00::/32":   {},
	"2606:4700::/32":   {},
	"2803:f800::/32":   {},
	"2405:b500::/32":   {},
	"2405:8100::/32":   {},
	"2a06:98c0::/29":   {},
	"2c0f:f248::/32":   {},
}

func containsCloudflareProxyCIDR(cidrs []string) bool {
	for _, cidr := range cidrs {
		if _, ok := cloudflareProxyCIDRs[cidr]; ok {
			return true
		}
	}
	return false
}

// equalCIDRs returns true if a and b contain the same CIDR strings in
// the same order. Used by validate() to detect the broad-RFC1918 fallback.
func equalCIDRs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// computeTrustedProxyCIDRs returns the effective trusted-proxy CIDR list based
// on the raw TRUSTED_PROXY_CIDRS env value and the deployment environment.
//
// In production, an empty/unset env value returns nil so validate() can enforce
// the production guard and fail startup — preventing silent deploys against
// load balancers outside the Docker bridge range.
//
// In non-production, an empty/unset env value falls back to the broad RFC1918
// Docker private range so dev/CI setups work without explicit configuration.
//
// A malformed CIDR returns the underlying parse error in all environments.
func computeTrustedProxyCIDRs(envValue, environment string) ([]string, error) {
	if envValue == "" {
		if environment == "production" {
			return nil, nil
		}
		envValue = defaultTrustedProxyCIDRs()
	}
	return parseTrustedProxyCIDRs(envValue)
}

// parseTrustedProxyCIDRs splits a comma-separated list of CIDRs and validates
// each. Returns an error on any malformed entry — silent skipping could leave
// the allowlist empty, silently regressing c.ClientIP() to return the reverse-
// proxy address and breaking IP-based rate limiting / session audit logs.
//
// Accepts both IPv4 and IPv6 CIDRs — net.ParseCIDR handles both uniformly.
func parseTrustedProxyCIDRs(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	cidrs := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(trimmed); err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in TRUSTED_PROXY_CIDRS: %w", trimmed, err)
		}
		cidrs = append(cidrs, trimmed)
	}
	return cidrs, nil
}

// parseAttestationTokenTTL parses the ATTESTATION_TOKEN_TTL env value and
// validates it is within the allowed range [30m, 24h]. Calls log.Fatalf on
// parse failure or out-of-range input so misconfiguration is caught at startup.
func parseAttestationTokenTTL(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Fatalf("FATAL: ATTESTATION_TOKEN_TTL invalid: %v", err)
	}
	if d < 30*time.Minute || d > 24*time.Hour {
		log.Fatalf("FATAL: ATTESTATION_TOKEN_TTL must be between 30m and 24h, got %s", d)
	}
	return d
}

// parseAttestationPruneInterval parses the ATTESTATION_PRUNE_INTERVAL env value
// and validates it is within the allowed range [1h, 24h]. Calls log.Fatalf on
// parse failure or out-of-range input. Per ADR-0010 D9 (retention pruning) the
// pruner runs on this cadence; an interval shorter than 1h is wasteful (rows
// don't accumulate that quickly) and longer than 24h defeats the purpose of
// the bounded growth invariant.
func parseAttestationPruneInterval(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Fatalf("FATAL: ATTESTATION_PRUNE_INTERVAL invalid: %v", err)
	}
	if d < time.Hour || d > 24*time.Hour {
		log.Fatalf("FATAL: ATTESTATION_PRUNE_INTERVAL must be between 1h and 24h, got %s", d)
	}
	return d
}
