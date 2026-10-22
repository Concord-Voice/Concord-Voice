package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Fake keys: the shapes ParseStripeKeyMode accepts, never real credentials.
const (
	fakeTestKey       = "sk_test_fake"
	fakeWebhookSecret = "whsec_fake" // pragma: allowlist secret
)

// billingCfg builds a config with the given instance type and, when withKeys,
// a Stripe test-mode key and webhook secret, the way LoadBillingConfig would
// leave them. A self-hosted instance carries SelfHosted too, as it does at boot.
func billingCfg(instanceType string, withKeys bool) *config.Config {
	cfg := &config.Config{InstanceType: instanceType, Environment: "test"}
	cfg.Billing.SelfHosted = config.IsSelfHostedInstance(instanceType)
	if withKeys {
		cfg.Billing.SecretKey = fakeTestKey // pragma: allowlist secret
		cfg.Billing.KeyMode = config.StripeKeyModeTest
		cfg.Billing.WebhookSecret = fakeWebhookSecret // pragma: allowlist secret
	}
	return cfg
}

func TestBillingState(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"self-hosted with keys", billingCfg(config.InstanceTypeSelfHosted, true), subscriptions.BillingSelfHosted},
		{"self-hosted without keys", billingCfg(config.InstanceTypeSelfHosted, false), subscriptions.BillingSelfHosted},
		{"SaaS without keys", billingCfg(config.InstanceTypeSaaS, false), subscriptions.BillingNotConfigured},
		{"SaaS with a test key", billingCfg(config.InstanceTypeSaaS, true), subscriptions.BillingAvailable},
		{"empty instance type is SaaS", billingCfg("", true), subscriptions.BillingAvailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, billingState(tc.cfg))
		})
	}
}

func TestWireBilling_NilWithoutKeysOrOnSelfHost(t *testing.T) {
	log := logger.New("test")

	t.Run("SaaS without keys wires nothing", func(t *testing.T) {
		assert.Nil(t, wireBilling(context.Background(), billingCfg(config.InstanceTypeSaaS, false), nil, nil, nil, nil, log))
	})
	t.Run("self-hosted with keys wires nothing", func(t *testing.T) {
		assert.Nil(t, wireBilling(context.Background(), billingCfg(config.InstanceTypeSelfHosted, true), nil, nil, nil, nil, log))
	})
	t.Run("self-hosted without keys wires nothing", func(t *testing.T) {
		assert.Nil(t, wireBilling(context.Background(), billingCfg(config.InstanceTypeSelfHosted, false), nil, nil, nil, nil, log))
	})
	t.Run("SaaS with a test key builds the service", func(t *testing.T) {
		assert.NotNil(t, wireBilling(context.Background(), billingCfg(config.InstanceTypeSaaS, true), nil, nil, nil, nil, log))
	})
}
