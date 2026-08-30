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
// an authority visibility mutation, clearing every captured pre-mutation viewer
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
// A nil resolver, a DETERMINED suppression, or an UNDETERMINED base presence
// all yield an EMPTY candidate set here, so a Redis blip produces no clears
// rather than a spurious clear to a viewer who never held the badge. That is
// the safe direction for THIS caller and it is deliberate.
//
// The #2446 pre-mutation capture needs the opposite for the undetermined case
// and must call CaptureServerVoiceCandidatesStrict instead — see the contrast
// documented there. Do NOT collapse the two back together: the leniency here is
// load-bearing for RBAC write availability, and the strictness there is
// load-bearing for not silently dropping a reconciliation leg.
func CaptureServerVoiceCandidates(
	ctx context.Context,
	db DBTX,
	senderPresence SenderPresenceResolver,
	senderID uuid.UUID,
	serverID uuid.UUID,
) (map[uuid.UUID]bool, error) {
	return captureServerVoiceCandidates(ctx, db, senderPresence, senderID, serverID, nil, false)
}

// CaptureServerVoiceCandidatesWithMembers is CaptureServerVoiceCandidates with
// the server-member read shared across one capture's senders (#2681).
//
// A nil loader means "read the members yourself" — the pre-#2681 behaviour, and
// the correct choice for a caller whose loop varies serverID rather than
// senderID, which is why internal/graphpresence stays on it.
//
// The loader is consulted ONLY at the member read below, beneath every
// short-circuit above it, so a capture whose senders all have presence disabled
// still issues zero member reads. Hoisting it earlier regresses that to one.
//
// Like CaptureServerVoiceCandidates it is LENIENT about an undetermined base
// presence: it serves the same #2445 caller, where an error blocks an RBAC
// write.
func CaptureServerVoiceCandidatesWithMembers(
	ctx context.Context,
	db DBTX,
	senderPresence SenderPresenceResolver,
	senderID uuid.UUID,
	serverID uuid.UUID,
	loader *ServerMemberLoader,
) (map[uuid.UUID]bool, error) {
	return captureServerVoiceCandidates(
		ctx, db, senderPresence, senderID, serverID, loader, false,
	)
}

// CaptureServerVoiceCandidatesWithMembersStrict is the strict counterpart to
// CaptureServerVoiceCandidatesWithMembers for a pre-mutation ownership capture.
func CaptureServerVoiceCandidatesWithMembersStrict(
	ctx context.Context,
	db DBTX,
	senderPresence SenderPresenceResolver,
	senderID uuid.UUID,
	serverID uuid.UUID,
	loader *ServerMemberLoader,
) (map[uuid.UUID]bool, error) {
	return captureServerVoiceCandidates(
		ctx, db, senderPresence, senderID, serverID, loader, true,
	)
}

// CaptureServerVoiceCandidatesStrict is CaptureServerVoiceCandidates for the
// #2446 PRE-MUTATION capture, where an undetermined base presence is an error
// rather than an empty audience.
//
// The distinction is not stylistic. #2445's caller (voicepresence.Executor.
// PrepareCapture) resolves candidates BEFORE an RBAC authority write, and per
// rbac.withAuthorityCapture a PrepareCapture error returns 500 with the write
// blocked — so propagating a transient Redis fault there would put Redis
// availability in front of CreateRole/UpdateRole/AssignRole/RemoveMember/ban
// (PR #2770 review, Gitar). #2446's caller instead reconciles an audience the
// write is about to destroy, where an empty set means "clear nobody" and
// silently drops the leg.
//
// Same fault, opposite correct answer, because the callers are asking different
// questions. Hence separate entry points rather than one changed contract.
//
// It takes no member loader: graphpresence's loop varies serverID rather than
// senderID, so there is nothing to share across senders (#2681).
func CaptureServerVoiceCandidatesStrict(
	ctx context.Context,
	db DBTX,
	senderPresence SenderPresenceResolver,
	senderID uuid.UUID,
	serverID uuid.UUID,
) (map[uuid.UUID]bool, error) {
	return captureServerVoiceCandidates(ctx, db, senderPresence, senderID, serverID, nil, true)
}

func captureServerVoiceCandidates(
	ctx context.Context,
	db DBTX,
	senderPresence SenderPresenceResolver,
	senderID uuid.UUID,
	serverID uuid.UUID,
	loader *ServerMemberLoader,
	strictPresence bool,
) (map[uuid.UUID]bool, error) {
	if db == nil {
		return nil, policyFailure(FailureSettingsRead, errors.New("missing capture database"))
	}
	if senderID == uuid.Nil || serverID == uuid.Nil {
		return nil, ErrInvalidActivityScope
	}
	// A NIL resolver keeps its existing meaning — empty, not an error — on BOTH
	// entry points. The finding this addresses is that TRANSIENT faults were
	// collapsed into the same empty as a suppression; a nil resolver is a wiring
	// bug, a different defect with a different control.
	if senderPresence == nil {
		return map[uuid.UUID]bool{}, nil
	}
	// The STATE form, not the bool one, so an undetermined answer is still
	// distinguishable at this point even when the caller has asked to absorb it.
	permitted, err := senderPresence.RichPresenceEmissionState(ctx, senderID)
	if err != nil {
		if !strictPresence {
			// Lenient caller (#2445): absorb, exactly as before this change.
			return map[uuid.UUID]bool{}, nil
		}
		// Strict caller (#2446): a capture that cannot determine the sender's
		// base presence must refuse rather than return an empty audience — the
		// empty was indistinguishable from a legitimate suppression, so the leg
		// was dropped, the caller's FailPosture never applied, and a viewer who
		// had just lost authorization kept the activity until the TTL expired
		// (CWE-284; PR #2770 review, CodeRabbit).
		return nil, policyFailure(FailureAudienceRead,
			fmt.Errorf("read rich-presence emission state: %w", err))
	}
	if !permitted {
		return map[uuid.UUID]bool{}, nil
	}
	settings, err := loadPolicySettings(ctx, db, senderID)
	if err != nil {
		return nil, policyFailure(FailureSettingsRead, err)
	}
	if !settings.master || settings.serverTier == TierOff {
		return map[uuid.UUID]bool{}, nil
	}
	members, err := captureServerMembers(ctx, db, serverID, senderID, loader)
	if err != nil {
		return nil, policyFailure(FailureAudienceRead, err)
	}
	return serverVoiceCandidates(ctx, db, senderID, settings.serverTier, members)
}

// ErrCaptureLoaderServerMismatch reports a capture whose shared member loader
// was built for a different server than the call names. The loader resolves
// membership from ITS OWN server binding, so honouring the mismatch would
// return another server's members as this sender's presence audience.
var ErrCaptureLoaderServerMismatch = errors.New(
	"rich-presence capture loader is bound to a different server",
)

// captureServerMembers resolves the sender's member set through the capture's
// shared loader when one is present, and falls back to a direct per-sender read
// when it is not.
//
// A non-nil loader makes the serverID argument advisory rather than
// authoritative — membersFor reads l.serverID, not this one. The binding is
// therefore asserted here and a mismatch fails CLOSED: PrepareCapture
// propagates the error and the RBAC mutation rolls back, where silently
// resolving the loader's server would commit a cross-server audience.
func captureServerMembers(
	ctx context.Context,
	db DBTX,
	serverID, senderID uuid.UUID,
	loader *ServerMemberLoader,
) (map[uuid.UUID]bool, error) {
	if loader == nil {
		return serverMembersOf(ctx, db, serverID, senderID)
	}
	if !loader.boundTo(serverID) {
		return nil, ErrCaptureLoaderServerMismatch
	}
	return loader.membersFor(ctx, senderID)
}
