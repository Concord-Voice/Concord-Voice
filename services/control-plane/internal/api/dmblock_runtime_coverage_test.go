package api

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadVoiceEnforcementSessionsUsesStaticTargetsAndOffset(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("requires an explicitly configured isolated DATABASE_URL")
	}
	parsed, err := url.Parse(databaseURL)
	require.NoError(t, err)
	if !strings.HasSuffix(strings.TrimSuffix(path.Base(parsed.Path), "/"), "_test") {
		t.Skip("DATABASE_URL must name an isolated *_test database")
	}

	db, _ := dbtest.SetupTestDB(t)
	conversationID, otherConversationID := uuid.New(), uuid.New()
	userID, otherUserID := uuid.New(), uuid.New()
	epoch := strings.Repeat("a", 32)
	otherEpoch := strings.Repeat("b", 32)
	nodeID := uuid.New()

	insert := func(roomID, rowUser uuid.UUID, roomKind, rowEpoch string) uuid.UUID {
		sessionID := uuid.New()
		_, insertErr := db.Exec(`
			INSERT INTO voice_enforcement_sessions
				(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			sessionID, nodeID, roomID, roomKind, rowUser, rowEpoch, "socket-"+sessionID.String())
		require.NoError(t, insertErr)
		return sessionID
	}

	for range 101 {
		insert(conversationID, userID, "dm", "")
	}
	decoy := insert(otherConversationID, userID, "dm", "")

	dmTarget := voiceEnforcementSessionTarget{
		kind: voiceEnforcementDMBlockTarget, conversationID: conversationID.String(), userID: userID,
	}
	firstPage, err := loadVoiceEnforcementSessions(context.Background(), db, dmTarget, 0)
	require.NoError(t, err)
	require.Len(t, firstPage, voiceEnforcementSessionEjectionMaxFanout)
	secondPage, err := loadVoiceEnforcementSessions(context.Background(), db, dmTarget, 1)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	assert.NotEqual(t, decoy, secondPage[0].sessionGeneration)
	assert.Equal(t, conversationID, secondPage[0].roomID)
	assert.Equal(t, userID, secondPage[0].userID)

	credentialMatch := insert(otherConversationID, userID, "channel", epoch)
	insert(otherConversationID, otherUserID, "channel", epoch)
	insert(otherConversationID, userID, "channel", otherEpoch)
	credentialTarget := voiceEnforcementSessionTarget{
		kind: voiceEnforcementCredentialEpochTarget, userID: userID, credentialEpoch: epoch,
	}
	credentialRows, err := loadVoiceEnforcementSessions(context.Background(), db, credentialTarget, 1)
	require.NoError(t, err)
	require.Len(t, credentialRows, 1)
	assert.Equal(t, credentialMatch, credentialRows[0].sessionGeneration)
	assert.Equal(t, userID, credentialRows[0].userID)
	assert.Equal(t, epoch, credentialRows[0].credentialEpoch)
}

func TestPublishVoiceEnforcementSessionEjectionPropagatesRequesterError(t *testing.T) {
	requestErr := errors.New("target unavailable")
	requester := &voiceEnforcementRequesterStub{err: requestErr}
	row := voiceEnforcementSessionRow{
		sessionGeneration: uuid.New(), nodeBootID: uuid.New(), roomID: uuid.New(),
		roomKind: "channel", userID: uuid.New(), socketID: "socket-1",
	}

	err := publishVoiceEnforcementSessionEjection(context.Background(), requester, voiceEnforcementProtocolTestSecret, uuid.New(), row)

	assert.ErrorIs(t, err, requestErr)
}

func TestPublishCredentialEpochVoiceEjectionPropagatesRequesterError(t *testing.T) {
	requestErr := errors.New("target unavailable")
	requester := &dmBlockVoiceEjectionRequesterStub{err: requestErr}

	err := publishCredentialEpochVoiceEjection(
		context.Background(), requester, dmBlockVoiceEjectionTestSecret, uuid.New(),
		strings.Repeat("a", 32), strings.Repeat("b", 32),
	)

	assert.ErrorIs(t, err, requestErr)
}

func TestPublishVoiceEnforcementSessionRowsEmptyInputDoesNotPublish(t *testing.T) {
	called := false
	err := publishVoiceEnforcementSessionRows(context.Background(), nil, func(voiceEnforcementSessionRow) error {
		called = true
		return nil
	})

	require.NoError(t, err)
	assert.False(t, called)
}

func TestVoiceEnforcementRolloutActivatedRejectsNilDatabase(t *testing.T) {
	activated, err := voiceEnforcementRolloutActivated(context.Background(), nil)

	assert.False(t, activated)
	assert.EqualError(t, err, "voice enforcement database is unavailable")
}

func TestLoadVoiceEnforcementSessionsRejectsNilDatabase(t *testing.T) {
	sessions, err := loadVoiceEnforcementSessions(context.Background(), nil, voiceEnforcementSessionTarget{}, 0)

	assert.Nil(t, sessions)
	assert.EqualError(t, err, "voice enforcement database is unavailable")
}
