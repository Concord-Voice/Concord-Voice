package messages_test

// The delete-scope step-up grace on the channel soft-lock routes (#3454 D-2,
// A-9, A-10): a committed, verified confirmation grants a 10-minute grace for
// the session and scope; a later trip inside it resets the counters and
// proceeds with no prompt, no verifier call and no budget write; a
// grace-covered delete never re-grants. Every test names the mutant it kills.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/permgen"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
)

var errGraceRedisInjected = errors.New("injected MGET failure")

// graceRedisSpy records every Redis command the handler issues, pipelined or
// not, and can fail MGET, the grace read's one command.
type graceRedisSpy struct {
	mu       sync.Mutex
	names    []string
	failMGet bool
}

func (s *graceRedisSpy) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
}

func (s *graceRedisSpy) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = nil
}

func (s *graceRedisSpy) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

func (s *graceRedisSpy) setFailMGet(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMGet = on
}

func (s *graceRedisSpy) DialHook(next redis.DialHook) redis.DialHook { return next }

func (s *graceRedisSpy) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		s.record(cmd.Name())
		s.mu.Lock()
		fail := s.failMGet && cmd.Name() == "mget"
		s.mu.Unlock()
		if fail {
			cmd.SetErr(errGraceRedisInjected)
			return errGraceRedisInjected
		}
		return next(ctx, cmd)
	}
}

func (s *graceRedisSpy) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			s.record(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

// graceFixture is a soft-lock harness whose Redis is spied on, one world, an
// author in it, and the author's session.
type graceFixture struct {
	*softLockHarness
	spy *graceRedisSpy
	w   softLockWorld
	sid string
}

// newGraceFixture builds the world and seeds the generations a grace is
// stamped with into the handler's Redis. In production the soft-lock and the
// permission cache share one Redis; here the cache is on the test Redis, so
// the generations are seeded where the grace store reads them.
func newGraceFixture(t *testing.T, enforcing bool) *graceFixture {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	mr := miniredis.RunT(t)
	rdb := fastRedisClient(t, mr.Addr())
	spy := &graceRedisSpy{}
	rdb.AddHook(spy)
	f := &graceFixture{softLockHarness: buildSoftLockHarness(t, ts, ts.DB, rdb, mr), spy: spy, sid: uuid.NewString()}
	f.w = f.world(t, enforcing)
	f.seedGenerations(t, f.w.author.ID, f.w.serverID)
	return f
}

func (s *softLockHarness) seedGenerations(t *testing.T, userID, serverID string) {
	t.Helper()
	require.NoError(t, s.mr.Set(permgen.UserKey(userID), "gen-user-1"))
	require.NoError(t, s.mr.Set(permgen.ServerKey(serverID), "gen-server-1"))
}

// trip primes the burst counter so the next delete on serverID is the 16th.
func (f *graceFixture) trip(t *testing.T, userID, serverID string) {
	t.Helper()
	require.NoError(t, f.mr.Set(burstKey(userID, serverID), "15"))
}

// del deletes messageID as the author, in session sid.
func (f *graceFixture) del(t *testing.T, sid, messageID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, request{method: http.MethodDelete, path: "/messages/" + messageID, userID: f.w.author.ID, session: sid, body: body})
}

// graceKeys returns every grace key userID holds.
func (s *softLockHarness) graceKeys(userID string) []string {
	var keys []string
	for _, k := range s.mr.Keys() {
		if strings.HasPrefix(k, "stepup:grace:"+userID+":") {
			keys = append(keys, k)
		}
	}
	return keys
}

// onlyGrace returns the author's one grace key and its decoded stamp.
func (f *graceFixture) onlyGrace(t *testing.T) (string, map[string]string) {
	t.Helper()
	keys := f.graceKeys(f.w.author.ID)
	require.Len(t, keys, 1, "exactly one grace")
	raw, err := f.mr.Get(keys[0])
	require.NoError(t, err)
	var stamp map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &stamp))
	return keys[0], stamp
}

// rewriteStamp replaces one field of the author's grace stamp.
func (f *graceFixture) rewriteStamp(t *testing.T, field, value string) {
	t.Helper()
	key, stamp := f.onlyGrace(t)
	stamp[field] = value
	raw, err := json.Marshal(stamp)
	require.NoError(t, err)
	require.NoError(t, f.mr.Set(key, string(raw)))
}

// grantByCode trips the soft-lock and confirms one delete with a valid code,
// which grants an mfa grace in session sid.
func (f *graceFixture) grantByCode(t *testing.T, sid, messageID string) {
	t.Helper()
	f.trip(t, f.w.author.ID, f.w.serverID)
	res := f.del(t, sid, messageID, map[string]string{"mfa_code": softLockValidCode})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
}

// TestSoftLockGrace_VerifiedDeleteGrantsAfterCommit: a verified, committed
// delete grants an mfa grace for exactly GraceTTL in the session's delete
// scope. Kills dropping the grant from settleSoftLock, and granting the wrong
// strength on the server rule.
func TestSoftLockGrace_VerifiedDeleteGrantsAfterCommit(t *testing.T) {
	f := newGraceFixture(t, true)
	f.enroll(t, f.w.author.ID)
	ids := f.seed(t, f.w.channelID, f.w.author, 1)

	f.grantByCode(t, f.sid, ids[0])

	key, stamp := f.onlyGrace(t)
	assert.Equal(t, "stepup:grace:"+f.w.author.ID+":"+f.sid+":server:"+f.w.serverID+":delete", key)
	assert.Equal(t, string(stepup.GraceStrengthMFA), stamp["strength"])
	assert.Equal(t, stepup.GraceTTL, f.mr.TTL(key))
}

// TestSoftLockGrace_RolledBackDeleteGrantsNothing: the confirmation verifies,
// then the delete's own recheck finds the message gone and the transaction
// rolls back, so the factor is restored and no grace is granted. Kills
// settling before the response-written return, i.e. granting on a rolled-back
// action.
func TestSoftLockGrace_RolledBackDeleteGrantsNothing(t *testing.T) {
	f := newGraceFixture(t, true)
	f.enroll(t, f.w.author.ID)
	ids := f.seed(t, f.w.channelID, f.w.author, 1)
	f.trip(t, f.w.author.ID, f.w.serverID)
	f.handler.SetBeforeSoftLockConfirmHookForTest(func() {
		_, err := f.ts.DB.Exec(`DELETE FROM messages WHERE id = $1`, ids[0])
		require.NoError(t, err)
	})

	res := f.del(t, f.sid, ids[0], map[string]string{"mfa_code": softLockValidCode})

	require.Equal(t, http.StatusNotFound, res.Code, res.Body.String())
	assert.Equal(t, []stepup.Purpose{stepup.PurposeMessageDelete}, f.verifier.calls(), "the code verified in the transaction")
	assert.False(t, f.totpStepSpent(t, f.w.author.ID), "which rolled back")
	assert.Empty(t, f.graceKeys(f.w.author.ID), "so no grace was granted")
}

// TestSoftLockGrace_CoveredTripResetsWithoutPromptOrBudget: a trip inside a
// valid grace proceeds with no factor, no verifier call and no budget write,
// and resets both counters. Kills dropping the grace check from
// confirmServerRuleTx (prompted), not resetting on a grace-covered outcome
// (the burst key stays), and clearing the budget on one (it goes).
func TestSoftLockGrace_CoveredTripResetsWithoutPromptOrBudget(t *testing.T) {
	f := newGraceFixture(t, true)
	f.enroll(t, f.w.author.ID)
	ids := f.seed(t, f.w.channelID, f.w.author, 2)
	f.grantByCode(t, f.sid, ids[0])
	f.trip(t, f.w.author.ID, f.w.serverID)
	require.NoError(t, f.mr.Set(budgetKey(f.w.author.ID), "2"))

	res := f.del(t, f.sid, ids[1], nil)

	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.False(t, f.messageExists(t, ids[1]))
	assert.Len(t, f.verifier.calls(), 1, "only the granting delete reached the verifier")
	assert.Empty(t, f.counter(burstKey(f.w.author.ID, f.w.serverID)), "a grace-covered delete resets the burst tier")
	assert.Empty(t, f.counter(dayKey(f.w.author.ID)), "and the day tier")
	assert.Equal(t, "2", f.counter(budgetKey(f.w.author.ID)), "but neither charges nor clears the budget")
}

// TestSoftLockGrace_DoesNotSlide: a grace-covered delete leaves the grace's
// value and remaining TTL exactly as they were. Kills granting on a
// grace-covered outcome.
func TestSoftLockGrace_DoesNotSlide(t *testing.T) {
	f := newGraceFixture(t, true)
	f.enroll(t, f.w.author.ID)
	ids := f.seed(t, f.w.channelID, f.w.author, 2)
	f.grantByCode(t, f.sid, ids[0])
	key, before := f.onlyGrace(t)
	f.mr.FastForward(4 * time.Minute)
	f.trip(t, f.w.author.ID, f.w.serverID)

	res := f.del(t, f.sid, ids[1], nil)

	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	_, after := f.onlyGrace(t)
	assert.Equal(t, before, after)
	assert.Equal(t, stepup.GraceTTL-4*time.Minute, f.mr.TTL(key), "the window did not slide")
}

// TestSoftLockGrace_StampMismatchVoids: each stamp the grace carries must
// still match, or the trip is prompted. The generations are bumped as a
// writer bumps them; the epoch is rewritten in the stamp. Kills judging the
// grace with anything weaker than Covers (a key-exists check).
func TestSoftLockGrace_StampMismatchVoids(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bump  func(f *graceFixture, t *testing.T)
		field string
	}{
		{name: "user generation bumped", bump: func(f *graceFixture, t *testing.T) {
			require.NoError(t, f.mr.Set(permgen.UserKey(f.w.author.ID), "gen-user-2"))
		}},
		{name: "server generation bumped", bump: func(f *graceFixture, t *testing.T) {
			require.NoError(t, f.mr.Set(permgen.ServerKey(f.w.serverID), "gen-server-2"))
		}},
		{name: "credential epoch differs", bump: func(f *graceFixture, t *testing.T) {
			f.rewriteStamp(t, "cred_epoch", "epoch-other")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraceFixture(t, true)
			f.enroll(t, f.w.author.ID)
			ids := f.seed(t, f.w.channelID, f.w.author, 2)
			f.grantByCode(t, f.sid, ids[0])
			tc.bump(f, t)
			f.trip(t, f.w.author.ID, f.w.serverID)

			body := requireSoftLockRefusal(t, f.del(t, f.sid, ids[1], nil), "mfa_required")
			assert.Equal(t, "MFA verification required", body["error"])
			assert.True(t, f.messageExists(t, ids[1]))
		})
	}
}

// TestSoftLockGrace_PasswordGraceNeverSatisfiesTheServerRule: a password
// grace earned under the own rule, while the server did not enforce, stops
// covering once it does. The account has no inline factor, so the own rule
// would still accept that grace; only the server rule refuses it. The
// generations are left as they were, so the strength rule alone decides.
// Kills judging the server arm under the own rule.
func TestSoftLockGrace_PasswordGraceNeverSatisfiesTheServerRule(t *testing.T) {
	f := newGraceFixture(t, false)
	ids := f.seed(t, f.w.channelID, f.w.author, 2)
	f.trip(t, f.w.author.ID, f.w.serverID)
	token := f.mintToken(t, f.w.author.ID, stepup.PurposeMessageDelete)
	res := f.del(t, f.sid, ids[0], map[string]string{"step_up_token": token})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	_, stamp := f.onlyGrace(t)
	require.Equal(t, string(stepup.GraceStrengthPassword), stamp["strength"])

	f.setEnforcing(t, f.w.serverID, true)
	f.trip(t, f.w.author.ID, f.w.serverID)
	requireSoftLockRefusal(t, f.del(t, f.sid, ids[1], nil), "mfa_enrollment_required")
	assert.True(t, f.messageExists(t, ids[1]))
}

// TestSoftLockGrace_PasswordGraceLapsesOnMFAEnrolment: under the own rule a
// spent password token grants a password grace, which covers the next trip
// (the positive control) and stops covering once the account has an inline
// factor. Kills granting mfa for a password confirmation.
func TestSoftLockGrace_PasswordGraceLapsesOnMFAEnrolment(t *testing.T) {
	f := newGraceFixture(t, false)
	ids := f.seed(t, f.w.channelID, f.w.author, 3)
	f.trip(t, f.w.author.ID, f.w.serverID)
	token := f.mintToken(t, f.w.author.ID, stepup.PurposeMessageDelete)
	res := f.del(t, f.sid, ids[0], map[string]string{"step_up_token": token})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	_, stamp := f.onlyGrace(t)
	require.Equal(t, string(stepup.GraceStrengthPassword), stamp["strength"])

	f.trip(t, f.w.author.ID, f.w.serverID)
	covered := f.del(t, f.sid, ids[1], nil)
	require.Equal(t, http.StatusOK, covered.Code, "a password grace covers the own rule: %s", covered.Body.String())

	f.enroll(t, f.w.author.ID)
	f.trip(t, f.w.author.ID, f.w.serverID)
	requireSoftLockRefusal(t, f.del(t, f.sid, ids[2], nil), "mfa_required")
	assert.True(t, f.messageExists(t, ids[2]))
}

// TestSoftLockGrace_UnreadableGracePrompts: a Redis error on the grace read,
// or a malformed grace value, reads as no grace, so the trip is prompted
// rather than refused with a 5xx or waved through.
func TestSoftLockGrace_UnreadableGracePrompts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakRead func(f *graceFixture, t *testing.T)
	}{
		{name: "MGET fails", breakRead: func(f *graceFixture, _ *testing.T) {
			f.spy.setFailMGet(true)
		}},
		{name: "malformed value", breakRead: func(f *graceFixture, t *testing.T) {
			key, _ := f.onlyGrace(t)
			require.NoError(t, f.mr.Set(key, "{not json"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraceFixture(t, true)
			f.enroll(t, f.w.author.ID)
			ids := f.seed(t, f.w.channelID, f.w.author, 2)
			f.grantByCode(t, f.sid, ids[0])
			tc.breakRead(f, t)
			f.trip(t, f.w.author.ID, f.w.serverID)

			requireSoftLockRefusal(t, f.del(t, f.sid, ids[1], nil), "mfa_required")
			assert.True(t, f.messageExists(t, ids[1]))
		})
	}
}

// TestSoftLockGrace_OtherSessionOrScopeGetsNone: a grace belongs to one
// session and one soft-lock scope. Another session on the same server, and
// the same session on another server, are both prompted. Kills reading the
// grace under a scope that ignores the server (every server sharing one).
func TestSoftLockGrace_OtherSessionOrScopeGetsNone(t *testing.T) {
	f := newGraceFixture(t, true)
	f.enroll(t, f.w.author.ID)
	ids := f.seed(t, f.w.channelID, f.w.author, 2)
	f.grantByCode(t, f.sid, ids[0])

	f.trip(t, f.w.author.ID, f.w.serverID)
	requireSoftLockRefusal(t, f.del(t, uuid.NewString(), ids[1], nil), "mfa_required")
	assert.True(t, f.messageExists(t, ids[1]), "another session is prompted")

	other := f.world(t, true)
	f.ts.AddMemberToServer(t, other.serverID, f.w.author.ID, "member")
	f.seedGenerations(t, f.w.author.ID, other.serverID)
	otherMsg := f.seed(t, other.channelID, f.w.author, 1)[0]
	f.trip(t, f.w.author.ID, other.serverID)
	requireSoftLockRefusal(t, f.del(t, f.sid, otherMsg, nil), "mfa_required")
	assert.True(t, f.messageExists(t, otherMsg), "another server's scope is prompted")
}

// TestSoftLockGrace_NoNewRedisCall: the grace read runs only over the
// threshold. A member outside every rule still makes no Redis call at all,
// and one under the threshold makes no MGET. Kills reading the grace before
// the population check or before the threshold verdict.
func TestSoftLockGrace_NoNewRedisCall(t *testing.T) {
	f := newGraceFixture(t, false)
	msgs := f.seed(t, f.w.channelID, f.w.author, 2)

	f.spy.reset()
	res := f.do(t, request{method: http.MethodDelete, path: "/messages/" + msgs[0], userID: f.w.moderator.ID, session: f.sid})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Empty(t, f.spy.commands(), "outside every rule, no Redis call")

	f.spy.reset()
	res = f.del(t, f.sid, msgs[1], nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.NotEmpty(t, f.spy.commands(), "the population member was counted")
	assert.NotContains(t, f.spy.commands(), "mget", "under the threshold, no grace read")
}

// purgeOwn purges the author's own messages in the fixture's channel, in
// session sid.
func (f *graceFixture) purgeOwn(t *testing.T, sid string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, request{method: http.MethodDelete, path: "/channels/" + f.w.channelID + "/messages",
		userID: f.w.author.ID, session: sid, body: body})
}

// TestSoftLockGrace_SelfPurgeGrantsAndHonours: an over-threshold self-purge
// that verifies grants a grace once its admission commits, and the next
// over-threshold self-purge in that session is admitted by it with no factor
// and no verifier call. Kills dropping the grace read from the self-purge's
// charge path, and dropping the grant from settleSelfPurge.
func TestSoftLockGrace_SelfPurgeGrantsAndHonours(t *testing.T) {
	f := newGraceFixture(t, true)
	f.enroll(t, f.w.author.ID)
	f.seed(t, f.w.channelID, f.w.author, 1)
	f.trip(t, f.w.author.ID, f.w.serverID)

	res := f.purgeOwn(t, f.sid, map[string]any{"range": "all", "mfa_code": softLockValidCode})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	_, stamp := f.onlyGrace(t)
	assert.Equal(t, string(stepup.GraceStrengthMFA), stamp["strength"])

	f.seed(t, f.w.channelID, f.w.author, 1)
	f.trip(t, f.w.author.ID, f.w.serverID)
	res = f.purgeOwn(t, f.sid, map[string]any{"range": "all"})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.EqualValues(t, 1, decode(t, res)["deleted_count"])
	assert.Len(t, f.verifier.calls(), 1, "the second purge was admitted by the grace")
	assert.Empty(t, f.counter(burstKey(f.w.author.ID, f.w.serverID)), "and reset the counters")
}

// TestSoftLockGrace_UnknownAdmissionGrantsNothing: the admission verified and
// committed, but neither its acknowledgement nor the reconciling read came
// back, so the admission is unknown and nothing is settled: no grace either.
// Kills settling (and so granting) on ErrAdmissionUnknown.
func TestSoftLockGrace_UnknownAdmissionGrantsNothing(t *testing.T) {
	s, hook := newHookedSoftLockHarness(t)
	w := s.world(t, false)
	s.enroll(t, w.author.ID)
	s.seedGenerations(t, w.author.ID, w.serverID)
	s.seed(t, w.channelID, w.author, 16)
	hook.Arm([]string{"SELECT EXISTS (SELECT 1 FROM message_purges WHERE id"}, nil, stmthook.ErrInjected)
	s.handler.SetBeforeSoftLockConfirmHookForTest(func() { hook.ArmCommit(true, errors.New("connection lost after COMMIT was sent")) })

	res := s.do(t, request{method: http.MethodDelete, path: "/channels/" + w.channelID + "/messages",
		userID: w.author.ID, session: uuid.NewString(), body: map[string]any{"range": "all", "mfa_code": softLockValidCode}})

	require.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
	assert.True(t, s.totpStepSpent(t, w.author.ID), "the admission did commit")
	assert.Empty(t, s.graceKeys(w.author.ID), "an unknown admission grants no grace")
}
