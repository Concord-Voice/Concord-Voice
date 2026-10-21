package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
)

// StripeKeyMode is the Stripe mode a secret or restricted key belongs to,
// read from its prefix (spec §11). It never carries the key itself.
type StripeKeyMode string

// Stripe key modes. StripeKeyModeNone means no key is configured.
const (
	StripeKeyModeNone StripeKeyMode = ""
	StripeKeyModeTest StripeKeyMode = "test"
	StripeKeyModeLive StripeKeyMode = "live"
)

// BillingPurchaseMode is BILLING_PURCHASE_MODE: who may start a purchase.
type BillingPurchaseMode string

// Purchase modes. Off is the default and the only mode a self-hosted
// instance accepts.
const (
	BillingPurchaseModeOff       BillingPurchaseMode = "off"
	BillingPurchaseModeAllowlist BillingPurchaseMode = "allowlist"
	BillingPurchaseModeOn        BillingPurchaseMode = "on"
)

// ErrInvalidStripeKey is ParseStripeKeyMode's error for any non-empty value
// that is not a secret or restricted key, a publishable pk_ key included. Its
// text never contains the key.
var ErrInvalidStripeKey = errors.New("config: stripe key must start with rk_test_, rk_live_, sk_test_ or sk_live_")

// billingSpace is what LoadBillingConfig trims from both ends of every billing
// variable: POSIX [:space:] in the C locale, the set check-billing-env.sh
// trims, so a CR or LF left by a dotenv writer is removed the same way on
// both sides.
const billingSpace = " \t\n\v\f\r"

var (
	billingUUIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	// foundersPricingPattern admits only the RFC3339 shape that both
	// validators check the same way: a 20xx date, whole seconds and a Z or
	// in-range ±hh:mm offset. time.Parse then rejects impossible dates such as
	// 2027-02-30. It accepts +24:00 and +05:60, so the offset bounds are
	// enforced by this pattern.
	foundersPricingPattern = regexp.MustCompile(`^20[0-9]{2}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
)

// BillingConfig is the validated Stripe billing configuration (#2033). PR-1
// loads and logs it; later PRs read it to register routes and start workers.
// String redacts both secrets.
type BillingConfig struct {
	PurchaseMode          BillingPurchaseMode
	PurchaseAllowlist     []string  // lower-case user UUIDs
	FoundersPricingEndsAt time.Time // zero when unset
	AutomaticTax          bool
	SecretKey             string `json:"-"` // #nosec G117 -- config field name; the key comes from the environment; json:"-", String(), GoString() and LogValue() keep it out of every output
	KeyMode               StripeKeyMode
	KeyRestricted         bool   // true for an rk_ key
	WebhookSecret         string `json:"-"` // #nosec G117 -- config field name; the secret comes from the environment; json:"-", String(), GoString() and LogValue() keep it out of every output
	SelfHosted            bool
	// SelfHostKeysIgnored records that a self-hosted instance found a key or
	// webhook secret in its environment and dropped it. The base compose file
	// forwards both variables to every instance, and the public self-host
	// overlay merges into it, so this is a warning and not an error.
	SelfHostKeysIgnored bool
}

// ParseStripeKeyMode reads the mode from a key's prefix. An empty key is
// StripeKeyModeNone. rk_test_ and rk_live_ are restricted keys; sk_test_ and
// sk_live_ are secret keys. Anything else, including a publishable pk_ key,
// returns ErrInvalidStripeKey. The key is not trimmed here; LoadBillingConfig
// trims it first.
func ParseStripeKeyMode(key string) (mode StripeKeyMode, restricted bool, err error) {
	switch {
	case key == "":
		return StripeKeyModeNone, false, nil
	case strings.HasPrefix(key, "rk_test_"):
		return StripeKeyModeTest, true, nil
	case strings.HasPrefix(key, "rk_live_"):
		return StripeKeyModeLive, true, nil
	case strings.HasPrefix(key, "sk_test_"):
		return StripeKeyModeTest, false, nil
	case strings.HasPrefix(key, "sk_live_"):
		return StripeKeyModeLive, false, nil
	default:
		return StripeKeyModeNone, false, ErrInvalidStripeKey
	}
}

// LoadBillingConfig reads and validates the six billing variables against the
// already-loaded ENVIRONMENT and INSTANCE_TYPE values (spec §11). It reports
// every problem at once, joined by "; ", and never puts a value in the error.
// Load calls it. TestBillingEnvCases calls it for each row of
// testdata/billing_env_cases.tsv, and [internal]check-billing-env.sh
// applies the same rules to the same rows, so a value that provisioning
// accepts is a value that boot accepts (R9).
func LoadBillingConfig(environment, instanceType string) (BillingConfig, error) {
	var problems billingProblems
	cfg := BillingConfig{
		PurchaseMode:          parsePurchaseMode(billingEnv("BILLING_PURCHASE_MODE"), &problems),
		PurchaseAllowlist:     parsePurchaseAllowlist(billingEnv("BILLING_PURCHASE_ALLOWLIST"), &problems),
		FoundersPricingEndsAt: parseFoundersPricingEndsAt(billingEnv("FOUNDERS_PRICING_ENDS_AT"), &problems),
		AutomaticTax:          parseAutomaticTax(billingEnv("STRIPE_AUTOMATIC_TAX"), &problems),
		SelfHosted:            IsSelfHostedInstance(instanceType),
	}
	key := billingEnv("STRIPE_SECRET_KEY")               // #nosec G101 -- env var name, not a secret
	webhookSecret := billingEnv("STRIPE_WEBHOOK_SECRET") // #nosec G101 -- env var name, not a secret
	if cfg.SelfHosted {
		cfg.applySelfHostedRules(key, webhookSecret, &problems)
	} else {
		cfg.applySaaSKeys(key, webhookSecret, environment == "production", &problems)
	}
	if len(problems) > 0 {
		return BillingConfig{}, fmt.Errorf("invalid billing config: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

// billingProblems collects every validation failure, so that one boot reports
// all of them, as LoadOpsMetricsConfig does.
type billingProblems []string

func (p *billingProblems) add(problem string) { *p = append(*p, problem) }

// billingEnv reads one billing variable with billingSpace trimmed.
func billingEnv(key string) string {
	return strings.Trim(os.Getenv(key), billingSpace)
}

func parsePurchaseMode(raw string, problems *billingProblems) BillingPurchaseMode {
	switch mode := BillingPurchaseMode(raw); mode {
	case "":
		return BillingPurchaseModeOff
	case BillingPurchaseModeOff, BillingPurchaseModeAllowlist, BillingPurchaseModeOn:
		return mode
	default:
		problems.add("BILLING_PURCHASE_MODE must be exactly off, allowlist or on")
		return BillingPurchaseModeOff
	}
}

// parsePurchaseAllowlist checks the allowlist whenever it is set, whatever the
// mode, and reports a bad entry by its 1-based position, never its value.
func parsePurchaseAllowlist(raw string, problems *billingProblems) []string {
	if raw == "" {
		return nil
	}
	entries := strings.Split(raw, ",")
	allowlist := make([]string, 0, len(entries))
	for i, entry := range entries {
		entry = strings.Trim(entry, billingSpace)
		switch {
		case entry == "":
			problems.add("BILLING_PURCHASE_ALLOWLIST has an empty entry")
		case !billingUUIDPattern.MatchString(entry):
			problems.add(fmt.Sprintf("BILLING_PURCHASE_ALLOWLIST entry %d is not a UUID", i+1))
		default:
			allowlist = append(allowlist, strings.ToLower(entry))
		}
	}
	return allowlist
}

func parseFoundersPricingEndsAt(raw string, problems *billingProblems) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if foundersPricingPattern.MatchString(raw) {
		if endsAt, err := time.Parse(time.RFC3339, raw); err == nil {
			return endsAt
		}
	}
	problems.add("FOUNDERS_PRICING_ENDS_AT must be an RFC3339 timestamp with a zone offset, such as 2027-05-01T00:00:00Z")
	return time.Time{}
}

func parseAutomaticTax(raw string, problems *billingProblems) bool {
	switch raw {
	case "", "false":
		return false
	case "true":
		return true
	default:
		problems.add("STRIPE_AUTOMATIC_TAX must be exactly empty, false or true")
		return false
	}
}

// applySelfHostedRules applies the self-host rule: purchase stays off, and a
// key or webhook secret in the environment is dropped without validation.
func (b *BillingConfig) applySelfHostedRules(key, webhookSecret string, problems *billingProblems) {
	if b.PurchaseMode != BillingPurchaseModeOff {
		problems.add("BILLING_PURCHASE_MODE must be off on self-hosted instances")
	}
	b.SelfHostKeysIgnored = key != "" || webhookSecret != ""
}

// applySaaSKeys validates the key pair and what the purchase mode needs.
// A live key is allowed only with ENVIRONMENT=production, and a test key only
// without it (C14).
func (b *BillingConfig) applySaaSKeys(key, webhookSecret string, production bool, problems *billingProblems) {
	mode, restricted, err := ParseStripeKeyMode(key)
	if err != nil {
		problems.add("STRIPE_SECRET_KEY must start with rk_test_, rk_live_, sk_test_ or sk_live_")
	}
	if webhookSecret != "" && !strings.HasPrefix(webhookSecret, "whsec_") {
		problems.add("STRIPE_WEBHOOK_SECRET must start with whsec_")
	}
	if (key == "") != (webhookSecret == "") {
		problems.add("STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET must both be set or both be empty")
	}
	if mode == StripeKeyModeLive && !production {
		problems.add("a live STRIPE_SECRET_KEY requires ENVIRONMENT=production")
	}
	if mode == StripeKeyModeTest && production {
		problems.add("a test STRIPE_SECRET_KEY is not allowed with ENVIRONMENT=production")
	}
	b.SecretKey, b.WebhookSecret = key, webhookSecret // pragma: allowlist secret
	b.KeyMode, b.KeyRestricted = mode, restricted
	b.checkPurchaseModeNeeds(problems)
}

// checkPurchaseModeNeeds applies what a mode other than off requires: both
// secrets, a live key for on, a non-empty allowlist for allowlist, and a
// founders cutoff for either.
func (b *BillingConfig) checkPurchaseModeNeeds(problems *billingProblems) {
	if b.PurchaseMode == BillingPurchaseModeOff {
		return
	}
	if b.SecretKey == "" || b.WebhookSecret == "" {
		problems.add(fmt.Sprintf("BILLING_PURCHASE_MODE=%s requires STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET", b.PurchaseMode))
	}
	if b.PurchaseMode == BillingPurchaseModeOn && b.KeyMode != StripeKeyModeLive {
		problems.add("BILLING_PURCHASE_MODE=on requires a live STRIPE_SECRET_KEY")
	}
	if b.PurchaseMode == BillingPurchaseModeAllowlist && len(b.PurchaseAllowlist) == 0 {
		problems.add("BILLING_PURCHASE_MODE=allowlist requires a non-empty BILLING_PURCHASE_ALLOWLIST")
	}
	if b.FoundersPricingEndsAt.IsZero() {
		problems.add(fmt.Sprintf("BILLING_PURCHASE_MODE=%s requires FOUNDERS_PRICING_ENDS_AT", b.PurchaseMode))
	}
}

// Summary renders the non-secret settings in the fixed form that the shared
// case table matches and that check-billing-env.sh prints on success.
func (b BillingConfig) Summary() string {
	return fmt.Sprintf("mode=%s key_mode=%s key_restricted=%t allowlist_entries=%d founders_pricing_ends_at=%s automatic_tax=%t self_hosted=%t self_host_keys_ignored=%t",
		b.PurchaseMode, b.KeyMode.class(), b.KeyRestricted, len(b.PurchaseAllowlist),
		b.foundersLabel(), b.AutomaticTax, b.SelfHosted, b.SelfHostKeysIgnored)
}

// String redacts both secrets to their byte length, so a %v or %+v of the
// config never prints a key (the GitHubFeedbackConfig pattern).
func (b BillingConfig) String() string {
	return fmt.Sprintf("BillingConfig{%s secret_key:%s webhook_secret:%s}",
		b.Summary(), redactedLength(b.SecretKey), redactedLength(b.WebhookSecret))
}

// GoString makes %#v print the redacted form. Without it, fmt would walk the
// struct and write both secrets in clear text.
func (b BillingConfig) GoString() string {
	return b.String()
}

// LogValue makes slog render the redacted form. The JSON handler marshals an
// Any value with encoding/json and ignores Stringer, so without LogValue
// slog.Any("billing", cfg) would reach for the fields directly. The json:"-"
// tags on SecretKey and WebhookSecret are the second layer for json.Marshal.
func (b BillingConfig) LogValue() slog.Value {
	return slog.StringValue(b.String())
}

// LogBoot writes the spec §14 boot line: purchase mode, key-mode class,
// founders cutoff and self-host state, never a key. It also logs a warning
// when a self-hosted instance dropped keys from its environment, and when a
// live key is unrestricted.
func (b BillingConfig) LogBoot(l *slog.Logger) {
	l.Info("billing configuration",
		"purchase_mode", string(b.PurchaseMode),
		"key_mode", b.KeyMode.class(),
		"key_restricted", b.KeyRestricted,
		"founders_pricing_ends_at", b.foundersLabel(),
		"allowlist_entries", len(b.PurchaseAllowlist),
		"automatic_tax", b.AutomaticTax,
		"self_hosted", b.SelfHosted)
	if b.SelfHostKeysIgnored {
		l.Warn("billing: STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET are ignored on self-hosted instances")
	}
	// Spec §11: sk_ is accepted, but the runtime key should be the restricted
	// rk_live_ key, whose permissions the runbook lists.
	if b.KeyMode == StripeKeyModeLive && !b.KeyRestricted {
		l.Warn("billing: STRIPE_SECRET_KEY is an unrestricted live key; deploy the restricted rk_live_ key ([internal]stripe-billing.md)")
	}
}

// class names the key mode for logs: none, test or live.
func (m StripeKeyMode) class() string {
	if m == StripeKeyModeNone {
		return "none"
	}
	return string(m)
}

// foundersLabel renders the founders cutoff for logs, or "unset".
func (b BillingConfig) foundersLabel() string {
	if b.FoundersPricingEndsAt.IsZero() {
		return "unset"
	}
	return b.FoundersPricingEndsAt.Format(time.RFC3339)
}

func redactedLength(value string) string {
	if value == "" {
		return "[unset]"
	}
	return fmt.Sprintf("[REDACTED %d bytes]", len(value))
}
