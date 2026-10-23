package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/google/uuid"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// clear-held (§3.4 DoR:1731, LD-46, D-268, D-146, D-286; B3-D1): the owner verb
// that ends a hold, account by account, with serve stopped, and the only
// owner verb that signs.
//
// Its invariant: a head leaves `held` only through one of three exits, each
// decided under L1 from the head's own state, and every exit but A0's leaves
// the account at force_unverified with no activation and no filled
// recovery-key slot completing before T + G′, T being a db_now read after L1.
//   - A held P, D or Pr is first restore-cancelled (row 22, commitRestoreCancel):
//     the commit core signs held_status and the result in one transaction and
//     leaves held set. The head is then an ordinary held head, and the L1
//     transaction below ends it.
//   - A held A0 clears held and nothing else: force_unverified would refuse the
//     row 1 the owner re-enrols with (DoR mutation 8).
//   - Any other held head, Pe and the state row 22 left behind included: under
//     L1, held=false, force_unverified=true, activation_floor_ms ≥ T + G′, and a
//     filled slot's replacement_floor_ms ≥ T + G′, in one transaction.
//
// Row 22 and the L1 transaction are two transactions, so a run can end between
// them. That is safe because the state row 22 leaves is never P, D or Pr: a
// re-run classifies the still-held head as an L1 case and never signs a second
// row 22 (Review Focus 2). What breaks it: a classifier that reads anything but
// the locked head's state (so a resumed head could be cancelled twice); held
// cleared in a transaction apart from force_unverified and the floors; T read
// before the head lock; a floor lowered below one written earlier (each floor
// is raised with GREATEST, as reconcile-complete's are); or a write while serve
// runs, which holds no D-241 memory of this verb's held_status and could
// commit on the head between its reads and its write. ServeLock, taken before
// anything else is read, rules that out on this node.
//
// Signing is fenced before any account is read: the verb refuses unless this
// RSK is listed in the installed high-water delegation, the check serve makes
// at StepKeys (D-223). Without it an RSK the realm no longer delegates could
// not sign row 22 (step 9 refuses it) yet would still clear every A0 and L1
// head in the batch, ending holds under a key the trust has dropped.

// ClearHeldMax is the most accounts one clear-held run takes.
const ClearHeldMax = 64

// The clear-held outcomes (§3.4). A refusal is ClearRefused plus its reason;
// the CLI exits non-zero on any.
const (
	ClearRestoreCancel = "restore_cancel"
	ClearAbsent        = "absent"
	ClearUnverified    = "unverified"
	ClearNotHeld       = "not_held"
	ClearRefused       = "refused:"
)

// ClearResult is one account's clear-held outcome, in input order.
type ClearResult struct{ Account, Outcome string }

// ClearHeld is clear-held (§3.4). It refuses, writing nothing, with no signer,
// with an empty, oversized or duplicated account list, with ServeLock held
// (ErrServeLockHeld), on a standby (ErrNotLeader) and when this RSK is not in
// the installed high-water delegation. Otherwise it clears each account in
// input order. An account's own refusal (clearRefusal) is recorded as its
// outcome and the rest continue; finish runs once, after the last account,
// and its error is returned with the results. Any other error, the
// database's, the clock's, the signer's or the lock's, is no refusal (Codex
// round 2, G5): the run stops at that account, which gets no result, and the
// results so far return with the error, joined with finish's, since the
// accounts before it committed.
//
// Everything after ServeLock, finish included, runs under the lock's watch
// (ServeLock.Watch): a lock session that dies mid-run cancels the rest, the
// run stops at the account it cuts short, and the error carries
// ErrServeLockLost. A run stopped between an account's row 22 and its L1
// transaction is the resumable state described above.
func (o *Owner) ClearHeld(ctx context.Context, accounts []string) ([]ClearResult, error) {
	if o.sig == nil {
		return nil, fmt.Errorf("%w: clear-held needs the RSK", ErrRefused)
	}
	if err := checkClearAccounts(accounts); err != nil {
		return nil, err
	}
	lock, err := AcquireServeLock(ctx, o.db)
	if errors.Is(err, ErrServeLockHeld) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("authority store: clear-held: serve lock: %w", err)
	}
	defer lock.Release()
	ctx, stop := lock.Watch(ctx)
	defer stop() // before Release: the watch pings the lock's connection
	results, err := o.clearLocked(ctx, accounts)
	return results, lockLost(ctx, err)
}

// clearLocked is ClearHeld once ServeLock is held, under ctx, the lock's
// watch.
func (o *Owner) clearLocked(ctx context.Context, accounts []string) ([]ClearResult, error) {
	conn, err := o.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := o.signerDelegated(ctx, conn); err != nil {
		return nil, err
	}
	s, err := Open(Config{DB: o.db, Signer: o.sig, Anchors: o.anchorBytes, Probe: o.probe, Logger: o.log,
		Hooks: Hooks{InRecovery: o.inRecovery, Statfs: ampleDisk}})
	if err != nil {
		return nil, fmt.Errorf("authority store: clear-held: commit core: %w", err)
	}
	results := make([]ClearResult, 0, len(accounts))
	for _, a := range accounts {
		out, err := o.clearOne(ctx, conn, s, a)
		if err != nil && (ctx.Err() != nil || !clearRefusal(err)) {
			if ferr := o.finish(ctx, conn); ferr != nil { // the accounts before this one committed
				return results, errors.Join(err, ferr)
			}
			return results, err
		}
		if err != nil {
			why := strings.TrimPrefix(strings.TrimPrefix(err.Error(), "authority store: "), "refused: ")
			out = ClearRefused + why
		}
		results = append(results, ClearResult{Account: a, Outcome: out})
	}
	return results, o.finish(ctx, conn)
}

// clearRefusal is whether err, from clearOne, is the account's own refusal:
// ErrRefused (row 22 not admitted by the policy or failing the client check,
// or a head row 22 left in a state it never leaves) or
// ErrNotHeldPendingOrDisputed (the head stopped being a held P, D or Pr
// between the classifier and row 22). Each leaves the account as it was and
// says nothing about the next one. Everything else, a 55P03 lock wait, a
// statement or connection failure, db_now behind the account's last as_of_ms
// or issued_at, a signer or delegation fault, a lost leader, is not: it is
// returned as the verb's error and exits by its own reason, never as
// refused.
func clearRefusal(err error) bool {
	return errors.Is(err, ErrRefused) || errors.Is(err, ErrNotHeldPendingOrDisputed)
}

// ClearHeldPlan is clear-held --dry-run: each account's would-be outcome, by
// the classifier ClearHeld uses, on a leader. It takes no lock, needs no
// signer and writes nothing. It does not predict a refusal: a row 22 the
// commit core would refuse plans as restore_cancel.
func (o *Owner) ClearHeldPlan(ctx context.Context, accounts []string) ([]ClearResult, error) {
	if err := checkClearAccounts(accounts); err != nil {
		return nil, err
	}
	conn, err := o.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	results := make([]ClearResult, 0, len(accounts))
	for _, a := range accounts {
		h, err := scanHead(conn.QueryRowContext(ctx, selectHead, a))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("authority store: clear-held plan: head: %w", err)
		}
		results = append(results, ClearResult{Account: a, Outcome: clearOutcome(h)})
	}
	return results, nil
}

// checkClearAccounts is the verb's input rule: 1 to ClearHeldMax account
// UUIDs, no two naming the same account in any spelling.
func checkClearAccounts(accounts []string) error {
	if len(accounts) == 0 || len(accounts) > ClearHeldMax {
		return fmt.Errorf("%w: clear-held takes 1 to %d accounts, got %d", ErrRefused, ClearHeldMax, len(accounts))
	}
	seen := make(map[uuid.UUID]bool, len(accounts))
	for _, a := range accounts {
		id, err := uuid.Parse(a)
		if err != nil {
			return fmt.Errorf("%w: clear-held: %q is not an account id", ErrRefused, a)
		}
		if seen[id] {
			return fmt.Errorf("%w: clear-held: account %s named twice", ErrRefused, id)
		}
		seen[id] = true
	}
	return nil
}

// signerDelegated is D-223's RSK check, as serve makes it at StepKeys: the
// merged view of the installed trust (no file items) on conn, and this RSK in
// its high-water delegation. An unusable trust passes its error through.
func (o *Owner) signerDelegated(ctx context.Context, conn *sql.Conn) error {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("authority store: clear-held: trust: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, v, err := (&Store{anchors: o.anchors}).mergeTrust(ctx, tx, nil)
	if err != nil {
		return fmt.Errorf("authority store: clear-held: installed trust: %w", err)
	}
	own := o.sig.PublicKey()
	if !slices.ContainsFunc(v.HW.RSKPublicKeys, func(k []byte) bool { return bytes.Equal(k, own) }) {
		return fmt.Errorf("%w: clear-held: this RSK is not listed in the installed high-water delegation", ErrRefused)
	}
	return nil
}

// ampleDisk is the commit core's Statfs: clear-held writes a few rows with
// serve stopped, and the disk watch belongs to serve.
func ampleDisk() (uint64, error) { return math.MaxUint64, nil }

// clearOne is one account: the L1 transaction, and for a held P, D or Pr, row
// 22 first and the L1 transaction after it.
func (o *Owner) clearOne(ctx context.Context, conn *sql.Conn, s *Store, account string) (string, error) {
	out, err := clearL1(ctx, conn, account)
	if err != nil || out != ClearRestoreCancel {
		return out, err
	}
	if _, err := s.commitRestoreCancel(ctx, account); err != nil {
		return "", err // nothing committed
	}
	// Row 22 committed. A release that reported it unreplicated is not a
	// refusal here: finish waits on the same LSN, after the last account.
	if o.afterRestoreCancel != nil {
		if err := o.afterRestoreCancel(); err != nil {
			return "", err
		}
	}
	next, err := clearL1(ctx, conn, account)
	if err != nil {
		return "", err
	}
	if next != ClearUnverified {
		return "", fmt.Errorf("%w: clear-held: after row 22 the head reads as %s", ErrRefused, next)
	}
	return ClearRestoreCancel, nil
}

// clearOutcome is the classifier ClearHeld and ClearHeldPlan share, read from
// one head (nil: no head row).
func clearOutcome(h *headRow) string {
	if h == nil || !h.Held {
		return ClearNotHeld
	}
	switch h.published().FromCode() {
	case ac.FromP, ac.FromD, ac.FromPr:
		return ClearRestoreCancel
	case ac.FromA0:
		return ClearAbsent
	}
	return ClearUnverified
}

// clearL1 is the L1 transaction on conn. It locks the head, classifies it, and
// writes the absent or unverified exit; for not_held and restore_cancel it
// writes nothing. T is read after the lock.
func clearL1(ctx context.Context, conn *sql.Conn, account string) (string, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", fmt.Errorf("authority store: clear-held: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return "", err
	}
	h, err := scanHead(tx.QueryRowContext(ctx, selectHeadForUpdate, account))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("authority store: clear-held: head: %w", err)
	}
	out := clearOutcome(h)
	switch out {
	case ClearAbsent:
		_, err = tx.ExecContext(ctx, `UPDATE authority.account_heads SET held = false WHERE account_id = $1`, account)
	case ClearUnverified:
		err = unverifiedExit(ctx, tx, account)
	default:
		return out, nil
	}
	if err != nil {
		return "", fmt.Errorf("authority store: clear-held: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("authority store: clear-held: commit: %w", err)
	}
	return out, nil
}

// unverifiedExit is the L1 exit (D-146, D-286, LD-46): held=false,
// force_unverified=true, and the floors raised to T + G′, a slot's only when
// it is filled (ah_slot: replacement_unavail_base_ms set).
func unverifiedExit(ctx context.Context, tx *sql.Tx, account string) error {
	now, err := clockMs(ctx, tx)
	if err != nil {
		return err
	}
	floor := i64(now + GPrimeMs)
	res, err := tx.ExecContext(ctx, `UPDATE authority.account_heads SET held = false, force_unverified = true,
		activation_floor_ms = GREATEST(COALESCE(activation_floor_ms, 0), $2),
		replacement_floor_ms = CASE WHEN replacement_unavail_base_ms IS NULL THEN replacement_floor_ms
			ELSE GREATEST(COALESCE(replacement_floor_ms, 0), $2) END
		WHERE account_id = $1`, account, floor)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.New("authority store: clear-held: the head row is gone")
	}
	return nil
}
