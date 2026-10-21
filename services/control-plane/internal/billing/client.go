package billing

import "github.com/stripe/stripe-go/v87"

// NewClient returns a stripe-go client that authenticates with key.
//
// baseURL is empty in production, which keeps the SDK default
// (https://api.stripe.com). Tests pass an httptest server URL. Nothing reads
// baseURL from the environment.
//
// Telemetry is always off. stripe-go v87 turns it on by default
// (stripe.EnableTelemetry is true, and the BackendConfig field comment that
// says otherwise is wrong); the first request would then write
// ~/.config/stripe/telemetry_id and send a per-host id to Stripe. The SDK's
// own logger is silenced too: every failure is returned to the caller,
// which decides what to print or log.
func NewClient(key, baseURL string) *stripe.Client {
	return stripe.NewClient(key, stripe.WithBackends(stripe.NewBackendsWithConfig(backendConfig(baseURL))))
}

// backendConfig is NewClient's backend configuration, split out so a unit
// test can read it without a network call.
func backendConfig(baseURL string) *stripe.BackendConfig {
	cfg := &stripe.BackendConfig{
		EnableTelemetry: stripe.Bool(false),
		LeveledLogger:   &stripe.LeveledLogger{Level: stripe.LevelNull},
	}
	if baseURL != "" {
		cfg.URL = stripe.String(baseURL)
	}
	return cfg
}
