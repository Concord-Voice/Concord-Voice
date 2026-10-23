package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// reconcile/required and reconcile/hold (DoR:527, :1610, :1611, :1728; RT6-7,
// D-287, B3-D14, B3-D15 R1).
//
// The invariant: the authority enters unreconciled, or holds an account, only
// on evidence it has verified itself. Each hold-req bundle must authenticate
// (ac.AuthenticateBundle: EN5, every part naming this realm and one account,
// the result binding its intent and cp-authz, and T7's RSK signature, window
// and scope), must not be superseded, and its verified result's seq must
// exceed the seq of its account's head, read under L1. An RSK-signed result
// above the head is proof that the authority's head once reached that seq, and
// a head goes back only across a restore or a fork; no link to the head is
// needed or claimed (DoR:527 sends the HIGHEST retained seq). Every bundle is
// authenticated before any head is read or locked, every head is then judged
// before anything is written, and the request is one transaction, so one
// unproven bundle refuses the whole request with ErrBadAuthz and writes
// nothing. What breaks it: a write before the last head is judged; an account
// or seq taken from anything but the authenticated result; a superseded
// bundle admitted, which would let a leaked retired RSK take the realm
// unreconciled; or a head judged without L1, which a commit could move.
//
// A full CP keeps one choice (§7): which of the accounts it holds evidence for
// end held. It gains no account it holds no ahead, RSK-signed bundle for.

// ReconcileRequired is POST /v1/reconcile/required (DoR:1610): once every
// bundle of the hold-req is proven ahead (aheadBundles), it calls
// authority.enter_unreconciled() in the same transaction, so it takes the
// heads and then meta, the one lock order (operatorRun). Its prologue is
// operatorRun's. It holds nothing: only reconcile/hold does.
func (s *Store) ReconcileRequired(ctx context.Context, body []byte) error {
	return operatorRun(ctx, s, "reconcile-required", body, decodeHoldReq,
		func(ctx context.Context, conn *sql.Conn, req ac.HoldReq) error {
			return operatorTx(ctx, conn, "reconcile-required", func(tx *sql.Tx) error {
				if _, err := s.aheadBundles(ctx, tx, req); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `SELECT authority.enter_unreconciled()`); err != nil {
					return fmt.Errorf("authority store: reconcile-required: enter: %w", err)
				}
				return nil
			})
		})
}

// ReconcileHold is POST /v1/reconcile/hold (DoR:1611): while unreconciled, it
// sets held on the head of every account the hold-req proves ahead, all in one
// transaction, and answers 200 with no body (B3-D14). Reconciled is
// ErrReconciled (409, B3-D2), before any bundle is judged. Its prologue is
// operatorRun's.
//
// reconciled is read without a lock: a reconcile-complete that commits while
// the bundles are judged lets this hold commit after it. That race only ends
// an account held on verified evidence, the state clear-held exists for; a
// meta lock here would order meta before the heads, against the one lock order
// (the heads, then meta: operatorRun).
func (s *Store) ReconcileHold(ctx context.Context, body []byte) (Outcome, error) {
	err := operatorRun(ctx, s, "reconcile-hold", body, decodeHoldReq,
		func(ctx context.Context, conn *sql.Conn, req ac.HoldReq) error {
			return operatorTx(ctx, conn, "reconcile-hold", func(tx *sql.Tx) error { return s.hold(ctx, tx, req) })
		})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{HTTP: 200, Kind: KindOK}, nil
}

func (s *Store) hold(ctx context.Context, tx *sql.Tx, req ac.HoldReq) error {
	var reconciled bool
	if err := tx.QueryRowContext(ctx, `SELECT reconciled FROM authority.meta WHERE id = 1`).Scan(&reconciled); err != nil {
		return fmt.Errorf("authority store: reconcile-hold: meta: %w", err)
	}
	if reconciled {
		return ErrReconciled
	}
	ids, err := s.aheadBundles(ctx, tx, req)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE authority.account_heads SET held = true WHERE account_id = ANY($1::uuid[])`,
		pq.Array(ids))
	if err != nil {
		return fmt.Errorf("authority store: reconcile-hold: hold: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("authority store: reconcile-hold: rows: %w", err)
	}
	if n != int64(len(ids)) { // aheadBundles locked a row for every id; anything else is not a hold
		return fmt.Errorf("authority store: reconcile-hold: held %d of %d heads", n, len(ids))
	}
	return nil
}

// decodeHoldReq is the hold-req and its order (ac.CheckHoldReq): one bundle per
// account, in strictly ascending account_id order, so aheadBundles takes L1 in
// that order. A body that fails is ErrBadAuthz.
func decodeHoldReq(body []byte) (ac.HoldReq, error) {
	req, err := ac.CheckHoldReq(body)
	if err != nil {
		return ac.HoldReq{}, fmt.Errorf("%w: the body is not a hold-req in account order", ErrBadAuthz)
	}
	return req, nil
}

// aheadBundles proves every bundle of req inside tx and returns their account
// ids, in req's order, which is strictly ascending account_id (CheckHoldReq,
// over the same result bytes AuthenticateBundle verifies).
//
//  1. Every bundle is authenticated (ac.AuthenticateBundle) under the store's
//     trust before any head is read or locked: a bundle that does not
//     authenticate, or that T7 reports superseded, refuses the request before
//     it can wait on, or create, any account's head.
//  2. Then, in that ascending order (DoR:1465), L1 on the account the verified
//     result names, so the head judged cannot move before tx ends, and the
//     verified result must name the head's realm (meta's) and a seq above the
//     head's.
//
// Any failure is ErrBadAuthz for the whole request; a database error is
// returned as it is.
func (s *Store) aheadBundles(ctx context.Context, tx *sql.Tx, req ac.HoldReq) ([]string, error) {
	trust, _, err := s.trustFor(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("authority store: hold: trust: %w", err)
	}
	hw := trust.HighWater()
	results := make([]ac.Result, 0, len(req.Bundles))
	for _, raw := range req.Bundles {
		res, superseded, r := ac.AuthenticateBundle(trust, hw, raw)
		if r != ac.ReasonOK {
			return nil, fmt.Errorf("%w: a hold-req bundle does not authenticate: %s", ErrBadAuthz, r)
		}
		if superseded { // strict T7: a hold-req carries no covering status (B3-D15 R1)
			return nil, fmt.Errorf("%w: a hold-req bundle is superseded", ErrBadAuthz)
		}
		results = append(results, res)
	}
	ids := make([]string, 0, len(results))
	for _, res := range results {
		h, _, err := lockHead(ctx, tx, res.AccountID)
		if err != nil {
			return nil, fmt.Errorf("authority store: hold: lock: %w", err)
		}
		if res.RealmID != h.RealmID {
			return nil, fmt.Errorf("%w: a hold-req bundle names another realm than the authority's", ErrBadAuthz)
		}
		if res.Seq <= h.Seq {
			return nil, fmt.Errorf("%w: a hold-req bundle is not ahead of its head", ErrBadAuthz)
		}
		ids = append(ids, res.AccountID)
	}
	return ids, nil
}
