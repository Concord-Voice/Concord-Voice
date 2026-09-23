package members

import (
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// populateRBAcRoles must report a failed roles query instead of leaving members
// role-less. ListMembers turns the error into a 500, because `roles: []` reads as
// "this member has no roles" to a client that builds assign/unassign from it.
func TestPopulateRBAcRolesReportsQueryFailure(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://unused@127.0.0.1:1/unused?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close()) // every query now fails without touching the network

	members := []MemberWithUser{{UserID: "u1"}}
	err = (&Handler{db: db, log: logger.New("test")}).populateRBAcRoles("s1", members)
	require.ErrorContains(t, err, "query member roles")
	require.Nil(t, members[0].Roles)
}
