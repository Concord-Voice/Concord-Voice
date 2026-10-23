package store

import (
	"context"
	"errors"
	"fmt"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

type sigDelegation struct {
	Gen, Serial uint64
	D           ac.Delegation
	Digest      []byte
}

// errNoDelegation is step 9's verdict (DoR:1450): 503 signer, so it wraps errSigner.
var errNoDelegation = fmt.Errorf("%w: no installed delegation covers db_now", errSigner)

// trustFor builds A2's Trust over the anchors and every installed succession
// and delegation, cached until the installed set changes. Delegations are
// insert-only, so (count, max gen, max serial) identifies the set. It also
// returns the delegation digests this build accepted: callers select only
// from that set (B1-D37), never from the shared cache.
func (s *Store) trustFor(ctx context.Context, q queryer) (*ac.Trust, map[string]bool, error) {
	var n, ns, mg, ms int64
	if err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM authority.delegations),
		(SELECT count(*) FROM authority.successions), COALESCE((SELECT max(product_root_gen) FROM authority.delegations), 0),
		COALESCE((SELECT max(serial) FROM authority.delegations), 0)`).Scan(&n, &ns, &mg, &ms); err != nil {
		return nil, nil, err
	}
	key := fmt.Sprintf("%d/%d/%d/%d", n, ns, mg, ms)
	s.trustMu.Lock()
	defer s.trustMu.Unlock()
	if s.trust != nil && s.trustKey == key {
		return s.trust, s.trustAccepted, nil
	}
	tr, accepted, err := buildTrust(ctx, q, s.anchors)
	if err != nil {
		return nil, nil, err
	}
	s.trust, s.trustKey, s.trustAccepted = tr, key, accepted
	return tr, accepted, nil
}

// buildTrust is trustFor's build, uncached: A2's Trust over anchors and every
// installed succession and delegation, and the msg digests of the delegation
// rows it accepted. The owner verbs build through it too (B3-D10), so a verb
// judges installed trust exactly as serve does.
func buildTrust(ctx context.Context, q queryer, anchors ac.Anchors) (*ac.Trust, map[string]bool, error) {
	tr, err := ac.NewTrust(anchors)
	if err != nil {
		return nil, nil, errors.Join(errSigner, err) // the anchors are unusable, not the database
	}
	if _, err := addAll(ctx, q, tr, `SELECT bytes, sig_a, sig_b FROM authority.successions ORDER BY prev_root_gen`, "succession"); err != nil {
		return nil, nil, err
	}
	accepted, err := addAll(ctx, q, tr, `SELECT bytes, sig_a, sig_b FROM authority.delegations ORDER BY product_root_gen, serial`, "delegation")
	if err != nil {
		return nil, nil, err
	}
	return tr, accepted, nil
}

// addAll feeds every row to tr and returns the msg digests it accepted. A row
// that fails T2 or T3 is ErrIgnored: it stays out of the trust and out of the
// returned set, so no selection reads it (review round 2, B1-D37).
func addAll(ctx context.Context, q queryer, tr *ac.Trust, query, typ string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	accepted := map[string]bool{}
	for rows.Next() {
		var msg, sa, sb []byte
		if err := rows.Scan(&msg, &sa, &sb); err != nil {
			return nil, err
		}
		err := tr.AddSigned(ac.Signed{Type: typ, Msg: msg, SigA: sa, SigB: sb}.Encode())
		var halt *ac.HaltError
		switch {
		case errors.As(err, &halt): // T4 (DoR:718): the installed trust halts
			return nil, fmt.Errorf("authority store: trust halt: %w", errors.Join(errSigner, err))
		case errors.Is(err, ac.ErrIgnored):
			// ignored by T2 or T3: not accepted
		case err != nil: // an installed envelope EN5 refuses: the trust is unusable
			return nil, errors.Join(errSigner, err)
		default:
			accepted[string(ac.Digest(msg))] = true
		}
	}
	return accepted, rows.Err()
}

// signingDelegation is T10: the highest installed delegation of gen whose
// window covers nowMs (not_before ≤ now < not_after, §2.10), among the rows in
// accepted, the set trustFor returned with the caller's trust. A row the
// trust ignored (review round 2) or never saw because it was inserted after
// the build (B1-D37) is skipped: results signed under it could be bytes no
// client verifies.
func (s *Store) signingDelegation(ctx context.Context, q queryer, gen, nowMs uint64, accepted map[string]bool) (*sigDelegation, error) {
	rows, err := q.QueryContext(ctx, `SELECT serial, bytes, digest FROM authority.delegations
		WHERE product_root_gen = $1 ORDER BY serial DESC`, i64(gen))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var serial int64
		var msg, dg []byte
		if err := rows.Scan(&serial, &msg, &dg); err != nil {
			return nil, err
		}
		if !accepted[string(ac.Digest(msg))] {
			continue
		}
		d, err := ac.DecodeDelegation(msg)
		if err != nil {
			return nil, errors.Join(errSigner, err) // an accepted row that does not decode
		}
		if d.NotBeforeMs <= nowMs && nowMs < d.NotAfterMs {
			return &sigDelegation{Gen: gen, Serial: u64(serial), D: d, Digest: dg}, rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, errNoDelegation
}

func loadDelegation(ctx context.Context, q queryer, gen, serial uint64) (*sigDelegation, error) {
	var msg, dg []byte
	if err := q.QueryRowContext(ctx, `SELECT bytes, digest FROM authority.delegations
		WHERE product_root_gen = $1 AND serial = $2`, i64(gen), i64(serial)).Scan(&msg, &dg); err != nil {
		return nil, err
	}
	d, err := ac.DecodeDelegation(msg)
	if err != nil {
		return nil, err
	}
	return &sigDelegation{Gen: gen, Serial: serial, D: d, Digest: dg}, nil
}
