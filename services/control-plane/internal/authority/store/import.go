package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// reconcile/import (DoR:1611, :1727-1730, :1765-1769; D-96, D-287; B3-D4,
// B3-D13, B3-D14, B3-D15 R3, B3-D16).
//
// The invariant. An event import writes is a result an RSK of this realm signed
// (authenticated before anything is written), that links to the head the
// authority holds (ReplayAuthorityBundle against a head rebuilt from the
// authority's own stored history, chainHeadOf), that the commit core itself
// would have signed for the same request against the same head at the same
// db_now (the request rebuilt from the item, policy.Admit through admit,
// CheckPost, and the signed result byte-equal to the commit core's own
// buildResult), whose keys bind (bindKeys), and that was issued no later than
// min(restore_started_at, db_now + ε) and no earlier than the D-96 margin
// before the backup's newest result. Any other item holds its account and
// stops that account's replay; it never writes the item. Only an account an
// authenticated item names is ever held (B3-D16: an item that does not
// authenticate makes the whole batch bad_authz before any write). Import
// never signs (B3-D4), never produces the erased state (DoR:1727), and never
// arms an activation_floor_ms: reconcile-complete floors every pending head. A
// floor the restored head already carries is carried across each replayed
// commit as the original commit carried it (carryFloor, at the result's
// issued_at), so the replay neither loses a backup floor nor invents one.
//
// What breaks it: a write before the whole batch is authenticated (a CP could
// then hold an account it holds no signed evidence for, D-287); a prev taken
// from anything but the authority's own rows; a decision not re-derived by
// the commit core's policy (import would commit whatever an RSK once signed,
// beside evidence the policy refuses); continuing an account past a refused
// item (a later item could link past the hole a refused fork leaves); or
// reading reconciled before L1, or without a lock: a reconcile-complete that
// commits between that read and the import's commit would leave an imported
// pending head with no G′ floor.
//
// Transaction shape (B3-D13). One transaction per account, in ascending
// account_id: L1 (lockHead), then meta FOR SHARE (the heads, then meta: the
// one lock order, operatorRun), then the fold, then each item under SAVEPOINT
// import_item. A refused item rolls back to its savepoint, sets held on the
// head, and the transaction commits with that account's earlier items; the
// next account opens its own transaction. meta FOR SHARE is what closes the
// reconcile-complete race: reconcile-complete takes meta FOR UPDATE before it
// floors the pending heads, so either it waits for this commit and then sees
// (and floors) what it wrote, or this transaction waits for it and then reads
// reconciled = true. A database error rolls back the current account only;
// earlier accounts stay committed and still pass the release gate before the
// error returns (releaseCommitted), and the CP's retry converges, because an
// item already present is skipped.

// ReconcileImport is POST /v1/reconcile/import. Its prologue is operatorRun's.
// It answers 200 with no body (B3-D14) whether or not an account ended held:
// the CP reads the outcome through /v1/status. A body that is not an
// import-batch in order, or any item that does not authenticate, is
// ErrBadAuthz with nothing written; a reconciled realm is ErrReconciled.
func (s *Store) ReconcileImport(ctx context.Context, body []byte) (Outcome, error) {
	err := operatorRun(ctx, s, "reconcile-import", body, decodeImportBatch,
		func(ctx context.Context, conn *sql.Conn, items []ac.ImportItem) error {
			return s.importBatch(ctx, conn, items)
		})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{HTTP: 200, Kind: KindOK}, nil
}

// decodeImportBatch is the import-batch and its order (ac.CheckImportBatch):
// 1..64 items, each a well-formed import-item, in strictly ascending
// (account_id, chain_seq, dseq). A body that fails is ErrBadAuthz.
func decodeImportBatch(body []byte) ([]ac.ImportItem, error) {
	ib, err := ac.CheckImportBatch(body)
	if err != nil {
		return nil, fmt.Errorf("%w: the body is not an import-batch in order", ErrBadAuthz)
	}
	items := make([]ac.ImportItem, 0, len(ib.Items))
	for _, raw := range ib.Items {
		it, err := ac.DecodeImportItem(raw)
		if err != nil { // CheckImportBatch decoded it already
			return nil, fmt.Errorf("%w: an import-item does not decode", ErrBadAuthz)
		}
		items = append(items, it)
	}
	return items, nil
}

// importGroup is one account's items, in batch order.
type importGroup struct {
	account string
	items   []ac.ImportItem
}

// authenticateItems is B3-D16's pre-pass: every item's bundle passes
// ac.AuthenticateBundle under the store's trust before any head is read,
// locked or written. A superseded result is admitted here (the replay admits
// it too: B3-D15 R3), but its delegation must be one the trust holds, and the
// trust is built from authority.delegations alone, so an uninstalled
// delegation is delegation_unknown and refuses the batch. A private-op item
// (no bundle) passes ac.AuthenticateDeviceBundle on its device entry instead
// (B3-D17); a published item's attached entry is judged beside its result in
// the replay (attachedEntry). The groups take the account from the
// authenticated result, in the batch's (ascending) order.
func authenticateItems(trust *ac.Trust, hw ac.HighWater, items []ac.ImportItem) ([]importGroup, error) {
	var groups []importGroup
	for _, it := range items {
		account, r := authenticateItem(trust, hw, it)
		if r != ac.ReasonOK {
			return nil, fmt.Errorf("%w: an import item does not authenticate: %s", ErrBadAuthz, r)
		}
		if n := len(groups); n > 0 && groups[n-1].account == account {
			groups[n-1].items = append(groups[n-1].items, it)
			continue
		}
		groups = append(groups, importGroup{account: account, items: []ac.ImportItem{it}})
	}
	return groups, nil
}

// authenticateItem is one item's authentication and the account its signed
// result names: the published bundle's, or a private-op item's device
// result's.
func authenticateItem(trust *ac.Trust, hw ac.HighWater, it ac.ImportItem) (string, ac.Reason) {
	if len(it.Bundle) == 0 {
		dr, _, r := ac.AuthenticateDeviceBundle(trust, hw, it.DeviceBundle)
		return dr.AccountID, r
	}
	res, _, r := ac.AuthenticateBundle(trust, hw, it.Bundle)
	return res.AccountID, r
}

// importer is one request's trust, read once: every item is authenticated,
// folded and replayed under the same Trust.
type importer struct {
	s        *Store
	trust    *ac.Trust
	hw       ac.HighWater
	accepted map[string]bool
}

func (s *Store) importBatch(ctx context.Context, conn *sql.Conn, items []ac.ImportItem) error {
	trust, accepted, err := s.trustFor(ctx, conn)
	if err != nil {
		return fmt.Errorf("authority store: reconcile-import: trust: %w", err)
	}
	hw := trust.HighWater()
	groups, err := authenticateItems(trust, hw, items)
	if err != nil {
		return err
	}
	imp := importer{s: s, trust: trust, hw: hw, accepted: accepted}
	committed := false
	for _, g := range groups {
		if err := operatorTx(ctx, conn, "reconcile-import", func(tx *sql.Tx) error {
			return imp.account(ctx, tx, g)
		}); err != nil {
			return s.releaseCommitted(ctx, conn, committed, err)
		}
		committed = true
	}
	return nil
}

// releaseCommitted is importBatch's way out when an account fails after an
// earlier one committed. Those commits pass step 12's release before err is
// returned, since operatorRun releases only on success: otherwise they would
// skip the replication gate. A release that does not confirm is 503
// replication in place of err, and the CP's retry converges (present items
// skip). With nothing committed, err is returned as it is.
func (s *Store) releaseCommitted(ctx context.Context, conn *sql.Conn, committed bool, err error) error {
	if !committed {
		return err
	}
	if out := s.release(ctx, conn, Outcome{HTTP: 200, Kind: KindOK}); out.Kind == KindError {
		return OutcomeError{out}
	}
	return err
}

// importBounds is §3.4's two bounds on an item the authority does not hold,
// read from meta under the account's transaction. recorded is false when
// restore-begin never recorded a restore point (restore_started_at or
// backup_max_issued_at_ms NULL).
type importBounds struct {
	RestoreStartedMs, BackupMaxIssuedMs, NowMs uint64
	recorded                                   bool
}

// refuses: issued_at more than RestoreMarginMs before the backup's newest
// result (D-96), or later than min(restore_started_at, db_now + ε), or no
// restore point to measure either from.
func (im importBounds) refuses(issued uint64) string {
	switch {
	case !im.recorded:
		return "no restore point is recorded"
	case issued+RestoreMarginMs < im.BackupMaxIssuedMs:
		return "issued before the D-96 margin"
	case issued > min(im.RestoreStartedMs, im.NowMs+ac.EpsilonMs):
		return "issued after the restore began"
	}
	return ""
}

// account imports one account's items in one transaction (see the shape at
// the top of this file).
func (imp importer) account(ctx context.Context, tx *sql.Tx, g importGroup) error {
	h, inPop, err := lockHead(ctx, tx, g.account) // L1
	if err != nil {
		return fmt.Errorf("authority store: reconcile-import: lock: %w", err)
	}
	st := &txState{tx: tx, h: h, inPop: inPop, trust: imp.trust, accepted: imp.accepted, replay: true}
	im, err := imp.readMeta(ctx, st)
	if err != nil {
		return err
	}
	if st.reconciled {
		return ErrReconciled
	}
	prev, err := chainHeadOf(ctx, tx, imp.trust, imp.hw, h)
	if errors.Is(err, errHistoryUnproven) { // a purged erased account, among others: a hold, never a 503
		return holdHead(ctx, tx, h.AccountID)
	}
	if err != nil {
		return fmt.Errorf("authority store: reconcile-import: fold: %w", err)
	}
	for _, it := range g.items {
		next, held, err := imp.item(ctx, st, prev, it, im)
		if err != nil || held { // a refusal skips this account's remaining items
			return err
		}
		prev = next
	}
	return nil
}

// readMeta reads, after L1 and under meta FOR SHARE, what the replay needs:
// reconciled, the restore epoch the imported events carry, the accumulator a
// pending result's activation starts from (as the commit core reads it), the
// restore point, and db_now. st.reconcileCompletedMs stays 0, so armActivation
// sets no floor (import never erases; reconcile-complete floors).
func (imp importer) readMeta(ctx context.Context, st *txState) (importBounds, error) {
	var started sql.NullTime
	var backupMax sql.NullInt64
	var epoch, acc int64
	if err := st.tx.QueryRowContext(ctx, `SELECT reconciled, restore_epoch, cancel_unavailable_ms, restore_started_at,
		backup_max_issued_at_ms FROM authority.meta WHERE id = 1 FOR SHARE`).
		Scan(&st.reconciled, &epoch, &acc, &started, &backupMax); err != nil {
		return importBounds{}, fmt.Errorf("authority store: reconcile-import: meta: %w", err)
	}
	now, err := imp.s.dbNow(ctx, st.tx)
	if err != nil {
		return importBounds{}, fmt.Errorf("authority store: reconcile-import: db_now: %w", err)
	}
	st.restoreEpoch, st.acc = u64(epoch), u64(acc)
	return importBounds{RestoreStartedMs: msOfNull(started), BackupMaxIssuedMs: u64(backupMax.Int64), NowMs: now,
		recorded: started.Valid && backupMax.Valid}, nil
}

// holdHead sets held on the account's head, inside the account's transaction.
func holdHead(ctx context.Context, tx *sql.Tx, account string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE authority.account_heads SET held = true WHERE account_id = $1`,
		account); err != nil {
		return fmt.Errorf("authority store: reconcile-import: hold: %w", err)
	}
	return nil
}

// item replays one item under its own savepoint. On a refusal it rolls the
// item back, holds the account and reports held; prev is returned unchanged.
func (imp importer) item(ctx context.Context, st *txState, prev ac.ChainHead, it ac.ImportItem,
	im importBounds) (ac.ChainHead, bool, error) {
	if _, err := st.tx.ExecContext(ctx, `SAVEPOINT import_item`); err != nil {
		return prev, false, fmt.Errorf("authority store: reconcile-import: savepoint: %w", err)
	}
	next, why, err := imp.replayItem(ctx, st, prev, it, im)
	if err != nil {
		return prev, false, err
	}
	if why == "" {
		if _, err := st.tx.ExecContext(ctx, `RELEASE SAVEPOINT import_item`); err != nil {
			return prev, false, fmt.Errorf("authority store: reconcile-import: release: %w", err)
		}
		return next, false, nil
	}
	if _, err := st.tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT import_item`); err != nil {
		return prev, false, fmt.Errorf("authority store: reconcile-import: rollback: %w", err)
	}
	return prev, true, holdHead(ctx, st.tx, st.h.AccountID)
}

// replayItem is one item against prev: a non-empty why is a refusal (the
// caller holds the account), an error a database failure. A private-op item
// (bundle "") is replayDevice's (importdevice.go) and leaves prev, the
// published chain, where it is. A published item:
//
//  1. Present: a result at or below the head's seq is skipped when the stored
//     event at that seq has its entry digest, and is a fork otherwise.
//  2. The bounds (importBounds.refuses).
//  3. ac.ReplayAuthorityBundle against prev with the head row's pin, a
//     superseded entry admitted (B3-D15 R3).
//  4. The commit core's decision, replayed (applyImported).
func (imp importer) replayItem(ctx context.Context, st *txState, prev ac.ChainHead, it ac.ImportItem,
	im importBounds) (ac.ChainHead, string, error) {
	if len(it.Bundle) == 0 {
		why, err := imp.s.replayDevice(ctx, st, it, im)
		return prev, why, err
	}
	b, err := ac.DecodeBundle(it.Bundle)
	if err != nil { // unreachable: AuthenticateBundle parsed these bytes
		return prev, "", fmt.Errorf("authority store: reconcile-import: bundle: %w", err)
	}
	res, err := ac.DecodeResult(b.Result)
	if err != nil {
		return prev, "", fmt.Errorf("authority store: reconcile-import: result: %w", err)
	}
	if res.Seq <= st.h.Seq {
		why, err := presentOrFork(ctx, st, eventPresentQuery, res.Seq, ac.EntryDigest(b.Result))
		return prev, why, err
	}
	if why := im.refuses(res.IssuedAtMs); why != "" {
		return prev, why, nil
	}
	next, _, r := ac.ReplayAuthorityBundle(imp.trust, imp.hw, prev, it.Bundle, st.h.Pin)
	if r != ac.ReasonOK {
		return prev, "replay: " + string(r), nil
	}
	why, err := imp.s.applyImported(ctx, st, b, res, it)
	if err != nil || why != "" {
		return prev, why, err
	}
	return next, "", nil
}

// The stored entry digest at a published seq, and at a private dseq.
const (
	eventPresentQuery  = `SELECT entry_digest FROM authority.account_events WHERE account_id = $1 AND seq = $2`
	devicePresentQuery = `SELECT entry_digest FROM authority.device_events WHERE account_id = $1 AND dseq = $2`
)

// presentOrFork: the item's result sits at or below the head (its seq, or a
// private entry's dseq). It is present (skipped, writing nothing) when the
// stored entry at that position, read by query, carries the same entry
// digest, and a fork otherwise.
func presentOrFork(ctx context.Context, st *txState, query string, pos uint64, entry []byte) (string, error) {
	var stored []byte
	err := st.tx.QueryRowContext(ctx, query, st.h.AccountID, i64(pos)).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "a fork: no stored event at that seq", nil
	case err != nil:
		return "", fmt.Errorf("authority store: reconcile-import: present: %w", err)
	case !bytes.Equal(stored, entry):
		return "a fork: another entry at that seq", nil
	}
	return "", nil
}

// applyImported is the commit core's decision, replayed for an authenticated
// result that links to the head (B3-D4), on the same request, head and db_now
// the commit core decided on: the request is rebuilt from the item
// (rebuildRequest), db_now is the result's issued_at, the delegation is the
// one the result names, the head is normalized at that db_now (step 7), and
// then admit (step 8, policy.Admit), the head signatures, step 8a, bindKeys
// (step 10) and the post-state (commitImported). record writes nothing under
// st.replay, so a step's refusal comes back as an outcome (refusalOf).
func (s *Store) applyImported(ctx context.Context, st *txState, b ac.Bundle, res ac.Result, it ac.ImportItem) (string, error) {
	if res.State == ac.StateErased { // row 19: import never produces E directly (DoR:1727)
		return "an erased result", nil
	}
	if az, err := ac.DecodeCPAuthz(b.CPAuthz); err == nil && az.Actor == ac.ActorRestore {
		return s.replayRestoreCancel(ctx, st, b, res, it) // row 22, by its own held_status (importdevice.go)
	}
	rq, why := rebuildRequest(b, it)
	if why != "" {
		return why, nil
	}
	sig, err := replayDelegation(ctx, st.tx, res.DelegationGen, res.DelegationSerial, st.accepted)
	if err != nil || sig == nil {
		return "the result's delegation is not installed", err
	}
	st.rq, st.sig, st.now = rq, sig, res.IssuedAtMs
	if out, done := s.windowExpiry(ctx, st); done { // steps 3 and 4 (6 is in the rebuild)
		return refusalOf(out)
	}
	if err := normalizeHead(ctx, st); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: normalize: %w", err)
	}
	if why, err := replayGates(ctx, st); why != "" || err != nil {
		return why, err
	}
	d, _, err := admit(st)
	if err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: device set: %w", err)
	}
	if d.Refusal != "" {
		return "policy: " + string(d.Refusal), nil
	}
	if !verifyHeadSigs(rq, st.h) {
		return "a head signature does not verify", nil
	}
	newSet, out, done := s.step8a(ctx, st, d)
	if done {
		return refusalOf(out)
	}
	if out, done := s.bindKeys(ctx, st, bindingsOf(rq), st.h.Seq+1); done {
		return refusalOf(out)
	}
	return s.commitImported(ctx, st, d, newSet, b, it)
}

// refusalOf reads a step's outcome under st.replay: a refusal (record's) or a
// 400 (a signature the step verifies) refuses the item; anything else is the
// step's store error.
func refusalOf(out Outcome) (string, error) {
	switch {
	case out.Kind == KindRefused:
		return "refused: " + out.Code, nil
	case out.HTTP == 400:
		return "a step's signature does not verify", nil
	}
	return "", fmt.Errorf("authority store: reconcile-import: step: %d %s", out.HTTP, out.Code)
}

// rebuildRequest is the request the commit core decided, rebuilt from the item
// as decodeSubmit decodes a submit (B3-D4 step 2): the bundle's positions, the
// attached entry's device-intent and signatures, and the item's cp_evidence.
// decodeSubmit refuses a restore actor (row 22 replays through
// replayRestoreCancel, which never reaches here). The bundle the
// commit core would store for that request must be the authenticated bytes;
// the cp-authz must bind the request and that evidence (cakBound: the
// evidence digest is not in the bundle, so nothing else checks it); the
// head-independent signatures must verify (step 4: sig_new_device sits in
// the attached entry, beyond ac's reach); and the intent must be well formed
// for its op (step 6).
func rebuildRequest(b ac.Bundle, it ac.ImportItem) (*request, string) {
	sub := ac.Submit{Intent: b.Intent, SigCandidate: b.SigCandidate, SigCurrentRoot: b.SigCurrentRoot,
		SigOperator: b.SigOperator, SigRecovery: b.SigRecovery, LegacyPopSig: b.LegacyPopSig, LegacySPKI: b.LegacySPKI,
		EKBinding: b.EKBinding, EKBindingSig: b.EKBindingSig, RecoveryBind: b.RecoveryBind,
		RecoveryBindSig: b.RecoveryBindSig, CPAuthz: b.CPAuthz, CPAuthzSig: b.CPAuthzSig, CPEvidence: it.CPEvidence}
	if len(it.DeviceBundle) > 0 {
		db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
		if err != nil {
			return nil, "the attached entry does not decode"
		}
		sub.DeviceIntent, sub.SigActor, sub.SigNewDevice = db.DeviceIntent, db.SigActor, db.SigNewDevice
	}
	rq, _, ok := decodeSubmit(sub.Encode())
	if !ok {
		return nil, "the item does not rebuild a submit"
	}
	stored := bundleOf(rq.Sub)
	stored.Result, stored.ResultSig = b.Result, b.ResultSig
	if !bytes.Equal(stored.Encode(), it.Bundle) {
		return nil, "the bundle is not the one the commit core stores"
	}
	if !cakBound(rq) {
		return nil, "the cp-authz does not bind the request and its evidence"
	}
	if !verifyStateless(rq) {
		return nil, "a request signature does not verify"
	}
	if ac.IntentForOp(rq.Intent, rq.AZ.Actor, rq.PoP, len(rq.Sub.SigCurrentRoot) > 0) != "" ||
		(rq.DI != nil && ac.DeviceIntentForOp(*rq.DI, rq.Intent.CandidateRoot) != "") {
		return nil, "the intent is malformed for its op"
	}
	return rq, ""
}

// replayDelegation is the delegation row the result names, when the request's
// trust accepted it (B1-D37). nil: no such row, or one the trust ignored.
func replayDelegation(ctx context.Context, tx *sql.Tx, gen, serial uint64, accepted map[string]bool) (*sigDelegation, error) {
	var msg, dg []byte
	err := tx.QueryRowContext(ctx, `SELECT bytes, digest FROM authority.delegations WHERE product_root_gen = $1 AND serial = $2`,
		i64(gen), i64(serial)).Scan(&msg, &dg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authority store: reconcile-import: delegation: %w", err)
	}
	if !accepted[string(ac.Digest(msg))] {
		return nil, nil
	}
	d, err := ac.DecodeDelegation(msg)
	if err != nil { // unreachable: the trust decoded this accepted row
		return nil, fmt.Errorf("authority store: reconcile-import: delegation: %w", err)
	}
	return &sigDelegation{Gen: gen, Serial: serial, D: d, Digest: dg}, nil
}

// commitImported is step 11 without the signature: the result the commit core
// builds for this decision (buildResult) must pass CheckPost and equal the
// signed result byte for byte; a device part's attached entry must be the
// commit core's too (attachedEntry). Then the events are written imported,
// under the current restore_epoch (writeEvents with st.replay), and the head
// moves exactly as a commit moves it (applyCommit, saveHead).
func (s *Store) commitImported(ctx context.Context, st *txState, d policy.Decision, newSet []byte, b ac.Bundle,
	it ac.ImportItem) (string, error) {
	rq, h := st.rq, st.h
	res, ok := s.buildResult(st, d)
	if !ok {
		return "the result's activation does not compute", nil
	}
	if ac.CheckPost(d.Row, h.published(), rq.Intent, res, d.OutcomeP, ac.Assurance(h.Segment),
		ac.Assurance(h.RecoveryBindAssurance)) != ac.ReasonOK {
		return "the post-state is not the decision's", nil
	}
	if rb := res.Encode(); rb == nil || !bytes.Equal(rb, b.Result) {
		return "the signed result is not the commit core's", nil
	}
	entry := ac.EntryDigest(b.Result)
	var drb, dsig []byte
	if rq.DI != nil {
		var why string
		if drb, dsig, newSet, why = attachedEntry(st, res, entry, newSet, it.DeviceBundle); why != "" {
			return why, nil
		}
	}
	if err := s.writeEvents(ctx, st, res, signedOut{rb: b.Result, rsig: b.ResultSig, entry: entry, drb: drb, dsig: dsig}); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: events: %w", err)
	}
	if err := applyCommit(st, d, res, entry, newSet, drb); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: apply: %w", err)
	}
	if err := saveHead(ctx, st.tx, h); err != nil {
		return "", fmt.Errorf("authority store: reconcile-import: head: %w", err)
	}
	return "", nil
}

// attachedEntry is a published item's attached device entry, replayed: its
// device result must be the one step 11 signs for this op (attachedResult)
// byte for byte, its signature must verify under an RSK the result's
// delegation lists, and the entry must carry nothing but the device-intent
// and signatures the rebuilt request took from it, so the bytes writeEvents
// stores are the bytes judged here. That is the attached entry's whole device
// replay: attachedResult is built on the head's dseq and private head digest
// (the link), step 8a's device part has already checked the device-intent's
// expected_dseq and run ApplyDeviceOp (DR1–DR9), and the attached op's policy
// is the published op's (policy.Admit), never AdmitPrivate.
func attachedEntry(st *txState, res ac.Result, entry, newSet, raw []byte) (drb, dsig, set []byte, why string) {
	drb, set = attachedResult(st, res, entry, newSet)
	if drb == nil {
		return nil, nil, nil, "the attached result does not compute"
	}
	db, err := ac.DecodeDeviceBundle(raw)
	if err != nil {
		return nil, nil, nil, "the attached entry does not decode"
	}
	want := ac.DeviceBundle{DeviceIntent: st.rq.Sub.DeviceIntent, SigActor: st.rq.Sub.SigActor,
		SigNewDevice: st.rq.Sub.SigNewDevice, DeviceResult: drb, DeviceResultSig: db.DeviceResultSig}.Encode()
	if !bytes.Equal(want, raw) {
		return nil, nil, nil, "the attached entry is not the commit core's"
	}
	if !slices.ContainsFunc(st.sig.D.RSKPublicKeys, func(k []byte) bool { return ac.VerifyP384(k, drb, db.DeviceResultSig) }) {
		return nil, nil, nil, "the attached entry's signature does not verify"
	}
	return drb, db.DeviceResultSig, set, ""
}

// errHistoryUnproven: an account's stored history does not rebuild its head
// (chainHeadOf). Import answers it by holding that account, and only for an
// account an authenticated item names (B3-D16); never by a 503 store, which
// would hand a CP that names the account the tick's non-client leg (D-147) at
// will.
var errHistoryUnproven = errors.New("authority store: the stored history does not rebuild the head")

// chainHeadOf rebuilds the ac.ChainHead of the head row h from the account's
// stored account_events bundles (the plan's Task 0 record, step 2; B3-D15 R3).
// The store persists no chain-head blob and ac has no constructor from
// history, so it starts at ac.GenesisHead(h's realm, h's account), the realm
// being meta.realm_id, and folds every stored bundle in seq order through
// ac.ReplayAuthorityBundle, VerifyBundle in the issuing authority's form:
//
//   - the seq-0 pin is h.Pin. At seq 0 lockHead fills it from
//     legacy_population, exactly what a fresh submit uses; at seq ≥ 1 it is
//     the persisted legacy_ek_digest, which checkPost keeps immutable from seq
//     0 on, so it is the pin that admitted seq 1. It is read at genesis only;
//   - a superseded entry is admitted. Every delegation the store's trust
//     holds is a delegations row (buildTrust reads nothing else), so a
//     superseded entry is one the authority signed under a delegation it still
//     has installed (DoR:1727), and T11's min_valid_serial would otherwise
//     stop the fold at the first renewal.
//
// The invariant: the head it returns is one ReplayAuthorityBundle produced,
// from the authority's realm, the head row's own pin and the account's own
// stored bytes, and it describes exactly h, its seq h.Seq and its entry digest
// h.Entry. So an item that replays against it links to the head the authority
// holds. A stored bundle that does not replay, a gap, or a fold that ends
// anywhere but h is errHistoryUnproven; a database error is returned as it
// is. The fold's inputs are authority rows only (account_events and the head
// row): a CP chooses which accounts are folded, never what a fold accepts.
//
// What breaks it: folding on past a Reason other than ok (the replay returns
// prev unchanged, so the fold would stop silently at the last good entry);
// dropping the end check (a head whose newest events are missing would
// rebuild an older head); or taking the pin from anywhere but the head row
// (a pin the account never had would admit a forged first result).
//
// Its cost is one signature verification per stored event, under L1. A CP can
// name up to 64 accounts per batch, and only while the realm is unreconciled
// (the import route refuses otherwise), so the fold runs once per account per
// batch.
func chainHeadOf(ctx context.Context, q queryer, trust *ac.Trust, hw ac.HighWater, h *headRow) (ac.ChainHead, error) {
	rows, err := q.QueryContext(ctx, `SELECT bundle FROM authority.account_events WHERE account_id = $1 ORDER BY seq`,
		h.AccountID)
	if err != nil {
		return ac.ChainHead{}, err
	}
	defer func() { _ = rows.Close() }()
	head := ac.GenesisHead(h.RealmID, h.AccountID)
	var n uint64
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return ac.ChainHead{}, err
		}
		next, _, r := ac.ReplayAuthorityBundle(trust, hw, head, b, h.Pin) // superseded: admitted (see above)
		if r != ac.ReasonOK {
			return ac.ChainHead{}, fmt.Errorf("%w: a stored bundle is %s", errHistoryUnproven, r)
		}
		head = next
		n++
	}
	if err := rows.Err(); err != nil {
		return ac.ChainHead{}, err
	}
	if n != h.Seq || (n > 0 && !bytes.Equal(ac.EntryDigest(head.HeadResult), h.Entry)) {
		return ac.ChainHead{}, fmt.Errorf("%w: the history ends elsewhere", errHistoryUnproven)
	}
	return head, nil
}
