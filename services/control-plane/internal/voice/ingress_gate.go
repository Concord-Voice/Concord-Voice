package voice

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/ingressbudget"
)

// Ingress admission bounds for the two NATS doors this package subscribes to
// (#2854 stage B1). The bus carries no per-service authorization -- the
// media-plane, which terminates untrusted WebRTC traffic, shares it -- so
// nothing upstream bounds how much work a forged publish can buy.
//
// These are security bounds and are therefore const, never configuration,
// following the internal/presencecapture precedent recorded in
// [internal]rules/backend.md ("Bounds are const, never configuration"). An
// operator-tunable ingress budget is an operator-tunable DoS window.
const (
	// Erasure door. POST /api/v1/privacy/erase-account is gated at 3 per user
	// per 24h, there is one publish per successful erasure, and there is no bulk
	// path and no sweeper. Honest sustained rate is a handful per DAY, so 10/s
	// sits roughly four orders of magnitude above honest peak while satisfying
	// epic #1236 section 11.4 (10 total per second) literally.
	//
	// ACCEPTED RESIDUAL, stated because an earlier revision of the design
	// dismissed it on false reasoning. This is ONE GLOBAL bucket, and dedup only
	// defends the same-UUID case, so a flood of DISTINCT random UUIDs consumes
	// every token and can shed a GENUINE erasure clear -- a right-to-erasure
	// suppression at roughly 10 messages per second. The design's R8 rejected a
	// resync latch by arguing an honest clear can only be shed by 32 SIMULTANEOUS
	// distinct-user erasures, which the 3-per-24h publisher gate makes
	// unreachable. That argument bounds honest PUBLISHERS and the attacker is not
	// one.
	//
	// PoC-proven, not theorised: 1000 distinct forged UUIDs admit exactly the
	// burst and the genuine clear that follows is shed as ingress_budget, while
	// the identical clear with no flood running is not shed at all. Attacker
	// cost is roughly 11 messages per second.
	//
	// The budget is kept anyway because the alternative is worse: with no budget
	// the same flood performs unbounded O(Rich Presence clients) fan-out per
	// message, which is how PR #2855 already ended up with a queue-saturation
	// path to the same suppression. B1 makes that suppression cheaper and more
	// total; it does not create it.
	//
	// There is NO discriminator available at this boundary. A genuine clear and
	// a forged one are byte-identical, and the existence probe cannot separate
	// them either, since a random UUID that was never a user reads as absent
	// exactly like an erased one. Telling them apart requires authenticating
	// the bus, which is #2857. Do not attempt to fix this by resizing the
	// budget: any budget an attacker can saturate suppresses honest traffic,
	// which is the same reasoning that removed the new-room gate below.
	erasureBudgetRefill = time.Second / 10
	erasureBudgetBurst  = 32

	// The CAPACITY is the safety property; the TTL is only freshness.
	erasureDedupCapacity = 8192
	erasureDedupTTL      = 10 * time.Minute

	// G2 -- per-room, and applied to voice.heartbeat ONLY.
	//
	// THE SUBJECT RESTRICTION IS THE POINT, not a narrowing for convenience.
	// An earlier revision metered every voice.* subject with a burst derived from
	// maxServerVoiceParticipantIDs -- a participant CEILING standing in for a
	// RATE. Two consequences, both proven:
	//
	//   * A media-plane restart makes a whole room rejoin at once, so a large
	//     room's honest rejoin exceeded the burst with NO attacker.
	//   * Producer churn from an ORDINARY AUTHENTICATED PARTICIPANT (no bus
	//     access at all -- media-plane's own limits allow ~2 producer publishes
	//     per second per socket) drained the budget on subjects this subscriber
	//     DISCARDS, starving the room's genuine events. Measured: 486 genuine
	//     voice.left offered in 1.2s under churn, ZERO admitted.
	//
	// Shedding heartbeat is a DELAY: it is idempotent, self-repairing, and
	// already coalesced by coalescePendingVoiceHeartbeat. Shedding a one-shot
	// transition is not -- specifically a shed room_empty, which is TERMINAL because the room is
	// already destroyed and no successor message exists. Metering a class whose
	// loss is unrecoverable was the error.
	//
	// CORRECTED in #2868. This note used to add that voice_participants "has
	// exactly one INSERT site (handleJoined) and heartbeat reconciliation is
	// removal-only, so a shed joined leaves no row for the session". That was
	// FALSE. There is exactly one INSERT STATEMENT -- in moveServerVoiceParticipant,
	// running ON CONFLICT (channel_id, user_id) DO UPDATE -- but two paths reach
	// it: handleServerVoiceJoined AND the heartbeat's
	// refreshServerHeartbeatParticipant, both via applyServerHeartbeatParticipant
	// -> upsertServerVoiceParticipant. bulkRefreshPrivateVoiceParticipants
	// upserts dm_voice_participants the same way. Reconciliation is therefore
	// BIDIRECTIONAL -- reconcileServerHeartbeatParticipants removes the rows the
	// media list omits, the refresh adds the ones it names -- so a lost joined
	// converges on the room's next tick.
	//
	// THE SUBJECT RESTRICTION IS UNAFFECTED BY THAT CORRECTION. It rests on the
	// two independent facts stated above -- the measured starvation (486 genuine
	// voice.left offered in 1.2s under producer churn, ZERO admitted) and
	// room_empty's terminality -- and joined's convergence touches neither. Do
	// not read the correction as licence to meter the one-shot subjects.
	//
	// Burst is now derived from ONE room's heartbeat behaviour alone, which does
	// not tighten as the fleet grows, and being per-room is what makes it
	// structurally incapable of starving another room.
	voiceRoomBudgetRefill = time.Second / 4
	voiceRoomBudgetBurst  = 64
	// Above the dispatcher's own room ceiling (voiceLifecycleDispatchRoomCount),
	// so residency tracking is never the binding constraint.
	voiceRoomBudgetCapacity = 2048

	// Largest honest voice.* payload is a heartbeat carrying up to
	// maxServerVoiceParticipantIDs UUID strings (~40 bytes each, so ~40KB) plus
	// its envelope. 128KB is ~3x that, and refuses a maximum-size 1MB document.
	voiceIngressMaxPayloadBytes = 128 * 1024

	// Sheds are reported on an interval, never per message. See ingressShedState.
	ingressShedLogInterval = 30 * time.Second
)

// A NEW-ROOM RATE GATE WAS DESIGNED AND THEN REMOVED. Do not reintroduce one
// without reading this.
//
// The design carried a third voice gate ("G3") metering the first admission of
// any room key not already resident, intended to protect
// voiceLifecycleDispatchRoomCount. It was implemented, and an adversarial pass
// proved it introduced a DENIAL PRIMITIVE THAT DOES NOT EXIST WITHOUT IT.
//
// The mechanism: a message shed by the new-room gate never reaches
// voiceRoomBudget.Allow(), and that call is the ONLY thing that makes a key
// resident. So a shed key stays non-resident and is metered as new on every
// subsequent attempt, forever. An attacker sustaining a few random channelIds
// per second denied essentially every new voice room on the replica --
// measured at 512 forged keys to drain the burst, then 50 consecutive honest
// attempts, ZERO admitted.
//
// It was removed rather than resized because no size fixes it: the gate cannot
// tell a forged key from an honest one, so any budget an attacker can saturate
// starves honest rooms. Its only protection target is dispatcher room-map
// exhaustion, which the design (section 4.3) already states B1 cannot close --
// the teardown is reachable by honest load alone, since worst-case dispatcher
// drain is far below honest ingress at scale. That class is owned by #2868.
//
// So the trade was: buy a cheap new denial, in exchange for partially delaying
// a class this change cannot close anyway. G1 and G2 are both strict
// improvements with no such cost. What remains uncovered is stated plainly in
// gateVoiceLifecycle below.

// There is deliberately NO package-global test clock here. A test needing a
// frozen clock assigns s.erasureBudget directly with NewBucketWithClock, so the
// substitution dies with the subscriber. A global stayed mutated after the test
// that set it and handed every later test in the package a frozen budget
// (CodeRabbit, PR #2871).
func newErasureBudget() *ingressbudget.Bucket {
	return ingressbudget.NewBucket(erasureBudgetRefill, erasureBudgetBurst)
}

func newErasureSeen() *ingressbudget.Window {
	return ingressbudget.NewWindow(erasureDedupCapacity, erasureDedupTTL)
}

func newVoiceRoomBudget() *ingressbudget.KeyedBuckets {
	return ingressbudget.NewKeyedBuckets(
		voiceRoomBudgetRefill, voiceRoomBudgetBurst, voiceRoomBudgetCapacity)
}

// ingressShedState accumulates shed counts between reports.
type ingressShedState struct {
	mu       sync.Mutex
	counts   map[string]int
	loggedAt time.Time
	// reported marks classes that have already had a report emitted. Only
	// recordNReportFirst consults it; recordN never sets it.
	reported map[string]bool
}

// record counts one shed and returns the accumulated counts when a report is
// due, nil otherwise.
func (g *ingressShedState) record(class string, now time.Time) map[string]int {
	return g.recordN(class, 1, now)
}

// recordN counts n sheds of one class and returns the accumulated counts when a
// report is due, nil otherwise. The dispatcher reports a whole coalesced burst
// at once, so it needs a count rather than n calls.
//
// Counting and reporting are AGGREGATED, never once per message. A gate that
// logs once per rejection IS an amplification primitive: it hands a forged flood
// one log write per message and recreates in the log exactly the amplification
// the gate just removed from the socket. Same reasoning as
// Hub.ClearErasedSenderCustomText (internal/websocket/customtext.go).
func (g *ingressShedState) recordN(class string, n int, now time.Time) map[string]int {
	return g.recordClass(class, n, now, false)
}

// recordNReportFirst is recordN except that the FIRST report of a class is
// EMITTED rather than merely arming the interval.
//
// recordN's arming branch is right for a recoverable shed -- a single stray
// message should not itself be a log line. It is wrong for a drop whose loss is
// unrecoverable. Reporting here is shed-DRIVEN, not time-driven, so a bounded
// burst that ends is never reported at all: the first drop arms the window and
// no later drop arrives to flush it. Before #2868 the overflow arm logged
// unconditionally on every wakeup, so retiring the teardown without this would
// have traded a disproportionate response for a SILENT one (CWE-778).
//
// It is not an amplification primitive. failure_class is a closed three-value
// vocabulary, so this emits at most one extra line per class for the life of a
// replica; every subsequent report is interval-gated exactly as recordN's are.
// A blanket "always emit for this class" WOULD be one -- the overflow worker can
// wake as fast as it can write.
func (g *ingressShedState) recordNReportFirst(class string, n int, now time.Time) map[string]int {
	return g.recordClass(class, n, now, true)
}

func (g *ingressShedState) recordClass(
	class string, n int, now time.Time, reportFirst bool,
) map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.counts == nil {
		g.counts = map[string]int{}
	}
	g.counts[class] += n

	if reportFirst && !g.reported[class] {
		if g.reported == nil {
			g.reported = map[string]bool{}
		}
		g.reported[class] = true
		due := g.counts
		g.counts = map[string]int{}
		g.loggedAt = now
		return due
	}

	if g.loggedAt.IsZero() {
		// The first shed of the process arms the interval rather than reporting
		// immediately, so a single stray message is not itself a log line. A
		// low-and-slow attacker is still reported: the next shed after the
		// interval flushes the accumulated count, including that first one.
		//
		// Known and accepted: reporting is shed-DRIVEN, so the tail of a flood
		// stays uncounted until the next shed arrives, which may be much later.
		// During a flood reports fire every interval, so an operator sees it
		// while it matters; only the final partial window is delayed. A periodic
		// flusher would close that at the cost of a goroutine and its shutdown
		// ordering, which is not worth it for a counter.
		g.loggedAt = now
		return nil
	}
	if now.Sub(g.loggedAt) < ingressShedLogInterval {
		return nil
	}

	due := g.counts
	g.counts = map[string]int{}
	g.loggedAt = now
	return due
}

const voiceDropShedMessage = "Voice lifecycle events dropped at the dispatcher"

// erasureShed records a shed on the erasure door.
func (s *NATSSubscriber) erasureShed(class string) {
	s.reportShed(&s.erasureShedState, "Presence erasure clear shed at the ingress gate", class)
}

// voiceShed records a shed on the voice-lifecycle door.
func (s *NATSSubscriber) voiceShed(class string) {
	s.reportShed(&s.voiceShedState, "Voice lifecycle event shed at the ingress gate", class)
}

func (s *NATSSubscriber) reportShed(state *ingressShedState, msg, class string) {
	if s.ingressShedObservedHook != nil {
		s.ingressShedObservedHook(class)
	}
	due := state.record(class, time.Now())
	if due == nil {
		return
	}
	if s.ingressShedLoggedHook != nil {
		s.ingressShedLoggedHook()
	}
	for shedClass, count := range due {
		s.log.Warn(msg, "failure_class", shedClass, "shed_count", count)
	}
}

// voiceDropShed records count dispatcher drops of one class and emits the
// interval-aggregated report when one is due.
//
// The EMISSION lives here rather than at its call site in nats.go, for the same
// reason reportShed above does. nats_rich_presence_log_guard_test walks nats.go
// for structured log values and admits only a string literal for failure_class
// and only an int literal or len() for a count, so an aggregated report -- whose
// entire payload is a class name and a running total -- cannot be emitted from
// that file at all. Both values are closed and neither is wire-derived: the
// class is one of the three voiceLifecycleDropClassName literals, and the count
// is an integer this package produced.
func (s *NATSSubscriber) voiceDropShed(class string, count uint64) {
	dropped := boundedShedCount(count)
	// Hoisted rather than tested per iteration: the hook is nil in production,
	// and a burst can carry a large tally that must not buy a spin per drop.
	if s.ingressShedObservedHook != nil {
		for range dropped {
			s.ingressShedObservedHook(class)
		}
	}
	// A convergent drop repairs itself on the room's next heartbeat tick, so its
	// first occurrence may arm the window silently like any recoverable shed. A
	// terminal or unresolvable drop has no successor event -- if its first
	// occurrence is swallowed and the burst then ends, nothing ever reports it.
	convergent := voiceLifecycleDropClassName(voiceLifecycleDropConvergent)
	var due map[string]int
	if class == convergent {
		due = s.voiceDropShedState.recordN(class, dropped, time.Now())
	} else {
		due = s.voiceDropShedState.recordNReportFirst(class, dropped, time.Now())
	}
	if due == nil {
		return
	}
	for dueClass, dueCount := range due {
		// Severity is per REPORTED class, not per triggering class: a flush
		// carries every class accumulated in the window, not only the one that
		// happened to cross the interval. A convergent drop is repaired by the
		// room's next heartbeat; a terminal or unresolvable one has no successor
		// event, so it is the level that wakes someone.
		if dueClass == convergent {
			s.log.Warn(voiceDropShedMessage, "failure_class", dueClass, "dropped", dueCount)
			continue
		}
		s.log.Error(voiceDropShedMessage, "failure_class", dueClass, "dropped", dueCount)
	}
	// AFTER the emit loop, unlike reportShed: the hook fires where it proves the
	// lines were written. ingress_gate_test.go records that watching it before
	// the loop left deleting the loop entirely green.
	if s.ingressShedLoggedHook != nil {
		s.ingressShedLoggedHook()
	}
}

// boundedShedCount clamps a drop tally into int32 range so the value is safe on
// any int width. NOTE the counter itself is int (64-bit on every target), so
// this is a portability floor rather than the counter's own width.
// Saturation is indistinguishable from a genuine 2147483647. The tally
// accumulates under the dispatcher lock with no ceiling, so it is clamped rather
// than converted: a report that wrapped negative would read as a healthy replica.
func boundedShedCount(count uint64) int {
	if count > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(count)
}

// voiceIngressChannelID extracts the room key G1 requires, and returns "" for
// anything it will not admit.
//
// It requires a UUID and not merely a non-empty string, for two reasons. Every
// downstream path resolves this field AS a uuid -- the DM and server handlers
// call uuid.Parse, and resolveRoom binds it straight to a uuid column and lets
// Postgres reject it with 22P02 -- so a non-UUID could never have been processed
// anyway and rejecting it here is free. (An earlier revision of this comment
// said every downstream HANDLER calls uuid.Parse; resolveRoom does not, and the
// conclusion holds by a different mechanism.) And the
// key is used as a map key in a bounded-by-COUNT residency map, so without a
// shape constraint an attacker supplies arbitrary-length keys (NATS default max
// payload is 1MB) and bounds the map's entry count while blowing its byte
// footprint.
//
// It deliberately does NOT reuse voiceLifecycleDispatchRoomKey: that function
// falls back to sha256(subject+data) when channelId is absent, so every distinct
// forged byte-string mints a distinct room key. That fallback IS the
// room-map-exhaustion primitive G1 exists to remove. Gating here rather than
// deleting the fallback localizes this change to its own surface -- the fallback
// is depended on by lifecycle_dispatcher_test.go, so removing it needs a change
// that owns that test too.
//
// An earlier revision attributed that removal to #2757. Verified 2026-08-22:
// #2757's body ("WS state externalization: Redis interest maps + NATS bcast
// fan-out") contains ZERO occurrences of "dispatcher" and restructures hub.go,
// not this path. The constraint stands on the test dependency alone; do not
// wait on #2757 for it, and do not cite #2757 as its owner.
func voiceIngressChannelID(data []byte) string {
	var envelope struct {
		ChannelID string `json:"channelId"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return ""
	}
	id, err := uuid.Parse(envelope.ChannelID)
	if err != nil {
		return ""
	}
	// Return the CANONICAL form, never the raw input. uuid.Parse accepts many
	// representations of the same UUID — mixed hex case, {braces}, a urn:uuid:
	// prefix, and the 32-char hyphenless form — and this string is used directly
	// as the per-room budget key. Returning the raw input let one logical room
	// alias into an unbounded number of distinct keys, each with its own bucket
	// and its own burst, which defeats G2's per-room bound outright and lets
	// aliases of a single room fill the count-bounded residency map.
	//
	// Measured before the fix: five aliases of one room admitted 320 messages
	// against a per-room burst of 64. Case variation alone is a far larger space
	// than five. Honest traffic is unaffected — clients emit canonical lowercase.
	//
	// The erasure door never had this because it keys on erased.String(), the
	// parsed value. Here the validation and the key came from DIFFERENT values:
	// parse the input, then key on the input. Found by Gitar on PR #2871; an
	// adversarial pass had cleared the same alias class on the erasure door and
	// generalized from it. Locked by TestUUIDAliasesCollapseToOneBudgetKey.
	return id.String()
}

// voiceIngressHandledSubjects is exactly the set handleVoiceLifecycleEvent
// cases on. voice.* is a single-token wildcard and delivers MORE than this:
// voice.producer_added and voice.producer_removed come from the media plane, and
// voice.user_mute / voice.user_deafen are published BY this service for the media
// plane to consume. None of the four has a case in the handler, so all four were
// copied, room-keyed, queued, dispatched and then silently dropped.
//
// Rejecting them at the subscription rather than after the dispatcher is not an
// optimisation: while G2 metered every subject, that discarded traffic was
// spending the room's budget and starving its genuine events.
//
// TestHandledSubjectsMatchTheHandlerCases pins this set against the handler's
// actual switch, so adding a case without adding the subject here fails rather
// than silently dropping the new event.
var voiceIngressHandledSubjects = map[string]bool{
	natsSubjectVoiceJoined:    true,
	natsSubjectVoiceLeft:      true,
	natsSubjectVoiceRoomEmpty: true,
	natsSubjectVoiceHeartbeat: true,
}

// voiceIngressMeteredSubjects is the subset G2 meters.
//
// ONLY voice.heartbeat. See the G2 constant block for why: shedding a heartbeat
// is a delay, shedding a one-shot state transition is permanent divergence.
//
// WHAT THIS COSTS, stated rather than implied: B1 bounds forged HEARTBEAT
// volume. It does NOT bound forged state-transition volume. A flood of forged
// voice.joined on the unauthenticated bus is admitted to the dispatcher, which
// is itself a bounded fair scheduler, and the residual volume class is owned by
// #2868. That is a narrower claim than the original design made, and it is
// narrower on purpose -- the protection given up was never sound, because it was
// shedding honest traffic to provide it.
var voiceIngressMeteredSubjects = map[string]bool{
	natsSubjectVoiceHeartbeat: true,
}

// gateVoiceLifecycle wraps the lifecycle handler with G1 and G2.
//
// It wraps at the SUBSCRIPTION REGISTRATION site rather than inside
// dispatcher.enqueue, because enqueue copies the payload as its first statement
// and computes a SHA-256 room key before taking its lock. A gate inside enqueue
// would already have paid both on the reject path.
//
// WHAT THIS DOES NOT COVER, stated rather than implied:
//
//   - It bounds forged VOLUME per room. It does NOT bound the number of distinct
//     rooms an attacker can introduce, because the only mechanism for that is a
//     global new-key budget and such a budget starves honest rooms (see the
//     removal note above).
//   - It therefore does NOT prevent dispatcher room-map exhaustion, and does not
//     prevent handleVoiceLifecycleDispatchOverflow from firing. That CONDITION
//     is still uncovered, and bounding it is #2757's scope, not this gate's.
//   - Nor could it. The condition is reachable by HONEST LOAD with no attacker:
//     worst-case dispatcher drain is voiceLifecycleDispatchWorkerCount divided by
//     richPresenceLifecycleTimeout, while an unpaced heartbeat tick from a large
//     deployment far exceeds it. That is latency amplification, not volume
//     amplification, and no ingress rate limit closes it.
//   - What the overflow arm DOES on that condition changed in #2868: it no
//     longer disconnects anything. Each dropped event is classified and counted
//     (dispatch_drop_convergent, dispatch_drop_terminal,
//     dispatch_drop_unresolvable) and reported per class on an interval by
//     voiceDropShed above. The events are still dropped -- that cost is
//     unchanged -- but crossing the ceiling is no longer destructive.
//   - disconnectAllRichPresenceClients has 47 call sites in nats.go, down from
//     48: the overflow arm was the one this gate could point at. The rest are
//     fail-closed arms on deadline, state_read and dependency failures, and ALL
//     are reachable by load. This gate raises the cost of DRIVING them. It does
//     not remove any of them.
func (s *NATSSubscriber) gateVoiceLifecycle(
	next func(string, []byte),
) func(string, []byte) {
	return func(subject string, data []byte) {
		// G-1 -- bound the bytes BEFORE any parse.
		//
		// json.Unmarshal runs on a raw NATS payload, and the transport's
		// per-message ceiling (1MB by default) bounds ONE message, not aggregate
		// parser work: an attacker publishing maximum-size documents buys
		// megabytes of scanning per second on the subscription's callback
		// goroutine, ahead of every gate below. Rejecting on length is O(1) and
		// needs no parse.
		//
		// The bound is derived, not picked: the largest honest payload is a
		// heartbeat carrying maxServerVoiceParticipantIDs UUID strings at ~40
		// bytes each, so ~40KB plus envelope. 128KB leaves roughly 3x headroom
		// over the largest honest message and still refuses a 1MB document.
		// (CodeRabbit, PR #2871 -- CWE-400.)
		if len(data) > voiceIngressMaxPayloadBytes {
			s.voiceShed("oversized_payload")
			return
		}
		// G1 -- no usable room key, no admission.
		channelID := voiceIngressChannelID(data)
		if channelID == "" {
			s.voiceShed("invalid_event")
			return
		}
		// G0 -- a subject this subscriber does not handle never enters the
		// dispatcher at all. It was previously copied, keyed, queued, dispatched
		// and dropped, AND it spent the room's budget on the way.
		if !voiceIngressHandledSubjects[subject] {
			s.voiceShed("unhandled_subject")
			return
		}
		// G2 -- per-room, heartbeat ONLY. This call is also what makes the key
		// resident; nothing may shed a metered message before reaching it, or the
		// key never becomes known and is metered as new forever. That was the
		// removed new-room gate's defect.
		if voiceIngressMeteredSubjects[subject] && !s.voiceRoomBudget.Allow(channelID) {
			s.voiceShed("ingress_budget")
			return
		}
		next(subject, data)
	}
}
