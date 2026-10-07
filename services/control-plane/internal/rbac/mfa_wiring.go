package rbac

import "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"

// SetMFAVerifier wires the verifier that confirms #3454's dangerous-action
// gates on role create, update and delete and on permission-override upserts.
// A nil verifier fails every gate closed with a 500, which is safe but silent,
// so the router's boot guard (requireDangerousActionGatesWired) asks
// HasMFAVerifier. Pattern mirrors servers.Handler's own SetMFAVerifier.
func (h *Handler) SetMFAVerifier(v stepup.MFATxCodeVerifier) { h.mfaVerifier = v }

// HasMFAVerifier reports whether SetMFAVerifier was called with a non-nil
// verifier.
func (h *Handler) HasMFAVerifier() bool { return h.mfaVerifier != nil }

// HasRedis reports whether the handler holds a Redis client. The dangerous-
// action gates charge the step-up attempt budget and pre-read the grace window
// on it before their transaction, and both fail closed without one, so the
// boot guard asks.
func (h *Handler) HasRedis() bool { return h.redis != nil }
