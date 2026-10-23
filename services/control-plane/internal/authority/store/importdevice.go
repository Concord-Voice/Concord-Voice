package store

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// reconcile/import of private-op device entries and of row 22 (DoR:1727, the
// "Device bundles (LD-32)" and row-22 sentences; D-288, RT6-8; B3-D16, B3-D17).
//
// The device invariant. A device entry import writes is one an RSK of this
// realm signed (AuthenticateDeviceBundle, in the pre-pass, before anything is
// read), that extends the private head the authority holds (deviceLinks: the
// next dseq, the private head it names, and the published head its chain_seq
// names, which the replay has already rebuilt from authority rows), and that
// the commit core's own device path would have signed for the same request on
// the same head at the same db_now: the request rebuilt from the entry and its
// cp_evidence through decodeDeviceSubmit, the head normalized at the entry's
// issued_at, then admitPrivate (policy.AdmitPrivate: the state gate, the
// device-intent's head, floor H, BindTier and scope), the head-dependent
// private signatures, step 8a's stale-device-seq check and ApplyDeviceOp (DR1,
// DR2, DR4, DR9), bindKeys, and the device result byte-equal to
// deviceResultOf. Any other entry holds its account and stops that account's
// replay; it is never written. The head's dseq, device_head_digest,
// device_set and device_set_digest are re-derived by applyDevice, exactly as
// the commit core moves them.
//
// VerifyDeviceLedger is not called: it walks a ledger from a verified device
// head up to a signed status and device-status, and import holds neither
// without signing (B3-D4). Its per-entry checks are here instead, in the
// commit core's form: T7 and the entry's identity in the pre-pass, the dseq
// and hash link in deviceLinks, DR1–DR9 in the commit core's own steps, and the
// recomputed set digest inside the byte-equal device result.
//
// What breaks it: a write before every item has authenticated (D-287); a link
// read from anything but the head row this transaction locked; a decision not
// re-derived by admitPrivate (import would commit a bind that floor H or
// BindTier refuses, beside evidence that refuses it, RT6-8); or a device
// result compared on anything less than its bytes (the set digest is the only
// place the outcome of ApplyDeviceOp is signed).
//
// The row-22 invariant (D-288, DoR mutation 5). A restore-cancel is admitted
// by row 22's checks 1–3 over its OWN held_status, which ReplayAuthorityBundle
// has already run against the rebuilt from-head (checkRestoreCancel; check 4
// needs a retained fix, which the authority keeps none of). The head's held
// column is read nowhere on this path: a backup taken before the hold
// restores a head with held = false, and the same row-22 result must import
// on it. Its decision is restoreCancelDecision on the raw from-head (a held
// head never normalizes, and held_status names that head as it was), and the
// result must equal buildResult byte for byte; it is then written through
// applyRestoreCancel, clear-held's own writer, with cp_evidence NULL.
// Row 22 is only clear-held's first half, so the replay then holds the
// account (B3-D18): see replayRestoreCancel.
// decodeSubmit still refuses actor restore, so /v1/transactions does too.

// replayDevice replays one private-op item (bundle "") against the head: a
// non-empty why is a refusal (the caller holds the account), an error a
// database failure. An entry at or below the head's dseq is present when the
// stored entry at that dseq has its digest, and a fork otherwise.
func (s *Store) replayDevice(ctx context.Context, st *txState, it ac.ImportItem, im importBounds) (string, error) {
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil { // unreachable: AuthenticateDeviceBundle parsed these bytes
		return "", fmt.Errorf("authority store: reconcile-import: device bundle: %w", err)
	}
	dr, err := ac.DecodeDeviceResult(db.DeviceResult)
	if err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: device result: %w", err)
	}
	h := st.h
	if dr.Dseq <= h.Dseq {
		return presentOrFork(ctx, st, devicePresentQuery, dr.Dseq, ac.Digest(db.DeviceResult))
	}
	if why := im.refuses(dr.IssuedAtMs); why != "" {
		return why, nil
	}
	if why := deviceLinks(h, dr); why != "" {
		return why, nil
	}
	rq, why := rebuildDeviceRequest(db, it)
	if why != "" {
		return why, nil
	}
	sig, err := replayDelegation(ctx, st.tx, dr.DelegationGen, dr.DelegationSerial, st.accepted)
	if err != nil || sig == nil {
		return "the device result's delegation is not installed", err
	}
	st.rq, st.sig, st.now = rq, sig, dr.IssuedAtMs
	if out, done := s.deviceWindowExpiry(ctx, st); done { // steps 3 and 4 (6 is in the rebuild)
		return refusalOf(out)
	}
	if err := normalizeHead(ctx, st); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: normalize: %w", err)
	}
	if why, err := replayGates(ctx, st); why != "" || err != nil {
		return why, err
	}
	return s.decideImportedDevice(ctx, st, db)
}

// replayGates are the live path's two db_now gates that are not refusals there,
// run after step 7 as the live path runs them: D-271's recover guard
// (recoverGuardBusy, 503 busy live) and step 9's issued_at order
// (issuedNotBefore, 503 clock live). Both replay paths hold the account on
// either: the result claims a db_now at which its commit passed them.
func replayGates(ctx context.Context, st *txState) (string, error) {
	if recoverGuardBusy(st) {
		return "the recover guard was armed at the result's issued_at", nil
	}
	ok, err := issuedNotBefore(ctx, st)
	if err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: issued_at order: %w", err)
	}
	if !ok {
		return "issued before the account's newest result", nil
	}
	return "", nil
}

// decideImportedDevice is decideDevice for a retained entry: the same steps on
// the same head, then the signed device result compared with the one the
// commit core builds, in place of signing it.
func (s *Store) decideImportedDevice(ctx context.Context, st *txState, db ac.DeviceBundle) (string, error) {
	rq, h := st.rq, st.h
	ds, cur, err := h.set()
	if err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: device set: %w", err)
	}
	if r := admitPrivate(st); r != "" {
		return "policy: " + string(r), nil
	}
	if !verifyDeviceStateSigs(rq, h, ds) {
		return "a private signature does not verify", nil
	}
	next, out, done := s.deviceStep8a(ctx, st, ds, cur)
	if done {
		return refusalOf(out)
	}
	if out, done := s.bindKeys(ctx, st, deviceBindings(rq), h.Seq); done {
		return refusalOf(out)
	}
	drb := deviceResultOf(st, next)
	if drb == nil || !bytes.Equal(drb, db.DeviceResult) {
		return "the signed device result is not the commit core's", nil
	}
	if !slices.ContainsFunc(st.sig.D.RSKPublicKeys, func(k []byte) bool { return ac.VerifyP384(k, drb, db.DeviceResultSig) }) {
		return "the device result's signature does not verify", nil
	}
	if err := insertDeviceEvent(ctx, st, deviceBundleOf(rq.Dev, drb, db.DeviceResultSig), drb); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: device event: %w", err)
	}
	if err := applyDevice(st, cur, next, drb); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: device head: %w", err)
	}
	if err := saveHead(ctx, st.tx, h); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: head: %w", err)
	}
	return "", nil
}

// deviceLinks is the device chain's link (the walk's preEntry, on the
// authority's own head): the entry is the head's next dseq, names the head's
// private entry digest, and was built on the published head the replay holds
// (writeDevice's chain_seq and chain_entry_digest). Anything else is a broken
// device chain: a gap, a fork, or an entry for a published head the replay has
// not reached or has passed.
func deviceLinks(h *headRow, dr ac.DeviceResult) string {
	switch {
	case dr.Dseq != h.Dseq+1:
		return "a broken device chain: a dseq gap"
	case !bytes.Equal(dr.PrevDeviceEntryDigest, h.DeviceHead):
		return "a broken device chain: another private head"
	case dr.ChainSeq != h.Seq || !bytes.Equal(dr.ChainEntryDigest, h.Entry):
		return "a broken device chain: another published head"
	}
	return ""
}

// rebuildDeviceRequest is the device request the commit core decided, rebuilt
// from the entry and the item's cp_evidence as decodeDeviceSubmit decodes a
// device-submit. The device bundle the commit core would store for it must be
// the authenticated bytes; the cp-authz must bind the request and that
// evidence (cakBound: the evidence digest is in no signed result, so nothing
// else checks it); sig_new_device must verify (step 4); and the device-intent
// and B must be well formed for the op (step 6).
func rebuildDeviceRequest(db ac.DeviceBundle, it ac.ImportItem) (*request, string) {
	sub := ac.DeviceSubmit{DeviceIntent: db.DeviceIntent, SigRoot: db.SigRoot, SigActor: db.SigActor,
		SigNewDevice: db.SigNewDevice, RecoveryBind: db.RecoveryBind, SigRecovery: db.SigRecovery,
		SigRecoveryPrior: db.SigRecoveryPrior, CPAuthz: db.CPAuthz, CPAuthzSig: db.CPAuthzSig, CPEvidence: it.CPEvidence}
	rq, _, ok := decodeDeviceSubmit(sub.Encode())
	if !ok {
		return nil, "the item does not rebuild a device-submit"
	}
	if !bytes.Equal(deviceBundleOf(rq.Dev, db.DeviceResult, db.DeviceResultSig), it.DeviceBundle) {
		return nil, "the device bundle is not the one the commit core stores"
	}
	if !cakBound(rq) {
		return nil, "the cp-authz does not bind the request and its evidence"
	}
	if !verifyNewDevice(rq) {
		return nil, "a request signature does not verify"
	}
	if deviceMalformedForOp(rq) {
		return nil, "the device-intent is malformed for its op"
	}
	return rq, ""
}

// replayRestoreCancel is row 22, replayed (see the row-22 invariant above). b
// has passed ReplayAuthorityBundle against the rebuilt from-head, row 22's
// checks 1–3 included; nothing here reads held.
func (s *Store) replayRestoreCancel(ctx context.Context, st *txState, b ac.Bundle, res ac.Result, it ac.ImportItem) (string, error) {
	if len(it.CPEvidence) != 0 { // unreachable: CheckImportItem refuses the batch first (E3-02)
		return "a restore-cancel carries cp_evidence", nil
	}
	rq, why := rebuildRestoreCancel(b, it)
	if why != "" {
		return why, nil
	}
	sig, err := replayDelegation(ctx, st.tx, res.DelegationGen, res.DelegationSerial, st.accepted)
	if err != nil || sig == nil {
		return "the result's delegation is not installed", err
	}
	st.rq, st.sig, st.now = rq, sig, res.IssuedAtMs
	st.preNorm, st.norm = st.h.published(), normOut{} // a held head never normalizes (step 7)
	d := restoreCancelDecision(st, ac.Bundle{Intent: b.Intent, CPAuthz: b.CPAuthz, HeldStatus: b.HeldStatus})
	if d.Refusal != "" || d.Row.N != 22 {
		return "policy: row 22 not admitted: " + string(d.Refusal), nil
	}
	built, ok := s.buildResult(st, d)
	if !ok || ac.CheckPost(d.Row, st.h.published(), rq.Intent, built, false, ac.Assurance(st.h.Segment),
		ac.Assurance(st.h.RecoveryBindAssurance)) != ac.ReasonOK {
		return "the post-state is not row 22's", nil
	}
	if rb := built.Encode(); rb == nil || !bytes.Equal(rb, b.Result) {
		return "the signed result is not the commit core's", nil
	}
	if err := applyRestoreCancel(ctx, st, d, built, it.Bundle); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: row 22: %w", err)
	}
	// Row 22 is only the first half of clear-held's P/D/Pr exit (DoR:1734,
	// D-146, LD-46). The second half, held=false with force_unverified and the
	// G′ floors, is a head write that no event carries and that only
	// authority_owner may make (account_heads_guard), so a re-imported row 22
	// lands without it; and the CP's retained head is now the imported one, so
	// restore step 7 posts no /hold for the account. Hold it here (B3-D18):
	// the owner's clear-held then re-runs the L1 half (the head row 22 leaves
	// is never P, D or Pr, so it classifies as unverified and signs nothing).
	st.h.Held = true
	return "", holdHead(ctx, st.tx, st.h.AccountID)
}

// rebuildRestoreCancel is the request restoreCancelTx built: the intent and the
// unsigned cp-authz, with no submit to decode (decodeSubmit refuses actor
// restore, and keeps refusing it for /v1/transactions). The bundle must be
// exactly the one restoreCancelTx stores (intent, cp-authz, held_status,
// result, signature, nothing else), and the cp-authz must name the intent.
func rebuildRestoreCancel(b ac.Bundle, it ac.ImportItem) (*request, string) {
	stored := ac.Bundle{Intent: b.Intent, CPAuthz: b.CPAuthz, HeldStatus: b.HeldStatus, Result: b.Result, ResultSig: b.ResultSig}
	if !bytes.Equal(stored.Encode(), it.Bundle) {
		return nil, "the bundle is not the one clear-held stores"
	}
	in, err := ac.DecodeIntent(b.Intent)
	if err != nil {
		return nil, "the intent does not decode"
	}
	az, err := ac.DecodeCPAuthz(b.CPAuthz)
	if err != nil || az.Actor != ac.ActorRestore {
		return nil, "the cp-authz is not a restore's"
	}
	if az.RealmID != in.RealmID || az.AccountID != in.AccountID || az.RequestID != in.RequestID ||
		!bytes.Equal(az.IntentDigest, ac.Digest(b.Intent)) || in.Op != ac.OpCancelPending {
		return nil, "the cp-authz does not bind a cancel_pending intent"
	}
	return &request{Intent: in, IntentDigest: ac.Digest(b.Intent), AZ: az, Sub: ac.Submit{Intent: b.Intent, CPAuthz: b.CPAuthz}}, ""
}
