package voice

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Issue #2854 stage B1 -- ingress admission control on the voice-lifecycle door.
//
// These assert against a COUNTING HANDLER rather than the overflow hook. The
// overflow hook only fires once the dispatcher is already saturated, so a test
// keyed on it would pass even if every message were admitted -- the same
// vacuity shape that made the erasure-clear tests useless in PR #2840.

func gatedVoiceSubscriber() *NATSSubscriber {
	return &NATSSubscriber{
		log:             logger.New("test"),
		voiceRoomBudget: newVoiceRoomBudget(),
	}
}

func roomPayload(channelID string) []byte {
	return []byte(`{"channelId":"` + channelID + `"}`)
}

// G1. A payload with no usable room key is rejected before enqueue.
func TestPayloadWithoutChannelIDNeverReachesEnqueue(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	gated("voice.joined", []byte(`{"userId":"`+uuid.New().String()+`"}`))
	gated("voice.joined", []byte(`{"channelId":""}`))
	gated("voice.joined", []byte(`not json at all`))
	gated("voice.joined", nil)

	require.Zero(t, admitted,
		"a payload with no usable channelId mints a fresh sha256 room key in "+
			"voiceLifecycleDispatchRoomKey and IS the room-map-exhaustion "+
			"primitive; G1 must reject it before enqueue")
}

// G1 requires a UUID, not merely a non-empty string.
//
// Regression lock on adversarial finding C5: residency is bounded by key COUNT,
// so an attacker supplying arbitrary-length keys bounded that count while
// blowing the map's byte footprint -- NATS default max payload is 1MB, against
// 2*2048 tracked keys. Requiring a UUID caps every key at 36 bytes and is free,
// because every downstream handler already calls uuid.Parse on this field.
func TestNonUUIDChannelIDIsRejected(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	gated("voice.joined", roomPayload("not-a-uuid"))
	gated("voice.joined", roomPayload(strings.Repeat("A", 100_000)))

	require.Zero(t, admitted,
		"a non-UUID channelId could never be processed downstream anyway, and "+
			"an unbounded-length one turns a count-bounded map into an "+
			"unbounded-bytes one")
}

func TestHonestPayloadWithChannelIDIsAdmitted(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	gated("voice.joined", roomPayload(uuid.New().String()))

	require.Equal(t, 1, admitted, "honest traffic must pass untouched")
}

// THE REGRESSION LOCK ON THE REMOVED NEW-ROOM GATE (adversarial finding C2).
//
// A third gate metering the first admission of any non-resident room key was
// designed, implemented, and removed. It sheds a message BEFORE it reaches
// voiceRoomBudget.Allow(), and that call is the only thing that makes a key
// resident -- so a shed key stayed non-resident and was metered as new forever.
// An attacker sustaining a few random channelIds per second denied essentially
// every new voice room on the replica: 512 forged keys to drain the burst, then
// 50 consecutive honest attempts, ZERO admitted.
//
// This test fails if such a gate is reintroduced. Read the removal note in
// ingress_gate.go before changing it -- no budget SIZE fixes this, because the
// gate cannot distinguish a forged key from an honest one.
func TestAFloodOfNewRoomKeysDoesNotDenyAnHonestNewRoom(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := map[string]int{}
	gated := s.gateVoiceLifecycle(func(_ string, d []byte) { admitted[string(d)]++ })

	for range 4096 {
		gated("voice.joined", roomPayload(uuid.New().String()))
	}

	honest := roomPayload(uuid.New().String())
	gated("voice.joined", honest)

	require.Equal(t, 1, admitted[string(honest)],
		"an honest NEW room must be admitted no matter how many distinct room "+
			"keys a flood has already introduced")
}

// The honest-traffic non-regression. media-plane emits its heartbeat as an
// unpaced for-loop inside one setInterval tick, so on a restart or bus reconnect
// every room arrives at once as a new key.
func TestAnHonestColdStartIsAdmittedInFull(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	rooms := make([]string, 1000)
	for i := range rooms {
		rooms[i] = uuid.New().String()
	}
	for _, room := range rooms { // cold start: every key is new
		gated("voice.heartbeat", roomPayload(room))
	}
	require.Equal(t, len(rooms), admitted,
		"a cold start must be admitted whole; nothing gates a key's FIRST message")

	admitted = 0
	for _, room := range rooms { // steady state: every key is resident
		gated("voice.heartbeat", roomPayload(room))
	}
	require.Equal(t, len(rooms), admitted,
		"and an established room's heartbeat must never be shed")
}

// G2 is per-room, which is what makes it structurally incapable of the
// cross-room starvation that removed the new-room gate.
func TestOneNoisyRoomDoesNotShedAnother(t *testing.T) {
	s := gatedVoiceSubscriber()
	seen := map[string]int{}
	gated := s.gateVoiceLifecycle(func(_ string, data []byte) { seen[string(data)]++ })

	noisy := roomPayload(uuid.New().String())
	quiet := roomPayload(uuid.New().String())

	// heartbeat, because that is now the only metered subject.
	for range 500 {
		gated("voice.heartbeat", noisy)
	}
	gated("voice.heartbeat", quiet)

	require.LessOrEqual(t, seen[string(noisy)], voiceRoomBudgetBurst+2,
		"the noisy room's heartbeats are metered by its own bucket")
	require.Equal(t, 1, seen[string(quiet)],
		"and a quiet room is unaffected -- G2 is per-room for exactly this reason")
}

// AC11 / adversarial finding C3. A gate that logs once per rejection IS the
// amplification primitive it was added to remove, so the two counts must
// diverge under load: sheds unbounded, reports bounded.
func TestGateShedsAreReportedAggregatedNotPerMessage(t *testing.T) {
	s := gatedVoiceSubscriber()
	classes := map[string]int{}
	reports := 0
	s.ingressShedObservedHook = func(c string) { classes[c]++ }
	s.ingressShedLoggedHook = func() { reports++ }
	gated := s.gateVoiceLifecycle(func(string, []byte) {})

	for range 10_000 {
		gated("voice.joined", []byte(`{}`)) // G1 rejects every one
	}

	// Assert the CLASS, not merely the count. Both aggregation tests used to
	// discard the class parameter, so swapping invalid_event for ingress_budget
	// left the whole suite green -- and [internal]rules/backend.md pins these as a
	// CLOSED vocabulary whose values carry alerting meaning.
	require.Equal(t, map[string]int{"invalid_event": 10_000}, classes,
		"a missing room key is invalid_event, never a budget shed")
	require.LessOrEqual(t, reports, 2, "10000 sheds must not buy 10000 log writes")
}

func TestUnwiredVoiceGateAdmitsEverything(t *testing.T) {
	s := &NATSSubscriber{log: logger.New("test")}
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	for range 10 {
		gated("voice.joined", roomPayload(uuid.New().String()))
	}

	require.Equal(t, 10, admitted,
		"an unwired gate is a no-op, never a deny -- G1 still applies, but the "+
			"budget must not refuse traffic when it was never constructed")
}

// Regression lock on the Gitar finding from PR #2871: representation aliases of
// one room must collapse to ONE budget key.
//
// uuid.Parse accepts mixed case, {braces}, a urn:uuid: prefix and the 32-char
// hyphenless form. Keying the per-room budget on the RAW input rather than the
// parsed value gave each representation its own bucket and its own burst, so
// G2's "bounds forged volume PER ROOM" was defeated by spelling the same room
// differently. Measured before the fix: five aliases admitted 320 messages
// against a burst of 64, and case variation alone is a far larger space.
func TestUUIDAliasesCollapseToOneBudgetKey(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	canonical := uuid.New().String()
	aliases := []string{
		canonical,
		strings.ToUpper(canonical),
		"{" + canonical + "}",
		"urn:uuid:" + canonical,
		strings.ReplaceAll(canonical, "-", ""),
	}
	for _, a := range aliases {
		_, err := uuid.Parse(a)
		require.NoErrorf(t, err, "precondition: uuid.Parse must accept the alias %q", a)
	}

	// heartbeat, because that is the only METERED subject after the G2 narrowing
	// -- the aliasing property is about budget-key identity, so it is only
	// observable on a subject whose budget is actually consulted. The property
	// itself is unchanged: every representation of one room must share a bucket.
	for _, a := range aliases {
		for range voiceRoomBudgetBurst + 20 {
			gated("voice.heartbeat", roomPayload(a))
		}
	}

	require.LessOrEqual(t, admitted, voiceRoomBudgetBurst+2,
		"every representation of one room must share a single bucket; %d aliases "+
			"must not buy %d times the per-room burst", len(aliases), len(aliases))
}

// THE FLUSH ARM. Drives ingressShedState directly with an injected clock,
// because reportShed calls record with time.Now() and every gate test completes
// far inside ingressShedLogInterval -- so record returns nil every time,
// `reports` is exactly 0, and the aggregation tests above pass VACUOUSLY.
//
// Concretely: make record always return nil and every other test in this package
// stays green. That leaves half of the AC11 contract unproven -- not "sheds are
// not logged per message", which they cover, but "a sustained flood IS reported
// at all", which nothing did.
func TestShedCountsFlushOnceTheIntervalElapses(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	var g ingressShedState

	require.Nil(t, g.record("ingress_budget", base),
		"the first shed arms the interval rather than reporting immediately")
	require.Nil(t, g.record("ingress_budget", base.Add(ingressShedLogInterval-time.Nanosecond)),
		"still inside the interval")

	due := g.record("replay", base.Add(ingressShedLogInterval))
	require.Equal(t, map[string]int{"ingress_budget": 2, "replay": 1}, due,
		"the flush carries the shed that armed the interval and everything since")

	require.Nil(t, g.record("replay", base.Add(ingressShedLogInterval)),
		"and the counts reset, so the next window starts clean")
}

// A low-and-slow attacker must still be reported. One shed per interval is the
// pathological pacing for a report-on-shed design, so assert it converges rather
// than assuming it.
func TestALowAndSlowShedderIsStillReported(t *testing.T) {
	base := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	var g ingressShedState

	require.Nil(t, g.record("invalid_event", base))
	reports := 0
	for i := 1; i <= 5; i++ {
		if g.record("invalid_event", base.Add(time.Duration(i)*ingressShedLogInterval)) != nil {
			reports++
		}
	}
	require.Equal(t, 5, reports,
		"a shedder pacing exactly at the interval is reported every window, "+
			"never silently accumulated")
}

// THE GATE MUST BE INSTALLED, NOT MERELY CONSTRUCTED.
//
// Every other test in this file calls s.gateVoiceLifecycle(...) DIRECTLY, and
// TestNewNATSSubscriberWiresTheIngressGates asserts the budget fields are
// non-nil. Neither reaches the registration site in Subscribe(), so reverting
// that one line to a bare dispatcher.enqueue left all of them green with G1 and
// G2 entirely absent in production.
//
// The failure shape is why this matters more than it looks: a disabled gate
// sheds nothing, so shed_count reads ZERO -- byte-identical to a healthy
// replica. The signal that proves the gate works is indistinguishable from the
// signal that proves it was deleted, so nothing at runtime would ever surface
// the regression.
//
// This is a SOURCE PIN rather than a behavioural test, and deliberately so: the
// subscriber holds a concrete *natsclient.Client with no interface seam, so the
// registered handler cannot be captured without a production refactor whose only
// consumer would be this test. The repo already uses this idiom for wiring that
// cannot otherwise be reached (internal/api/router_rich_presence_test.go). If a
// seam appears later, replace this with a real capture-and-feed test.
//
// The erasure door needs no equivalent: its gates run inside
// handlePresenceErasureCleared, so registration cannot un-wire them.
func TestSubscribeInstallsTheVoiceGate(t *testing.T) {
	source, err := os.ReadFile("nats.go")
	require.NoError(t, err)
	text := string(source)

	require.Equal(t, 1, strings.Count(text, "s.gateVoiceLifecycle(dispatcher.enqueue)"),
		"Subscribe() must register the GATED handler exactly once")

	require.NotRegexp(t,
		`SubscribeWithSubject\(\s*natsSubjectVoiceWildcard,\s*dispatcher\.enqueue\s*\)`,
		text,
		"the wildcard subscription must never register the bare dispatcher; "+
			"that single-line revert disables G1 and G2 with every test green "+
			"and zero runtime signal")
}

// reportShed's EMISSION arm — the one line that actually reaches an operator.
//
// This asserts the LOG OUTPUT, not the ingressShedLoggedHook. That distinction
// is the whole finding: an earlier revision of this test watched the hook, and
// the hook fires BEFORE the emit loop, so deleting the loop entirely left the
// test green. Watching a test seam proves the seam ran, never that the thing
// after it did.
//
// The flush tests above cover ingressShedState.record and bypass reportShed
// completely, so without this nothing held the claim that a sustained flood is
// reported to anyone at all.
func TestReportShedWritesAnAggregatedLogLine(t *testing.T) {
	var sink bytes.Buffer
	s := gatedVoiceSubscriber()
	s.log = logger.NewWithWriter(&sink)

	// Arm the interval, then age it past the threshold.
	s.voiceShed("invalid_event")
	require.Empty(t, sink.String(), "the arming shed does not itself emit")

	s.voiceShedState.mu.Lock()
	s.voiceShedState.loggedAt = time.Now().Add(-2 * ingressShedLogInterval)
	s.voiceShedState.mu.Unlock()

	s.voiceShed("ingress_budget")

	out := sink.String()
	require.NotEmpty(t, out,
		"a shed arriving after the interval must reach the log; without this the "+
			"gate can be entirely silent with every other test green")
	require.Contains(t, out, "Voice lifecycle event shed at the ingress gate")
	require.Contains(t, out, "failure_class=invalid_event",
		"the aggregated line carries the class of the shed that armed the window")
	require.Contains(t, out, "shed_count=1")

	// And it re-arms rather than emitting on every subsequent shed.
	sink.Reset()
	s.voiceShed("ingress_budget")
	require.Empty(t, sink.String(), "the window resets after a flush")
}

// INVERTED FROM THE RED-TEAM PoC. Producer churn must not starve a room's
// genuine lifecycle events.
//
// The PoC measured 486 genuine voice.left offered in 1.2s under producer churn
// with ZERO admitted, and its reach path is the severe part: an ORDINARY
// AUTHENTICATED PARTICIPANT, no bus access at all. media-plane's own limits
// allow ~2 producer publishes per second per socket, so three sockets in one
// room out-produce the per-room refill.
//
// Two things now prevent it, and this test fails if EITHER is undone:
// producer subjects are rejected before the dispatcher (they have no handler
// case), and metering is confined to voice.heartbeat.
func TestProducerChurnCannotStarveGenuineLifecycleEvents(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := map[string]int{}
	gated := s.gateVoiceLifecycle(func(subj string, _ []byte) { admitted[subj]++ })

	room := roomPayload(uuid.New().String())

	// Churn far past the per-room burst on the discarded subjects.
	for range voiceRoomBudgetBurst * 8 {
		gated("voice.producer_added", room)
		gated("voice.producer_removed", room)
	}
	// Then the genuine one-shot transitions for that same room.
	for range 20 {
		gated("voice.left", room)
		gated("voice.joined", room)
		gated("voice.room_empty", room)
	}

	require.Zero(t, admitted["voice.producer_added"],
		"a subject with no handler case must never reach the dispatcher")
	require.Zero(t, admitted["voice.producer_removed"], "likewise")
	require.Equal(t, 20, admitted["voice.left"],
		"churn must not shed a single genuine voice.left -- the PoC measured "+
			"486 offered and zero admitted")
	require.Equal(t, 20, admitted["voice.joined"],
		"a shed joined is unrecoverable: one INSERT site, removal-only reconciliation")
	require.Equal(t, 20, admitted["voice.room_empty"],
		"a shed room_empty is terminal: the room is already destroyed")
}

// One-shot state transitions are NEVER metered, however many arrive.
//
// This is the narrowed claim made executable: B1 bounds forged HEARTBEAT
// volume, not forged state-transition volume. If someone reattaches metering to
// these subjects "for symmetry", this goes red and the removal note above
// explains why the symmetry is wrong.
func TestOneShotTransitionsAreNeverMetered(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	room := roomPayload(uuid.New().String())
	const burstsWorth = voiceRoomBudgetBurst * 4

	// EVERY one-shot subject, not only voice.joined. Metering any one of them is
	// the same permanent divergence, and a version of this test that saturated
	// only voice.joined stayed green while voice.left or voice.room_empty could
	// be metered freely -- the churn test above offers 20 of each, well under the
	// burst, so it did not cover the gap either (CodeRabbit, PR #2871).
	for _, subject := range []string{
		natsSubjectVoiceJoined, natsSubjectVoiceLeft, natsSubjectVoiceRoomEmpty,
	} {
		admitted = 0
		for range burstsWorth {
			gated(subject, room)
		}
		require.Equal(t, burstsWorth, admitted,
			"shedding a one-shot transition is permanent divergence, so none is "+
				"shed on "+subject+"; the residual volume class is #2868's")
	}
}

// The handled-subject set must equal the handler's switch. Adding a case
// without adding the subject silently drops the new event at the gate; adding a
// subject without a case reintroduces the copy-queue-dispatch-discard waste.
func TestHandledSubjectsMatchTheHandlerCases(t *testing.T) {
	source, err := os.ReadFile("nats.go")
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func (s *NATSSubscriber) handleVoiceLifecycleEvent")
	require.Positive(t, start, "handler not found -- this pin needs updating")
	end := strings.Index(body[start:], "\n}\n")
	require.Positive(t, end, "handler body not delimited as expected")
	handler := body[start : start+end]

	// DISCOVER the case labels; do not probe a fixed list. Probing four known
	// constants passes when a FIFTH case is added and both maps are left alone
	// -- which is exactly the state where the gate silently drops the new event
	// (CodeRabbit, PR #2871). Discovery turns that into a loud failure here.
	labels := regexp.MustCompile(`(?m)^\s*case (natsSubject\w+):`).
		FindAllStringSubmatch(handler, -1)
	require.NotEmpty(t, labels, "no case labels found -- this pin needs updating")

	known := map[string]string{
		"natsSubjectVoiceJoined":    natsSubjectVoiceJoined,
		"natsSubjectVoiceLeft":      natsSubjectVoiceLeft,
		"natsSubjectVoiceRoomEmpty": natsSubjectVoiceRoomEmpty,
		"natsSubjectVoiceHeartbeat": natsSubjectVoiceHeartbeat,
	}

	cased := map[string]bool{}
	for _, m := range labels {
		subject, ok := known[m[1]]
		require.True(t, ok,
			"handler switches on %s, which this test cannot resolve. Add it to "+
				"known AND to voiceIngressHandledSubjects -- until then the gate "+
				"drops every message on that subject", m[1])
		cased[subject] = true
	}

	require.Equal(t, cased, voiceIngressHandledSubjects,
		"voiceIngressHandledSubjects must equal the handler's switch exactly")
}

// G-1. Oversized payloads are refused on LENGTH, before any parse.
//
// The transport's per-message ceiling bounds one message, not aggregate parser
// work: json.Unmarshal ran on the raw payload ahead of every gate, so an
// attacker publishing maximum-size documents bought megabytes of scanning per
// second on the subscription's callback goroutine (CodeRabbit, CWE-400).
func TestOversizedPayloadsAreRefusedBeforeParsing(t *testing.T) {
	s := gatedVoiceSubscriber()
	admitted := 0
	classes := map[string]int{}
	s.ingressShedObservedHook = func(c string) { classes[c]++ }
	gated := s.gateVoiceLifecycle(func(string, []byte) { admitted++ })

	room := uuid.New().String()
	// MALFORMED and oversized -- unterminated string, no closing brace. This is
	// what proves ORDERING: a length check running BEFORE the parse yields
	// oversized_payload, while a parse running first fails, leaves channelID
	// empty, and yields invalid_event. The two orderings are distinguishable.
	//
	// An earlier revision used a VALID oversized document and claimed in a
	// comment that it proved ordering. It did not: a valid document parses,
	// yields a good channelID, and only then reaches the length check, so the
	// class is oversized_payload under BOTH orderings and the test passed either
	// way (CodeRabbit, PR #2871).
	malformed := []byte(`{"channelId":"` + room + `","pad":"` +
		strings.Repeat("A", voiceIngressMaxPayloadBytes))
	gated("voice.heartbeat", malformed)

	require.Zero(t, admitted, "an oversized payload must never reach the dispatcher")
	require.Equal(t, map[string]int{"oversized_payload": 1}, classes,
		"refused on LENGTH before the parse -- an invalid_event class here means "+
			"the parse ran first, which is the defect this gate exists to remove")

	// The largest honest shape still passes: a full-room heartbeat.
	ids := make([]string, maxServerVoiceParticipantIDs)
	for i := range ids {
		ids[i] = `"` + uuid.New().String() + `"`
	}
	honest := []byte(`{"channelId":"` + room + `","userIds":[` + strings.Join(ids, ",") + `]}`)
	require.Less(t, len(honest), voiceIngressMaxPayloadBytes,
		"the bound must clear the largest honest payload with headroom")
	gated("voice.heartbeat", honest)
	require.Equal(t, 1, admitted, "a full-room heartbeat is admitted")
}

// The ACCEPTED RESIDUAL, made executable rather than only documented.
//
// erasureBudget is replica-wide and distinct UUIDs bypass the dedup window, so
// a flood of distinct forged identifiers exhausts the burst and can shed a
// GENUINE clear. That is documented in the erasure constant block as
// unfixable without bus authentication (#2857) -- resizing cannot distinguish
// forged from genuine. Asserting it keeps the residual honest: if someone
// later "fixes" it by resizing, this still fails and points at the reasoning.
func TestDistinctUUIDFloodCanShedAGenuineClear_AcceptedResidual(t *testing.T) {
	var o erasureClearObserver
	s := gatedErasureSubscriber(&o)
	s.db = withTestDB(t)

	for range erasureBudgetBurst * 4 {
		s.handlePresenceErasureCleared(
			[]byte(`{"user_id":"` + uuid.New().String() + `"}`))
	}

	genuine := uuid.New()
	s.handlePresenceErasureCleared([]byte(`{"user_id":"` + genuine.String() + `"}`))

	require.NotContains(t, o.cleared, genuine,
		"ACCEPTED RESIDUAL: a distinct-UUID flood sheds a genuine clear. This is "+
			"not a bug to fix by resizing -- a genuine and a forged clear are "+
			"byte-identical and the existence probe cannot separate them either, "+
			"so the only real fix is authenticating the bus (#2857). If this "+
			"assertion ever fails, the residual closed and the docs must follow.")
}
