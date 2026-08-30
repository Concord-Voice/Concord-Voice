import React from 'react';
import type { SubscriptionInfo } from '../../../hooks/ui/useSubscription';
import { tierDisplayName, sourceDisplay, formatExpiry } from '../subscription/subscriptionCopy';

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
          <p className="subscription-plan-blurb">You&rsquo;re on Sonic, the free plan.</p>
        </div>
      )}
    </div>
  );
};

export default PlanCard;
