package messages_test

// Handler-seam tests for the self-purge soft-lock (#3455 T5b, design spec
// "Developer decisions" D-1). Each test names the mutant it kills; see
// softlock_harness_test.go for the harness.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func (s *softLockHarness) purgeChannel(t *testing.T, userID, channelID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return s.do(t, request{method: http.MethodDelete, path: "/channels/" + channelID + "/messages", userID: userID, body: body})
}

func (s *softLockHarness) purgeServer(t *testing.T, userID, serverID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return s.do(t, request{method: http.MethodDelete, path: "/servers/" + serverID + "/messages", userID: userID, body: body})
}

func (s *softLockHarness) countBy(t *testing.T, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, s.ts.DB.QueryRow(`SELECT count(*) FROM messages WHERE user_id = $1`, userID).Scan(&n))
	return n
}

func (s *softLockHarness) auditRows(t *testing.T, contextID string) int {
	t.Helper()
	var n int
	require.NoError(t, s.ts.DB.QueryRow(`SELECT count(*) FROM message_purges WHERE context_id = $1`, contextID).Scan(&n))
	return n
}

// TestSelfPurge_ChannelOwnRule: a ManageOwn member (forced to self) purging 16
// of their own messages on a non-enforcing server trips the soft-lock and
// confirms with the password. The refusal purges nothing and writes no audit
// row; the wrong password is charged to the budget; the confirmed purge
// deletes only the author's messages, clears the budget and resets both
// tiers.
//
// Mutants killed: skipping the gate (the first purge succeeds); running the
// gate after purge.Run writes its audit (an audit row survives the refusal);
// not resetting after a confirmed purge (the counters survive).
func TestSelfPurge_ChannelOwnRule(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.seed(t, w.channelID, w.author, 16)
	s.seed(t, w.channelID, w.owner, 3)

	requireSoftLockRefusal(t, s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"}), "password_required")
	assert.Equal(t, 16, s.countBy(t, w.author.ID), "a refused self-purge deletes nothing")
	assert.Zero(t, s.auditRows(t, w.channelID), "a refused self-purge writes no audit row")

	// #3509: a token that matches nothing stands where a wrong password did.
	wrong := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all", "step_up_token": "never-minted-token-0000000000000000000"})
	assert.Equal(t, stepup.ErrMsgStepUpTokenInvalid, requireSoftLockRefusal(t, wrong, "step_up_token_invalid")["error"])
	assert.Equal(t, "1", s.counter(budgetKey(w.author.ID)))

	ok := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{
		"range": "all", "step_up_token": s.mintToken(t, w.author.ID, stepup.PurposeChannelPurge),
	})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.EqualValues(t, 16, decode(t, ok)["deleted_count"])
	assert.Zero(t, s.countBy(t, w.author.ID))
	assert.Equal(t, 3, s.countBy(t, w.owner.ID), "a self-purge reaches only the actor's messages")
	assert.Empty(t, s.counter(budgetKey(w.author.ID)))
	assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)))
	assert.Empty(t, s.counter(dayKey(w.author.ID)))
}

// TestSelfPurge_ServerRule: an enrolled owner purging their own messages
// across two channels of an enforcing server, by explicit self target, is
// counted as one bounded batch and confirms with MFA under the server purge's
// own purpose.
//
// Mutants killed: counting only the first self-authored spec (10 < 16, the
// first purge succeeds); passing the channel purge's purpose, or the delete
// purpose, to the verifier.
func TestSelfPurge_ServerRule(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.owner.ID)
	second := s.ts.CreateTestChannel(t, w.serverID, "second")
	s.seed(t, w.channelID, w.owner, 10)
	s.seed(t, second, w.owner, 10)
	self := map[string]any{"range": "all", "target_user_id": w.owner.ID}

	body := requireSoftLockRefusal(t, s.purgeServer(t, w.owner.ID, w.serverID, self), "mfa_required")
	assert.Equal(t, []any{"totp"}, body["methods"])
	assert.Equal(t, 20, s.countBy(t, w.owner.ID))

	self["mfa_code"] = softLockValidCode
	ok := s.purgeServer(t, w.owner.ID, w.serverID, self)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Zero(t, s.countBy(t, w.owner.ID))
	assert.Equal(t, []stepup.Purpose{stepup.PurposeServerPurge}, s.verifier.calls())
	assert.Empty(t, s.counter(burstKey(w.owner.ID, w.serverID)))
}

// TestSelfPurge_ChannelPurpose pins the channel purge's purpose.
//
// Mutant killed: passing PurposeServerPurge or PurposeMessageDelete from
// PurgeChannel.
func TestSelfPurge_ChannelPurpose(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.author.ID)
	s.seed(t, w.channelID, w.author, 16)

	ok := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all", "mfa_code": softLockValidCode})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, []stepup.Purpose{stepup.PurposeChannelPurge}, s.verifier.calls())
}

// TestSelfPurge_OtherAuthorsNeverCount: a moderator's purge of another
// author, or of all authors, is never counted — even on an enforcing server,
// even when the all-authors purge sweeps up the moderator's own messages.
//
// On an enforcing server both purges are D1 dangerous actions (#3454 A-3), so
// each carries mfa_code. That confirmation writes the dangerous-action budget
// and grace keys, which are not the soft-lock's; the assertion is therefore
// that no soft-lock counter key exists, not that Redis is empty.
//
// Mutant killed: counting every spec regardless of its resolved author (keys
// appear, and 20 messages trip the refusal).
func TestSelfPurge_OtherAuthorsNeverCount(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.owner.ID)
	s.seed(t, w.channelID, w.author, 20)

	other := s.purgeServer(t, w.owner.ID, w.serverID, map[string]any{
		"range": "all", "target_user_id": w.author.ID, "mfa_code": softLockValidCode,
	})
	require.Equal(t, http.StatusOK, other.Code, other.Body.String())
	assert.Zero(t, s.countBy(t, w.author.ID))

	s.seed(t, w.channelID, w.author, 20)
	s.seed(t, w.channelID, w.owner, 20)
	all := s.purgeChannel(t, w.owner.ID, w.channelID, map[string]any{"range": "all", "mfa_code": softLockValidCode})
	require.Equal(t, http.StatusOK, all.Code, all.Body.String())
	assert.Zero(t, s.countBy(t, w.owner.ID))

	for _, k := range s.mr.Keys() {
		assert.False(t, strings.HasPrefix(k, "stepup:delete_softlock:"), "soft-lock counter key %q", k)
	}
}

// TestSelfPurge_ModerationPathNeverCounts: the ban/kick purge
// (PurgeUserServerMessages) is never counted, even when its target is the
// actor.
//
// Mutant killed: moving the gate into purgeServerCore (the moderation path
// is counted and, over the threshold, refused).
func TestSelfPurge_ModerationPathNeverCounts(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.owner.ID)
	s.seed(t, w.channelID, w.owner, 20)

	deleted, status, err := s.handler.PurgeUserServerMessages(context.Background(), w.serverID, w.owner.ID, w.owner.ID, "ban", messages.PurgeExempt, false)
	require.NoError(t, err)
	assert.Equal(t, messages.PurgeCompleted, status)
	assert.Equal(t, 20, deleted)
	assert.Empty(t, s.mr.Keys())
}

// TestSelfPurge_Counting pins the bounded COUNT: below the threshold the
// purge is charged its size and passes; a zero count never touches Redis; and
// an oversized purge is charged exactly the cap, which trips both tiers.
//
// Mutants killed: charging 1 instead of the count (the counters read 1);
// calling Redis on a zero count (a key appears); dropping the LIMIT, or any
// cap other than one past the day threshold (the counters read other than
// 101).
func TestSelfPurge_Counting(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)

	t.Run("zero own messages", func(t *testing.T) {
		res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		assert.Empty(t, s.mr.Keys())
	})

	t.Run("below the threshold", func(t *testing.T) {
		s.seed(t, w.channelID, w.author, 5)
		res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		assert.Equal(t, "5", s.counter(burstKey(w.author.ID, w.serverID)))
		assert.Equal(t, "5", s.counter(dayKey(w.author.ID)))
	})

	t.Run("capped", func(t *testing.T) {
		// Start the moderator's two tiers from zero; only these keys, never a
		// flush (#2680).
		s.mr.Del(burstKey(w.moderator.ID, w.serverID))
		s.mr.Del(dayKey(w.moderator.ID))
		s.seed(t, w.channelID, w.moderator, 150)
		res := s.purgeChannel(t, w.moderator.ID, w.channelID, map[string]any{"range": "all", "target_user_id": w.moderator.ID})
		requireSoftLockRefusal(t, res, "password_required")
		assert.Equal(t, "101", s.counter(burstKey(w.moderator.ID, w.serverID)))
		assert.Equal(t, "101", s.counter(dayKey(w.moderator.ID)))
		assert.Equal(t, 150, s.countBy(t, w.moderator.ID))
	})
}

// TestSelfPurge_BodyCaps pins the step-up fields' caps on the purge body,
// ReadOptionalStepUp's: 256 CHARACTERS for the code and 1 KiB of BYTES for
// the password.
//
// Mutants killed: dropping either cap (the oversize body is accepted);
// counting the code in bytes (256 two-byte characters are refused); counting
// the password in characters (513 two-byte characters are accepted).
//
// The whole body is capped too, at the delete routes' 4 KiB (the red-team
// pass on #3455 found the purge routes read it with no limit at all). Both
// step-up fields at their caps still fit inside it.
func TestSelfPurge_BodyCaps(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.setOwnRule(t, w.author.ID, false) // outside the population: the caps alone decide

	// Re-decided for #3509: step_up_token takes mfa_code's 256-character cap,
	// and current_password has no cap any more because its presence alone is
	// a 400, whatever its value or the actor's position.
	cases := []struct {
		name  string
		field string
		value any
		want  int
	}{
		{"code at the cap", "mfa_code", strings.Repeat("é", 256), http.StatusOK},
		{"code over the cap", "mfa_code", strings.Repeat("1", 257), http.StatusBadRequest},
		{"token at the cap", "step_up_token", strings.Repeat("é", 256), http.StatusOK},
		{"token over the cap", "step_up_token", strings.Repeat("t", 257), http.StatusBadRequest},
		{"any current_password", "current_password", "a", http.StatusBadRequest},
		{"null current_password", "current_password", nil, http.StatusBadRequest},
		{"whole body over 4 KiB", "padding", strings.Repeat("x", 5000), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all", tc.field: tc.value})
			require.Equal(t, tc.want, res.Code, res.Body.String())
			if tc.want == http.StatusBadRequest {
				assert.Equal(t, "Invalid request body", decode(t, res)["error"])
			}
		})
	}

	// Control: both step-up fields at their caps together stay under 4 KiB.
	t.Run("both fields at their caps", func(t *testing.T) {
		res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{
			"range":         "all",
			"mfa_code":      strings.Repeat("é", 256),
			"step_up_token": strings.Repeat("é", 256),
		})
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	})
}

// TestSelfPurge_DeadRedis: a population member's self-purge with the
// counter's Redis unreachable is refused with the 503, purges nothing and
// writes no audit row.
//
// Mutant killed: proceeding on a HitN error (the purge runs).
func TestSelfPurge_DeadRedis(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	s := buildSoftLockHarness(t, ts, ts.DB, fastRedisClient(t, "127.0.0.1:1"), nil)
	w := s.world(t, false)
	s.seed(t, w.channelID, w.author, 3)

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
	require.Equal(t, http.StatusServiceUnavailable, res.Code, res.Body.String())
	assert.Equal(t, stepup.ErrMsgDeleteGuardUnavailable, decode(t, res)["error"])
	assert.Equal(t, 3, s.countBy(t, w.author.ID))
	assert.Zero(t, s.auditRows(t, w.channelID))
}

// purgeRaw sends body as the purge request's raw bytes, so a test can send
// what no map encodes: a second JSON value, or trailing bytes.
func (s *softLockHarness) purgeRaw(t *testing.T, userID, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", userID)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

// TestSelfPurge_BodyIsOneBoundedDocument: a purge body is exactly one JSON
// object within the 4 KiB bound. The binder stopped at the end of the first
// value, so a second value, or kilobytes after a valid object, were never read
// and the purge ran (review of #3509). Both purge routes read one body.
func TestSelfPurge_BodyIsOneBoundedDocument(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.setOwnRule(t, w.author.ID, false) // outside the population: the body alone decides
	routes := map[string]string{
		"channel": "/channels/" + w.channelID + "/messages",
		"server":  "/servers/" + w.serverID + "/messages",
	}
	cases := []struct {
		name, body string
		want       int
	}{
		{"a second JSON value", `{"range":"all"}{"range":"all"}`, http.StatusBadRequest},
		{"trailing bytes past 4 KiB", `{"range":"all"}` + strings.Repeat("x", 5000), http.StatusBadRequest},
		{"trailing garbage within the bound", `{"range":"all"} x`, http.StatusBadRequest},
		{"null", `null`, http.StatusBadRequest},
		{"control: one object with trailing whitespace", "{\"range\":\"all\"} \n", http.StatusOK},
	}
	for route, path := range routes {
		for _, tc := range cases {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				s.seed(t, w.channelID, w.author, 1)
				before := s.countBy(t, w.author.ID)
				res := s.purgeRaw(t, w.author.ID, path, tc.body)
				require.Equal(t, tc.want, res.Code, res.Body.String())
				if tc.want == http.StatusBadRequest {
					assert.Equal(t, "Invalid request body", decode(t, res)["error"])
					assert.Equal(t, before, s.countBy(t, w.author.ID), "a refused body purges nothing")
				} else {
					assert.Zero(t, s.countBy(t, w.author.ID), "the control purges")
				}
			})
		}
	}
}
