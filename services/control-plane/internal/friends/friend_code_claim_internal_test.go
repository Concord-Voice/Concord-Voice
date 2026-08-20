package friends

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// This test is in package friends, which cannot import internal/testhelpers:
// that package builds the whole router and therefore depends on this one. It
// uses internal/testhelpers/testdb instead, which has zero internal
// dependencies.

// claimResult.codeID is the only carrier of the redeemed row's id between the
// locked read and the friend_code_claimed broadcast, and no other test asserts
// it: every capture/gate test builds the handler with a nil hub, so
// notifyFriendCodeClaimed returns at its first line without reading the field,
// and the ClaimFriendCode HTTP body carries no code_id at all.
//
// The consequence of losing it is silent and user-visible. The desktop schema
// types the field code_id: UUID.optional()
// (client/desktop/src/renderer/types/ws-events.ts), and an empty string is not
// undefined — it fails UUID validation, so the ENTIRE friend_code_claimed event
// is rejected at the dispatch boundary and the code owner never learns their
// code was redeemed.
//
// The capture assertions ride along because executeFriendCodeClaim hand-builds
// its own presencehook.Spec literal, independent of the one ClaimFriendCode
// builds for the gate. That pair is the weakest of the four #2446 sites — see
// the note above the orientation subtests in presence_capture_db_test.go — so
// this pins the capture literal's family, posture and orientation absolutely
// rather than only against its counterpart.
func TestExecuteFriendCodeClaimCarriesTheConsumedCodeID(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	ctx := context.Background()

	owner := dbtest.CreateUser(t, db)
	claimer := dbtest.CreateUser(t, db)

	var codeID string
	require.NoError(t, db.QueryRow(
		`INSERT INTO friend_codes (user_id, code, auto_accept) VALUES ($1, $2, TRUE) RETURNING id`,
		owner, "CLAIMIDA",
	).Scan(&codeID))

	h := NewHandler(db, logger.New("test"), nil)
	capture := &recordingCapture{}

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	claim, err := h.executeFriendCodeClaim(
		ctx, capture, tx, claimer.String(), owner.String(), codeID, true,
	)
	require.NoError(t, err)

	assert.Equal(t, codeID, claim.codeID,
		"the notification tail's only source for code_id is this field; an empty one "+
			"ships \"code_id\": \"\" and the client rejects the whole broadcast")
	assert.Equal(t, "accepted", claim.status)
	assert.NotEmpty(t, claim.friendshipID)

	// The row the returned id names must be the row this claim consumed, which
	// an id copied from anywhere else would not be.
	var useCount int
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT use_count FROM friend_codes WHERE id = $1`, claim.codeID).Scan(&useCount))
	assert.Equal(t, 1, useCount)

	require.Len(t, capture.subjects, 1)
	assert.Equal(t, presencecapture.FamilyFriendshipAccept, capture.subjects[0].Family)
	assert.Equal(t, presencecapture.FailClosedBlockWrite, capture.subjects[0].FailPosture)
	assert.Equal(t, claimer, capture.subjects[0].Principal)
	assert.Equal(t, owner, capture.subjects[0].Counterpart)
}

// A non-auto-accepting claim writes a 'pending' row and takes no capture, but it
// consumes a use of the code exactly as an accepting one does — so the owner's
// friend_code_claimed broadcast still needs the id.
func TestExecuteFriendCodeClaimCarriesTheCodeIDWithoutAutoAccept(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	ctx := context.Background()

	owner := dbtest.CreateUser(t, db)
	claimer := dbtest.CreateUser(t, db)

	var codeID string
	require.NoError(t, db.QueryRow(
		`INSERT INTO friend_codes (user_id, code, auto_accept) VALUES ($1, $2, FALSE) RETURNING id`,
		owner, "CLAIMIDP",
	).Scan(&codeID))

	h := NewHandler(db, logger.New("test"), nil)
	capture := &recordingCapture{}

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	claim, err := h.executeFriendCodeClaim(
		ctx, capture, tx, claimer.String(), owner.String(), codeID, false,
	)
	require.NoError(t, err)

	assert.Equal(t, codeID, claim.codeID)
	assert.Equal(t, "pending", claim.status)
	assert.Empty(t, capture.subjects, "a pending row confers no FoF visibility, so it captures nothing")
	assert.Nil(t, claim.plan)
}
