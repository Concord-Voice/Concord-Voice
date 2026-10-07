package dm

// The delete-scope step-up grace on the DM message delete (#3454 D-2, A-9,
// A-10). A DM delete has only the own rule, so an mfa grace covers an MFA
// account and a password grace covers an account with no inline factor, until
// it enrols one. Every test names the mutant it kills.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/permgen"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

var errDMGraceRedisInjected = errors.New("injected MGET failure")

// dmGraceRedisSpy records every Redis command, pipelined or not, and can fail
// MGET, the grace read's one command.
type dmGraceRedisSpy struct {
	mu       sync.Mutex
	names    []string
	failMGet bool
}

func (s *dmGraceRedisSpy) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
}

func (s *dmGraceRedisSpy) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

func (s *dmGraceRedisSpy) setFailMGet(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMGet = on
}

func (s *dmGraceRedisSpy) DialHook(next redis.DialHook) redis.DialHook { return next }

func (s *dmGraceRedisSpy) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		s.record(cmd.Name())
		s.mu.Lock()
		fail := s.failMGet && cmd.Name() == "mget"
		s.mu.Unlock()
		if fail {
			cmd.SetErr(errDMGraceRedisInjected)
			return errDMGraceRedisInjected
		}
		return next(ctx, cmd)
	}
}

func (s *dmGraceRedisSpy) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			s.record(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

// newDMGraceHarness is the soft-lock harness with a session claim, the user
// generation a DM grace is stamped with, and a spy on its Redis.
func newDMGraceHarness(t *testing.T) (*softLockHarness, *dmGraceRedisSpy) {
	t.Helper()
	hs := newSoftLockHarness(t)
	hs.sid = uuid.NewString()
	require.NoError(t, hs.mr.Set(permgen.UserKey(hs.actor), "gen-user-1"))
	spy := &dmGraceRedisSpy{}
	hs.rdb.AddHook(spy)
	return hs, spy
}

func (hs *softLockHarness) graceKeys() []string {
	var keys []string
	for _, k := range hs.mr.Keys() {
		if strings.HasPrefix(k, "stepup:grace:") {
			keys = append(keys, k)
		}
	}
	return keys
}

func (hs *softLockHarness) onlyGrace(t *testing.T) (string, map[string]string) {
	t.Helper()
	keys := hs.graceKeys()
	require.Len(t, keys, 1, "exactly one grace")
	raw, err := hs.mr.Get(keys[0])
	require.NoError(t, err)
	var stamp map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &stamp))
	return keys[0], stamp
}

// grantByCode confirms one over-threshold delete with a valid code, which
// grants an mfa grace in hs.sid.
func (hs *softLockHarness) grantByCode(t *testing.T) {
	t.Helper()
	hs.overThreshold(t)
	w := hs.delete(t, hs.message(t), codeBody(softLockGoodCode))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestDMDeleteSoftLockGrace_GrantedOnlyAfterCommit: a verified, committed
// delete grants an mfa grace for GraceTTL in the session's DM scope; a
// verified delete whose transaction then rolls back grants nothing. Kills
// dropping the grant, and settling a delete whose transaction failed.
func TestDMDeleteSoftLockGrace_GrantedOnlyAfterCommit(t *testing.T) {
	hs, _ := newDMGraceHarness(t)
	hs.enrollMFA(t)

	gone := hs.message(t)
	hs.verifier.onVerify = func() {
		_, err := hs.db.Exec(`DELETE FROM dm_messages WHERE id = $1`, gone)
		require.NoError(t, err)
	}
	hs.overThreshold(t)
	w := hs.delete(t, gone, codeBody(softLockGoodCode))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Len(t, hs.verifier.purposes, 1, "the code verified inside the transaction")
	assert.Empty(t, hs.graceKeys(), "which rolled back, so no grace")

	hs.verifier.onVerify = nil
	hs.grantByCode(t)
	key, stamp := hs.onlyGrace(t)
	assert.Equal(t, "stepup:grace:"+hs.actor+":"+hs.sid+":dm:delete", key)
	assert.Equal(t, string(stepup.GraceStrengthMFA), stamp["strength"])
	assert.Equal(t, stepup.GraceTTL, hs.mr.TTL(key))
}

// TestDMDeleteSoftLockGrace_CoveredTripNeitherPromptsNorSlides: a trip inside
// the grace deletes with no factor, no verifier call and no budget write,
// resets both tiers, and leaves the grace's value and TTL untouched. Kills
// dropping the grace check (prompted), granting on a grace-covered outcome
// (the TTL restarts), clearing the budget on one, and not resetting on one.
func TestDMDeleteSoftLockGrace_CoveredTripNeitherPromptsNorSlides(t *testing.T) {
	hs, _ := newDMGraceHarness(t)
	hs.enrollMFA(t)
	hs.grantByCode(t)
	key, before := hs.onlyGrace(t)
	hs.mr.FastForward(4 * time.Minute)
	hs.overThreshold(t)
	require.NoError(t, hs.mr.Set(hs.budgetKey(), "2"))

	messageID := hs.message(t)
	w := hs.delete(t, messageID, "")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, hs.messageExists(t, messageID))
	assert.Len(t, hs.verifier.purposes, 1, "only the granting delete reached the verifier")
	assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a grace-covered delete resets the burst tier")
	assertNoKey(t, hs.mr, hs.dayKey(hs.actor), "and the day tier")
	assert.Equal(t, "2", mustRedisGet(t, hs.mr, hs.budgetKey()), "but neither charges nor clears the budget")
	_, after := hs.onlyGrace(t)
	assert.Equal(t, before, after)
	assert.Equal(t, stepup.GraceTTL-4*time.Minute, hs.mr.TTL(key), "the window did not slide")
}

// TestDMDeleteSoftLockGrace_PasswordGraceLapsesOnMFAEnrolment: a spent
// password token grants a password grace, which covers the next trip and
// stops covering once the account has an inline factor. Kills granting mfa
// for a password confirmation, and judging the grace under the server rule
// (the positive control is then prompted).
func TestDMDeleteSoftLockGrace_PasswordGraceLapsesOnMFAEnrolment(t *testing.T) {
	hs, _ := newDMGraceHarness(t)
	hs.setPassword(t)
	hs.overThreshold(t)
	w := hs.delete(t, hs.message(t), tokenBody(hs.mintToken(t, stepup.PurposeDMMessageDelete)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, stamp := hs.onlyGrace(t)
	require.Equal(t, string(stepup.GraceStrengthPassword), stamp["strength"])

	hs.overThreshold(t)
	covered := hs.delete(t, hs.message(t), "")
	require.Equal(t, http.StatusOK, covered.Code, "a password grace covers the own rule: %s", covered.Body.String())

	hs.enrollMFA(t)
	hs.overThreshold(t)
	refused := hs.message(t)
	body := assertSoftLockRefusal(t, hs.delete(t, refused, ""), "MFA verification required", softLockRetryAfter30)
	assert.Equal(t, true, body["mfa_required"])
	assert.True(t, hs.messageExists(t, refused))
}

// TestDMDeleteSoftLockGrace_VoidedOrUnreadablePrompts: a bumped user
// generation, a different epoch in the stamp, another session, a failed
// grace read and a malformed value each read as no grace, so the trip is
// prompted. Kills judging the grace with anything weaker than Covers, and
// keying it on anything but the session.
func TestDMDeleteSoftLockGrace_VoidedOrUnreadablePrompts(t *testing.T) {
	rewrite := func(hs *softLockHarness, t *testing.T, field, value string) {
		key, stamp := hs.onlyGrace(t)
		stamp[field] = value
		raw, err := json.Marshal(stamp)
		require.NoError(t, err)
		require.NoError(t, hs.mr.Set(key, string(raw)))
	}
	for _, tc := range []struct {
		name  string
		after func(hs *softLockHarness, spy *dmGraceRedisSpy, t *testing.T)
	}{
		{name: "user generation bumped", after: func(hs *softLockHarness, _ *dmGraceRedisSpy, t *testing.T) {
			require.NoError(t, hs.mr.Set(permgen.UserKey(hs.actor), "gen-user-2"))
		}},
		{name: "credential epoch differs", after: func(hs *softLockHarness, _ *dmGraceRedisSpy, t *testing.T) {
			rewrite(hs, t, "cred_epoch", "epoch-other")
		}},
		{name: "another session", after: func(hs *softLockHarness, _ *dmGraceRedisSpy, _ *testing.T) {
			hs.sid = uuid.NewString()
		}},
		{name: "MGET fails", after: func(_ *softLockHarness, spy *dmGraceRedisSpy, _ *testing.T) {
			spy.setFailMGet(true)
		}},
		{name: "malformed value", after: func(hs *softLockHarness, _ *dmGraceRedisSpy, t *testing.T) {
			key, _ := hs.onlyGrace(t)
			require.NoError(t, hs.mr.Set(key, "{not json"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs, spy := newDMGraceHarness(t)
			hs.enrollMFA(t)
			hs.grantByCode(t)
			tc.after(hs, spy, t)
			hs.overThreshold(t)

			messageID := hs.message(t)
			body := assertSoftLockRefusal(t, hs.delete(t, messageID, ""), "MFA verification required", softLockRetryAfter30)
			assert.Equal(t, true, body["mfa_required"])
			assert.True(t, hs.messageExists(t, messageID))
		})
	}
}

// TestDMDeleteSoftLockGrace_NoNewRedisCall: the grace read runs only over the
// threshold. An author whose own rule is off still makes no Redis call, and
// one under the threshold makes no MGET. Kills reading the grace before the
// own-rule check or before the threshold verdict.
func TestDMDeleteSoftLockGrace_NoNewRedisCall(t *testing.T) {
	hs, spy := newDMGraceHarness(t)

	w := hs.delete(t, hs.message(t), "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, spy.commands(), "the population member was counted")
	assert.NotContains(t, spy.commands(), "mget", "under the threshold, no grace read")

	hs.setOwnRule(t, false)
	before := len(spy.commands())
	w = hs.delete(t, hs.message(t), "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, spy.commands(), before, "outside the own rule, no Redis call")
}
