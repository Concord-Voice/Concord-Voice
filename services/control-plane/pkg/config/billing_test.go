package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// billingTestEnv lists every variable the billing rules read. Each case starts
// with all of them empty, which LoadBillingConfig and check-billing-env.sh both
// treat exactly as unset.
var billingTestEnv = []string{
	"ENVIRONMENT", "INSTANCE_TYPE",
	"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET",
	"BILLING_PURCHASE_MODE", "BILLING_PURCHASE_ALLOWLIST",
	"FOUNDERS_PRICING_ENDS_AT", "STRIPE_AUTOMATIC_TAX",
}

func clearBillingEnv(t *testing.T) {
	t.Helper()
	for _, key := range billingTestEnv {
		t.Setenv(key, "")
	}
}

// billingCaseEscapes decodes the case table's value escapes. The shell side
// decodes the same three with printf %b.
var billingCaseEscapes = strings.NewReplacer(`\r`, "\r", `\n`, "\n", `\t`, "\t")

// billingEnvCaseCount is the number of non-comment rows in
// testdata/billing_env_cases.tsv. The shell twin
// ([internal]tests/test-billing-env.sh, Task 9) asserts the same
// count, so a truncated or partly commented-out table fails both readers
// instead of shrinking the shared contract unnoticed.
const billingEnvCaseCount = 82

// posixSpace is spelled out here rather than reusing billingSpace, so that a
// change to the production trim set fails these tests instead of moving the
// expectation with it.
const posixSpace = " \t\n\v\f\r"

type billingEnvCase struct {
	name    string
	outcome string
	expect  string
	env     map[string]string
}

// readBillingEnvCases parses testdata/billing_env_cases.tsv and fails on any
// row the shell reader would see differently: an empty field, an unknown
// variable, a stray backslash or a line ending in whitespace.
func readBillingEnvCases(t *testing.T) []billingEnvCase {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "billing_env_cases.tsv"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	var cases []billingEnvCase
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		require.Falsef(t, strings.HasSuffix(text, " ") || strings.HasSuffix(text, "\t"),
			"line %d ends in whitespace, which the trailing-whitespace hook would strip", line)
		fields := strings.Split(text, "\t")
		require.GreaterOrEqualf(t, len(fields), 3, "line %d: want name, outcome, expect", line)
		for _, field := range fields {
			require.NotEmptyf(t, field, "line %d has an empty field; the shell reader collapses those", line)
		}
		tc := billingEnvCase{name: fields[0], outcome: fields[1], expect: fields[2], env: map[string]string{}}
		require.Falsef(t, seen[tc.name], "line %d repeats case %q", line, tc.name)
		seen[tc.name] = true
		for _, assignment := range fields[3:] {
			key, raw, ok := strings.Cut(assignment, "=")
			require.Truef(t, ok && slices.Contains(billingTestEnv, key), "line %d: bad assignment %q", line, assignment)
			value := billingCaseEscapes.Replace(raw)
			require.NotContainsf(t, value, `\`, "line %d: only \\r, \\n and \\t escapes are allowed", line)
			tc.env[key] = value
		}
		cases = append(cases, tc)
	}
	require.NoError(t, scanner.Err())
	require.Len(t, cases, billingEnvCaseCount,
		"the case table must hold exactly %d rows; if you added or removed one, change billingEnvCaseCount and the shell twin's count together", billingEnvCaseCount)
	return cases
}

// TestBillingEnvCases runs every row of the shared case table through
// LoadBillingConfig. [internal]tests/test-billing-env.sh runs the
// same rows through check-billing-env.sh, so the two cannot drift (R9).
func TestBillingEnvCases(t *testing.T) {
	for _, tc := range readBillingEnvCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			clearBillingEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			cfg, err := LoadBillingConfig(getEnv("ENVIRONMENT", "production"), getEnv("INSTANCE_TYPE", "saas"))
			assertBillingCase(t, tc, cfg, err)
		})
	}
}

// assertBillingCase checks one row's outcome. It is split out of
// TestBillingEnvCases so that both stay within the cognitive-complexity
// ceiling of 15, which gocognit and SonarQube apply to _test.go files too.
func assertBillingCase(t *testing.T, tc billingEnvCase, cfg BillingConfig, err error) {
	t.Helper()
	key := strings.Trim(tc.env["STRIPE_SECRET_KEY"], posixSpace)
	whsec := strings.Trim(tc.env["STRIPE_WEBHOOK_SECRET"], posixSpace)
	var secrets []string
	for _, value := range []string{key, whsec} {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	switch tc.outcome {
	case "ok":
		require.NoError(t, err)
		if tc.expect != "-" {
			assert.Contains(t, cfg.Summary(), tc.expect)
		}
		if cfg.SelfHosted {
			assert.Empty(t, cfg.SecretKey, "a self-hosted instance must drop the key")
			assert.Empty(t, cfg.WebhookSecret, "a self-hosted instance must drop the webhook secret")
		} else {
			assert.Equal(t, key, cfg.SecretKey, "the stored key must be trimmed")
			assert.Equal(t, whsec, cfg.WebhookSecret, "the stored webhook secret must be trimmed")
		}
		for _, secret := range secrets { // pragma: allowlist secret
			assert.NotContains(t, cfg.String(), secret)
		}
	case "fatal":
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.expect)
		for _, secret := range secrets { // pragma: allowlist secret
			assert.NotContains(t, err.Error(), secret)
		}
	default:
		t.Fatalf("unknown outcome %q", tc.outcome)
	}
}

// TestLoadBillingConfigStoresAllowlistLowerCase pins rule 2's stored form.
// Summary() prints only the entry count, so no table row can see it. A later
// PR compares these entries with lower-case user UUIDs, so an upper-case entry
// would silently never match.
func TestLoadBillingConfigStoresAllowlistLowerCase(t *testing.T) {
	clearBillingEnv(t)
	t.Setenv("BILLING_PURCHASE_ALLOWLIST", " 3F1C2B4A-5D6E-4F70-8A91-B2C3D4E5F607 ,0a1B2c3D-4e5F-4061-8273-94A5b6C7d8E9")
	cfg, err := LoadBillingConfig("development", "saas")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"3f1c2b4a-5d6e-4f70-8a91-b2c3d4e5f607",
		"0a1b2c3d-4e5f-4061-8273-94a5b6c7d8e9",
	}, cfg.PurchaseAllowlist, "entries are trimmed and lower-cased, in input order")
}

func TestParseStripeKeyMode(t *testing.T) {
	tests := []struct {
		name           string
		key            string
		wantMode       StripeKeyMode
		wantRestricted bool
		wantErr        bool
	}{
		{name: "empty is none", key: "", wantMode: StripeKeyModeNone},
		{name: "restricted test", key: "rk_test_fake", wantMode: StripeKeyModeTest, wantRestricted: true},
		{name: "restricted live", key: "rk_live_fake", wantMode: StripeKeyModeLive, wantRestricted: true},
		{name: "secret test", key: "sk_test_fake", wantMode: StripeKeyModeTest},
		{name: "secret live", key: "sk_live_fake", wantMode: StripeKeyModeLive},
		{name: "publishable test", key: "pk_test_fake", wantErr: true},
		{name: "publishable live", key: "pk_live_fake", wantErr: true},
		{name: "webhook secret", key: "whsec_fake", wantErr: true},
		{name: "upper-case prefix", key: "SK_TEST_FAKE", wantErr: true},
		{name: "unknown mode", key: "sk_prod_fake", wantErr: true},
		{name: "no trimming here", key: " sk_test_fake", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mode, restricted, err := ParseStripeKeyMode(tc.key)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidStripeKey)
				assert.NotContains(t, err.Error(), tc.key)
				assert.Equal(t, StripeKeyModeNone, mode)
				assert.False(t, restricted)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, mode)
			assert.Equal(t, tc.wantRestricted, restricted)
		})
	}
}

func TestBillingConfigStringRedactsSecrets(t *testing.T) {
	t.Run("set secrets print only their length", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("STRIPE_SECRET_KEY", "rk_test_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := LoadBillingConfig("development", "saas")
		require.NoError(t, err)

		out := cfg.String()
		assert.NotContains(t, out, "rk_test_fakekey")
		assert.NotContains(t, out, "whsec_fakesig")
		assert.Contains(t, out, "secret_key:[REDACTED 15 bytes]")
		assert.Contains(t, out, "webhook_secret:[REDACTED 13 bytes]")
		assert.Contains(t, out, "key_mode=test key_restricted=true")
	})
	t.Run("unset secrets say so", func(t *testing.T) {
		assert.Contains(t, BillingConfig{}.String(), "secret_key:[unset] webhook_secret:[unset]")
	})
}

// TestBillingConfigSecretsStayOutOfStructuredOutput pins the sinks String()
// does not cover. Production logs through slog.NewJSONHandler, which marshals
// an Any value with encoding/json and ignores Stringer; %#v and json.Marshal
// walk the exported fields the same way. Each sink gets a positive control, so
// an empty or unrelated output cannot satisfy the NotContains checks.
func TestBillingConfigSecretsStayOutOfStructuredOutput(t *testing.T) {
	clearBillingEnv(t)
	const (
		fakeKey = "rk_live_fakekey" // pragma: allowlist secret
		fakeSig = "whsec_fakesig"   // pragma: allowlist secret
	)
	t.Setenv("STRIPE_SECRET_KEY", fakeKey)
	t.Setenv("STRIPE_WEBHOOK_SECRET", fakeSig)
	cfg, err := LoadBillingConfig("production", "saas")
	require.NoError(t, err)
	require.Equal(t, fakeKey, cfg.SecretKey, "the fixture must carry the secrets the sinks are checked for")
	require.Equal(t, fakeSig, cfg.WebhookSecret)

	var jsonLog, textLog bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("x", "billing", cfg)
	slog.New(slog.NewTextHandler(&textLog, nil)).Info("x", "billing", cfg)
	marshalled, err := json.Marshal(cfg)
	require.NoError(t, err)

	sinks := []struct {
		name    string
		out     string
		control string
	}{
		{"slog JSON handler", jsonLog.String(), "secret_key:[REDACTED 15 bytes]"},
		{"slog text handler", textLog.String(), "secret_key:[REDACTED 15 bytes]"},
		{"json.Marshal", string(marshalled), `"PurchaseMode"`},
		{"%#v", fmt.Sprintf("%#v", cfg), "webhook_secret:[REDACTED 13 bytes]"},
	}
	for _, sink := range sinks {
		t.Run(sink.name, func(t *testing.T) {
			assert.Contains(t, sink.out, sink.control, "the sink must have rendered the config")
			assert.NotContains(t, sink.out, fakeKey)
			assert.NotContains(t, sink.out, fakeSig)
		})
	}
	assert.NotContains(t, string(marshalled), "SecretKey", "json:\"-\" omits the field, not only its value")
	assert.NotContains(t, string(marshalled), "WebhookSecret")
}

func TestBillingConfigLogBoot(t *testing.T) {
	t.Run("logs mode, key class and cutoff, never the key", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("STRIPE_SECRET_KEY", "rk_live_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		t.Setenv("BILLING_PURCHASE_MODE", "allowlist")
		t.Setenv("BILLING_PURCHASE_ALLOWLIST", "3f1c2b4a-5d6e-4f70-8a91-b2c3d4e5f607")
		t.Setenv("FOUNDERS_PRICING_ENDS_AT", "2027-05-01T00:00:00Z")
		cfg, err := LoadBillingConfig("production", "saas")
		require.NoError(t, err)

		var buf bytes.Buffer
		cfg.LogBoot(slog.New(slog.NewTextHandler(&buf, nil)))
		out := buf.String()
		assert.Contains(t, out, `msg="billing configuration"`)
		assert.Contains(t, out, "purchase_mode=allowlist")
		assert.Contains(t, out, "key_mode=live")
		assert.Contains(t, out, "key_restricted=true")
		assert.Contains(t, out, "founders_pricing_ends_at=2027-05-01T00:00:00Z")
		assert.Contains(t, out, "allowlist_entries=1")
		assert.Contains(t, out, "self_hosted=false")
		assert.NotContains(t, out, "level=WARN")
		assert.NotContains(t, out, "rk_live_fakekey")
		assert.NotContains(t, out, "whsec_fakesig")
	})
	t.Run("warns about an unrestricted sk_live_ key in production", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("STRIPE_SECRET_KEY", "sk_live_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := LoadBillingConfig("production", "saas")
		require.NoError(t, err)

		var buf bytes.Buffer
		cfg.LogBoot(slog.New(slog.NewTextHandler(&buf, nil)))
		out := buf.String()
		assert.Contains(t, out, "key_mode=live")
		assert.Contains(t, out, "key_restricted=false")
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, "unrestricted live key")
		assert.NotContains(t, out, "sk_live_fakekey")
		assert.NotContains(t, out, "whsec_fakesig")
	})
	t.Run("an rk_live_ key does not warn", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("STRIPE_SECRET_KEY", "rk_live_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := LoadBillingConfig("production", "saas")
		require.NoError(t, err)

		var buf bytes.Buffer
		cfg.LogBoot(slog.New(slog.NewTextHandler(&buf, nil)))
		assert.Contains(t, buf.String(), "key_restricted=true")
		assert.NotContains(t, buf.String(), "unrestricted live key")
	})
	t.Run("an sk_test_ key does not warn", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("STRIPE_SECRET_KEY", "sk_test_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := LoadBillingConfig("development", "saas")
		require.NoError(t, err)

		var buf bytes.Buffer
		cfg.LogBoot(slog.New(slog.NewTextHandler(&buf, nil)))
		assert.NotContains(t, buf.String(), "unrestricted live key")
	})
	t.Run("self-host logs billing off and warns about ignored keys", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("STRIPE_SECRET_KEY", "rk_live_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := LoadBillingConfig("production", "Self-Hosted")
		require.NoError(t, err)

		var buf bytes.Buffer
		cfg.LogBoot(slog.New(slog.NewTextHandler(&buf, nil)))
		out := buf.String()
		assert.Contains(t, out, "purchase_mode=off")
		assert.Contains(t, out, "key_mode=none")
		assert.Contains(t, out, "founders_pricing_ends_at=unset")
		assert.Contains(t, out, "self_hosted=true")
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, "STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET are ignored on self-hosted instances")
		assert.NotContains(t, out, "rk_live_fakekey")
		assert.NotContains(t, out, "whsec_fakesig")
	})
}

func TestLoadWiresBillingConfig(t *testing.T) {
	t.Run("valid settings reach Config.Billing", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("STRIPE_SECRET_KEY", "rk_test_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, BillingPurchaseModeOff, cfg.Billing.PurchaseMode)
		assert.Equal(t, StripeKeyModeTest, cfg.Billing.KeyMode)
		assert.True(t, cfg.Billing.KeyRestricted)
	})
	t.Run("an invalid setting fails Load without echoing the key", func(t *testing.T) {
		clearBillingEnv(t)
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("STRIPE_SECRET_KEY", "rk_live_fakekey")
		t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fakesig")
		cfg, err := Load()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "a live STRIPE_SECRET_KEY requires ENVIRONMENT=production")
		assert.NotContains(t, err.Error(), "rk_live_fakekey")
	})
}
