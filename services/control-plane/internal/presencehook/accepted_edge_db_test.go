package presencehook_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// internal/testhelpers/testdb has zero internal dependencies, so presencehook's
// tests can use it without the import cycle that internal/testhelpers proper
// would create.

func edgeTestUser(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2 || '@test.local', 'u_' || left($2, 8), 'x', true, true)`,
		id, id.String())
	require.NoError(t, err)
	return id
}

// TestAcceptedEdgeExistsAgainstARealDatabase covers both verdicts and, more
// importantly, pins the SYMMETRY: the predicate must answer true regardless of
// which endpoint requested the friendship. Four callers share this one function
// and two of them pass the actor first while the third passes the counterpart
// first, so an asymmetric predicate would silently disagree between the probe
// and the authoritative read it is checked against.
func TestAcceptedEdgeExistsAgainstARealDatabase(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	requester := edgeTestUser(t, db)
	addressee := edgeTestUser(t, db)
	stranger := edgeTestUser(t, db)

	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		requester, addressee)
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("accepted edge, requester first", func(t *testing.T) {
		got, err := presencehook.AcceptedEdgeExists(ctx, db, requester, addressee)
		require.NoError(t, err)
		require.True(t, got)
	})

	t.Run("accepted edge, addressee first", func(t *testing.T) {
		got, err := presencehook.AcceptedEdgeExists(ctx, db, addressee, requester)
		require.NoError(t, err)
		require.True(t, got, "the predicate is symmetric; a caller must not have to know the direction")
	})

	t.Run("no edge at all", func(t *testing.T) {
		got, err := presencehook.AcceptedEdgeExists(ctx, db, requester, stranger)
		require.NoError(t, err)
		require.False(t, got)
	})

	t.Run("pending is not accepted", func(t *testing.T) {
		pending := edgeTestUser(t, db)
		_, err := db.Exec(
			`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending')`,
			requester, pending)
		require.NoError(t, err)

		got, err := presencehook.AcceptedEdgeExists(ctx, db, requester, pending)
		require.NoError(t, err)
		require.False(t, got,
			"only an ACCEPTED edge carries presence visibility, so only it can be destroyed by these writes")
	})

	t.Run("blocked is not accepted", func(t *testing.T) {
		blocked := edgeTestUser(t, db)
		_, err := db.Exec(
			`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'blocked')`,
			requester, blocked)
		require.NoError(t, err)

		got, err := presencehook.AcceptedEdgeExists(ctx, db, requester, blocked)
		require.NoError(t, err)
		require.False(t, got)
	})

	t.Run("works on a transaction handle too", func(t *testing.T) {
		// The same function serves BlockUser's authoritative in-transaction read.
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		got, err := presencehook.AcceptedEdgeExists(ctx, tx, requester, addressee)
		require.NoError(t, err)
		require.True(t, got, "RowQuerier must be satisfied by *sql.Tx as well as *sql.DB")
	})
}
