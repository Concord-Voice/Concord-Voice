import React from 'react';
import { useSubscription } from '../../../hooks/useSubscription';
import PlanCard from './PlanCard';
import FeatureGrid from './FeatureGrid';
import RedeemCodeForm from './RedeemCodeForm';
import PlanCatalog from './PlanCatalog';
import CollapsibleSection from '../CollapsibleSection';
import './subscription.css';

const SubscriptionSection: React.FC = () => {
  const info = useSubscription();

  return (
    <div className="subscription-section">
      <p className="subscription-disclaimer">
        Subscriptions are coming in the near future to unlock the full Concord Voice experience.
        These planned options and prices may change before launch.
      </p>
      <CollapsibleSection id="section-current-plan" title="Current Plan" defaultOpen>
        <PlanCard info={info} />
        <FeatureGrid />
      </CollapsibleSection>
      <PlanCatalog />
      <CollapsibleSection id="section-redeem-code" title="Redeem a Code">
        <RedeemCodeForm onRedeemed={info.refetch} />
      </CollapsibleSection>
    </div>
  );
};

export default SubscriptionSection;
