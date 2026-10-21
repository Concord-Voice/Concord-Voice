package entitlements

// The recorder's counter and window run on an in-process miniredis, not
// redistest, for the frozen clock: a one-hour window is crossed with
// FastForward in microseconds ([internal]rules/tests.md, "frozen clock and dead
// clients for counter tests").

import (
	"bytes"
	"context"
	"database/sql"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register the postgres driver for the closed-handle DB error case
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type pushedEntitlements struct {
	userID uuid.UUID
	dto    EntitlementDTO
}

type recordingPusher struct {
	mu     sync.Mutex
	pushes []pushedEntitlements
}

func (p *recordingPusher) BroadcastEntitlements(userID uuid.UUID, dto EntitlementDTO) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, pushedEntitlements{userID: userID, dto: dto})
}

func (p *recordingPusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pushes)
}

type recordingEmitter struct {
	mu     sync.Mutex
	events []securityevent.Event
}

func (e *recordingEmitter) Emit(_ context.Context, event securityevent.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *recordingEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.events)
}

type fixedTier string

func (f fixedTier) GetTierChecked(context.Context, string) (string, error) { return string(f), nil }

type recorderFixture struct {
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	rec     *ViolationRecorder
	pusher  *recordingPusher
	emitter *recordingEmitter
	logs    *bytes.Buffer
}

func newRecorderFixture(t *testing.T) *recorderFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	f := &recorderFixture{
		mr: mr, rdb: rdb,
		pusher:  &recordingPusher{},
		emitter: &recordingEmitter{},
		logs:    &bytes.Buffer{},
	}
	f.rec = NewViolationRecorder(rdb, logger.NewWithWriter(f.logs), fixedTier(TierFree), f.pusher, f.emitter)
	return f
}

// recordAndWait drives the PUBLIC entry point and waits for the background
// record, so every assertion below observes what production would.
func (f *recorderFixture) recordAndWait(userID uuid.UUID, reason ViolationReason) {
	f.rec.Record(context.Background(), userID, reason)
	f.rec.Wait()
}

func TestParseViolationReasonAdmitsOnlyTheClosedSet(t *testing.T) {
	for _, reason := range []ViolationReason{
		ViolationAttachmentOverTier, ViolationAudioPtimeBelowTier, ViolationAudioBitrateOverTier,
	} {
		got, ok := ParseViolationReason(string(reason))
		assert.True(t, ok, reason)
		assert.Equal(t, reason, got)
	}
	// The media plane's own refusal strings are NOT accepted: the fold onto
	// the counted reasons happens before publish, never here.
	for _, raw := range []string{
		"", "ptime_undeclared", "opus_bitrate_above_tier_ceiling",
		"ATTACHMENT_OVER_TIER", "attachment_over_tier ", "animated_profile_premium",
	} {
		got, ok := ParseViolationReason(raw)
		assert.False(t, ok, "%q must be refused", raw)
		assert.Empty(t, got)
	}
}

func TestViolationKeyIsCanonicalByType(t *testing.T) {
	id := uuid.MustParse("{A1B2C3D4-E5F6-4711-8899-AABBCCDDEEFF}")
	assert.Equal(t, "entviol:a1b2c3d4-e5f6-4711-8899-aabbccddeeff", ViolationKey(id))
	dashless := strings.ReplaceAll(id.String(), "-", "")
	assert.Equal(t, ViolationKey(id), ViolationKey(uuid.MustParse(dashless)),
		"two spellings of one user must share one window")
}

// The threshold fires on the 5th count and on no other: not at 4, and not
// again at 6. The assertions are on the push and the event, the two
// outermost effects, not on an internal "escalated" flag.
func TestViolationRecorderEscalatesExactlyOnceAtTheThreshold(t *testing.T) {
	f := newRecorderFixture(t)
	user := uuid.New()

	for i := 1; i < ViolationThreshold; i++ {
		f.recordAndWait(user, ViolationAttachmentOverTier)
	}
	assert.Zero(t, f.pusher.count(), "no push below the threshold")
	assert.Zero(t, f.emitter.count(), "no security event below the threshold")

	f.recordAndWait(user, ViolationAudioBitrateOverTier)
	require.Equal(t, 1, f.pusher.count(), "exactly one push at the threshold")
	require.Equal(t, 1, f.emitter.count(), "exactly one security event at the threshold")

	f.recordAndWait(user, ViolationAttachmentOverTier)
	f.recordAndWait(user, ViolationAttachmentOverTier)
	assert.Equal(t, 1, f.pusher.count(), "no second push in the same window")
	assert.Equal(t, 1, f.emitter.count(), "no second event in the same window")

	push := f.pusher.pushes[0]
	assert.Equal(t, user, push.userID)
	assert.Equal(t, ToDTO(For(TierFree)), push.dto, "the resync carries the enforced set")

	assert.Equal(t, securityevent.Event{
		EventType:  securityevent.EventSecurityControl,
		Outcome:    securityevent.OutcomeDenied,
		Severity:   securityevent.SeverityMedium,
		ReasonCode: securityevent.ReasonAuthorizationDenied,
	}, f.emitter.events[0], "closed vocabulary only: no user id, no reason, no evidence ref")

	logs := f.logs.String()
	assert.Equal(t, ViolationThreshold+2, strings.Count(logs, "msg=entitlement.violation "),
		"one entitlement.violation line per count")
	assert.Equal(t, 1, strings.Count(logs, "msg=entitlement.misbehavior_threshold"))
	assert.Contains(t, logs, "count=7")
	assert.Contains(t, logs, "user_id="+user.String())
	assert.Contains(t, logs, "level=WARN")
}

// Another user's counts never reach this user's window.
func TestViolationRecorderCountsPerUser(t *testing.T) {
	f := newRecorderFixture(t)
	a, b := uuid.New(), uuid.New()
	for i := 1; i < ViolationThreshold; i++ {
		f.recordAndWait(a, ViolationAttachmentOverTier)
		f.recordAndWait(b, ViolationAttachmentOverTier)
	}
	assert.Zero(t, f.pusher.count())
	f.recordAndWait(b, ViolationAttachmentOverTier)
	require.Equal(t, 1, f.pusher.count())
	assert.Equal(t, b, f.pusher.pushes[0].userID)
}

// The window is FIXED at the first count (EXPIRE NX), expires after an hour,
// and the next window escalates once again.
func TestViolationRecorderWindowIsFixedAndExpires(t *testing.T) {
	f := newRecorderFixture(t)
	user := uuid.New()
	key := ViolationKey(user)

	f.recordAndWait(user, ViolationAttachmentOverTier)
	require.Equal(t, ViolationWindow, f.mr.TTL(key))

	// Late in the window a further count must NOT slide it.
	f.mr.FastForward(59*time.Minute + 30*time.Second)
	f.recordAndWait(user, ViolationAttachmentOverTier)
	assert.Equal(t, 30*time.Second, f.mr.TTL(key), "a later count never extends the window")
	got, err := f.mr.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "2", got)

	// Past the window the counter is gone and counting restarts at 1.
	f.mr.FastForward(31 * time.Second)
	assert.False(t, f.mr.Exists(key), "the window expired")

	for i := 1; i <= ViolationThreshold; i++ {
		f.recordAndWait(user, ViolationAudioPtimeBelowTier)
	}
	assert.Equal(t, 1, f.pusher.count(), "the new window escalates once")
	assert.Equal(t, ViolationWindow, f.mr.TTL(key), "the new window is a full hour")
}

// A key that somehow lost its TTL is repaired by its next count rather than
// counting forever.
func TestViolationRecorderRepairsAKeyWithNoTTL(t *testing.T) {
	f := newRecorderFixture(t)
	user := uuid.New()
	require.NoError(t, f.mr.Set(ViolationKey(user), "2"))
	require.Zero(t, f.mr.TTL(ViolationKey(user)))

	f.recordAndWait(user, ViolationAttachmentOverTier)
	assert.Equal(t, ViolationWindow, f.mr.TTL(ViolationKey(user)))
}

func assertUnrecorded(t *testing.T, f *recorderFixture, failureClass string) {
	t.Helper()
	assert.Zero(t, f.pusher.count())
	assert.Zero(t, f.emitter.count())
	logs := f.logs.String()
	assert.Contains(t, logs, "msg=entitlement.violation_unrecorded")
	assert.Contains(t, logs, "failure_class="+failureClass)
	assert.NotContains(t, logs, "msg=entitlement.violation ", "no count was logged")
}

// Redis down, shape 1: no client at all.
func TestViolationRecorderNilRedisIsUnrecordedNotFatal(t *testing.T) {
	logs := &bytes.Buffer{}
	f := &recorderFixture{pusher: &recordingPusher{}, emitter: &recordingEmitter{}, logs: logs}
	f.rec = NewViolationRecorder(nil, logger.NewWithWriter(logs), fixedTier(TierFree), f.pusher, f.emitter)

	for i := 0; i < ViolationThreshold; i++ {
		f.recordAndWait(uuid.New(), ViolationAttachmentOverTier)
	}
	assertUnrecorded(t, f, "violation_counter_unavailable")
}

// Redis down, shape 2: the server went away under a live client.
func TestViolationRecorderClosedRedisIsUnrecordedNotFatal(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.redis = redis.NewClient(&redis.Options{Addr: f.mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = f.rec.redis.Close() })
	f.mr.Close()

	f.recordAndWait(uuid.New(), ViolationAttachmentOverTier)
	assertUnrecorded(t, f, "violation_counter_unavailable")
}

// Redis down, shape 3: a server that accepts and never answers. This is the
// shape that would SLOW a synchronous caller, so it is the one that proves
// Record does not wait on Redis at all.
func TestViolationRecorderNeverBlocksTheCallerOnRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	// Accept every connection and never answer; the test closes them all.
	var connMu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			connMu.Lock()
			conns = append(conns, conn)
			connMu.Unlock()
		}
	}()
	t.Cleanup(func() {
		connMu.Lock()
		defer connMu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	f := newRecorderFixture(t)
	f.rec.redis = redis.NewClient(&redis.Options{
		Addr: ln.Addr().String(), MaxRetries: -1, DialerRetries: 1,
		ReadTimeout: 300 * time.Millisecond, ContextTimeoutEnabled: true,
	})
	t.Cleanup(func() { _ = f.rec.redis.Close() })

	start := time.Now()
	f.rec.Record(context.Background(), uuid.New(), ViolationAttachmentOverTier)
	returnedAfter := time.Since(start)
	assert.Less(t, returnedAfter, 100*time.Millisecond,
		"Record must return before the hung Redis answers")

	f.rec.Wait()
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond,
		"the background record really was held by the hung server")
	assertUnrecorded(t, f, "violation_counter_unavailable")
}

// A canceled request context (the refusal's response is already written)
// must not abort the count.
func TestViolationRecorderSurvivesACanceledRequestContext(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	user := uuid.New()
	f.rec.Record(ctx, user, ViolationAttachmentOverTier)
	f.rec.Wait()
	got, err := f.mr.Get(ViolationKey(user))
	require.NoError(t, err)
	assert.Equal(t, "1", got)
}

func TestViolationRecorderDropsWhenEverySlotIsTaken(t *testing.T) {
	f := newRecorderFixture(t)
	for i := 0; i < violationRecordSlots; i++ {
		f.rec.slots <- struct{}{}
	}
	user := uuid.New()
	f.recordAndWait(user, ViolationAttachmentOverTier)
	assert.False(t, f.mr.Exists(ViolationKey(user)), "a saturated record is dropped, not queued")
	assertUnrecorded(t, f, "violation_record_saturated")
	for i := 0; i < violationRecordSlots; i++ {
		<-f.rec.slots
	}
}

func TestViolationRecorderRefusesAnOutOfSetReason(t *testing.T) {
	f := newRecorderFixture(t)
	user := uuid.New()
	f.recordAndWait(user, ViolationReason("animated_profile_premium"))
	assert.False(t, f.mr.Exists(ViolationKey(user)))
	assertUnrecorded(t, f, "invalid_reason")
	assert.NotContains(t, f.logs.String(), "animated_profile_premium", "the refused value is never logged")
}

func TestViolationRecorderNilReceiverIsANoOp(t *testing.T) {
	var rec *ViolationRecorder
	assert.NotPanics(t, func() {
		rec.Record(context.Background(), uuid.New(), ViolationAttachmentOverTier)
		rec.Wait()
	})
}

// With no pusher the threshold still flags: the event is emitted.
func TestViolationRecorderEscalatesWithoutAPusher(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.pusher = nil
	user := uuid.New()
	for i := 0; i < ViolationThreshold; i++ {
		f.recordAndWait(user, ViolationAttachmentOverTier)
	}
	assert.Equal(t, 1, f.emitter.count())
	assert.Contains(t, f.logs.String(), "msg=entitlement.misbehavior_threshold")
}

func TestNewViolationRecorderDefaultsItsOptionalDependencies(t *testing.T) {
	rec := NewViolationRecorder(nil, nil, nil, nil, nil)
	require.NotNil(t, rec.log)
	assert.Equal(t, securityevent.Discard, rec.events)
	assert.Equal(t, violationRecordSlots, cap(rec.slots))
	assert.NotPanics(t, func() {
		rec.Record(context.Background(), uuid.New(), ViolationAttachmentOverTier)
		rec.Wait()
	})
}

// Every record must hand its slot back. A leak is invisible until the
// violationRecordSlots-th record, after which EVERY count is dropped as
// violation_record_saturated while each earlier record looked fine. Driving
// twice the slot count sequentially, each followed by Wait, is what exposes it:
// a record that never releases exhausts the pool at record 65.
func TestViolationRecorderReleasesItsSlotAfterEveryRecord(t *testing.T) {
	f := newRecorderFixture(t)
	user := uuid.New()
	total := 2 * violationRecordSlots

	for i := 0; i < total; i++ {
		f.recordAndWait(user, ViolationAttachmentOverTier)
	}

	got, err := f.mr.Get(ViolationKey(user))
	require.NoError(t, err)
	assert.Equal(t, "128", got, "every record was counted, none dropped for want of a slot")
	assert.NotContains(t, f.logs.String(), "violation_record_saturated")
	assert.Zero(t, len(f.rec.slots), "every slot was returned")
}

// The flag still lands when the recorder has a pusher but no tier source: the
// event is emitted and the push is skipped, without dereferencing the nil
// resolver. A guard that required BOTH to be missing would call GetTier on it.
func TestViolationRecorderWithAPusherButNoTierSourceFlagsAndDoesNotPush(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.tiers = nil
	user := uuid.New()

	require.NotPanics(t, func() {
		for i := 0; i < ViolationThreshold; i++ {
			f.recordAndWait(user, ViolationAttachmentOverTier)
		}
	})

	assert.Equal(t, 1, f.emitter.count(), "the event is not gated on the tier source")
	assert.Zero(t, f.pusher.count(), "nothing to push without a tier source")
}

// deadlineHoldingTier models a tier read that outlives its deadline: it answers
// only when its context ends, and then reports the context's error. A failed
// read is not a tier, and must never be pushed.
type deadlineHoldingTier struct {
	mu      sync.Mutex
	calls   int
	ctxErrs []error
}

func (d *deadlineHoldingTier) GetTierChecked(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.ctxErrs = append(d.ctxErrs, ctx.Err())
	return "", ctx.Err()
}

// A degraded resolve is flagged and logged, never pushed. The positive control
// is TestViolationRecorderEscalatesExactlyOnceAtTheThreshold: the same five
// counts with a resolver that answers promptly DO push. Waiting inside a
// goroutine with a hard limit is also the bound proof -- a record that held no
// deadline of its own would never finish against this resolver.
func TestViolationRecorderDoesNotPushADegradedTierResolve(t *testing.T) {
	f := newRecorderFixture(t)
	tier := &deadlineHoldingTier{}
	f.rec.tiers = tier
	user := uuid.New()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < ViolationThreshold; i++ {
			f.recordAndWait(user, ViolationAttachmentOverTier)
		}
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the record did not finish: the tier resolve is not bounded")
	}

	tier.mu.Lock()
	defer tier.mu.Unlock()
	require.Equal(t, 1, tier.calls, "the resolver was reached, so the test is not vacuous")
	assert.ErrorIs(t, tier.ctxErrs[0], context.DeadlineExceeded, "the resolve ended on its own deadline")

	assert.Equal(t, 1, f.emitter.count(), "the security event still fires")
	assert.Zero(t, f.pusher.count(), "a degraded answer is never pushed")
	logs := f.logs.String()
	assert.Equal(t, 1, strings.Count(logs, "msg=entitlement.misbehavior_threshold"),
		"the WARN line still fires")
	assert.Contains(t, logs, "msg=entitlement.violation_unrecorded")
	assert.Contains(t, logs, "failure_class=violation_resync_unresolved")
}

// The real failure path, end to end: the production Cache over a database
// handle whose every query fails, with an empty cache. The error comes out of
// ResolveTierChecked itself, so this is the test that notices the resolver
// swallowing a DB error into TierFree -- a stub that returns an error proves
// only that the recorder handles one.
func TestViolationRecorderDoesNotPushWhenTheRealTierReadFails(t *testing.T) {
	f := newRecorderFixture(t)
	db, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, db.Close(), "a closed handle fails every query")
	f.rec.tiers = NewCache(f.rdb, db)
	user := uuid.New()

	for i := 0; i < ViolationThreshold; i++ {
		f.recordAndWait(user, ViolationAttachmentOverTier)
	}

	assert.Equal(t, 1, f.emitter.count(), "the security event still fires")
	assert.Zero(t, f.pusher.count(), "a failed tier read is never pushed")
	logs := f.logs.String()
	assert.Equal(t, 1, strings.Count(logs, "msg=entitlement.misbehavior_threshold"))
	assert.Contains(t, logs, "failure_class=violation_resync_unresolved")
	assert.False(t, f.mr.Exists("ent:"+user.String()), "a failed read must not seed the cache")

	// Control: the same cache and the same dead database DO push when the tier
	// is already cached, so the silence above is the failed read, not the setup.
	cached := uuid.New()
	require.NoError(t, f.rec.tiers.(*Cache).SetTier(context.Background(), cached.String(), TierPremium))
	for i := 0; i < ViolationThreshold; i++ {
		f.recordAndWait(cached, ViolationAttachmentOverTier)
	}
	require.Equal(t, 1, f.pusher.count(), "a cached tier is pushed")
	assert.Equal(t, cached, f.pusher.pushes[0].userID)
	assert.Equal(t, ToDTO(For(TierPremium)), f.pusher.pushes[0].dto)
}
