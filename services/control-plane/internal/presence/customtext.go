package presence

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// ComputeCustomTextAudience returns the set of user IDs permitted to see
// senderID's custom-text status, cut by the sender's custom_text_tier and then
// by the sender's materialized recipient exclusions:
//
//	0 (Off)     -> empty set
//	1 (Friends) -> friends + friends-of-friends (when dm_friends_of_friends is on)
//	2 (Servers) -> friends + FoF + shared-server peers
//
// The sender is never included. Like ComputePresenceAudience it is pure over DB
// state and fail-closed: callers MUST treat a non-nil error as "do not fan out"
// (custom text is risk: privacy — never deliver to a non-audience viewer).
// A missing settings row is treated as Off. Recipient exclusions are the final
// discretionary filter; a failure to read them returns an error rather than an
// unfiltered tier audience.
func ComputeCustomTextAudience(ctx context.Context, db DBTX, senderID uuid.UUID) (map[uuid.UUID]bool, error) {
	var tier int
	err := db.QueryRowContext(ctx,
		`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`, senderID).Scan(&tier)
	if err == sql.ErrNoRows {
		tier = 0 // no row => Off; still run the final fail-closed filter
	} else if err != nil {
		return nil, fmt.Errorf("custom-text audience: read tier: %w", err)
	}

	base, err := computeCustomTextBaseAudienceForTier(ctx, db, senderID, tier)
	if err != nil {
		return nil, err
	}
	return applyCustomTextOverrides(ctx, db, senderID, base)
}

// ComputeCustomTextAudienceForTier computes the effective audience for an
// explicit prior tier while applying the sender's current materialized
// exclusions. Ordinary Custom Status payload/tier writes do not change those
// exclusions, so BroadcastCustomText can reconstruct only viewers who could
// actually hold the prior status. Override writes use their own transactional
// old/new audience delta. tier<=0 yields the empty set after the same fail-closed
// exclusion read; the sender is never included.
func ComputeCustomTextAudienceForTier(ctx context.Context, db DBTX, senderID uuid.UUID, tier int) (map[uuid.UUID]bool, error) {
	base, err := computeCustomTextBaseAudienceForTier(ctx, db, senderID, tier)
	if err != nil {
		return nil, err
	}
	return applyCustomTextOverrides(ctx, db, senderID, base)
}

func computeCustomTextBaseAudienceForTier(ctx context.Context, db DBTX, senderID uuid.UUID, tier int) (map[uuid.UUID]bool, error) {
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
