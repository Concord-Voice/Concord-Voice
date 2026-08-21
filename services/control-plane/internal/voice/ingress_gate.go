package voice

import (
	"encoding/json"
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
	// transition is PERMANENT DIVERGENCE: voice_participants has exactly one
	// INSERT site (handleJoined) and heartbeat reconciliation is removal-only, so
	// a shed joined leaves no row for the session; and a shed room_empty is
	// terminal, because the room is already destroyed and no successor message
	// exists. Metering a class whose loss is unrecoverable was the error.
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
}

// record counts a shed and returns the accumulated counts when a report is due,
// nil otherwise.
//
// Counting and reporting are AGGREGATED, never once per message. A gate that
// logs once per rejection IS an amplification primitive: it hands a forged flood
// one log write per message and recreates in the log exactly the amplification
// the gate just removed from the socket. Same reasoning as
// Hub.ClearErasedSenderCustomText (internal/websocket/customtext.go).
func (g *ingressShedState) record(class string, now time.Time) map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.counts == nil {
		g.counts = map[string]int{}
	}
	g.counts[class]++

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
// is depended on by lifecycle_dispatcher_test.go, and removing it belongs with
// #2757's restructuring of this path.
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
//     prevent handleVoiceLifecycleDispatchOverflow from firing.
//   - Nor could it. That teardown is reachable by HONEST LOAD with no attacker:
//     worst-case dispatcher drain is voiceLifecycleDispatchWorkerCount divided by
//     richPresenceLifecycleTimeout, while an unpaced heartbeat tick from a large
//     deployment far exceeds it. That is latency amplification, not volume
//     amplification, and no ingress rate limit closes it. Owned by #2868.
//   - disconnectAllRichPresenceClients has 48 call sites in this file. Only one
//     is the overflow arm; the rest are fail-closed arms on deadline, state_read
//     and dependency failures, and ALL are reachable by load. This gate raises
//     the cost of DRIVING them. It does not remove any of them.
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
