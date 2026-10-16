package entitlements

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ViolationReason names one entitlement refusal a WELL-BEHAVED client prevents
// itself (#1308). The stock client checks every one of these against its own
// entitlement set before it asks, so the server seeing one means the client
// skipped its check, is stale, or was modified.
//
// The set is CLOSED. It is logged and it feeds a Redis key family, so a value
// built from request data would be both an injection vector and an unbounded
// label. ParseViolationReason is the only way in from the wire.
//
// Deliberately absent: animated-profile premium refusals, image-size caps,
// username cadence, generic body-size 413s and chunk 413s. Those are reachable
// by an honest client (a stale cache, an image whose processed size the client
// cannot predict) or measure transport shape rather than entitlement.
//
// The counted reasons are not unreachable by an honest client either: one
// holding a stale set in the moments after a downgrade, or refused while the
// tier lookup has degraded to free, is counted too. The threshold -- five in
// an hour -- is what absorbs those, so a count means "refused", never "proven
// hostile".
type ViolationReason string

// The three counted reasons. The two audio ones are the media plane's #3858
// gate refusals folded by kind: an UNDECLARED ptime or bitrate counts under the
// matching reason, because the stock client declares both on every tier and
// absence is therefore the same misbehaviour as an over-tier value.
const (
	ViolationAttachmentOverTier   ViolationReason = "attachment_over_tier"
	ViolationAudioPtimeBelowTier  ViolationReason = "audio_ptime_below_tier"
	ViolationAudioBitrateOverTier ViolationReason = "audio_bitrate_over_tier"
)

// ParseViolationReason admits exactly the closed set and nothing else.
func ParseViolationReason(raw string) (ViolationReason, bool) {
	switch reason := ViolationReason(raw); reason {
	case ViolationAttachmentOverTier, ViolationAudioPtimeBelowTier, ViolationAudioBitrateOverTier:
		return reason, true
	default:
		return "", false
	}
}

// Window and threshold are security constants, never configuration: an
// operator-tunable threshold is an operator-tunable way to switch the signal
// off (the presencecapture "bounds are const" precedent in backend.md).
const (
	// ViolationThreshold is the count at which the window's one escalation
	// fires. It fires on EXACTLY this count -- not on every count above it.
	ViolationThreshold = 5
	// ViolationWindow is FIXED, starting at the first count. EXPIRE NX sets it
	// only on a key that has no TTL, so later counts never slide it.
	ViolationWindow = time.Hour

	violationKeyPrefix = "entviol:"

	// Each Redis-bound step of a record -- the count, and the threshold's tier
	// resolve -- gets its own bound of this length. The resync push is NOT
	// bounded by it: the hub's user broadcast blocks until the hub loop takes
	// the message. A stalled hub therefore holds slots, and violationRecordSlots
	// is what bounds that.
	violationRecordTimeout = 2 * time.Second
	// At most this many records run at once. A record that finds every slot
	// taken is DROPPED rather than queued: slots only fill when Redis is slow,
	// and queueing behind a slow Redis is how telemetry starts holding memory
	// for the request path it observes.
	violationRecordSlots = 64

	violationLogMessage          = "entitlement.violation"
	violationThresholdLogMessage = "entitlement.misbehavior_threshold"
	violationDroppedLogMessage   = "entitlement.violation_unrecorded"
)

// ViolationKey is the one place the counter key is spelled. It takes a
// uuid.UUID rather than a string so the key is canonical BY TYPE: one user has
// one spelling, so one user has one window. A string key would let `{BRACED}`,
// UPPERCASE and dash-less spellings of the same id each mint their own window
// and escalate once apiece -- the claim-spelling class backend.md records for
// #3362.
func ViolationKey(userID uuid.UUID) string {
	return violationKeyPrefix + userID.String()
}

// Recorder is what a refusal site needs: one call that cannot fail, cannot
// block, and returns nothing the caller could branch on. Both the media upload
// handlers and the media-plane NATS door depend on this rather than on the
// concrete ViolationRecorder, so their tests can count calls.
type Recorder interface {
	Record(ctx context.Context, userID uuid.UUID, reason ViolationReason)
}

// EntitlementPusher is the ONE effect the threshold has on a user's sessions:
// a resync push of the entitlement set the server is enforcing.
//
// It is narrower than SessionNotifier on purpose. SessionNotifier also carries
// DisconnectUser, and the approved response to misbehaviour is flag-and-resync
// with NO disconnect -- a disconnect driven by this path would hand anyone who
// can forge a voice.tier_violation (the NATS bus is unauthenticated, #2857) a
// way to kick a chosen user offline. Taking a type with no such method makes
// that unreachable by construction rather than by review.
type EntitlementPusher interface {
	BroadcastEntitlements(userID uuid.UUID, dto EntitlementDTO)
}

// CheckedTierResolver is the tier read the resync needs. Unlike TierResolver it
// reports a failed read as an error instead of the fail-closed free tier, which
// is right for enforcing one request and wrong as a set to push. *Cache
// satisfies it with GetTierChecked.
type CheckedTierResolver interface {
	GetTierChecked(ctx context.Context, userID string) (string, error)
}

// ViolationRecorder counts entitlement violations per user in a fixed one-hour
// window and escalates once per window.
//
// INVARIANT (once per window). For a user U, a window is the lifetime of the
// Redis key ViolationKey(U): created by the first INCR, expiring
// ViolationWindow later. Within it the threshold effects run exactly once, in
// the single record whose INCR returned exactly ViolationThreshold. INCR is
// atomic and returns each integer at most once per key lifetime, and the key
// is shared by every replica, so this holds fleet-wide. It breaks if:
//
//   - the comparison becomes >= (escalates on every later count);
//   - EXPIRE loses NX (each count slides the window, so a steady trickle keeps
//     one window alive and never escalates again);
//   - the key is built from a non-canonical spelling (one user, several
//     windows -- ViolationKey's uuid.UUID parameter is what prevents this);
//   - INCR and EXPIRE NX leave the MULTI/EXEC (on a server that rejects
//     EXPIRE NX, a plain pipeline would still apply the INCR and leave a key
//     with no TTL; inside MULTI the EXEC aborts and neither applies).
//
// A Redis failover that loses the key mid-window lets that user escalate
// again. Accepted: the cost is one more resync push and one more event.
//
// The escalation is AT MOST ONCE. An INCR that applies but whose reply is lost
// (the deadline expires after EXEC reached Redis) is logged as
// violation_counter_unavailable; if it was the fifth, that window never
// escalates, because the next count is six. A marker key would not fix it --
// the marker's own reply can be lost the same way -- and at-least-once would
// duplicate the security event. Accepted: one user's flag missing for one hour,
// with every refusal still enforced and every other count still logged.
//
// TELEMETRY NEVER AFFECTS THE CALLER. Record returns nothing, never blocks on
// I/O, and is safe on a nil receiver and a nil Redis client. The refusal it
// observes has already been decided and its response is unchanged whatever
// happens here.
type ViolationRecorder struct {
	redis  *redis.Client
	log    *logger.Logger
	tiers  CheckedTierResolver
	pusher EntitlementPusher
	events securityevent.Emitter

	slots    chan struct{}
	inflight sync.WaitGroup
}

// NewViolationRecorder builds the recorder. tiers and pusher may be nil, which
// disables only the resync push; events may be nil, which discards the
// security event. A nil Redis client leaves every record unrecorded, logged.
func NewViolationRecorder(
	rdb *redis.Client,
	log *logger.Logger,
	tiers CheckedTierResolver,
	pusher EntitlementPusher,
	events securityevent.Emitter,
) *ViolationRecorder {
	if log == nil {
		log = logger.NewWithWriter(io.Discard)
	}
	if events == nil {
		events = securityevent.Discard
	}
	return &ViolationRecorder{
		redis:  rdb,
		log:    log,
		tiers:  tiers,
		pusher: pusher,
		events: events,
		slots:  make(chan struct{}, violationRecordSlots),
	}
}

// Record observes one refusal. It hands the count to a background slot and
// returns at once: telemetry must never slow the request it observes, and a
// synchronous Redis round trip -- bounded or not -- would.
//
// The context is DETACHED (WithoutCancel): the refusal's response is about to
// be written and its request context canceled, which must not abort the count.
func (r *ViolationRecorder) Record(ctx context.Context, userID uuid.UUID, reason ViolationReason) {
	if r == nil {
		return
	}
	if _, ok := ParseViolationReason(string(reason)); !ok {
		// Unreachable from the wire (both doors parse first); a caller passing
		// an out-of-set value is a programming error, and it must not reach
		// the log or the key family.
		r.log.Warn(violationDroppedLogMessage, "failure_class", "invalid_reason")
		return
	}
	select {
	case r.slots <- struct{}{}:
	default:
		r.log.Warn(violationDroppedLogMessage, "failure_class", "violation_record_saturated")
		return
	}
	r.inflight.Add(1)
	detached := context.WithoutCancel(ctx)
	go func() {
		defer r.inflight.Done()
		defer func() { <-r.slots }()
		r.record(detached, userID, reason)
	}()
}

// Wait blocks until every record dispatched so far has finished. Production
// does not need it -- each Redis step is bounded by violationRecordTimeout -- but a
// caller that must observe a record's effects, a test above all, cannot
// otherwise know the record has run, and "nothing happened" is unprovable
// without it.
func (r *ViolationRecorder) Wait() {
	if r == nil {
		return
	}
	r.inflight.Wait()
}

// record is the synchronous core: count, log, and escalate on the threshold.
// The count and the escalation each get their own violationRecordTimeout, so a
// slow count cannot leave the escalation's tier resolve a spent deadline -- the
// condition that turned a degraded answer into a pushed one.
func (r *ViolationRecorder) record(ctx context.Context, userID uuid.UUID, reason ViolationReason) {
	countCtx, cancelCount := context.WithTimeout(ctx, violationRecordTimeout)
	count, err := r.count(countCtx, userID)
	cancelCount()
	if err != nil {
		// A fixed class and nothing else: the raw driver error can echo the
		// key, which carries the user id (observability.md).
		r.log.Warn(violationDroppedLogMessage, "failure_class", "violation_counter_unavailable")
		return
	}
	r.log.Warn(violationLogMessage,
		"user_id", userID.String(), "reason", string(reason), "count", count)
	if count != ViolationThreshold {
		return
	}
	escalateCtx, cancelEscalate := context.WithTimeout(ctx, violationRecordTimeout)
	defer cancelEscalate()
	r.escalate(escalateCtx, userID, reason)
}

var errViolationCounterUnwired = errors.New("violation counter has no redis client")

// count increments the user's window counter and returns the new value.
//
// One MULTI/EXEC, mirroring stepup.DeleteSoftLock: INCR; EXPIRE NX. NX sets a
// TTL only on a key that has none, so the window is fixed at the first count,
// and a key whose EXPIRE was ever lost is repaired by its next count.
func (r *ViolationRecorder) count(ctx context.Context, userID uuid.UUID) (int64, error) {
	if r.redis == nil {
		return 0, errViolationCounterUnwired
	}
	key := ViolationKey(userID)
	var incr *redis.IntCmd
	if _, err := r.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.Incr(ctx, key)
		p.ExpireNX(ctx, key, ViolationWindow)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("violation counter: %w", err)
	}
	n, err := incr.Result()
	if err != nil {
		return 0, fmt.Errorf("violation counter: %w", err)
	}
	return n, nil
}

// escalate runs the window's one threshold response: flag and resync, never
// disconnect.
//
// The security event carries ONLY the closed vocabulary -- no user id, no
// reason -- so the writer coalesces a burst into one line per second and
// Nightwatch needs no new value. The user id goes to the WARN line alone.
//
// The push is the entitlement set the server ENFORCES, read through the same
// cache the upload doors enforce with, deliberately not re-resolved past it: a
// client that misbehaves because it holds a stale set is corrected to what the
// server holds it to, and an attacker who can forge the trigger cannot use it
// to churn the tier cache.
//
// A DEGRADED answer is never pushed. TierResolver fails closed to free, which
// is right for enforcing one request and wrong as a set the client then holds
// for the rest of its session: it would lower a paying user's live call
// quality, and the trigger can be forged (#2857). So the tier is read through
// GetTierChecked, and when it cannot be read -- a database error, a spent
// deadline -- the push is skipped and logged; the WARN line and the event
// stand.
func (r *ViolationRecorder) escalate(ctx context.Context, userID uuid.UUID, reason ViolationReason) {
	r.log.Warn(violationThresholdLogMessage,
		"user_id", userID.String(), "reason", string(reason), "count", ViolationThreshold)
	r.events.Emit(ctx, securityevent.Event{
		EventType:  securityevent.EventSecurityControl,
		Outcome:    securityevent.OutcomeDenied,
		Severity:   securityevent.SeverityMedium,
		ReasonCode: securityevent.ReasonAuthorizationDenied,
	})
	if r.pusher == nil || r.tiers == nil {
		return
	}
	tier, err := r.tiers.GetTierChecked(ctx, userID.String())
	if err != nil {
		// A fixed class only: the error can carry the user id.
		r.log.Warn(violationDroppedLogMessage, "failure_class", "violation_resync_unresolved")
		return
	}
	r.pusher.BroadcastEntitlements(userID, ToDTO(For(tier)))
}
