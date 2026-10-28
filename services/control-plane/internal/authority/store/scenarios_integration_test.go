package store

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func (e *env) activate() { e.advance(e.r.Current.D.DeltaMs + e.r.Current.D.GMs) }

func (e *env) state(a *authoritytest.Account) string {
	st, _, _ := readHead(e.t, e, a.ID)
	return st.State
}

// --- Commit and replay -------------------------------------------------------

func TestReplayOfAnExpiredRequestReturnsTheStoredOutcome(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b := a.EnrollNew(t)
	first := e.submit(b.Bytes)
	e.advance(ac.IntentWindowMs + 1) // the intent has expired
	if again := e.submit(b.Bytes); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("replay after expiry = %+v, want the stored commit (the replay lookup precedes expiry)", again)
	}
}

func TestRegistrationLaneWithEveryGeneralSlotHeld(t *testing.T) {
	e := newEnv(t)
	victim, _ := e.enrolledWith()
	for i := 0; i < MutatingSlots-1; i++ {
		if _, ok := e.s.adm.acquireMutating(classOther); !ok {
			t.Fatal("could not hold a general slot")
		}
	}
	e.errorAs(victim.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorBusy)
	a := e.r.NewAccount(t)
	e.commitOK(a, a.EnrollNew(t).Bytes) // D-179
}

func TestCommitBucketEmptyStillCommitsCancels(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.drainBucket()
	e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorBusy)
	e.commitOK(a, a.CancelByRoot(t).Bytes) // D-211: cancel_pending is exempt
}

func TestSubmitOnAStandbyIsNotLeader(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.s.hooks.InRecovery = func(context.Context, *sql.Conn) (bool, error) { return true, nil }
	e.s.hooks.Statfs = func() (uint64, error) { return DiskHardFloorBytes - 1, nil } // step 1a precedes 2b
	e.errorAs(a.Suspend(t, ac.ActorUser).Bytes, 503, ac.ErrorNotLeader)
}

func TestSubmitBelowTheHardDiskFloorIs503DiskForEveryRequest(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	rr := a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword)
	e.commitOK(a, rr.Bytes)
	e.s.hooks.Statfs = func() (uint64, error) { return DiskHardFloorBytes - 1, nil }
	e.errorAs(a.CancelByRoot(t).Bytes, 503, ac.ErrorDisk)
	e.errorAs(rr.Bytes, 503, ac.ErrorDisk) // a replay too (D-232)
}

// cancelAtReplicationProbe ends the request only after it reaches step 12.
type cancelAtReplicationProbe struct {
	cancel context.CancelFunc
	calls  int
}

func (p *cancelAtReplicationProbe) Replicated(context.Context, *sql.Conn, string) (bool, error) {
	p.calls++
	p.cancel()
	return false, nil
}

// D-200: the fresh, replay and refusal paths all wait on the gate.
func TestSubmitPathsPassTheGate(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	b := a.EnrollNew(t)
	e.commitOK(a, b.Bytes)
	fresh := e.r.NewAccount(t)
	bodies := map[string][]byte{
		"fresh":   fresh.EnrollNew(t).Bytes,
		"replay":  b.Bytes,
		"refusal": a.Suspend(t, ac.ActorOperator).Bytes, // recorded wrong_state
	}
	requireGate(t, e.f, true)
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := &cancelAtReplicationProbe{cancel: cancel}
			e.s.probe = probe
			out := e.s.Submit(ctx, body)
			if probe.calls == 0 {
				t.Fatal("request never reached the replication probe")
			}
			wantBody := ac.ErrorEnvelope{Code: string(ac.ErrorReplication)}.Encode()
			if out.HTTP != 503 || out.Kind != KindError || out.Code != string(ac.ErrorReplication) || !bytes.Equal(out.Body, wantBody) {
				t.Fatalf("with replication unconfirmed = %+v, want 503 replication and no signed outcome bytes", out)
			}
		})
	}
}

func TestBadCurrentRootSignatureIs400Unrecorded(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b := a.Build(t, authoritytest.Spec{Op: ac.OpRotateEK, NewEK: true, RootSigns: true, Factors: []string{ac.FactorPassword},
		PasswordLogin: true, Mutate: func(p *authoritytest.Parts) {
			if len(p.Submit.SigCurrentRoot) > 0 {
				p.Submit.SigCurrentRoot[7] ^= 1
			}
		}})
	e.errorAs(b.Bytes, 400, ac.ErrorBadSignature)
	if countRows(t, e, "request_ledger") != 0 {
		t.Fatal("a bad root signature was recorded")
	}
}

func TestPolicyRunsBeforeDeviceRules(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET force_unverified = true WHERE account_id = $1`, a.ID)
	a.Actor = authoritytest.NewDevice(t)                   // unknown to the set
	e.refusedAs(a.RotateEK(t).Bytes, ac.RefusalWrongState) // policy's wrong_state, not 8a's device_unknown (V-16)
}

// --- Head and bindings --------------------------------------------------------

func TestBurnedCandidatesOnCancelDisputeAndRevoke(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	p := a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword)
	e.commitOK(a, p.Bytes)
	e.commitOK(a, a.CancelByRoot(t).Bytes)
	reuse := a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, CandidateKey: p.Candidate,
		Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, WindowClear: true})
	e.refusedAs(reuse.Bytes, ac.RefusalRootReused)           // a cancelled candidate stays burned
	d := a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword) // within N_cancel → D
	e.commitOK(a, d.Bytes)
	revived := a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, CandidateKey: d.Candidate,
		Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, WindowClear: true})
	e.refusedAs(revived.Bytes, ac.RefusalRootReused) // a disputed candidate is burned too
}

// --- Population (enroll_existing; the test delegation includes it, D-168) -----

func TestPopulationRules(t *testing.T) {
	e, accts := newEnvPop(t, 5)
	pop, competitor, pinless, revoked, again := accts[0], accts[1], accts[2], accts[3], accts[4]
	e.refusedAs(pop.EnrollNew(t).Bytes, ac.RefusalInPopulation) // a population UUID never enrolls new
	// The refusal committed a genesis head with no stored pin; its status still
	// carries the first-sight pin (D-149, R5).
	if st, _, _ := readHead(t, e, pop.ID); st.Seq != 0 || !bytes.Equal(st.LegacyEKDigest, pop.Legacy.Digest) {
		t.Fatalf("status after a genesis refusal: seq %d, pin %x, want seq 0 and the population pin", st.Seq, st.LegacyEKDigest)
	}

	r := e.commitOK(competitor, competitor.EnrollExisting(t, true, ac.FactorPassword).Bytes) // row 2: continuity pending
	if r.State != ac.StatePending || r.Assurance != ac.AssuranceContinuity {
		t.Fatalf("row 2: %s/%s, want pending/continuity", r.State, r.Assurance)
	}
	e.commitOK(competitor, competitor.CancelByPoP(t).Bytes) // row 14 → Ap
	if competitor.Head.State != ac.StateAbsent {
		t.Fatalf("PoP cancel: %s, want absent (Ap)", competitor.Head.State)
	}
	if r := e.commitOK(competitor, competitor.EnrollExisting(t, true, ac.FactorPassword).Bytes); r.State != ac.StateDisputed {
		t.Fatalf("repeat after cancel: %s, want disputed", r.State)
	}

	fresh := authoritytest.NewLegacyKey(t) // a real key whose PoP verifies but which is not the pinned one,
	pinless.Legacy = &fresh                // so the pin comparison alone refuses it (mutation I2-12)
	e.refusedAs(pinless.EnrollExisting(t, true, ac.FactorPassword).Bytes, ac.RefusalLegacyMismatch)

	e.commitOK(revoked, revoked.RevokeRoot(t).Bytes) // row 17: Ap → Apr
	if r := e.commitOK(revoked, revoked.EnrollExisting(t, false, ac.FactorEmail, ac.FactorPassword).Bytes); r.Assurance != ac.AssuranceUnverified || r.LegacyPinRevoked != 1 {
		t.Fatalf("row 3 from Apr: %+v, want unverified with the pin revoked", r)
	}
	// Row 4 against that pending head: the PoP verifies and the pin digest matches,
	// but the pin is revoked, so S does not meet the floor (B1-D35, A2 floor.go:121).
	e.refusedAs(revoked.EnrollExisting(t, true, ac.FactorPassword).Bytes, ac.RefusalInsufficientAuthorization)

	epoch := ac.Digest([]byte("epoch"))
	e.commitOK(again, again.Build(t, authoritytest.Spec{Op: ac.OpEnrollExisting, NewRoot: true, NewEK: true, PoP: true,
		EpochDigest: epoch, Factors: []string{ac.FactorPassword}, PasswordLogin: true}).Bytes)
	same := again.Build(t, authoritytest.Spec{Op: ac.OpEnrollExisting, NewRoot: true, NewEK: true, PoP: true,
		EpochDigest: epoch, Factors: []string{ac.FactorPassword}, PasswordLogin: true})
	e.refusedAs(same.Bytes, ac.RefusalWrongState) // same-epoch competitor
	noPoP := again.Build(t, authoritytest.Spec{Op: ac.OpEnrollExisting, NewRoot: true, NewEK: true,
		Factors: []string{ac.FactorPassword}, PasswordLogin: true})
	e.refusedAs(noPoP.Bytes, ac.RefusalInsufficientAuthorization) // row 4 without PoP needs R_full or R_email
	withPoP := again.Build(t, authoritytest.Spec{Op: ac.OpEnrollExisting, NewRoot: true, NewEK: true, PoP: true,
		Factors: []string{ac.FactorPassword}, PasswordLogin: true})
	if r := e.commitOK(again, withPoP.Bytes); r.State != ac.StateDisputed {
		t.Fatalf("row 4 with PoP and S: %s, want disputed", r.State)
	}
}

// --- Assurance ----------------------------------------------------------------

func TestUnverifiedSegmentCapsEveryLaterReplace(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	newDev := authoritytest.NewDevice(t)
	e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, NewDevice: newDev,
		Factors: []string{ac.FactorEmail}, PasswordLogin: true, WindowClear: true}).Bytes) // R_email only → unverified
	e.activate()
	a.Activated()
	// Derived activation writes no entry (DR8): the root and the EK move in a's view, and
	// the pending device becomes the only active one (D-190).
	a.Head.Root, a.Head.EKGen, a.Actor = authoritytest.Point(a.Root), a.Head.EKGen+1, newDev
	e.commitOK(a, a.RotateEK(t).Bytes) // signed by an unverified-segment root: not a qualifying op
	full := a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, PasswordLogin: true,
		WindowClear: true, Factors: []string{ac.FactorEmail, ac.FactorPassword}}) // R_full, markers clear (as a DB writer could forge)
	if r := e.commitOK(a, full.Bytes); r.Assurance != ac.AssuranceUnverified {
		t.Fatalf("replace from an unverified segment = %s, want unverified (D-182)", r.Assurance)
	}
}

func TestForceUnverifiedColumn(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET force_unverified = true WHERE account_id = $1`, a.ID)
	e.refusedAs(a.RotateEK(t).Bytes, ac.RefusalWrongState)
	e.refusedAs(a.RotateRoot(t).Bytes, ac.RefusalWrongState)
	e.deviceRefused(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes, ac.RefusalWrongState)
	if r := e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes); r.Assurance != ac.AssuranceUnverified {
		t.Fatalf("row 11 under force_unverified = %s, want unverified", r.Assurance)
	}
	if r := e.commitOK(a, a.CancelByRoot(t).Bytes); r.Assurance != "" {
		t.Fatalf("row 13 under force_unverified = %q, want \"\" (V-12)", r.Assurance)
	}
}

// --- Suspension and revocation ------------------------------------------------

func TestRevokeClearsTheThiefsCancelAndTheReplaceRunsUnvetoedAsUnverified(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.commitOK(a, a.CancelByRoot(t).Bytes) // the thief, who holds the root, cancels
	if r := e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes); r.State != ac.StateDisputed {
		t.Fatalf("the owner's retry: %s, want disputed", r.State)
	}
	e.commitOK(a, a.RevokeRoot(t).Bytes) // row 16: Sor, clears last_cancel_at (D-145)
	r := e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	if r.State != ac.StatePending || r.Assurance != ac.AssuranceUnverified {
		t.Fatalf("replace after revoke: %s/%s, want pending/unverified", r.State, r.Assurance)
	}
}

func TestReplaceFromSoIsWrongState(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)                                          // row 8 → So
	e.refusedAs(a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes, ac.RefusalWrongState) // D-141
}

func TestCancelRestoresTheSetAndActivationRemovesOperatorOnlyFromSor(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Suspend(t, ac.ActorUser).Bytes) // Su {user}
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	if r := e.commitOK(a, a.CancelByRoot(t).Bytes); r.State != ac.StateSuspended || len(r.SuspendedBy) != 1 {
		t.Fatalf("cancel from P(suspended): %+v, want Su with {user} restored", r)
	}
	e.commitOK(a, a.RevokeRoot(t).Bytes) // row 15: Sor {operator, user}
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.activate()
	st, _, _ := readHead(t, e, a.ID)
	if st.State != ac.StateSuspended || len(st.SuspendedBy) != 1 || st.SuspendedBy[0] != ac.ActorUser || st.RootRevoked != 0 {
		t.Fatalf("activation from Sor: %+v, want suspended {user}, root not revoked", st)
	}
}

// --- Erase --------------------------------------------------------------------

func TestEraseRules(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes)                     // Pe
	e.commitOK(a, a.CancelByRoot(t).Bytes)                                               // the root cancels a Pe
	e.refusedAs(a.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes, ac.RefusalWrongState) // D-186
	e.commitOK(a, a.RevokeRoot(t).Bytes)                                                 // clears last_erase_cancel_at
	e.commitOK(a, a.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes)                     // admitted after revoke
	e.activate()
	if e.state(a) != ac.StateErased {
		t.Fatal("Pe did not activate at visible")
	}
	b, _ := e.enrolledWith()
	if r := e.commitOK(b, b.Erase(t, ac.ActorOperator).Bytes); r.State != ac.StateErased {
		t.Fatalf("row 19: %s, want erased at once", r.State)
	}
}

func TestErasureListEraseRespectsGPrime(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	mustExec(t, e.f.Owner, `UPDATE authority.meta SET reconcile_completed_at = to_timestamp($1 / 1000.0)`, e.now.Load())
	floor := e.now.Load() + GPrimeMs
	e.commitOK(a, a.Erase(t, ac.ActorUser, ac.FactorErasureList).Bytes)
	e.activate()
	if e.state(a) != ac.StatePending {
		t.Fatal("an erasure-list erase activated before reconcile_completed_at + G′ (D-186)")
	}
	e.now.Store(floor)
	e.r.Clock.Ms = floor
	if e.state(a) != ac.StateErased {
		t.Fatal("the erase did not activate at the G′ floor")
	}
}

// --- Activation and the private set -------------------------------------------

func TestRotateEKGenerationAndActivationInstallsThePendingDevice(t *testing.T) {
	e := newEnv(t)
	a, genesis := e.enrolledWith()
	e.commitOK(a, a.RotateEK(t).Bytes)
	if a.Head.EKGen != 2 {
		t.Fatalf("ek_generation after rotate_ek = %d, want 2", a.Head.EKGen)
	}
	newDev := authoritytest.NewDevice(t)
	e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, NewDevice: newDev,
		Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, WindowClear: true}).Bytes)
	e.activate()
	a.Activated()
	a.Actor = newDev
	e.commitOK(a, a.Suspend(t, ac.ActorUser).Bytes) // any commit persists Normalize
	set := readSet(t, e, a.ID)
	if len(set.Active) != 1 || bytes.Contains([]byte(set.Active[0]), []byte(genesis.ID)) || set.Pending != "" {
		t.Fatalf("after activation the set is %+v, want only the pending device (D-190)", set)
	}
}

func TestCancelAndRow16DiscardThePendingDevice(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.commitOK(a, a.CancelByRoot(t).Bytes)
	if readSet(t, e, a.ID).Pending != "" {
		t.Fatal("cancel kept the pending device (DR8)")
	}
	b, _ := e.enrolledWith()
	e.commitOK(b, b.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	e.commitOK(b, b.RevokeRoot(t).Bytes) // row 16
	if readSet(t, e, b.ID).Pending != "" {
		t.Fatal("row 16 kept the pending device (DR8)")
	}
}

// --- Operator key (LD-4, LD-37, AC24) -----------------------------------------

func TestOperatorRowsRefusedUnderTheGADelegation(t *testing.T) {
	e := newEnv(t, authoritytest.WithoutOperatorKeys())
	a, _ := e.enrolledWith()
	e.errorAs(a.Suspend(t, ac.ActorOperator).Bytes, 400, ac.ErrorBadSignature)
}

type failOn struct {
	authoritytestSigner
	tag string
}

func (f failOn) Sign(m []byte) ([]byte, error) {
	if bytes.Contains(m, []byte(`"`+f.tag+`"`)) {
		return nil, errSigner
	}
	return f.authoritytestSigner.Sign(m)
}

func TestDeviceResultSignerFailureLeavesNoRows(t *testing.T) {
	e := newEnv(t)
	e.s.sig = failOn{e.r.RSK, "device-result"}
	a := e.r.NewAccount(t)
	e.errorAs(a.EnrollNew(t).Bytes, 503, ac.ErrorSigner)
	if countRows(t, e, "account_events")+countRows(t, e, "device_events") != 0 {
		t.Fatal("the device-result was signed outside the transaction: rows survived its failure")
	}
}

// lyingSigner returns a signature over every result that fails verification,
// and signs device-results honestly, so only the result's self-verify can catch it.
type lyingSigner struct{ authoritytestSigner }

func (l lyingSigner) Sign(m []byte) ([]byte, error) {
	sig, err := l.authoritytestSigner.Sign(m)
	if err == nil && len(sig) > 0 && !bytes.Contains(m, []byte(`"device-result"`)) {
		sig[len(sig)-1] ^= 1
	}
	return sig, err
}

func TestSelfVerifyFailureIs503WithZeroRows(t *testing.T) {
	e := newEnv(t)
	e.s.sig = lyingSigner{e.r.RSK}
	a := e.r.NewAccount(t)
	e.errorAs(a.EnrollNew(t).Bytes, 503, ac.ErrorSigner)
	if countRows(t, e, "account_events")+countRows(t, e, "key_bindings")+countRows(t, e, "device_events") != 0 {
		t.Fatal("a result whose signature failed self-verification was written (step 11)")
	}
}
