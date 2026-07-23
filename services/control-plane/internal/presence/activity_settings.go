package presence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const activitySettingsSuppressionMinimumBudget = 2 * activityCleanupTimeout

var errActivitySettingsSuppressionBudget = errors.New(
	"rich-presence settings cleanup budget cannot preserve fail-closed recovery",
)

// ActivityPolicySettings is the policy state bracketing one committed settings
// write. It intentionally excludes Custom Status, which has its own delivery
// plan and acknowledgement path.
type ActivityPolicySettings struct {
	MasterEnabled          bool
	ServerVoiceTier        Tier
	ServerVoiceShowDetails bool
	PrivateCallTier        Tier
	PrivateCallShowDetails bool
}

// SuppressAllActivityAlreadyGated removes every supported current activity for
// an account that is about to be erased, then disconnects local Rich Presence
// clients so no previously delivered projection can survive the database
// cascade. The caller must already own the sender gate.
func (s *ActivityService) SuppressAllActivityAlreadyGated(
	ctx context.Context,
	userID uuid.UUID,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if userID == uuid.Nil {
		return errors.New("account activity cleanup requires a user")
	}
	if s == nil || s.store == nil || s.delivery == nil {
		return errors.New("account activity cleanup unavailable")
	}

	cleanupCtx, cancelCleanup := boundedActivityCleanupContext(ctx)
	defer cancelCleanup()
	var cleanupErrors []error
	for _, category := range []Category{CategoryServerVoice, CategoryPrivateCall} {
		if err := s.store.Delete(cleanupCtx, userID, category); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"delete account %s rich-presence activity: %w", category, err,
			))
		}
	}
	if err := s.disconnectAll(ctx); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

// ApplySettingsSuppressionAlreadyGated removes activity made ineligible by a
// confirmed committed settings write. The caller must already own the sender
// gate and supplies the context budget for the full suppression phase.
func (s *ActivityService) ApplySettingsSuppressionAlreadyGated(
	ctx context.Context,
	userID uuid.UUID,
	before ActivityPolicySettings,
	after ActivityPolicySettings,
) error {
	if err := validateActivitySettingsCleanup(ctx, s, userID, before, after); err != nil {
		return err
	}
	if before == after {
		return nil
	}
	if !activitySettingsSuppressionHasRecoveryBudget(ctx) {
		return errors.Join(
			errActivitySettingsSuppressionBudget,
			s.disconnectAllWithinBudget(ctx),
		)
	}

	workCtx, cancelWork := context.WithTimeout(ctx, activityCleanupTimeout)
	defer cancelWork()

	cleanupErrors := s.disconnectActivitySettingsRecipients(
		workCtx, ctx, userID, before, after,
	)
	cleanupErrors = append(cleanupErrors, s.deleteSuppressedActivity(workCtx, userID, after)...)
	return errors.Join(cleanupErrors...)
}

func activitySettingsSuppressionHasRecoveryBudget(ctx context.Context) bool {
	deadline, bounded := ctx.Deadline()
	return !bounded || time.Until(deadline) > activitySettingsSuppressionMinimumBudget
}

func validateActivitySettingsCleanup(
	ctx context.Context,
	service *ActivityService,
	userID uuid.UUID,
	before ActivityPolicySettings,
	after ActivityPolicySettings,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if userID == uuid.Nil {
		return errors.New("rich-presence settings cleanup requires a user")
	}
	if !validActivityPolicySettings(before) || !validActivityPolicySettings(after) {
		return errors.New("rich-presence settings cleanup received an invalid tier")
	}
	if service == nil || service.store == nil || service.delivery == nil || service.settingsRecipients == nil {
		return errors.New("rich-presence settings cleanup unavailable")
	}
	return nil
}

func (s *ActivityService) disconnectActivitySettingsRecipients(
	workCtx context.Context,
	suppressionCtx context.Context,
	userID uuid.UUID,
	before ActivityPolicySettings,
	after ActivityPolicySettings,
) []error {
	var cleanupErrors []error
	recipients, err := s.settingsRecipients(workCtx, userID, before, after)
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("resolve affected rich-presence clients: %w", err))
		if disconnectErr := s.disconnectAllWithinBudget(suppressionCtx); disconnectErr != nil {
			cleanupErrors = append(cleanupErrors, disconnectErr)
		}
		return cleanupErrors
	}
	if len(recipients) == 0 {
		return cleanupErrors
	}
	if disconnectErr := s.delivery.DisconnectRichPresenceClients(workCtx, recipients); disconnectErr != nil {
		cleanupErrors = append(
			cleanupErrors,
			fmt.Errorf("disconnect affected rich-presence clients: %w", disconnectErr),
		)
	}
	return cleanupErrors
}

func (s *ActivityService) deleteSuppressedActivity(
	ctx context.Context,
	userID uuid.UUID,
	after ActivityPolicySettings,
) []error {
	var cleanupErrors []error
	if !after.MasterEnabled || after.ServerVoiceTier == TierOff {
		if err := s.store.Delete(ctx, userID, CategoryServerVoice); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete suppressed server-voice activity: %w", err))
		}
	}
	if !after.MasterEnabled {
		if err := s.store.Delete(ctx, userID, CategoryPrivateCall); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete suppressed private-call activity: %w", err))
		}
	}
	return cleanupErrors
}

func validActivityPolicySettings(settings ActivityPolicySettings) bool {
	return validTier(settings.ServerVoiceTier) && validTier(settings.PrivateCallTier)
}

const activitySettingsRecipientLimit = 512

type activitySettingsRecipientDependencies struct {
	db         DBTX
	visibility ChannelVisibilityResolver
	builder    activityBuilder
	store      *ActivityStore
}

func computeActivitySettingsRecipients(
	ctx context.Context,
	dependencies activitySettingsRecipientDependencies,
	userID uuid.UUID,
	before ActivityPolicySettings,
	after ActivityPolicySettings,
) (map[uuid.UUID]bool, error) {
	if dependencies.db == nil || dependencies.builder == nil || dependencies.store == nil {
		return nil, errors.New("rich-presence settings recipient resolver unavailable")
	}
	recipients := make(map[uuid.UUID]bool)
	serverRecipients, serverActive, err := changedServerSettingsRecipients(
		ctx, dependencies, userID, before, after,
	)
	if err != nil {
		return nil, err
	}
	if err := mergeActiveSettingsRecipients(recipients, serverRecipients, serverActive, userID); err != nil {
		return nil, err
	}
	privateRecipients, privateActive, err := changedPrivateSettingsRecipients(
		ctx, dependencies, userID, before, after,
	)
	if err != nil {
		return nil, err
	}
	if err := mergeActiveSettingsRecipients(recipients, privateRecipients, privateActive, userID); err != nil {
		return nil, err
	}
	return recipients, nil
}

func changedServerSettingsRecipients(
	ctx context.Context,
	dependencies activitySettingsRecipientDependencies,
	userID uuid.UUID,
	before ActivityPolicySettings,
	after ActivityPolicySettings,
) (map[uuid.UUID]bool, bool, error) {
	if !serverActivityPolicyChanged(before, after) {
		return nil, false, nil
	}
	return currentServerSettingsRecipients(
		ctx,
		dependencies,
		userID,
		maxEnabledServerTier(before, after),
		before.MasterEnabled && before.ServerVoiceTier != TierOff,
	)
}

func changedPrivateSettingsRecipients(
	ctx context.Context,
	dependencies activitySettingsRecipientDependencies,
	userID uuid.UUID,
	before ActivityPolicySettings,
	after ActivityPolicySettings,
) (map[uuid.UUID]bool, bool, error) {
	if !privateActivityPolicyChanged(before, after) {
		return nil, false, nil
	}
	return currentPrivateSettingsRecipients(
		ctx,
		dependencies.db,
		dependencies.builder,
		dependencies.store,
		userID,
		maxEnabledPrivateTier(before, after),
		before.MasterEnabled,
	)
}

func mergeActiveSettingsRecipients(
	into map[uuid.UUID]bool,
	recipients map[uuid.UUID]bool,
	active bool,
	userID uuid.UUID,
) error {
	if !active {
		return nil
	}
	recipients[userID] = true
	mergeAudience(into, recipients)
	if len(into) > activitySettingsRecipientLimit {
		return errors.New("rich-presence settings recipient union limit exceeded")
	}
	return nil
}

func serverActivityPolicyChanged(before, after ActivityPolicySettings) bool {
	return before.MasterEnabled != after.MasterEnabled ||
		before.ServerVoiceTier != after.ServerVoiceTier ||
		before.ServerVoiceShowDetails != after.ServerVoiceShowDetails
}

func privateActivityPolicyChanged(before, after ActivityPolicySettings) bool {
	return before.MasterEnabled != after.MasterEnabled ||
		before.PrivateCallTier != after.PrivateCallTier ||
		before.PrivateCallShowDetails != after.PrivateCallShowDetails
}

func maxEnabledServerTier(before, after ActivityPolicySettings) Tier {
	var tier Tier
	if before.MasterEnabled {
		tier = before.ServerVoiceTier
	}
	if after.MasterEnabled && after.ServerVoiceTier > tier {
		tier = after.ServerVoiceTier
	}
	return tier
}

func maxEnabledPrivateTier(before, after ActivityPolicySettings) Tier {
	var tier Tier
	if before.MasterEnabled {
		tier = before.PrivateCallTier
	}
	if after.MasterEnabled && after.PrivateCallTier > tier {
		tier = after.PrivateCallTier
	}
	return tier
}

func currentServerSettingsRecipients(
	ctx context.Context,
	dependencies activitySettingsRecipientDependencies,
	userID uuid.UUID,
	tier Tier,
	priorEligible bool,
) (map[uuid.UUID]bool, bool, error) {
	state, found, err := dependencies.store.Get(ctx, userID, CategoryServerVoice)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return settingsRecipientsWithoutState(CategoryServerVoice, priorEligible)
	}
	built, err := loadCurrentServerSettingsActivity(
		ctx, dependencies.builder, dependencies.store, userID, state,
	)
	if err != nil {
		return nil, false, err
	}
	serverID := built.Input.ServerVoice.Context.ServerID
	channelID := built.Input.ServerVoice.Context.ChannelID
	if tier == TierOff {
		if err := requireCurrentSettingsGeneration(
			ctx, dependencies.store, userID, CategoryServerVoice, state,
			"rich-presence server settings generation changed",
		); err != nil {
			return nil, false, err
		}
		return map[uuid.UUID]bool{}, true, nil
	}
	out, err := visibleServerSettingsRecipients(
		ctx, dependencies.db, dependencies.visibility, userID, serverID, channelID, tier,
	)
	if err != nil {
		return nil, false, err
	}
	if err := requireCurrentSettingsGeneration(
		ctx, dependencies.store, userID, CategoryServerVoice, state,
		"rich-presence server settings generation changed",
	); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func loadCurrentServerSettingsActivity(
	ctx context.Context,
	builder activityBuilder,
	store *ActivityStore,
	userID uuid.UUID,
	state ActivityState,
) (BuiltActivity, error) {
	if err := requireCurrentSettingsGeneration(
		ctx, store, userID, CategoryServerVoice, state,
		"rich-presence server settings lifecycle unavailable for stored state",
	); err != nil {
		return BuiltActivity{}, err
	}
	built, err := builder.Build(ctx, userID, Scope{
		Category: CategoryServerVoice, RoomID: state.SourceToken,
		LifecycleID: state.SourceToken, EventAt: time.UnixMicro(state.SourceVersion),
	})
	if err != nil {
		return BuiltActivity{}, err
	}
	if built.SourceToken != state.SourceToken || built.SourceVersion != state.SourceVersion ||
		built.Input.ServerVoice == nil {
		return BuiltActivity{}, errors.New("rich-presence server settings generation changed")
	}
	return built, nil
}

func visibleServerSettingsRecipients(
	ctx context.Context,
	db DBTX,
	visibility ChannelVisibilityResolver,
	userID, serverID, channelID uuid.UUID,
	tier Tier,
) (map[uuid.UUID]bool, error) {
	candidates, err := serverSettingsCandidates(ctx, db, userID, serverID, tier)
	if err != nil {
		return nil, err
	}
	if visibility == nil {
		return nil, errors.New("rich-presence settings channel authorization unavailable")
	}
	byText, texts := textIndexedSettingsCandidates(candidates)
	visible, err := visibility.FilterVisibleUserIDsForChannelFresh(
		ctx, serverID.String(), channelID.String(), texts,
	)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(visible))
	for _, visibleID := range visible {
		if candidateID, ok := byText[visibleID]; ok {
			out[candidateID] = true
		}
	}
	return out, nil
}

func textIndexedSettingsCandidates(
	candidates map[uuid.UUID]bool,
) (map[string]uuid.UUID, []string) {
	byText := make(map[string]uuid.UUID, len(candidates))
	texts := make([]string, 0, len(candidates))
	for candidateID := range candidates {
		text := candidateID.String()
		byText[text] = candidateID
		texts = append(texts, text)
	}
	return byText, texts
}

func requireCurrentSettingsGeneration(
	ctx context.Context,
	store *ActivityStore,
	userID uuid.UUID,
	category Category,
	state ActivityState,
	inactiveMessage string,
) error {
	active, err := store.IsActiveGeneration(
		ctx, userID, category, state.SourceToken, state.SourceVersion,
	)
	if err != nil {
		return err
	}
	if !active {
		return errors.New(inactiveMessage)
	}
	return nil
}

func serverSettingsCandidates(
	ctx context.Context,
	db DBTX,
	userID, serverID uuid.UUID,
	tier Tier,
) (map[uuid.UUID]bool, error) {
	return queryBoundedSettingsRecipients(ctx, db, `
		SELECT member.user_id
		FROM server_members member
		WHERE member.server_id = $2
		  AND member.user_id <> $1
		  AND ($3 = 2 OR EXISTS (
			SELECT 1 FROM friendships direct
			WHERE direct.status = 'accepted'
			  AND (
			    (direct.requester_id = $1 AND direct.addressee_id = member.user_id)
			    OR (direct.addressee_id = $1 AND direct.requester_id = member.user_id)
			  )
		  ) OR EXISTS (
			SELECT 1
			FROM privacy_settings sender_privacy
			JOIN friendships sender_friend
			  ON sender_friend.status = 'accepted'
			 AND (sender_friend.requester_id = $1 OR sender_friend.addressee_id = $1)
			WHERE sender_privacy.user_id = $1
			  AND sender_privacy.dm_friends_of_friends
			  AND EXISTS (
			    SELECT 1
			    FROM friendships friend_candidate
			    WHERE friend_candidate.status = 'accepted'
			      AND (
			        (friend_candidate.requester_id = CASE
			           WHEN sender_friend.requester_id = $1 THEN sender_friend.addressee_id
			           ELSE sender_friend.requester_id END
			         AND friend_candidate.addressee_id = member.user_id)
			        OR
			        (friend_candidate.addressee_id = CASE
			           WHEN sender_friend.requester_id = $1 THEN sender_friend.addressee_id
			           ELSE sender_friend.requester_id END
			         AND friend_candidate.requester_id = member.user_id)
			      )
			  )
		  ))
		ORDER BY member.user_id
		LIMIT $4
	`, userID, serverID, tier, activitySettingsRecipientLimit+1)
}

func currentPrivateSettingsRecipients(
	ctx context.Context,
	db DBTX,
	builder activityBuilder,
	store *ActivityStore,
	userID uuid.UUID,
	tier Tier,
	priorEligible bool,
) (map[uuid.UUID]bool, bool, error) {
	state, found, err := store.Get(ctx, userID, CategoryPrivateCall)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return settingsRecipientsWithoutState(CategoryPrivateCall, priorEligible)
	}
	active, err := store.IsActiveGeneration(
		ctx, userID, CategoryPrivateCall, state.SourceToken, state.SourceVersion,
	)
	if err != nil {
		return nil, false, err
	}
	if !active {
		return nil, false, errors.New(
			"rich-presence private settings lifecycle unavailable for stored state",
		)
	}
	conversationID, err := exactCurrentPrivateCallScope(ctx, db, userID, state)
	if err != nil {
		return nil, false, err
	}
	built, err := builder.Build(ctx, userID, Scope{
		Category: CategoryPrivateCall, RoomID: conversationID,
		LifecycleID: state.SourceToken, EventAt: time.UnixMicro(state.SourceVersion),
	})
	if err != nil {
		return nil, false, err
	}
	if built.SourceToken != state.SourceToken || built.SourceVersion != state.SourceVersion ||
		built.Input.PrivateCall == nil ||
		built.Input.PrivateCall.Context.ConversationID != conversationID {
		return nil, false, errors.New("rich-presence private settings generation changed")
	}
	recipients, err := queryBoundedSettingsRecipients(ctx, db, `
		WITH candidates AS (
			SELECT participant.user_id
			FROM dm_voice_participants participant
			WHERE participant.conversation_id = $2
			UNION
			SELECT CASE
			         WHEN direct.requester_id = $1 THEN direct.addressee_id
			         ELSE direct.requester_id
			       END
			FROM friendships direct
			WHERE $3 >= 1 AND direct.status = 'accepted'
			  AND (direct.requester_id = $1 OR direct.addressee_id = $1)
			UNION
			SELECT CASE
			         WHEN friend_candidate.requester_id = CASE
			           WHEN sender_friend.requester_id = $1 THEN sender_friend.addressee_id
			           ELSE sender_friend.requester_id END
			         THEN friend_candidate.addressee_id
			         ELSE friend_candidate.requester_id
			       END
			FROM privacy_settings sender_privacy
			JOIN friendships sender_friend
			  ON sender_friend.status = 'accepted'
			 AND (sender_friend.requester_id = $1 OR sender_friend.addressee_id = $1)
			JOIN friendships friend_candidate
			  ON friend_candidate.status = 'accepted'
			 AND (
			   friend_candidate.requester_id = CASE
			     WHEN sender_friend.requester_id = $1 THEN sender_friend.addressee_id
			     ELSE sender_friend.requester_id END
			   OR friend_candidate.addressee_id = CASE
			     WHEN sender_friend.requester_id = $1 THEN sender_friend.addressee_id
			     ELSE sender_friend.requester_id END
			 )
			WHERE $3 >= 1 AND sender_privacy.user_id = $1
			  AND sender_privacy.dm_friends_of_friends
			UNION
			SELECT peer.user_id
			FROM server_members sender_member
			JOIN server_members peer ON peer.server_id = sender_member.server_id
			WHERE $3 = 2 AND sender_member.user_id = $1
		)
		SELECT user_id FROM candidates
		WHERE user_id <> $1
		ORDER BY user_id
		LIMIT $4
	`, userID, conversationID, tier, activitySettingsRecipientLimit+1)
	if err != nil {
		return nil, false, err
	}
	stillActive, err := store.IsActiveGeneration(
		ctx, userID, CategoryPrivateCall, state.SourceToken, state.SourceVersion,
	)
	if err != nil {
		return nil, false, err
	}
	if !stillActive {
		return nil, false, errors.New("rich-presence private settings generation changed")
	}
	return recipients, true, nil
}

func settingsRecipientsWithoutState(
	category Category,
	priorEligible bool,
) (map[uuid.UUID]bool, bool, error) {
	switch category {
	case CategoryServerVoice, CategoryPrivateCall:
	default:
		return nil, false, ErrInvalidActivityState
	}
	if !priorEligible {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf(
		"rich-presence %s settings evidence unavailable for prior-eligible policy", category,
	)
}

func exactCurrentPrivateCallScope(
	ctx context.Context,
	db DBTX,
	userID uuid.UUID,
	state ActivityState,
) (conversationID uuid.UUID, returnErr error) {
	rows, err := db.QueryContext(ctx, `
		SELECT conversation_id, lifecycle_event_at
		FROM dm_voice_participants
		WHERE user_id = $1
		ORDER BY conversation_id
		LIMIT 2
	`, userID)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	var count int
	for rows.Next() {
		var lifecycleAt time.Time
		if err := rows.Scan(&conversationID, &lifecycleAt); err != nil {
			return uuid.Nil, err
		}
		count++
		if lifecycleAt.UnixMicro() != state.SourceVersion {
			return uuid.Nil, errors.New("rich-presence private settings scope changed")
		}
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	if count != 1 {
		return uuid.Nil, errors.New("rich-presence private settings scope is not unique")
	}
	return conversationID, nil
}

func queryBoundedSettingsRecipients(
	ctx context.Context,
	db DBTX,
	query string,
	args ...any,
) (out map[uuid.UUID]bool, returnErr error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	out = make(map[uuid.UUID]bool)
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		out[userID] = true
		if len(out) > activitySettingsRecipientLimit {
			return nil, errors.New("rich-presence settings recipient limit exceeded")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func mergeAudience(into, from map[uuid.UUID]bool) {
	for userID, included := range from {
		if included {
			into[userID] = true
		}
	}
}
