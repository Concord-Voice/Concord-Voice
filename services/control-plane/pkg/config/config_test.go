package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetEnv(t *testing.T) {
	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv("TEST_CONFIG_VAR", "custom_value")

		assert.Equal(t, "custom_value", getEnv("TEST_CONFIG_VAR", "default"))
	})

	t.Run("returns default when not set", func(t *testing.T) {
		assert.Equal(t, "default_value", getEnv("NONEXISTENT_CONFIG_VAR", "default_value"))
	})

	t.Run("empty env value uses default", func(t *testing.T) {
		t.Setenv("TEST_EMPTY_VAR", "")

		assert.Equal(t, "fallback", getEnv("TEST_EMPTY_VAR", "fallback"))
	})
}

func TestParseOrigins(t *testing.T) {
	t.Run("single origin", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001")
		assert.Equal(t, []string{"http://localhost:3001"}, origins)
	})

	t.Run("multiple origins", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001,http://localhost:3002")
		assert.Equal(t, []string{"http://localhost:3001", "http://localhost:3002"}, origins)
	})

	t.Run("trims whitespace", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001 , http://localhost:3002 ")
		assert.Equal(t, []string{"http://localhost:3001", "http://localhost:3002"}, origins)
	})

	t.Run("filters empty strings", func(t *testing.T) {
		origins := parseOrigins("http://localhost:3001,,http://localhost:3002")
		assert.Equal(t, []string{"http://localhost:3001", "http://localhost:3002"}, origins)
	})

	t.Run("empty input returns empty slice", func(t *testing.T) {
		origins := parseOrigins("")
		assert.Empty(t, origins)
	})
}

func TestLoad(t *testing.T) {
	// Save and unset all env vars to test defaults
	envVars := []string{"ENVIRONMENT", "PORT", "DATABASE_URL", "REDIS_URL", "JWT_SECRET", "NATS_URL", "ALLOWED_ORIGINS", "TRUSTED_PROXY_CIDRS"}
	saved := make(map[string]string)
	for _, key := range envVars {
		saved[key] = os.Getenv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("failed to unset %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v != "" {
				_ = os.Setenv(k, v) //nolint:errcheck
			}
		}
	})

	t.Run("default values", func(t *testing.T) {
		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, "development", cfg.Environment)
		assert.Equal(t, "8080", cfg.Port)
		assert.Contains(t, cfg.DatabaseURL, "postgres://")
		assert.Contains(t, cfg.RedisURL, "redis://")
		assert.NotEmpty(t, cfg.JWTSecret)
		assert.Contains(t, cfg.NATSUrl, "nats://")
		assert.NotEmpty(t, cfg.AllowedOrigins)
		assert.Equal(t, []string{"172.16.0.0/12"}, cfg.TrustedProxyCIDRs)
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("PORT", "9090")
		t.Setenv("ENVIRONMENT", "staging")

		cfg, err := Load()
		require.NoError(t, err)

		assert.Equal(t, "9090", cfg.Port)
		assert.Equal(t, "staging", cfg.Environment)
	})
}

// TestLoad_AllowedOriginsDefault_OmitsHostedSpaOrigin pins the post-#1664
// public-mirror sanitization: the hosted Pages SPA origin is NOT a code
// default anymore. The default carries only neutral dev/test/CI origins
// (localhost + app://concord); production supplies the real remote-SPA
// origin via the ALLOWED_ORIGINS env var (provisioned by the canonical
// writer). Keeping the hosted subdomain out of the source default is a
// public-mirror leak-prevention requirement — the leak-check guard
// (scripts/check-public-mirror-leaks.sh) regression-locks the source side;
// this test regression-locks the runtime default.
func TestLoad_AllowedOriginsDefault_OmitsHostedSpaOrigin(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development") // skip production guards
	t.Setenv("ALLOWED_ORIGINS", "")        // force default
	cfg, err := Load()
	require.NoError(t, err)
	assert.NotContains(t, cfg.AllowedOrigins, "https://spa.concordvoice.chat",
		"hosted Pages SPA origin must NOT be a code default (set via env in production; #1664 mirror sanitization)")
	// Positive: the neutral dev/test/CI origins remain.
	assert.Contains(t, cfg.AllowedOrigins, "app://concord")
	assert.Contains(t, cfg.AllowedOrigins, "http://localhost:3001")
}

func TestLoadTrustedProxyCIDRs(t *testing.T) {
	// Pin ENVIRONMENT=development in every sub-test so Load() doesn't hit the
	// production guards (and fatally exit on unrelated missing prod config)
	// if the CI runner has ENVIRONMENT=production set. Scoped to each sub-test
	// via t.Setenv so it auto-restores on exit.
	t.Run("uses env var when set", func(t *testing.T) {
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,192.168.0.0/16")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, cfg.TrustedProxyCIDRs)
	})

	t.Run("uses default when unset", func(t *testing.T) {
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("TRUSTED_PROXY_CIDRS", "")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"172.16.0.0/12"}, cfg.TrustedProxyCIDRs)
	})
}

func TestComputeTrustedProxyCIDRs(t *testing.T) {
	t.Run("empty env in production returns nil", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("", "production")
		require.NoError(t, err)
		assert.Nil(t, got, "production with no env value must leave the slice empty so validate() can fail startup")
	})

	t.Run("empty env in development falls back to default", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("", "development")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.16.0.0/12"}, got)
	})

	t.Run("empty env in staging falls back to default", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("", "staging")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.16.0.0/12"}, got)
	})

	t.Run("explicit env value in production is honored", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("10.0.0.0/8", "production")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8"}, got)
	})

	t.Run("explicit env value in development overrides default", func(t *testing.T) {
		got, err := computeTrustedProxyCIDRs("10.0.0.0/8", "development")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8"}, got)
	})

	t.Run("malformed env value returns error in production", func(t *testing.T) {
		_, err := computeTrustedProxyCIDRs("not-a-cidr", "production")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})

	t.Run("malformed env value returns error in development", func(t *testing.T) {
		_, err := computeTrustedProxyCIDRs("not-a-cidr", "development")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})
}

// validProductionConfig returns a Config with all production guards satisfied.
func validProductionConfig() *Config { // #nosec G101 -- test fixture with fake credentials
	testVal := "real_pw"                                                          //nolint:gosec // test fixture, not a real credential
	dbURL := "postgres://concord:" + testVal + "@db:5432/concord?sslmode=disable" //nolint:gosec // test fixture
	redisURL := "redis://:" + testVal + "@redis:6379"                             //nolint:gosec // test fixture
	return &Config{                                                               //nolint:gosec // G101: test fixture with fake values, not real credentials
		Environment:       "production",
		JWTSecret:         "real-production-jwt-secret", //nolint:gosec // G101 false positive: test fixture, not a real secret
		DatabaseURL:       dbURL,
		RedisURL:          redisURL,
		MFAEncryptionKey:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MinIOSecretKey:    "real-minio-secret",
		MinIOAccessKey:    "real-minio-access",
		MinIOEndpoint:     "minio:9000",
		MinIOBucket:       "concord-media",
		SMTPHost:          "smtp.example.com",
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
		// Satisfy the #725 MEDIA_PLANE_URL production guards: must be set,
		// must not be the dev default, must not be the root zone.
		MediaPlaneURL: "https://media.concordvoice.chat",
		// #158 — feedback handler. Production requires both fields; empty
		// either is a fatal-exit per the guards added on this branch.
		GitHubFeedback: GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture with fake PAT, not a real credential
			Token: "ghp_real_production_pat", // #nosec G101 -- test fixture, not a real PAT
			Repo:  "Concord-Voice/Concord-Voice-Feedback",
		},
		// #1688 — admin WebAuthn relying party. When the console is enabled,
		// production requires RP_ID, ≥1 origin, AND a non-empty AAGUID allow-list
		// (checkAAGUID is fail-closed; empty would brick enrollment).
		AdminWebAuthnRPID:           "admin.concordvoice.chat",
		AdminWebAuthnRPOrigins:      []string{"https://admin.concordvoice.chat"},
		AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"},
	}
}

func TestValidateDevelopmentSkipsAllChecks(t *testing.T) {
	cfg := &Config{Environment: "development"}
	assert.NoError(t, cfg.validate())
}

func TestValidateProductionPassesWithValidConfig(t *testing.T) {
	cfg := validProductionConfig()
	assert.NoError(t, cfg.validate())
}

// TestValidateRedemptionAdminToken covers the #1303 issuer-authz token guard:
// unset is allowed (endpoint disabled), a ≥32-char token is allowed, and a
// set-but-too-short token is a fatal-exit.
func TestValidateRedemptionAdminToken(t *testing.T) {
	t.Run("unset is allowed (HTTP gen endpoint disabled)", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.RedemptionAdminToken = ""
		assert.NoError(t, cfg.validate())
	})

	t.Run("valid 32+ char token allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.RedemptionAdminToken = "0123456789abcdef0123456789abcdef" //nolint:gosec // 32-char test fixture // pragma: allowlist secret
		assert.NoError(t, cfg.validate())
	})

	t.Run("short token is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.RedemptionAdminToken = "too-short"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "REDEMPTION_ADMIN_TOKEN")
	})
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	t.Run("empty input returns empty slice", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("")
		require.NoError(t, err)
		assert.Empty(t, cidrs)
	})

	t.Run("single IPv4 CIDR", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16"}, cidrs)
	})

	t.Run("multiple IPv4 CIDRs", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16,10.0.0.0/8")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "10.0.0.0/8"}, cidrs)
	})

	t.Run("mixed IPv4 and IPv6 CIDRs", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16,::1/128")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "::1/128"}, cidrs)
	})

	t.Run("trims whitespace around entries", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs(" 172.19.0.0/16 , 10.0.0.0/8 ")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "10.0.0.0/8"}, cidrs)
	})

	t.Run("filters empty entries from double commas", func(t *testing.T) {
		cidrs, err := parseTrustedProxyCIDRs("172.19.0.0/16,,10.0.0.0/8")
		require.NoError(t, err)
		assert.Equal(t, []string{"172.19.0.0/16", "10.0.0.0/8"}, cidrs)
	})

	t.Run("rejects malformed CIDR", func(t *testing.T) {
		_, err := parseTrustedProxyCIDRs("not-a-cidr")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
		assert.Contains(t, err.Error(), "TRUSTED_PROXY_CIDRS")
	})

	t.Run("rejects missing mask", func(t *testing.T) {
		_, err := parseTrustedProxyCIDRs("172.19.0.0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "172.19.0.0")
	})

	t.Run("rejects one bad entry in a list", func(t *testing.T) {
		_, err := parseTrustedProxyCIDRs("172.19.0.0/16,not-a-cidr,10.0.0.0/8")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})
}

func TestValidateProductionRejectsDevDefaults(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantMsg string
	}{
		{"dev JWT secret", func(c *Config) { c.JWTSecret = devJWTSecret }, "JWT_SECRET"},
		{"dev DATABASE_URL", func(c *Config) { c.DatabaseURL = defaultDevDatabaseURL() }, "DATABASE_URL"},
		{"dev REDIS_URL", func(c *Config) { c.RedisURL = defaultDevRedisURL() }, "REDIS_URL"},
		{"dev MFA encryption key", func(c *Config) { c.MFAEncryptionKey = devMFAEncKey }, "MFA_ENCRYPTION_KEY"},
		{"dev MinIO secret key", func(c *Config) { c.MinIOSecretKey = devMinIOSecretKey }, "MINIO_SECRET_KEY"},
		{"empty SMTP host", func(c *Config) { c.SMTPHost = "" }, "SMTP_HOST"},
		{"empty MinIO access key", func(c *Config) { c.MinIOAccessKey = "" }, "MINIO_ACCESS_KEY"},
		{"empty MinIO endpoint", func(c *Config) { c.MinIOEndpoint = "" }, "MINIO_ENDPOINT"},
		{"empty MinIO bucket", func(c *Config) { c.MinIOBucket = "" }, "MINIO_BUCKET"},
		{"empty TRUSTED_PROXY_CIDRS", func(c *Config) { c.TrustedProxyCIDRs = nil }, "TRUSTED_PROXY_CIDRS"},
		// #725 cross-cutting: MEDIA_PLANE_URL guards. Three classes of failure
		// should all be rejected by the production validate() pass.
		//
		// Class 1 — unset / dev-default:
		{"empty MEDIA_PLANE_URL", func(c *Config) { c.MediaPlaneURL = "" }, "MEDIA_PLANE_URL"},
		{"dev-default MEDIA_PLANE_URL", func(c *Config) { c.MediaPlaneURL = "http://localhost:3000" }, "MEDIA_PLANE_URL"},
		// Class 2 — root-zone exact forms:
		{"MEDIA_PLANE_URL root https", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root http", func(c *Config) { c.MediaPlaneURL = "http://concordvoice.chat" }, "MEDIA_PLANE_URL"},
		// Class 3 — root-zone variants that exact string match would miss.
		// Per #725 review: Copilot + Seer + 3 subagents independently flagged
		// that exact-match guards are bypassed by any of these variants.
		{"MEDIA_PLANE_URL root with trailing slash", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat/" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root with explicit port", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat:443" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root with path", func(c *Config) { c.MediaPlaneURL = "https://concordvoice.chat/api" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root uppercase", func(c *Config) { c.MediaPlaneURL = "HTTPS://CONCORDVOICE.CHAT" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root wss scheme", func(c *Config) { c.MediaPlaneURL = "wss://concordvoice.chat" }, "MEDIA_PLANE_URL"},
		{"MEDIA_PLANE_URL root with www", func(c *Config) { c.MediaPlaneURL = "https://www.concordvoice.chat" }, "MEDIA_PLANE_URL"},
		// #158 — feedback handler production guards. Either field empty must
		// fatal-exit; same shape as the other "empty X in production" rows.
		{"empty FEEDBACK_PAT", func(c *Config) { c.GitHubFeedback.Token = "" }, "FEEDBACK_PAT"},
		{"empty FEEDBACK_REPO", func(c *Config) { c.GitHubFeedback.Repo = "" }, "FEEDBACK_REPO"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validProductionConfig()
			tt.mutate(cfg)
			err := cfg.validate()
			require.Error(t, err, "expected error for %s", tt.name)
			assert.True(t, strings.Contains(err.Error(), tt.wantMsg),
				"expected error to contain %q, got: %v", tt.wantMsg, err)
		})
	}
}

// TestValidateProductionAcceptsMediaSubdomain confirms valid MEDIA_PLANE_URL
// forms are NOT rejected — regression guard against a future tightening that
// would reject legitimate media-plane hosts along with the root zone.
func TestValidateProductionAcceptsMediaSubdomain(t *testing.T) {
	accepts := []string{
		"https://media.concordvoice.chat",     // canonical production value
		"https://media.concordvoice.chat/",    // trailing slash OK on subdomain
		"https://media.concordvoice.chat:443", // explicit port OK on subdomain
		"https://other.example.com",           // unrelated host passes (operator override)
		"http://media.example.com",            // http OK for non-production-hostname variants
	}
	for _, v := range accepts {
		t.Run(v, func(t *testing.T) {
			cfg := validProductionConfig()
			cfg.MediaPlaneURL = v
			assert.NoError(t, cfg.validate(),
				"%s must be accepted — only the root concordvoice.chat zone is rejected", v)
		})
	}
}

// TestMediaPlaneURLIsRoot covers the helper function directly so future
// refactors can't regress the host-matching logic without a test failure.
func TestMediaPlaneURLIsRoot(t *testing.T) {
	cases := []struct {
		input string
		root  bool
	}{
		// Root zone — should all be flagged:
		{"https://concordvoice.chat", true},
		{"http://concordvoice.chat", true},
		{"https://concordvoice.chat/", true},
		{"https://concordvoice.chat:443", true},
		{"https://concordvoice.chat/api", true},
		{"HTTPS://CONCORDVOICE.CHAT", true},
		{"wss://concordvoice.chat", true},
		{"https://www.concordvoice.chat", true},
		{"https://WWW.Concordvoice.Chat", true},
		// Non-root — should all pass:
		{"https://media.concordvoice.chat", false},
		{"https://api.concordvoice.chat", false},
		{"https://turn.concordvoice.chat", false},
		{"https://example.com", false},
		{"http://localhost:3000", false}, // dev default, rejected by a different guard
		{"", false},                      // empty, rejected by a different guard
		{"not-a-url", false},             // malformed — different guard territory
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.root, mediaPlaneURLIsRoot(c.input))
		})
	}
}

// ─── Feedback config (#158, PR #1547 Fix 6) ───────────────────────────────

// Load TrimSpaces FEEDBACK_PAT / FEEDBACK_REPO — a trailing newline on the PAT
// corrupts the Authorization header and a stray-whitespace repo slug yields
// per-request 502s. Run in development so the production guards don't fire.
func TestLoad_FeedbackFieldsAreTrimmed(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("FEEDBACK_PAT", "  ghp_trim_me\n") //nolint:gosec // test fixture, not a real PAT
	t.Setenv("FEEDBACK_REPO", "\tConcord-Voice/Concord-Voice-Feedback \n")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "ghp_trim_me", cfg.GitHubFeedback.Token,
		"FEEDBACK_PAT must be TrimSpace'd at Load")
	assert.Equal(t, "Concord-Voice/Concord-Voice-Feedback", cfg.GitHubFeedback.Repo,
		"FEEDBACK_REPO must be TrimSpace'd at Load")
}

// A malformed FEEDBACK_REPO fails validate() in production — mirrors the
// rigor of the MEDIA_PLANE_URL guard (parse-then-validate, not empty-only).
func TestValidateProductionRejectsMalformedFeedbackRepo(t *testing.T) {
	tests := []struct {
		name string
		repo string
	}{
		{"no slash", "ownerrepo"},
		{"empty owner segment", "/repo"},
		{"empty repo segment", "owner/"},
		{"both segments empty", "/"},
		{"embedded space", "owner /repo"},
		{"interior whitespace in repo", "owner/re po"},
		{"extra path segment", "owner/repo/extra"},
		{"trailing newline (untrimmed)", "owner/repo\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validProductionConfig()
			cfg.GitHubFeedback.Repo = tt.repo
			err := cfg.validate()
			require.Error(t, err, "expected error for malformed repo %q", tt.repo)
			assert.Contains(t, err.Error(), "FEEDBACK_REPO",
				"error must name the offending field")
		})
	}
}

// A valid owner/repo slug passes validate() in production (regression guard so
// the shape check doesn't reject legitimate values).
func TestValidateProductionAcceptsValidFeedbackRepo(t *testing.T) {
	accepts := []string{
		"Concord-Voice/Concord-Voice-Feedback",
		"owner/repo",
		"a/b",
		"org.with.dots/repo_with-mix.123",
	}
	for _, repo := range accepts {
		t.Run(repo, func(t *testing.T) {
			cfg := validProductionConfig()
			cfg.GitHubFeedback.Repo = repo
			assert.NoError(t, cfg.validate(), "valid slug %q must be accepted", repo)
		})
	}
}

// feedbackRepoHasValidShape unit coverage — happy path + every rejection case.
func TestFeedbackRepoHasValidShape(t *testing.T) {
	cases := []struct {
		input string
		valid bool
	}{
		// Valid:
		{"owner/repo", true},
		{"Concord-Voice/Concord-Voice-Feedback", true},
		{"a/b", true},
		{"org.with.dots/repo_with-mix.123", true},
		// Invalid:
		{"", false},
		{"ownerrepo", false},
		{"/repo", false},
		{"owner/", false},
		{"/", false},
		{"owner/repo/extra", false},
		{"owner /repo", false},
		{"owner/re po", false},
		{"owner/repo\n", false},
		{" owner/repo", false},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.valid, feedbackRepoHasValidShape(c.input))
		})
	}
}

// Google SSO config tests (#270 Task 4).
//
// The production guard fires on misconfiguration regardless of environment —
// if SSO is enabled but credentials are missing, Load() returns an error so
// dev/staging surfaces the bug at startup instead of failing on first login.

func TestGoogleSSOConfig_Disabled_NoFatal(t *testing.T) {
	t.Setenv("GOOGLE_SSO_ENABLED", "false")
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.GoogleSSO.Enabled)
}

func TestGoogleSSOConfig_EnabledWithClientID_OK(t *testing.T) {
	t.Setenv("GOOGLE_SSO_ENABLED", "true")
	t.Setenv("GOOGLE_CLIENT_ID", "test-client-id.apps.googleusercontent.com")
	// GOOGLE_CLIENT_SECRET removed in #975; stale values are silently ignored.

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.GoogleSSO.Enabled)
	assert.Equal(t, "test-client-id.apps.googleusercontent.com", cfg.GoogleSSO.ClientID)
}

func TestGoogleSSOConfig_EnabledMissingClientID_Error(t *testing.T) {
	t.Setenv("GOOGLE_SSO_ENABLED", "true")
	t.Setenv("GOOGLE_CLIENT_ID", "")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GoogleSSO enabled but GOOGLE_CLIENT_ID")
}

func TestGoogleSSOConfig_String(t *testing.T) {
	cfg := GoogleSSOConfig{
		Enabled:  true,
		ClientID: "test-client-id",
	}
	str := cfg.String()
	assert.Contains(t, str, "test-client-id")
	// ClientSecret removed in #975 — String() no longer includes a secret field.
	assert.NotContains(t, str, "ClientSecret")
	assert.NotContains(t, str, "[REDACTED]")
	assert.NotContains(t, str, "[empty]")
}

// TestLoad_GoogleSSO_NoClientSecretRequired verifies that enabling Google SSO
// with only a client_id (no client_secret) loads successfully after #975
// removed server-side code exchange. GOOGLE_CLIENT_SECRET is now ignored.
func TestLoad_GoogleSSO_NoClientSecretRequired(t *testing.T) {
	t.Setenv("GOOGLE_SSO_ENABLED", "true")
	t.Setenv("GOOGLE_CLIENT_ID", "cid.apps.googleusercontent.com")
	t.Setenv("GOOGLE_CLIENT_SECRET", "") // explicitly empty/unset — must not error
	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.GoogleSSO.Enabled)
	assert.Equal(t, "cid.apps.googleusercontent.com", cfg.GoogleSSO.ClientID)
}

// TestLoad_GoogleSSO_StaleSecretTolerated verifies that a stale
// GOOGLE_CLIENT_SECRET env var (e.g., left over before the #975 migration)
// is silently ignored — Load() must not error on its presence.
func TestLoad_GoogleSSO_StaleSecretTolerated(t *testing.T) {
	t.Setenv("GOOGLE_SSO_ENABLED", "true")
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "orphan-value") // present but ignored
	_, err := Load()
	require.NoError(t, err)
}

// Apple SSO config tests (#271 Task 3).
//
// Apple has 4 required env vars (vs Google's 2) and the production guard
// accumulates ALL missing fields rather than failing on the first.
// PrivateKey supports two transport formats: literal PEM with \n escapes
// (for .env files / K8s ConfigMaps) and @/path/to/file (for mounted secrets).

// generateTestApplePEM returns a freshly-generated P-256 ECDSA private key
// encoded as PKCS8 PEM bytes. Mirrors the helper in internal/oauth/apple_test.go
// — defined here because that helper lives in package oauth_test, which is not
// accessible from package config tests.
func generateTestApplePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestLoad_AppleSSO_Disabled(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "false")
	t.Setenv("APPLE_CLIENT_ID", "")
	t.Setenv("APPLE_TEAM_ID", "")
	t.Setenv("APPLE_KEY_ID", "")
	t.Setenv("APPLE_PRIVATE_KEY", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.AppleSSO.Enabled)
	assert.Empty(t, cfg.AppleSSO.PrivateKey)
}

func TestLoad_AppleSSO_EnabledWithAllFields_LiteralPEM(t *testing.T) {
	pemBytes := generateTestApplePEM(t)
	// Simulate .env-style storage: real newlines replaced with literal \n.
	escaped := strings.ReplaceAll(string(pemBytes), "\n", `\n`)

	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", escaped)

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.AppleSSO.Enabled)
	assert.Equal(t, "chat.concordvoice.signin", cfg.AppleSSO.ClientID)
	assert.Equal(t, "TEAM123ABC", cfg.AppleSSO.TeamID)
	assert.Equal(t, "KEYID12345", cfg.AppleSSO.KeyID)

	// The escape decoder must restore real newlines so PEM parsing works.
	assert.Contains(t, string(cfg.AppleSSO.PrivateKey), "\n", "loader must convert \\n escapes back to real newlines")
	assert.NotContains(t, string(cfg.AppleSSO.PrivateKey), `\n`, "loader must remove the literal escape sequence")

	// PEM must round-trip: decode the loaded bytes successfully.
	block, _ := pem.Decode(cfg.AppleSSO.PrivateKey)
	require.NotNil(t, block, "loaded PrivateKey must be a valid PEM block")
	assert.Equal(t, "PRIVATE KEY", block.Type)
}

func TestLoad_AppleSSO_EnabledWithAllFields_FilePath(t *testing.T) {
	pemBytes := generateTestApplePEM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "apple_signin.p8")
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))

	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", "@"+path)

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.AppleSSO.Enabled)
	assert.Equal(t, pemBytes, cfg.AppleSSO.PrivateKey)
}

func TestLoad_AppleSSO_FilePathNotFound(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", "@/nonexistent/path/apple_signin.p8")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_PRIVATE_KEY file")
}

// TestLoad_AppleSSO_DisabledIgnoresStaleFilePath pins the operator-friendly
// behavior where a deployment with APPLE_SSO_ENABLED=false must NOT fail
// startup on a stale APPLE_PRIVATE_KEY=@/path env var left over from a
// prior enabled deployment. Apple SSO loading must early-return before
// touching the filesystem when SSO is disabled.
func TestLoad_AppleSSO_DisabledIgnoresStaleFilePath(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "false")
	t.Setenv("APPLE_PRIVATE_KEY", "@/nonexistent/path/apple_signin.p8")

	cfg, err := Load()
	require.NoError(t, err, "disabled Apple SSO must not fail on stale file path env var")
	assert.False(t, cfg.AppleSSO.Enabled)
	assert.Empty(t, cfg.AppleSSO.PrivateKey,
		"PrivateKey must remain empty when SSO is disabled, regardless of env var content")
}

// TestLoad_AppleSSO_DisabledIgnoresStaleLiteralPEM is the literal-PEM
// counterpart to the @/path case above. A deployment with disabled Apple
// SSO and a stale literal-PEM env var must NOT load it into PrivateKey
// (the field stays empty), matching the disabled-path early-return
// invariant.
func TestLoad_AppleSSO_DisabledIgnoresStaleLiteralPEM(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "false")
	t.Setenv("APPLE_PRIVATE_KEY", string(generateTestApplePEM(t)))

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.AppleSSO.Enabled)
	assert.Empty(t, cfg.AppleSSO.PrivateKey,
		"disabled SSO must not populate PrivateKey from a literal PEM env var")
}

// TestLoad_AppleSSO_EnabledEmptyAtPrefix_TreatedAsMissing pins the operator-
// friendly behavior where APPLE_PRIVATE_KEY="@" or "@   " (a malformed
// at-prefix with empty/whitespace path) is treated as missing rather than
// passed to os.ReadFile(""), which would surface as a confusing "no such
// file or directory" error.
func TestLoad_AppleSSO_EnabledEmptyAtPrefix_TreatedAsMissing(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", "@   ") // malformed: @-prefix with whitespace path

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_PRIVATE_KEY",
		"malformed @-prefix must surface as missing-PrivateKey, not a confusing file-IO error")
}

func TestLoad_AppleSSO_EnabledMissingClientID(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", string(generateTestApplePEM(t)))

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_CLIENT_ID")
}

func TestLoad_AppleSSO_EnabledMissingTeamID(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", string(generateTestApplePEM(t)))

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_TEAM_ID")
}

func TestLoad_AppleSSO_EnabledMissingKeyID(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "")
	t.Setenv("APPLE_PRIVATE_KEY", string(generateTestApplePEM(t)))

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_KEY_ID")
}

func TestLoad_AppleSSO_EnabledMissingPrivateKey(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", "")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_PRIVATE_KEY")
}

// TestLoad_AppleSSO_EnabledWhitespaceOnlyPrivateKey_TreatedAsMissing pins
// the operator-friendly behavior that whitespace-only env values surface as
// the missing-field error rather than passing through to NewAppleProvider's
// PEM parser, which would emit a confusing "PEM block decode failed" far
// from the actual misconfig source.
func TestLoad_AppleSSO_EnabledWhitespaceOnlyPrivateKey_TreatedAsMissing(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "KEYID12345")
	t.Setenv("APPLE_PRIVATE_KEY", "   \t\n  ") // whitespace only

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_PRIVATE_KEY",
		"whitespace-only PrivateKey should be treated as missing, not as a literal value to pass through to PEM parsing")
}

func TestLoad_AppleSSO_EnabledMissingMultiple(t *testing.T) {
	// ClientID + TeamID set; KeyID + PrivateKey unset → error must mention BOTH.
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.concordvoice.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123ABC")
	t.Setenv("APPLE_KEY_ID", "")
	t.Setenv("APPLE_PRIVATE_KEY", "")

	_, err := Load()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "APPLE_KEY_ID", "error must list KeyID as missing")
	assert.Contains(t, msg, "APPLE_PRIVATE_KEY", "error must list PrivateKey as missing — accumulating guard, not first-fail")
}

func TestAppleSSOConfig_String_Disabled(t *testing.T) {
	cfg := AppleSSOConfig{Enabled: false}
	assert.Equal(t, "AppleSSOConfig{Enabled:false}", cfg.String())
}

func TestAppleSSOConfig_String_RedactsPrivateKey(t *testing.T) {
	privKey := generateTestApplePEM(t)
	cfg := AppleSSOConfig{
		Enabled:    true,
		ClientID:   "chat.concordvoice.signin",
		TeamID:     "TEAM123ABC",
		KeyID:      "KEYID12345",
		PrivateKey: privKey,
	}
	output := cfg.String()

	// PEM bytes must NEVER appear in the redacted output.
	assert.NotContains(t, output, string(privKey), "raw PEM bytes must not leak through String()")
	assert.NotContains(t, output, "PRIVATE KEY", "PEM markers must not leak through String()")

	// Non-secret fields are visible — operators need them for log triage.
	assert.Contains(t, output, "chat.concordvoice.signin")
	assert.Contains(t, output, "TEAM123ABC")
	assert.Contains(t, output, "KEYID12345")

	// Redaction marker includes the byte count so operators can distinguish
	// empty-vs-populated keys without exposing the bytes themselves.
	assert.Regexp(t, regexp.MustCompile(`REDACTED \d+ bytes`), output, "redaction marker must include byte count")
}

// TestLoadDefaultAllowedOriginsIncludesAppScheme pins the dev/CI default for
// ALLOWED_ORIGINS to include the desktop client's app://concord origin
// alongside the existing localhost dev entries. Production overrides via
// env var, so this default is purely a non-production safety net — without
// app://concord in the default, the bundled-SPA fallback path's CORS
// preflight (Origin: app://concord) is silently rejected in dev/CI (#830).
//
// The defensive localhost:3001 assertion guards against accidental
// regression that would break dev-mode CORS for the Vite dev server.
func TestLoadDefaultAllowedOriginsIncludesAppScheme(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development") // skip production guards
	t.Setenv("ALLOWED_ORIGINS", "")        // unset → use default
	cfg, err := Load()
	require.NoError(t, err)
	assert.Contains(t, cfg.AllowedOrigins, "app://concord")
	// Defensive: verify dev defaults still present
	assert.Contains(t, cfg.AllowedOrigins, "http://localhost:3001")
}

// TestLoadAppleSSOConfig_FilePathSyntax verifies that APPLE_PRIVATE_KEY
// values prefixed with @ are resolved by reading the file at the given
// path. Closes coverage gap on config.go:280-293.
func TestLoadAppleSSOConfig_FilePathSyntax(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "apple.p8")
	require.NoError(t, os.WriteFile(tmpFile, []byte("test-pem-bytes"), 0o600))

	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.test.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123456")
	t.Setenv("APPLE_KEY_ID", "KEY1234567")
	t.Setenv("APPLE_PRIVATE_KEY", "@"+tmpFile)

	cfg := &Config{}
	require.NoError(t, loadAppleSSOConfig(cfg))
	assert.Equal(t, []byte("test-pem-bytes"), cfg.AppleSSO.PrivateKey)
	assert.True(t, cfg.AppleSSO.Enabled)
	assert.Equal(t, "chat.test.signin", cfg.AppleSSO.ClientID)
	assert.Equal(t, "TEAM123456", cfg.AppleSSO.TeamID)
	assert.Equal(t, "KEY1234567", cfg.AppleSSO.KeyID)
}

// TestLoadAppleSSOConfig_FilePathMissing verifies that a non-existent
// @/path returns a wrapped fs error rather than silently leaving
// PrivateKey empty (which would surface later as an opaque PEM-parse
// error far from the actual misconfig). Closes coverage gap on
// config.go:289-291.
func TestLoadAppleSSOConfig_FilePathMissing(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.test.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123456")
	t.Setenv("APPLE_KEY_ID", "KEY1234567")
	t.Setenv("APPLE_PRIVATE_KEY", "@/nonexistent/path/apple.p8")

	cfg := &Config{}
	err := loadAppleSSOConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_PRIVATE_KEY file:")
}

// TestLoadAppleSSOConfig_FilePathWhitespaceOnly verifies that
// APPLE_PRIVATE_KEY="@   " (whitespace after the @-prefix) is treated
// as a missing-key error rather than being silently passed through.
// Closes coverage gap on config.go:282-283 (the strings.TrimSpace gate
// on the after-@ portion).
func TestLoadAppleSSOConfig_FilePathWhitespaceOnly(t *testing.T) {
	t.Setenv("APPLE_SSO_ENABLED", "true")
	t.Setenv("APPLE_CLIENT_ID", "chat.test.signin")
	t.Setenv("APPLE_TEAM_ID", "TEAM123456")
	t.Setenv("APPLE_KEY_ID", "KEY1234567")
	t.Setenv("APPLE_PRIVATE_KEY", "@   ")

	cfg := &Config{}
	err := loadAppleSSOConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APPLE_PRIVATE_KEY")
	assert.Nil(t, cfg.AppleSSO.PrivateKey)
}

// TestParseAttestationTokenTTL_Default verifies the default "2h" TTL parses correctly.
func TestParseAttestationTokenTTL_Default(t *testing.T) {
	require.Equal(t, 2*time.Hour, parseAttestationTokenTTL("2h"))
}

// TestParseAttestationTokenTTL_TooShort uses the os/exec self-invocation pattern
// to test that parseAttestationTokenTTL calls log.Fatalf on out-of-range values.
// The function is expected to terminate the process; we re-exec the test binary
// with an env flag to trigger the fatal path inside a child process.
func TestParseAttestationTokenTTL_TooShort(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		parseAttestationTokenTTL("10m")
		return
	}
	// os.Executable returns the path to the running test binary — not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary path), not user-controlled input; self-invocation pattern for testing log.Fatalf
	cmd := exec.Command(testBin, "-test.run=TestParseAttestationTokenTTL_TooShort") //nolint:gosec // G204: same rationale
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	err = cmd.Run()
	require.Error(t, err, "expected fatal exit on too-short TTL")
}

// TestParseAttestationPruneInterval_Default verifies the default "6h" interval
// parses correctly. Per finding #18 of #1264 review: the cleanup pruner now
// runs at this cadence by default.
func TestParseAttestationPruneInterval_Default(t *testing.T) {
	require.Equal(t, 6*time.Hour, parseAttestationPruneInterval("6h"))
}

// TestParseAttestationPruneInterval_AcceptsBounds verifies the [1h, 24h] range
// boundaries are inclusive.
func TestParseAttestationPruneInterval_AcceptsBounds(t *testing.T) {
	require.Equal(t, 1*time.Hour, parseAttestationPruneInterval("1h"))
	require.Equal(t, 24*time.Hour, parseAttestationPruneInterval("24h"))
}

// TestParseAttestationPruneInterval_TooShort uses the os/exec self-invocation
// pattern to test that parseAttestationPruneInterval calls log.Fatalf on
// out-of-range input.
func TestParseAttestationPruneInterval_TooShort(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		parseAttestationPruneInterval("10m")
		return
	}
	testBin, err := os.Executable()
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary path), not user-controlled input; self-invocation pattern for testing log.Fatalf
	cmd := exec.Command(testBin, "-test.run=TestParseAttestationPruneInterval_TooShort") //nolint:gosec // G204: same rationale
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	err = cmd.Run()
	require.Error(t, err, "expected fatal exit on too-short prune interval")
}

// TestParseAttestationPruneInterval_TooLong covers the upper-bound guard.
func TestParseAttestationPruneInterval_TooLong(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		parseAttestationPruneInterval("48h")
		return
	}
	testBin, err := os.Executable()
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary path), not user-controlled input; self-invocation pattern for testing log.Fatalf
	cmd := exec.Command(testBin, "-test.run=TestParseAttestationPruneInterval_TooLong") //nolint:gosec // G204: same rationale
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	err = cmd.Run()
	require.Error(t, err, "expected fatal exit on too-long prune interval")
}

// TestParseAttestationPruneInterval_Invalid covers the parse-failure path.
func TestParseAttestationPruneInterval_Invalid(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		parseAttestationPruneInterval("not-a-duration")
		return
	}
	testBin, err := os.Executable()
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary path), not user-controlled input; self-invocation pattern for testing log.Fatalf
	cmd := exec.Command(testBin, "-test.run=TestParseAttestationPruneInterval_Invalid") //nolint:gosec // G204: same rationale
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	err = cmd.Run()
	require.Error(t, err, "expected fatal exit on invalid duration")
}

// TestValidate_RequireAttestation_MissingAudience verifies that enabling client
// attestation without an OIDC audience returns a descriptive error.
// Uses validateAttestation() directly to avoid needing a fully-populated
// production Config (which would require satisfying all other production guards).
func TestValidate_RequireAttestation_MissingAudience(t *testing.T) {
	cfg := &Config{RequireClientAttestation: true, OIDCAudience: ""}
	err := cfg.validateAttestation()
	require.ErrorContains(t, err, "ATTESTATION_OIDC_AUDIENCE")
}

// validAttestationConfig returns the minimum set of attestation fields that
// satisfy validateAttestation when RequireClientAttestation=true. Tests
// mutate one field at a time to assert each per-axis production guard fires.
func validAttestationConfig() *Config {
	return &Config{
		Environment:              "development", // bypass the production issuer check
		RequireClientAttestation: true,
		OIDCIssuer:               "https://token.actions.githubusercontent.com",
		OIDCAudience:             "https://api.concordvoice.chat",
		OIDCSubjectPrefix:        "repo:markdrogersjr/Concord:",
		OIDCSPAWorkflow:          "main-cd.yml",
		OIDCSPARef:               "refs/heads/main",
		OIDCBinaryWorkflow:       "build-desktop.yml",
		OIDCBinaryRef:            "refs/heads/main",
	}
}

// TestValidateAttestation_DisabledSkipsAllChecks verifies the
// RequireClientAttestation=false early-return.
func TestValidateAttestation_DisabledSkipsAllChecks(t *testing.T) {
	cfg := &Config{RequireClientAttestation: false}
	require.NoError(t, cfg.validateAttestation())
}

// TestValidateAttestation_HappyPath verifies the success branch — all per-axis
// fields populated.
func TestValidateAttestation_HappyPath(t *testing.T) {
	require.NoError(t, validAttestationConfig().validateAttestation())
}

// TestValidateAttestation_ProductionIssuerCheck covers the production-only
// guard that ATTESTATION_OIDC_ISSUER MUST be the canonical GitHub URL.
func TestValidateAttestation_ProductionIssuerCheck(t *testing.T) {
	cfg := validAttestationConfig()
	cfg.Environment = "production"
	cfg.OIDCIssuer = "https://attacker.example.com"
	err := cfg.validateAttestation()
	require.ErrorContains(t, err, "canonical GitHub issuer in production")
}

// TestValidateAttestation_MissingSPAWorkflow covers the SPA-axis workflow
// guard.
func TestValidateAttestation_MissingSPAWorkflow(t *testing.T) {
	cfg := validAttestationConfig()
	cfg.OIDCSPAWorkflow = ""
	err := cfg.validateAttestation()
	require.ErrorContains(t, err, "ATTESTATION_OIDC_SPA_WORKFLOW")
}

// TestValidateAttestation_MissingSPARef covers the SPA-axis ref guard.
func TestValidateAttestation_MissingSPARef(t *testing.T) {
	cfg := validAttestationConfig()
	cfg.OIDCSPARef = ""
	err := cfg.validateAttestation()
	require.ErrorContains(t, err, "ATTESTATION_OIDC_SPA_REF")
}

// TestValidateAttestation_MissingBinaryWorkflow covers the binary-axis
// workflow guard.
func TestValidateAttestation_MissingBinaryWorkflow(t *testing.T) {
	cfg := validAttestationConfig()
	cfg.OIDCBinaryWorkflow = ""
	err := cfg.validateAttestation()
	require.ErrorContains(t, err, "ATTESTATION_OIDC_BINARY_WORKFLOW")
}

// TestValidateAttestation_MissingBinaryRef covers the binary-axis ref guard.
func TestValidateAttestation_MissingBinaryRef(t *testing.T) {
	cfg := validAttestationConfig()
	cfg.OIDCBinaryRef = ""
	err := cfg.validateAttestation()
	require.ErrorContains(t, err, "ATTESTATION_OIDC_BINARY_REF")
}

// TestValidate_WarnsOnBroadRFC1918Fallback verifies that production with
// the broad RFC1918 fallback for TRUSTED_PROXY_CIDRS does NOT fail
// startup but emits a warning log line. The test captures the standard
// log output via a buffer.
func TestValidate_WarnsOnBroadRFC1918Fallback(t *testing.T) {
	var buf bytes.Buffer
	oldFlags := log.Flags()
	oldOutput := log.Writer()
	log.SetFlags(0)
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetFlags(oldFlags)
		log.SetOutput(oldOutput)
	})

	// Build a config that satisfies all production guards.
	// Use concatenation for URLs so detect-secrets doesn't flag embedded basic-auth.
	pw := "pass"                                          //nolint:gosec // test fixture, not a real credential
	dbURL := "postgres://prod:" + pw + "@db:5432/concord" //nolint:gosec // test fixture
	redisURL := "redis://:" + pw + "@redis:6379"          //nolint:gosec // test fixture
	cfg := &Config{                                       //nolint:gosec // G101: test fixture with fake values, not real credentials
		Environment:       "production",
		JWTSecret:         "real-secret-not-default", //nolint:gosec // G101 false positive: test fixture
		DatabaseURL:       dbURL,
		RedisURL:          redisURL,
		MFAEncryptionKey:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MinIOAccessKey:    "ak",
		MinIOSecretKey:    "real-minio-secret",
		MinIOEndpoint:     "minio:9000",
		MinIOBucket:       "concord-media",
		SMTPHost:          "smtp.example.com",
		TrustedProxyCIDRs: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
		MediaPlaneURL:     "https://media.concordvoice.chat",
		// #158 — feedback handler production guards.
		GitHubFeedback: GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture with fake PAT
			Token: "ghp_real_production_pat", // #nosec G101 -- test fixture, not a real PAT
			Repo:  "Concord-Voice/Concord-Voice-Feedback",
		},
		// #1688 — admin WebAuthn production guards.
		AdminWebAuthnRPID:           "admin.concordvoice.chat",
		AdminWebAuthnRPOrigins:      []string{"https://admin.concordvoice.chat"},
		AdminWebAuthnAllowedAAGUIDs: []string{"ee882879-721c-4913-9775-3dfcce97072a"},
	}

	err := cfg.validate()
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "TRUSTED_PROXY_CIDRS is the broad RFC1918 fallback")
}

// GitHub Feedback config tests (#158).
//
// PR #1547 review: the PAT must never leak through %+v dumps anywhere.
// Mirrors the AppleSSO / CloudflareKVBridge redacting-String pattern.

func TestGitHubFeedbackConfig_String_Redacts_Token(t *testing.T) {
	cfg := GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture with fake PAT
		Token: "ghp_1234567890abcdef1234567890abcdef12345678", // #nosec G101 -- test fixture, not a real PAT // pragma: allowlist secret
		Repo:  "Concord-Voice/Concord-Voice-Feedback",
	}
	s := cfg.String()
	assert.NotContains(t, s, "ghp_1234567890abcdef1234567890abcdef12345678", // pragma: allowlist secret
		"PAT must NEVER appear in String() output")
	assert.Contains(t, s, "[REDACTED")
	assert.Contains(t, s, "Concord-Voice/Concord-Voice-Feedback",
		"Repo slug is non-sensitive and SHOULD appear in String() for diagnostics")
}

func TestGitHubFeedbackConfig_String_UnsetTokenSentinel(t *testing.T) {
	cfg := GitHubFeedbackConfig{Token: "", Repo: "owner/repo"}
	s := cfg.String()
	assert.Contains(t, s, "[unset]")
	assert.NotContains(t, s, "REDACTED",
		"empty token should render as [unset], not [REDACTED 0 bytes]")
}

func TestGitHubFeedbackConfig_String_FormatV_DoesNotLeak(t *testing.T) {
	cfg := GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture with fake PAT
		Token: "ghp_sensitive_pat_value_zzzzzzzzzzzzzzzzzz", // #nosec G101 -- test fixture, not a real PAT // pragma: allowlist secret
		Repo:  "owner/repo",
	}
	// The whole point: %+v on a Config containing GitHubFeedbackConfig
	// must invoke the redacting String() method, never print the raw
	// struct field.
	out := fmt.Sprintf("%+v", cfg)
	assert.NotContains(t, out, "ghp_sensitive_pat_value_zzzzzzzzzzzzzzzzzz") // pragma: allowlist secret
}

// Cloudflare KV bridge config tests (#973).
//
// Mirrors the AppleSSO gate shape: disabled => zero-value group; enabled =>
// all three credential fields required, every missing field reported at once.

func TestLoad_CloudflareKVBridge_Disabled(t *testing.T) {
	t.Setenv("CLOUDFLARE_KV_BRIDGE_ENABLED", "false")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "")
	t.Setenv("CLOUDFLARE_KV_API_TOKEN", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.CloudflareKVBridge.Enabled)
	assert.Empty(t, cfg.CloudflareKVBridge.APIToken)
}

func TestLoad_CloudflareKVBridge_DefaultIsDisabled(t *testing.T) {
	t.Setenv("CLOUDFLARE_KV_BRIDGE_ENABLED", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.CloudflareKVBridge.Enabled)
}

func TestLoad_CloudflareKVBridge_EnabledWithAllFields(t *testing.T) {
	t.Setenv("CLOUDFLARE_KV_BRIDGE_ENABLED", "true")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct-123")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "ns-456")
	t.Setenv("CLOUDFLARE_KV_API_TOKEN", "test-token-789")

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.CloudflareKVBridge.Enabled)
	assert.Equal(t, "acct-123", cfg.CloudflareKVBridge.AccountID)
	assert.Equal(t, "ns-456", cfg.CloudflareKVBridge.NamespaceID)
	assert.Equal(t, "test-token-789", cfg.CloudflareKVBridge.APIToken)
}

func TestLoad_CloudflareKVBridge_EnabledMissingAllFields_ListsEveryName(t *testing.T) {
	t.Setenv("CLOUDFLARE_KV_BRIDGE_ENABLED", "true")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "")
	t.Setenv("CLOUDFLARE_KV_API_TOKEN", "")

	_, err := Load()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "CLOUDFLARE_ACCOUNT_ID", "error must list AccountID as missing")
	assert.Contains(t, msg, "CLOUDFLARE_KV_NAMESPACE_ID", "error must list NamespaceID as missing")
	assert.Contains(t, msg, "CLOUDFLARE_KV_API_TOKEN", "error must list APIToken as missing — accumulating guard, not first-fail")
}

func TestLoad_CloudflareKVBridge_EnabledMissingToken_OnlyTokenListed(t *testing.T) {
	t.Setenv("CLOUDFLARE_KV_BRIDGE_ENABLED", "true")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct-123")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "ns-456")
	t.Setenv("CLOUDFLARE_KV_API_TOKEN", "")

	_, err := Load()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "CLOUDFLARE_KV_API_TOKEN")
	assert.NotContains(t, msg, "CLOUDFLARE_ACCOUNT_ID,", "present fields must not be listed as missing")
}

func TestCloudflareKVBridgeConfig_String_Disabled(t *testing.T) {
	cfg := CloudflareKVBridgeConfig{Enabled: false}
	assert.Equal(t, "CloudflareKVBridgeConfig{Enabled:false}", cfg.String())
}

func TestCloudflareKVBridgeConfig_String_RedactsToken(t *testing.T) {
	cfg := CloudflareKVBridgeConfig{
		Enabled:     true,
		AccountID:   "acct-123",
		NamespaceID: "ns-456",
		APIToken:    "super-secret-cf-token",
	}
	output := cfg.String()

	assert.NotContains(t, output, "super-secret-cf-token", "token must never leak through String()")
	assert.Contains(t, output, "acct-123")
	assert.Contains(t, output, "ns-456")
	assert.Contains(t, output, "REDACTED")
}
