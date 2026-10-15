package store

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// PostgreSQL reports COMMIT success after a cancelled synchronous-replication
// wait. The release gate must still never release the bytes (D-200, LD-2.2).
func TestCancelledSyncWaitIsCommittedLocallyAndNeverReleased(t *testing.T) {
	f := authoritydb.SetupAt(t, authoritydb.EnvGhostURL)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, SQLProbe{}, Hooks{})
	seal(t, f)
	requireGate(t, f, true)
	mustExec(t, f.Super, `ALTER SYSTEM SET synchronous_standby_names = 'ghost'`)
	mustExec(t, f.Super, `SELECT pg_reload_conf()`)
	t.Cleanup(func() {
		_, _ = f.Super.Exec(`ALTER SYSTEM RESET synchronous_standby_names`)
		_, _ = f.Super.Exec(`SELECT pg_reload_conf()`)
	})
	s.txDL = 2 * time.Second
	a := r.NewAccount(t)
	out := s.Submit(context.Background(), a.EnrollNew(t).Bytes)
	if out.Code != string(ac.ErrorReplication) {
		t.Fatalf("a commit whose sync wait was cancelled = %+v, want 503 replication with no bytes", out)
	}
	var n int
	if err := f.Super.QueryRow(`SELECT count(*) FROM authority.account_events WHERE account_id = $1`, a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the event committed locally: count %d, %v (the case the gate exists for)", n, err)
	}
}
