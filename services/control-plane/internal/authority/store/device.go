package store

import (
	"bytes"
	"context"
	"database/sql"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// decodeDeviceSubmit decodes a device-submit (A2's pattern check) into the
// shared request shape. The identity comes from the device-intent, or from B
// on the sheet form.
func decodeDeviceSubmit(body []byte) (*request, Outcome, bool) {
	ds, err := ac.CheckDeviceSubmit(body)
	if err != nil {
		return malformed()
	}
	rq := &request{Dev: &ds, Sub: ac.Submit{CPAuthz: ds.CPAuthz, CPAuthzSig: ds.CPAuthzSig, CPEvidence: ds.CPEvidence}}
	if rq.AZ, err = ac.DecodeCPAuthz(ds.CPAuthz); err != nil {
		return malformed()
	}
	if rq.Ev, err = ac.DecodeCPEvidence(ds.CPEvidence); err != nil {
		return malformed()
	}
	if rq.AZ.Actor != ac.ActorUser { // B1-D22
		return nil, errorOutcome(400, ac.ErrorBadAuthz), false
	}
	if len(ds.RecoveryBind) > 0 {
		b, err := ac.DecodeRecoveryBind(ds.RecoveryBind)
		if err != nil {
			return malformed()
		}
		rq.Bind, rq.H = &b, ac.Digest(ds.RecoveryBind)
	}
	if len(ds.DeviceIntent) > 0 {
		di, err := ac.DecodeDeviceIntent(ds.DeviceIntent)
		if err != nil {
			return malformed()
		}
		rq.DI, rq.D = &di, ac.Digest(ds.DeviceIntent)
		rq.Intent = ac.Intent{RealmID: di.RealmID, AccountID: di.AccountID, RequestID: di.RequestID, Op: di.Op, ExpiresAtMs: di.ExpiresAtMs}
		rq.IntentDigest = rq.D
	} else { // the sheet form of cancel_recovery_bind
		rq.Intent = ac.Intent{RealmID: rq.Bind.RealmID, AccountID: rq.Bind.AccountID, RequestID: rq.Bind.RequestID,
			Op: ac.OpCancelRecoveryBind, ExpiresAtMs: rq.Bind.ExpiresAtMs}
		rq.IntentDigest = rq.H
	}
	// B names the device-intent's realm and account, and the device-intent
	// names B: #18 = H(B) (DoR:420). A2's CheckDeviceSubmit already pins
	// B.request_id to #5 for bind, unbind and the root-form cancel, and B #7
	// to #8 on the root-form cancel (submitPatternHolds, E3-10).
	if rq.Bind != nil && (rq.Bind.RealmID != rq.Intent.RealmID || rq.Bind.AccountID != rq.Intent.AccountID ||
		(rq.DI != nil && !bytes.Equal(rq.DI.RecoveryBindDigest, rq.H))) {
		return malformed()
	}
	rq.LedgerDigest = rq.IntentDigest
	if rq.Intent.Op == ac.OpCancelRecoveryBind {
		rq.LedgerDigest = rq.H
	}
	rq.Class = classOfOp(rq.Intent.Op)
	return rq, Outcome{}, true // no signature here: admitAndDecode may run this without a slot
}

// verifyNewDevice is step 4 on the device path: sig_new_device over D under
// the new dsk (DoR:1455), run under the slot (review round 2). The device
// path's other signatures depend on the head (verifyDeviceStateSigs, step 8a).
func verifyNewDevice(rq *request) bool {
	return rq.DI == nil || len(rq.DI.NewDeviceDSK) == 0 ||
		ac.VerifyP384(rq.DI.NewDeviceDSK, ac.Sig{Role: ac.RoleNewDevice, Digest: rq.D}.Encode(), rq.Dev.SigNewDevice)
}

// SubmitDevice is POST /v1/device-transactions (§3.2.4, LD-32, LD-34, LD-43).
func (s *Store) SubmitDevice(ctx context.Context, body []byte) Outcome {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.txDL)
	defer cancel()
	out, op, rid := s.submitDevice(ctx, body)
	s.log.Info("authority.device", "op", op, "outcome", int(out.Kind), "code", out.Code, "request_id", rid,
		"latency_ms", time.Since(start).Milliseconds())
	return out
}

func (s *Store) submitDevice(ctx context.Context, body []byte) (Outcome, string, string) {
	if len(body) > BodyCapDevice {
		return errorOutcome(413, ac.ErrorTooLarge), "", ""
	}
	rq, rel, out, ok := s.admitAndDecode(body, decodeDeviceSubmit) // steps 1–2 (Task 11)
	if !ok {
		return out, "", ""
	}
	defer rel()
	op, rid := rq.Intent.Op, rq.Intent.RequestID
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return s.storeError(), op, rid
	}
	defer func() { _ = conn.Close() }()
	if out, ok := s.leaderCheck(ctx, conn); !ok {
		return out, op, rid
	}
	if out, proceed := s.preBegin(ctx, conn, rq); !proceed {
		return s.release(ctx, conn, out), op, rid
	}
	return s.release(ctx, conn, s.commitDevice(ctx, conn, rq)), op, rid
}

func (s *Store) commitDevice(ctx context.Context, conn *sql.Conn, rq *request) Outcome {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return s.storeError()
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return s.storeError()
	}
	st := &txState{tx: tx, rq: rq}
	if out, done := s.stepsL1ToReplay(ctx, st); done {
		return out
	}
	if out, done := s.deviceWindowExpiryHeld(ctx, st); done {
		return out
	}
	if out, done := s.stepNormalize(ctx, st); done {
		return out
	}
	return s.decideDevice(ctx, st)
}

// deviceWindowExpiryHeld is steps 3–6 for a private op: a cancel's window and
// expiry for both cancel forms (B #7 too on the root form), a non-cancel's for
// add_device and bind_recovery (#8), held, then malformed_for_op.
func (s *Store) deviceWindowExpiryHeld(ctx context.Context, st *txState) (Outcome, bool) {
	rq := st.rq
	var out Outcome
	var done bool
	if rq.Intent.Op == ac.OpCancelRecoveryBind {
		out, done = s.cancelWindowExpiry(ctx, st)
	} else {
		out, done = s.nonCancelWindowExpiry(ctx, st)
	}
	if done {
		return out, true
	}
	if st.h.Held {
		return s.record(ctx, st, ac.RefusalHeld), true
	}
	if rq.DI != nil && ac.DeviceIntentForOp(*rq.DI, nil) != "" {
		return s.record(ctx, st, ac.RefusalMalformedForOp), true
	}
	if rq.Bind != nil && ac.RecoveryBindForKind(*rq.Bind) != "" {
		return s.record(ctx, st, ac.RefusalMalformedForOp), true
	}
	return Outcome{}, false
}

// cancelWindowExpiry is a cancel's window and expiry, for both cancel forms
// (B #7 too on the root form).
func (s *Store) cancelWindowExpiry(ctx context.Context, st *txState) (Outcome, bool) {
	rq, now, a := st.rq, st.now, st.rq.AZ.AssertedAtMs
	if a > now+ac.EpsilonMs {
		s.nonClient503.Store(true)
		return errorOutcome(503, ac.ErrorClock), true
	}
	if a <= sat(now, st.sig.D.GMs+ac.CancelSlackMs) {
		return s.record(ctx, st, ac.RefusalAuthzOutOfWindow), true
	}
	if rq.Intent.ExpiresAtMs <= a || (rq.Bind != nil && rq.Bind.ExpiresAtMs <= a) {
		return s.record(ctx, st, ac.RefusalExpired), true
	}
	if hi := a + ac.IntentWindowMs; rq.Intent.ExpiresAtMs > hi || (rq.Bind != nil && rq.Bind.ExpiresAtMs > hi) { // DoR:1190, D-267 (red-team F1)
		return s.record(ctx, st, ac.RefusalExpiryOutOfRange), true
	}
	return Outcome{}, false
}

// nonCancelWindowExpiry is a non-cancel's window and expiry (#8).
func (s *Store) nonCancelWindowExpiry(ctx context.Context, st *txState) (Outcome, bool) {
	rq, now, a := st.rq, st.now, st.rq.AZ.AssertedAtMs
	if a <= sat(now, ac.IntentWindowMs) || a > now+ac.EpsilonMs {
		return s.record(ctx, st, ac.RefusalAuthzOutOfWindow), true
	}
	if code := expiry(&request{Intent: rq.Intent, AZ: rq.AZ}, now, st.restoreStartedMs); code != "" {
		return s.record(ctx, st, code), true
	}
	return Outcome{}, false
}

func (s *Store) decideDevice(ctx context.Context, st *txState) Outcome {
	rq, h := st.rq, st.h
	if rq.Class != classCancel && h.RecoverGuardUntilMs != nil && *h.RecoverGuardUntilMs > st.now { // D-271
		return errorOutcome(503, ac.ErrorBusy)
	}
	ds, cur, err := h.set()
	if err != nil {
		return s.storeError()
	}
	if r := policy.AdmitPrivate(policy.PrivateInput{Head: h.policyHead(st.inPop), Op: rq.Intent.Op, DeviceIntent: rq.DI,
		Bind: rq.Bind, PriorSig: len(rq.Dev.SigRecoveryPrior) > 0, RootForm: rq.DI != nil && rq.Intent.Op == ac.OpCancelRecoveryBind,
		SheetForm: rq.DI == nil, Evidence: ac.EvidenceOf(rq.Ev), Scope: st.sig.D.Scope}); r != "" {
		return s.record(ctx, st, r)
	}
	if !verifyDeviceStateSigs(rq, h, ds) {
		return errorOutcome(400, ac.ErrorBadSignature)
	}
	next, out, done := s.deviceStep8a(ctx, st, ds, cur)
	if done {
		return out
	}
	if out, done := s.step9(ctx, st); done {
		return out
	}
	if out, done := s.bindKeys(ctx, st, deviceBindings(rq), h.Seq); done {
		return out
	}
	return s.writeDevice(ctx, st, cur, next)
}

// verifyDeviceStateSigs: sig_root under the effective active root (over D on
// add_device, over H(B) on bind_recovery and the root-form cancel);
// sig_recovery over H(B) under B.recovery_key on a bind; sig_recovery_prior,
// and the sheet form's sig_recovery, under the bound key.
func verifyDeviceStateSigs(rq *request, h *headRow, ds ac.DeviceSet) bool {
	d := rq.Dev
	sig := func(key []byte, role string, digest, s []byte) bool {
		return len(key) > 0 && ac.VerifyP384(key, ac.Sig{Role: role, Digest: digest}.Encode(), s)
	}
	switch {
	case rq.Intent.Op == ac.OpAddDevice:
		return sig(h.Root, ac.RoleCurrentRoot, rq.D, d.SigRoot)
	case rq.DI == nil: // sheet form
		return sig(ds.RecoveryKey, ac.RoleRecoveryKey, rq.H, d.SigRecovery)
	}
	if !sig(h.Root, ac.RoleCurrentRoot, rq.H, d.SigRoot) {
		return false
	}
	if rq.Bind.Kind == ac.BindKindBind && !sig(rq.Bind.RecoveryKey, ac.RoleRecoveryKey, rq.H, d.SigRecovery) {
		return false
	}
	if len(d.SigRecoveryPrior) > 0 && !sig(ds.RecoveryKey, ac.RoleRecoveryKey, rq.H, d.SigRecoveryPrior) {
		return false
	}
	return true
}

func (s *Store) deviceStep8a(ctx context.Context, st *txState, ds ac.DeviceSet, cur []byte) ([]byte, Outcome, bool) {
	rq, h := st.rq, st.h
	var actor, wait string
	var nd *ac.DeviceEntry
	if di := rq.DI; di != nil {
		actor, wait, nd = di.ActorDeviceID, di.BindWait, newEntry(di)
		if di.ExpectedDseq != h.Dseq || !bytes.Equal(di.ExpectedDeviceHead, h.DeviceHead) { // (1)
			return nil, s.record(ctx, st, ac.RefusalStaleDeviceSeq), true
		}
	}
	// ApplyDeviceOp is pure and runs DR1 before DR4, so its device_unknown is
	// (2) for every half of DR1, recorded before (3) sig_actor (DoR:1449).
	next, r := ac.ApplyDeviceOp(cur, ac.DeviceOpInput{Op: rq.Intent.Op, Actor: actor, New: nd, Bind: rq.Dev.RecoveryBind,
		BindWait: wait, PriorSig: len(rq.Dev.SigRecoveryPrior) > 0, RootForm: rq.DI != nil && rq.Intent.Op == ac.OpCancelRecoveryBind,
		Head: h.headInput()}, st.now, st.sig.D.DeltaMs) // (4) DR4, (5) DR2, DR9
	if r == ac.RefusalDeviceUnknown {
		return nil, s.record(ctx, st, r), true
	}
	if rq.DI != nil {
		a := findActive(ds, actor) // (2)
		if a == nil {
			return nil, s.record(ctx, st, ac.RefusalDeviceUnknown), true
		}
		if !ac.VerifyP384(a.DSK[:], ac.Sig{Role: ac.RoleActorDevice, Digest: rq.D}.Encode(), rq.Dev.SigActor) { // (3)
			return nil, errorOutcome(400, ac.ErrorBadSignature), true
		}
	}
	if r != "" {
		return nil, s.record(ctx, st, r), true
	}
	return next, Outcome{}, false
}

// deviceBindings: add_device binds dsk and dkx; a bind B binds its recovery
// key (role recovery, LD-34); an unbind and both cancels bind nothing.
func deviceBindings(rq *request) []keyBinding {
	var out []keyBinding
	if rq.Intent.Op == ac.OpAddDevice {
		out = append(out, keyBinding{ac.KeyDigest(rq.DI.NewDeviceDSK), "dsk"}, keyBinding{ac.KeyDigest(rq.DI.NewDeviceDKX), "dkx"})
	}
	if rq.Intent.Op == ac.OpBindRecovery && rq.Bind.Kind == ac.BindKindBind {
		out = append(out, keyBinding{ac.KeyDigest(rq.Bind.RecoveryKey), "recovery"})
	}
	sortBindings(out)
	return out
}

func (s *Store) writeDevice(ctx context.Context, st *txState, cur, next []byte) Outcome {
	rq, h := st.rq, st.h
	sd, err := ac.DeviceSetDigest(next)
	if err != nil {
		return s.storeError()
	}
	named := rq.D // device-result #6: D, or H(B) on the sheet form, which has no device-intent (DoR:445)
	if rq.DI == nil {
		named = rq.H
	}
	drb := ac.DeviceResult{RealmID: rq.Intent.RealmID, AccountID: rq.Intent.AccountID, RequestID: rq.Intent.RequestID,
		DeviceIntentDigest: named, CPAuthzDigest: ac.Digest(rq.Dev.CPAuthz), Op: rq.Intent.Op, Dseq: h.Dseq + 1,
		PrevDeviceEntryDigest: h.DeviceHead, ChainSeq: h.Seq, ChainEntryDigest: h.Entry, DeviceSetDigest: sd,
		IssuedAtMs: st.now, DelegationGen: st.sig.Gen, DelegationSerial: st.sig.Serial}.Encode()
	if drb == nil {
		return s.storeError()
	}
	dsig, err := signer.SignAndVerify(s.sig, drb)
	if err != nil {
		return s.signerError()
	}
	d := rq.Dev
	bundle := ac.DeviceBundle{DeviceIntent: d.DeviceIntent, SigRoot: d.SigRoot, SigActor: d.SigActor, SigNewDevice: d.SigNewDevice,
		RecoveryBind: d.RecoveryBind, SigRecovery: d.SigRecovery, SigRecoveryPrior: d.SigRecoveryPrior, CPAuthz: d.CPAuthz,
		CPAuthzSig: d.CPAuthzSig, DeviceResult: drb, DeviceResultSig: dsig}.Encode()
	if _, err := st.tx.ExecContext(ctx, `INSERT INTO authority.device_events (account_id, dseq, request_id, device_intent_digest,
		op, chain_seq, chain_entry_digest, bundle, cp_evidence, entry_digest, prev_entry_digest, issued_at_ms, delegation_gen,
		delegation_serial, imported) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, false)`,
		h.AccountID, i64(h.Dseq+1), rq.Intent.RequestID, rq.LedgerDigest, rq.Intent.Op, i64(h.Seq), nilIfEmpty(h.Entry),
		bundle, d.CPEvidence, ac.Digest(drb), nilIfEmpty(h.DeviceHead), i64(st.now), i64(st.sig.Gen), i64(st.sig.Serial)); err != nil {
		return s.storeError()
	}
	if err := applyDevice(st, cur, next, drb); err != nil {
		return s.storeError()
	}
	if err := saveHead(ctx, st.tx, h); err != nil {
		return s.storeError()
	}
	if err := st.tx.Commit(); err != nil {
		return s.storeError()
	}
	return Outcome{HTTP: 200, Kind: KindCommitted, Body: ac.DeviceCommitted{DeviceBundle: bundle}.Encode()}
}

// applyDevice writes the private head and the recovery columns (D-273, D-274).
func applyDevice(st *txState, cur, next, drb []byte) error {
	h := st.h
	h.Dseq++
	h.DeviceHead = ac.Digest(drb)
	if err := h.setDeviceSet(next); err != nil {
		return err
	}
	old, err := ac.DecodeDeviceSet(cur)
	if err != nil {
		return err
	}
	nw, err := ac.DecodeDeviceSet(next)
	if err != nil {
		return err
	}
	if !bytes.Equal(nw.RecoveryBindDigest, old.RecoveryBindDigest) {
		h.RecoveryBindAssurance = ""
		if len(nw.RecoveryKey) > 0 {
			h.RecoveryBindAssurance = h.Segment
		}
	}
	switch {
	case len(nw.ReplacementBindDigest) == 0:
		h.clearSlotColumns()
	case !bytes.Equal(nw.ReplacementBindDigest, old.ReplacementBindDigest): // the slot filled
		acc, g := st.acc, st.sig.D.GMs
		h.ReplUnavailBase, h.ReplG, h.ReplFloor = &acc, &g, nil
		h.ReplacementBindAssurance = ""
		if len(nw.ReplacementKey) > 0 {
			h.ReplacementBindAssurance = h.Segment
		}
	}
	return nil
}
