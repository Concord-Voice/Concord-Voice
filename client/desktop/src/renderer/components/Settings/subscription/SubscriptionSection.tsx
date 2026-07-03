import React from 'react';
import { useSubscription } from '../../../hooks/useSubscription';
import PlanCard from './PlanCard';
import FeatureGrid from './FeatureGrid';
import RedeemCodeForm from './RedeemCodeForm';
import KickstarterPromoCard from './KickstarterPromoCard';
import './subscription.css';

// Composes the Settings ▸ Account subscription page (#1304): current-plan card,
// redeem form, feature grid, and the Kickstarter promo. Owns the useSubscription
// status read and threads its refetch to the redeem form (so a successful redeem
// re-reads the status immediately). Beta scope: NO Stripe purchase path.
const SubscriptionSection: React.FC = () => {
  const info = useSubscription();

  return (
    <div className="subscription-section">
      <PlanCard info={info} />
      <RedeemCodeForm onRedeemed={info.refetch} />
      <FeatureGrid />
      <KickstarterPromoCard />
    </div>
  );
};

export default SubscriptionSection;
