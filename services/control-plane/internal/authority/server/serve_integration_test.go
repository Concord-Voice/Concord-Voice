package server

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// The leader installs the file's covering delegation, answers /v1/signer
// and stops cleanly on cancel.
func TestRunServesAndStopsCleanly(t *testing.T) {
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	r := startRun(t, baseDeps(f, rl, trustOf(t, rl.Current)))
	r.ready(t)
	m := getSigner(t, cpClient(t, rl.CPTLS, rl.TLSKey), r.addr)
	if m.Role != "leader" || m.DelegationSerial != 1 || m.RealmID != rl.ID {
		t.Fatalf("%+v", m)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("a clean stop returned %v", err)
	}
	if r.notes.count("STOPPING=1") != 1 {
		t.Fatal("no STOPPING=1")
	}
}

// B2-D8: each refusal to start, with its reason, and no READY=1.
func TestRunRefusesToStart(t *testing.T) {
	for _, c := range []struct {
		name   string
		meta   string // "" seeds the realm's ID; "-" seeds nothing; any other value is seeded as is
		mutate func(t *testing.T, f *authoritydb.Fixture, d *Deps)
		reason string
	}{
		{"owner connection", "", func(_ *testing.T, f *authoritydb.Fixture, d *Deps) { d.DB = f.Owner }, "config"},
		{"meta missing", "-", nil, "config"},
		{"realm mismatch", uuid.NewString(), nil, "trust"},
		{"anchors do not decode", "", func(_ *testing.T, _ *authoritydb.Fixture, d *Deps) { d.Anchors = []byte("x") }, "trust"},
		{"trust file malformed", "", func(_ *testing.T, _ *authoritydb.Fixture, d *Deps) {
			d.TrustFile = []byte(`["concord",1,"trust",[]]`)
		}, "trust"},
		{"RSK not in HW (D-223)", "", func(t *testing.T, _ *authoritydb.Fixture, d *Deps) {
			s, err := signer.GenerateP384()
			if err != nil {
				t.Fatal(err)
			}
			d.RSK = s
		}, "trust"},
		{"TLS key not in HW (D-223)", "", func(t *testing.T, _ *authoritydb.Fixture, d *Deps) { d.TLSKey = newKey(t) }, "trust"},
		{"serve lock held (D-171)", "", func(t *testing.T, f *authoritydb.Fixture, _ *Deps) {
			l, err := store.AcquireServeLock(context.Background(), f.App)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(l.Release)
		}, "lock"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := authoritydb.Setup(t)
			rl := authoritytest.NewRealm(t)
			switch c.meta {
			case "":
				f.SeedMeta(t, rl.ID)
			case "-":
			default:
				f.SeedMeta(t, c.meta)
			}
			d := baseDeps(f, rl, trustOf(t, rl.Current))
			if c.mutate != nil {
				c.mutate(t, f, &d)
			}
			r := startRun(t, d)
			if err := r.result(t); exitReason(err) != c.reason {
				t.Fatalf("Run: %v, want reason %s", err, c.reason)
			}
			if r.notes.count("READY=1") != 0 {
				t.Fatal("READY=1 before a refusal")
			}
		})
	}
}

// M2 (fix round 1): each startup query runs under its own RequestDeadlineTx,
// so a database that authenticates and then wedges a query refuses start
// with that step's reason instead of holding Run until systemd gives up.
// One wedge sits in Run (the schema check, behind a lock a superuser holds),
// one in build (the leader read, through Hooks.InRecovery).
func TestAWedgedStartupStepRefusesWithinItsDeadline(t *testing.T) {
	for _, c := range []struct {
		name  string
		wedge func(t *testing.T, f *authoritydb.Fixture, d *Deps)
	}{
		{"schema check", func(t *testing.T, f *authoritydb.Fixture, _ *Deps) {
			tx, err := f.Super.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback() })
			if _, err := tx.Exec(`LOCK TABLE authority.schema_migrations IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
		}},
		{"leader read", func(_ *testing.T, _ *authoritydb.Fixture, d *Deps) {
			d.Hooks.InRecovery = func(ctx context.Context, _ *sql.Conn) (bool, error) {
				<-ctx.Done()
				return false, ctx.Err()
			}
		}},
		// LoadTrust's own lock_timeout (2 s) would end this wedge too, which
		// is why the window below is shorter than it.
		{"trust load", func(t *testing.T, f *authoritydb.Fixture, _ *Deps) {
			tx, err := f.Super.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback() })
			if _, err := tx.Exec(`LOCK TABLE authority.meta IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := authoritydb.Setup(t)
			rl := authoritytest.NewRealm(t)
			f.SeedMeta(t, rl.ID)
			d := baseDeps(f, rl, trustOf(t, rl.Current))
			c.wedge(t, f, &d)
			deadline := store.RequestDeadlineTx
			store.RequestDeadlineTx = 300 * time.Millisecond
			t.Cleanup(func() { store.RequestDeadlineTx = deadline })
			r := startRun(t, d)
			select {
			case r.err = <-r.done:
				r.ended = true
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("Run still waits 1.5 s into a wedged step: the step has no deadline of its own")
			}
			if exitReason(r.err) != ReasonConfig {
				t.Fatalf("Run: %v, want reason config", r.err)
			}
			if r.notes.count("READY=1") != 0 {
				t.Fatal("READY=1 before a refusal")
			}
		})
	}
}

// D-176 and mutation 8: a delegation whose window has not begun refuses
// start on every node, and nothing is installed. d2 lists a fresh RSK, so
// D-223's reuse check cannot refuse in D-176's place, and the refusal must
// be ErrTrustNotYetValid itself.
func TestFutureDelegationRefusesStartOnEveryNode(t *testing.T) {
	for _, standby := range []bool{false, true} {
		t.Run(map[bool]string{false: "leader", true: "standby"}[standby], func(t *testing.T) {
			f := authoritydb.Setup(t)
			rl := authoritytest.NewRealm(t)
			f.SeedMeta(t, rl.ID)
			rl.Install(t, f.App, rl.Current)
			d1 := rl.Current
			rsk2, err := signer.GenerateP384()
			if err != nil {
				t.Fatal(err)
			}
			now := uint64(time.Now().UnixMilli())
			d2 := rl.NextDelegation(t, authoritytest.WithWindow(now+3_600_000, now+7_200_000),
				authoritytest.WithRSKs(rsk2.PublicKey()))
			d := baseDeps(f, rl, trustOf(t, d1, d2))
			d.Hooks.InRecovery = inRecovery(standby)
			if err := startRun(t, d).result(t); exitReason(err) != "trust" || !errors.Is(err, store.ErrTrustNotYetValid) {
				t.Fatalf("Run: %v, want reason trust from ErrTrustNotYetValid", err)
			}
			if got := serials(t, f.App); !slices.Equal(got, []int64{1}) {
				t.Fatalf("installed %v, want [1]", got)
			}
		})
	}
}

// Review Focus 3 (B2-D20, DoR:1056): a standby restarted with D(s+1)'s keys
// before the leader installs D(s+1) reaches READY=1 and installs nothing. If
// it is promoted then, it stops with reason=trust rather than lead without a
// delegation listing its RSK (B2-D33); the restart, as the leader, installs
// D(s+1) and signs under it.
func TestStandbyStartsAheadOfTheLeaderInstall(t *testing.T) {
	fastLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	d1 := rl.Current
	rsk2, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	tls2 := newKey(t)
	d2 := rl.NextDelegation(t, authoritytest.WithRSKs(rsk2.PublicKey()),
		func(d *ac.Delegation) { d.AuthorityTLSKeys = [][]byte{authoritytest.Point(tls2)} })
	deps := baseDeps(f, rl, trustOf(t, d1, d2))
	deps.RSK, deps.TLSKey = rsk2, tls2

	var recovering atomic.Bool
	recovering.Store(true)
	standby := deps
	standby.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) { return recovering.Load(), nil }
	s := startRun(t, standby)
	s.ready(t)
	if got := serials(t, f.App); !slices.Equal(got, []int64{1}) {
		t.Fatalf("the standby installed: %v", got)
	}
	recovering.Store(false) // Patroni promotes it before the old leader installed D(s+1)
	if err := s.result(t); exitReason(err) != "trust" {
		t.Fatalf("promoted with D(s+1) uninstalled: %v, want reason trust", err)
	}
	if got := serials(t, f.App); !slices.Equal(got, []int64{1}) {
		t.Fatalf("the promoted standby installed before stopping: %v", got)
	}

	l := startRun(t, deps) // systemd's restart, now as the leader
	l.ready(t)
	if got := serials(t, f.App); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("the leader installed %v, want [1 2]", got)
	}
	if m := getSigner(t, cpClient(t, rl.CPTLS, tls2), l.addr); m.Role != "leader" || m.DelegationSerial != 2 {
		t.Fatalf("leader signer: %+v", m)
	}
}

// B2-D33's control, and N4 (carry 7): a standby whose file is wholly
// installed is not Pending, so a promotion runs the schema check over the
// app pool and makes it the leader without a restart.
func TestPromotionWithNothingPendingBecomesLeader(t *testing.T) {
	fastLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	var recovering atomic.Bool
	recovering.Store(true)
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	d.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) { return recovering.Load(), nil }
	r := startRun(t, d)
	r.ready(t)
	c := cpClient(t, rl.CPTLS, rl.TLSKey)
	if m := getSigner(t, c, r.addr); m.Role != "standby" {
		t.Fatalf("before promotion: %+v", m)
	}
	recovering.Store(false)
	waitFor(t, "the promoted node to lead", func() bool { return getSigner(t, c, r.addr).Role == "leader" })
}

// Sec-M2, end to end: a standby starts on d1 with nothing pending; its leader
// then installs d2, which replicates. Promoted, the standby stops with
// reason=trust rather than lead on its start-time pins, and the restart
// would load d2.
func TestPromotionOntoMovedTrustStops(t *testing.T) {
	fastLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	var recovering atomic.Bool
	recovering.Store(true)
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	d.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) { return recovering.Load(), nil }
	r := startRun(t, d)
	r.ready(t)
	rsk2, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	rl.Install(t, f.App, rl.NextDelegation(t, authoritytest.WithRSKs(rsk2.PublicKey()))) // the leader's install, replicated
	recovering.Store(false)
	if err := r.result(t); exitReason(err) != "trust" || !errors.Is(err, errTrustMoved) {
		t.Fatalf("Run: %v, want reason trust from errTrustMoved", err)
	}
}

// D-223's reuse bullet (DoR:252), N2 (carry 5): an RSK of HW listed in
// another delegation in the trust refuses start with reason=trust, whether
// that delegation was accepted or left out because its window ended before
// it was installed. NextDelegation keeps the realm's RSK, so d2 reuses d1's.
// LoadTrust refuses it before its commit; serve maps it.
func TestRunRefusesAnRSKReusedAcrossDelegations(t *testing.T) {
	for _, ended := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "ended uninstalled"}[ended], func(t *testing.T) {
			f := authoritydb.Setup(t)
			rl := authoritytest.NewRealm(t)
			f.SeedMeta(t, rl.ID)
			d1 := rl.Current
			var opts []authoritytest.RealmOption
			if ended {
				now := uint64(time.Now().UnixMilli())
				opts = append(opts, authoritytest.WithWindow(now-7_200_000, now-3_600_000))
			}
			d2 := rl.NextDelegation(t, opts...)
			err := startRun(t, baseDeps(f, rl, trustOf(t, d1, d2))).result(t)
			if exitReason(err) != "trust" || !errors.Is(err, store.ErrRSKReused) {
				t.Fatalf("Run: %v, want reason trust from ErrRSKReused", err)
			}
			if got := serials(t, f.App); len(got) != 0 {
				t.Fatalf("installed %v before refusing", got)
			}
		})
	}
}

// B2-D21: the schema check runs under the serve lock. With the lock held and
// the schema dirty, the refusal is lock; a check made before the lock would
// refuse config.
func TestRunChecksTheSchemaUnderTheServeLock(t *testing.T) {
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	held, err := store.AcquireServeLock(context.Background(), f.App)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(held.Release)
	if _, err := f.Super.Exec(`UPDATE authority.schema_migrations SET dirty = true`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.Super.Exec(`UPDATE authority.schema_migrations SET dirty = false`) })
	if err := startRun(t, baseDeps(f, rl, trustOf(t, rl.Current))).result(t); exitReason(err) != "lock" {
		t.Fatalf("Run: %v, want reason lock", err)
	}
}

// B2-D34: a request that outlives the shutdown budget keeps the serve lock
// held until it returns, so migrate cannot start under it (D-171). The tick
// and the role poll are pushed out of the way, so only the request reaches
// the wedged InRecovery hook.
func TestShutdownHoldsTheServeLockUntilEveryRequestReturns(t *testing.T) {
	slowLoops(t)
	budget := shutdownTimeout
	shutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = budget })
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	var wedge atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	buf := &syncBuffer{}
	d.Log = slog.New(slog.NewJSONHandler(buf, nil))
	d.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) {
		if wedge.CompareAndSwap(true, false) {
			close(entered)
			<-release // ignores its context, as a wedged request would
		}
		return false, nil
	}
	r := startRun(t, d)
	r.ready(t)
	c := cpClient(t, rl.CPTLS, rl.TLSKey)
	wedge.Store(true)
	go func() {
		if resp, err := c.Get("https://" + r.addr + "/v1/accounts/" + uuid.NewString() + "/head"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	r.cancel()
	time.Sleep(200 * time.Millisecond) // four shutdown budgets
	if _, err := store.AcquireServeLock(context.Background(), f.App); !errors.Is(err, store.ErrServeLockHeld) {
		t.Fatalf("the serve lock was free while a request ran: %v", err)
	}
	close(release)
	if err := r.result(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	l, err := store.AcquireServeLock(context.Background(), f.App)
	if err != nil {
		t.Fatalf("the serve lock after Run returned: %v", err)
	}
	l.Release()
	// Carry 19: the overrun is logged in the convention, once.
	n := 0
	for _, m := range logLines(t, buf.Bytes()) {
		if m["msg"] == "authority.shutdown" && m["op"] == "authority.shutdown" && m["code"] == "timeout" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d shutdown-timeout lines, want 1: %s", n, buf.Bytes())
	}
	checkLogConvention(t, buf.Bytes())
}

// B2-D21 (D-171): losing the serve lock's connection stops serve with
// reason=lock. The role poll keeps running until serve's context ends, so it
// fails again; the first fatal stands (carry 10).
func TestLostServeLockStopsServe(t *testing.T) {
	fastLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	r := startRun(t, baseDeps(f, rl, trustOf(t, rl.Current)))
	r.ready(t)
	key := store.ServeLockKey
	if _, err := f.Super.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 1 AND classid = $1 AND objid = $2`, key>>32, key&0xffffffff); err != nil {
		t.Fatal(err)
	}
	if err := r.result(t); exitReason(err) != "lock" {
		t.Fatalf("Run: %v, want reason lock", err)
	}
}

// Carry 11 and 14: the disk is sampled before READY=1, into the Admission the
// store reads. With the tick an hour away, a node starting below the soft
// floor reports disk_ready = 0 at once and refuses an ordinary commit with
// 503 disk. Had the watcher waited for its first tick, both would read the
// Admission's optimistic start; had the store an Admission of its own, the
// commit would pass the soft floor and reach a later step.
func TestTheDiskIsSampledBeforeReadyIntoTheSharedAdmission(t *testing.T) {
	slowLoops(t)
	const gib = uint64(1) << 30
	s := serveRealm(t, func(d *Deps) {
		// At the soft floor max(2 GiB, total/10), above the 512 MiB hard floor.
		d.Statfs = func() (uint64, uint64, error) { return 10 * gib, 100 * gib, nil }
	})
	c := cpClient(t, s.rl.CPTLS, s.rl.TLSKey)
	if m := getSigner(t, c, s.r.addr); m.DiskReady != 0 {
		t.Fatalf("disk_ready %d right after READY=1, want 0", m.DiskReady)
	}
	code, body := call(t, c, s.r.addr, "POST", "/v1/transactions", s.rl.NewAccount(t).EnrollNew(t).Bytes)
	if code != http.StatusServiceUnavailable || string(body) != string(envelope(ac.ErrorDisk)) {
		t.Fatalf("an ordinary commit below the soft floor: %d %q, want 503 disk", code, body)
	}
}

// Carry 11: the first role poll runs before READY=1, so a node that is the
// leader at start answers as one at once, with the role poll an hour away.
func TestTheFirstRolePollRunsBeforeReady(t *testing.T) {
	slowLoops(t)
	s := serveRealm(t, nil)
	if m := getSigner(t, cpClient(t, s.rl.CPTLS, s.rl.TLSKey), s.r.addr); m.Role != "leader" {
		t.Fatalf("right after READY=1: role %q, want leader", m.Role)
	}
}

// Carry 11 and 13: pending is set before the first role poll. A standby that
// loads trust it has not installed and is promoted before that poll stops
// with reason=trust and never sends READY=1, rather than lead with an
// uninstalled delegation until a later poll that, with the poll an hour
// away, never comes.
func TestAPromotionBeforeTheFirstPollWithPendingTrustStops(t *testing.T) {
	slowLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	d1 := rl.Current
	rsk2, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	d2 := rl.NextDelegation(t, authoritytest.WithRSKs(rsk2.PublicKey()))
	d := baseDeps(f, rl, trustOf(t, d1, d2))
	d.RSK = rsk2
	var reads atomic.Int32
	d.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) {
		return reads.Add(1) == 1, nil // a standby when LoadTrust reads the role, the leader after
	}
	r := startRun(t, d)
	if err := r.result(t); exitReason(err) != "trust" || !errors.Is(err, errPromotedPending) {
		t.Fatalf("Run: %v, want reason trust from errPromotedPending", err)
	}
	if r.notes.count("READY=1") != 0 {
		t.Fatal("READY=1 sent by a node that cannot lead")
	}
	if got := serials(t, f.App); !slices.Equal(got, []int64{1}) {
		t.Fatalf("installed %v, want [1]", got)
	}
}

// Carry 13 (B2-D33): a standby that starts Pending says so, once, in a line
// an operator can find; a standby with nothing pending, and a leader, do not.
// Its leader's refusal of the same file shows as that node's authority.exit.
func TestAPendingStandbyLogsItAtStart(t *testing.T) {
	for _, c := range []struct {
		name          string
		standby, next bool // next: the file carries an uninstalled D(s+1) and the node holds its RSK
		lines         int
	}{
		{"pending standby", true, true, 1},
		{"standby with nothing pending", true, false, 0},
		{"leader installing the same file", false, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := authoritydb.Setup(t)
			rl := authoritytest.NewRealm(t)
			f.SeedMeta(t, rl.ID)
			rl.Install(t, f.App, rl.Current)
			d := baseDeps(f, rl, trustOf(t, rl.Current))
			if c.next {
				d1 := rl.Current
				rsk2, err := signer.GenerateP384()
				if err != nil {
					t.Fatal(err)
				}
				d = baseDeps(f, rl, trustOf(t, d1, rl.NextDelegation(t, authoritytest.WithRSKs(rsk2.PublicKey()))))
				d.RSK = rsk2
			}
			buf := &syncBuffer{}
			d.Log = slog.New(slog.NewJSONHandler(buf, nil))
			d.Hooks.InRecovery = inRecovery(c.standby)
			r := startRun(t, d)
			r.ready(t)
			if err := r.stop(t); err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, m := range logLines(t, buf.Bytes()) {
				if m["msg"] == "authority.trust" && m["op"] == "authority.trust" && m["code"] == "pending" &&
					m["outcome"] == float64(store.KindRefused) {
					n++
				}
			}
			if n != c.lines {
				t.Fatalf("%d pending lines, want %d: %s", n, c.lines, buf.Bytes())
			}
			checkLogConvention(t, buf.Bytes())
		})
	}
}

// R1 (carry 1), D-207 (mutation 5) and M2 (carry 9): a pin revoked while its
// connections are idle in keep-alive gets no reply on the next request on
// either, OPTIONS * included, which net/http would otherwise answer itself
// without the peer check; a new handshake with the same certificate is
// refused as revoked; and all three refusals reach the log as one listener
// summary line, not one line each.
func TestRevokedPinClosesKeepAliveConnection(t *testing.T) {
	s := serveRealm(t, nil)
	cfg := s.clientConfig(t, mint(t, s.rl.CPTLS, x509.ExtKeyUsageClientAuth))
	get, opts := keepAlive(t, s.r.addr, cfg, "GET /v1/signer"), keepAlive(t, s.r.addr, cfg, "OPTIONS *")
	if code, err := get(); err != nil || code != http.StatusOK {
		t.Fatalf("GET before the revocation: %d %v", code, err)
	}
	if code, err := opts(); err != nil || code != http.StatusNotFound {
		t.Fatalf("OPTIONS * before the revocation: %d %v, want the authority's 404", code, err)
	}
	pin, err := ac.TLSPin(authoritytest.Point(s.rl.CPTLS))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.f.Owner.Exec(`INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cp_tls')`, pin); err != nil {
		t.Fatal(err)
	}
	if code, err := get(); err == nil {
		t.Errorf("the revoked connection was answered: GET %d", code)
	}
	if code, err := opts(); err == nil {
		t.Errorf("the revoked connection was answered: OPTIONS * %d", code)
	}
	if reaches(s.r.addr, cfg) {
		t.Error("a new handshake with the revoked pin was served")
	}
	// The listener's own check refuses it at the handshake, before any
	// request: the per-request check alone would leave it open (E1).
	if got := refusedAs(s.r.addr, cfg); got != "reset" {
		t.Errorf("a new connection with the revoked pin saw %q before any request, want the listener's reset", got)
	}
	if err := s.r.stop(t); err != nil {
		t.Fatal(err)
	}
	if got := tlsSummaries(t, s.log.Bytes()); !maps.Equal(got, map[string]summary{"revoked": {1, 4}}) {
		t.Errorf("authority.tls summaries %v, want one line counting four revoked", got)
	}
	checkLogConvention(t, s.log.Bytes())
}

// keepAlive opens one connection and returns a function that sends line
// (method and target) on it as one HTTP/1.1 request and reads the reply.
func keepAlive(t *testing.T, addr string, cfg *tls.Config, line string) func() (int, error) {
	t.Helper()
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	br := bufio.NewReader(c)
	return func() (int, error) {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, line+" HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
			return 0, err
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
}

// The DoR's handshake matrix through serve (DoR:1662), carry 17: each
// refusal names its B2-D9 reason, and a shape refusal happens INSIDE the
// handshake, as an alert, while a pin refusal is the listener's bare reset
// after it. The second half is what kills mutation 7 here: without tlspin's
// len==1 check the listener still refuses two certificates as chain_len, but
// only after the handshake. Without VerifyConnection (mutation 9b) the shape
// cases reach the pin check, as unpinned or, with the pinned key, served.
func TestHandshakeRefusalsThroughServe(t *testing.T) {
	s := serveRealm(t, nil)
	pinned := mint(t, s.rl.CPTLS, x509.ExtKeyUsageClientAuth)
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	extra := mint(t, newKey(t), x509.ExtKeyUsageClientAuth)
	twoCerts := pinned
	twoCerts.Certificate = [][]byte{pinned.Certificate[0], extra.Certificate[0]}
	use := func(c tls.Certificate) func(*tls.Config) {
		return func(cfg *tls.Config) { cfg.Certificates = []tls.Certificate{c} }
	}
	cases := []struct {
		name string
		cfg  func(*tls.Config)
		code string // "" means served
		wire string // what the client sees on its first read
	}{
		{"the pinned client (control)", nil, "", "open"},
		{"no certificate", func(cfg *tls.Config) { cfg.Certificates = nil }, "handshake", "alert"},
		{"unpinned key, identical DN", use(mint(t, newKey(t), x509.ExtKeyUsageClientAuth)), "unpinned", "reset"},
		{"the authority's own TLS key (B2-D30)", use(mint(t, s.rl.TLSKey, x509.ExtKeyUsageClientAuth)), "unpinned", "reset"},
		{"P-256", use(rawCert(t, p256, x509.ExtKeyUsageClientAuth)), "not_p384", "alert"},
		{"RSA", use(rawCert(t, rsaKey, x509.ExtKeyUsageClientAuth)), "not_p384", "alert"},
		{"ServerAuth only, pinned key", use(rawCert(t, s.rl.CPTLS, x509.ExtKeyUsageServerAuth)), "eku", "alert"},
		{"no EKU, pinned key", use(rawCert(t, s.rl.CPTLS)), "eku", "alert"},
		{"both EKUs, pinned key", use(rawCert(t, s.rl.CPTLS, x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)), "eku", "alert"},
		{"two certificates (mutation 7)", use(twoCerts), "chain_len", "alert"},
		{"TLS 1.2 only", func(cfg *tls.Config) { cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS12, tls.VersionTLS12 }, "handshake", "alert"},
		{"X25519 only", func(cfg *tls.Config) { cfg.CurvePreferences = []tls.CurveID{tls.X25519} }, "handshake", "alert"},
		{"ALPN h2", func(cfg *tls.Config) { cfg.NextProtos = []string{"h2"} }, "handshake", "alert"},
	}
	want := map[string]summary{}
	for _, c := range cases {
		cfg := s.clientConfig(t, pinned)
		if c.cfg != nil {
			c.cfg(cfg)
		}
		if got := reaches(s.r.addr, cfg); got != (c.code == "") {
			t.Errorf("%s: served = %v", c.name, got)
		}
		if got := refusedAs(s.r.addr, cfg); got != c.wire {
			t.Errorf("%s: the client saw %q, want %q", c.name, got, c.wire)
		}
		if c.code != "" {
			w := want[c.code]
			w.total += 2 // reaches and refusedAs each made one connection
			want[c.code] = w
		}
	}
	if err := s.r.stop(t); err != nil {
		t.Fatal(err)
	}
	got := tlsSummaries(t, s.log.Bytes())
	for code, w := range want {
		if got[code].total != w.total {
			t.Errorf("%s: logged %d refusals, want %d", code, got[code].total, w.total)
		}
	}
	for code := range got {
		if _, ok := want[code]; !ok {
			t.Errorf("logged %d refusals as %s, a code no case expects", got[code].total, code)
		}
	}
	checkLogConvention(t, s.log.Bytes())
}

// Carry 19: a whole serve, start to clean stop, logs nothing outside the
// convention, and logs no exit line of its own (Task 11 writes that).
func TestServeLogsOnlyTheConvention(t *testing.T) {
	s := serveRealm(t, func(d *Deps) { d.Hooks.InRecovery = inRecovery(false) })
	getSigner(t, cpClient(t, s.rl.CPTLS, s.rl.TLSKey), s.r.addr)
	if err := s.r.stop(t); err != nil {
		t.Fatal(err)
	}
	for _, m := range logLines(t, s.log.Bytes()) {
		if m["msg"] == "authority.exit" {
			t.Errorf("Run logged an exit line: %v", m)
		}
	}
	checkLogConvention(t, s.log.Bytes())
}

// Carry 12 (N4): the schema check serve runs at start, and again on every
// promotion, reads authority.schema_migrations as authority_app. 000001's
// GRANT SELECT ON ALL TABLES covers only the tables that exist when it runs,
// so this proves the runner creates the version table before it: on a
// freshly migrated schema the app role can read it. The control revokes the
// grant and must see the same check fail, so the test can see a gap.
func TestCheckSchemaPassesAsTheAppRoleOnAFreshMigration(t *testing.T) {
	owner, super := authoritydb.DirtySetup(t)
	if err := store.MigrateEmbedded(owner); err != nil {
		t.Fatal(err)
	}
	app := authoritydb.OpenAs(t, authoritydb.URL(t, authoritydb.EnvDirtyURL, authoritydb.DirtyDBName), authoritydb.RoleApp)
	ctx := context.Background()
	if err := store.CheckSchema(ctx, app); err != nil {
		t.Fatalf("CheckSchema as authority_app on a fresh migration: %v", err)
	}
	if _, err := super.Exec(`REVOKE SELECT ON authority.schema_migrations FROM authority_app`); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckSchema(ctx, app); err == nil || errors.Is(err, store.ErrSchemaSkew) ||
		errors.Is(err, store.ErrSchemaDirty) {
		t.Fatalf("control: CheckSchema without the grant returned %v, want a read error", err)
	}
}

// A stop that lands during the first role poll ends serve
// cleanly before the listener exists: nil, and no READY=1. Hook call 1 is
// build's leader read, call 2 the first poll's.
func TestAStopDuringTheFirstPollNeverSendsReady(t *testing.T) {
	slowLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	var calls atomic.Int32
	polled, release := make(chan struct{}), make(chan struct{})
	d.Hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) {
		if calls.Add(1) == 2 {
			close(polled)
			<-release
		}
		return false, nil
	}
	r := startRun(t, d)
	select {
	case <-polled:
	case <-time.After(10 * time.Second):
		t.Fatal("the first role poll never ran")
	}
	r.cancel()
	close(release)
	if err := r.result(t); err != nil {
		t.Fatalf("Run: %v, want nil", err)
	}
	if n := r.notes.count("READY=1"); n != 0 {
		t.Fatalf("READY=1 sent %d times after a stop during the first poll", n)
	}
}

// When D-223's key check refuses, the refusal names how many
// trust-file items did not verify. Here a forged next delegation is the only
// one listing this node's RSK, so the refusal would otherwise blame the key.
func TestAKeyRefusalNamesTheTrustItemsThatDidNotVerify(t *testing.T) {
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	d1 := rl.Current
	d2 := rl.NextDelegation(t)
	forged := append([]byte(nil), d2.SigB...)
	forged[10] ^= 0xff
	d2.Signed = ac.Signed{Type: "delegation", Msg: d2.Msg, SigA: d2.SigA, SigB: forged}.Encode()
	d := baseDeps(f, rl, trustOf(t, d1, d2))
	other, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	d.RSK = other
	err = startRun(t, d).result(t)
	if exitReason(err) != ReasonTrust || exitStep(err) != StepKeys || !errors.Is(err, errNodeRSK) ||
		!strings.Contains(err.Error(), "(1 trust-file items did not verify)") {
		t.Fatalf("Run: %v, want reason trust at step keys naming one unverified item", err)
	}
}

// The role check's refusal names its step, here through the role
// check itself (an owner connection), not a failed dial.
func TestTheRoleRefusalNamesItsStep(t *testing.T) {
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	d := baseDeps(f, rl, trustOf(t, rl.Current))
	d.DB = f.Owner
	if err := startRun(t, d).result(t); exitReason(err) != ReasonConfig || exitStep(err) != StepRole ||
		!errors.Is(err, store.ErrWrongRole) {
		t.Fatalf("Run as the owner: %v, want reason config at step role", err)
	}
}
