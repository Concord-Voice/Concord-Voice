package presence_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type visibilityCall struct {
	serverID     string
	channelID    string
	candidateIDs []string
}

type visibilityStub struct {
	visibleByUser map[string][]string
	defaultIDs    []string
	extraIDs      []string
	failAfter     int
	calls         []visibilityCall
}

func (stub *visibilityStub) FilterVisibleUserIDsForChannelFresh(
	_ context.Context,
	serverID string,
	channelID string,
	candidateIDs []string,
) ([]string, error) {
	stub.calls = append(stub.calls, visibilityCall{
		serverID:     serverID,
		channelID:    channelID,
		candidateIDs: append([]string(nil), candidateIDs...),
	})
	if stub.failAfter > 0 && len(stub.calls) >= stub.failAfter {
		return nil, errors.New("forced visibility failure")
	}

	visible := make([]string, 0, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		ids := stub.defaultIDs
		if perUser, ok := stub.visibleByUser[candidateID]; ok {
			ids = perUser
		}
		if containsVisibilityID(ids, channelID) {
			visible = append(visible, candidateID)
		}
	}
	return append(visible, stub.extraIDs...), nil
}

func containsVisibilityID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// permitAllPresence permits every sender, matching the pre-#2444 behavior the
// existing cases in this file assert: base presence was not a policy input.
type permitAllPresence struct{}

func (permitAllPresence) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

// stubSenderPresence counts calls so a case can prove the gate ran exactly once.
type stubSenderPresence struct {
	permitted bool
	calls     int
}

func (s *stubSenderPresence) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	s.calls++
	return s.permitted
}

// failDB fails every read, proving the presence gate short-circuits before any
// settings read.
type failDB struct{}

func (failDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("db must not be queried on the suppressed path")
}

func (failDB) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("db must not be queried on the suppressed path")
}

type failingQueryDB struct {
	presence.DBTX
	contains string
}

func (db failingQueryDB) QueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	if strings.Contains(query, db.contains) {
		return nil, errors.New("forced query failure")
	}
	return db.DBTX.QueryContext(ctx, query, args...)
}

type substituteRowDB struct {
	presence.DBTX
	contains string
	query    string
}

type substituteQueryDB struct {
	presence.DBTX
	contains string
	query    string
}

func (db substituteQueryDB) QueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	if strings.Contains(query, db.contains) {
		return db.DBTX.QueryContext(ctx, db.query)
	}
	return db.DBTX.QueryContext(ctx, query, args...)
}

func (db substituteRowDB) QueryRowContext(
	ctx context.Context,
	query string,
	args ...any,
) *sql.Row {
	if strings.Contains(query, db.contains) {
		return db.DBTX.QueryRowContext(ctx, db.query)
	}
	return db.DBTX.QueryRowContext(ctx, query, args...)
}

func setupPolicyDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	require.NoError(t, testhelpers.TruncateAllTables(db))
	return db, context.Background()
}

func setPolicySettings(
	t *testing.T,
	db *sql.DB,
	userID uuid.UUID,
	master bool,
	serverTier presence.Tier,
	serverDetails bool,
	privateTier presence.Tier,
	privateDetails bool,
) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, server_voice_tier, server_voice_show_details,
			private_call_tier, private_call_show_details
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET
			master_enabled = EXCLUDED.master_enabled,
			server_voice_tier = EXCLUDED.server_voice_tier,
			server_voice_show_details = EXCLUDED.server_voice_show_details,
			private_call_tier = EXCLUDED.private_call_tier,
			private_call_show_details = EXCLUDED.private_call_show_details
	`, userID, master, serverTier, serverDetails, privateTier, privateDetails)
	require.NoError(t, err)
}

func validServerVoicePolicyInput(senderID uuid.UUID) presence.PolicyInput {
	serverID := uuid.New()
	channelID := uuid.New()
	startedAt := int64(123)
	return presence.PolicyInput{
		SenderID: senderID,
		Category: presence.CategoryServerVoice,
		ServerVoice: &presence.ServerVoicePolicyInput{
			Context: presence.ServerVoiceContext{ServerID: serverID, ChannelID: channelID},
			Payload: presence.ServerVoicePayload{
				ServerID: serverID, ChannelID: channelID,
				ServerName: "Server", ChannelName: "General", StartedAt: &startedAt,
			},
		},
	}
}

func validPrivateCallPolicyInput(senderID uuid.UUID) presence.PolicyInput {
	startedAt := int64(123)
	return presence.PolicyInput{
		SenderID: senderID,
		Category: presence.CategoryPrivateCall,
		PrivateCall: &presence.PrivateCallPolicyInput{
			Context: presence.PrivateCallContext{
				ConversationID: uuid.New(),
				ParticipantIDs: []uuid.UUID{senderID},
			},
			Payload: presence.PrivateCallPayload{
				CallType: "dm", ParticipantCount: 1, StartedAt: &startedAt,
			},
		},
	}
}

func createServerVoicePolicyFixture(
	t *testing.T,
	db *sql.DB,
	senderID uuid.UUID,
	members ...uuid.UUID,
) presence.PolicyInput {
	t.Helper()
	serverID := testhelpers.CreateServer(t, db, senderID)
	testhelpers.AddServerMember(t, db, serverID, senderID)
	for _, memberID := range members {
		testhelpers.AddServerMember(t, db, serverID, memberID)
	}
	channelID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'General', 'voice')`,
		channelID, serverID,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		channelID, senderID,
	)
	require.NoError(t, err)
	var serverName string
	require.NoError(t, db.QueryRow(`SELECT name FROM servers WHERE id = $1`, serverID).Scan(&serverName))
	startedAt := int64(123)
	return presence.PolicyInput{
		SenderID: senderID,
		Category: presence.CategoryServerVoice,
		ServerVoice: &presence.ServerVoicePolicyInput{
			Context: presence.ServerVoiceContext{ServerID: serverID, ChannelID: channelID},
			Payload: presence.ServerVoicePayload{
				ServerID: serverID, ChannelID: channelID,
				ServerName: serverName, ChannelName: "General", StartedAt: &startedAt,
			},
		},
	}
}

func createPrivateCallPolicyFixture(
	t *testing.T,
	db *sql.DB,
	isGroup bool,
	participants ...uuid.UUID,
) presence.PolicyInput {
	t.Helper()
	require.NotEmpty(t, participants)
	conversationID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, $2, $3)`,
		conversationID, isGroup, participants[0],
	)
	require.NoError(t, err)
	for _, participantID := range participants {
		_, err = db.Exec(
			`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`,
			conversationID, participantID,
		)
		require.NoError(t, err)
		_, err = db.Exec(
			`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`,
			conversationID, participantID,
		)
		require.NoError(t, err)
	}
	callType := "dm"
	if isGroup {
		callType = "group"
	}
	startedAt := int64(123)
	return presence.PolicyInput{
		SenderID: participants[0],
		Category: presence.CategoryPrivateCall,
		PrivateCall: &presence.PrivateCallPolicyInput{
			Context: presence.PrivateCallContext{
				ConversationID: conversationID,
				ParticipantIDs: append([]uuid.UUID(nil), participants...),
			},
			Payload: presence.PrivateCallPayload{
				CallType: callType, ParticipantCount: len(participants), StartedAt: &startedAt,
			},
		},
	}
}

func requireZeroPolicyError(t *testing.T, decision presence.Decision, err error, class presence.FailureClass) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, presence.Decision{}, decision)
	require.False(t, decision.Minimized)
	require.Equal(t, class, presence.PolicyErrorClass(err))
	require.Equal(t, "rich-presence policy failed: "+string(class), err.Error())
}

func requireEmptyDecision(t *testing.T, decision presence.Decision, err error) {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, decision.Audience)
	require.Empty(t, decision.Audience)
	require.Nil(t, decision.Payload)
	require.False(t, decision.Minimized)
}

func TestAuthorizeAndMinimize_SuppressedSenderSkipsSettingsRead(t *testing.T) {
	resolver := &stubSenderPresence{permitted: false}

	decision, err := presence.AuthorizeAndMinimize(
		context.Background(), failDB{}, nil, resolver, validServerVoicePolicyInput(uuid.New()),
	)

	require.NoError(t, err)
	require.True(t, decision.SuppressedBySenderPresence)
	require.NotNil(t, decision.Audience)
	require.Empty(t, decision.Audience)
	require.Nil(t, decision.Payload)
	require.False(t, decision.Minimized)
	require.Equal(t, 1, resolver.calls)
}

func TestAuthorizeAndMinimize_MissingSenderPresenceResolverFailsClosed(t *testing.T) {
	decision, err := presence.AuthorizeAndMinimize(
		context.Background(), failDB{}, nil, nil, validPrivateCallPolicyInput(uuid.New()),
	)

	require.NoError(t, err)
	require.True(t, decision.SuppressedBySenderPresence)
	require.Empty(t, decision.Audience)
	require.Nil(t, decision.Payload)
}

func TestAuthorizeAndMinimize_PermittedSenderPresenceLeavesDecisionUnmarked(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	input := createServerVoicePolicyFixture(t, db, senderID, viewerID)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	setPolicySettings(t, db, senderID, true, presence.TierFriends, true, presence.TierOff, false)
	visibility := &visibilityStub{defaultIDs: []string{input.ServerVoice.Context.ChannelID.String()}}
	resolver := &stubSenderPresence{permitted: true}

	decision, err := presence.AuthorizeAndMinimize(ctx, db, visibility, resolver, input)

	require.NoError(t, err)
	require.False(t, decision.SuppressedBySenderPresence)
	require.Equal(t, map[uuid.UUID]bool{viewerID: true}, decision.Audience)
	require.Equal(t, 1, resolver.calls)
}

func TestAuthorizeAndMinimize_InvalidInputPrecedesSenderPresenceGate(t *testing.T) {
	resolver := &stubSenderPresence{permitted: true}

	decision, err := presence.AuthorizeAndMinimize(
		context.Background(), failDB{}, nil, resolver, presence.PolicyInput{},
	)

	requireZeroPolicyError(t, decision, err, presence.FailureInvalidInput)
	require.Zero(t, resolver.calls, "invalid input must be rejected before the presence gate")
}

func TestAuthorizeAndMinimize_InvalidInputReturnsZeroDecision(t *testing.T) {
	senderID := uuid.New()
	zeroStartedAt := int64(0)
	unsafeStartedAt := presence.MaxActivityUnixSeconds + 1

	tests := []struct {
		name  string
		input presence.PolicyInput
	}{
		{name: "unknown category", input: presence.PolicyInput{SenderID: senderID, Category: "unsupported"}},
		{name: "zero sender", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.SenderID = uuid.Nil
			return input
		}()},
		{name: "missing category context", input: presence.PolicyInput{
			SenderID: senderID, Category: presence.CategoryServerVoice,
		}},
		{name: "zero server context", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Context.ServerID = uuid.Nil
			input.ServerVoice.Payload.ServerID = uuid.Nil
			return input
		}()},
		{name: "zero channel context", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Context.ChannelID = uuid.Nil
			input.ServerVoice.Payload.ChannelID = uuid.Nil
			return input
		}()},
		{name: "category mismatched pointer", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.Category = presence.CategoryPrivateCall
			return input
		}()},
		{name: "extra category context", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.PrivateCall = validPrivateCallPolicyInput(senderID).PrivateCall
			return input
		}()},
		{name: "server id mismatch", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Payload.ServerID = uuid.New()
			return input
		}()},
		{name: "channel id mismatch", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Payload.ChannelID = uuid.New()
			return input
		}()},
		{name: "empty server name", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Payload.ServerName = ""
			return input
		}()},
		{name: "oversized channel name", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Payload.ChannelName = strings.Repeat("x", 101)
			return input
		}()},
		{name: "nonpositive server timestamp", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Payload.StartedAt = &zeroStartedAt
			return input
		}()},
		{name: "precision-unsafe server timestamp", input: func() presence.PolicyInput {
			input := validServerVoicePolicyInput(senderID)
			input.ServerVoice.Payload.StartedAt = &unsafeStartedAt
			return input
		}()},
		{name: "zero conversation", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Context.ConversationID = uuid.Nil
			return input
		}()},
		{name: "invalid call type", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Payload.CallType = "conference"
			return input
		}()},
		{name: "invalid participant count", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Payload.ParticipantCount = 0
			return input
		}()},
		{name: "oversized participant count", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Payload.ParticipantCount = 256
			return input
		}()},
		{name: "empty participant list", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Context.ParticipantIDs = nil
			return input
		}()},
		{name: "nil participant", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Context.ParticipantIDs = []uuid.UUID{uuid.Nil}
			return input
		}()},
		{name: "duplicate participant", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Context.ParticipantIDs = []uuid.UUID{senderID, senderID}
			return input
		}()},
		{name: "oversized participant list", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Context.ParticipantIDs = make([]uuid.UUID, 256)
			for index := range input.PrivateCall.Context.ParticipantIDs {
				input.PrivateCall.Context.ParticipantIDs[index] = uuid.New()
			}
			return input
		}()},
		{name: "nonpositive private timestamp", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Payload.StartedAt = &zeroStartedAt
			return input
		}()},
		{name: "precision-unsafe private timestamp", input: func() presence.PolicyInput {
			input := validPrivateCallPolicyInput(senderID)
			input.PrivateCall.Payload.StartedAt = &unsafeStartedAt
			return input
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := presence.AuthorizeAndMinimize(context.Background(), nil, nil, permitAllPresence{}, test.input)
			requireZeroPolicyError(t, decision, err, presence.FailureInvalidInput)
		})
	}
}

func TestAuthorizeAndMinimize_MasterOffSuppressesBothCategories(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	setPolicySettings(t, db, senderID, false, presence.TierServers, true, presence.TierServers, true)

	for _, input := range []presence.PolicyInput{
		validServerVoicePolicyInput(senderID),
		validPrivateCallPolicyInput(senderID),
	} {
		decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
		requireEmptyDecision(t, decision, err)
	}
}

func TestAuthorizeAndMinimize_ServerVoiceTierOffReturnsNoPayload(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	setPolicySettings(t, db, senderID, true, presence.TierOff, true, presence.TierServers, true)

	decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, validServerVoicePolicyInput(senderID))
	requireEmptyDecision(t, decision, err)
}

func TestAuthorizeAndMinimize_ServerVoiceFriendsIntersectsServerAndFoFOptIn(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	friendInServer := testhelpers.CreateUser(t, db)
	friendOutsideServer := testhelpers.CreateUser(t, db)
	middleFriend := testhelpers.CreateUser(t, db)
	friendOfFriendInServer := testhelpers.CreateUser(t, db)
	unrelatedMember := testhelpers.CreateUser(t, db)
	input := createServerVoicePolicyFixture(
		t, db, senderID, friendInServer, friendOfFriendInServer, unrelatedMember,
	)
	testhelpers.AddFriendship(t, db, senderID, friendInServer)
	testhelpers.AddFriendship(t, db, senderID, friendOutsideServer)
	testhelpers.AddFriendship(t, db, senderID, middleFriend)
	testhelpers.AddFriendship(t, db, middleFriend, friendOfFriendInServer)
	testhelpers.SetFriendsOfFriends(t, db, senderID, true)
	setPolicySettings(t, db, senderID, true, presence.TierFriends, true, presence.TierOff, false)
	visibility := &visibilityStub{defaultIDs: []string{input.ServerVoice.Context.ChannelID.String()}}

	decision, err := presence.AuthorizeAndMinimize(ctx, db, visibility, permitAllPresence{}, input)
	require.NoError(t, err)
	require.False(t, decision.Minimized)
	require.Equal(t, map[uuid.UUID]bool{
		friendInServer:         true,
		friendOfFriendInServer: true,
	}, decision.Audience)
	require.Contains(t, string(decision.Payload), `"channel_name":"General"`)
	require.NotContains(t, decision.Audience, friendOutsideServer)
	require.NotContains(t, decision.Audience, unrelatedMember)

	testhelpers.SetFriendsOfFriends(t, db, senderID, false)
	decision, err = presence.AuthorizeAndMinimize(ctx, db, visibility, permitAllPresence{}, input)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]bool{friendInServer: true}, decision.Audience)
}

func TestAuthorizeAndMinimize_ServerVoiceServersUsesExactMembersAndVisibility(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	visibleMember := testhelpers.CreateUser(t, db)
	hiddenMembers := make([]uuid.UUID, 0, 8)
	for i := 0; i < 8; i++ {
		hiddenMembers = append(hiddenMembers, testhelpers.CreateUser(t, db))
	}
	outsideFriend := testhelpers.CreateUser(t, db)
	serverMembers := append([]uuid.UUID{visibleMember}, hiddenMembers...)
	input := createServerVoicePolicyFixture(t, db, senderID, serverMembers...)
	testhelpers.AddFriendship(t, db, senderID, outsideFriend)
	setPolicySettings(t, db, senderID, true, presence.TierServers, true, presence.TierOff, false)
	visibility := &visibilityStub{visibleByUser: map[string][]string{
		visibleMember.String(): {input.ServerVoice.Context.ChannelID.String()},
	}, extraIDs: []string{senderID.String(), outsideFriend.String(), uuid.NewString(), visibleMember.String()}}

	decision, err := presence.AuthorizeAndMinimize(ctx, db, visibility, permitAllPresence{}, input)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]bool{visibleMember: true}, decision.Audience)
	require.NotContains(t, decision.Audience, senderID)
	require.NotContains(t, decision.Audience, outsideFriend)
	require.Len(t, visibility.calls, 1, "candidate growth must not increase resolver calls")
	call := visibility.calls[0]
	require.Equal(t, input.ServerVoice.Context.ServerID.String(), call.serverID)
	require.Equal(t, input.ServerVoice.Context.ChannelID.String(), call.channelID)
	require.Len(t, call.candidateIDs, len(serverMembers))
	calledUsers := make(map[string]bool, len(call.candidateIDs))
	for _, candidateID := range call.candidateIDs {
		calledUsers[candidateID] = true
	}
	for _, serverMember := range serverMembers {
		require.True(t, calledUsers[serverMember.String()])
	}
	require.False(t, calledUsers[outsideFriend.String()])

	t.Run("all hidden returns no payload", func(t *testing.T) {
		decision, err := presence.AuthorizeAndMinimize(ctx, db, &visibilityStub{}, permitAllPresence{}, input)
		requireEmptyDecision(t, decision, err)
	})

	t.Run("nil resolver fails closed", func(t *testing.T) {
		decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAuthorizationRead)
	})
}

func TestAuthorizeAndMinimize_ServerVoiceRejectsChannelServerTypeAndJoinMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *sql.DB, *presence.PolicyInput)
	}{
		{name: "server mismatch", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput) {
			otherServerID := uuid.New()
			input.ServerVoice.Context.ServerID = otherServerID
			input.ServerVoice.Payload.ServerID = otherServerID
		}},
		{name: "non voice channel", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput) {
			_, err := db.Exec(`UPDATE channels SET type = 'text' WHERE id = $1`, input.ServerVoice.Context.ChannelID)
			require.NoError(t, err)
		}},
		{name: "sender not joined", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput) {
			_, err := db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1`, input.ServerVoice.Context.ChannelID)
			require.NoError(t, err)
		}},
		{name: "sender no longer a server member", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput) {
			_, err := db.Exec(
				`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`,
				input.ServerVoice.Context.ServerID, input.SenderID,
			)
			require.NoError(t, err)
		}},
		{name: "channel name mismatch", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput) {
			input.ServerVoice.Payload.ChannelName = "Stale"
		}},
		{name: "server name mismatch", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput) {
			input.ServerVoice.Payload.ServerName = "Stale"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, ctx := setupPolicyDB(t)
			senderID := testhelpers.CreateUser(t, db)
			input := createServerVoicePolicyFixture(t, db, senderID)
			test.mutate(t, db, &input)

			decision, err := presence.AuthorizeAndMinimize(ctx, db, &visibilityStub{}, permitAllPresence{}, input)
			requireZeroPolicyError(t, decision, err, presence.FailureStateRead)
		})
	}
}

func TestAuthorizeAndMinimize_ServerVoiceVisibilityErrorDiscardsWholeDecision(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	memberA := testhelpers.CreateUser(t, db)
	memberB := testhelpers.CreateUser(t, db)
	input := createServerVoicePolicyFixture(t, db, senderID, memberA, memberB)
	setPolicySettings(t, db, senderID, true, presence.TierServers, true, presence.TierOff, false)
	visibility := &visibilityStub{
		defaultIDs: []string{input.ServerVoice.Context.ChannelID.String()},
		failAfter:  1,
	}

	decision, err := presence.AuthorizeAndMinimize(ctx, db, visibility, permitAllPresence{}, input)
	requireZeroPolicyError(t, decision, err, presence.FailureAuthorizationRead)
	require.Len(t, visibility.calls, 1)
}

func TestAuthorizeAndMinimize_ServerVoiceDetailsOffOmitsGranularBytes(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	input := createServerVoicePolicyFixture(t, db, senderID, viewerID)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	setPolicySettings(t, db, senderID, true, presence.TierFriends, false, presence.TierOff, false)
	visibility := &visibilityStub{defaultIDs: []string{input.ServerVoice.Context.ChannelID.String()}}

	decision, err := presence.AuthorizeAndMinimize(ctx, db, visibility, permitAllPresence{}, input)
	require.NoError(t, err)
	require.True(t, decision.Minimized)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(decision.Payload, &payload))
	require.Equal(t, map[string]any{
		"channel_id": input.ServerVoice.Context.ChannelID.String(),
		"server_id":  input.ServerVoice.Context.ServerID.String(),
		"started_at": float64(123),
	}, payload)
	require.NotContains(t, string(decision.Payload), input.ServerVoice.Payload.ChannelName)
	require.NotContains(t, string(decision.Payload), input.ServerVoice.Payload.ServerName)
}

func TestAuthorizeAndMinimize_PrivateCallTierOffReturnsCurrentParticipantsOnly(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	currentParticipant := testhelpers.CreateUser(t, db)
	nonVoiceParticipant := testhelpers.CreateUser(t, db)
	input := createPrivateCallPolicyFixture(t, db, false, senderID, currentParticipant)
	_, err := db.Exec(
		`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`,
		input.PrivateCall.Context.ConversationID, nonVoiceParticipant,
	)
	require.NoError(t, err)
	input.PrivateCall.Context.ParticipantIDs = []uuid.UUID{currentParticipant, senderID}

	decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]bool{currentParticipant: true}, decision.Audience)
	require.NotContains(t, decision.Audience, nonVoiceParticipant)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(decision.Payload, &payload))
	require.Equal(t, map[string]any{"call_type": "dm", "started_at": float64(123)}, payload)
	require.NotContains(t, string(decision.Payload), currentParticipant.String())
	require.NotContains(t, string(decision.Payload), senderID.String())

	t.Run("sender only has no audience or payload", func(t *testing.T) {
		require.NoError(t, testhelpers.TruncateAllTables(db))
		onlySender := testhelpers.CreateUser(t, db)
		onlySenderInput := createPrivateCallPolicyFixture(t, db, false, onlySender)
		decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, onlySenderInput)
		requireEmptyDecision(t, decision, err)
	})
}

func TestAuthorizeAndMinimize_PrivateCallFriendsAndServersComposeExistingPredicates(t *testing.T) {
	db, ctx := setupPolicyDB(t)
	senderID := testhelpers.CreateUser(t, db)
	currentParticipant := testhelpers.CreateUser(t, db)
	friendID := testhelpers.CreateUser(t, db)
	friendOfFriendID := testhelpers.CreateUser(t, db)
	serverPeerID := testhelpers.CreateUser(t, db)
	input := createPrivateCallPolicyFixture(t, db, true, senderID, currentParticipant)
	testhelpers.AddFriendship(t, db, senderID, friendID)
	testhelpers.AddFriendship(t, db, friendID, friendOfFriendID)
	testhelpers.SetFriendsOfFriends(t, db, senderID, true)
	serverID := testhelpers.CreateServer(t, db, senderID)
	testhelpers.AddServerMember(t, db, serverID, senderID)
	testhelpers.AddServerMember(t, db, serverID, serverPeerID)
	setPolicySettings(t, db, senderID, true, presence.TierOff, false, presence.TierFriends, true)

	decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]bool{
		currentParticipant: true,
		friendID:           true,
		friendOfFriendID:   true,
	}, decision.Audience)
	require.NotContains(t, decision.Audience, serverPeerID)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(decision.Payload, &payload))
	require.Equal(t, map[string]any{
		"call_type":         "group",
		"participant_count": float64(2),
		"started_at":        float64(123),
	}, payload)
	for _, identity := range []uuid.UUID{senderID, currentParticipant, friendID, friendOfFriendID} {
		require.NotContains(t, string(decision.Payload), identity.String())
	}

	setPolicySettings(t, db, senderID, true, presence.TierOff, false, presence.TierServers, true)
	decision, err = presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]bool{
		currentParticipant: true,
		friendID:           true,
		friendOfFriendID:   true,
		serverPeerID:       true,
	}, decision.Audience)
}

func TestAuthorizeAndMinimize_PrivateCallRejectsStaleParticipantSetTypeAndCount(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *sql.DB, *presence.PolicyInput, uuid.UUID)
	}{
		{name: "call type mismatch", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput, _ uuid.UUID) {
			input.PrivateCall.Payload.CallType = "group"
		}},
		{name: "participant count mismatch", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput, _ uuid.UUID) {
			input.PrivateCall.Payload.ParticipantCount = 1
		}},
		{name: "missing claimed participant", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput, _ uuid.UUID) {
			input.PrivateCall.Context.ParticipantIDs = input.PrivateCall.Context.ParticipantIDs[:1]
		}},
		{name: "extra claimed participant", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput, extraID uuid.UUID) {
			input.PrivateCall.Context.ParticipantIDs = append(input.PrivateCall.Context.ParticipantIDs, extraID)
		}},
		{name: "replaced claimed participant", mutate: func(_ *testing.T, _ *sql.DB, input *presence.PolicyInput, extraID uuid.UUID) {
			input.PrivateCall.Context.ParticipantIDs[1] = extraID
		}},
		{name: "sender absent from live call", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput, _ uuid.UUID) {
			_, err := db.Exec(
				`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`,
				input.PrivateCall.Context.ConversationID, input.SenderID,
			)
			require.NoError(t, err)
		}},
		{name: "sender no longer a conversation member", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput, _ uuid.UUID) {
			_, err := db.Exec(
				`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
				input.PrivateCall.Context.ConversationID, input.SenderID,
			)
			require.NoError(t, err)
		}},
		{name: "call participant no longer a conversation member", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput, _ uuid.UUID) {
			_, err := db.Exec(
				`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
				input.PrivateCall.Context.ConversationID, input.PrivateCall.Context.ParticipantIDs[1],
			)
			require.NoError(t, err)
		}},
		{name: "live call has extra participant", mutate: func(t *testing.T, db *sql.DB, input *presence.PolicyInput, extraID uuid.UUID) {
			_, err := db.Exec(
				`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`,
				input.PrivateCall.Context.ConversationID, extraID,
			)
			require.NoError(t, err)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, ctx := setupPolicyDB(t)
			senderID := testhelpers.CreateUser(t, db)
			participantID := testhelpers.CreateUser(t, db)
			extraID := testhelpers.CreateUser(t, db)
			input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
			test.mutate(t, db, &input, extraID)

			decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
			requireZeroPolicyError(t, decision, err, presence.FailureStateRead)
		})
	}
}

func TestAuthorizeAndMinimize_MissingSettingsUsesCategoryDefaults(t *testing.T) {
	t.Run("server voice defaults to friends with details", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		friendID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID, friendID)
		testhelpers.AddFriendship(t, db, senderID, friendID)
		visibility := &visibilityStub{defaultIDs: []string{input.ServerVoice.Context.ChannelID.String()}}

		decision, err := presence.AuthorizeAndMinimize(ctx, db, visibility, permitAllPresence{}, input)
		require.NoError(t, err)
		require.Equal(t, map[uuid.UUID]bool{friendID: true}, decision.Audience)
		require.Contains(t, string(decision.Payload), `"channel_name":"General"`)
		require.Contains(t, string(decision.Payload), `"server_name":`)
	})

	t.Run("private call defaults to participants with coarse payload", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)

		decision, err := presence.AuthorizeAndMinimize(ctx, db, nil, permitAllPresence{}, input)
		require.NoError(t, err)
		require.Equal(t, map[uuid.UUID]bool{participantID: true}, decision.Audience)
		require.NotContains(t, string(decision.Payload), "participant_count")
	})
}

func TestAuthorizeAndMinimize_InvalidStoredTierFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "server voice tier",
			query: `SELECT TRUE, 99::smallint, TRUE, 0::smallint, FALSE`,
		},
		{
			name:  "private call tier",
			query: `SELECT TRUE, 1::smallint, TRUE, 99::smallint, FALSE`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, ctx := setupPolicyDB(t)
			senderID := testhelpers.CreateUser(t, db)
			wrapped := substituteRowDB{
				DBTX: db, contains: "FROM user_presence_settings", query: test.query,
			}
			decision, err := presence.AuthorizeAndMinimize(
				ctx, wrapped, nil, permitAllPresence{}, validServerVoicePolicyInput(senderID),
			)
			requireZeroPolicyError(t, decision, err, presence.FailureSettingsRead)
		})
	}
}

func TestAuthorizeAndMinimize_ReadErrorsReturnZeroDecisionAndFixedClass(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		senderID := uuid.New()
		decision, err := presence.AuthorizeAndMinimize(
			context.Background(), nil, nil, permitAllPresence{}, validServerVoicePolicyInput(senderID),
		)
		requireZeroPolicyError(t, decision, err, presence.FailureSettingsRead)
	})

	t.Run("settings row", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		wrapped := substituteRowDB{
			DBTX: db, contains: "FROM user_presence_settings", query: `SELECT 1 / 0`,
		}
		decision, err := presence.AuthorizeAndMinimize(
			ctx, wrapped, nil, permitAllPresence{}, validServerVoicePolicyInput(senderID),
		)
		requireZeroPolicyError(t, decision, err, presence.FailureSettingsRead)
	})

	t.Run("server voice state row", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID)
		wrapped := substituteRowDB{DBTX: db, contains: "FROM channels c", query: `SELECT 1 / 0`}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, &visibilityStub{}, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureStateRead)
	})

	t.Run("private conversation row", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
		wrapped := substituteRowDB{DBTX: db, contains: "FROM dm_conversations", query: `SELECT 1 / 0`}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureStateRead)
	})

	t.Run("private participant query", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
		wrapped := failingQueryDB{DBTX: db, contains: "FROM dm_voice_participants"}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureStateRead)
	})

	t.Run("private participant row scan", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
		wrapped := substituteQueryDB{
			DBTX: db, contains: "FROM dm_voice_participants", query: `SELECT 'not-a-uuid'`,
		}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureStateRead)
		require.ErrorContains(t, errors.Unwrap(err), "scan private call participant row")
	})

	t.Run("active server member query", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		memberID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID, memberID)
		wrapped := failingQueryDB{DBTX: db, contains: "FROM server_members WHERE server_id"}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, &visibilityStub{}, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("friend query", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		memberID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID, memberID)
		wrapped := failingQueryDB{DBTX: db, contains: "FROM friendships"}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, &visibilityStub{}, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("friends of friends flag row", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		memberID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID, memberID)
		wrapped := substituteRowDB{DBTX: db, contains: "FROM privacy_settings", query: `SELECT 1 / 0`}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, &visibilityStub{}, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("friends of friends query", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		memberID := testhelpers.CreateUser(t, db)
		friendID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID, memberID)
		testhelpers.AddFriendship(t, db, senderID, friendID)
		testhelpers.SetFriendsOfFriends(t, db, senderID, true)
		wrapped := failingQueryDB{DBTX: db, contains: "WITH sender_friends"}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, &visibilityStub{}, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("private friend query", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
		setPolicySettings(t, db, senderID, true, presence.TierOff, false, presence.TierFriends, true)
		wrapped := failingQueryDB{DBTX: db, contains: "FROM friendships"}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("private friends of friends flag row", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
		setPolicySettings(t, db, senderID, true, presence.TierOff, false, presence.TierFriends, true)
		wrapped := substituteRowDB{DBTX: db, contains: "FROM privacy_settings", query: `SELECT 1 / 0`}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("shared server peer query", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		participantID := testhelpers.CreateUser(t, db)
		input := createPrivateCallPolicyFixture(t, db, false, senderID, participantID)
		setPolicySettings(t, db, senderID, true, presence.TierOff, false, presence.TierServers, true)
		wrapped := failingQueryDB{DBTX: db, contains: "JOIN server_members sm2"}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, nil, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})

	t.Run("uuid row scan", func(t *testing.T) {
		db, ctx := setupPolicyDB(t)
		senderID := testhelpers.CreateUser(t, db)
		memberID := testhelpers.CreateUser(t, db)
		input := createServerVoicePolicyFixture(t, db, senderID, memberID)
		wrapped := substituteQueryDB{
			DBTX: db, contains: "FROM server_members WHERE server_id", query: `SELECT 'not-a-uuid'`,
		}
		decision, err := presence.AuthorizeAndMinimize(ctx, wrapped, &visibilityStub{}, permitAllPresence{}, input)
		requireZeroPolicyError(t, decision, err, presence.FailureAudienceRead)
	})
}
