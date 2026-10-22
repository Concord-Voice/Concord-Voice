package migrationrunner_test

import (
	"database/sql"
	"testing"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/migrationrunner"
)

// closeCounter is a source.Driver that only counts Close; RunSource's early
// returns call nothing else on it.
type closeCounter struct {
	source.Driver
	closes int
}

func (c *closeCounter) Close() error { c.closes++; return nil }

// RunSource takes ownership of src whatever it returns: a failure before the
// migrate instance exists closes src exactly once. A closed *sql.DB fails
// postgres.WithInstance with no connection attempt, so the test needs no
// database.
func TestRunSourceClosesSourceOnError(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	src := &closeCounter{}
	err = migrationrunner.RunSource(db, src)
	require.ErrorContains(t, err, "could not create migration driver")
	require.Equal(t, 1, src.closes, "src must be closed exactly once on the early return")
}
