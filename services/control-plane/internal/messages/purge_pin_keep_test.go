package messages_test

// Channel and server purges keep pinned messages unless the request sets
// include_pinned (#3458), and the self-purge soft-lock counts only the rows
// the purge may delete (spec invariant I3).

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

func (s *softLockHarness) pin(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		_, err := s.ts.DB.Exec(`UPDATE messages SET pinned_at = NOW(), pinned_by = user_id WHERE id = $1`, id)
		require.NoError(t, err)
	}
}

func (s *softLockHarness) auditIncludePinned(t *testing.T, contextID string) bool {
	t.Helper()
	var included bool
	require.NoError(t, s.ts.DB.QueryRow(`SELECT include_pinned FROM message_purges WHERE context_id = $1`, contextID).Scan(&included))
	return included
}

// An owner's all-authors purge is never soft-locked, so these cases isolate
// the flag. Absent and false must match: an older client never sends it.
//
// Mutants killed: dropping IncludePinned from PurgeChannel's or
// runServerPurge's Plan (the "true" case keeps the pin).
func TestPurge_IncludePinnedReachesTheEngine(t *testing.T) {
	cases := []struct {
		name          string
		includePinned any // nil = omitted
		wantKept      bool
	}{
		{"absent", nil, true},
		{"false", false, true},
		{"true", true, false},
	}
	for _, route := range []string{"channel", "server"} {
		for _, tc := range cases {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				s := newSoftLockHarness(t)
				w := s.world(t, false)
				ids := s.seed(t, w.channelID, w.author, 3)
				s.pin(t, ids[0])
				body := map[string]any{"range": "all"}
				if tc.includePinned != nil {
					body["include_pinned"] = tc.includePinned
				}

				res, contextID := s.purgeChannel(t, w.owner.ID, w.channelID, body), w.channelID
				if route == "server" {
					res, contextID = s.purgeServer(t, w.owner.ID, w.serverID, body), w.serverID
				}

				require.Equal(t, http.StatusOK, res.Code, res.Body.String())
				assert.Equal(t, tc.wantKept, s.messageExists(t, ids[0]), "pinned message kept")
				assert.False(t, s.messageExists(t, ids[1]))
				assert.Equal(t, !tc.wantKept, s.auditIncludePinned(t, contextID))
			})
		}
	}
}

// 16 own messages with 2 pinned sit either side of the threshold (15):
// keeping pins counts 14 and needs no confirmation; including them counts 16
// and does.
//
// Mutants killed: dropping the soft-lock pin predicate (OFF is refused);
// binding $6 to a constant false (ON passes unconfirmed).
func TestSelfPurge_SoftLockCountExcludesKeptPins(t *testing.T) {
	t.Run("off: pins are not counted", func(t *testing.T) {
		s := newSoftLockHarness(t)
		w := s.world(t, false)
		ids := s.seed(t, w.channelID, w.author, 16)
		s.pin(t, ids[0], ids[1])

		res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		assert.EqualValues(t, 14, decode(t, res)["deleted_count"])
		assert.Equal(t, 2, s.countBy(t, w.author.ID), "both pins survive")
		assert.Equal(t, "14", s.counter(burstKey(w.author.ID, w.serverID)), "only deleted messages are charged")
	})

	t.Run("on: pins are counted", func(t *testing.T) {
		s := newSoftLockHarness(t)
		w := s.world(t, false)
		ids := s.seed(t, w.channelID, w.author, 16)
		s.pin(t, ids[0], ids[1])

		requireSoftLockRefusal(t, s.purgeChannel(t, w.author.ID, w.channelID,
			map[string]any{"range": "all", "include_pinned": true}), "password_required")
		assert.Equal(t, 16, s.countBy(t, w.author.ID))
	})
}

// Under the threshold the counted IDs fence the purge, so a pin the count
// skipped stays out of reach even if it is unpinned before the first batch.
// The audit row is written after the count and before that batch.
//
// Mutant killed: dropping the soft-lock pin predicate (the pin is counted
// into the fence, and the engine deletes it once unpinned).
func TestSelfPurge_UnderThreshold_UnpinAfterCountStaysKept(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	ids := s.seed(t, w.channelID, w.author, 10)
	s.pin(t, ids[0])
	s.onPurgeAudit(t, w.author.ID, fmt.Sprintf(
		`UPDATE messages SET pinned_at = NULL, pinned_by = NULL WHERE id = '%s'`, uuid.MustParse(ids[0])))

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{"range": "all"})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.EqualValues(t, 9, decode(t, res)["deleted_count"])
	assert.True(t, s.messageExists(t, ids[0]), "a pin the count skipped is outside the fence")
}

// Over the threshold there is no fence: the engine's own capture is what
// keeps a pin (I1). A pin unpinned before the engine first looks is no longer
// a pin and is deleted; one still pinned is kept.
func TestSelfPurge_OverThreshold_EngineKeepsWhatItSeesPinned(t *testing.T) {
	s := newSoftLockHarness(t)
	w := s.world(t, false)
	ids := s.seed(t, w.channelID, w.author, 20)
	s.pin(t, ids[0], ids[1])
	s.onPurgeAudit(t, w.author.ID, fmt.Sprintf(
		`UPDATE messages SET pinned_at = NULL, pinned_by = NULL WHERE id = '%s'`, uuid.MustParse(ids[0])))

	res := s.purgeChannel(t, w.author.ID, w.channelID, map[string]any{
		"range": "all", "step_up_token": s.mintToken(t, w.author.ID, stepup.PurposeChannelPurge),
	})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.False(t, s.messageExists(t, ids[0]), "unpinned before the engine looked")
	assert.True(t, s.messageExists(t, ids[1]), "still pinned when the engine looked")
	assert.Equal(t, 1, s.countBy(t, w.author.ID))
}
