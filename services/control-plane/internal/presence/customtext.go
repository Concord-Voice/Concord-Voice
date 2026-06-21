package presence

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// ComputeCustomTextAudience returns the set of user IDs permitted to see
// senderID's custom-text status, cut by the sender's custom_text_tier:
//
//	0 (Off)     -> empty set
//	1 (Friends) -> friends + friends-of-friends (when dm_friends_of_friends is on)
//	2 (Servers) -> friends + FoF + shared-server peers
//
// The sender is never included. Like ComputePresenceAudience it is pure over DB
// state and fail-closed: callers MUST treat a non-nil error as "do not fan out"
// (custom text is risk: privacy — never deliver to a non-audience viewer).
// A missing settings row is treated as Off.
func ComputeCustomTextAudience(ctx context.Context, db *sql.DB, senderID uuid.UUID) (map[uuid.UUID]bool, error) {
	var tier int
	err := db.QueryRowContext(ctx,
		`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`, senderID).Scan(&tier)
	if err == sql.ErrNoRows {
		return map[uuid.UUID]bool{}, nil // no row => Off
	}
	if err != nil {
		return nil, fmt.Errorf("custom-text audience: read tier: %w", err)
	}
	return ComputeCustomTextAudienceForTier(ctx, db, senderID, tier)
}

// ComputeCustomTextAudienceForTier computes the custom-text audience for an
// EXPLICIT tier (not the one currently persisted). It is the building block of
// ComputeCustomTextAudience, exposed so a settings-change fan-out can compute the
// PRIOR audience (using the old tier) and clear viewers who lose visibility when
// the tier narrows or custom text is turned off — risk: privacy (#1233, Gitar
// review: stale status must not linger on a viewer who lost permission). tier<=0
// yields the empty set; the sender is never included; fail-closed on DB error.
func ComputeCustomTextAudienceForTier(ctx context.Context, db *sql.DB, senderID uuid.UUID, tier int) (map[uuid.UUID]bool, error) {
	if tier <= 0 {
		return map[uuid.UUID]bool{}, nil
	}

	friends, err := friendsOf(ctx, db, senderID)
	if err != nil {
		return nil, err
	}
	fof, err := friendsOfFriendsOf(ctx, db, senderID)
	if err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]bool, len(friends)+len(fof))
	for id := range friends {
		out[id] = true
	}
	for id := range fof {
		out[id] = true
	}
	if tier == 2 {
		peers, err := serverPeersOf(ctx, db, senderID)
		if err != nil {
			return nil, err
		}
		for id := range peers {
			out[id] = true
		}
	}
	delete(out, senderID) // the sender is never in their own audience
	return out, nil
}
