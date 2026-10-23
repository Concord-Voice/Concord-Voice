package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func bundleOf(sub ac.Submit) ac.Bundle {
	return ac.Bundle{Intent: sub.Intent, SigCandidate: sub.SigCandidate, SigCurrentRoot: sub.SigCurrentRoot,
		SigOperator: sub.SigOperator, SigRecovery: sub.SigRecovery, LegacyPopSig: sub.LegacyPopSig, LegacySPKI: sub.LegacySPKI,
		EKBinding: sub.EKBinding, EKBindingSig: sub.EKBindingSig, RecoveryBind: sub.RecoveryBind,
		RecoveryBindSig: sub.RecoveryBindSig, CPAuthz: sub.CPAuthz, CPAuthzSig: sub.CPAuthzSig}
}

func (h *headRow) set() (ac.DeviceSet, []byte, error) {
	b := h.DeviceSet
	if b == nil {
		b = ac.EmptyDeviceSet(h.RealmID, h.AccountID)
	}
	ds, err := ac.DecodeDeviceSet(b)
	return ds, b, err
}

// decide is steps 8–11.
func (s *Store) decide(ctx context.Context, st *txState) Outcome {
	rq, h := st.rq, st.h
	if recoverGuardBusy(st) { // D-271
		return errorOutcome(503, ac.ErrorBusy)
	}
	d, ds, err := admit(st)
	if err != nil {
		return s.storeError()
	}
	if d.Refusal != "" {
		return s.refuseMaybeArm(ctx, st, ds, d.Refusal)
	}
	if !verifyHeadSigs(rq, h) { // B1-D17
		return errorOutcome(400, ac.ErrorBadSignature)
	}
	newSet, out, done := s.step8a(ctx, st, d)
	if done {
		return out
	}
	if out, done := s.step9(ctx, st); done {
		return out
	}
	if out, done := s.bindKeys(ctx, st, bindingsOf(rq), h.Seq+1); done {
		return out
	}
	return s.step11(ctx, st, d, newSet)
}

// admit is step 8's policy decision on st's request against st's normalized
// head, and the head's private set. The commit path and import's replay
// (import.go) both decide through it, so the two cannot build a different
// policy input for one request (B3-D4).
func admit(st *txState) (policy.Decision, ac.DeviceSet, error) {
	rq, h := st.rq, st.h
	ds, _, err := h.set()
	if err != nil {
		return policy.Decision{}, ac.DeviceSet{}, err
	}
	ev := ac.EvidenceOf(rq.Ev)
	ev.PoP, ev.RecoveryKeyBound = rq.PoP, len(ds.RecoveryKey) > 0
	ev.LegacyPinLive = !h.PinRevoked // B1-D35: row 4's PoP floor needs the from-state's pin unrevoked
	return policy.Admit(policy.Input{Head: h.policyHead(st.inPop), Bundle: bundleOf(rq.Sub), Intent: rq.Intent,
		Actor: rq.AZ.Actor, Evidence: ev, EKBinding: rq.EKB, EKBindingBytes: rq.Sub.EKBinding,
		EpochDigest: rq.Ev.CredentialEpochDigest, Scope: st.sig.D.Scope, DBNowMs: st.now}), ds, nil
}

// verifyHeadSigs: sig_current_root and rotate_ek's ek-binding signature under
// the normalized head's root (B1-D17).
func verifyHeadSigs(rq *request, h *headRow) bool {
	sub := rq.Sub
	if len(sub.SigCurrentRoot) > 0 && (len(h.Root) == 0 ||
		!ac.VerifyP384(h.Root, ac.Sig{Role: ac.RoleCurrentRoot, Digest: rq.IntentDigest}.Encode(), sub.SigCurrentRoot)) {
		return false
	}
	if rq.Intent.Op == ac.OpRotateEK && (len(h.Root) == 0 || !ac.VerifyP384(h.Root, sub.EKBinding, sub.EKBindingSig)) {
		return false
	}
	return true
}

// refuseMaybeArm records a step-8 refusal and, for a stale recover under the
// bound key built on the head published when it was sent, arms the recover
// guard in the same COMMIT (D-271, RB3-1, RT6-2).
func (s *Store) refuseMaybeArm(ctx context.Context, st *txState, ds ac.DeviceSet, code ac.Refusal) Outcome {
	rq, h := st.rq, st.h
	stale := code == ac.RefusalStaleSeq || code == ac.RefusalStaleHead
	armed := h.RecoverGuardUntilMs != nil && *h.RecoverGuardUntilMs > st.now
	if rq.Intent.Op == ac.OpRecover && stale && !armed && bytes.Equal(rq.H, ds.RecoveryBindDigest) {
		ok, err := namedPublishedHead(ctx, st)
		if err != nil {
			return s.storeError()
		}
		if ok {
			until := st.now + st.sig.D.GMs
			if _, err := st.tx.ExecContext(ctx, `UPDATE authority.account_heads SET recover_guard_until_ms = $2
				WHERE account_id = $1`, h.AccountID, i64(until)); err != nil {
				return s.storeError()
			}
		}
	}
	return s.record(ctx, st, code)
}

// namedPublishedHead: expected_head_digest is the entry at expected_seq, and
// that entry's successor was issued at or after asserted_at − ε (RT6-2).
func namedPublishedHead(ctx context.Context, st *txState) (bool, error) {
	in := st.rq.Intent
	var entry []byte
	var succIssued int64
	err := st.tx.QueryRowContext(ctx, `SELECT e.entry_digest, n.issued_at_ms FROM authority.account_events e
		JOIN authority.account_events n ON n.account_id = e.account_id AND n.seq = e.seq + 1
		WHERE e.account_id = $1 AND e.seq = $2`, in.AccountID, i64(in.ExpectedSeq)).Scan(&entry, &succIssued)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(entry, in.ExpectedHeadDigest) && u64(succIssued)+ac.EpsilonMs >= st.rq.AZ.AssertedAtMs, nil
}

func outcomeCode(d policy.Decision) string {
	switch d.Post.State {
	case ac.StatePending:
		return "P"
	case ac.StateDisputed:
		return "D"
	}
	return ""
}

func findActive(ds ac.DeviceSet, id string) *ac.DeviceEntry {
	for _, s := range ds.Active {
		if e, err := ac.ParseDev(s); err == nil && e.ID == id {
			return &e
		}
	}
	return nil
}

func newEntry(di *ac.DeviceIntent) *ac.DeviceEntry {
	if di.NewDeviceID == "" {
		return nil
	}
	e := &ac.DeviceEntry{ID: di.NewDeviceID}
	copy(e.DSK[:], di.NewDeviceDSK)
	copy(e.DKX[:], di.NewDeviceDKX)
	return e
}

func (h *headRow) headInput() ac.HeadInput {
	var rd []byte
	if len(h.Root) > 0 {
		rd = ac.KeyDigest(h.Root)
	}
	return ac.HeadInput{State: h.State, Seq: h.Seq, EntryDigest: h.Entry, RootDigest: rd, ForceUnverified: h.ForceUnverified}
}

// step8a is §3.2.4 step 8a for a published op, and DR8's entry-less change for
// a published op with no device part. newSet nil means "unchanged".
func (s *Store) step8a(ctx context.Context, st *txState, d policy.Decision) ([]byte, Outcome, bool) {
	rq, h := st.rq, st.h
	ds, cur, err := h.set()
	if err != nil {
		return nil, s.storeError(), true
	}
	if d.Row.N == 21 { // recovery-key cancel_pending
		if out, done := s.cancelPendingSig(ctx, st); done {
			return nil, out, true
		}
	}
	if rq.DI == nil {
		return entryless(cur, d.Row.N), Outcome{}, false
	}
	next, out, done := s.step8aDeviceOp(ctx, st, d, ds, cur)
	if done {
		return nil, out, true
	}
	if rq.Intent.Op == ac.OpRecover { // B1-D18
		if out, done := s.recoverBindSig(ctx, st); done {
			return nil, out, true
		}
	}
	return next, Outcome{}, false
}

// cancelPendingSig is row 21's sig_recovery under the pending recover's
// B.recovery_key.
func (s *Store) cancelPendingSig(ctx context.Context, st *txState) (Outcome, bool) {
	rq := st.rq
	key, err := pendingRecoverKey(ctx, st)
	if err != nil {
		return s.storeError(), true
	}
	if !ac.VerifyP384(key, ac.Sig{Role: ac.RoleRecoveryKey, Digest: rq.IntentDigest}.Encode(), rq.Sub.SigRecovery) {
		return errorOutcome(400, ac.ErrorBadSignature), true
	}
	return Outcome{}, false
}

// step8aDeviceOp is step 8a's (1)–(6) for a published op with a device part.
func (s *Store) step8aDeviceOp(ctx context.Context, st *txState, d policy.Decision, ds ac.DeviceSet, cur []byte) ([]byte, Outcome, bool) {
	rq, h, di := st.rq, st.h, st.rq.DI
	if di.ActorDeviceID != "" { // (1) V2-01
		if di.ExpectedDseq != h.Dseq || !bytes.Equal(di.ExpectedDeviceHead, h.DeviceHead) {
			return nil, s.record(ctx, st, ac.RefusalStaleDeviceSeq), true
		}
	}
	// ApplyDeviceOp is pure and runs DR1 before DR4, so its device_unknown is
	// (2) for every half of DR1, removed IDs included, recorded before (3).
	anb, _ := anbOf(st)
	next, r := ac.ApplyDeviceOp(cur, ac.DeviceOpInput{Op: rq.Intent.Op, Actor: di.ActorDeviceID, New: newEntry(di),
		Removed: di.RemovedDeviceIDs, Bind: rq.Sub.RecoveryBind, Head: h.headInput(), Outcome: outcomeCode(d),
		ProposingAnbMs: anb}, st.now, st.sig.D.DeltaMs) // (4) DR4, (5) DR2, (6) DR9 (e), DR5/DR6
	if r == ac.RefusalDeviceUnknown {
		return nil, s.record(ctx, st, r), true
	}
	if di.ActorDeviceID != "" {
		actor := findActive(ds, di.ActorDeviceID) // (2) DR1
		if actor == nil {
			return nil, s.record(ctx, st, ac.RefusalDeviceUnknown), true
		}
		if !ac.VerifyP384(actor.DSK[:], ac.Sig{Role: ac.RoleActorDevice, Digest: rq.D}.Encode(), rq.Sub.SigActor) { // (3) DR3
			return nil, errorOutcome(400, ac.ErrorBadSignature), true
		}
	}
	if r != "" {
		return nil, s.record(ctx, st, r), true
	}
	return next, Outcome{}, false
}

// recoverBindSig is a recover's recovery_bind_sig check (bindSigMatches).
func (s *Store) recoverBindSig(ctx context.Context, st *txState) (Outcome, bool) {
	ok, err := bindSigMatches(ctx, st)
	if err != nil {
		return s.storeError(), true
	}
	if !ok {
		return errorOutcome(400, ac.ErrorBadSignature), true
	}
	return Outcome{}, false
}

// entryless is DR8 for a published op with no device part: rows 13, 14
// (cancel), 16, 17 (revoke from P or D) and 21 (the recovery-key cancel of a
// Pr head, which keeps the bound key) set pending := "".
func entryless(cur []byte, row int) []byte {
	switch row {
	case 13, 14, 16, 17, 21:
		return ac.NormalizeDeviceSet(cur, false, false, false, true, false)
	}
	return nil
}

// pendingRecoverKey reads B.recovery_key from the pending recover's stored bundle.
func pendingRecoverKey(ctx context.Context, st *txState) ([]byte, error) {
	var bundle []byte
	if err := st.tx.QueryRowContext(ctx, `SELECT bundle FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
		st.h.AccountID, i64(st.h.Seq)).Scan(&bundle); err != nil {
		return nil, err
	}
	b, err := ac.DecodeBundle(bundle)
	if err != nil {
		return nil, err
	}
	rb, err := ac.DecodeRecoveryBind(b.RecoveryBind)
	if err != nil {
		return nil, err
	}
	return rb.RecoveryKey, nil
}

// bindSigMatches: recovery_bind_sig equals the sig_root of the stored
// bind_recovery entry whose B digest is H(B) (LD-45).
func bindSigMatches(ctx context.Context, st *txState) (bool, error) {
	rows, err := st.tx.QueryContext(ctx, `SELECT bundle FROM authority.device_events WHERE account_id = $1 AND op = 'bind_recovery'`,
		st.h.AccountID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		db, err := ac.DecodeDeviceBundle(raw)
		if err != nil {
			return false, err
		}
		if bytes.Equal(ac.Digest(db.RecoveryBind), st.rq.H) {
			return bytes.Equal(db.SigRoot, st.rq.Sub.RecoveryBindSig), rows.Err()
		}
	}
	return false, rows.Err()
}

// recoverGuardBusy is D-271's guard: a non-cancel while a stale recover's
// guard is armed past db_now. Live it is 503 busy; import's replay holds the
// account on it, since the guard only moves later (refuseMaybeArm arms it,
// nothing clears it, and import replays no refusal), so a head the original
// commit passed it on passes it here.
func recoverGuardBusy(st *txState) bool {
	h := st.h
	return st.rq.Class != classCancel && h.RecoverGuardUntilMs != nil && *h.RecoverGuardUntilMs > st.now
}

// issuedNotBefore is step 9's first clause (D-36): db_now ≥ max(issued_at)
// over the account's published head and its private entries. Live a failure
// is 503 clock; import's replay holds the account, at the result's issued_at.
func issuedNotBefore(ctx context.Context, st *txState) (bool, error) {
	var devMax int64
	if err := st.tx.QueryRowContext(ctx, `SELECT COALESCE(max(issued_at_ms), 0) FROM authority.device_events WHERE account_id = $1`,
		st.h.AccountID).Scan(&devMax); err != nil {
		return false, err
	}
	return st.now >= st.h.IssuedAtMs && st.now >= u64(devMax), nil
}

// step9: db_now ≥ max(issued_at) over the account's published and private
// entries (D-36), and this node's RSK is listed in the signing delegation (T10).
func (s *Store) step9(ctx context.Context, st *txState) (Outcome, bool) {
	ok, err := issuedNotBefore(ctx, st)
	if err != nil {
		return s.storeError(), true
	}
	if !ok {
		s.nonClient503.Store(true)
		return errorOutcome(503, ac.ErrorClock), true
	}
	if !s.rskListed(st.sig) {
		return s.signerError(), true
	}
	return Outcome{}, false
}

// rskListed is T10's last clause: this node's own RSK is listed in the
// delegation it signs under. Every path that signs uses it: step 9, row 22
// and the signed reads.
func (s *Store) rskListed(d *sigDelegation) bool {
	own := s.sig.PublicKey()
	for _, k := range d.D.RSKPublicKeys {
		if bytes.Equal(k, own) {
			return true
		}
	}
	return false
}
