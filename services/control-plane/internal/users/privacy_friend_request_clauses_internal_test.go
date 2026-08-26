package users

// Spec T10: buildPrivacyClauses' handling of allow_friend_requests_from.
//
// In-package because buildPrivacyClauses, updatePrivacyRequest and
// friendRequestMode are all unexported. Driving this through the HTTP endpoint
// instead would need a database for every case, and would prove less: the $N
// numbering that the argIdx subtests below pin is invisible from outside — a
// clause built with the wrong placeholder index writes ANOTHER column's value
// into this one, and the request still returns 200.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// invalidModeMessage is the exact §6.2 body. Spelled out here rather than
// referenced from handlers.go on purpose: the desktop client discriminates on
// this string, so a test that shared the constant would go green through a copy
// change that broke the client.
const invalidModeMessage = "allow_friend_requests_from must be everyone, mutual_servers, or nobody"

func modePtr(m friendRequestMode) *friendRequestMode { return &m }

// TestBuildPrivacyClausesAcceptsEachValidFriendRequestMode covers the happy
// path for all three enum values. Each must append exactly one clause at $2
// (the first free placeholder, $1 being userID) and exactly one arg.
func TestBuildPrivacyClausesAcceptsEachValidFriendRequestMode(t *testing.T) {
	for _, mode := range []friendRequestMode{
		friendRequestModeEveryone, friendRequestModeMutualServers, friendRequestModeNobody,
	} {
		t.Run(string(mode), func(t *testing.T) {
			clauses, args, status, msg := buildPrivacyClauses(
				&updatePrivacyRequest{AllowFriendRequestsFrom: modePtr(mode)})

			require.Zero(t, status, "a valid mode must not be a validation failure")
			require.Empty(t, msg)
			require.Equal(t, []string{"allow_friend_requests_from = $2"}, clauses)

			// The arg must be a plain string, not a friendRequestMode: lib/pq
			// has no driver.Valuer for a named string type and rejects it at
			// exec time with "unsupported type", which no compile step catches.
			require.Len(t, args, 1)
			assert.Equal(t, string(mode), args[0])
			assert.IsType(t, "", args[0],
				"the arg must be driver-representable; a bare friendRequestMode fails "+
					"at Exec, not at compile")
		})
	}
}

// TestBuildPrivacyClausesRejectsAnInvalidFriendRequestMode locks the 400 arm,
// including its exact copy.
//
// The empty string matters most: it is what a client sends for a cleared
// control, and a validator written as a set-membership test on non-empty values
// would let it through to the CHECK constraint, turning a 400 into a 500.
func TestBuildPrivacyClausesRejectsAnInvalidFriendRequestMode(t *testing.T) {
	for _, bad := range []friendRequestMode{
		"", "EVERYONE", "friends", "mutual servers", "nobody ", "everyone,nobody",
	} {
		t.Run(string("mode="+bad), func(t *testing.T) {
			clauses, args, status, msg := buildPrivacyClauses(
				&updatePrivacyRequest{AllowFriendRequestsFrom: modePtr(bad)})

			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, invalidModeMessage, msg)
			assert.Nil(t, clauses, "a rejected request must build no SQL")
			assert.Nil(t, args)
		})
	}
}

// TestBuildPrivacyClausesRejectsAnInvalidModeEvenAlongsideValidFields is the
// fail-closed arm.
//
// A caller that batched a legal toggle with an illegal mode must have the WHOLE
// update refused. Returning the valid clauses and dropping the invalid one
// would half-apply a settings PATCH and hand the client a 400 for a change that
// partly landed.
func TestBuildPrivacyClausesRejectsAnInvalidModeEvenAlongsideValidFields(t *testing.T) {
	yes := true
	clauses, args, status, msg := buildPrivacyClauses(&updatePrivacyRequest{
		SearchableByEmail:       &yes,
		AllowFriendRequestsFrom: modePtr("garbage"),
	})

	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, invalidModeMessage, msg)
	assert.Nil(t, clauses, "the valid sibling clause must be discarded too")
	assert.Nil(t, args)
}

// TestBuildPrivacyClausesOmitsFriendRequestModeWhenAbsent is the partial-update
// contract: a PATCH that does not mention the field must not write it.
//
// The nil case is asserted twice — alone (no clauses at all) and beside another
// field (that field's clause still at $2, unshifted) — because a nil-handling
// bug that appended an empty clause would show up only in the second shape.
func TestBuildPrivacyClausesOmitsFriendRequestModeWhenAbsent(t *testing.T) {
	t.Run("no fields at all", func(t *testing.T) {
		clauses, args, status, msg := buildPrivacyClauses(&updatePrivacyRequest{})
		require.Zero(t, status)
		require.Empty(t, msg)
		assert.Empty(t, clauses, "an empty request must build no clauses")
		assert.Empty(t, args)
	})

	t.Run("another field only", func(t *testing.T) {
		yes := true
		clauses, args, status, msg := buildPrivacyClauses(
			&updatePrivacyRequest{SearchableByEmail: &yes})
		require.Zero(t, status)
		require.Empty(t, msg)
		assert.Equal(t, []string{"searchable_by_email = $2"}, clauses)
		assert.Equal(t, []interface{}{true}, args)
		for _, c := range clauses {
			assert.NotContains(t, c, "allow_friend_requests_from")
		}
	})
}

// TestBuildPrivacyClausesNumbersTheFriendRequestModePlaceholderInOrder is the
// argIdx-discipline lock.
//
// The mode clause is appended LAST, after thirteen bool clauses and the
// dm_privacy_level branch. If it reused a placeholder index — or if a preceding
// branch forgot to advance argIdx — the SET list and the args slice would
// disagree and the column would be written with a neighbouring field's value.
// Both survive Exec, so nothing but placeholder arithmetic catches it.
//
// dm_privacy_level is included deliberately: it is the only branch that appends
// EXTRA clauses (dmPrivacyLegacySync) without consuming placeholders, so it is
// the one place where clause count and arg count legitimately diverge.
func TestBuildPrivacyClausesNumbersTheFriendRequestModePlaceholderInOrder(t *testing.T) {
	yes := true
	level := 2
	clauses, args, status, msg := buildPrivacyClauses(&updatePrivacyRequest{
		MessagesFriendsOnly:     &yes,
		DMPrivacyLevel:          &level,
		SearchableByEmail:       &yes,
		AllowFriendRequestsFrom: modePtr(friendRequestModeMutualServers),
	})
	require.Zero(t, status)
	require.Empty(t, msg)

	// $1 is userID; the four placeholder-consuming fields take $2..$5 in
	// declaration order, with the mode last.
	assert.Contains(t, clauses, "messages_friends_only = $2")
	assert.Contains(t, clauses, "dm_privacy_level = $3")
	assert.Contains(t, clauses, "searchable_by_email = $4")
	assert.Contains(t, clauses, "allow_friend_requests_from = $5")

	require.Equal(t, []interface{}{true, 2, true, "mutual_servers"}, args,
		"the args slice must line up positionally with $2..$5")
	assert.Equal(t, "allow_friend_requests_from = $5", clauses[len(clauses)-1],
		"the mode clause is appended last; if it moves, re-check every placeholder above")
}
