package websocket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// identity mints, once per user, the live signed identity a WS frame carries
// into its writes' family guard (#2272).
func (s *hubTestSetup) identity(t *testing.T, user uuid.UUID) loginfamily.AccessIdentity {
	t.Helper()
	if id, ok := s.identities[user]; ok {
		return id
	}
	id, ok := loginfamily.AccessIdentityFromContext(dbtest.ActorContext(context.Background(), t, s.db, user.String()))
	require.True(t, ok)
	if s.identities == nil {
		s.identities = map[uuid.UUID]loginfamily.AccessIdentity{}
	}
	s.identities[user] = id
	return id
}

// live stamps msg as readPump would for its sender's live session.
func (s *hubTestSetup) live(t *testing.T, msg IncomingMessage) IncomingMessage {
	t.Helper()
	id := s.identity(t, msg.UserID)
	msg.SID, msg.CredEpoch, msg.TokenExpiresAt = id.SID, id.CredentialEpoch, id.AccessExpiresAt
	return msg
}

// terminate commits a revocation of the user's live frame family, as a session
// revoke that won the user lock after the hub admitted the frame would.
func (s *hubTestSetup) terminate(t *testing.T, user uuid.UUID) {
	t.Helper()
	id := s.identity(t, user)
	_, err := s.db.Exec(`UPDATE login_families f SET terminal_at=NOW(), epoch_generation=$3
		FROM login_family_sids s WHERE s.user_id=$1 AND s.sid=$2 AND f.user_id=s.user_id AND f.family_id=s.family_id`,
		user, id.SID, uuid.New())
	require.NoError(t, err)
}

// Codex #4135 P1: a channel frame that passed the hub's terminal check must
// still refuse to persist once its family is terminal.
func TestPersistMessage_RevokedFamilyWritesNoCiphertext(t *testing.T) {
	setup := setupMessageTest(t)
	channelID := uuid.MustParse(setup.convID)
	incarnation := membershipIncarnation(t, setup.db, setup.user2, setup.user1)
	identity := setup.identity(t, setup.user1)
	setup.terminate(t, setup.user1)

	_, _, _, _, errMsg, _ := setup.hub.persistMessage(persistMessageParams{
		channelUUID: channelID, userID: setup.user1, identity: identity, membershipIncarnation: incarnation,
		content: "Y2lwaGVydGV4dA==", keyVersion: 1,
	})
	require.Equal(t, "Authentication required", errMsg)
	var count int
	require.NoError(t, setup.db.QueryRow(`SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&count))
	require.Zero(t, count)
}

// The DM path carries the same family fence.
func TestPersistDMMessage_RevokedFamilyWritesNoCiphertext(t *testing.T) {
	setup := setupEpochTest(t, true, false)
	input := &dmMessageInput{content: "Y2lwaGVydGV4dA==", keyVersion: 1, msgType: "user", identity: setup.identity(t, setup.user1)}
	setup.terminate(t, setup.user1)

	_, _, _, _, err := setup.hub.persistDMMessage(uuid.MustParse(setup.convID), setup.user1, "", input)
	require.ErrorIs(t, err, credepoch.ErrEpochMismatch)
	var count int
	require.NoError(t, setup.db.QueryRow(`SELECT count(*) FROM dm_messages WHERE conversation_id = $1`, setup.convID).Scan(&count))
	require.Zero(t, count)
}
