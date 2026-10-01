package messages_test

// Reproduction for the own-rule password defect on the messages routes (#3509,
// Codex security P1; design spec "Developer decisions, 2026-10-01", T-1, T-4).
//
// Under the decision the account password reaches only the mint endpoint
// (POST /api/v1/auth/step-up/password). The routes take a single-use
// step_up_token and REFUSE a body that still carries current_password, with a
// 400, so a stale client fails loudly. Today stepup.VerifyOwnRuleTx's password
// arm verifies in.CurrentPassword directly, so the correct password confirms
// the delete or purge and it goes through.
//
// Oracle: a request carrying the correct current_password is refused with 400
// and nothing is deleted or purged.
//
// Three routes are driven here (message delete, channel self-purge, server
// self-purge); the two DM routes live in
// internal/dm/message_delete_password_token_repro_test.go and
// internal/dm/clear_password_token_repro_test.go.
//
// TestOwnRuleRoutes_RefuseCurrentPassword_Repro3509 failed before the fix and
// passes after it. The pre-fix control that asserted the old contract (a wrong
// current_password refused 403 "Invalid password") was deleted with the fix,
// as T-4 requires; the MFA-account control below holds before and after.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// armedOwnRuleRoute is one own-rule route driven to the point where its next
// request meets the password arm.
type armedOwnRuleRoute struct {
	actorID string
	// password is the actor's real account password.
	password string
	// call sends the route's request with the step-up fields in body (nil for
	// none).
	call func(body map[string]any) *httptest.ResponseRecorder
	// intact reports that nothing was deleted or purged.
	intact func() bool
}

// ownRuleRoutes lists every messages route that calls stepup.VerifyOwnRuleTx.
// Each arm builds a fresh non-enforcing world, so the server rule never
// governs, leaves the actor without inline MFA (unless mfa), and leaves the
// own rule at its default (no privacy row, which reads TRUE). A request then
// meets the own-rule password arm once the soft-lock has tripped:
//   - delete: 15 deletes already made, the next is the 16th;
//   - purges: 16 of the actor's own messages exceed the threshold of 15.
var ownRuleRoutes = []struct {
	name string
	arm  func(t *testing.T, s *softLockHarness, mfa bool) armedOwnRuleRoute
}{
	{"channel message delete", func(t *testing.T, s *softLockHarness, mfa bool) armedOwnRuleRoute {
		w := s.world(t, false)
		if mfa {
			s.enroll(t, w.author.ID)
		}
		ids := s.seed(t, w.channelID, w.author, 16)
		s.deleteN(t, w.author.ID, ids[:15])
		return armedOwnRuleRoute{
			actorID:  w.author.ID,
			password: w.author.Password,
			call: func(body map[string]any) *httptest.ResponseRecorder {
				if body == nil {
					// A nil map in an `any` encodes as JSON null, which is a 400.
					return s.deleteMessage(t, w.author.ID, ids[15], nil)
				}
				return s.deleteMessage(t, w.author.ID, ids[15], body)
			},
			intact: func() bool { return s.messageExists(t, ids[15]) },
		}
	}},
	{"channel self-purge", func(t *testing.T, s *softLockHarness, mfa bool) armedOwnRuleRoute {
		w := s.world(t, false)
		if mfa {
			s.enroll(t, w.author.ID)
		}
		s.seed(t, w.channelID, w.author, 16)
		return armedOwnRuleRoute{
			actorID:  w.author.ID,
			password: w.author.Password,
			call: func(body map[string]any) *httptest.ResponseRecorder {
				return s.purgeChannel(t, w.author.ID, w.channelID, withRange(body))
			},
			intact: func() bool { return s.countBy(t, w.author.ID) == 16 && s.auditRows(t, w.channelID) == 0 },
		}
	}},
	{"server self-purge", func(t *testing.T, s *softLockHarness, mfa bool) armedOwnRuleRoute {
		w := s.world(t, false)
		if mfa {
			s.enroll(t, w.owner.ID)
		}
		s.seed(t, w.channelID, w.owner, 16)
		return armedOwnRuleRoute{
			actorID:  w.owner.ID,
			password: w.owner.Password,
			call: func(body map[string]any) *httptest.ResponseRecorder {
				withTarget := withRange(body)
				withTarget["target_user_id"] = w.owner.ID
				return s.purgeServer(t, w.owner.ID, w.serverID, withTarget)
			},
			intact: func() bool { return s.countBy(t, w.owner.ID) == 16 && s.auditRows(t, w.serverID) == 0 },
		}
	}},
}

// withRange adds the purge routes' required range to a copy of body.
func withRange(body map[string]any) map[string]any {
	out := map[string]any{"range": "all"}
	for k, v := range body {
		out[k] = v
	}
	return out
}

// regression for #3509 (Codex P1)
func TestOwnRuleRoutes_RefuseCurrentPassword_Repro3509(t *testing.T) {
	s := newSoftLockHarness(t)
	for _, route := range ownRuleRoutes {
		t.Run(route.name, func(t *testing.T) {
			armed := route.arm(t, s, false)

			// Precondition, and the arm being reached: with no step-up field the
			// route answers the own-rule password prompt. This holds before and
			// after the fix, so it proves the fixture is tripped, on the own
			// rule, for an account with no MFA, without depending on how the
			// password is later refused. (A refused request is charged to the
			// budget but never resets the counters, so the request below meets
			// the same state.)
			prompt := armed.call(nil)
			requireSoftLockRefusal(t, prompt, "password_required")
			require.True(t, armed.intact(), "precondition: the prompt must not delete or purge anything")

			res := armed.call(map[string]any{"current_password": armed.password})

			assert.Equal(t, http.StatusBadRequest, res.Code,
				"a body carrying current_password must be refused with 400: the password reaches only the mint endpoint; got %d %s",
				res.Code, res.Body.String())
			assert.True(t, armed.intact(),
				"a request that carried the account password must not delete or purge anything")
		})
	}
}

// Control, valid before and after: an account with inline MFA, sent no step-up
// field, gets the MFA refusal and never the password prompt. The repro's
// refusal belongs to the password arm only.
func TestOwnRuleRoutes_MFAAccountRefusalShape(t *testing.T) {
	s := newSoftLockHarness(t)
	for _, route := range ownRuleRoutes {
		t.Run(route.name, func(t *testing.T) {
			armed := route.arm(t, s, true)

			res := armed.call(nil)

			body := requireSoftLockRefusal(t, res, "mfa_required")
			assert.Equal(t, "MFA verification required", body["error"])
			assert.NotContains(t, body, "password_required")
			assert.True(t, armed.intact())
		})
	}
}
