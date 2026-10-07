package api

import (
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// dangerousActionGateGap names the first dependency of #3454's dangerous-action
// gates that a handler lacks, or returns "" when every one is wired.
//
// It covers the four handlers no other guard does. servers (UpdateServer,
// DeleteServer) is covered by requireServersMFAVerifierWired and messages (the
// purges) by requireMessageDeleteGuardWired. media is checked only when it
// exists: without object storage it is nil and its upload routes are never
// registered, so there is no gate to wire.
//
// It is one predicate with two readers, requireDangerousActionGatesWired and
// the features.mfaEnforcedDangerousActions capability, so the capability can
// never claim gating the boot guard would not have admitted. It interrogates
// the HANDLERS, never the values the router holds, for the reason
// requirePermissionInvalidatorWired gives.
func dangerousActionGateGap(ch *channels.Handler, mem *members.Handler, rb *rbac.Handler, med *media.Handler) string {
	switch {
	case ch == nil || !ch.HasMFAVerifier():
		return "channels handler has no MFA verifier: deleting a channel or shortening its message expiration would fail on every enforcing server"
	case !ch.HasRedis():
		return "channels handler has no Redis client: the step-up attempt budget would deny every gated channel action"
	case mem == nil || !mem.HasMFAVerifier():
		return "members handler has no MFA verifier: a ban, or a kick that purges messages, would fail on every enforcing server"
	case !mem.HasRedis():
		return "members handler has no Redis client: the step-up attempt budget would deny every gated ban or kick"
	case rb == nil || !rb.HasMFAVerifier():
		return "rbac handler has no MFA verifier: role and permission-override changes that grant dangerous bits would fail on every enforcing server"
	case !rb.HasRedis():
		return "rbac handler has no Redis client: the step-up attempt budget would deny every gated role or override change"
	case med != nil && !med.HasMFAVerifier():
		return "media handler has no MFA verifier: server icon and banner uploads would fail on every enforcing server"
	case med != nil && !med.HasRedis():
		return "media handler has no Redis client: the step-up attempt budget would deny every gated server icon or banner upload"
	case med != nil && !med.HasResolver():
		return "media handler has no permission resolver: server icon and banner uploads would be authorized by membership alone"
	}
	return ""
}

// requireDangerousActionGatesWired fatal-exits when a dangerous-action gate
// (#3454) lacks a dependency.
//
// Every one fails CLOSED, which is the safe direction but a silent one: with
// no verifier a gated action on an enforcing server is a 500, and with no
// Redis client the step-up attempt budget answers every gated request that
// carries a code with a 503. Boot is where that should surface. Extracted from
// NewRouter, which sits at the go:S3776 limit.
func requireDangerousActionGatesWired(
	log *logger.Logger, ch *channels.Handler, mem *members.Handler, rb *rbac.Handler, med *media.Handler,
) {
	if gap := dangerousActionGateGap(ch, mem, rb, med); gap != "" {
		log.Fatal(gap)
	}
}
