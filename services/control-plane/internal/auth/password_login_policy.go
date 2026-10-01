package auth

import "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"

// EffectivePasswordLoginDisabled applies deployment policy to the stored preference.
// Self-hosted instances keep native password login available without changing
// this preference. Successful final self-hosted unlink separately clears it;
// otherwise retained intent applies again when the instance returns to SaaS.
func EffectivePasswordLoginDisabled(instanceType string, storedDisabled bool) bool {
	return storedDisabled && !config.IsSelfHostedInstance(instanceType)
}
