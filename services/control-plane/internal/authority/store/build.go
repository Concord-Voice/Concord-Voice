package store

import (
	"bytes"
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// anbOf is R2: asserted_at + delta_ms + g_ms of the signing delegation.
func anbOf(st *txState) (uint64, bool) {
	s := st.rq.AZ.AssertedAtMs + st.sig.D.DeltaMs + st.sig.D.GMs
	return s, s <= ac.MaxSafeInt && s >= st.rq.AZ.AssertedAtMs
}

func (s *Store) buildResult(st *txState, d policy.Decision) (ac.Result, bool) {
	rq, h := st.rq, st.h
	res := ac.Result{RealmID: rq.Intent.RealmID, AccountID: rq.Intent.AccountID, RequestID: rq.Intent.RequestID,
		IntentDigest: rq.IntentDigest, CPAuthzDigest: ac.Digest(rq.Sub.CPAuthz), Op: rq.Intent.Op, Seq: h.Seq + 1,
		State: d.Post.State, PriorState: d.Post.Prior, SuspendedBy: d.Post.Susp, ActiveRoot: d.Post.Root,
		RootRevoked: d.Post.RootRevoked, EKBindingDigest: d.Post.EK, PendingRoot: d.Post.PendRoot,
		PendingEKBindingDigest: d.Post.PendEK, LegacyEKDigest: h.Pin, LegacyPinRevoked: d.Post.PinRevoked,
		PrevEntryDigest: h.Entry, RecoveryGen: h.RG + d.Row.DeltaRG, Assurance: string(d.Assurance),
		IssuedAtMs: st.now, DelegationGen: st.sig.Gen, DelegationSerial: st.sig.Serial}
	if res.State == ac.StatePending {
		anb, ok := anbOf(st)
		if !ok {
			return res, false
		}
		res.ActivationNotBeforeMs = anb
	}
	return res, true
}

// step11 builds, checks (AC9), signs, writes and commits.
func (s *Store) step11(ctx context.Context, st *txState, d policy.Decision, newSet []byte) Outcome {
	rq, h := st.rq, st.h
	res, ok := s.buildResult(st, d)
	if !ok {
		return s.storeError()
	}
	if ac.CheckPost(d.Row, h.published(), rq.Intent, res, d.OutcomeP, ac.Assurance(h.Segment),
		ac.Assurance(h.RecoveryBindAssurance)) != ac.ReasonOK {
		return s.storeError() // AC9: never sign what a client would refuse
	}
	rb := res.Encode()
	if rb == nil {
		return s.storeError()
	}
	rsig, err := signer.SignAndVerify(s.sig, rb)
	if err != nil {
		return s.signerError()
	}
	entry := ac.EntryDigest(rb)
	var drb, dsig []byte
	if rq.DI != nil {
		if newSet == nil {
			newSet = h.DeviceSet
		}
		sd, err := ac.DeviceSetDigest(newSet)
		if err != nil {
			return s.storeError()
		}
		drb = ac.DeviceResult{RealmID: res.RealmID, AccountID: res.AccountID, RequestID: res.RequestID,
			DeviceIntentDigest: rq.D, Op: res.Op, Dseq: h.Dseq + 1, // #7 "": attached (DoR:446, B1-D9)
			PrevDeviceEntryDigest: h.DeviceHead, ChainSeq: res.Seq, ChainEntryDigest: entry, DeviceSetDigest: sd,
			IssuedAtMs: st.now, DelegationGen: st.sig.Gen, DelegationSerial: st.sig.Serial}.Encode()
		if drb == nil {
			return s.storeError()
		}
		if dsig, err = signer.SignAndVerify(s.sig, drb); err != nil {
			return s.signerError()
		}
	}
	if err := s.writeEvents(ctx, st, res, signedOut{rb: rb, rsig: rsig, entry: entry, drb: drb, dsig: dsig}); err != nil {
		return s.storeError()
	}
	if err := applyCommit(st, d, res, entry, newSet, drb); err != nil {
		return s.storeError()
	}
	if err := saveHead(ctx, st.tx, h); err != nil {
		return s.storeError()
	}
	if err := st.tx.Commit(); err != nil {
		return s.storeError()
	}
	return Outcome{HTTP: 200, Kind: KindCommitted,
		Body: ac.Committed{Result: rb, ResultSig: rsig, DeviceResult: drb, DeviceResultSig: dsig}.Encode()}
}

// signedOut is what step 11 signed: the encoded result and its signature, its
// entry digest, and the device result and its signature (nil when no DI).
type signedOut struct {
	rb, rsig, entry, drb, dsig []byte
}

func (s *Store) writeEvents(ctx context.Context, st *txState, res ac.Result, so signedOut) error {
	rq := st.rq
	rb, rsig, entry, drb, dsig := so.rb, so.rsig, so.entry, so.drb, so.dsig
	b := bundleOf(rq.Sub)
	b.Result, b.ResultSig = rb, rsig
	if _, err := st.tx.ExecContext(ctx, `INSERT INTO authority.account_events (account_id, seq, restore_epoch, request_id,
		intent_digest, op, bundle, cp_evidence, entry_digest, prev_entry_digest, delegation_gen, delegation_serial,
		issued_at_ms, asserted_at_ms, imported) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, false)`,
		res.AccountID, i64(res.Seq), i64(st.restoreEpoch), res.RequestID, rq.IntentDigest, res.Op, b.Encode(),
		rq.Sub.CPEvidence, entry, nilIfEmpty(res.PrevEntryDigest), i64(res.DelegationGen), i64(res.DelegationSerial),
		i64(st.now), i64(rq.AZ.AssertedAtMs)); err != nil {
		return err
	}
	if drb == nil {
		return nil
	}
	db := ac.DeviceBundle{DeviceIntent: rq.Sub.DeviceIntent, SigActor: rq.Sub.SigActor, SigNewDevice: rq.Sub.SigNewDevice,
		DeviceResult: drb, DeviceResultSig: dsig} // cp_authz "": the published bundle carries it (DoR:509, B1-D9)
	_, err := st.tx.ExecContext(ctx, `INSERT INTO authority.device_events (account_id, dseq, request_id, device_intent_digest,
		op, chain_seq, chain_entry_digest, bundle, cp_evidence, entry_digest, prev_entry_digest, issued_at_ms,
		delegation_gen, delegation_serial, imported) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL, $9, $10, $11, $12, $13, false)`,
		res.AccountID, i64(st.h.Dseq+1), res.RequestID, rq.D, res.Op, i64(res.Seq), entry, db.Encode(),
		ac.Digest(drb), nilIfEmpty(st.h.DeviceHead), i64(st.now), i64(res.DelegationGen), i64(res.DelegationSerial))
	return err
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// applyCommit moves the head to the committed result (step 11's head update).
func applyCommit(st *txState, d policy.Decision, res ac.Result, entry, newSet, drb []byte) error {
	h, rq := st.h, st.rq
	from, prevRoot := h.published().FromCode(), h.Root
	pending := res.State == ac.StatePending
	h.Seq, h.Entry, h.IssuedAtMs, h.RG = res.Seq, entry, res.IssuedAtMs, res.RecoveryGen
	h.State, h.Prior = res.State, res.PriorState
	h.SuspOp, h.SuspUser = has(res.SuspendedBy, ac.ActorOperator), has(res.SuspendedBy, ac.ActorUser)
	h.Root, h.RootRevoked, h.EK = res.ActiveRoot, res.RootRevoked == 1, res.EKBindingDigest
	h.PendRoot, h.PendEK, h.PinRevoked = res.PendingRoot, res.PendingEKBindingDigest, res.LegacyPinRevoked == 1
	h.PendEKGen = 0
	if rq.EKB != nil && pending {
		h.PendEKGen = rq.EKB.EKGeneration
	} else if rq.EKB != nil && bytes.Equal(res.EKBindingDigest, rq.Intent.EKBindingDigest) {
		h.EKGen = rq.EKB.EKGeneration
	}
	h.PendErase, h.PendingRecover = pending && d.Row.N == 18, pending && d.Row.N == 20
	h.ActBase, h.ActG, h.ActUnavailBase, h.ActFloor, h.PendEpoch = nil, nil, nil, nil, nil
	if pending {
		armActivation(st, d)
	}
	cancelTimes(st, d, from)
	if d.Row.N == 1 { // enroll_new opens an initial segment
		h.Segment, h.SegmentRoot = ac.AssuranceInitial, res.ActiveRoot
	}
	if !bytes.Equal(prevRoot, h.Root) {
		h.ForceUnverified = false // a root change clears it (D-188); the trigger requires the root change
	}
	if res.State == ac.StateErased {
		h.ErasedAtMs = st.now // row 19
	}
	return advanceDevice(h, newSet, drb)
}

// armActivation starts the activation clock of a pending result.
func armActivation(st *txState, d policy.Decision) {
	h, rq := st.h, st.rq
	base, g, acc := rq.AZ.AssertedAtMs+st.sig.D.DeltaMs, st.sig.D.GMs, st.acc
	h.ActBase, h.ActG, h.ActUnavailBase = &base, &g, &acc
	if d.Row.N == 18 && has(rq.Ev.Factors, ac.FactorErasureList) && st.reconcileCompletedMs != 0 {
		f := st.reconcileCompletedMs + GPrimeMs // D-186
		h.ActFloor = &f
	}
	if d.Row.N == 2 || d.Row.N == 3 || d.Row.N == 11 || d.Row.N == 20 {
		h.PendEpoch = nilIfEmpty(rq.Ev.CredentialEpochDigest)
	}
}

// cancelTimes stamps or clears the cancel times. from is the head's published
// state before the commit.
func cancelTimes(st *txState, d policy.Decision, from ac.FromCode) {
	h := st.h
	switch {
	case d.Row.SetsLastCancel: // rows 13, 14
		h.LastCancelAtMs = st.now
		if from == ac.FromPe {
			h.LastEraseCancelAtMs = st.now
		}
	case d.Row.ClearsCancelTimes: // rows 15–17 (D-145, D-186)
		h.LastCancelAtMs, h.LastEraseCancelAtMs = 0, 0
	}
}

// advanceDevice moves the device chain head and the private set to the commit.
func advanceDevice(h *headRow, newSet, drb []byte) error {
	if drb != nil {
		h.Dseq++
		h.DeviceHead = ac.Digest(drb) // B1-D21
	}
	if newSet != nil {
		if err := h.setDeviceSet(newSet); err != nil {
			return err
		}
		if ds, err := ac.DecodeDeviceSet(newSet); err == nil && len(ds.ReplacementBindDigest) == 0 {
			h.clearSlotColumns() // recover (LD-34), rotate_root (LD-52) and the cancels empty the slot
		}
	}
	return nil
}
