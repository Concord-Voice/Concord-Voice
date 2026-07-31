package presence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrRecheckSenderNotCurrent reports that the sender is no longer the unique
// authoritative occupant of the captured Server Voice scope. It is produced
// ONLY by RefreshServerVoiceRecheck (#2445): an RBAC visibility sweep racing an
// ordinary voice leave is a routine race, not evidence that the replica's
// audience computation is untrustworthy, so it must not reach disconnectAll.
// The caller disconnects that sender's captured viewers and continues.
var ErrRecheckSenderNotCurrent = errors.New("rich-presence recheck sender is not current")

// RefreshServerVoiceRecheck republishes one sender's Server Voice state after
// an RBAC/SBAC visibility mutation, clearing every captured pre-mutation viewer
// who is absent from the freshly authorized audience.
//
// It mirrors RefreshPrivateCall minus the mutation: an RBAC recheck applies no
// presence mutation, so the parameter is not offered. recheckViewers is the
// exact pre-mutation authorized audience captured inside the mutation's
// transaction; the existing recheckedActivityClears computes captured − fresh.
//
// Do NOT add recheckViewers to RefreshServerVoice itself — its heartbeat-bridge
// caller in internal/voice/nats.go stays byte-for-byte untouched.
func (s *ActivityService) RefreshServerVoiceRecheck(
	ctx context.Context,
	senderID uuid.UUID,
	scope Scope,
	recheckViewers map[uuid.UUID]bool,
) error {
	if scope.Category != CategoryServerVoice {
		return ErrInvalidActivityScope
	}
	if err := validateActivityServiceCall(ctx, s, senderID, scope, CategoryServerVoice); err != nil {
		return err
	}
	request := refreshActivityRequest{
		senderID:                senderID,
		scope:                   scope,
		recheckViewers:          recheckViewers,
		category:                CategoryServerVoice,
		recheckBenignNotCurrent: true,
	}
	return s.coordinator.WithSender(ctx, senderID, func() error {
		return s.refreshAlreadyGated(ctx, request)
	})
}

// CurrentServerVoiceScope reads the sender's exact current Server Voice
// lifecycle generation from committed state. It asserts nothing: a sender with
// no row, or an ambiguous multi-row sender, returns found=false and the caller
// skips it.
//
// EventAt MUST be voice_participants.lifecycle_event_at, never time.Now():
// SourceVersion is derived from it and the store's CAS admits an idempotent
// same-token equal-version re-store. A fabricated timestamp would either miss
// the lifecycle envelope match or advance the generation spuriously.
func CurrentServerVoiceScope(
	ctx context.Context,
	db DBTX,
	senderID uuid.UUID,
) (Scope, bool, error) {
	if db == nil {
		return Scope{}, false, errors.New("rich-presence scope database unavailable")
	}
	if senderID == uuid.Nil {
		return Scope{}, false, ErrInvalidActivityScope
	}
	rows, err := db.QueryContext(ctx, `
		SELECT vp.channel_id, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE vp.user_id = $1 AND c.type = 'voice'
		ORDER BY vp.channel_id
		LIMIT 2
	`, senderID)
	if err != nil {
		return Scope{}, false, fmt.Errorf("query current server voice scope: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only scan; Err() is checked below

	var (
		channelID   uuid.UUID
		lifecycleAt time.Time
		count       int
	)
	for rows.Next() {
		count++
		if count > 1 {
			continue
		}
		if scanErr := rows.Scan(&channelID, &lifecycleAt); scanErr != nil {
			return Scope{}, false, fmt.Errorf("scan current server voice scope: %w", scanErr)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return Scope{}, false, fmt.Errorf("iterate current server voice scope: %w", rowsErr)
	}
	if count != 1 || channelID == uuid.Nil || !IsValidActivitySourceTime(lifecycleAt) {
		return Scope{}, false, nil
	}
	return Scope{
		Category:    CategoryServerVoice,
		RoomID:      channelID,
		LifecycleID: channelID,
		EventAt:     lifecycleAt,
	}, true, nil
}

// CaptureServerVoiceCandidates resolves the sender's Server Voice policy
// CANDIDATE half — settings (master, tier), server membership, and the
// friends/FoF relationship set — using exactly the components
// AuthorizeAndMinimize composes, minus the channel-visibility filter.
//
// The split exists because an RBAC write mutates only visibility inputs
// (roles / member_roles / channel_permission_overrides). The candidate half is
// untouched by every hooked site, so binding it to the mutation's transaction
// buys nothing and would cost O(#senders) round trips under the advisory lock.
//
// A nil or fail-closed sender-presence resolver yields an EMPTY candidate set,
// so a Redis blip produces no clears rather than a spurious clear to a viewer
// who never held the badge. That is the safe direction and it is deliberate.
func CaptureServerVoiceCandidates(
	ctx context.Context,
	db DBTX,
	senderPresence SenderPresenceResolver,
	senderID uuid.UUID,
	serverID uuid.UUID,
) (map[uuid.UUID]bool, error) {
	if db == nil {
		return nil, policyFailure(FailureSettingsRead, errors.New("missing capture database"))
	}
	if senderID == uuid.Nil || serverID == uuid.Nil {
		return nil, ErrInvalidActivityScope
	}
	if senderPresence == nil ||
		!senderPresence.RichPresenceEmissionPermitted(ctx, senderID) {
		return map[uuid.UUID]bool{}, nil
	}
	settings, err := loadPolicySettings(ctx, db, senderID)
	if err != nil {
		return nil, policyFailure(FailureSettingsRead, err)
	}
	if !settings.master || settings.serverTier == TierOff {
		return map[uuid.UUID]bool{}, nil
	}
	members, err := serverMembersOf(ctx, db, serverID, senderID)
	if err != nil {
		return nil, policyFailure(FailureAudienceRead, err)
	}
	return serverVoiceCandidates(ctx, db, senderID, settings.serverTier, members)
}
