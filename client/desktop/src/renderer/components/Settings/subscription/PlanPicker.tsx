import React, { useId, useState } from 'react';
import {
  findPlanPrice,
  useBillingStore,
  type BillingPlans,
  type PlanId,
  type PlanInterval,
} from '../../../stores/auth/billingStore';
import {
  BILLING_COPY,
  PLAN_VALUE_LINE,
  codeOverlapNote,
  formatExpiry,
  formatPrice,
  foundersTerms,
  intervalSuffix,
  planDisplayName,
} from './subscriptionCopy';

// The plan picker (#2033 PR-4, spec §13). It replaces the static PlanCatalog
// snapshot and is fed by GET /billing/plans. SubscriptionSection mounts it only
// when billing is available on this server, the shell has the billing launcher,
// the account has no live Stripe subscription, and `purchase_available` is true.
//
// Subscribing opens Stripe's hosted Checkout in the launcher sheet. Nothing here
// grants anything: the server confirms the purchase through its webhook, and the
// PlanCard shows Confirming until the subscription read says it is live.

interface PlanPickerProps {
  plans: BillingPlans;
  /** The live code row's period end: undefined without one, null when it never lapses. */
  codeExpiry?: string | null;
  /** Account trial eligibility. Only Supersonic carries the trial; Wing starts paid. */
  trialAvailable?: boolean;
}

const PLAN_ORDER: readonly PlanId[] = ['supersonic', 'wing'];

function subscribeLabel(name: string, trial: boolean): string {
  return trial ? `Start 30-day free trial of ${name}` : `Subscribe to ${name}`;
}

const PlanPicker: React.FC<PlanPickerProps> = ({ plans, codeExpiry, trialAvailable = false }) => {
  const [billingInterval, setBillingInterval] = useState<PlanInterval>('month');
  const [clicked, setClicked] = useState<PlanId | null>(null);
  const opening = useBillingStore((s) => s.opening);
  const pending = useBillingStore((s) => s.pending);
  const startCheckout = useBillingStore((s) => s.startCheckout);
  const legendId = useId();
  const busy = opening !== null || pending;

  const foundersActive = plans.founders.active;
  // The "1 month free" claim is shown only while the catalog bears it out.
  const yearSavesMonth = PLAN_ORDER.every((plan) => {
    const month = findPlanPrice(plans, plan, 'month');
    const year = findPlanPrice(plans, plan, 'year');
    return month !== undefined && year !== undefined && year.unit_amount === month.unit_amount * 11;
  });
  const foundersEnd = foundersActive ? formatExpiry(plans.founders.ends_at) : undefined;

  const subscribe = async (plan: PlanId, e: React.MouseEvent<HTMLButtonElement>): Promise<void> => {
    if (busy) return;
    const trigger = e.currentTarget;
    setClicked(plan);
    await startCheckout(plan, billingInterval);
    setClicked(null);
    // The sheet hands focus back to the window; return it to the kept-mounted trigger.
    if (trigger.isConnected) trigger.focus();
  };

  return (
    <section className="subscription-picker" aria-labelledby={legendId}>
      <h3 id={legendId} className="subscription-picker-title">
        Choose a plan
      </h3>
      <fieldset className="subscription-interval">
        <legend className="subscription-interval-legend">Billing period</legend>
        <label className="subscription-interval-option">
          <input
            type="radio"
            name="subscription-interval"
            value="month"
            checked={billingInterval === 'month'}
            onChange={() => setBillingInterval('month')}
          />
          <span>Monthly</span>
        </label>
        <label className="subscription-interval-option">
          <input
            type="radio"
            name="subscription-interval"
            value="year"
            checked={billingInterval === 'year'}
            onChange={() => setBillingInterval('year')}
          />
          {yearSavesMonth ? 'Yearly (1 month free)' : 'Yearly'}
        </label>
      </fieldset>

      <ul className="subscription-picker-grid">
        {PLAN_ORDER.map((plan) => {
          const price = findPlanPrice(plans, plan, billingInterval);
          if (!price) return null;
          const name = planDisplayName(plan) ?? plan;
          const suffix = intervalSuffix(billingInterval);
          const regular = `${formatPrice(price.unit_amount, price.currency)}${suffix}`;
          const foundersMinor = foundersActive ? price.founders_unit_amount : undefined;
          return (
            <li key={plan} className="subscription-picker-card">
              <h4 className="subscription-picker-name">{name}</h4>
              <p className="subscription-picker-value">{PLAN_VALUE_LINE[plan]}</p>
              {foundersMinor === undefined ? (
                <p className="subscription-picker-price">
                  <span className="subscription-picker-amount">{regular}</span>
                </p>
              ) : (
                <>
                  <p className="subscription-picker-price">
                    <s className="subscription-picker-regular">
                      <span className="subscription-sr-only">Regular price </span>
                      {regular}
                    </s>{' '}
                    <span className="subscription-picker-amount">
                      {formatPrice(foundersMinor, price.currency)}
                      {suffix}
                    </span>{' '}
                    <span className="subscription-founders-chip">{BILLING_COPY.foundersChip}</span>
                  </p>
                  <p className="subscription-picker-note">{BILLING_COPY.foundersDiscount}</p>
                </>
              )}
              <button
                type="button"
                className="subscription-button"
                aria-disabled={busy}
                onClick={(e) => {
                  void subscribe(plan, e);
                }}
              >
                {opening === 'checkout' && clicked === plan
                  ? BILLING_COPY.openingCheckout
                  : subscribeLabel(name, trialAvailable && plan === 'supersonic')}
              </button>
            </li>
          );
        })}
      </ul>

      {codeExpiry !== undefined && (
        <p className="subscription-picker-note">
          {codeOverlapNote(formatExpiry(codeExpiry ?? undefined), trialAvailable)}
        </p>
      )}

      <div className="subscription-terms">
        {foundersEnd && <p>{foundersTerms(foundersEnd)}</p>}
        {trialAvailable && <p>{BILLING_COPY.trialTerms}</p>}
        <p>{BILLING_COPY.renewsAutomatically}</p>
        <p>{trialAvailable ? BILLING_COPY.trialStripePage : BILLING_COPY.stripePage}</p>
      </div>
    </section>
  );
};

export default PlanPicker;
