package friends

// Gate-helper coverage for #1240 (spec T1-T3).
//
// This test is in package friends, which cannot import internal/testhelpers:
// that package builds the whole router and therefore depends on this one. It
// uses internal/testhelpers/testdb instead, which has zero internal
// dependencies. Same reasoning as friend_code_claim_internal_test.go and
// stranger_gate_internal_test.go.
//
// Being in-package is the point here. canReceiveFriendRequestFrom is
// deliberately unexported (spec §5, conflict 5), and the truth table it
// implements is not observable end-to-end: over HTTP, five of the six cells
// collapse onto the same 403 body BY DESIGN — that indistinguishability is the
// feature (§2) — so this is the only place the six cells can be told apart.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The three enum values, spelled as literals rather than imported from
// internal/users. The column is a cross-package contract — users writes it,
// this package reads it — and privacy.go hard-codes 'nobody' and 'everyone' as
// SQL string literals. A shared Go constant would let a rename compile on both
// sides while leaving the SQL silently unmatched.
const (
	modeEveryone      = "everyone"
	modeMutualServers = "mutual_servers"
	modeNobody        = "nobody"
)

// createServerWithMembers inserts a server owned by the first member and adds
// every listed user to it.
//
// Package-local rather than added to internal/testhelpers/testdb on purpose
// (spec §11): the shared fixture package is under concurrent edit, so this
// keeps the blast radius minimal. It inserts the bare membership row and
// nothing else — no roles, no @all — because the gate's join reads
// server_members alone, and a fixture that seeded RBAC would mask a gate that
// had started consulting it.
func createServerWithMembers(t *testing.T, db *sql.DB, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	require.NotEmpty(t, members, "a server needs at least an owner")

	serverID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'gate-test-server', $2)`,
		serverID, members[0],
	)
	require.NoError(t, err)

	for _, m := range members {
		_, err := db.Exec(
			`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
			serverID, m,
		)
		require.NoError(t, err)
	}
	return serverID
}

// setFriendRequestMode materializes the target's privacy_settings row at the
// given mode. Rows are created lazily, so calling this is what separates a
// CONFIGURED target from the row-less majority exercised by
// TestCanReceiveFriendRequestFromTreatsAMissingRowAsEveryone.
func setFriendRequestMode(t *testing.T, db *sql.DB, userID uuid.UUID, mode string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO privacy_settings (user_id, allow_friend_requests_from) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET allow_friend_requests_from = EXCLUDED.allow_friend_requests_from`,
		userID, mode,
	)
	require.NoError(t, err)
}

// TestCanReceiveFriendRequestFromTruthTable is spec T1: the full
// {shares a server, does not} x {everyone, mutual_servers, nobody} matrix.
//
// Both axes are asserted in ONE table rather than split into two tests because
// mutual_servers is the only cell where they interact. A per-axis split would
// let a helper that ignored the join entirely still pass the everyone/nobody
// rows.
func TestCanReceiveFriendRequestFromTruthTable(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(db, logger.New("test"), nil)
	ctx := context.Background()

	cases := []struct {
		name         string
		sharesServer bool
		mode         string
		want         bool
	}{
		{"shares a server, everyone", true, modeEveryone, true},
		{"shares a server, mutual_servers", true, modeMutualServers, true},
		{"shares a server, nobody", true, modeNobody, false},
		{"shares no server, everyone", false, modeEveryone, true},
		{"shares no server, mutual_servers", false, modeMutualServers, false},
		{"shares no server, nobody", false, modeNobody, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh principals per cell: a shared pair would let one cell's
			// server membership leak into the next.
			target := dbtest.CreateUser(t, db)
			requester := dbtest.CreateUser(t, db)

			if tc.sharesServer {
				createServerWithMembers(t, db, target, requester)
			} else {
				// Both are in a server, just not the SAME one. If neither had
				// any membership at all, a helper whose join had lost its
				// sm1.server_id = sm2.server_id predicate would still pass.
				createServerWithMembers(t, db, target)
				createServerWithMembers(t, db, requester)
			}
			setFriendRequestMode(t, db, target, tc.mode)

			got, err := h.canReceiveFriendRequestFrom(ctx, target.String(), requester.String())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got,
				"shares=%v mode=%q must yield eligible=%v", tc.sharesServer, tc.mode, tc.want)
		})
	}
}

// TestCanReceiveFriendRequestFromIsDirectional pins the parameter ORDER.
//
// Both parameters are strings, so swapping targetID and requesterID at a call
// site compiles. The shared-server join is symmetric and would not notice, but
// the enum lookup is anchored to $1 — so a swap silently enforces the
// REQUESTER's own preference against themselves. With the requester on
// 'nobody' and the target on 'everyone', the correct answer is true and the
// swapped answer is false.
func TestCanReceiveFriendRequestFromIsDirectional(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(db, logger.New("test"), nil)

	target := dbtest.CreateUser(t, db)
	requester := dbtest.CreateUser(t, db)
	createServerWithMembers(t, db, target, requester)
	setFriendRequestMode(t, db, target, modeEveryone)
	setFriendRequestMode(t, db, requester, modeNobody)

	got, err := h.canReceiveFriendRequestFrom(
		context.Background(), target.String(), requester.String())
	require.NoError(t, err)
	assert.True(t, got,
		"the gate reads the TARGET's preference; the requester's own 'nobody' is irrelevant. "+
			"A false here means the two arguments are transposed somewhere")
}

// TestCanReceiveFriendRequestFromTreatsAMissingRowAsEveryone is spec T2, and it
// is the #1354 trap in its exact original shape: privacy_settings rows are
// created lazily, so "no row" is the MAJORITY state, not an edge case.
//
// If the LEFT JOIN became an inner join, this query would return
// sql.ErrNoRows for every user who has never PATCHed their settings — and the
// SendRequest caller maps ErrNoRows to 404, so the visible failure would be
// "User not found" for most of the user base. If the COALESCE were dropped,
// the comparison would go NULL and the scan would fail.
//
// Both axes are covered because the two CASE arms COALESCE separately:
// dropping it from only one arm breaks only one of these subtests.
func TestCanReceiveFriendRequestFromTreatsAMissingRowAsEveryone(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(db, logger.New("test"), nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		sharesServer bool
	}{
		{"shares a server", true},
		{"shares no server", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := dbtest.CreateUser(t, db)
			requester := dbtest.CreateUser(t, db)
			if tc.sharesServer {
				createServerWithMembers(t, db, target, requester)
			}

			// Asserted, not assumed: if some future migration or fixture starts
			// eagerly creating the row, this test would silently stop covering
			// the row-less path while still passing.
			var rows int
			require.NoError(t, db.QueryRow(
				`SELECT count(*) FROM privacy_settings WHERE user_id = $1`, target).Scan(&rows))
			require.Zero(t, rows, "precondition: the target must have NO privacy_settings row")

			got, err := h.canReceiveFriendRequestFrom(ctx, target.String(), requester.String())
			require.NoError(t, err)
			assert.True(t, got,
				"a target with no privacy_settings row must behave as 'everyone' via "+
					"LEFT JOIN + COALESCE — not 404, and not a NULL scan error")
		})
	}
}

// TestCanReceiveFriendRequestFromReturnsErrNoRowsForAnUnknownTarget is spec T3.
//
// It pins the ERROR IDENTITY, not merely "an error": both call sites branch on
// errors.Is(err, sql.ErrNoRows) to emit 404 and fall through to 500 otherwise.
// A helper that wrapped the error, or substituted a sentinel of its own, would
// turn every mistyped user id into a 500.
func TestCanReceiveFriendRequestFromReturnsErrNoRowsForAnUnknownTarget(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(db, logger.New("test"), nil)

	requester := dbtest.CreateUser(t, db)
	unknown := uuid.New()

	got, err := h.canReceiveFriendRequestFrom(
		context.Background(), unknown.String(), requester.String())

	require.Error(t, err)
	assert.True(t, errors.Is(err, sql.ErrNoRows),
		"both call sites branch on errors.Is(err, sql.ErrNoRows) to answer 404; a wrapped "+
			"or substituted error turns an unknown user into a 500. got: %v", err)
	assert.False(t, got, "the boolean must be false on the error path, never a stale true")
}
