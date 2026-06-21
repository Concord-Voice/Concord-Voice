// Package presence is the single server-side authority for presence audience
// computation. ComputePresenceAudience is the base predicate (issue #47); the
// rich-presence epic (#1236) composes per-category tiers on top of it
// (see [internal]specs/2026-05-28-rich-presence-design.md §14.1).
package presence

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// ComputePresenceAudience returns the set of user IDs permitted to see senderID's
// base presence: accepted friends, friends-of-friends when the sender has
// dm_friends_of_friends enabled, and users sharing >=1 server with the sender.
// The sender is never included. Pure over DB state; computed on demand, never
// cached (spec §6.7). Callers MUST treat a non-nil error as fail-closed: do not
// fan out presence when the audience cannot be computed.
//
// It is the union of the three reusable component sets (friendsOf,
// friendsOfFriendsOf, serverPeersOf); the rich-presence per-category tier logic
// composes those components selectively (see customtext.go, #1233).
func ComputePresenceAudience(ctx context.Context, db *sql.DB, senderID uuid.UUID) (map[uuid.UUID]bool, error) {
	friends, err := friendsOf(ctx, db, senderID)
	if err != nil {
		return nil, err
	}
	fof, err := friendsOfFriendsOf(ctx, db, senderID)
	if err != nil {
		return nil, err
	}
	peers, err := serverPeersOf(ctx, db, senderID)
	if err != nil {
		return nil, err
	}

	audience := make(map[uuid.UUID]bool, len(friends)+len(fof)+len(peers))
	for id := range friends {
		audience[id] = true
	}
	for id := range fof {
		audience[id] = true
	}
	for id := range peers {
		audience[id] = true
	}
	delete(audience, senderID) // the sender is never in their own audience
	return audience, nil
}

// friendsOf returns the sender's accepted friends (the CASE picks the other side
// of each friendship row).
func friendsOf(ctx context.Context, db *sql.DB, senderID uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	rows, err := db.QueryContext(ctx, `
		SELECT CASE WHEN requester_id = $1 THEN addressee_id ELSE requester_id END AS friend_id
		FROM friendships
		WHERE (requester_id = $1 OR addressee_id = $1) AND status = 'accepted'
	`, senderID)
	if err != nil {
		return nil, fmt.Errorf("presence audience: query friends: %w", err)
	}
	if err := scanIDs(rows, out); err != nil {
		return nil, fmt.Errorf("presence audience: scan friends: %w", err)
	}
	return out, nil
}

// friendsOfFriendsOf returns friends-of-friends IDs ONLY when the sender enabled
// dm_friends_of_friends; otherwise an empty set. This opt-in gate (spec §6.6) is
// shared by base presence and the rich-presence Friends/Servers tiers.
func friendsOfFriendsOf(ctx context.Context, db *sql.DB, senderID uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	enabled, err := friendsOfFriendsEnabled(ctx, db, senderID)
	if err != nil {
		return nil, fmt.Errorf("presence audience: read fof flag: %w", err)
	}
	if !enabled {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		WITH sender_friends AS (
			SELECT CASE WHEN requester_id = $1 THEN addressee_id ELSE requester_id END AS friend_id
			FROM friendships
			WHERE (requester_id = $1 OR addressee_id = $1) AND status = 'accepted'
		)
		SELECT CASE WHEN f.requester_id = sf.friend_id THEN f.addressee_id ELSE f.requester_id END AS fof_id
		FROM sender_friends sf
		JOIN friendships f
		  ON (f.requester_id = sf.friend_id OR f.addressee_id = sf.friend_id)
		 AND f.status = 'accepted'
	`, senderID)
	if err != nil {
		return nil, fmt.Errorf("presence audience: query fof: %w", err)
	}
	if err := scanIDs(rows, out); err != nil {
		return nil, fmt.Errorf("presence audience: scan fof: %w", err)
	}
	return out, nil
}

// serverPeersOf returns users co-resident in >=1 server with the sender.
func serverPeersOf(ctx context.Context, db *sql.DB, senderID uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT sm2.user_id
		FROM server_members sm1
		JOIN server_members sm2 ON sm1.server_id = sm2.server_id
		WHERE sm1.user_id = $1
	`, senderID)
	if err != nil {
		return nil, fmt.Errorf("presence audience: query servers: %w", err)
	}
	if err := scanIDs(rows, out); err != nil {
		return nil, fmt.Errorf("presence audience: scan servers: %w", err)
	}
	return out, nil
}

// friendsOfFriendsEnabled reads the sender's dm_friends_of_friends flag from
// privacy_settings; a missing row defaults to false.
func friendsOfFriendsEnabled(ctx context.Context, db *sql.DB, userID uuid.UUID) (bool, error) {
	var enabled bool
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(dm_friends_of_friends, FALSE) FROM privacy_settings WHERE user_id = $1`,
		userID,
	).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled, nil
}

// scanIDs scans a single-uuid-column result set into the destination set and
// always closes the rows. It checks rows.Err() after iteration per
// [internal]rules/backend.md.
func scanIDs(rows *sql.Rows, into map[uuid.UUID]bool) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		into[id] = true
	}
	return rows.Err()
}
