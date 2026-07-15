package presence

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type serverVoiceState struct {
	channelName string
	serverName  string
}

func loadServerVoiceState(
	ctx context.Context,
	db DBTX,
	senderID uuid.UUID,
	input ServerVoicePolicyInput,
) (serverVoiceState, error) {
	var state serverVoiceState
	err := db.QueryRowContext(ctx, `
		SELECT c.name, s.name
		FROM channels c
		JOIN servers s ON s.id = c.server_id
		JOIN server_members sm ON sm.server_id = c.server_id AND sm.user_id = $3
		JOIN voice_participants vp ON vp.channel_id = c.id AND vp.user_id = $3
		WHERE c.server_id = $1 AND c.id = $2 AND c.type = 'voice'
	`, input.Context.ServerID, input.Context.ChannelID, senderID).Scan(
		&state.channelName,
		&state.serverName,
	)
	if err != nil {
		return serverVoiceState{}, fmt.Errorf("read server voice state: %w", err)
	}
	if state.channelName != input.Payload.ChannelName || state.serverName != input.Payload.ServerName {
		return serverVoiceState{}, errors.New("stale server voice payload")
	}
	return state, nil
}

type privateCallState struct {
	callType     string
	participants map[uuid.UUID]bool
}

func loadPrivateCallState(
	ctx context.Context,
	db DBTX,
	senderID uuid.UUID,
	input PrivateCallPolicyInput,
) (result privateCallState, returnErr error) {
	var isGroup bool
	if err := db.QueryRowContext(ctx,
		`SELECT is_group FROM dm_conversations WHERE id = $1`,
		input.Context.ConversationID,
	).Scan(&isGroup); err != nil {
		return privateCallState{}, fmt.Errorf("read private call conversation: %w", err)
	}

	participants := make(map[uuid.UUID]bool)
	rows, err := db.QueryContext(ctx, `
		SELECT vp.user_id, dp.user_id IS NOT NULL
		FROM dm_voice_participants vp
		LEFT JOIN dm_participants dp
		  ON dp.conversation_id = vp.conversation_id AND dp.user_id = vp.user_id
		WHERE vp.conversation_id = $1
	`, input.Context.ConversationID)
	if err != nil {
		return privateCallState{}, fmt.Errorf("query private call participants: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = privateCallState{}
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("close private call participant rows: %w", closeErr),
			)
		}
	}()
	for rows.Next() {
		var (
			participantID uuid.UUID
			isMember      bool
		)
		if err := rows.Scan(&participantID, &isMember); err != nil {
			return privateCallState{}, fmt.Errorf("scan private call participant row: %w", err)
		}
		if !isMember {
			return privateCallState{}, errors.New("private call participant is not a conversation member")
		}
		participants[participantID] = true
	}
	if err := rows.Err(); err != nil {
		return privateCallState{}, fmt.Errorf("iterate private call participant rows: %w", err)
	}
	if !participants[senderID] {
		return privateCallState{}, errors.New("sender is not in current private call")
	}

	expectedCallType := "dm"
	if isGroup {
		expectedCallType = "group"
	}
	if input.Payload.CallType != expectedCallType ||
		input.Payload.ParticipantCount != len(participants) ||
		!sameParticipantSet(participants, input.Context.ParticipantIDs) {
		return privateCallState{}, errors.New("stale private call context")
	}

	return privateCallState{
		callType:     expectedCallType,
		participants: participants,
	}, nil
}

func sameParticipantSet(current map[uuid.UUID]bool, claimed []uuid.UUID) bool {
	if len(current) != len(claimed) {
		return false
	}
	for id, included := range current {
		if id == uuid.Nil || !included {
			return false
		}
	}

	seen := make(map[uuid.UUID]bool, len(claimed))
	for _, id := range claimed {
		if id == uuid.Nil || seen[id] || !current[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
