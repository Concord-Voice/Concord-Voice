package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// Task 6 (plan) as extended by the orchestrator: reconcile/required and
// reconcile/hold (DoR:525-530, 1610-1611, 1728, 1771; D-287; B3-D6), and the
// one prologue the four operator routes share (DoR:196, 1431, 1596).
//
// The error-carrying design under test: a refusal that the server must put on
// the wire with its own ac code (503 not_leader, disk, busy, replication) is
// returned as the VALUE store.OutcomeError{Outcome}, possibly wrapped with %w.
// Callers find it with errors.As(err, &store.OutcomeError{}). Its Error() has a
// value receiver, so both the value and a pointer to it satisfy error, but the
// store returns the value.

// ---- the restored-database fixture ----

// restored is a database that a restore moved BACK from the CP's view: every
// account is at seq 1 (its head and its seq-1 bundle), while the CP retained a
// seq-2 bundle the authority has never seen. ahead[i] is that retained bundle;
// atHead[i] is the head's own bundle, which is NOT ahead of the head.
// Accounts are in ascending account_id order, the order a hold-req requires.
type restored struct {
	e      *env
	accts  []*authoritytest.Account
	ahead  [][]byte
	atHead [][]byte
}

type restoredOpts struct {
	// newDelegation signs the retained bundles under a delegation that is then
	// removed from the store: a bundle under an uninstalled delegation.
	newDelegation bool
	// otherRealm moves meta to a realm the bundles were not signed for.
	otherRealm bool
}

func newRestored(t *testing.T, n int, o restoredOpts) *restored {
	t.Helper()
	e := newEnv(t)
	rs := &restored{e: e}
	for range n {
		a, _ := e.enrolledWith()
		rs.accts = append(rs.accts, a)
	}
	sort.Slice(rs.accts, func(i, j int) bool { return rs.accts[i].ID < rs.accts[j].ID })
	for _, a := range rs.accts {
		rs.atHead = append(rs.atHead, eventBundle(t, e, a.ID, 1))
	}
	mustExec(t, e.f.Super, `DROP TABLE IF EXISTS public.zz_snap_heads`)
	mustExec(t, e.f.Super, `CREATE TABLE public.zz_snap_heads AS SELECT * FROM authority.account_heads`)
	t.Cleanup(func() { _, _ = e.f.Super.Exec(`DROP TABLE IF EXISTS public.zz_snap_heads`) })

	var next authoritytest.SignedDelegation
	if o.newDelegation {
		next = e.r.NextDelegation(t)
		e.r.Install(t, e.f.App, next)
	}
	for _, a := range rs.accts {
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
		rs.ahead = append(rs.ahead, eventBundle(t, e, a.ID, 2))
	}

	// The restore: the heads go back to their seq-1 rows and the seq-2 events
	// are gone. A replica-role transaction skips the schema's append-only
	// guards, as the harness's own reset does.
	mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
		DELETE FROM authority.account_events WHERE seq = 2;
		DELETE FROM authority.account_heads;
		INSERT INTO authority.account_heads SELECT * FROM public.zz_snap_heads;
		COMMIT`)
	if o.newDelegation {
		mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
			DELETE FROM authority.successions WHERE true;
			DELETE FROM authority.delegations WHERE serial = `+strconv.FormatUint(next.D.Serial, 10)+`; COMMIT`)
	}
	if o.otherRealm {
		mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
			UPDATE authority.meta SET realm_id = '`+uuid.NewString()+`'; COMMIT`)
	}
	for _, a := range rs.accts {
		a.Sync(t, e.f.App)
		if a.Head.Seq != 1 {
			t.Fatalf("the restored head of %s is at seq %d, want 1", a.ID, a.Head.Seq)
		}
	}
	return rs
}

func eventBundle(t *testing.T, e *env, account string, seq int) []byte {
	t.Helper()
	var b []byte
	if err := e.f.App.QueryRow(`SELECT bundle FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
		account, seq).Scan(&b); err != nil {
		t.Fatalf("bundle %d of %s: %v", seq, account, err)
	}
	return b
}

func holdReqBytes(t *testing.T, bundles ...[]byte) []byte {
	t.Helper()
	b := ac.HoldReq{Bundles: bundles}.Encode()
	if b == nil {
		t.Fatal("the test hold-req does not encode")
	}
	return b
}

func (rs *restored) reconciled(t *testing.T) bool {
	t.Helper()
	var on bool
	if err := rs.e.f.App.QueryRow(`SELECT reconciled FROM authority.meta WHERE id = 1`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on
}

func (rs *restored) heldAccounts(t *testing.T) []string {
	t.Helper()
	rows, err := rs.e.f.App.Query(`SELECT account_id::text FROM authority.account_heads WHERE held ORDER BY account_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func (rs *restored) enterUnreconciled(t *testing.T) {
	t.Helper()
	mustExec(t, rs.e.f.Owner, `SELECT authority.enter_unreconciled()`)
}

// ---- reconcile/required ----

func TestRequiredEntersUnreconciled(t *testing.T) {
	rs := newRestored(t, 1, restoredOpts{})
	if !rs.reconciled(t) {
		t.Fatal("the fixture starts unreconciled (the control)")
	}
	if err := rs.e.s.ReconcileRequired(liveCtx(t), holdReqBytes(t, rs.ahead[0])); err != nil {
		t.Fatalf("ReconcileRequired with one verified bundle ahead = %v, want nil", err)
	}
	if rs.reconciled(t) {
		t.Fatal("meta.reconciled is still true after reconcile/required")
	}
	if held := rs.heldAccounts(t); len(held) != 0 {
		t.Fatalf("reconcile/required held %v: only reconcile/hold holds", held)
	}
}

// DoR:1771: an unproven body is bad_authz, the realm stays reconciled, and
// nothing is held, on either route.
func TestRequiredAndHoldRefuseUnproven(t *testing.T) {
	cases := []struct {
		name string
		opts restoredOpts
		n    int
		body func(t *testing.T, rs *restored) []byte
	}{
		{"a bundle not ahead of the head", restoredOpts{}, 1, func(t *testing.T, rs *restored) []byte {
			return holdReqBytes(t, rs.atHead[0])
		}},
		{"a bundle under an uninstalled delegation", restoredOpts{newDelegation: true}, 1,
			func(t *testing.T, rs *restored) []byte { return holdReqBytes(t, rs.ahead[0]) }},
		{"a bundle for another realm", restoredOpts{otherRealm: true}, 1,
			func(t *testing.T, rs *restored) []byte { return holdReqBytes(t, rs.ahead[0]) }},
		{"one good bundle and one bad", restoredOpts{}, 2, func(t *testing.T, rs *restored) []byte {
			return holdReqBytes(t, rs.ahead[0], rs.atHead[1])
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := newRestored(t, c.n, c.opts)
			body := c.body(t, rs)

			err := rs.e.s.ReconcileRequired(liveCtx(t), body)
			if !errors.Is(err, ErrBadAuthz) {
				t.Fatalf("ReconcileRequired = %v, want ErrBadAuthz", err)
			}
			if !rs.reconciled(t) {
				t.Fatal("a refused reconcile/required left the realm unreconciled")
			}

			// Hold takes the body only while unreconciled; reach that state
			// directly so the refusal under test is the bundle's, not the state's.
			rs.enterUnreconciled(t)
			_, err = rs.e.s.ReconcileHold(liveCtx(t), body)
			if !errors.Is(err, ErrBadAuthz) {
				t.Fatalf("ReconcileHold = %v, want ErrBadAuthz", err)
			}
			if held := rs.heldAccounts(t); len(held) != 0 {
				t.Fatalf("a refused request held %v, want nothing", held)
			}
		})
	}
}

// ---- reconcile/hold ----

func TestHoldOnlyWhileUnreconciled(t *testing.T) {
	t.Run("reconciled is ErrReconciled and holds nothing", func(t *testing.T) {
		rs := newRestored(t, 1, restoredOpts{})
		_, err := rs.e.s.ReconcileHold(liveCtx(t), holdReqBytes(t, rs.ahead[0]))
		if !errors.Is(err, ErrReconciled) {
			t.Fatalf("ReconcileHold while reconciled = %v, want ErrReconciled", err)
		}
		if held := rs.heldAccounts(t); len(held) != 0 {
			t.Fatalf("held %v while reconciled", held)
		}
	})

	t.Run("two good bundles hold both accounts", func(t *testing.T) {
		rs := newRestored(t, 2, restoredOpts{})
		rs.enterUnreconciled(t)
		out, err := rs.e.s.ReconcileHold(liveCtx(t), holdReqBytes(t, rs.ahead[0], rs.ahead[1]))
		if err != nil {
			t.Fatalf("ReconcileHold = %v, want nil", err)
		}
		if out.HTTP != 200 || len(out.Body) != 0 {
			t.Fatalf("ReconcileHold outcome = %+v, want 200 with an empty body (B3-D14)", out)
		}
		want := []string{rs.accts[0].ID, rs.accts[1].ID}
		if held := rs.heldAccounts(t); len(held) != 2 || held[0] != want[0] || held[1] != want[1] {
			t.Fatalf("held = %v, want %v", held, want)
		}
	})

	t.Run("a bad third bundle holds neither of the good two", func(t *testing.T) {
		rs := newRestored(t, 3, restoredOpts{})
		rs.enterUnreconciled(t)
		_, err := rs.e.s.ReconcileHold(liveCtx(t), holdReqBytes(t, rs.ahead[0], rs.ahead[1], rs.atHead[2]))
		if !errors.Is(err, ErrBadAuthz) {
			t.Fatalf("ReconcileHold = %v, want ErrBadAuthz", err)
		}
		if held := rs.heldAccounts(t); len(held) != 0 {
			t.Fatalf("held %v: the request is one transaction, all or nothing", held)
		}
	})
}

// ---- the prologue the four operator routes share ----

// wantOutcomeError requires err to carry the store's own Outcome: the status
// and ac code the server must put on the wire, with the code's envelope.
func wantOutcomeError(t *testing.T, label string, err error, http int, code ac.ErrorCode) {
	t.Helper()
	var oe OutcomeError
	if !errors.As(err, &oe) {
		t.Fatalf("%s: err = %v, want a store.OutcomeError", label, err)
	}
	if oe.Outcome.HTTP != http || oe.Outcome.Kind != KindError || oe.Outcome.Code != string(code) {
		t.Fatalf("%s: outcome = %+v, want %d %s", label, oe.Outcome, http, code)
	}
	if !bytes.Equal(oe.Outcome.Body, ac.ErrorEnvelope{Code: string(code)}.Encode()) {
		t.Fatalf("%s: the outcome body is not the %s envelope", label, code)
	}
	if errors.Is(err, ErrBadAuthz) || errors.Is(err, ErrSealed) || errors.Is(err, ErrReconciled) {
		t.Fatalf("%s: %v also matches a 400 or 409 sentinel", label, err)
	}
}

// openWith is a second store over the same database with the seams a case
// needs. It takes the realm's signer and anchors, so it verifies what the
// fixture's own store committed.
func openWith(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, hooks Hooks, probe ReplicationProbe,
	adm *Admission) *Store {
	t.Helper()
	if hooks.Statfs == nil {
		hooks.Statfs = func() (uint64, error) { return 1 << 40, nil }
	}
	s, err := Open(Config{DB: f.App, Signer: r.RSK, Anchors: r.AnchorBytes, Probe: probe, Admission: adm, Hooks: hooks,
		TxDeadline: 400 * time.Millisecond, ReadDeadline: 400 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOperatorRoutesSharePrologue(t *testing.T) {
	standby := Hooks{InRecovery: func(context.Context, *sql.Conn) (bool, error) { return true, nil }}
	lowDisk := Hooks{Statfs: func() (uint64, error) { return DiskHardFloorBytes - 1, nil }}

	type route struct {
		name string
		// setup returns the fixture's db handles and the body the route takes,
		// and puts the database in the state in which the route would commit.
		setup func(t *testing.T) (*authoritydb.Fixture, *authoritytest.Realm, []byte)
		call  func(s *Store, ctx context.Context, body []byte) error
		// committed reports that the route's write is in the database.
		committed func(t *testing.T, f *authoritydb.Fixture) bool
	}
	routes := []route{
		{
			name: "InsertLegacyPopulation",
			setup: func(t *testing.T) (*authoritydb.Fixture, *authoritytest.Realm, []byte) {
				e := newLiveEnv(t, OwnerConfig{}) // unsealed
				return e.f, e.r, legacyBatchBytes(t, legacyPop(1))
			},
			call: func(s *Store, ctx context.Context, b []byte) error { return s.InsertLegacyPopulation(ctx, b) },
			committed: func(t *testing.T, f *authoritydb.Fixture) bool {
				return populationCount(t, f.App) == 1
			},
		},
		{
			name: "ReconcileRequired",
			setup: func(t *testing.T) (*authoritydb.Fixture, *authoritytest.Realm, []byte) {
				rs := newRestored(t, 1, restoredOpts{})
				return rs.e.f, rs.e.r, holdReqBytes(t, rs.ahead[0])
			},
			call: func(s *Store, ctx context.Context, b []byte) error { return s.ReconcileRequired(ctx, b) },
			committed: func(t *testing.T, f *authoritydb.Fixture) bool {
				return !(&restored{e: &env{f: f}}).reconciled(t)
			},
		},
		{
			name: "ReconcileHold",
			setup: func(t *testing.T) (*authoritydb.Fixture, *authoritytest.Realm, []byte) {
				rs := newRestored(t, 1, restoredOpts{})
				rs.enterUnreconciled(t)
				return rs.e.f, rs.e.r, holdReqBytes(t, rs.ahead[0])
			},
			call: func(s *Store, ctx context.Context, b []byte) error {
				_, err := s.ReconcileHold(ctx, b)
				return err
			},
			committed: func(t *testing.T, f *authoritydb.Fixture) bool {
				return len((&restored{e: &env{f: f}}).heldAccounts(t)) == 1
			},
		},
		{
			name: "ReconcileImport",
			setup: func(t *testing.T) (*authoritydb.Fixture, *authoritytest.Realm, []byte) {
				e := newEnv(t)
				_, it := oneAhead(t, e)
				beginImport(t, e, restorePoint{})
				return e.f, e.r, importBatch(t, it)
			},
			call: func(s *Store, ctx context.Context, b []byte) error {
				_, err := s.ReconcileImport(ctx, b)
				return err
			},
			committed: func(t *testing.T, f *authoritydb.Fixture) bool {
				var n int
				if err := f.App.QueryRow(`SELECT count(*) FROM authority.account_events WHERE imported`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n == 1
			},
		},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			f, r, body := rt.setup(t)

			t.Run("a standby is not_leader", func(t *testing.T) {
				s := openWith(t, f, r, standby, &fakeProbe{}, nil)
				wantOutcomeError(t, "standby", rt.call(s, liveCtx(t), body), 503, ac.ErrorNotLeader)
				if rt.committed(t, f) {
					t.Fatal("a standby wrote")
				}
			})

			t.Run("below the disk hard floor is disk", func(t *testing.T) {
				s := openWith(t, f, r, lowDisk, &fakeProbe{}, nil)
				wantOutcomeError(t, "low disk", rt.call(s, liveCtx(t), body), 503, ac.ErrorDisk)
				if rt.committed(t, f) {
					t.Fatal("a refusal for disk wrote")
				}
			})

			t.Run("a failing Statfs fails closed as disk", func(t *testing.T) {
				s := openWith(t, f, r, Hooks{Statfs: func() (uint64, error) { return 0, errors.New("statfs") }},
					&fakeProbe{}, nil)
				wantOutcomeError(t, "statfs error", rt.call(s, liveCtx(t), body), 503, ac.ErrorDisk)
			})

			t.Run("no general slot free is busy, and the reserves are not an operator's", func(t *testing.T) {
				adm := NewAdmission()
				for {
					rel, ok := adm.acquireGeneral()
					if !ok {
						break
					}
					defer rel()
				}
				s := openWith(t, f, r, Hooks{}, &fakeProbe{}, adm)
				wantOutcomeError(t, "busy", rt.call(s, liveCtx(t), body), 503, ac.ErrorBusy)
				if rt.committed(t, f) {
					t.Fatal("a refusal for busy wrote")
				}
			})

			t.Run("the slot is given back", func(t *testing.T) {
				adm := NewAdmission()
				s := openWith(t, f, r, standby, &fakeProbe{}, adm) // refuses at step (a), after the slot
				_ = rt.call(s, liveCtx(t), body)
				held := 0
				for {
					rel, ok := adm.acquireGeneral()
					if !ok {
						break
					}
					held++
					defer rel()
				}
				if held != MutatingSlots-1 {
					t.Fatalf("%d general slots free after a refused call, want %d", held, MutatingSlots-1)
				}
			})

			t.Run("an unconfirmed commit is replication, and it stays committed", func(t *testing.T) {
				requireGate(t, f, true)
				probe := &fakeProbe{}
				s := openWith(t, f, r, Hooks{}, probe, nil)
				wantOutcomeError(t, "replication", rt.call(s, liveCtx(t), body), 503, ac.ErrorReplication)
				if probe.calls.Load() == 0 {
					t.Fatal("the route never asked the replication probe")
				}
				if !rt.committed(t, f) {
					t.Fatal("the write was not committed locally before the release gate (the CP's retry converges)")
				}
			})
		})
	}
}

// DoR:1596: an operator route is not a user op, so it takes no commit-bucket
// token: a drained bucket must not turn into 503 busy.
func TestOperatorRoutesTakeNoCommitToken(t *testing.T) {
	e := newLiveEnv(t, OwnerConfig{})
	frozen := time.Now()
	adm := newAdmissionAt(func() time.Time { return frozen }) // the bucket never refills
	for adm.takeCommitToken(classOther) {
	}
	s := openWith(t, e.f, e.r, Hooks{}, &fakeProbe{}, adm)
	if err := s.InsertLegacyPopulation(liveCtx(t), legacyBatchBytes(t, legacyPop(2))); err != nil {
		t.Fatalf("InsertLegacyPopulation with a drained commit bucket = %v, want nil", err)
	}
	if n := populationCount(t, e.f.App); n != 2 {
		t.Fatalf("legacy_population holds %d rows, want 2", n)
	}
}

// ---- B3-D15 R1: hold-req authenticates; it does not re-fold history ----
//
// A hold-req bundle is the retained RSK-signed bundle with the HIGHEST seq the
// CP holds (DoR:527), so it is routinely several seqs past the restored head, may
// sit under a delegation a later renewal superseded, and may belong to a
// legacy-population account whose history needs the pin. None of that is a
// reason to refuse it: ac.AuthenticateBundle needs no head, and the store only
// requires the verified result's seq to exceed the head's.

// uuidList is the quoted, comma-separated list of ids for an IN (...) clause;
// uuid.MustParse keeps anything but a UUID out of the SQL.
func uuidList(ids ...string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = "'" + uuid.MustParse(id).String() + "'"
	}
	return strings.Join(q, ",")
}

// snapshotHeads copies authority.account_heads to public.<name> (dropped at the
// end of the test): the restore point restoreTo moves accounts back to.
func snapshotHeads(t *testing.T, e *env, name string) {
	t.Helper()
	mustExec(t, e.f.Super, `DROP TABLE IF EXISTS public.`+name)
	mustExec(t, e.f.Super, `CREATE TABLE public.`+name+` AS SELECT * FROM authority.account_heads`)
	t.Cleanup(func() { _, _ = e.f.Super.Exec(`DROP TABLE IF EXISTS public.` + name) })
}

// restoreTo moves the accounts back to the snapshot: events past the
// snapshot's seq are gone (all of them for an account the snapshot lacks, which
// then reads as seq 0) and the head row is the snapshot's. A replica-role
// transaction skips the schema's append-only guards, as newRestored's does.
func restoreTo(t *testing.T, e *env, snap string, ids ...string) {
	t.Helper()
	l := uuidList(ids...)
	mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
		DELETE FROM authority.account_events ev WHERE ev.account_id IN (`+l+`)
		  AND ev.seq > COALESCE((SELECT s.seq FROM public.`+snap+` s WHERE s.account_id = ev.account_id), 0);
		DELETE FROM authority.account_heads WHERE account_id IN (`+l+`);
		INSERT INTO authority.account_heads SELECT * FROM public.`+snap+` WHERE account_id IN (`+l+`);
		COMMIT`)
}

func heldIDs(t *testing.T, e *env) []string { return (&restored{e: e}).heldAccounts(t) }

func sortedByID(accts []*authoritytest.Account) {
	sort.Slice(accts, func(i, j int) bool { return accts[i].ID < accts[j].ID })
}

// requireRequiredThenHold sends body to reconcile/required and then to
// reconcile/hold (which takes the unreconciled state required leaves), and
// requires both to succeed and hold exactly want, in account order. Required
// holds nothing by itself.
func requireRequiredThenHold(t *testing.T, e *env, body []byte, want []string) {
	t.Helper()
	rs := &restored{e: e}
	if err := e.s.ReconcileRequired(liveCtx(t), body); err != nil {
		t.Fatalf("ReconcileRequired = %v, want nil", err)
	}
	if rs.reconciled(t) {
		t.Fatal("reconcile/required left the realm reconciled")
	}
	if held := heldIDs(t, e); len(held) != 0 {
		t.Fatalf("reconcile/required held %v: only reconcile/hold holds", held)
	}
	if _, err := e.s.ReconcileHold(liveCtx(t), body); err != nil {
		t.Fatalf("ReconcileHold = %v, want nil", err)
	}
	if held := heldIDs(t, e); strings.Join(held, ",") != strings.Join(want, ",") {
		t.Fatalf("held = %v, want %v", held, want)
	}
}

// requireRefusedAndNothingHeld sends body to both routes and requires
// ErrBadAuthz from each, the realm still reconciled after required, and
// nothing held after hold.
func requireRefusedAndNothingHeld(t *testing.T, e *env, body []byte) {
	t.Helper()
	rs := &restored{e: e}
	mustExec(t, e.f.Super, `UPDATE authority.meta SET reconciled = true`) // a defined start, whatever the last case left
	if err := e.s.ReconcileRequired(liveCtx(t), body); !errors.Is(err, ErrBadAuthz) {
		t.Fatalf("ReconcileRequired = %v, want ErrBadAuthz", err)
	}
	if !rs.reconciled(t) {
		t.Fatal("a refused reconcile/required left the realm unreconciled")
	}
	rs.enterUnreconciled(t) // hold takes a body only while unreconciled: the refusal under test is the bundle's
	if _, err := e.s.ReconcileHold(liveCtx(t), body); !errors.Is(err, ErrBadAuthz) {
		t.Fatalf("ReconcileHold = %v, want ErrBadAuthz", err)
	}
	if held := heldIDs(t, e); len(held) != 0 {
		t.Fatalf("a refused request held %v, want nothing", held)
	}
}

// newSeveralAhead enrolls one account per depth (all at seq 1, in ascending
// account_id order), commits operator suspend/unsuspend events on account i up
// to seq depths[i], keeps that account's seq-depths[i] bundle, and restores every
// account to its seq-1 head. bundles[i] is the highest bundle the CP retained
// for accts[i]: depths[i] - 1 seqs past the restored head.
func newSeveralAhead(t *testing.T, depths ...int) (e *env, accts []*authoritytest.Account, bundles [][]byte) {
	t.Helper()
	e = newEnv(t)
	for range depths {
		a, _ := e.enrolledWith()
		accts = append(accts, a)
	}
	sortedByID(accts)
	snapshotHeads(t, e, "zz_deep_heads")
	ids := make([]string, 0, len(accts))
	for i, a := range accts {
		for seq := 2; seq <= depths[i]; seq++ {
			if seq%2 == 0 {
				e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
			} else {
				e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes)
			}
		}
		bundles = append(bundles, eventBundle(t, e, a.ID, depths[i]))
		ids = append(ids, a.ID)
	}
	restoreTo(t, e, "zz_deep_heads", ids...)
	for _, a := range accts {
		a.Sync(t, e.f.App)
		if a.Head.Seq != 1 {
			t.Fatalf("the restored head of %s is at seq %d, want 1", a.ID, a.Head.Seq)
		}
	}
	return e, accts, bundles
}

// A hold-req bundle several seqs ahead of the head authenticates: its
// prev_entry_digest names an entry this authority never stored, so a link to
// the head is neither checkable nor claimed (DoR:527 says "highest", not
// "head + 1"). Before B3-D15 it was chain_gap, and the accounts that lost the
// most could never be held.
func TestHoldAcceptsBundleSeveralAhead(t *testing.T) {
	e, accts, bundles := newSeveralAhead(t, 3, 4) // head seq 1; bundles at seq 3 and seq 4
	body := holdReqBytes(t, bundles...)
	requireRequiredThenHold(t, e, body, []string{accts[0].ID, accts[1].ID})
}

// A legacy-population account's first result is a PoP enroll_existing that
// only the account's pin can explain (V3-05: the seq-0 pin is AccountMemory's,
// and VerifyBundle has none). Its restored head is at seq 0 (no row, or the
// genesis row lockHead writes) or at seq 1; either way a bundle ahead of it is
// authenticated without that pin.
func TestHoldPinnedAccount(t *testing.T) {
	e, accts := newEnvPop(t, 2)
	sortedByID(accts)
	snapshotHeads(t, e, "zz_pin_heads0") // no head rows yet: every account is at seq 0
	for _, a := range accts {
		r := e.commitOK(a, a.EnrollExisting(t, true, ac.FactorPassword).Bytes)
		if r.Seq != 1 || r.State != ac.StatePending {
			t.Fatalf("the PoP enroll_existing of %s = seq %d %s, want seq 1 pending", a.ID, r.Seq, r.State)
		}
	}
	snapshotHeads(t, e, "zz_pin_heads1")
	e.commitOK(accts[1], accts[1].CancelByPoP(t).Bytes) // seq 2
	retained := [][]byte{eventBundle(t, e, accts[0].ID, 1), eventBundle(t, e, accts[1].ID, 2)}

	restoreTo(t, e, "zz_pin_heads0", accts[0].ID) // reset to seq 0: the retained bundle is the PoP result itself
	restoreTo(t, e, "zz_pin_heads1", accts[1].ID) // reset to seq 1
	requireRequiredThenHold(t, e, holdReqBytes(t, retained...), []string{accts[0].ID, accts[1].ID})
}

// T11 sets min_valid_serial at every renewal, so after one the history an
// account stored under serial s is superseded. A hold bundle under s + 1
// authenticates whatever its account's history looks like; a bundle under s is
// itself superseded and stays refused, so a leaked retired RSK cannot take the
// realm unreconciled (B3-D15 R1).
func TestHoldAfterRenewal(t *testing.T) {
	e := newEnv(t)
	accts := make([]*authoritytest.Account, 0, 2)
	for range 2 {
		a, _ := e.enrolledWith() // seq 1 under serial s
		accts = append(accts, a)
	}
	sortedByID(accts)
	snapshotHeads(t, e, "zz_renew_heads")
	e.commitOK(accts[0], accts[0].Suspend(t, ac.ActorOperator).Bytes)
	underS := eventBundle(t, e, accts[0].ID, 2)

	next := e.r.NextDelegation(t) // serial s + 1, min_valid_serial = s + 1
	e.r.Install(t, e.f.App, next)
	e.commitOK(accts[1], accts[1].Suspend(t, ac.ActorOperator).Bytes)
	underNext := eventBundle(t, e, accts[1].ID, 2)
	restoreTo(t, e, "zz_renew_heads", accts[0].ID, accts[1].ID)

	t.Run("a bundle under the superseded serial is refused and holds nothing", func(t *testing.T) {
		requireRefusedAndNothingHeld(t, e, holdReqBytes(t, underS))
	})
	t.Run("one superseded bundle refuses the whole request", func(t *testing.T) {
		requireRefusedAndNothingHeld(t, e, holdReqBytes(t, underS, underNext))
	})
	t.Run("a bundle under the renewed serial holds, whatever its history", func(t *testing.T) {
		requireRequiredThenHold(t, e, holdReqBytes(t, underNext), []string{accts[1].ID})
	})
}

// A hold-req bundle is judged as a whole: a validly RSK-signed result beside
// an intent that is not the one it binds is refused, because the authority
// signed a different request. The account and seq the store acts on come from
// the verified result, so no part of the bundle may disagree with it.
func TestHoldRefusesInconsistentBundle(t *testing.T) {
	swap := func(t *testing.T, ahead []byte, intent []byte) []byte {
		t.Helper()
		b, err := ac.DecodeBundle(ahead)
		if err != nil {
			t.Fatal(err)
		}
		b.Intent = intent
		out := b.Encode()
		if out == nil {
			t.Fatal("the swapped bundle does not encode")
		}
		return out
	}
	intentOf := func(t *testing.T, raw []byte) []byte {
		t.Helper()
		b, err := ac.DecodeBundle(raw)
		if err != nil {
			t.Fatal(err)
		}
		return b.Intent
	}

	t.Run("the intent of another account", func(t *testing.T) {
		rs := newRestored(t, 2, restoredOpts{})
		bad := swap(t, rs.ahead[0], intentOf(t, rs.ahead[1]))
		requireRefusedAndNothingHeld(t, rs.e, holdReqBytes(t, bad))
	})
	t.Run("an earlier intent of the same account", func(t *testing.T) {
		rs := newRestored(t, 1, restoredOpts{})
		bad := swap(t, rs.ahead[0], intentOf(t, rs.atHead[0]))
		requireRefusedAndNothingHeld(t, rs.e, holdReqBytes(t, bad))
	})
}

// Every bundle is authenticated before any head is read or locked
// (aheadBundles step 1), so a request one bundle of which does not
// authenticate is refused without waiting on, or creating, any account's head.
// The first account's head is held elsewhere and the refused bundle is the
// second: a store that locks as it goes, or locks every head before it
// authenticates, waits on the held row, and its 2-s lock timeout is a database
// error, not ErrBadAuthz.
func TestHoldAuthenticatesBeforeLockingAnyHead(t *testing.T) {
	rs := newRestored(t, 2, restoredOpts{})
	b, err := ac.DecodeBundle(rs.ahead[1])
	if err != nil {
		t.Fatal(err)
	}
	b.ResultSig = append([]byte(nil), b.ResultSig...)
	b.ResultSig[len(b.ResultSig)-1] ^= 1 // bad_signature; the hold-req order still holds
	bad := b.Encode()
	if bad == nil {
		t.Fatal("the broken bundle does not encode")
	}
	body := holdReqBytes(t, rs.ahead[0], bad)
	beginHolding(t, rs.e.f.Super, `SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, rs.accts[0].ID)

	within := func(label string, call func() error) {
		t.Helper()
		start := time.Now()
		err := call()
		if !errors.Is(err, ErrBadAuthz) {
			t.Fatalf("%s with the first head held elsewhere = %v, want ErrBadAuthz: the refused bundle was judged after a lock", label, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%s took %v: it waited on a head before refusing", label, d)
		}
	}
	within("ReconcileRequired", func() error { return rs.e.s.ReconcileRequired(liveCtx(t), body) })
	if !rs.reconciled(t) {
		t.Fatal("a refused reconcile/required left the realm unreconciled")
	}
	rs.enterUnreconciled(t)
	within("ReconcileHold", func() error {
		_, err := rs.e.s.ReconcileHold(liveCtx(t), body)
		return err
	})
}

// ---- lock order: purge_erased against reconcile/required ----

// purge_erased takes meta FOR SHARE and then the account's head row (L1,
// FOR UPDATE; B1-D30). ReconcileRequired takes the heads its bundles name and
// then updates meta (enter_unreconciled). Taken in opposite orders those two
// deadlock, SQLSTATE 40P01, when a hold-req names an erased account the purge
// is about to reap. The test fixes no lock order, only that none may invert:
// whichever the store settles on, the pair must finish.
//
// The interleaving is forced rather than hoped for. An explicit transaction
// holds a row; ReconcileRequired is started and seen waiting (pg_locks: the
// fixture's SET SESSION AUTHORIZATION hides the role in pg_stat_activity), then
// the purge is started and seen waiting; the row is then released, and the
// side that queued first takes it first. Each held row catches one direction
// of inversion, so the test runs both:
//
//   - the head row: the required side takes the head first, so a purge that
//     holds meta FOR SHARE while it waits for the head deadlocks it;
//   - meta: the required side takes meta first, so a reconcile/required that
//     locks meta before the heads deadlocks against a purge holding the head.
func TestPurgeAndRequiredDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
	e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes)
	ahead := eventBundle(t, e, a.ID, 3)
	e.commitOK(a, a.Erase(t, ac.ActorOperator).Bytes) // seq 4, the last event: purge_erased's gate (3)
	// A restore left the head at seq 1 while the CP retained a seq-3 bundle; the
	// erase is due. A replica-role transaction skips the append-only guards.
	l := uuidList(a.ID)
	mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE authority.account_heads SET seq = 1, erased_at = now() - make_interval(days => 31)
		WHERE account_id IN (`+l+`); COMMIT`)
	mustExec(t, e.f.Super, `DROP TABLE IF EXISTS public.zz_dl_heads, public.zz_dl_events`)
	mustExec(t, e.f.Super, `CREATE TABLE public.zz_dl_heads AS SELECT * FROM authority.account_heads WHERE account_id IN (`+l+`)`)
	mustExec(t, e.f.Super, `CREATE TABLE public.zz_dl_events AS SELECT * FROM authority.account_events WHERE account_id IN (`+l+`)`)
	t.Cleanup(func() {
		_, _ = e.f.Super.Exec(`DROP TABLE IF EXISTS public.zz_dl_heads`)
		_, _ = e.f.Super.Exec(`DROP TABLE IF EXISTS public.zz_dl_events`)
	})
	reset := func() { // a round consumes the fixture: events purged, realm unreconciled
		mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
			DELETE FROM authority.account_events WHERE account_id IN (`+l+`);
			DELETE FROM authority.account_heads WHERE account_id IN (`+l+`);
			INSERT INTO authority.account_heads SELECT * FROM public.zz_dl_heads;
			INSERT INTO authority.account_events OVERRIDING SYSTEM VALUE SELECT * FROM public.zz_dl_events;
			UPDATE authority.meta SET reconciled = true; COMMIT`)
	}
	body := holdReqBytes(t, ahead)

	deadlock := func(err error) bool {
		var st interface{ SQLState() string }
		return err != nil && ((errors.As(err, &st) && st.SQLState() == "40P01") ||
			strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock detected"))
	}
	waiters := func() int {
		var n int
		if err := e.f.Super.QueryRow(`SELECT count(DISTINCT l.pid) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// awaitWaiters blocks until n backends wait on a lock. A side that returns
	// first has not reached the interleaving: the fixture is wrong, and the
	// test says so instead of timing out.
	awaitWaiters := func(n int, side string, done <-chan error) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for waiters() < n {
			select {
			case err := <-done:
				t.Fatalf("%s returned %v before waiting on the held row: the interleaving was never reached", side, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("fewer than %d lock waiters after 8 s (%s)", n, side)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	holds := []struct {
		name, q string
		args    []any
	}{
		{"the head row held", `SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, []any{a.ID}},
		{"meta held", `SELECT 1 FROM authority.meta WHERE id = 1 FOR UPDATE`, nil},
	}
	for _, held := range holds {
		for i := range 10 {
			reset()
			holder, err := e.f.Super.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			// A Fatal before the release below must not leave the row locked
			// for the rest of the package: the pools are shared and outlive this
			// test. A second Rollback is a harmless ErrTxDone.
			t.Cleanup(func() { _ = holder.Rollback() })
			if _, err := holder.Exec(held.q, held.args...); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			t.Cleanup(cancel)
			required, purged := make(chan error, 1), make(chan error, 1)
			go func() { required <- e.s.ReconcileRequired(ctx, body) }()
			awaitWaiters(1, "ReconcileRequired", required)
			go func() {
				_, err := e.s.Purge(ctx)
				purged <- err
			}()
			awaitWaiters(2, "Purge", purged)
			if err := holder.Rollback(); err != nil { // release the held row: the pair now races for it
				t.Fatal(err)
			}

			for name, ch := range map[string]chan error{"ReconcileRequired": required, "Purge": purged} {
				select {
				case err := <-ch:
					if deadlock(err) {
						t.Fatalf("%s, round %d: %s hit a deadlock (40P01): purge_erased and reconcile/required take meta and the head row in opposite orders: %v", held.name, i, name, err)
					}
					if err != nil {
						t.Fatalf("%s, round %d: %s = %v, want nil", held.name, i, name, err)
					}
				case <-time.After(15 * time.Second):
					t.Fatalf("%s, round %d: %s did not finish within its deadline", held.name, i, name)
				}
			}
			cancel()
		}
	}
}
