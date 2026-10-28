import React, { useEffect } from 'react';
import { ExternalLink } from 'lucide-react';
import { useSubscription } from '../../../hooks/ui/useSubscription';
import { useBillingStore } from '../../../stores/auth/billingStore';
import { isBillingLauncherAvailable } from '../../../services/system/billingLauncher';
import PlanCard from './PlanCard';
import SeatInviteCard from './SeatInviteCard';
import PlanPicker from './PlanPicker';
import FeatureGrid from './FeatureGrid';
import RedeemCodeForm from './RedeemCodeForm';
import CollapsibleSection from '../CollapsibleSection';
import RememberedDisclosure from '../RememberedDisclosure';
import SafeLink from '../../Markdown/SafeLink';
import { BILLING_COPY, BILLING_NOTICE_COPY } from './subscriptionCopy';
import './subscription.css';

// Settings ▸ Account ▸ Subscription (#1304; paid plans #2033 PR-4, spec §13).
// Section order: Wing invites (PR-5), PlanCard, then the plan picker, the benefits, the redeem form,
// and the planned-options footer. Paid controls exist only where the server
// says billing is available AND the shell has the billing launcher. With
// purchase off, the page is the pre-billing one: plan card, benefits, redeem
// and the footer.

const SubscriptionSection: React.FC = () => {
  const info = useSubscription();
  const plans = useBillingStore((s) => s.plans);
  const plansError = useBillingStore((s) => s.plansError);
  const notice = useBillingStore((s) => s.notice);
  const opening = useBillingStore((s) => s.opening);
  const fetchPlans = useBillingStore((s) => s.fetchPlans);
  const openPortal = useBillingStore((s) => s.openPortal);
  const dismissTransient = useBillingStore((s) => s.dismissTransient);

  const billingAvailable = !info.loading && !info.error && info.billing === 'available';

  useEffect(() => {
    if (billingAvailable) void fetchPlans();
  }, [billingAvailable, fetchPlans]);

  // The activation cue and a notice belong to this visit; reopening the page
  // must not replay them.
  useEffect(() => dismissTransient, [dismissTransient]);

  const bridge = isBillingLauncherAvailable();
  // A +1 on a friend's Wing cannot buy (409 seat_member); Leave Wing comes first.
  const canBuy =
    billingAvailable && bridge && info.stripe === undefined && info.seat?.role !== 'member';
  const showPicker = canBuy && plans?.purchase_available === true;
  const showPlansAlert = canBuy && plansError;
  // The footer describes paid plans as planned, so it stays only while they
  // cannot be bought: billing not configured, or purchase switched off. A
  // self-hosted server has no paid plans at all, and a failed catalog read
  // shows its own alert instead.
  const showFooter =
    info.billing === 'not_configured' ||
    (billingAvailable && !plansError && plans?.purchase_available === false);

  return (
    <div className="subscription-section">
      <CollapsibleSection id="section-subscription" title="Subscription">
        <div className="subscription-summary">
          {billingAvailable &&
            info.invites.map((invite) => (
              <SeatInviteCard
                key={invite.invite_id}
                invite={invite}
                ownPlanEnds={info.stripe?.ends_at ?? info.stripe?.current_period_end}
              />
            ))}
          <PlanCard info={info} />
          {notice &&
            (notice === 'checkout_closed' ? (
              <output className="subscription-notice">{BILLING_NOTICE_COPY[notice]}</output>
            ) : (
              <div className="subscription-notice subscription-notice-alert" role="alert">
                <span>{BILLING_NOTICE_COPY[notice]}</span>
                {notice === 'manage_existing' && billingAvailable && bridge && (
                  <button
                    type="button"
                    className="subscription-button"
                    aria-disabled={opening !== null}
                    onClick={() => {
                      void openPortal();
                    }}
                  >
                    {opening === 'portal' ? BILLING_COPY.openingPortal : 'Manage subscription'}
                  </button>
                )}
              </div>
            ))}
          {showPicker && plans && (
            <PlanPicker
              plans={plans}
              codeExpiry={info.codeExpiry}
              trialAvailable={info.trialAvailable === true}
            />
          )}
          {showPlansAlert && (
            <div className="subscription-notice subscription-notice-alert" role="alert">
              <span>{BILLING_COPY.plansUnavailable}</span>
              <button
                type="button"
                className="subscription-button"
                onClick={() => {
                  void fetchPlans();
                }}
              >
                Retry
              </button>
            </div>
          )}
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
          {showFooter && (
            <div className="subscription-planned-options">
              <p className="subscription-disclaimer">
                Paid subscriptions are planned. Options and prices may change.
              </p>
              <SafeLink href="https://concordvoice.com/compare/">
                Compare planned options
                <ExternalLink
                  size={13}
                  className="settings-external-link-icon"
                  aria-hidden="true"
                />
              </SafeLink>
            </div>
          )}
        </div>
      </CollapsibleSection>
    </div>
  );
};

export default SubscriptionSection;
