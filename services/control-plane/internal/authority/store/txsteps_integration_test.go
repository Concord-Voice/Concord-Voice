package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func TestAssertedWindowEdges(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	now := e.now.Load()
	e.refusedAs(a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
		Factors: []string{ac.FactorRegistration}, AssertedAtMs: now - ac.IntentWindowMs}).Bytes, ac.RefusalAuthzOutOfWindow)
	e.refusedAs(a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
		Factors: []string{ac.FactorRegistration}, AssertedAtMs: now + ac.EpsilonMs + 1}).Bytes, ac.RefusalAuthzOutOfWindow)
	e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
		Factors: []string{ac.FactorRegistration}, AssertedAtMs: now + ac.EpsilonMs}).Bytes)
}

// D-228: a cancel asserted beyond db_now + ε is the authority's clock lagging:
// an unrecorded 503 clock and a tick leg, never a ledger row.
func TestCancelAssertedInTheFutureIs503ClockAndUnrecorded(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b := a.Build(t, authoritytest.Spec{Op: ac.OpCancelPending, RootSigns: true, AssertedAtMs: e.now.Load() + ac.EpsilonMs + 1})
	e.errorAs(b.Bytes, 503, ac.ErrorClock)
	var n int
	_ = e.f.App.QueryRow(`SELECT count(*) FROM authority.request_ledger`).Scan(&n)
	if n != 0 || !e.s.nonClient503.Load() {
		t.Fatalf("ledger rows %d, tick leg %v: want 0 and set", n, e.s.nonClient503.Load())
	}
}

func TestExpiryBounds(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	now := e.now.Load()
	spec := func(exp uint64) []byte {
		return a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
			Factors: []string{ac.FactorRegistration}, ExpiresAtMs: exp}).Bytes
	}
	e.refusedAs(spec(now), ac.RefusalExpired)
	e.refusedAs(spec(now+ac.IntentWindowMs+1), ac.RefusalExpiryOutOfRange)
	mustExec(t, e.f.Owner, `UPDATE authority.meta SET restore_started_at = to_timestamp($1 / 1000.0)`, now)
	e.refusedAs(spec(now+ac.IntentWindowMs-1), ac.RefusalExpired) // ≤ restore_started_at + 11 min
}

// Red-team F1 (Phase 4): a cancel's expiry is at most asserted_at +
// INTENT_WINDOW (DoR:1190 recovery-bind #7, :270 intent #7), the bound D-267's
// safety argument for the target-"" sheet cancel rests on. Step 4 checks it at
// every cancel expiry position: one ms past it is a recorded
// expiry_out_of_range, and the cap itself commits.
func TestCancelExpiryIsCappedAtTheIntentWindow(t *testing.T) {
	cancelBind := func(a *authoritytest.Account, key []byte, exp uint64) *ac.RecoveryBind {
		return &ac.RecoveryBind{RealmID: a.R.ID, AccountID: a.ID, Kind: ac.BindKindCancel, ExpiresAtMs: exp, RecoveryKey: key}
	}
	t.Run("sheet form B #7", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		k0 := authoritytest.NewDevice(t).DSK
		e.deviceOK(a, a.BindRecovery(t, k0, ac.BindWaitNow, nil).Bytes)
		e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes) // a refresh fills the slot
		sheet := func(exp uint64) []byte {
			return a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpCancelRecoveryBind, SheetForm: true, ExpiresAtMs: exp,
				Bind: cancelBind(a, authoritytest.Point(k0), exp), BindKey: k0}).Bytes
		}
		hi := e.now.Load() + ac.IntentWindowMs // asserted_at is db_now here
		e.deviceRefused(sheet(hi+1), ac.RefusalExpiryOutOfRange)
		e.deviceOK(a, sheet(hi))
	})
	t.Run("root form device-intent #8", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes) // a pending first bind
		// B #7 = #8 on the root form (E3-10), so one expiry fills both.
		root := func(exp uint64) []byte {
			return a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpCancelRecoveryBind, RootForm: true, ExpiresAtMs: exp,
				Bind: cancelBind(a, authoritytest.Point(a.Recovery), exp)}).Bytes
		}
		hi := e.now.Load() + ac.IntentWindowMs
		e.deviceRefused(root(hi+1), ac.RefusalExpiryOutOfRange)
		e.deviceOK(a, root(hi))
	})
	t.Run("cancel_pending intent #7", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes) // a pending replace_root
		cancel := func(exp uint64) []byte {
			return a.Build(t, authoritytest.Spec{Op: ac.OpCancelPending, RootSigns: true, ExpiresAtMs: exp}).Bytes
		}
		hi := e.now.Load() + ac.IntentWindowMs
		e.refusedAs(cancel(hi+1), ac.RefusalExpiryOutOfRange)
		e.commitOK(a, cancel(hi))
	})
}

// B1-D20: db_now is read after L1, so an L1 wait counts against expiry.
func TestExpiryAfterAnL1Wait(t *testing.T) {
	f := newEnv(t)
	f.s.hooks.DBNow = nil // the real database clock
	a, _ := f.enrolledWith()
	b := a.Build(t, authoritytest.Spec{Op: ac.OpSuspend, Factors: []string{ac.FactorPassword}, PasswordLogin: true,
		AssertedAtMs: uint64(time.Now().UnixMilli()), ExpiresAtMs: uint64(time.Now().UnixMilli()) + 800})
	blocker, err := f.f.App.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, a.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan Outcome, 1)
	go func() { done <- f.submit(b.Bytes) }()
	time.Sleep(1200 * time.Millisecond) // under lock_timeout (2 s)
	_ = blocker.Rollback()
	if out := <-done; out.Kind != KindRefused || out.Code != string(ac.RefusalExpired) {
		t.Fatalf("a request that expired while waiting on L1 = %+v, want recorded expired", out)
	}
}

func TestHeldRefusesExceptExemptRows(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.pinHeld(a.ID)
	e.refusedAs(a.Suspend(t, ac.ActorUser).Bytes, ac.RefusalHeld)
	if out := e.submit(a.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes); out.Kind != KindCommitted {
		t.Fatalf("a held user erase (row 18) = %+v, want committed as a Pe that cannot activate", out)
	}
	e.advance(10 * 86_400_000)
	a.Sync(t, e.f.App)
	if a.Head.State != ac.StatePending {
		t.Fatalf("a held Pe activated: %s", a.Head.State)
	}
}

func TestMalformedForOpIsRecorded(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b := a.Build(t, authoritytest.Spec{Op: ac.OpSuspend, Factors: []string{ac.FactorPassword}, PasswordLogin: true,
		Mutate: func(p *authoritytest.Parts) { p.Intent.CandidateRoot = authoritytest.Point(e.r.CAK) }})
	e.refusedAs(b.Bytes, ac.RefusalMalformedForOp)
}

// atInstall moves db_now (and the realm clock) up to serial's installed_at.
// db_now is pinned at NewRealm time, before any install, so without this a
// zero drain grace still admits: db_now < installed_at + 0 (PR review).
func (e *env) atInstall(t *testing.T, serial uint64) {
	t.Helper()
	var ms int64
	if err := e.f.App.QueryRow(`SELECT floor(extract(epoch FROM installed_at) * 1000)::bigint
		FROM authority.delegations WHERE serial = $1`, i64(serial)).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	if u64(ms) > e.now.Load() {
		e.now.Store(u64(ms))
		e.r.Clock.Ms = e.now.Load()
	}
}

func TestT9DrainGraceAndZero(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	oldCAK, freshCAK := e.r.CAK, authoritytest.NewRealm(t).CAK
	next := e.r.NextDelegation(t, func(d *ac.Delegation) {
		d.CAKPublicKeys = [][]byte{authoritytest.Point(freshCAK)}
	})
	e.r.Install(t, e.f.App, next)
	e.atInstall(t, next.D.Serial)
	e.r.CAK = oldCAK // keep signing with the superseded CAK
	// The old CAK is still listed in serial 1; serial 2 supersedes it. Within the drain grace it is accepted.
	if out := e.submit(a.Suspend(t, ac.ActorUser).Bytes); out.Kind == KindError && out.Code == string(ac.ErrorBadAuthz) {
		t.Fatalf("a CAK within drain_grace_ms was refused: %+v", out)
	}
	mustExec(t, e.f.Owner, `UPDATE authority.delegations SET drain_grace_ms = 0 WHERE serial = 1`)
	e.errorAs(a.Unsuspend(t, ac.ActorUser).Bytes, 400, ac.ErrorBadAuthz)
}

// B1-D36 (PR review; red-team F2): a delegation the trust ignored supersedes
// nothing. Serial 2 lists the same keys but has a bad second signature, so
// AddSigned ignores it and the signer stays on serial 1. end-drain on serial 1
// makes any supersession refuse at once, so this commits only if serial 2 is
// not counted.
func TestT9NeverCountsADelegationTheTrustIgnored(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	good := e.r.Current
	bad := e.r.NextDelegation(t)
	e.r.Current = good
	bad.SigB = append([]byte(nil), bad.SigB...)
	bad.SigB[0] ^= 0xff // fails dual control: AddSigned returns ErrIgnored
	e.r.Install(t, e.f.App, bad)
	e.atInstall(t, bad.D.Serial)
	mustExec(t, e.f.Owner, `UPDATE authority.delegations SET drain_grace_ms = 0 WHERE serial = 1`)
	e.commitOK(a, a.Suspend(t, ac.ActorUser).Bytes)
}

// B1-D37 (Codex review round 6): selection reads only the rows its own trust
// build accepted. A row installed after the build, here a T3-invalid one, is
// neither the signing delegation nor a superseding one until a later trustFor
// processes it.
func TestSelectionIgnoresARowInstalledAfterTheTrustBuild(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	good := e.r.Current
	tr, accepted, err := e.s.trustFor(ctx, e.f.App)
	if err != nil {
		t.Fatal(err)
	}
	bad := e.r.NextDelegation(t)
	e.r.Current = good
	bad.SigB = append([]byte(nil), bad.SigB...)
	bad.SigB[0] ^= 0xff // T3-invalid, and installed after the build
	e.r.Install(t, e.f.App, bad)
	if d, err := e.s.signingDelegation(ctx, e.f.App, tr.CurrentGen(), e.now.Load(), accepted); err != nil || d.Serial != good.D.Serial {
		t.Fatalf("signing delegation = %+v, %v; want serial %d, never the unseen serial %d", d, err, good.D.Serial, bad.D.Serial)
	}
	if at, err := e.s.supersedingInstalledAt(ctx, e.f.App, good.D.ProductRootGen, good.D.Serial, accepted); err != nil || at.Valid {
		t.Fatalf("superseding installed_at = %+v, %v; want none, since serial %d was never accepted", at, err, bad.D.Serial)
	}
}

func TestRevokedCAKTakesEffectOnTheNextRequest(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	mustExec(t, e.f.Owner, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cak')`,
		ac.KeyDigest(authoritytest.Point(e.r.CAK)))
	e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 400, ac.ErrorBadAuthz) // D-207: uncached, no restart
}

// revoked_keys is keyed on key_digest alone and is insert-only, so a row
// filed under the wrong kind can never be corrected. Both checks match the
// digest alone: a kind filter would let a revoked key through.
func TestAMisKindedRevocationStillRevokes(t *testing.T) {
	t.Run("cak", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		mustExec(t, e.f.Owner, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cp_tls')`,
			ac.KeyDigest(authoritytest.Point(e.r.CAK)))
		e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 400, ac.ErrorBadAuthz)
	})
	t.Run("operator", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		mustExec(t, e.f.Owner, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cp_tls')`,
			ac.KeyDigest(authoritytest.Point(e.r.Operator)))
		e.errorAs(a.Suspend(t, ac.ActorOperator).Bytes, 400, ac.ErrorBadAuthz)
	})
}

// Codex round 3: a database error while the store reads the installed trust is
// 503 store, never 503 signer. signer is step 9's verdict that no installed
// delegation is usable (DoR:1450, :1756); a read that failed reached no verdict,
// and store is the separate code for it (DoR:1600, :1614). Revoking SELECT on
// delegations makes trustFor's first statement fail before any delegation is
// judged. TestStep9RefusesWithoutUsableDelegation is the control: a real
// verdict stays signer.
func TestDelegationReadFailureIsStoreNotSigner(t *testing.T) {
	revoker := func(t *testing.T, e *env) func() error {
		t.Cleanup(func() { // shared database: restore before the next test
			if _, err := e.f.Super.Exec(`GRANT SELECT ON authority.delegations TO authority_app`); err != nil {
				t.Errorf("restore the delegations grant: %v", err)
			}
		})
		return func() error {
			_, err := e.f.Super.Exec(`REVOKE SELECT ON authority.delegations FROM authority_app`)
			return err
		}
	}
	// The finding's line: resolveSigner, after L1. checkCAK reads the trust
	// before its db_now (read 1); readNowAfterL1 is read 2, just before
	// resolveSigner, so only the transaction meets the revoke.
	t.Run("transaction resolveSigner", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		revoke := revoker(t, e)
		var reads atomic.Int32
		var revokeErr error
		e.s.hooks.DBNow = func(context.Context) (uint64, error) {
			if reads.Add(1) == 2 {
				revokeErr = revoke()
			}
			return e.now.Load(), nil
		}
		before := countRows(t, e, "account_events") + countRows(t, e, "request_ledger")
		out := e.submit(a.Suspend(t, ac.ActorUser).Bytes)
		if revokeErr != nil {
			t.Fatalf("revoke: %v", revokeErr)
		}
		if n := reads.Load(); n != 2 {
			t.Fatalf("db_now read %d times, want 2: the revoke did not land between checkCAK and resolveSigner", n)
		}
		if out.Kind != KindError || out.HTTP != 503 || out.Code != string(ac.ErrorStore) {
			t.Fatalf("a delegation read that failed inside the transaction = %+v, want 503 store", out)
		}
		if after := countRows(t, e, "account_events") + countRows(t, e, "request_ledger"); after != before {
			t.Fatalf("a 503 store wrote %d rows", after-before)
		}
	})
	t.Run("before BEGIN checkCAK", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		if err := revoker(t, e)(); err != nil {
			t.Fatal(err)
		}
		e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorStore)
	})
	t.Run("reads readTx", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		e.freshTick(t)
		if err := revoker(t, e)(); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if out := e.s.Head(ctx, a.ID); out.HTTP != 503 || out.Code != string(ac.ErrorStore) {
			t.Fatalf("head with the delegations unreadable = %+v, want 503 store", out)
		}
		if out := e.s.Statuses(ctx, ac.StatusReq{AccountIDs: []string{a.ID}}.Encode()); out.HTTP != 503 || out.Code != string(ac.ErrorStore) {
			t.Fatalf("statuses with the delegations unreadable = %+v, want 503 store", out)
		}
	})
	t.Run("restore-cancel resolveSigner", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
		e.pinHeld(a.ID)
		if err := revoker(t, e)(); err != nil {
			t.Fatal(err)
		}
		if out, err := e.s.commitRestoreCancel(context.Background(), a.ID); err == nil || out.HTTP != 503 || out.Code != string(ac.ErrorStore) {
			t.Fatalf("row 22 with the delegations unreadable = %+v, %v; want 503 store", out, err)
		}
	})
}
