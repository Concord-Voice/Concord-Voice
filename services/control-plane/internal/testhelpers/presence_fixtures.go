package testhelpers

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// CreateUser inserts a minimal users row (matching the NOT NULL columns:
// email, username, password_hash) and returns its id. $1 is the uuid (id
// column) and $2 is its text form for the SQL-side email/username derivation —
// distinct params so pq deduces one type per parameter (a single $1 used as
// both uuid and ::text triggers "inconsistent types deduced", 42P08). The
// password_hash is a non-secret SQL literal ('x'); audience computation never
// reads it.
func CreateUser(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2 || '@test.local', 'u_' || left($2, 8), 'x', true, true)`,
		id, id.String(),
	)
	require.NoError(t, err)
	return id
}

// AddFriendship inserts an accepted friendship (requester=a, addressee=b).
func AddFriendship(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		a, b,
	)
	require.NoError(t, err)
}

// RemoveFriendship deletes any friendship row between a and b (either direction).
func RemoveFriendship(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`DELETE FROM friendships
		 WHERE (requester_id = $1 AND addressee_id = $2)
		    OR (requester_id = $2 AND addressee_id = $1)`,
		a, b,
	)
	require.NoError(t, err)
}

// CreateServer inserts a minimal servers row owned by owner and returns its id.
// The name is derived from the uuid in SQL (|| concat) to keep the query literal.
func CreateServer(t *testing.T, db *sql.DB, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 's_' || left($2, 8), $3)`,
		id, id.String(), owner,
	)
	require.NoError(t, err)
	return id
}

// AddServerMember inserts a server_members row (role defaults to 'member').
func AddServerMember(t *testing.T, db *sql.DB, serverID, userID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`,
		serverID, userID,
	)
	require.NoError(t, err)
}

// SetFriendsOfFriends upserts the user's dm_friends_of_friends flag. All other
// privacy_settings columns carry NOT NULL DEFAULTs, so the minimal insert is valid.
func SetFriendsOfFriends(t *testing.T, db *sql.DB, userID uuid.UUID, enabled bool) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO privacy_settings (user_id, dm_friends_of_friends) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET dm_friends_of_friends = EXCLUDED.dm_friends_of_friends`,
		userID, enabled,
	)
	require.NoError(t, err)
}

// SetCustomTextTier upserts the user's custom_text_tier (0=Off, 1=Friends,
// 2=Servers) into user_presence_settings. Other columns carry NOT NULL DEFAULTs
// or are nullable, so the minimal insert is valid.
func SetCustomTextTier(t *testing.T, db *sql.DB, userID uuid.UUID, tier int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET custom_text_tier = EXCLUDED.custom_text_tier`,
		userID, tier,
	)
	require.NoError(t, err)
}
