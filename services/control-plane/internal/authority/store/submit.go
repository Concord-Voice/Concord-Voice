package store

import (
	"context"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Submit is POST /v1/transactions (§3.2.4).
func (s *Store) Submit(ctx context.Context, body []byte) Outcome {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.txDL)
	defer cancel()
	out, op, rid := s.submit(ctx, body)
	s.log.Info("authority.submit", "op", op, "outcome", int(out.Kind), "code", out.Code, "request_id", rid,
		"latency_ms", time.Since(start).Milliseconds()) // §3.6: never account_id, intents, keys or evidence
	return out
}

func (s *Store) submit(ctx context.Context, body []byte) (Outcome, string, string) {
	if len(body) > BodyCapTx {
		return errorOutcome(413, ac.ErrorTooLarge), "", ""
	}
	rq, rel, out, ok := s.admitAndDecode(body, decodeSubmit)
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
	return s.release(ctx, conn, s.commit(ctx, conn, rq)), op, rid
}

// admitAndDecode is steps 1 and 2 in the DoR's order (review round 2): a
// general mutating slot, then the decode under it. Only when every general
// slot is held does it decode without one, to learn whether the request may
// take its class's reserve (D-271, D-179). That decode is bounded by the body
// cap and checks no signature (step 4 does, under the slot); any other class
// is 503 busy. A body that fails to decode gives its slot back.
func (s *Store) admitAndDecode(body []byte, decode func([]byte) (*request, Outcome, bool)) (*request, func(), Outcome, bool) {
	if rel, ok := s.adm.acquireGeneral(); ok {
		rq, out, ok := decode(body)
		if !ok {
			rel()
			return nil, nil, out, false
		}
		return rq, rel, Outcome{}, true
	}
	rq, out, ok := decode(body)
	if !ok {
		return nil, nil, out, false
	}
	rel, ok := s.adm.acquireMutating(rq.Class)
	if !ok {
		return nil, nil, errorOutcome(503, ac.ErrorBusy), false
	}
	return rq, rel, Outcome{}, true
}
