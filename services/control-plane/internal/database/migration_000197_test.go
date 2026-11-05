package database_test

import (
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000197: the refresh token session binding CHECK refuses half-bound
// pairs that 000195's version admitted through an UNKNOWN result.

const migration000197Constraint = "refresh_tokens_pop_binding_check"

var migration000197JKT = strings.Repeat("a", 43)

func migration000197Token(q migrationExecer, userID uuid.UUID, jkt, alg any) error {
	_, err := q.Exec(`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, pop_jkt, pop_alg)
		VALUES ($1, $2, NOW() + INTERVAL '1 hour', $3, $4)`,
		userID, uuid.NewString(), jkt, alg)
	return err
}

func TestMigration000197_RefusesHalfBoundPairs(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := testhelpers.CreateUser(t, db)

	require.NoError(t, migration000197Token(db, user, nil, nil), "unbound")
	require.NoError(t, migration000197Token(db, user, migration000197JKT, "ES256"), "bound")
	for name, pair := range map[string][2]any{
		"alg without jkt": {nil, "ES256"},
		"jkt without alg": {migration000197JKT, nil},
		"other alg":       {migration000197JKT, "RS256"},
		"short jkt":       {"abc", "ES256"},
	} {
		t.Run(name, func(t *testing.T) {
			migrationRequirePQError(t, migration000197Token(db, user, pair[0], pair[1]), "23514", migration000197Constraint)
		})
	}
}

func TestMigration000197_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := testhelpers.CreateUser(t, db)
	up := migrationReadFile(t, "../../migrations/000197_tighten_refresh_token_session_binding_check.up.sql")
	down := migrationReadFile(t, "../../migrations/000197_tighten_refresh_token_session_binding_check.down.sql")

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "refresh_tokens")
	_, err = tx.Exec(down)
	require.NoError(t, err)
	// The down restores 000195's check, which admits the half-bound pair.
	require.NoError(t, migration000197Token(tx, user, nil, "ES256"))
	assert.NotEqual(t, applied, migrationSchemaSnapshot(t, tx, "refresh_tokens"))
	_, err = tx.Exec(`DELETE FROM refresh_tokens WHERE user_id = $1`, user)
	require.NoError(t, err)

	_, err = tx.Exec(up)
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "refresh_tokens"))
}

func TestMigration000197_UpRefusesAnExistingHalfBoundRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := testhelpers.CreateUser(t, db)
	up := migrationReadFile(t, "../../migrations/000197_tighten_refresh_token_session_binding_check.up.sql")
	down := migrationReadFile(t, "../../migrations/000197_tighten_refresh_token_session_binding_check.down.sql")

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	_, err = tx.Exec(down)
	require.NoError(t, err)
	require.NoError(t, migration000197Token(tx, user, migration000197JKT, nil))
	// A NOT VALID check would admit the old row but refuse every later
	// revocation of it, so the up refuses to run over one.
	_, err = tx.Exec(up)
	migrationRequirePQError(t, err, "23514", "")
}
