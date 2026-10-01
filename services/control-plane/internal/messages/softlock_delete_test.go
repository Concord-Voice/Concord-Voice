package messages_test

// Handler-seam tests for the channel message delete's soft-lock (#3455 T4,
// design spec §2.4–§2.6 as amended by D-1). Each test names the mutant it
// kills; see softlock_harness_test.go for the harness and why it uses
// miniredis.

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// deleteN deletes each of ids as userID and requires a 200 for each.
func (s *softLockHarness) deleteN(t *testing.T, userID string, ids []string) {
	t.Helper()
	for i, id := range ids {
		w := s.deleteMessage(t, userID, id, nil)
		require.Equalf(t, http.StatusOK, w.Code, "delete %d: %s", i+1, w.Body.String())
	}
}

// TestDeleteSoftLock_AC1_ServerRule is AC1 under the server rule: on an
// enforcing server, deletes 1–15 pass, the 16th is refused for MFA, the 16th
// with a valid code succeeds and resets both tiers, the next 15 pass, and the
// 16th after that is refused again.
//
// Mutants killed: dropping the Hit (the 16th succeeds); dropping the refusal
// decoration (no delete_rate_limited / Retry-After); dropping resetSoftLock
// after a confirmed commit (the second run of 15 is refused at its first
// delete); passing a purpose other than PurposeMessageDelete.
func TestDeleteSoftLock_AC1_ServerRule(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.author.ID)
	ids := s.seed(t, w.channelID, w.author, 33)

	s.deleteN(t, w.author.ID, ids[:15])

	refused := s.deleteMessage(t, w.author.ID, ids[15], nil)
	body := requireSoftLockRefusal(t, refused, "mfa_required")
	assert.Equal(t, "MFA verification required", body["error"])
	assert.Equal(t, []any{"totp"}, body["methods"])
	assert.Equal(t, "30", refused.Header().Get("Retry-After"))
	assert.True(t, s.messageExists(t, ids[15]), "a refused delete must not delete")

	confirmed := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"mfa_code": softLockValidCode})
	require.Equal(t, http.StatusOK, confirmed.Code, confirmed.Body.String())
	assert.False(t, s.messageExists(t, ids[15]))
	assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)), "a confirmed delete resets the burst tier")
	assert.Empty(t, s.counter(dayKey(w.author.ID)), "a confirmed delete resets the day tier")
	assert.Empty(t, s.counter(budgetKey(w.author.ID)), "a verified commit clears the budget")
	assert.Equal(t, []stepup.Purpose{stepup.PurposeMessageDelete}, s.verifier.calls())

	s.deleteN(t, w.author.ID, ids[16:31])
	requireSoftLockRefusal(t, s.deleteMessage(t, w.author.ID, ids[31], nil), "mfa_required")
}

// TestDeleteSoftLock_AC1_OwnRule is AC1 under the own rule: on a
// non-enforcing server an unenrolled author with the default setting (no
// privacy row, which reads TRUE) confirms with the password. A wrong password
// is refused byte-exactly, and the budget is charged for it and cleared by the
// verified commit that follows.
//
// Mutants killed: governing the own rule with MFA only (the 16th answers
// mfa_enrollment_required instead of password_required); reading a missing
// privacy row as FALSE (the 16th succeeds); clearing the budget after a
// refusal (the budget reads empty after the wrong password).
func TestDeleteSoftLock_AC1_OwnRule(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	ids := s.seed(t, w.channelID, w.author, 33)

	s.deleteN(t, w.author.ID, ids[:15])

	requireSoftLockRefusal(t, s.deleteMessage(t, w.author.ID, ids[15], nil), "password_required")

	// #3509: the password reaches only the mint endpoint. A token that matches
	// nothing is the refusal a wrong password used to be, and is charged.
	wrong := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"step_up_token": "never-minted-token-0000000000000000000"})
	body := requireSoftLockRefusal(t, wrong, "step_up_token_invalid")
	assert.Equal(t, stepup.ErrMsgStepUpTokenInvalid, body["error"])
	assert.Equal(t, true, body["password_required"])
	assert.Equal(t, "1", s.counter(budgetKey(w.author.ID)), "a refused factor is charged")

	ok := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"step_up_token": s.mintToken(t, w.author.ID, stepup.PurposeMessageDelete)})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Empty(t, s.counter(budgetKey(w.author.ID)))
	assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)))
	assert.Empty(t, s.verifier.calls(), "an account without MFA never reaches the MFA verifier")

	s.deleteN(t, w.author.ID, ids[16:31])
	requireSoftLockRefusal(t, s.deleteMessage(t, w.author.ID, ids[31], nil), "password_required")
}

// TestDeleteSoftLock_RefusalsPrecedeCounting pins I7: a permission 403 and a
// 404 leave no counter, so nothing about the population reaches a member the
// route refuses anyway.
//
// Mutant killed: moving the Hit above preflightMessageDelete's permission check
// (a key appears).
func TestDeleteSoftLock_RefusalsPrecedeCounting(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	ownerMsg := s.seed(t, w.channelID, w.owner, 1)[0]

	forbidden := s.deleteMessage(t, w.author.ID, ownerMsg, nil)
	require.Equal(t, http.StatusForbidden, forbidden.Code, forbidden.Body.String())
	assert.Nil(t, decode(t, forbidden)["delete_rate_limited"])

	missing := s.deleteMessage(t, w.author.ID, "00000000-0000-4000-8000-000000000000", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	assert.Empty(t, s.mr.Keys(), "no refusal may touch the counter")
}

// TestDeleteSoftLock_Population pins the D-1 population table: a moderator
// deleting someone else's message on a non-enforcing server, and an author
// whose own rule is explicitly off, are never counted and never refused; an
// author with no privacy row is counted.
//
// Mutants killed: counting everyone (keys appear for the first two cases);
// granting the own rule on the author's setting without actor == author (the
// moderator is counted); reading the row as FALSE when absent (the third case
// has no key).
func TestDeleteSoftLock_Population(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)

	t.Run("moderator deleting another author's messages", func(t *testing.T) {
		s.deleteN(t, w.moderator.ID, s.seed(t, w.channelID, w.author, 20))
		assert.Empty(t, s.mr.Keys())
	})

	t.Run("author with the own rule off", func(t *testing.T) {
		s.setOwnRule(t, w.moderator.ID, false)
		s.deleteN(t, w.moderator.ID, s.seed(t, w.channelID, w.moderator, 20))
		assert.Empty(t, s.mr.Keys())
	})

	t.Run("author with no privacy row", func(t *testing.T) {
		s.deleteN(t, w.author.ID, s.seed(t, w.channelID, w.author, 1))
		assert.Equal(t, "1", s.counter(burstKey(w.author.ID, w.serverID)))
		assert.Equal(t, "1", s.counter(dayKey(w.author.ID)))
	})
}

// TestDeleteSoftLock_DeadRedis is AC4 at the handler seam: with the counter's
// Redis unreachable a population member gets the 503 and nothing is deleted,
// while a member outside the population deletes normally. The 503's log line
// carries no population field (C7).
//
// Mutants killed: proceeding on a Hit error (the row is deleted); skipping
// the Hit for everyone when Redis is down (the population member gets 200);
// adding user_id or server_id to the 503's log line.
func TestDeleteSoftLock_DeadRedis(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	s := buildSoftLockHarness(t, ts, ts.DB, fastRedisClient(t, "127.0.0.1:1"), nil)
	w := s.world(t, false)
	authorMsg := s.seed(t, w.channelID, w.author, 1)[0]

	refused := s.deleteMessage(t, w.author.ID, authorMsg, nil)
	require.Equal(t, http.StatusServiceUnavailable, refused.Code, refused.Body.String())
	assert.Equal(t, stepup.ErrMsgDeleteGuardUnavailable, decode(t, refused)["error"])
	assert.Equal(t, "30", refused.Header().Get("Retry-After"))
	assert.True(t, s.messageExists(t, authorMsg), "a 503 must not delete")

	other := s.deleteMessage(t, w.moderator.ID, authorMsg, nil)
	require.Equal(t, http.StatusOK, other.Code, "outside the population, Redis is never consulted")

	s.requireNoPopulationField(t, w)
}

// TestDeleteSoftLock_BudgetMatrix pins X4: the shared step-up budget is
// charged only over the threshold and only when a factor is present, cleared
// only after a verified commit, and an exhausted budget refuses before the
// verifier runs.
//
// Mutants killed: charging under the threshold (the budget reads 1 after the
// first delete); verifying a code under the threshold (the verifier is
// called); charging a factor-less request (the budget reads 1 after the bare
// refusal); consuming after the transaction opens instead of refusing first
// (the verifier is called on the exhausted budget).
func TestDeleteSoftLock_BudgetMatrix(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.author.ID)
	ids := s.seed(t, w.channelID, w.author, 40)

	under := s.deleteMessage(t, w.author.ID, ids[0], map[string]string{"mfa_code": "000000"})
	require.Equal(t, http.StatusOK, under.Code, "under the threshold a supplied code is ignored")
	assert.Empty(t, s.counter(budgetKey(w.author.ID)))
	assert.Empty(t, s.verifier.calls())

	s.deleteN(t, w.author.ID, ids[1:15])
	requireSoftLockRefusal(t, s.deleteMessage(t, w.author.ID, ids[15], nil), "mfa_required")
	assert.Empty(t, s.counter(budgetKey(w.author.ID)), "a request without a factor is not charged")

	wrong := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"mfa_code": "000000"})
	assert.Equal(t, stepup.ErrMsgInvalidMFACode, requireSoftLockRefusal(t, wrong, "")["error"])
	assert.Equal(t, "1", s.counter(budgetKey(w.author.ID)))

	ok := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"mfa_code": softLockValidCode})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Empty(t, s.counter(budgetKey(w.author.ID)))

	s.deleteN(t, w.author.ID, ids[16:31])
	require.NoError(t, s.mr.Set(budgetKey(w.author.ID), "5"))
	before := len(s.verifier.calls())
	exhausted := s.deleteMessage(t, w.author.ID, ids[31], map[string]string{"mfa_code": softLockValidCode})
	require.Equal(t, http.StatusTooManyRequests, exhausted.Code, exhausted.Body.String())
	assert.Equal(t, true, decode(t, exhausted)["step_up_budget_exhausted"])
	assert.Len(t, s.verifier.calls(), before, "an exhausted budget refuses before the verifier runs")
	assert.True(t, s.messageExists(t, ids[31]))
}

// TestDeleteSoftLock_EnrollmentRequired: on an enforcing server the server
// rule governs even for the author, and it is MFA only, so an unenrolled
// author is refused with mfa_enrollment_required and a Retry-After; once the
// burst window rolls over the delete succeeds.
//
// Mutant killed: letting the own rule govern when both apply (the refusal
// becomes password_required, and a password would pass).
func TestDeleteSoftLock_EnrollmentRequired(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	ids := s.seed(t, w.channelID, w.author, 16)

	s.deleteN(t, w.author.ID, ids[:15])
	// #3509: the password's stand-in on the route is a minted password token,
	// and the server rule refuses it exactly as it refused the password.
	withToken := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"step_up_token": s.mintToken(t, w.author.ID, stepup.PurposeMessageDelete)})
	body := requireSoftLockRefusal(t, withToken, "mfa_enrollment_required")
	assert.Equal(t, stepup.ErrMsgMFAEnrollmentRequired, body["error"])

	s.mr.FastForward(stepup.DeleteSoftLockWindow)
	after := s.deleteMessage(t, w.author.ID, ids[15], nil)
	require.Equal(t, http.StatusOK, after.Code, after.Body.String())
}

// TestDeleteSoftLock_InFlightFlipOff_OwnRuleConfirms is the orchestrator's
// ruling on a flip: the server read as enforcing, the owner turned it OFF
// before the transaction's lock, and the own rule applies to this author, so
// the delete must confirm under the own rule — never pass unverified.
//
// Mutant killed: answering (false, nil) whenever the locked gate reads not
// enforcing (the first request deletes without any factor).
func TestDeleteSoftLock_InFlightFlipOff_OwnRuleConfirms(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	ids := s.seed(t, w.channelID, w.author, 16)
	s.deleteN(t, w.author.ID, ids[:15])

	s.handler.SetBeforeSoftLockConfirmHookForTest(func() { s.setEnforcing(t, w.serverID, false) })

	refused := s.deleteMessage(t, w.author.ID, ids[15], nil)
	requireSoftLockRefusal(t, refused, "password_required")
	assert.True(t, s.messageExists(t, ids[15]))

	s.setEnforcing(t, w.serverID, true) // the unlocked read must see ON again
	ok := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"step_up_token": s.mintToken(t, w.author.ID, stepup.PurposeMessageDelete)})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)), "an own-rule confirmation resets")
}

// TestDeleteSoftLock_InFlightFlipOn_ServerRuleGoverns: the server read as not
// enforcing, the owner turned enforcement ON before the confirmation's lock,
// and the author's own rule applies. The locked re-read must see the server
// rule, which is MFA only, so an unenrolled author's password no longer passes.
// Choosing the own rule from the unlocked read let a password through a rule
// the server had already raised to MFA. From review of #3509.
func TestDeleteSoftLock_InFlightFlipOn_ServerRuleGoverns(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	ids := s.seed(t, w.channelID, w.author, 16)
	s.deleteN(t, w.author.ID, ids[:15])

	s.handler.SetBeforeSoftLockConfirmHookForTest(func() { s.setEnforcing(t, w.serverID, true) })

	res := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"step_up_token": s.mintToken(t, w.author.ID, stepup.PurposeMessageDelete)})
	requireSoftLockRefusal(t, res, "mfa_enrollment_required")
	assert.True(t, s.messageExists(t, ids[15]), "a password token does not satisfy the server rule")
}

// TestDeleteSoftLock_ConfirmOutsidePopulation_Refused: the confirmation may
// answer "no rule applies" only for an actor the unlocked read put under the
// server rule and the locked re-read took out of it. A gate that was never in
// the population is asked for the own rule's factor, so a caller that confirms
// without charging first is refused. Kills dropping g.enforcing from that arm.
func TestDeleteSoftLock_ConfirmOutsidePopulation_Refused(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)

	confirmed, err := s.handler.ConfirmOutsidePopulationForTest(context.Background(), w.moderator.ID, w.serverID)
	assert.False(t, confirmed)
	var stepErr *stepup.Error
	require.ErrorAs(t, err, &stepErr, "outside the population, nothing may be waved through")
	assert.Equal(t, http.StatusForbidden, stepErr.Status)
	assert.False(t, stepErr.EpochMismatch(), "refused for the missing factor, not the credential epoch")
}

// TestDeleteSoftLock_InFlightFlipOff_NoRuleProceeds: a moderator deleting
// someone else's message on a server that stops enforcing in flight is under
// no rule any more, so the delete proceeds unverified — with no Reset and no
// budget Clear, and a charge already taken stands.
//
// Mutants killed: reporting the unverified path as confirmed (the counter is
// reset and the budget cleared); verifying under the own rule regardless of
// who the author is (the wrong code is refused).
func TestDeleteSoftLock_InFlightFlipOff_NoRuleProceeds(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, true)
	s.enroll(t, w.moderator.ID)
	ids := s.seed(t, w.channelID, w.author, 16)
	s.deleteN(t, w.moderator.ID, ids[:15])

	s.handler.SetBeforeSoftLockConfirmHookForTest(func() { s.setEnforcing(t, w.serverID, false) })

	res := s.deleteMessage(t, w.moderator.ID, ids[15], map[string]string{"mfa_code": "000000"})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.False(t, s.messageExists(t, ids[15]))
	assert.Equal(t, "16", s.counter(burstKey(w.moderator.ID, w.serverID)), "no Reset without a verified factor")
	assert.Equal(t, "1", s.counter(budgetKey(w.moderator.ID)), "the charge stands, and is not cleared")
	assert.Empty(t, s.verifier.calls())
}

// TestDeleteMessage_CredentialEpochFence pins AC6 and the §2.6 boy-scout fix:
// a request whose credential epoch rotated in flight gets the 401 on the
// normal path (outside the population, and inside it under the threshold) and
// on the confirmation path, and deletes nothing.
//
// Mutant killed: removing the normal path's Guard (the first two cases
// delete, 200).
func TestDeleteMessage_CredentialEpochFence(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	ids := s.seed(t, w.channelID, w.author, 17)
	s.deleteN(t, w.author.ID, ids[:15])

	for _, userID := range []string{w.author.ID, w.moderator.ID} {
		_, err := s.ts.DB.Exec(`UPDATE users SET credential_epoch = 'rotated-epoch' WHERE id = $1`, userID)
		require.NoError(t, err)
	}
	stale := func(userID, id string) int {
		return s.do(t, request{method: http.MethodDelete, path: "/messages/" + id, userID: userID, epoch: "stale-epoch"}).Code
	}

	assert.Equal(t, http.StatusUnauthorized, stale(w.moderator.ID, ids[15]), "normal path, outside the population")
	assert.True(t, s.messageExists(t, ids[15]))

	s.mr.FastForward(stepup.DeleteSoftLockWindow)
	assert.Equal(t, http.StatusUnauthorized, stale(w.author.ID, ids[15]), "normal path, under the threshold")
	assert.True(t, s.messageExists(t, ids[15]))

	require.NoError(t, s.mr.Set(burstKey(w.author.ID, w.serverID), "15"))
	assert.Equal(t, http.StatusUnauthorized, stale(w.author.ID, ids[16]), "confirmation path")
	assert.True(t, s.messageExists(t, ids[16]))
}

// TestDeleteSoftLock_LockConflict pins §2.6's first step: a lock timeout on
// the users row arrives as a 55P03 inside a 500 *stepup.Error, and must be
// answered with the lock-conflict 503 rather than the error's own 500.
//
// Mutant killed: classifying *stepup.Error before IsLockConflict (the
// response is 500 "Verification failed").
func TestDeleteSoftLock_LockConflict(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	u, err := url.Parse(testdb.DatabaseURL())
	require.NoError(t, err)
	q := u.Query()
	q.Set("lock_timeout", "200") // milliseconds, as a startup parameter
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mr := miniredis.RunT(t)
	s := buildSoftLockHarness(t, ts, db, fastRedisClient(t, mr.Addr()), mr)
	w := s.world(t, false)
	id := s.seed(t, w.channelID, w.author, 1)[0]
	require.NoError(t, s.mr.Set(burstKey(w.author.ID, w.serverID), "15"))

	holder, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, w.author.ID)
	require.NoError(t, err)

	res := s.deleteMessage(t, w.author.ID, id, nil)
	require.Equal(t, http.StatusServiceUnavailable, res.Code, res.Body.String())
	assert.Equal(t, true, decode(t, res)["lock_conflict"])
	assert.Equal(t, "1", res.Header().Get("Retry-After"))
	assert.True(t, s.messageExists(t, id))
}

// TestDeleteSoftLock_LogsCarryNoPopulationField pins C7 on the Reset and
// Clear failure lines: after a verified commit whose Redis died mid-request,
// both best-effort writes fail and log at Warn with a fixed failure class and
// no population field, and the delete itself still succeeds.
//
// Mutants killed: treating a Reset or Clear failure as fatal (the delete
// answers 5xx); adding user_id, server_id or the scope to either line.
func TestDeleteSoftLock_LogsCarryNoPopulationField(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	ids := s.seed(t, w.channelID, w.author, 16)
	s.deleteN(t, w.author.ID, ids[:15])

	token := s.mintToken(t, w.author.ID, stepup.PurposeMessageDelete)
	s.handler.SetBeforeSoftLockConfirmHookForTest(s.mr.Close)
	res := s.deleteMessage(t, w.author.ID, ids[15], map[string]string{"step_up_token": token})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	logs := s.logs.String()
	assert.Contains(t, logs, "failure_class=delete_softlock_reset_failed")
	assert.Contains(t, logs, "failure_class=delete_softlock_budget_clear_failed")
	s.requireNoPopulationField(t, w)
}

// TestDeleteSoftLock_ClosedFlipOnIsOneUncountedDelete documents design spec
// §2.7's accepted window from the other side: a moderator outside the
// population whose server starts enforcing after the unlocked read deletes
// once, uncounted. The next delete reads the new flag and is counted.
func TestDeleteSoftLock_ClosedFlipOnIsOneUncountedDelete(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	s.enroll(t, w.moderator.ID)
	ids := s.seed(t, w.channelID, w.author, 2)

	s.handler.SetBeforeSoftLockConfirmHookForTest(func() { s.setEnforcing(t, w.serverID, true) })
	s.deleteN(t, w.moderator.ID, ids[:1])
	assert.Empty(t, s.mr.Keys(), "the flip after the unlocked read costs one uncounted delete")

	s.deleteN(t, w.moderator.ID, ids[1:])
	assert.Equal(t, "1", s.counter(burstKey(w.moderator.ID, w.serverID)))
}
