package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Task 5: the legacy population and seal-legacy (DoR:1596-1612, 1730-1735,
// 1763, 1774; B1-D30, B1-D32; B3-D2, B3-D8). The server-side half, "the seal
// takes effect without a restart", is TestSealTakesEffectWithoutRestart in
// internal/authority/server/owner_integration_test.go.

// legacyPop is n deterministic population records. The ids are canonical
// lowercase UUIDs, so their text order is the order the column sorts in.
func legacyPop(n int) []ac.PopulationRecord {
	recs := make([]ac.PopulationRecord, n)
	for i := range recs {
		recs[i] = ac.PopulationRecord{
			AccountID:      fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
			LegacyEKDigest: d48(byte(i + 1)),
		}
	}
	return recs
}

// legacyBatchBytes is the canonical legacy-batch body for recs, rows in the
// order given (the store must not depend on the order a client sent them in).
func legacyBatchBytes(t *testing.T, recs []ac.PopulationRecord) []byte {
	t.Helper()
	rows := make([]string, len(recs))
	for i, r := range recs {
		rows[i] = r.AccountID + "." + base64.StdEncoding.EncodeToString(r.LegacyEKDigest)
	}
	b := ac.LegacyBatch{Rows: rows}.Encode()
	if b == nil {
		t.Fatal("the test legacy-batch does not encode")
	}
	return b
}

func reversed(recs []ac.PopulationRecord) []ac.PopulationRecord {
	out := make([]ac.PopulationRecord, len(recs))
	for i, r := range recs {
		out[len(recs)-1-i] = r
	}
	return out
}

func populationDigest(t *testing.T, recs []ac.PopulationRecord) []byte {
	t.Helper()
	d, err := ac.PopulationDigest(recs)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func populationCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM authority.legacy_population`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sealState is meta's two seal columns. at is the zero Time while unsealed.
func sealState(t *testing.T, db *sql.DB) (at time.Time, digest []byte) {
	t.Helper()
	var ts sql.NullTime
	if err := db.QueryRow(`SELECT legacy_sealed_at, population_digest FROM authority.meta WHERE id = 1`).
		Scan(&ts, &digest); err != nil {
		t.Fatal(err)
	}
	return ts.Time, digest
}

func insertRows(t *testing.T, e *liveEnv, recs []ac.PopulationRecord) {
	t.Helper()
	if err := e.s.InsertLegacyPopulation(liveCtx(t), legacyBatchBytes(t, recs)); err != nil {
		t.Fatalf("InsertLegacyPopulation = %v, want nil", err)
	}
}

// ---- the population insert ----

func TestInsertLegacyPopulation(t *testing.T) {
	t.Run("a batch of three inserts three rows", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		recs := legacyPop(3)
		insertRows(t, e, recs)
		if n := populationCount(t, e.f.App); n != 3 {
			t.Fatalf("legacy_population holds %d rows, want 3", n)
		}
		for _, r := range recs {
			var got []byte
			if err := e.f.App.QueryRow(`SELECT legacy_ek_digest FROM authority.legacy_population WHERE account_id = $1`,
				r.AccountID).Scan(&got); err != nil {
				t.Fatalf("row for %s: %v", r.AccountID, err)
			}
			if string(got) != string(r.LegacyEKDigest) {
				t.Fatalf("row for %s holds the wrong legacy_ek_digest", r.AccountID)
			}
		}
	})

	t.Run("a repeat is nil and adds nothing", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		recs := legacyPop(3)
		insertRows(t, e, recs)
		insertRows(t, e, recs)
		if n := populationCount(t, e.f.App); n != 3 {
			t.Fatalf("legacy_population holds %d rows after a repeat, want 3", n)
		}
	})

	t.Run("an overlapping batch adds only the new rows", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		insertRows(t, e, legacyPop(3))
		insertRows(t, e, legacyPop(5))
		if n := populationCount(t, e.f.App); n != 5 {
			t.Fatalf("legacy_population holds %d rows, want 5", n)
		}
	})

	// B1-D32 and DoR:1609: after the seal a row is refused with ErrSealed,
	// which the HTTP layer answers 409, never the 503 of a store failure.
	// The seal is written directly here so this case does not lean on SealLegacy.
	t.Run("after the seal it is ErrSealed", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		insertRows(t, e, legacyPop(3))
		mustExec(t, e.f.Owner, `UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`,
			populationDigest(t, legacyPop(3)))
		for name, recs := range map[string][]ac.PopulationRecord{
			"a new row":     legacyPop(4),
			"only old rows": legacyPop(3),
			"a single new":  {{AccountID: "00000000-0000-4000-8000-0000000000aa", LegacyEKDigest: d48(9)}},
		} {
			err := e.s.InsertLegacyPopulation(liveCtx(t), legacyBatchBytes(t, recs))
			if !errors.Is(err, ErrSealed) {
				t.Fatalf("InsertLegacyPopulation of %s after the seal = %v, want ErrSealed", name, err)
			}
			if errors.Is(err, ErrBadAuthz) {
				t.Fatalf("a sealed refusal %v is also ErrBadAuthz", err)
			}
		}
		if n := populationCount(t, e.f.App); n != 3 {
			t.Fatalf("legacy_population holds %d rows after refused inserts, want 3", n)
		}
	})

	t.Run("an undecodable body is ErrBadAuthz and writes nothing", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		good := legacyBatchBytes(t, legacyPop(3))
		for name, body := range map[string][]byte{
			"empty":     nil,
			"garbage":   []byte("not a legacy-batch"),
			"truncated": good[:len(good)/2],
		} {
			err := e.s.InsertLegacyPopulation(liveCtx(t), body)
			if !errors.Is(err, ErrBadAuthz) {
				t.Fatalf("InsertLegacyPopulation(%s) = %v, want ErrBadAuthz", name, err)
			}
		}
		if n := populationCount(t, e.f.App); n != 0 {
			t.Fatalf("legacy_population holds %d rows after undecodable bodies, want 0", n)
		}
	})
}

// ---- seal-legacy ----

func TestSealLegacy(t *testing.T) {
	t.Run("a matching digest seals and records it", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		recs := legacyPop(3)
		insertRows(t, e, reversed(recs)) // inserted out of order: the digest is over account_id order
		if at, d := sealState(t, e.f.App); !at.IsZero() || d != nil {
			t.Fatalf("the fixture starts sealed: %v, %x (the control)", at, d)
		}
		want := populationDigest(t, recs)
		if err := e.o.SealLegacy(liveCtx(t), want); err != nil {
			t.Fatalf("SealLegacy = %v, want nil", err)
		}
		at, d := sealState(t, e.f.App)
		if at.IsZero() {
			t.Fatal("legacy_sealed_at is still NULL after SealLegacy")
		}
		if string(d) != string(want) {
			t.Fatalf("population_digest = %x, want %x", d, want)
		}
		// The seal freezes the population (B1-D32).
		if err := e.s.InsertLegacyPopulation(liveCtx(t), legacyBatchBytes(t, legacyPop(4))); !errors.Is(err, ErrSealed) {
			t.Fatalf("InsertLegacyPopulation after SealLegacy = %v, want ErrSealed", err)
		}
	})

	t.Run("a repeat with the same digest is nil and changes nothing", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		recs := legacyPop(3)
		insertRows(t, e, recs)
		d := populationDigest(t, recs)
		if err := e.o.SealLegacy(liveCtx(t), d); err != nil {
			t.Fatal(err)
		}
		at1, d1 := sealState(t, e.f.App)
		if err := e.o.SealLegacy(liveCtx(t), d); err != nil {
			t.Fatalf("the repeat SealLegacy = %v, want nil (B3-D8)", err)
		}
		at2, d2 := sealState(t, e.f.App)
		if !at1.Equal(at2) {
			t.Fatalf("legacy_sealed_at moved from %v to %v on a repeat", at1, at2)
		}
		if string(d1) != string(d2) {
			t.Fatal("population_digest changed on a repeat")
		}
	})

	t.Run("a different digest after the seal is ErrRefused", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		recs := legacyPop(3)
		insertRows(t, e, recs)
		d := populationDigest(t, recs)
		if err := e.o.SealLegacy(liveCtx(t), d); err != nil {
			t.Fatal(err)
		}
		at1, _ := sealState(t, e.f.App)
		err := e.o.SealLegacy(liveCtx(t), d48(0xee))
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("SealLegacy with a different digest after the seal = %v, want ErrRefused", err)
		}
		at2, d2 := sealState(t, e.f.App)
		if !at1.Equal(at2) || string(d2) != string(d) {
			t.Fatal("a refused re-seal changed the seal")
		}
	})

	// DoR:1774: the digest the CP computed must be the one the authority reads;
	// a mismatch on the first call seals nothing, and the population stays open.
	t.Run("a mismatch on the first call seals nothing", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		insertRows(t, e, legacyPop(3))
		for name, bad := range map[string][]byte{
			"the digest of fewer rows": populationDigest(t, legacyPop(2)),
			"an unrelated digest":      d48(0xee),
			"a short digest":           populationDigest(t, legacyPop(3))[:47],
			"no digest":                nil,
		} {
			if err := e.o.SealLegacy(liveCtx(t), bad); !errors.Is(err, ErrRefused) {
				t.Fatalf("SealLegacy(%s) = %v, want ErrRefused", name, err)
			}
			if at, d := sealState(t, e.f.App); !at.IsZero() || d != nil {
				t.Fatalf("SealLegacy(%s) sealed anyway: %v, %x", name, at, d)
			}
		}
		// Not sealed, so the CP can still add the row it forgot, then seal.
		insertRows(t, e, legacyPop(4))
		if err := e.o.SealLegacy(liveCtx(t), populationDigest(t, legacyPop(4))); err != nil {
			t.Fatalf("SealLegacy over the completed population = %v, want nil", err)
		}
	})
}

// B1-D30: SealLegacy locks meta FOR UPDATE before it digests the population,
// and the guard on legacy_population takes meta FOR SHARE, so a change either
// commits before the seal reads the rows or waits and then sees the seal.
//
// The inserter here is a transaction on the app pool holding an uncommitted
// row, hence meta FOR SHARE from the guard. waitSealBlocked proves the seal is
// parked on that lock by reading pg_stat_activity, not by sleeping.
func TestSealLegacyLocksMetaFirst(t *testing.T) {
	const pendingID = "00000000-0000-4000-8000-0000000000bb"
	pending := ac.PopulationRecord{AccountID: pendingID, LegacyEKDigest: d48(0x77)}

	// run holds an inserter open on `pending`, starts SealLegacy(digest) and
	// returns the seal's result once end has decided the inserter's fate.
	run := func(t *testing.T, e *liveEnv, digest []byte, end func(*sql.Tx) error) error {
		t.Helper()
		ctx := liveCtx(t)
		tx, err := e.f.App.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`,
			pending.AccountID, pending.LegacyEKDigest); err != nil {
			t.Fatal(err)
		}
		res := make(chan error, 1)
		go func() { res <- e.o.SealLegacy(ctx, digest) }()
		waitSealBlocked(t, e.f.Owner, res)
		if err := end(tx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-res:
			return err
		case <-ctx.Done():
			t.Fatal("SealLegacy did not return after the inserter ended")
			return nil
		}
	}

	t.Run("an inserter that commits first is inside the digest", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		before := legacyPop(2)
		insertRows(t, e, before)
		all := append(append([]ac.PopulationRecord{}, before...), pending)
		want := populationDigest(t, all)
		if err := run(t, e, want, (*sql.Tx).Commit); err != nil {
			t.Fatalf("SealLegacy(digest of the rows including the one committed meanwhile) = %v, want nil", err)
		}
		if _, d := sealState(t, e.f.App); string(d) != string(want) {
			t.Fatalf("population_digest = %x, want the digest over all %d rows", d, len(all))
		}
		if n := populationCount(t, e.f.App); n != 3 {
			t.Fatalf("legacy_population holds %d rows, want 3", n)
		}
		late := ac.PopulationRecord{AccountID: "00000000-0000-4000-8000-0000000000cc", LegacyEKDigest: d48(0x78)}
		if err := e.s.InsertLegacyPopulation(liveCtx(t), legacyBatchBytes(t, []ac.PopulationRecord{late})); !errors.Is(err, ErrSealed) {
			t.Fatalf("an insert after the seal = %v, want ErrSealed", err)
		}
	})

	// The seal must not have read the population before it held the lock: a
	// digest of only the rows committed at the time it started is stale.
	t.Run("a digest taken before the inserter committed is refused", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		before := legacyPop(2)
		insertRows(t, e, before)
		err := run(t, e, populationDigest(t, before), (*sql.Tx).Commit)
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("SealLegacy(digest without the row that committed first) = %v, want ErrRefused", err)
		}
		if at, d := sealState(t, e.f.App); !at.IsZero() || d != nil {
			t.Fatalf("a refused seal sealed: %v, %x", at, d)
		}
	})

	t.Run("an inserter that rolls back is outside the digest", func(t *testing.T) {
		e := newLiveEnv(t, OwnerConfig{})
		before := legacyPop(2)
		insertRows(t, e, before)
		if err := run(t, e, populationDigest(t, before), (*sql.Tx).Rollback); err != nil {
			t.Fatalf("SealLegacy(digest of the committed rows) = %v, want nil", err)
		}
		if n := populationCount(t, e.f.App); n != 2 {
			t.Fatalf("legacy_population holds %d rows, want 2", n)
		}
	})
}

// waitSealBlocked returns once some session in this database is waiting on a
// lock, and fails if the seal has already returned. The inserter holds its
// locks granted and nothing else runs concurrently, so the waiter is the seal
// parked on meta FOR UPDATE.
//
// It reads pg_locks, not pg_stat_activity's wait_event_type: the harness
// switches role with SET SESSION AUTHORIZATION, so a pooled session's usename
// is the login role rather than current_user, and authority_owner, which has
// no pg_read_all_stats, reads every session's wait_event_type as NULL, its own
// included. pg_locks hides nothing; pg_stat_activity still shows datname.
func waitSealBlocked(t *testing.T, owner *sql.DB, res <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-res:
			t.Fatalf("SealLegacy returned (%v) while an inserter held meta FOR SHARE: it did not lock meta first (B1-D30)", err)
		default:
		}
		var n int
		if err := owner.QueryRow(`SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("SealLegacy never blocked behind the inserter's meta FOR SHARE")
}

// D-225 for seal-legacy: it ends on the replication check like every live
// verb. Not-leader is TestOwnerVerbOnStandbyIsNotLeader's, with this verb in
// its table.
func TestSealLegacyEndsOnTheReplicationCheck(t *testing.T) {
	p := &fakeProbe{}
	e := newLiveEnv(t, OwnerConfig{Probe: p})
	recs := legacyPop(3)
	insertRows(t, e, recs)
	d := populationDigest(t, recs)
	requireGate(t, e.f, true)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := e.o.SealLegacy(ctx, d); !errors.Is(err, ErrNotReplicated) {
		t.Fatalf("SealLegacy with a probe that never confirms = %v, want ErrNotReplicated", err)
	}
	if p.calls.Load() == 0 {
		t.Fatal("SealLegacy never asked the replication probe")
	}
	if at, _ := sealState(t, e.f.App); at.IsZero() {
		t.Fatal("SealLegacy did not commit before waiting on replication (D-225: committed locally, not replicated)")
	}
	p.ok.Store(true)
	if err := e.o.SealLegacy(liveCtx(t), d); err != nil {
		t.Fatalf("the re-run once a standby confirms = %v, want nil (B3-D8)", err)
	}
}
