package middleware

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

const (
	serviceHopProofHeader     = "X-Concord-Service-Proof"
	serviceHopTimestampHeader = "X-Concord-Service-Timestamp"
	serviceHopProofPurpose    = "concord/media-plane-service-hop/v1"
	serviceHopProofVersion    = "v1"

	// serviceHopContextKey marks a request as an authenticated internal hop.
	serviceHopContextKey = "concord_service_hop"

	// serviceHopRejectLogInterval bounds how often a rejected proof is logged.
	// The proof header is client-settable, so an unsampled line per rejection
	// would itself be an amplification primitive.
	serviceHopRejectLogInterval = time.Minute
)

// serviceHopPaths is the closed set of routes the exemption exists for.
//
// The middleware is registered on the whole `authRequired` group, so without
// this the marker would be settable on any route beneath it. Nothing reads the
// marker outside RequireClientVersion today, and the proof binds the request
// URI so a captured proof cannot be moved — but the middleware's own contract
// says it grants one thing on two endpoints, and an allowlist makes the code
// enforce the sentence rather than merely assert it.
// The value is a fixed label, not the pattern itself: it is what the rejection
// log records, so the logged route provably comes from this table rather than
// from the request. That keeps the log useful without routing a request-derived
// string into a log sink at all — cheaper than arguing the taint is benign.
var serviceHopPaths = map[string]string{
	"/api/v1/channels/:id/voice/join":              "channel_voice_join",
	"/api/v1/dm/conversations/:id/voice/authorize": "dm_voice_authorize",
}

// MediaPlaneServiceHop authenticates the media plane's server-to-server
// re-validation calls and marks them on the context.
//
// It grants exactly one thing: exemption from RequireClientVersion. That gate
// asks whether the END-USER's application is new enough — a question this hop
// cannot answer, and one whose answer is client-asserted anyway, so exempting
// it grants an attacker nothing they did not already have. On 2026-09-02
// setting CLIENT_MIN_VERSION made clientversion.Parse("") fail for a hop that
// sends no version header, and every voice join 403'd.
//
// It deliberately does NOT exempt RequireAttestation. That exemption was
// removed after an adversarial pass proved the premise behind it false: the
// media plane admits SFU sockets on the JWT alone and server-channel
// AuthorizeJoin writes no reservation, so nothing forces the client-gated call
// to have happened first. A tampered client — precisely what attestation
// exists to stop — could skip its own gated call, reach the hop, and be
// admitted with ADR-0010's revocation kill-switch never consulted. Attestation
// is the one control such a client cannot satisfy, so it stays enforced;
// relaying the client's attestation headers is the right way to make voice work
// with it enabled, and is deliberately out of scope here.
//
// Must be registered AFTER AuthRequired. That is enforced below rather than
// merely documented: a request with no authenticated user never reaches proof
// verification.
func MediaPlaneServiceHop(jwtSecret string, log *logger.Logger) gin.HandlerFunc {
	key := mediaproof.DeriveKey(jwtSecret, serviceHopProofPurpose)
	if len(key) == 0 && log != nil {
		log.Warn("media-plane service-hop proof disabled: no signing key derivable")
	}
	var lastRejectLog atomic.Int64
	return func(c *gin.Context) {
		// Overwhelmingly the common path: a real client sends no proof.
		// Skip the HMAC entirely rather than paying for it per request.
		proof := c.GetHeader(serviceHopProofHeader)
		if proof == "" {
			c.Next()
			return
		}
		routeLabel, allowed := serviceHopPaths[c.FullPath()]
		if !allowed {
			c.Next()
			return
		}
		// Ordering guard, not decoration: AuthRequired sets user_id, so its
		// absence means this middleware was registered before it.
		if _, ok := c.Get("user_id"); !ok {
			c.Next()
			return
		}
		timestamp := c.GetHeader(serviceHopTimestampHeader)
		accessToken, found := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !found || accessToken == "" {
			logServiceHopRejection(log, &lastRejectLog, routeLabel, "no_bearer")
			c.Next()
			return
		}
		// RequestURI, not Path: the query string is part of the request being
		// authorized, so binding only the path would let a proof for /x be
		// replayed at /x?whatever. No route decides on a query parameter today;
		// binding it costs nothing and removes the question.
		if mediaproof.Verify(
			key, proof,
			serviceHopProofVersion,
			timestamp,
			c.Request.Method,
			c.Request.URL.RequestURI(),
			mediaproof.TokenDigest(accessToken),
		) {
			c.Set(serviceHopContextKey, true)
		} else {
			// A present-but-invalid proof is NOT an error: the request simply
			// does not earn the exemption and proceeds through the client gates
			// like any other. Rejecting outright would turn a clock-skew blip or
			// a mid-deploy secret rotation into a hard failure on the path whose
			// whole purpose is keeping voice available.
			//
			// But it must not be SILENT. With no gate enabled a broken proof
			// returns 200, so the mechanism could be dead for weeks and the
			// first symptom would be voice dropping fleet-wide the moment
			// CLIENT_MIN_VERSION is set again — the very outage it prevents,
			// re-armed invisibly.
			logServiceHopRejection(log, &lastRejectLog, routeLabel, "proof_invalid")
		}
		c.Next()
	}
}

// logServiceHopRejection emits at most one line per serviceHopRejectLogInterval.
// Both values it records come from package constants — the route label from
// serviceHopPaths, the reason from a fixed set — so nothing request-supplied
// reaches the log sink. Never the proof, the timestamp, or the bearer token.
func logServiceHopRejection(log *logger.Logger, last *atomic.Int64, routeLabel, reason string) {
	if log == nil {
		return
	}
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev < int64(serviceHopRejectLogInterval) || !last.CompareAndSwap(prev, now) {
		return
	}
	log.Warn("media-plane service-hop proof rejected",
		"failure_class", "service_hop_proof_rejected",
		"reason", reason,
		"route", routeLabel,
	)
}

// IsMediaPlaneServiceHop reports whether MediaPlaneServiceHop authenticated
// this request. False whenever the middleware did not run, so a route group
// that omits it cannot accidentally grant the exemption.
func IsMediaPlaneServiceHop(c *gin.Context) bool {
	hop, ok := c.Get(serviceHopContextKey)
	if !ok {
		return false
	}
	verified, ok := hop.(bool)
	return ok && verified
}
