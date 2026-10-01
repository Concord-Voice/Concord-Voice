package messages_test

// Regressions for the self-purge soft-lock's two gaps between its count and
// the engine's batches, both from review of #3509. Each installs a trigger on
// message_purges scoped to one actor: the audit row is the one write every
// purge makes after the count and before its first batch, so the trigger
// lands an event in exactly that window.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// trigger runs stmt, a PL/pgSQL statement, for each row of event (for example
// "AFTER INSERT ON message_purges") that when selects, until the test ends.
// Callers build when from parsed UUIDs only.
func (s *softLockHarness) trigger(t *testing.T, event, when, stmt string) {
	t.Helper()
	name := "test_softlock_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	table := event[strings.LastIndex(event, " ")+1:]
	_, err := s.ts.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger AS $$
		BEGIN %[2]s; IF TG_OP = 'DELETE' THEN RETURN OLD; END IF; RETURN NEW; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER %[1]s %[3]s
		FOR EACH ROW WHEN (%[4]s) EXECUTE FUNCTION %[1]s()`, name, stmt, event, when))
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, cleanupErr := s.ts.DB.Exec(fmt.Sprintf(
			`DROP TRIGGER IF EXISTS %[1]s ON %[2]s; DROP FUNCTION IF EXISTS %[1]s()`, name, table)); cleanupErr != nil {
			t.Errorf("cleanup %s: %v", name, cleanupErr)
		}
	})
}

// onPurgeAudit runs stmt whenever actorID's purge writes its audit row.
func (s *softLockHarness) onPurgeAudit(t *testing.T, actorID, stmt string) {
	t.Helper()
	s.trigger(t, "AFTER INSERT ON message_purges", fmt.Sprintf("NEW.actor_id = '%s'::uuid", uuid.MustParse(actorID)), stmt)
}

// TestSelfPurge_MessageSentAfterTheCount_IsNotPurged: the soft-lock counts the
// author's 15 messages, which is at the threshold and not over it, so no
// confirmation runs. A 16th arrives before the first batch. The purge must
// delete only the 15 it counted: deleting the 16th as well would take the
// author past the threshold without a confirmation, and leave both tiers one
// short.
func TestSelfPurge_MessageSentAfterTheCount_IsNotPurged(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.seed(t, w.channelID, w.author, 15)
	late := uuid.NewString()
	s.onPurgeAudit(t, w.author.ID, fmt.Sprintf(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		VALUES ('%s', '%s', '%s', 'late', 1, FALSE, NOW(), NOW())`,
		uuid.MustParse(late), uuid.MustParse(w.channelID), uuid.MustParse(w.author.ID)))

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.EqualValues(t, 15, decode(t, res)["deleted_count"], "only the counted messages are purged")
	assert.True(t, s.messageExists(t, late), "a message sent after the count survives the purge")
	assert.Equal(t, "15", s.counter(burstKey(w.author.ID, w.serverID)))
}

// TestSelfPurge_NothingCounted_NothingOwnPurged is the same window with a
// zero count: the author had nothing to count, so the purge may delete none
// of their messages, including one sent after the count.
func TestSelfPurge_NothingCounted_NothingOwnPurged(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	late := uuid.NewString()
	s.onPurgeAudit(t, w.author.ID, fmt.Sprintf(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		VALUES ('%s', '%s', '%s', 'late', 1, FALSE, NOW(), NOW())`,
		uuid.MustParse(late), uuid.MustParse(w.channelID), uuid.MustParse(w.author.ID)))

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.EqualValues(t, 0, decode(t, res)["deleted_count"])
	assert.True(t, s.messageExists(t, late), "a message sent after a zero count survives the purge")
}

// TestSelfPurge_FailedAdmission_SpendsNoFactor: the author confirms with a
// valid code, and the purge then fails to write its audit row, so nothing is
// purged. The confirmation must commit with that admission, not before it:
// committed alone, it spent a single-use factor on a purge that did nothing,
// and cleared the step-up budget for a verification that no longer counts.
func TestSelfPurge_FailedAdmission_SpendsNoFactor(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.enroll(t, w.author.ID)
	s.seed(t, w.channelID, w.author, 16)
	s.onPurgeAudit(t, w.author.ID, `RAISE EXCEPTION 'audit refused'`)

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all", "mfa_code": softLockValidCode})
	require.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
	assert.Equal(t, 16, s.countBy(t, w.author.ID), "nothing is purged")
	assert.Equal(t, []stepup.Purpose{stepup.PurposeChannelPurge}, s.verifier.calls(), "the code was verified")
	assert.False(t, s.totpStepSpent(t, w.author.ID), "a purge that failed its admission spends no factor")
	assert.Equal(t, "1", s.counter(budgetKey(w.author.ID)), "and leaves the budget charged")
}

// TestSelfPurge_BatchFailureAfterAdmission_KeepsCounters: the confirmation
// verified and the admission committed, so the factor is spent and the budget
// cleared; the first batch then fails, so nothing is purged. The counters
// reset only after a purge that succeeded, so they stand.
func TestSelfPurge_BatchFailureAfterAdmission_KeepsCounters(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.enroll(t, w.author.ID)
	s.seed(t, w.channelID, w.author, 16)
	s.trigger(t, "BEFORE DELETE ON messages", fmt.Sprintf("OLD.user_id = '%s'::uuid", uuid.MustParse(w.author.ID)),
		`RAISE EXCEPTION 'delete refused'`)

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all", "mfa_code": softLockValidCode})
	require.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
	assert.Equal(t, 16, s.countBy(t, w.author.ID), "nothing is purged")
	assert.Equal(t, 1, s.auditRows(t, w.channelID), "the purge was admitted")
	assert.True(t, s.totpStepSpent(t, w.author.ID), "the admission spent the factor")
	assert.Empty(t, s.counter(budgetKey(w.author.ID)), "and cleared the budget")
	assert.Equal(t, "16", s.counter(burstKey(w.author.ID, w.serverID)), "a failed purge resets nothing")
}

// TestSelfPurge_ServerRefusal_NoPurgeFailureLine: a soft-lock refusal on the
// server route is answered as a refusal, not logged as a failed purge. The
// failure line carries server_id, which a soft-lock line must not (C7).
func TestSelfPurge_ServerRefusal_NoPurgeFailureLine(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.owner.ID)
	s.seed(t, w.channelID, w.owner, 16)

	res := s.purgeServer(t, w.owner.ID, w.serverID, map[string]any{"range": "all", "target_user_id": w.owner.ID})
	requireSoftLockRefusal(t, res, "mfa_required")
	assert.NotContains(t, s.logs.String(), "Server purge failed")
	assert.Zero(t, s.auditRows(t, w.serverID), "a refusal leaves no audit row")
}
