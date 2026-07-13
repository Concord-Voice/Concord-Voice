package presencehistory

import (
	"database/sql"
	"testing"

	dbtest "github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
)

type presenceHistoryTestUser struct {
	ID string
}

type presenceHistoryTestServer struct {
	DB *sql.DB
}

func setupPresenceHistoryTestServer(t *testing.T) *presenceHistoryTestServer {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)
	return &presenceHistoryTestServer{DB: db}
}

func (server *presenceHistoryTestServer) CreateTestUser(
	t *testing.T,
	_ string,
) presenceHistoryTestUser {
	t.Helper()
	return presenceHistoryTestUser{ID: dbtest.CreateUser(t, server.DB).String()}
}
