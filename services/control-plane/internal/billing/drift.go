package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
)

// Drift worker (#2033 spec §11).
//
// Webhooks are the fast path; this worker is the safety net for the ones that
// never arrive. Each pass reconciles up to driftBatch live Stripe rows, most
// urgent first, then looks for subscriptions Stripe holds that Concord has no
// row for (a lost creation webhook), then prunes the dedupe table. Everything
// goes through Reconcile, so a drift pass and a webhook that race converge on
// the same row under the same advisory key.

const (
	// driftInterval is the worker tick.
	driftInterval = 15 * time.Minute
	// driftBatch bounds the rows one pass reconciles.
	driftBatch = 50
	// discoveryBatch bounds the customers one pass lists subscriptions for.
	discoveryBatch = 20
	// eventRetention is how long stripe_webhook_events rows are kept.
	eventRetention = 30 * 24 * time.Hour

	// discoveryCandidateLimit bounds the mappings read per pass, and so the
	// size of the in-memory check-time map.
	discoveryCandidateLimit = 500

	// driftSelectSQL picks the rows to reconcile, urgent buckets first:
	// 1 lapsed inside the 6 h buffer (period over, grace still running),
	// 2 past_due inside its period or the buffer, 3 renewing within 24 h and
	// not touched for 15 min, 4 not touched for 24 h. Buckets 1-3 take only
	// live statuses; unpaid, incomplete and paused rows (and a live row lapsed
	// past the buffer) reach the worker only through bucket 4, so a lost
	// webhook on them is still corrected once a day. A row in none of them is
	// fresh, or ended, and is left alone. $1 is the batch size.
	//
	// ponytail: buckets 1-3 can fill the whole batch and starve bucket 4. At
	// about 3,000 monthly subscribers some 100 rows renew within any 24 h, so
	// bucket 3 alone fills every 50-row batch and the daily bucket never runs.
	// Upgrade path: reserve a fixed share of the batch (say 10 of 50) for
	// bucket 4, as a second query or a per-bucket ROW_NUMBER() cap.
	driftSelectSQL = `SELECT stripe_subscription_id FROM (
		SELECT stripe_subscription_id, updated_at,
			CASE
				WHEN status IN ('active', 'trialing', 'past_due')
					AND current_period_end < NOW()
					AND current_period_end > NOW() - INTERVAL '6 hours' THEN 1
				WHEN status = 'past_due'
					AND current_period_end > NOW() - INTERVAL '6 hours' THEN 2
				WHEN status IN ('active', 'trialing', 'past_due')
					AND current_period_end >= NOW()
					AND current_period_end < NOW() + INTERVAL '24 hours'
					AND updated_at < NOW() - INTERVAL '15 minutes' THEN 3
				WHEN updated_at < NOW() - INTERVAL '24 hours' THEN 4
			END AS bucket
		FROM subscriptions
		WHERE source = 'stripe'
			AND stripe_subscription_id IS NOT NULL
			AND status IN ('active', 'trialing', 'past_due', 'unpaid', 'incomplete', 'paused')
	) AS candidates
	WHERE bucket IS NOT NULL
	ORDER BY bucket, updated_at
	LIMIT $1`

	// driftTouchSQL records that drift looked at a row it could not act on,
	// so the row leaves buckets 3 and 4 for a day instead of heading the
	// batch on every tick. Only drift reads subscriptions.updated_at.
	driftTouchSQL = `UPDATE subscriptions SET updated_at = NOW()
		WHERE stripe_subscription_id = $1 AND source = 'stripe'`

	// discoveryCandidatesSQL lists mappings that reached Checkout in the last
	// seven days and still have no live Stripe row. The predicate is a
	// constant, never formatted. $1 is the read limit.
	discoveryCandidatesSQL = `SELECT sc.stripe_customer_id FROM stripe_customers sc
		WHERE sc.last_checkout_at > NOW() - INTERVAL '7 days'
			AND NOT EXISTS (
				SELECT 1 FROM subscriptions
				WHERE user_id = sc.user_id AND source = 'stripe' AND ` + entitlements.LiveRowPredicate + `)
		ORDER BY sc.last_checkout_at DESC
		LIMIT $1`

	pruneEventsSQL = `DELETE FROM stripe_webhook_events
		WHERE processed_at < NOW() - make_interval(secs => $1)`

	// expireInvitesSQL materializes lapsed Wing invites (spec §9 Expiry). It
	// locks only the seat rows it updates, so it cannot close a cycle with a
	// seat op; an accept that holds one re-checks expiry under its own lock.
	// $1 is the batch size.
	expireInvitesSQL = `UPDATE subscription_seats
		SET state = 'expired', end_reason = 'invite_expired', ended_at = NOW()
		WHERE id IN (SELECT id FROM subscription_seats
			WHERE state = 'pending' AND invite_expires_at <= NOW()
			ORDER BY invite_expires_at LIMIT $1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, payer_user_id, holder_user_id`
)

// DriftReport is what one RunDrift pass did.
type DriftReport struct {
	Fetched     int // rows the bucket query selected
	Reconciled  int // selected rows reconciled without error
	Corrections int // of those, rows whose stored state differed from Stripe
	Failed      int // selected rows whose reconcile returned an error
	Discovered  int // subscriptions found in Stripe that changed a local row
	Pruned      int // dedupe rows deleted
	Expired     int // Wing invites that lapsed (PR-5)
	PairEnded   int // seats ended because the pair unfriended or blocked
}

// RunDriftWorker runs a drift pass at startup and then every driftInterval
// until ctx is canceled. It blocks; start it with go.
func (s *Service) RunDriftWorker(ctx context.Context) {
	runTicker(ctx, driftInterval, func(ctx context.Context) {
		if _, err := s.RunDrift(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("billing: drift pass failed",
				logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		}
	})
}

// logDriftSummary writes the one summary line of a pass that did anything.
// failed counts rows a reconcile could not finish, so it is a warning when
// non-zero: the per-row lines say which, and only this line says how many.
func (s *Service) logDriftSummary(report DriftReport) {
	if report == (DriftReport{}) {
		return
	}
	attrs := []any{
		"fetched", report.Fetched, "corrected", report.Corrections, "failed", report.Failed,
		"discovered", report.Discovered, "pruned", report.Pruned, "invites_expired", report.Expired,
		"pair_ended", report.PairEnded,
	}
	if report.Failed > 0 {
		s.Log.Warn("billing: drift pass finished", attrs...)
		return
	}
	s.Log.Info("billing: drift pass finished", attrs...)
}

// RunDrift runs one pass: reconcile the most urgent live rows, run discovery,
// prune old events, expire lapsed Wing invites, end seats the pair no longer
// supports. A subscription whose reconcile fails is logged and skipped;
// the error returned is a failed stage query or ctx ending, and the report
// still describes the work that was done. Each pass that did anything logs one
// summary line.
func (s *Service) RunDrift(ctx context.Context) (report DriftReport, err error) {
	defer func() { s.logDriftSummary(report) }()
	var errs []error
	stages := []func(context.Context, *DriftReport) error{s.driftRows, s.discover, s.pruneEvents, s.expireInvites, s.endUnsupportedSeats}
	for _, stage := range stages {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := stage(ctx, &report); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

// driftRows reconciles the selected rows and counts the corrections.
func (s *Service) driftRows(ctx context.Context, report *DriftReport) error {
	ids, err := s.queryStrings(ctx, driftSelectSQL, driftBatch)
	if err != nil {
		return withStage("drift_select", fmt.Errorf("billing: drift: select: %w", err))
	}
	report.Fetched = len(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed, err := s.Reconcile(ctx, id, "", "")
		if err != nil {
			report.Failed++
			s.Log.Warn("billing: drift could not reconcile a subscription",
				logKeySubID, id, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
			if errors.Is(err, ErrUnactionable) {
				s.touchUnactionable(ctx, id)
			}
			continue
		}
		report.Reconciled++
		if changed {
			report.Corrections++
			s.Counters.Increment(opsmetrics.MetricBillingDriftCorrectionsTotal)
			s.Log.Warn("billing: drift corrected a subscription", logKeySubID, id)
		}
	}
	return nil
}

// touchUnactionable moves updated_at of a row Reconcile could not act on, so
// it comes back through bucket 4 once a day instead of on every tick. Nothing
// else about the row changes, and a failure only costs an early revisit.
func (s *Service) touchUnactionable(ctx context.Context, subID string) {
	if _, err := s.DB.ExecContext(ctx, driftTouchSQL, subID); err != nil {
		s.Log.Warn("billing: drift could not defer an unactionable subscription",
			logKeySubID, subID, logKeyErrorClass, errorClass(err), logKeyStage, "drift_touch")
	}
}

// discover lists the subscriptions of the least recently checked mappings that
// reached Checkout recently but hold no live row, and reconciles each, so a
// subscription whose creation webhook was lost still gets its row (F12).
func (s *Service) discover(ctx context.Context, report *DriftReport) error {
	candidates, err := s.queryStrings(ctx, discoveryCandidatesSQL, discoveryCandidateLimit)
	if err != nil {
		return withStage("discovery_candidates", fmt.Errorf("billing: drift: discovery candidates: %w", err))
	}
	for _, cusID := range s.nextDiscoveryBatch(candidates) {
		if err := ctx.Err(); err != nil {
			return err
		}
		found, err := s.discoverCustomer(ctx, cusID)
		s.markDiscoveryChecked(cusID)
		report.Discovered += found
		if err != nil {
			s.Log.Warn("billing: drift discovery failed for a customer",
				logKeyCustomerID, cusID, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		}
	}
	return nil
}

// discoverCustomer lists every subscription of cusID (ended ones too, so every
// state converges) and reconciles each. It returns how many changed a row.
func (s *Service) discoverCustomer(ctx context.Context, cusID string) (changedRows int, err error) {
	params := &stripe.SubscriptionListParams{
		Customer: stripe.String(cusID),
		Status:   stripe.String("all"),
	}
	var subIDs []string
	for sub, err := range s.Stripe.V1Subscriptions.List(ctx, params).All(ctx) {
		if err != nil {
			return 0, withStage("list_subscriptions", fmt.Errorf("billing: drift: list subscriptions: %w", err))
		}
		subIDs = append(subIDs, sub.ID)
	}
	for _, subID := range subIDs {
		changed, err := s.Reconcile(ctx, subID, "", "")
		if err != nil {
			if ctx.Err() != nil {
				return changedRows, ctx.Err()
			}
			s.Log.Warn("billing: drift could not reconcile a discovered subscription",
				logKeySubID, subID, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
			continue
		}
		if changed {
			changedRows++
		}
	}
	return changedRows, nil
}

// nextDiscoveryBatch returns up to discoveryBatch of candidates, never-checked
// customers first and then the longest ago checked. Check times of customers
// that are no longer candidates are dropped, which bounds the map.
func (s *Service) nextDiscoveryBatch(candidates []string) []string {
	s.discoveryMu.Lock()
	defer s.discoveryMu.Unlock()

	kept := make(map[string]time.Time, len(candidates))
	for _, id := range candidates {
		if at, ok := s.discovery[id]; ok {
			kept[id] = at
		}
	}
	s.discovery = kept

	ordered := append([]string(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return s.discovery[ordered[i]].Before(s.discovery[ordered[j]]) // zero time sorts first
	})
	return ordered[:min(len(ordered), discoveryBatch)]
}

// markDiscoveryChecked records that cusID was just checked, whether or not the
// check succeeded, so one failing customer cannot monopolise the batch.
func (s *Service) markDiscoveryChecked(cusID string) {
	s.discoveryMu.Lock()
	defer s.discoveryMu.Unlock()
	s.discovery[cusID] = s.now()
}

// pruneEvents deletes dedupe rows older than eventRetention.
func (s *Service) pruneEvents(ctx context.Context, report *DriftReport) error {
	res, err := s.DB.ExecContext(ctx, pruneEventsSQL, eventRetention.Seconds())
	if err != nil {
		return withStage("prune_events", fmt.Errorf("billing: drift: prune events: %w", err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return withStage("prune_events", fmt.Errorf("billing: drift: prune events: %w", err))
	}
	report.Pruned = int(n)
	return nil
}

// expireInvites marks up to driftBatch lapsed pending invites expired and
// tells both sides. The pushes run after the statement's own commit.
func (s *Service) expireInvites(ctx context.Context, report *DriftReport) (err error) {
	rows, err := s.DB.QueryContext(ctx, expireInvitesSQL, driftBatch)
	if err != nil {
		return withStage("expire_invites", fmt.Errorf("billing: drift: expire invites: %w", err))
	}
	var out seatOutcome
	for rows.Next() {
		c := seatChange{state: SeatStateExpired}
		if err := rows.Scan(&c.seatID, &c.payer, &c.holder); err != nil {
			_ = rows.Close()
			return withStage("expire_invites", fmt.Errorf("billing: drift: scan expired invite: %w", err))
		}
		out.changes = append(out.changes, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return withStage("expire_invites", fmt.Errorf("billing: drift: expire invites: %w", err))
	}
	report.Expired = len(out.changes)
	s.afterSeatCommit(ctx, out)
	return nil
}

// queryStrings runs a query that selects one text column with one integer
// argument and returns the column.
func (s *Service) queryStrings(ctx context.Context, query string, limit int) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
