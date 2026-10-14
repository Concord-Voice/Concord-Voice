import React from 'react';
import type { SubscriptionInfo } from '../../../hooks/ui/useSubscription';
import { useSubscriptionStore } from '../../../stores/auth/subscriptionStore';
import { tierDisplayName, sourceDisplay, formatExpiry } from './subscriptionCopy';

interface PlanCardProps {
  info: SubscriptionInfo;
}

const STATUS_DISPLAY: Record<string, string> = {
  active: 'Active',
  trialing: 'Trial',
  past_due: 'Payment past due',
  canceled: 'Canceled',
  expired: 'Expired',
};

const PlanCard: React.FC<PlanCardProps> = ({ info }) => {
  const entitlementHydrated = useSubscriptionStore((s) => s.hydrated);
  const entitlementTier = useSubscriptionStore((s) => s.entitlement.tier);
  const knownTier = info.tier === 'free' || info.tier === 'premium';
  let stateLabel: string | undefined;
  if (info.tier === 'free' && info.status === 'none') {
    stateLabel = 'Free plan';
  } else if (knownTier && Object.hasOwn(STATUS_DISPLAY, info.status)) {
    stateLabel = STATUS_DISPLAY[info.status];
  }

  if (info.loading) {
    return (
      <div className="subscription-plan-card">
        <p className="subscription-plan-blurb" aria-live="polite">
          Checking your subscription access…
        </p>
      </div>
    );
  }

  if (info.error || !stateLabel) {
    return (
      <div className="subscription-plan-card">
        <p className="subscription-plan-blurb" role={info.error ? 'alert' : undefined}>
          {info.error
            ? 'Could not check your subscription status.'
            : 'Subscription status not confirmed.'}
        </p>
        {entitlementHydrated && (
          <p className="subscription-plan-known-access">
            Last-known access: <strong>{tierDisplayName(entitlementTier)}</strong>
          </p>
        )}
        <button type="button" className="subscription-plan-retry" onClick={info.refetch}>
          Retry
        </button>
      </div>
    );
  }

  const planName = tierDisplayName(info.tier);
  const source =
    info.source === 'stripe' && info.status !== 'active'
      ? 'Stripe subscription'
      : sourceDisplay(info.source);
  const expiry = formatExpiry(info.expiry);

  return (
    <div className="subscription-plan-card" data-tier={info.tier}>
      <div className="subscription-plan-header">
        <h3 className="subscription-plan-name">{planName}</h3>
        <span className="subscription-plan-state">{stateLabel}</span>
      </div>

      <div className="subscription-plan-details">
        {source && <p className="subscription-plan-source">{source}</p>}
        {expiry && info.source && (
          <p className="subscription-plan-expiry">
            {info.source === 'stripe' ? 'Current period ends' : 'Access ends'}{' '}
            <strong>{expiry}</strong>
          </p>
        )}
        {info.tier === 'free' && info.status === 'none' && (
          <p className="subscription-plan-blurb">You&rsquo;re on Sonic, the free plan.</p>
        )}
      </div>
    </div>
  );
};

export default PlanCard;
