package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// ErrNotHeldPendingOrDisputed refuses row 22 on a head that is not a held P, D or Pr.
var ErrNotHeldPendingOrDisputed = errors.New("authority store: row 22 needs a held P, D or Pr head")

// commitRestoreCancel is clear-held's row 22. A non-nil error means nothing
// committed. Its refusals of this account are ErrNotHeldPendingOrDisputed and
// ErrRefused (the policy does not admit row 22, or the result fails the client
// check), which clear-held records as the account's outcome; every other error
// is the database's, the clock's, the signer's or the leader check's, and
// stops clear-held's run (clearRefusal; Codex round 2, G5).
func (s *Store) commitRestoreCancel(ctx context.Context, accountID string) (Outcome, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return s.storeError(), err
	}
	defer func() { _ = conn.Close() }()
	if out, ok := s.leaderCheck(ctx, conn); !ok { // an error, like every refusal: row 22 did not commit
		if out.Code == string(ac.ErrorNotLeader) { // a failover mid-run exits not_leader, not config
			return out, fmt.Errorf("authority store: row 22 refused before BEGIN: %w", ErrNotLeader)
		}
		return out, errors.New("authority store: row 22 refused before BEGIN: " + out.Code)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return s.storeError(), err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return s.storeError(), err
	}
	st := &txState{tx: tx, rq: &request{}}
	if st.h, st.inPop, err = lockHead(ctx, tx, accountID); err != nil {
		return s.storeError(), err
	}
	code := st.h.published().FromCode()
	if !st.h.Held || (code != ac.FromP && code != ac.FromD && code != ac.FromPr) {
		return Outcome{}, ErrNotHeldPendingOrDisputed
	}
	if err := s.readNowAfterL1(ctx, st); err != nil {
		return s.signerOrStore(err), err
	}
	// D-241 (DoR:478, B1-D26): held_status is a status, so it takes the reads'
	// fence. Under L1 the head cannot change, so db_now is re-read in place.
	for try := 0; !s.reserveAsOf([]string{accountID}, st.now); try++ {
		if try == asOfRetries {
			s.nonClient503.Store(true)
			return errorOutcome(503, ac.ErrorClock), errors.New("authority store: db_now never passed the account's last as_of_ms")
		}
		s.pause()
		if err := s.readNowAfterL1(ctx, st); err != nil {
			return s.signerOrStore(err), err
		}
	}
	if err := s.resolveSigner(ctx, st); err != nil { // for the db_now the fence settled on
		return s.signerOrStore(err), err
	}
	out, err := s.restoreCancelTx(ctx, st)
	if err != nil {
		return out, err
	}
	return s.release(ctx, conn, out), nil
}

func (s *Store) restoreCancelTx(ctx context.Context, st *txState) (Outcome, error) {
	// Step 9 binds row 22 as it binds every signed result: db_now ≥ max(issued_at)
	// (D-36) and this node's RSK listed in the signing delegation (T10).
	if out, done := s.step9(ctx, st); done {
		return out, errors.New("authority store: row 22 refused at step 9: " + out.Code)
	}
	// T6 (DoR:347, :720): held_status is a status, so `status` must be in the
	// signing delegation's scope, or every client rejects the bundle (review round 3).
	if !slices.Contains(st.sig.D.Scope, ac.ScopeStatus) {
		return s.signerError(), errors.New("authority store: row 22 needs status in the signing delegation's scope")
	}
	h, T := st.h, st.now
	n := normOut{} // a held head never normalizes (step 7)
	held, err := s.signedOf("status", statusOf(h.RealmID, h, n, T, st.sig).Encode())
	if err != nil {
		return s.signerOrStore(err), err
	}
	in := ac.Intent{RealmID: h.RealmID, AccountID: h.AccountID, RequestID: uuid.NewString(), Op: ac.OpCancelPending,
		ExpiresAtMs: T + ac.IntentWindowMs, ExpectedSeq: h.Seq, ExpectedRecoveryGen: h.RG, ExpectedHeadDigest: h.Entry}
	ib := in.Encode()
	az := ac.CPAuthz{RealmID: h.RealmID, AccountID: h.AccountID, RequestID: in.RequestID, IntentDigest: ac.Digest(ib),
		Actor: ac.ActorRestore, AssertedAtMs: T, EvidenceDigest: ac.Digest(held)}
	azb := az.Encode()
	st.rq = &request{Intent: in, IntentDigest: ac.Digest(ib), AZ: az, Sub: ac.Submit{Intent: ib, CPAuthz: azb}}
	bundle := ac.Bundle{Intent: ib, CPAuthz: azb, HeldStatus: held}
	d := restoreCancelDecision(st, bundle)
	if d.Refusal != "" || d.Row.N != 22 {
		return s.storeError(), fmt.Errorf("%w: row 22 not admitted: %s", ErrRefused, d.Refusal)
	}
	res, ok := s.buildResult(st, d)
	if !ok || ac.CheckPost(d.Row, h.published(), in, res, false, ac.Assurance(h.Segment), ac.Assurance(h.RecoveryBindAssurance)) != ac.ReasonOK {
		return s.storeError(), fmt.Errorf("%w: row-22 result fails the client check", ErrRefused)
	}
	rb := res.Encode()
	rsig, err := signer.SignAndVerify(s.sig, rb)
	if err != nil {
		return s.signerError(), err
	}
	bundle.Result, bundle.ResultSig = rb, rsig
	if err := applyRestoreCancel(ctx, st, d, res, bundle.Encode()); err != nil {
		return s.storeError(), err
	}
	if err := st.tx.Commit(); err != nil {
		return s.storeError(), err
	}
	return Outcome{HTTP: 200, Kind: KindCommitted, Body: ac.Committed{Result: rb, ResultSig: rsig}.Encode()}, nil
}

// restoreCancelDecision is row 22's policy decision on st's head as it is,
// never normalized: a held head never normalizes (step 7), and import's replay
// of row 22 takes the raw from-head its own held_status names. policy.Admit
// does not read held, so the decision is the same whether or not the head it
// lands on is held (D-288). clear-held and import both decide through it.
func restoreCancelDecision(st *txState, bundle ac.Bundle) policy.Decision {
	return policy.Admit(policy.Input{Head: st.h.policyHead(st.inPop), Bundle: bundle, Intent: st.rq.Intent,
		Actor: ac.ActorRestore, Scope: st.sig.D.Scope, DBNowMs: st.now})
}

// applyRestoreCancel writes row 22's event and moves the head to it: the event
// carries no cp_evidence (NULL, E3-02) and asserted_at = issued_at = st.now;
// the private set takes DR8 (pending := "", the slot emptied, the bound key
// kept). imported is st.replay. clear-held (restoreCancelTx) and import
// (importdevice.go) both write row 22 through it.
func applyRestoreCancel(ctx context.Context, st *txState, d policy.Decision, res ac.Result, bundle []byte) error {
	h, rb := st.h, res.Encode()
	if rb == nil {
		return errors.New("authority store: row 22's result does not encode")
	}
	entry := ac.EntryDigest(rb)
	if _, err := st.tx.ExecContext(ctx, `INSERT INTO authority.account_events (account_id, seq, restore_epoch, request_id,
		intent_digest, op, bundle, cp_evidence, entry_digest, prev_entry_digest, delegation_gen, delegation_serial,
		issued_at_ms, asserted_at_ms, imported) VALUES ($1, $2, $3, $4, $5, 'cancel_pending', $6, NULL, $7, $8, $9, $10, $11, $11, $12)`,
		h.AccountID, i64(res.Seq), i64(st.restoreEpoch), st.rq.Intent.RequestID, st.rq.IntentDigest, bundle, entry,
		nilIfEmpty(h.Entry), i64(res.DelegationGen), i64(res.DelegationSerial), i64(st.now), st.replay); err != nil {
		return err
	}
	_, cur, err := h.set()
	if err != nil {
		return err
	}
	next := ac.NormalizeDeviceSet(cur, false, false, false, true, true) // DR8: pending := "", slot emptied, bound key kept
	if err := applyCommit(st, d, res, entry, next, nil); err != nil {
		return err
	}
	return saveHead(ctx, st.tx, h)
}
