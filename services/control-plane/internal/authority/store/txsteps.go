package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

type txState struct {
	tx                                                        *sql.Tx
	rq                                                        *request
	now                                                       uint64
	h                                                         *headRow
	inPop                                                     bool
	sig                                                       *sigDelegation
	trust                                                     *ac.Trust
	accepted                                                  map[string]bool // the delegation digests trust accepted (B1-D37)
	acc, restoreStartedMs, reconcileCompletedMs, restoreEpoch uint64
	reconciled                                                bool // read after L1 (B1-D33)
	norm                                                      normOut
	preNorm                                                   ac.PublishedHead
	// replay marks import's replay of a retained result (import.go, B3-D4):
	// record writes no ledger row and does not commit, and writeEvents marks
	// the events imported. Every commit-path transaction leaves it false.
	replay bool
}

func setLocalTimeouts(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{`SET LOCAL lock_timeout = '2s'`, `SET LOCAL statement_timeout = '5s'`,
		`SET LOCAL idle_in_transaction_session_timeout = '10s'`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// commit opens the transaction and runs steps 1–7, then decide (steps 8–11).
// Every early return rolls back through the deferred Rollback.
func (s *Store) commit(ctx context.Context, conn *sql.Conn, rq *request) Outcome {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return s.storeError()
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return s.storeError()
	}
	st := &txState{tx: tx, rq: rq}
	if out, done := s.stepsL1ToReplay(ctx, st); done { // 1, db_now, 2
		return out
	}
	if out, done := s.stepsWindowToMalformed(ctx, st); done { // 3–6
		return out
	}
	if out, done := s.stepNormalize(ctx, st); done { // 7
		return out
	}
	return s.decide(ctx, st) // 8–11 (Task 11)
}

func (s *Store) stepsL1ToReplay(ctx context.Context, st *txState) (Outcome, bool) {
	rq := st.rq
	h, inPop, err := lockHead(ctx, st.tx, rq.Intent.AccountID) // 1: L1
	if err != nil {
		return s.storeError(), true
	}
	st.h, st.inPop = h, inPop
	if err := s.readNowAfterL1(ctx, st); err != nil { // B1-D20
		return s.signerOrStore(err), true
	}
	if !st.reconciled { // B1-D33: enter_unreconciled() can run between preBegin and L1
		return errorOutcome(503, ac.ErrorReconcile), true
	}
	stored, hit, err := lookupStored(ctx, st.tx, rq.Intent.AccountID, rq.Intent.RequestID, rq.ledgerKey(), rq.Dev != nil) // 2
	if err != nil {
		return s.storeError(), true
	}
	if hit {
		if err := st.tx.Commit(); err != nil {
			return s.storeError(), true
		}
		return stored, true
	}
	if err := s.resolveSigner(ctx, st); err != nil { // after a miss only (B1-D19)
		return s.signerOrStore(err), true
	}
	if ok, err := s.drainAndRevocation(ctx, st); err != nil {
		return s.storeError(), true
	} else if !ok {
		return errorOutcome(400, ac.ErrorBadAuthz), true
	}
	return Outcome{}, false
}

// readNowAfterL1 reads db_now once and the meta values steps 3–11 need (B1-D20),
// with reconciled for B1-D33.
func (s *Store) readNowAfterL1(ctx context.Context, st *txState) error {
	now, err := s.dbNow(ctx, st.tx)
	if err != nil {
		return err
	}
	st.now = now
	var rs, rc sql.NullTime
	var epoch, acc int64
	if err := st.tx.QueryRowContext(ctx, `SELECT cancel_unavailable_ms, restore_started_at, reconcile_completed_at,
		restore_epoch, reconciled FROM authority.meta WHERE id = 1`).Scan(&acc, &rs, &rc, &epoch, &st.reconciled); err != nil {
		return err
	}
	st.acc, st.restoreStartedMs, st.reconcileCompletedMs, st.restoreEpoch = u64(acc), msOfNull(rs), msOfNull(rc), u64(epoch)
	return nil
}

// resolveSigner reads the trust and T10's signing delegation (B1-D19). It runs
// after the locked replay lookup misses: a replay sends bytes already signed,
// so it must not need this node to be able to sign now (review round 3).
func (s *Store) resolveSigner(ctx context.Context, st *txState) error {
	var err error
	if st.trust, st.accepted, err = s.trustFor(ctx, st.tx); err != nil {
		return err
	}
	st.sig, err = s.signingDelegation(ctx, st.tx, st.trust.CurrentGen(), st.now, st.accepted)
	return err
}

// errSigner marks a 503 signer cause (DoR:1450 step 9, :1519, :1756): no usable
// signing delegation, or the RSK failed to sign. It is wrapped only where that
// verdict is reached. A database error is never wrapped, so signerOrStore
// reports it as 503 store (DoR:1600, :1614; Codex round 3).
var errSigner = errors.New("authority store: no usable signing delegation")

func (s *Store) signerOrStore(err error) Outcome {
	if errors.Is(err, errSigner) {
		return s.signerError()
	}
	return s.storeError()
}

// drainAndRevocation is step 2's miss branch: T8's generation again, against
// the signer this transaction resolved (B1-D34), then T9's drain clause read
// inside this transaction (D-171) and revoked_keys, uncached (D-207).
func (s *Store) drainAndRevocation(ctx context.Context, st *txState) (bool, error) {
	c := st.rq.CAK
	if c.Gen != st.sig.Gen || c.Serial > st.sig.Serial { // T8 against the signer of this transaction (B1-D34)
		return false, nil
	}
	var grace int64
	if err := st.tx.QueryRowContext(ctx, `SELECT drain_grace_ms FROM authority.delegations
		WHERE product_root_gen = $1 AND serial = $2`, i64(c.Gen), i64(c.Serial)).Scan(&grace); err != nil {
		return false, err
	}
	if grace < 0 { // CHECK (drain_grace_ms IN (0, 660000)) rules it out; fail closed
		return false, nil
	}
	supAt, err := supersedingInstalledAt(ctx, st.tx, c.Gen, c.Serial, st.accepted)
	if err != nil {
		return false, err
	}
	if !ac.DrainClause(supAt.Valid, msOfNull(supAt), uint64(grace), st.now) {
		return false, nil
	}
	revoked, err := isRevoked(ctx, st.tx, ac.KeyDigest(c.Key))
	if err != nil || revoked {
		return false, err
	}
	if st.rq.AZ.Actor != ac.ActorOperator {
		return true, nil
	}
	var qerr error
	ref := ac.DelegationRef{Gen: st.sig.Gen, Serial: st.sig.Serial}
	ok := st.trust.OperatorAuthority(st.rq.IntentDigest, st.rq.Sub.SigOperator, ref,
		st.rq.AZ.AssertedAtMs, ref, func(k []byte) bool {
			r, err := isRevoked(ctx, st.tx, ac.KeyDigest(k))
			if err != nil {
				qerr = err
				return true // fail closed
			}
			return r
		})
	return ok && qerr == nil, qerr
}

// supersedingInstalledAt is T9's installed_at(superseding): the earliest
// installed_at among higher serials of gen in accepted, the set trustFor
// returned with this transaction's trust (B1-D36, B1-D37). A row the trust
// ignored, or never saw, never supersedes, as it is never the signer.
// end-drain asks the same question through it (B3-D10), so the verb's
// "superseded" and serve's cannot differ.
func supersedingInstalledAt(ctx context.Context, q queryer, gen, serial uint64, accepted map[string]bool) (sql.NullTime, error) {
	rows, err := q.QueryContext(ctx, `SELECT bytes, installed_at FROM authority.delegations
		WHERE product_root_gen = $1 AND serial > $2 ORDER BY installed_at`, i64(gen), i64(serial))
	if err != nil {
		return sql.NullTime{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var msg []byte
		var at sql.NullTime
		if err := rows.Scan(&msg, &at); err != nil {
			return sql.NullTime{}, err
		}
		if accepted[string(ac.Digest(msg))] {
			return at, rows.Err()
		}
	}
	return sql.NullTime{}, rows.Err()
}

// isRevoked matches key_digest alone. It is revoked_keys' primary key and
// the table is insert-only, so a row filed under the wrong kind can never be
// corrected: a kind filter would let that key through.
func isRevoked(ctx context.Context, tx *sql.Tx, digest []byte) (bool, error) {
	var x bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM authority.revoked_keys WHERE key_digest = $1)`,
		digest).Scan(&x)
	return x, err
}

func isCancel(op string) bool { return op == ac.OpCancelPending }

// stepsWindowToMalformed is steps 3–6.
func (s *Store) stepsWindowToMalformed(ctx context.Context, st *txState) (Outcome, bool) {
	rq := st.rq
	if out, done := s.windowExpiry(ctx, st); done { // 3, 4
		return out, true
	}
	if st.h.Held && policy.HeldRefuses(st.h.published().FromCode(), rq.Intent.Op, rq.AZ.Actor) { // 5
		return s.record(ctx, st, ac.RefusalHeld), true
	}
	rootCancels := len(rq.Sub.SigCurrentRoot) > 0
	if ac.IntentForOp(rq.Intent, rq.AZ.Actor, rq.PoP, rootCancels) != "" { // 6
		return s.record(ctx, st, ac.RefusalMalformedForOp), true
	}
	if rq.DI != nil && ac.DeviceIntentForOp(*rq.DI, rq.Intent.CandidateRoot) != "" {
		return s.record(ctx, st, ac.RefusalMalformedForOp), true
	}
	return Outcome{}, false
}

// windowExpiry is steps 3 and 4 for a published op: the cp-authz window, then
// the intent's expiry. Import's replay runs it at the result's issued_at
// (import.go), where a cp-authz asserted after that db_now is a refusal, never
// the 503 that sets the tick's non-client leg (D-147): only a live clock lags.
func (s *Store) windowExpiry(ctx context.Context, st *txState) (Outcome, bool) {
	rq, now, a := st.rq, st.now, st.rq.AZ.AssertedAtMs
	if isCancel(rq.Intent.Op) { // 3, cancel
		if a > now+ac.EpsilonMs {
			if st.replay {
				return s.record(ctx, st, ac.RefusalAuthzOutOfWindow), true
			}
			s.nonClient503.Store(true) // D-228: the authority's clock lags
			return errorOutcome(503, ac.ErrorClock), true
		}
		lower := sat(now, st.sig.D.GMs+ac.CancelSlackMs)
		if a <= lower {
			return s.record(ctx, st, ac.RefusalAuthzOutOfWindow), true
		}
	} else if a <= sat(now, ac.IntentWindowMs) || a > now+ac.EpsilonMs { // 3, non-cancel
		return s.record(ctx, st, ac.RefusalAuthzOutOfWindow), true
	}
	if code := expiry(rq, now, st.restoreStartedMs); code != "" { // 4
		return s.record(ctx, st, code), true
	}
	return Outcome{}, false
}

func sat(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

func expiry(rq *request, now, restoreStarted uint64) ac.Refusal {
	exp := rq.Intent.ExpiresAtMs
	if isCancel(rq.Intent.Op) {
		if exp <= rq.AZ.AssertedAtMs {
			return ac.RefusalExpired
		}
		if exp > rq.AZ.AssertedAtMs+ac.IntentWindowMs { // DoR:270, D-267 (red-team F1)
			return ac.RefusalExpiryOutOfRange
		}
		return ""
	}
	switch {
	case exp <= now:
		return ac.RefusalExpired
	case exp > now+ac.IntentWindowMs:
		return ac.RefusalExpiryOutOfRange
	case restoreStarted != 0 && exp <= restoreStarted+ac.IntentWindowMs:
		return ac.RefusalExpired
	}
	return ""
}

// stepNormalize is step 7 (normalizeHead); a failure is 503 store.
func (s *Store) stepNormalize(ctx context.Context, st *txState) (Outcome, bool) {
	if err := normalizeHead(ctx, st); err != nil {
		return s.storeError(), true
	}
	return Outcome{}, false
}

// normalizeHead is step 7 on st.h at st.now. The pending result's assurance
// (which opens the segment at activation) is read from the head event's
// stored result. Import's replay normalizes through it too, at each retained
// result's own issued_at (import.go).
func normalizeHead(ctx context.Context, st *txState) error {
	st.preNorm = st.h.published()
	pa := ""
	if st.h.State == ac.StatePending && len(st.h.PendRoot) > 0 {
		var bundle []byte
		if err := st.tx.QueryRowContext(ctx, `SELECT bundle FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
			st.h.AccountID, i64(st.h.Seq)).Scan(&bundle); err != nil {
			return err
		}
		b, err := ac.DecodeBundle(bundle)
		if err != nil {
			return err
		}
		res, err := ac.DecodeResult(b.Result)
		if err != nil {
			return err
		}
		pa = res.Assurance
	}
	n, err := normalize(st.h, st.now, st.acc, pa)
	if err != nil {
		return err
	}
	st.norm = n
	return nil
}

// record writes a recorded refusal (D-90) and commits it. Under import's
// replay (st.replay) it writes and commits nothing: the replay rolls the item
// back and holds the account (import.go), so no ledger row is ever written
// for a request this process never received, and the account's transaction
// is never committed from inside a step.
func (s *Store) record(ctx context.Context, st *txState, code ac.Refusal) Outcome {
	rq := st.rq
	if st.replay {
		return refusedOutcome(rq.Intent.RequestID, code)
	}
	if _, err := st.tx.ExecContext(ctx, `INSERT INTO authority.request_ledger (account_id, request_id, intent_digest, code, cp_evidence)
		VALUES ($1, $2, $3, $4, $5)`, rq.Intent.AccountID, rq.Intent.RequestID, rq.ledgerKey(), string(code), rq.Sub.CPEvidence); err != nil {
		return s.storeError()
	}
	if err := st.tx.Commit(); err != nil {
		return s.storeError()
	}
	return refusedOutcome(rq.Intent.RequestID, code)
}

var _ = bytes.Equal
