//go:build integration

package expiration

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdatedAtRFC3339 pins the two cases the broadcast actually hits. It needs
// no database, but lives with its sibling so both halves of broadcast.go are
// exercised by one file.
func TestUpdatedAtRFC3339(t *testing.T) {
	// A zero time.Time would serialize as year 1 and render in the client as a
	// real date, which is worse than an absent field — so nil must stay nil
	// rather than becoming a formatted zero value.
	assert.Nil(t, UpdatedAtRFC3339(Policy{}))

	// The stamp is rendered in UTC regardless of the location it was read back
	// in: a pool configured for another timezone must not shift the wire value.
	zone := time.FixedZone("UTC-5", -5*60*60)
	stamped := time.Date(2026, 9, 16, 7, 0, 0, 0, zone)
	assert.Equal(t, "2026-09-16T12:00:00Z", UpdatedAtRFC3339(Policy{UpdatedAt: &stamped}))
}

func TestActorName(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("resolves the username and display name", func(t *testing.T) {
		user := testdb.CreateUser(t, db)
		_, err := db.Exec(`UPDATE users SET display_name = 'Alice Example' WHERE id = $1`, user)
		require.NoError(t, err)

		username, display := ActorName(ctx, db, user.String())
		assert.NotEmpty(t, username, "the broadcast needs a username to render an author")
		assert.Equal(t, "Alice Example", display)
	})

	t.Run("a NULL display name is empty, not a literal", func(t *testing.T) {
		user := testdb.CreateUser(t, db)
		_, err := db.Exec(`UPDATE users SET display_name = NULL WHERE id = $1`, user)
		require.NoError(t, err)

		username, display := ActorName(ctx, db, user.String())
		assert.NotEmpty(t, username)
		assert.Empty(t, display, "a NULL display_name must scan to empty, never to a sentinel")
	})

	// The durable row has already committed by the time the broadcast runs, so a
	// name that cannot be resolved must cost the NAME and nothing else — the
	// renderer falls back to "Someone". Returning an error here would put a
	// users-table read in front of delivering a row that already exists.
	t.Run("an absent user degrades to empty strings", func(t *testing.T) {
		username, display := ActorName(ctx, db, uuid.New().String())
		assert.Empty(t, username)
		assert.Empty(t, display)
	})

	t.Run("a malformed id degrades rather than propagating a driver error", func(t *testing.T) {
		// users.id is a uuid column, so this fails inside PostgreSQL rather than
		// returning no rows — a different error path to the one above, and it
		// must land in the same place.
		username, display := ActorName(ctx, db, "not-a-uuid")
		assert.Empty(t, username)
		assert.Empty(t, display)
	})
}
