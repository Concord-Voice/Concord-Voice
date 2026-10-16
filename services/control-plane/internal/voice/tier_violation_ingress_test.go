package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type recordedTierViolation struct {
	userID uuid.UUID
	reason entitlements.ViolationReason
}

// recordingTierViolationSink stands in for *entitlements.ViolationRecorder,
// whose counting and escalation are proven in internal/entitlements. What this
// file owns is which NATS messages reach it.
type recordingTierViolationSink struct {
	mu    sync.Mutex
	calls []recordedTierViolation
}

func (r *recordingTierViolationSink) Record(_ context.Context, userID uuid.UUID, reason entitlements.ViolationReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedTierViolation{userID: userID, reason: reason})
}

func (r *recordingTierViolationSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

type tierViolationDoor struct {
	s        *NATSSubscriber
	sink     *recordingTierViolationSink
	classes  map[string]int
	enqueued int
	gated    func(string, []byte)
}

// newTierViolationDoor builds a subscriber with the door's budgets and a
// recording sink. send reaches admitTierViolation directly -- the function
// Subscribe registers on the tier-violation queue group -- while gated is the
// voice.* wildcard handler, which must NOT route the subject to the door
// (TestTierViolationWildcardCopyIsDroppedSilently).
func newTierViolationDoor() *tierViolationDoor {
	d := &tierViolationDoor{sink: &recordingTierViolationSink{}, classes: map[string]int{}}
	d.s = &NATSSubscriber{
		log:                     logger.New("test"),
		voiceRoomBudget:         newVoiceRoomBudget(),
		tierViolationUserBudget: newTierViolationUserBudget(),
		tierViolationBudget:     newTierViolationBudget(),
		tierViolations:          d.sink,
	}
	d.s.ingressShedObservedHook = func(c string) { d.classes[c]++ }
	d.gated = d.s.gateVoiceLifecycle(func(string, []byte) { d.enqueued++ })
	return d
}

func (d *tierViolationDoor) send(payload string) {
	d.s.admitTierViolation([]byte(payload))
}

func tierViolationPayload(userID, reason string) string {
	return `{"userId":"` + userID + `","reason":"` + reason + `","timestamp":"2026-10-15T18:00:00.000Z"}`
}

func TestTierViolationDoorRecordsAValidReport(t *testing.T) {
	d := newTierViolationDoor()
	user := uuid.New()

	d.send(tierViolationPayload(user.String(), "audio_ptime_below_tier"))
	d.send(tierViolationPayload(user.String(), "audio_bitrate_over_tier"))

	require.Equal(t, []recordedTierViolation{
		{userID: user, reason: entitlements.ViolationAudioPtimeBelowTier},
		{userID: user, reason: entitlements.ViolationAudioBitrateOverTier},
	}, d.sink.calls)
	assert.Zero(t, d.enqueued, "a tier violation never enters the lifecycle dispatcher")
	assert.Empty(t, d.classes, "and is never shed by a lifecycle gate")
}

func TestTierViolationDoorRejectsWithoutRecording(t *testing.T) {
	user := uuid.New().String()
	// MALFORMED and oversized: a parse running before the length bound would
	// report malformed_payload, so only this shape proves the ORDER.
	oversizedMalformed := `{"userId":"` + user + `","reason":` + strings.Repeat("x", tierViolationMaxPayloadBytes)
	cases := []struct {
		name, payload, class string
	}{
		{"oversized malformed", oversizedMalformed, "oversized_payload"},
		{"not json", `not json at all`, "malformed_payload"},
		{"wrong field type", `{"userId":42,"reason":"audio_ptime_below_tier"}`, "malformed_payload"},
		{"json array", `[]`, "malformed_payload"},
		{"missing user", `{"reason":"audio_ptime_below_tier"}`, "invalid_user"},
		{"bad uuid", tierViolationPayload("not-a-uuid", "audio_ptime_below_tier"), "invalid_user"},
		{"nil uuid", tierViolationPayload("00000000-0000-0000-0000-000000000000", "audio_ptime_below_tier"), "invalid_user"},
		{"uppercase uuid", tierViolationPayload(strings.ToUpper(user), "audio_ptime_below_tier"), "invalid_user"},
		{"braced uuid", tierViolationPayload("{"+user+"}", "audio_ptime_below_tier"), "invalid_user"},
		{"unknown reason", tierViolationPayload(user, "animated_profile_premium"), "invalid_reason"},
		{"media-plane raw reason", tierViolationPayload(user, "ptime_undeclared"), "invalid_reason"},
		{"attachment reason over NATS", tierViolationPayload(user, "attachment_over_tier"), "invalid_reason"},
		{"missing reason", `{"userId":"` + user + `"}`, "invalid_reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTierViolationDoor()
			d.send(tc.payload)
			assert.Zero(t, d.sink.count(), "a rejected report records nothing")
			assert.Equal(t, map[string]int{tc.class: 1}, d.classes)
			assert.Zero(t, d.enqueued)
		})
	}
}

func TestTierViolationDoorMetersPerUser(t *testing.T) {
	d := newTierViolationDoor()
	user := uuid.New().String()
	for i := 0; i < tierViolationUserBudgetBurst+3; i++ {
		d.send(tierViolationPayload(user, "audio_bitrate_over_tier"))
	}
	assert.Equal(t, tierViolationUserBudgetBurst, d.sink.count())
	assert.Equal(t, map[string]int{"ingress_budget": 3}, d.classes)

	// Another user is unaffected by the first one's exhaustion.
	d.send(tierViolationPayload(uuid.New().String(), "audio_bitrate_over_tier"))
	assert.Equal(t, tierViolationUserBudgetBurst+1, d.sink.count())
}

// Distinct forged ids each arrive with a full per-user burst; the global
// budget is what bounds them. The per-user budget runs FIRST, so one user's
// flood spends at most its own burst of the global tokens.
func TestTierViolationDoorMetersGloballyAfterPerUser(t *testing.T) {
	d := newTierViolationDoor()
	noisy := uuid.New().String()
	for i := 0; i < 50; i++ {
		d.send(tierViolationPayload(noisy, "audio_bitrate_over_tier"))
	}
	require.Equal(t, tierViolationUserBudgetBurst, d.sink.count())

	remaining := tierViolationBudgetBurst - tierViolationUserBudgetBurst
	for i := 0; i < remaining+5; i++ {
		d.send(tierViolationPayload(uuid.New().String(), "audio_ptime_below_tier"))
	}
	assert.Equal(t, tierViolationBudgetBurst, d.sink.count(),
		"distinct ids are capped by the global burst")
	assert.Equal(t, map[string]int{"ingress_budget": 50 - tierViolationUserBudgetBurst + 5}, d.classes)
}

// Routing tier_violation first must not change what happens to everything
// else on voice.*.
func TestTierViolationRoutingLeavesOtherSubjectsUnchanged(t *testing.T) {
	d := newTierViolationDoor()
	d.gated("voice.producer_added", roomPayload(uuid.New().String()))
	d.gated(natsSubjectVoiceJoined, roomPayload(uuid.New().String()))
	assert.Equal(t, map[string]int{"unhandled_subject": 1}, d.classes)
	assert.Equal(t, 1, d.enqueued)
	assert.Zero(t, d.sink.count())
}

func TestTierViolationDoorWithoutASinkIsANoOp(t *testing.T) {
	d := newTierViolationDoor()
	d.s.tierViolations = nil
	assert.NotPanics(t, func() {
		d.send(tierViolationPayload(uuid.New().String(), "audio_ptime_below_tier"))
	})
	assert.Empty(t, d.classes)
}

func TestSetTierViolationRecorderWiresTheSink(t *testing.T) {
	s := NewNATSSubscriber(nil, logger.New("test"), nil, nil, nil, nil, nil)
	sink := &recordingTierViolationSink{}
	s.SetTierViolationRecorder(sink)
	s.admitTierViolation([]byte(tierViolationPayload(uuid.New().String(), "audio_ptime_below_tier")))
	assert.Equal(t, 1, sink.count())
}

// The report line is aggregated and carries only the class and a count.
func TestTierViolationShedReportIsAggregated(t *testing.T) {
	var sink bytes.Buffer
	d := newTierViolationDoor()
	d.s.log = logger.NewWithWriter(&sink)
	user := uuid.New().String()

	d.send(`{"userId":"` + user + `"`) // arms the interval silently
	require.Empty(t, sink.String())
	d.s.tierViolationShedState.mu.Lock()
	d.s.tierViolationShedState.loggedAt = d.s.tierViolationShedState.loggedAt.Add(-2 * ingressShedLogInterval)
	d.s.tierViolationShedState.mu.Unlock()
	d.send(tierViolationPayload(user, "nope"))

	out := sink.String()
	assert.Contains(t, out, tierViolationShedMessage)
	assert.Contains(t, out, "failure_class=malformed_payload")
	assert.Contains(t, out, "failure_class=invalid_reason")
	assert.NotContains(t, out, user, "never the payload")
	assert.Equal(t, 2, strings.Count(out, "shed_count="), fmt.Sprintf("one line per class: %s", out))
}

// voice.tier_violation reaches EVERY replica through the voice.* wildcard, and
// the queue-group subscription is what counts it once. The wildcard's copy must
// therefore be dropped: counted too, it would count one report once per replica
// into the fleet-shared window. Dropped means silently -- it is not refused, so
// no shed class is recorded either.
func TestTierViolationWildcardCopyIsDroppedSilently(t *testing.T) {
	d := newTierViolationDoor()

	d.gated(natsSubjectVoiceTierViolation,
		[]byte(tierViolationPayload(uuid.New().String(), "audio_ptime_below_tier")))

	assert.Zero(t, d.sink.count(), "the wildcard copy is never counted")
	assert.Zero(t, d.enqueued, "and never enters the lifecycle dispatcher")
	assert.Empty(t, d.classes, "and is not shed: it is delivered twice, not refused")
}

type pushedEntitlements struct {
	userID uuid.UUID
	dto    entitlements.EntitlementDTO
}

type recordingEntitlementPusher struct {
	mu     sync.Mutex
	pushes []pushedEntitlements
}

func (p *recordingEntitlementPusher) BroadcastEntitlements(userID uuid.UUID, dto entitlements.EntitlementDTO) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, pushedEntitlements{userID: userID, dto: dto})
}

type recordingSecurityEmitter struct {
	mu     sync.Mutex
	events []securityevent.Event
}

func (e *recordingSecurityEmitter) Emit(_ context.Context, event securityevent.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

type promptTier string

func (p promptTier) GetTierChecked(context.Context, string) (string, error) { return string(p), nil }

// The door wired to the REAL recorder, end to end: five admitted reports for
// one user reach the threshold, which flags once (one security event) and
// resyncs once (one entitlements push). The sink-recording tests above prove
// which messages reach the recorder; this proves what the recorder then does
// with what the door hands it, so a door/recorder contract drift (a reason the
// recorder refuses, a user id it cannot key) cannot hide behind the stub.
func TestTierViolationDoorDrivesTheRealRecorderToItsThreshold(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	pusher := &recordingEntitlementPusher{}
	emitter := &recordingSecurityEmitter{}
	recorder := entitlements.NewViolationRecorder(
		rdb, logger.New("test"), promptTier(entitlements.TierPremium), pusher, emitter)

	d := newTierViolationDoor()
	d.s.SetTierViolationRecorder(recorder)
	user := uuid.New()

	for i := 0; i < entitlements.ViolationThreshold; i++ {
		d.send(tierViolationPayload(user.String(), "audio_bitrate_over_tier"))
	}
	recorder.Wait()

	got, err := mr.Get(entitlements.ViolationKey(user))
	require.NoError(t, err)
	assert.Equal(t, "5", got, "all five reports were counted into the user's window")

	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	require.Len(t, pusher.pushes, 1, "exactly one resync push at the threshold")
	assert.Equal(t, user, pusher.pushes[0].userID)
	assert.Equal(t, entitlements.ToDTO(entitlements.For(entitlements.TierPremium)), pusher.pushes[0].dto)
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	assert.Len(t, emitter.events, 1, "exactly one security event at the threshold")
	assert.Empty(t, d.classes, "nothing was shed")
}

// Source pin, in the idiom of TestSubscribeInstallsTheVoiceGate: Subscribe
// holds a concrete *natsclient.Client with no seam, so the registration cannot
// be captured here. The live queue-group test (tier_violation_queue_test.go)
// proves the behaviour against a real server; this pin is the cheap check that
// still runs where NATS is unreachable. A plain Subscribe would deliver the
// report to every replica and count it once per replica, with every test that
// feeds the door directly still green.
func TestSubscribeRegistersTheTierViolationQueueGroup(t *testing.T) {
	source, err := os.ReadFile("nats.go")
	require.NoError(t, err)
	text := string(source)

	require.Equal(t, 1, strings.Count(text, "QueueSubscribe("),
		"nats.go must register exactly one queue subscription")
	require.Regexp(t,
		`QueueSubscribe\(\s*natsSubjectVoiceTierViolation,\s*s\.tierViolationQueueGroup\(\),\s*s\.admitTierViolation\s*,?\s*\)`,
		text, "the queue subscription must be the tier-violation door, in the resolved queue group")
	require.Len(t,
		regexp.MustCompile(`\.Subscribe(WithSubject)?\(\s*natsSubjectVoiceTierViolation`).FindAllString(text, -1),
		0, "the subject must never get a plain subscription")
}

// Production joins natsQueueVoiceTierViolation: the override exists for the live
// test alone, and an unset one must not leave the group empty, which would not
// be the group every replica shares.
func TestTierViolationQueueGroupDefaultsToTheProductionGroup(t *testing.T) {
	s := &NATSSubscriber{}
	assert.Equal(t, natsQueueVoiceTierViolation, s.tierViolationQueueGroup())

	s.tierViolationQueue = "test.tier-violation.override"
	assert.Equal(t, "test.tier-violation.override", s.tierViolationQueueGroup())
}
