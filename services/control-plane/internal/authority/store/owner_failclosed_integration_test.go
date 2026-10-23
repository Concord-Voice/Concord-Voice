package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// A database failure never reads as a refusal or a success: every owner verb
// and both hold routes answer it with an error that is none of the store's
// verdict sentinels, and with no result. Three failures, each at a different
// depth: the pool is closed (no connection at all), the verb's own call is
// cancelled right after its leader check (the next statement on it fails), and meta is
// locked beyond lock_timeout (the verb's own statements fail inside its
// transaction).

type faultKind int

const (
	poolClosed faultKind = iota
	connectionFails
	metaLocked
)

func (k faultKind) String() string {
	return [...]string{"the pool is closed", "the connection fails after the leader check", "meta is locked"}[k]
}

// faultEnv is one realm with an Owner and a Store over pools of their own (so
// closing them leaves the fixture's), and the failure applied to them.
type faultEnv struct {
	ctx       context.Context
	rs        *restored
	o         *Owner
	s         *Store
	unlock    func()
	snapshot  string
	liveBytes []byte // a hold-req the store would otherwise accept
}

func newFaultEnv(t *testing.T, kind faultKind, unreconciled bool) *faultEnv {
	t.Helper()
	rs := newRestored(t, 1, restoredOpts{})
	e := rs.e
	dsn := authoritydb.URL(t, authoritydb.EnvURL, authoritydb.DBName)
	ownerPool, appPool := authoritydb.OpenAs(t, dsn, authoritydb.RoleOwner), authoritydb.OpenAs(t, dsn, authoritydb.RoleApp)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var hook func(context.Context, *sql.Conn) (bool, error)
	if kind == connectionFails { // a leader check that says primary, then the call is gone
		hook = func(context.Context, *sql.Conn) (bool, error) {
			cancel()
			return false, nil
		}
	}
	o, err := OpenOwner(OwnerConfig{DB: ownerPool, Signer: e.r.RSK, Anchors: e.r.AnchorBytes, InRecovery: hook})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{DB: appPool, Signer: e.r.RSK, Anchors: e.r.AnchorBytes, Probe: &fakeProbe{},
		Hooks: Hooks{InRecovery: hook, Statfs: ampleDisk}, TxDeadline: 5 * time.Second, ReadDeadline: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fe := &faultEnv{ctx: ctx, rs: rs, o: o, s: s, unlock: func() {}, liveBytes: holdReqBytes(t, rs.ahead[0])}
	if unreconciled {
		rs.enterUnreconciled(t)
	}
	fe.snapshot = authoritySnapshot(t, e.f.Super)
	switch kind {
	case poolClosed:
		_ = ownerPool.Close()
		_ = appPool.Close()
	case metaLocked:
		tx, err := e.f.Super.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`LOCK TABLE authority.meta IN ACCESS EXCLUSIVE MODE`); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		fe.unlock = func() { _ = tx.Rollback() }
		t.Cleanup(fe.unlock)
	}
	return fe
}

func TestOwnerVerbsFailClosedOnADatabaseFailure(t *testing.T) {
	digest := make([]byte, 48)
	account := uuid.NewString()
	type verb struct {
		name string
		// onMeta marks a verb whose own transaction reads or writes meta, the
		// only ones a locked meta stops.
		onMeta bool
		// unreconciled puts the realm where reconcile/hold accepts a body.
		unreconciled bool
		// run returns the verb's error and whether it returned a non-zero result.
		run func(fe *faultEnv) (bool, error)
	}
	verbs := []verb{
		{"restore-begin", true, false, func(fe *faultEnv) (bool, error) {
			pt, err := fe.o.RestoreBegin(fe.ctx, nil)
			return !reflect.ValueOf(pt).IsZero(), err
		}},
		{"reconcile-complete", true, false, func(fe *faultEnv) (bool, error) {
			c, err := fe.o.ReconcileComplete(fe.ctx)
			return c != (ReconcileCounts{}), err
		}},
		{"clear-held", false, false, func(fe *faultEnv) (bool, error) {
			r, err := fe.o.ClearHeld(fe.ctx, []string{account})
			return len(r) != 0, err
		}},
		{"clear-held --dry-run", false, false, func(fe *faultEnv) (bool, error) {
			r, err := fe.o.ClearHeldPlan(fe.ctx, []string{account})
			return len(r) != 0, err
		}},
		{"seal-legacy", true, false, func(fe *faultEnv) (bool, error) { return false, fe.o.SealLegacy(fe.ctx, digest) }},
		{"end-drain", false, false, func(fe *faultEnv) (bool, error) { return false, fe.o.EndDrain(fe.ctx, 0, 1) }},
		{"revoke-key", false, false, func(fe *faultEnv) (bool, error) {
			return false, fe.o.RevokeKey(fe.ctx, "cak", digest)
		}},
		{"replication-gate", true, false, func(fe *faultEnv) (bool, error) { return false, fe.o.ReplicationGate(fe.ctx, true) }},
		{"reconcile/hold", true, true, func(fe *faultEnv) (bool, error) {
			out, err := fe.s.ReconcileHold(fe.ctx, fe.liveBytes)
			return out.HTTP != 0 || len(out.Body) != 0, err
		}},
		{"reconcile/required", true, false, func(fe *faultEnv) (bool, error) {
			return false, fe.s.ReconcileRequired(fe.ctx, fe.liveBytes)
		}},
	}
	for _, kind := range []faultKind{poolClosed, connectionFails, metaLocked} {
		for _, v := range verbs {
			if kind == metaLocked && !v.onMeta {
				continue
			}
			t.Run(kind.String()+"/"+v.name, func(t *testing.T) {
				fe := newFaultEnv(t, kind, v.unreconciled)
				result, err := v.run(fe)
				if err == nil {
					t.Fatalf("%s answered success while %s", v.name, kind)
				}
				for _, verdict := range []error{ErrRefused, ErrBadAuthz, ErrReconciled, ErrNotLeader, ErrNotReplicated, ErrServeLockHeld} {
					if errors.Is(err, verdict) {
						t.Fatalf("%s while %s = %v, which reads as the verdict %v", v.name, kind, err, verdict)
					}
				}
				if result {
					t.Fatalf("%s while %s returned a result beside its error %v", v.name, kind, err)
				}
				fe.unlock()
				if after := authoritySnapshot(t, fe.rs.e.f.Super); after != fe.snapshot {
					t.Fatalf("%s while %s wrote:\nbefore %s\nafter  %s", v.name, kind, fe.snapshot, after)
				}
			})
		}
	}
}
