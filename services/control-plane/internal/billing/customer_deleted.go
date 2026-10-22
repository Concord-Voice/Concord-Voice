package billing

import (
	"context"
	"errors"
	"fmt"
)

// customer.deleted (#2033 spec §7, plan Task 4).
//
// Deleting a Stripe Customer cancels its subscriptions at once, so each local
// row of that customer is reconciled (the fetch now reads canceled) before the
// mapping is dropped. The rows are reconciled one at a time through Reconcile,
// so each takes its own advisory key and transaction and no key is ever held
// while another is taken. The mapping delete and the dedupe row share one final
// transaction that runs only after every row succeeded (F24): a retryable
// failure leaves both in place, and Stripe's retry reprocesses the whole event.

const (
	subscriptionsByCustomerSQL = `SELECT stripe_subscription_id FROM subscriptions
		WHERE stripe_customer_id = $1 AND source = 'stripe' AND stripe_subscription_id IS NOT NULL
		ORDER BY stripe_subscription_id`
	deleteMappingSQL = `DELETE FROM stripe_customers WHERE stripe_customer_id = $1`

	// Failure stages of the customer.deleted pass (logKeyStage values).
	stageListSubs      = "list_subscriptions"
	stageDeleteMapping = "delete_mapping"
)

// reconcileCustomerDeleted reconciles every stored subscription of cusID, then
// deletes cusID's mapping and records eventID in one transaction. A row whose
// reconcile is unactionable (the subscription is gone from Stripe, or its
// payer was erased) does not block the deletion; the first other error is
// returned and nothing further is written.
func (s *Service) reconcileCustomerDeleted(ctx context.Context, cusID, eventID, eventType string) error {
	subIDs, err := s.customerSubscriptionIDs(ctx, cusID)
	if err != nil {
		return err
	}
	for _, subID := range subIDs {
		if _, err := s.Reconcile(ctx, subID, "", ""); err != nil && !errors.Is(err, ErrUnactionable) {
			return fmt.Errorf("billing: customer deleted: reconcile subscription: %w", err)
		}
	}
	return s.dropCustomerMapping(ctx, cusID, eventID, eventType)
}

// customerSubscriptionIDs lists the Stripe subscription ids stored for cusID.
func (s *Service) customerSubscriptionIDs(ctx context.Context, cusID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, subscriptionsByCustomerSQL, cusID)
	if err != nil {
		return nil, withStage(stageListSubs, fmt.Errorf("billing: customer deleted: list subscriptions: %w", err))
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, withStage(stageListSubs, fmt.Errorf("billing: customer deleted: scan subscription: %w", err))
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, withStage(stageListSubs, fmt.Errorf("billing: customer deleted: list subscriptions: %w", err))
	}
	return ids, nil
}

// dropCustomerMapping deletes cusID's mapping and writes the dedupe row in one
// transaction, so the event is recorded only together with its last effect.
func (s *Service) dropCustomerMapping(ctx context.Context, cusID, eventID, eventType string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return withStage(stageBegin, fmt.Errorf("billing: customer deleted: begin: %w", err))
	}
	defer func() {
		s.logRollback(tx.Rollback(), "billing: customer deleted rollback failed", logKeyCustomerID, cusID)
	}()

	if _, err := tx.ExecContext(ctx, deleteMappingSQL, cusID); err != nil {
		return withStage(stageDeleteMapping, fmt.Errorf("billing: customer deleted: delete mapping: %w", err))
	}
	if eventID != "" {
		if err := insertDedupe(ctx, tx, eventID, eventType); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return withStage(stageCommit, fmt.Errorf("billing: customer deleted: commit: %w", err))
	}
	s.Log.Info("billing: Stripe customer deleted; mapping dropped", logKeyCustomerID, cusID)
	return nil
}
