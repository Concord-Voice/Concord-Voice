package channels

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// resolveTargetKeyVersionDM used to discard its Scan error and clamp to 1
// (#1218). That is the one fallback its own doc comment forbids: stamping a
// peer fulfillment at a version no historical message references breaks
// history decryption for the recovering user — and a read error is
// indistinguishable, to the caller, from "brand-new conversation".
//
// The legacy dm.resolveDMDistributionKeyVersion failed closed with a 500 here.
// That handler was deleted in this change, so the fail-closed behaviour has to
// live on the surviving path or it does not exist anywhere.
func TestResolveTargetKeyVersionDM(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	h := &Handler{db: db, log: logger.New("test")}

	t.Run("explicit version wins without touching the database", func(t *testing.T) {
		explicit := 7
		// A nil-db handler proves no query is issued on this path.
		nilDB := &Handler{log: logger.New("test")}
		v, err := nilDB.resolveTargetKeyVersionDM(uuid.NewString(), &explicit)
		require.NoError(t, err)
		assert.Equal(t, 7, v)
	})

	t.Run("unknown conversation defaults to version 1", func(t *testing.T) {
		v, err := h.resolveTargetKeyVersionDM(uuid.NewString(), nil)
		require.NoError(t, err)
		assert.Equal(t, 1, v, "COALESCE(MAX(key_version), 1) with no rows")
	})

	t.Run("a read error fails closed instead of clamping to 1", func(t *testing.T) {
		// sql.Open + Close rather than SetupTestDB + its cleanup. The cleanup
		// runs TruncateAllTables against the SHARED test database, so obtaining
		// a closed handle that way emptied the schema mid-test and released a
		// refcount on the advisory lock. It was contained only by this being
		// the last subtest — any subtest appended after it would have lost its
		// fixtures with no visible cause. sql.Open performs no round trip, so
		// this reaches the same "database is closed" with no blast radius.
		broken, openErr := sql.Open("postgres", dbtest.DatabaseURL())
		require.NoError(t, openErr)
		require.NoError(t, broken.Close())
		bh := &Handler{db: broken, log: logger.New("test")}

		v, err := bh.resolveTargetKeyVersionDM(uuid.NewString(), nil)
		require.Error(t, err,
			"a failed key-version read must surface, not silently resolve to version 1")
		assert.Zero(t, v, "no usable version on the error path")
	})
}
