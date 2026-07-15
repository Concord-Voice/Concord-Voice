package presence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxPrivateCallParticipants = 255

// ChannelVisibilityResolver resolves the fresh viewers of one exact channel in
// one bulk operation.
type ChannelVisibilityResolver interface {
	FilterVisibleUserIDsForChannelFresh(
		ctx context.Context,
		serverID string,
		channelID string,
		candidateUserIDs []string,
	) ([]string, error)
}

type policySettings struct {
	master         bool
	serverTier     Tier
	serverDetails  bool
	privateTier    Tier
	privateDetails bool
}

type pendingPolicyDecision struct {
	audience map[uuid.UUID]bool
	payload  any
	details  bool
}

func defaultPolicySettings() policySettings {
	return policySettings{
		master:        true,
		serverTier:    TierFriends,
		serverDetails: true,
	}
}

func loadPolicySettings(ctx context.Context, db DBTX, senderID uuid.UUID) (policySettings, error) {
	out := defaultPolicySettings()
	err := db.QueryRowContext(ctx, `
		SELECT master_enabled, server_voice_tier, server_voice_show_details,
		       private_call_tier, private_call_show_details
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(
		&out.master,
		&out.serverTier,
		&out.serverDetails,
		&out.privateTier,
		&out.privateDetails,
	)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return policySettings{}, err
	}
	if !validTier(out.serverTier) || !validTier(out.privateTier) {
		return policySettings{}, errors.New("invalid stored presence tier")
	}
	return out, nil
}

func validTier(tier Tier) bool {
	return tier >= TierOff && tier <= TierServers
}

func policyFailure(class FailureClass, cause error) error {
	return &PolicyError{class: class, cause: cause}
}

func validatePolicyInput(input PolicyInput) error {
	if input.SenderID == uuid.Nil {
		return policyFailure(FailureInvalidInput, errors.New("invalid policy input"))
	}

	switch input.Category {
	case CategoryServerVoice:
		if input.ServerVoice == nil || input.PrivateCall != nil || !validServerVoiceInput(*input.ServerVoice) {
			return policyFailure(FailureInvalidInput, errors.New("invalid policy input"))
		}
	case CategoryPrivateCall:
		if input.PrivateCall == nil || input.ServerVoice != nil || !validPrivateCallInput(*input.PrivateCall) {
			return policyFailure(FailureInvalidInput, errors.New("invalid policy input"))
		}
	default:
		return policyFailure(FailureInvalidInput, errors.New("invalid policy input"))
	}
	return nil
}

func validServerVoiceInput(input ServerVoicePolicyInput) bool {
	if input.Context.ServerID == uuid.Nil || input.Context.ChannelID == uuid.Nil {
		return false
	}
	if input.Payload.ServerID != input.Context.ServerID || input.Payload.ChannelID != input.Context.ChannelID {
		return false
	}
	if !validPresenceName(input.Payload.ServerName) || !validPresenceName(input.Payload.ChannelName) {
		return false
	}
	return input.Payload.StartedAt == nil || *input.Payload.StartedAt > 0
}

func validPrivateCallInput(input PrivateCallPolicyInput) bool {
	if input.Context.ConversationID == uuid.Nil {
		return false
	}
	if input.Payload.CallType != "dm" && input.Payload.CallType != "group" {
		return false
	}
	if input.Payload.ParticipantCount < 1 || input.Payload.ParticipantCount > maxPrivateCallParticipants {
		return false
	}
	if len(input.Context.ParticipantIDs) < 1 || len(input.Context.ParticipantIDs) > maxPrivateCallParticipants {
		return false
	}
	seen := make(map[uuid.UUID]struct{}, len(input.Context.ParticipantIDs))
	for _, participantID := range input.Context.ParticipantIDs {
		if participantID == uuid.Nil {
			return false
		}
		if _, duplicate := seen[participantID]; duplicate {
			return false
		}
		seen[participantID] = struct{}{}
	}
	return input.Payload.StartedAt == nil || *input.Payload.StartedAt > 0
}

func validPresenceName(name string) bool {
	if !utf8.ValidString(name) {
		return false
	}
	runeCount := utf8.RuneCountInString(name)
	return runeCount >= 1 && runeCount <= 100
}

// AuthorizeAndMinimize validates one rich-presence event, computes its exact
// audience, and returns only the payload bytes authorized by current settings.
func AuthorizeAndMinimize(
	ctx context.Context,
	db DBTX,
	visibility ChannelVisibilityResolver,
	input PolicyInput,
) (Decision, error) {
	if err := validatePolicyInput(input); err != nil {
		return Decision{}, err
	}
	if db == nil {
		return Decision{}, policyFailure(FailureSettingsRead, errors.New("missing policy database"))
	}

	settings, err := loadPolicySettings(ctx, db, input.SenderID)
	if err != nil {
		return Decision{}, policyFailure(FailureSettingsRead, err)
	}
	if !settings.master {
		return emptyDecision(), nil
	}

	pending, suppressed, err := preparePolicyDecision(ctx, db, visibility, input, settings)
	if err != nil {
		return Decision{}, err
	}
	if suppressed || len(pending.audience) == 0 {
		return emptyDecision(), nil
	}

	raw, err := json.Marshal(pending.payload)
	if err != nil {
		return Decision{}, policyFailure(FailureMinimization, err)
	}
	if !pending.details {
		raw, err = ApplyMinimization(input.Category, raw)
		if err != nil {
			return Decision{}, policyFailure(FailureMinimization, err)
		}
	}
	return Decision{Audience: pending.audience, Payload: raw}, nil
}

func preparePolicyDecision(
	ctx context.Context,
	db DBTX,
	visibility ChannelVisibilityResolver,
	input PolicyInput,
	settings policySettings,
) (pendingPolicyDecision, bool, error) {
	switch input.Category {
	case CategoryServerVoice:
		return prepareServerVoiceDecision(ctx, db, visibility, input, settings)
	case CategoryPrivateCall:
		return preparePrivateCallDecision(ctx, db, input, settings)
	default:
		return pendingPolicyDecision{}, false, policyFailure(FailureInvalidInput, errors.New("invalid policy input"))
	}
}

func prepareServerVoiceDecision(
	ctx context.Context,
	db DBTX,
	visibility ChannelVisibilityResolver,
	input PolicyInput,
	settings policySettings,
) (pendingPolicyDecision, bool, error) {
	if settings.serverTier == TierOff {
		return pendingPolicyDecision{}, true, nil
	}
	if _, err := loadServerVoiceState(ctx, db, input.SenderID, *input.ServerVoice); err != nil {
		return pendingPolicyDecision{}, false, policyFailure(FailureStateRead, err)
	}
	audience, err := computeServerVoicePolicyAudience(
		ctx,
		db,
		visibility,
		input.SenderID,
		settings.serverTier,
		input.ServerVoice.Context,
	)
	return pendingPolicyDecision{
		audience: audience,
		payload:  input.ServerVoice.Payload,
		details:  settings.serverDetails,
	}, false, err
}

func preparePrivateCallDecision(
	ctx context.Context,
	db DBTX,
	input PolicyInput,
	settings policySettings,
) (pendingPolicyDecision, bool, error) {
	state, err := loadPrivateCallState(ctx, db, input.SenderID, *input.PrivateCall)
	if err != nil {
		return pendingPolicyDecision{}, false, policyFailure(FailureStateRead, err)
	}
	audience, err := computePrivateCallPolicyAudience(
		ctx,
		db,
		input.SenderID,
		settings.privateTier,
		state.participants,
	)
	return pendingPolicyDecision{
		audience: audience,
		payload:  input.PrivateCall.Payload,
		details:  settings.privateDetails,
	}, false, err
}

func emptyDecision() Decision {
	return Decision{Audience: map[uuid.UUID]bool{}}
}

func computeServerVoicePolicyAudience(
	ctx context.Context,
	db DBTX,
	visibility ChannelVisibilityResolver,
	senderID uuid.UUID,
	tier Tier,
	voiceContext ServerVoiceContext,
) (map[uuid.UUID]bool, error) {
	members, err := serverMembersOf(ctx, db, voiceContext.ServerID, senderID)
	if err != nil {
		return nil, policyFailure(FailureAudienceRead, err)
	}

	candidates, err := serverVoiceCandidates(ctx, db, senderID, tier, members)
	if err != nil {
		return nil, err
	}
	return visibleServerVoiceCandidates(ctx, visibility, voiceContext, candidates)
}

func serverVoiceCandidates(
	ctx context.Context,
	db DBTX,
	senderID uuid.UUID,
	tier Tier,
	members map[uuid.UUID]bool,
) (map[uuid.UUID]bool, error) {
	switch tier {
	case TierFriends:
		relationships, err := friendsAndFriendsOfFriends(ctx, db, senderID)
		if err != nil {
			return nil, err
		}
		return intersectAudience(relationships, members), nil
	case TierServers:
		return copyAudience(members), nil
	default:
		return nil, policyFailure(FailureAudienceRead, errors.New("invalid server voice tier"))
	}
}

func friendsAndFriendsOfFriends(
	ctx context.Context,
	db DBTX,
	senderID uuid.UUID,
) (map[uuid.UUID]bool, error) {
	friends, err := friendsOf(ctx, db, senderID)
	if err != nil {
		return nil, policyFailure(FailureAudienceRead, err)
	}
	friendsOfFriends, err := friendsOfFriendsOf(ctx, db, senderID)
	if err != nil {
		return nil, policyFailure(FailureAudienceRead, err)
	}
	for candidateID := range friendsOfFriends {
		friends[candidateID] = true
	}
	return friends, nil
}

func intersectAudience(left, right map[uuid.UUID]bool) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool)
	for candidateID := range left {
		if right[candidateID] {
			out[candidateID] = true
		}
	}
	return out
}

func copyAudience(source map[uuid.UUID]bool) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(source))
	for candidateID := range source {
		out[candidateID] = true
	}
	return out
}

func visibleServerVoiceCandidates(
	ctx context.Context,
	visibility ChannelVisibilityResolver,
	voiceContext ServerVoiceContext,
	candidates map[uuid.UUID]bool,
) (map[uuid.UUID]bool, error) {
	if visibility == nil {
		return nil, policyFailure(FailureAuthorizationRead, errors.New("missing channel visibility resolver"))
	}

	channelID := voiceContext.ChannelID.String()
	serverID := voiceContext.ServerID.String()
	candidateByID := make(map[string]uuid.UUID, len(candidates))
	for candidateID := range candidates {
		candidateByID[candidateID.String()] = candidateID
	}
	candidateIDs := make([]string, 0, len(candidateByID))
	for candidateID := range candidateByID {
		candidateIDs = append(candidateIDs, candidateID)
	}

	visibleUserIDs, err := visibility.FilterVisibleUserIDsForChannelFresh(ctx, serverID, channelID, candidateIDs)
	if err != nil {
		return nil, policyFailure(FailureAuthorizationRead, err)
	}

	audience := make(map[uuid.UUID]bool, len(visibleUserIDs))
	for _, visibleUserID := range visibleUserIDs {
		if candidateID, candidate := candidateByID[visibleUserID]; candidate {
			audience[candidateID] = true
		}
	}
	return audience, nil
}

func computePrivateCallPolicyAudience(
	ctx context.Context,
	db DBTX,
	senderID uuid.UUID,
	tier Tier,
	participants map[uuid.UUID]bool,
) (map[uuid.UUID]bool, error) {
	audience := make(map[uuid.UUID]bool, len(participants))
	for participantID := range participants {
		audience[participantID] = true
	}

	if tier >= TierFriends {
		friends, err := friendsOf(ctx, db, senderID)
		if err != nil {
			return nil, policyFailure(FailureAudienceRead, err)
		}
		friendsOfFriends, err := friendsOfFriendsOf(ctx, db, senderID)
		if err != nil {
			return nil, policyFailure(FailureAudienceRead, err)
		}
		for friendID := range friends {
			audience[friendID] = true
		}
		for friendOfFriendID := range friendsOfFriends {
			audience[friendOfFriendID] = true
		}
	}
	if tier == TierServers {
		peers, err := serverPeersOf(ctx, db, senderID)
		if err != nil {
			return nil, policyFailure(FailureAudienceRead, err)
		}
		for peerID := range peers {
			audience[peerID] = true
		}
	}
	delete(audience, senderID)
	return audience, nil
}
