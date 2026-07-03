import React from 'react';
import type { SubscriptionInfo } from '../../../hooks/useSubscription';
import { tierDisplayName, sourceDisplay, formatExpiry } from '../subscription/subscriptionCopy';

// The current-plan summary card (#1304). Two states:
//  - FREE ("Sonic — free"): shows the locked "available at v1.0" purchase
//    placeholder (no Stripe CTA — the #2033 wire-up point).
//  - ACTIVE-from-code ("Supersonic — active" + source + optional expiry).
//
// Marketing tier names come from tierDisplayName (Sonic/Supersonic) — never the
// wire free/premium. The purchase placeholder is the #1301 lock affordance: a
// native <button aria-disabled> with an accessible label, NEVER a live Stripe
// button (Sonar S6819 + the frontend.md locked-placeholder rule).

interface PlanCardProps {
  info: SubscriptionInfo;
}

const PlanCard: React.FC<PlanCardProps> = ({ info }) => {
  const isPremium = info.tier === 'premium';
  const planName = tierDisplayName(info.tier);
  const source = sourceDisplay(info.source);
  const expiry = formatExpiry(info.expiry);

  return (
    <div className="subscription-plan-card" data-tier={info.tier}>
      <div className="subscription-plan-header">
        <span className="subscription-plan-name">{planName}</span>
        <span className="subscription-plan-state">{isPremium ? 'Active' : 'Free plan'}</span>
      </div>

      {isPremium ? (
        <div className="subscription-plan-details">
          {source && <p className="subscription-plan-source">{source}</p>}
          {expiry && (
            <p className="subscription-plan-expiry">
              Renews / expires on <strong>{expiry}</strong>
            </p>
          )}
          {!expiry && <p className="subscription-plan-expiry">No expiry — enjoy Supersonic.</p>}
        </div>
      ) : (
        <div className="subscription-plan-details">
          <p className="subscription-plan-blurb">
            You&rsquo;re on Sonic, the free plan. Redeem a code below to unlock Supersonic, or grab
            it when purchasing opens.
          </p>
          {/* Locked purchase placeholder — NOT a Stripe button. The #2033 wire-up
              point. Native <button aria-disabled> per the S6819 / frontend.md
              locked-placeholder rule (never role+tabIndex). */}
          <button
            type="button"
            className="subscription-purchase-locked"
            aria-disabled="true"
            aria-label="Upgrade to Supersonic — available at version 1.0"
          >
            Upgrade to Supersonic{' '}
            <span className="subscription-purchase-badge">Available at v1.0</span>
          </button>
        </div>
      )}
    </div>
  );
};

export default PlanCard;
