import React from 'react';
import { ExternalLink } from 'lucide-react';
import { useSubscription } from '../../../hooks/ui/useSubscription';
import PlanCard from './PlanCard';
import FeatureGrid from './FeatureGrid';
import RedeemCodeForm from './RedeemCodeForm';
import CollapsibleSection from '../CollapsibleSection';
import RememberedDisclosure from '../RememberedDisclosure';
import SafeLink from '../../Markdown/SafeLink';
import './subscription.css';

const SubscriptionSection: React.FC = () => {
  const info = useSubscription();

  return (
    <div className="subscription-section">
      <CollapsibleSection id="section-subscription" title="Subscription">
        <div className="subscription-summary">
          <PlanCard info={info} />
          <RememberedDisclosure
            id="subscription-benefits"
            summary="View benefits"
            className="subscription-benefits"
            summaryClassName="subscription-benefits-summary"
          >
            <FeatureGrid />
          </RememberedDisclosure>
          <div className="subscription-redeem">
            <h3 className="subscription-redeem-title">Redeem a code</h3>
            <RedeemCodeForm onRedeemed={info.refetch} />
          </div>
          <div className="subscription-planned-options">
            <p className="subscription-disclaimer">
              Paid subscriptions are planned. Options and prices may change.
            </p>
            <SafeLink href="https://concordvoice.com/compare/">
              Compare planned options
              <ExternalLink size={13} className="settings-external-link-icon" aria-hidden="true" />
            </SafeLink>
          </div>
        </div>
      </CollapsibleSection>
    </div>
  );
};

export default SubscriptionSection;
