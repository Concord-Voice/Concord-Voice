import React, { useEffect, useRef, useState } from 'react';
import {
  CalendarClock,
  CircleCheck,
  Clock,
  CreditCard,
  LoaderCircle,
  Ticket,
  Users,
  type LucideIcon,
} from 'lucide-react';
import type { SubscriptionInfo } from '../../../hooks/ui/useSubscription';
import { useSubscriptionStore } from '../../../stores/auth/subscriptionStore';
import {
  findPlanPrice,
  useBillingStore,
  type PlanChangeQuote,
  type PlanId,
  type PlanInterval,
  type PortalFlow,
  type StripeRow,
} from '../../../stores/auth/billingStore';
import { isBillingLauncherAvailable } from '../../../services/system/billingLauncher';
import ConfirmActionModal from '../../ui/ConfirmActionModal';
import WingSeatPanel, { useFriendName } from './WingSeatPanel';
import {
  BILLING_COPY,
  alsoFromCode,
  formatExpiry,
  formatPrice,
  foundersAmount,
  intervalSuffix,
  planDisplayName,
  SEAT_COPY,
  SEAT_FALLBACK_PAYER,
  seatIncludedWith,
  seatOnHold,
  seatLeaveConfirm,
  planChangeSummary,
  sourceDisplay,
  tierDisplayName,
} from './subscriptionCopy';

// The plan card at the top of Settings ▸ Account ▸ Subscription (#1304, with the
// Stripe states of #2033 PR-4, spec §13). Every state names itself with a glyph
// AND a word, never a glyph or a colour alone:
//   P0 loading · P1 free · P2 code · P3 active · P4 canceling · P5 past due ·
//   trialing ·
//   P6 confirming · P8 shell too old · P9 Wing +1 · P10 degraded read ·
//   P12 self-hosted.
// A Wing payer's +1 well (#2033 PR-5) nests below the Stripe state.
// The headline is the Stripe row when there is one, then the code; a code that
// also runs under a subscription shows as a second line.

interface PlanCardProps {
  info: SubscriptionInfo;
}

interface PlanChoice {
  sourcePlan: StripeRow['plan'];
  sourceInterval: StripeRow['interval'];
  plan: PlanId;
  interval: PlanInterval;
}

const STATUS_DISPLAY: Record<string, string> = {
  active: 'Active',
  trialing: 'Trial',
  past_due: 'Payment past due',
  canceled: 'Canceled',
  expired: 'Expired',
};

interface StateWordProps {
  icon?: LucideIcon;
  children: React.ReactNode;
}

const StateWord: React.FC<StateWordProps> = ({ icon: Icon, children }) => (
  <span className="subscription-plan-state">
    {Icon && <Icon size={16} className="subscription-plan-state-icon" aria-hidden="true" />}
    {children}
  </span>
);

interface PlanHeaderProps {
  name: string;
  children: React.ReactNode;
}

const PlanHeader: React.FC<PlanHeaderProps> = ({ name, children }) => (
  <div className="subscription-plan-header">
    <h3 className="subscription-plan-name">{name}</h3>
    {children}
  </div>
);

/** The row's own price, else the catalogue price at the row's rate. */
function planUnitAmount(
  stripe: StripeRow,
  price: ReturnType<typeof findPlanPrice>
): number | undefined {
  if (stripe.unit_amount !== undefined) return stripe.unit_amount;
  if (!price) return undefined;
  return stripe.founders_rate ? foundersAmount(price.unit_amount) : price.unit_amount;
}

function trialBlurb(
  stripe: StripeRow,
  ends: string | undefined,
  amount: string | undefined
): string {
  if (stripe.canceling) return `Your free trial ends ${ends ?? 'soon'}. You won't be charged.`;
  if (!ends) {
    return 'Your chosen plan starts billing when the trial ends unless you cancel before then. Any redeemed code credit reduces the first paid charge.';
  }
  const at = amount ? ` at ${amount}` : '';
  return `Paid billing begins on ${ends}${at} unless you cancel before then. Any redeemed code credit reduces the first paid charge.`;
}

function quoteLine(
  previewing: boolean,
  previewError: boolean,
  quote: PlanChangeQuote | null | undefined
): string {
  if (previewing) return 'Calculating your charge…';
  if (previewError) return 'Quote unavailable. Choose your plan again to refresh it.';
  if (!quote) return 'Calculating your charge…';
  if (quote.effective === 'scheduled') return 'Due now: $0.00. The new price starts at renewal.';
  return `Due now: ${formatPrice(quote.due_now, 'usd')} (Stripe invoice preview).`;
}

function cancelHint(inAppCancel: boolean, pendingPlan: boolean): string {
  if (!inAppCancel) return BILLING_COPY.manageHint;
  return pendingPlan
    ? 'Opening billing never changes your booking. Paid access lasts until the period ends if you cancel.'
    : 'Paid access lasts until the period ends if you cancel.';
}

function stripePriceLine(
  stripe: StripeRow,
  price: ReturnType<typeof findPlanPrice>,
  renews: string | undefined
): string | undefined {
  const renewal = renews ? `Renews ${renews}.` : undefined;
  if (!stripe.interval || (stripe.unit_amount === undefined && !price)) return renewal;
  const amount = planUnitAmount(stripe, price);
  if (amount === undefined) return renewal;
  const charge = `${formatPrice(amount, 'usd')}${intervalSuffix(stripe.interval)}.`;
  return renewal ? `${charge} ${renewal}` : charge;
}

/** The state word for a known tier and status, else undefined. */
function stateLabelFor(info: SubscriptionInfo): string | undefined {
  if (info.tier === 'free' && info.status === 'none') return 'Free plan';
  const knownTier = info.tier === 'free' || info.tier === 'premium';
  if (knownTier && Object.hasOwn(STATUS_DISPLAY, info.status)) return STATUS_DISPLAY[info.status];
  return undefined;
}

/** The screen-reader announcement for Confirming and for activation. */
function liveText(
  info: SubscriptionInfo,
  pending: boolean,
  pendingStalled: boolean,
  activated: boolean
): string {
  if (pending) return pendingStalled ? BILLING_COPY.confirmingStalled : BILLING_COPY.confirming;
  if (!activated) return '';
  return info.stripe?.status === 'trialing' ? BILLING_COPY.trialActivated : BILLING_COPY.activated;
}

/** The preview for the selected plan, when it still describes that change. */
function quoteForSelection(
  preview: PlanChangeQuote | null | undefined,
  stripe: StripeRow,
  plan: PlanId,
  interval: PlanInterval
): PlanChangeQuote | null {
  if (preview?.plan !== plan || preview.interval !== interval) return null;
  if (preview.effective === 'immediate') return preview;
  const atRenewal =
    stripe.current_period_end !== undefined && preview.change_at === stripe.current_period_end;
  return atRenewal ? preview : null;
}

/** The trial's price, such as "$9.99/month", when the row or catalogue knows it. */
function trialAmount(
  stripe: StripeRow,
  plans: ReturnType<typeof useBillingStore.getState>['plans']
): string | undefined {
  const price = stripe.plan
    ? findPlanPrice(plans, stripe.plan, stripe.interval ?? 'month')
    : undefined;
  const unitAmount = planUnitAmount(stripe, price);
  if (unitAmount === undefined || !stripe.interval) return undefined;
  return formatPrice(unitAmount, 'usd') + intervalSuffix(stripe.interval);
}

type PortalButton = (
  key: string,
  label: string,
  flow: PortalFlow | undefined,
  className: string,
  ref?: React.Ref<HTMLButtonElement>
) => React.ReactNode;

interface StripeStateProps {
  stripe: StripeRow;
  name: string;
  codeLine: React.ReactNode;
  shellNote: React.ReactNode;
  paidControls: boolean;
  activated: boolean;
  portalButton: PortalButton;
  manageRef: React.Ref<HTMLButtonElement>;
}

const PastDueState: React.FC<Omit<StripeStateProps, 'stripe' | 'activated'>> = ({
  name,
  codeLine,
  shellNote,
  paidControls,
  portalButton,
  manageRef,
}) => (
  <>
    <PlanHeader name={name}>
      <StateWord icon={CreditCard}>{BILLING_COPY.pastDueTitle}</StateWord>
    </PlanHeader>
    <div className="subscription-plan-details">
      <p className="subscription-plan-blurb">{BILLING_COPY.pastDueBody}</p>
      {codeLine}
      {shellNote}
    </div>
    {paidControls && (
      <div className="subscription-plan-actions">
        {portalButton(
          'update-card',
          'Update card',
          'payment_method_update',
          'subscription-button',
          manageRef
        )}
      </div>
    )}
  </>
);

const TrialState: React.FC<StripeStateProps> = ({
  stripe,
  name,
  codeLine,
  shellNote,
  paidControls,
  activated,
  portalButton,
  manageRef,
}) => {
  const plans = useBillingStore((s) => s.plans);
  const ends = formatExpiry(stripe.ends_at ?? stripe.current_period_end);
  const amount = trialAmount(stripe, plans);
  return (
    <>
      <PlanHeader name={name}>
        <StateWord icon={CalendarClock}>{ends ? `Trial ends ${ends}` : 'Free trial'}</StateWord>
      </PlanHeader>
      {activated && <span className="subscription-activation-rule" aria-hidden="true" />}
      <div className="subscription-plan-details">
        <p className="subscription-plan-blurb">{trialBlurb(stripe, ends, amount)}</p>
        {stripe.founders_rate && !stripe.canceling && (
          <p className="subscription-plan-blurb">
            {BILLING_COPY.foundersChip} applies after the trial.
          </p>
        )}
        {codeLine}
        {shellNote}
      </div>
      {paidControls && (
        <div className="subscription-plan-actions">
          {stripe.canceling
            ? portalButton(
                'resume',
                'Resume subscription',
                undefined,
                'subscription-button',
                manageRef
              )
            : portalButton(
                'manage',
                'Manage subscription',
                undefined,
                'subscription-button',
                manageRef
              )}
          {!stripe.canceling &&
            portalButton(
              'cancel',
              'Cancel subscription',
              'subscription_cancel',
              'subscription-text-button'
            )}
        </div>
      )}
    </>
  );
};

const CancelingState: React.FC<Omit<StripeStateProps, 'activated'>> = ({
  stripe,
  name,
  codeLine,
  shellNote,
  paidControls,
  portalButton,
  manageRef,
}) => {
  const ends = formatExpiry(stripe.ends_at ?? stripe.current_period_end);
  return (
    <>
      <PlanHeader name={name}>
        <StateWord icon={CalendarClock}>{ends ? `Ends ${ends}` : 'Ending'}</StateWord>
      </PlanHeader>
      <div className="subscription-plan-details">
        {ends && (
          <p className="subscription-plan-blurb">
            You keep {name} until {ends}.
          </p>
        )}
        {stripe.founders_rate && (
          <p className="subscription-plan-blurb">{BILLING_COPY.foundersEnds}</p>
        )}
        {codeLine}
        {shellNote}
      </div>
      {paidControls && (
        <div className="subscription-plan-actions">
          {portalButton(
            'resume',
            'Resume subscription',
            undefined,
            'subscription-button',
            manageRef
          )}
        </div>
      )}
    </>
  );
};

interface PlanChangeFormProps {
  stripe: StripeRow;
  currentPlan: NonNullable<StripeRow['plan']>;
  currentInterval: NonNullable<StripeRow['interval']>;
  nextPlan: PlanId;
  nextInterval: PlanInterval;
  onChoose: (plan: PlanId, interval: PlanInterval) => void;
}

const PlanChangeForm: React.FC<PlanChangeFormProps> = ({
  stripe,
  currentPlan,
  currentInterval,
  nextPlan,
  nextInterval,
  onChoose,
}) => {
  const plans = useBillingStore((s) => s.plans);
  const preview = useBillingStore((s) => s.preview);
  const previewing = useBillingStore((s) => s.previewing);
  const previewError = useBillingStore((s) => s.previewError);
  const changing = useBillingStore((s) => s.changing);
  const opening = useBillingStore((s) => s.opening);
  const changePlan = useBillingStore((s) => s.changePlan);
  const sameSelection = nextPlan === currentPlan && nextInterval === currentInterval;
  const targetPrice = findPlanPrice(plans, nextPlan, nextInterval);
  const selectedQuote = quoteForSelection(preview, stripe, nextPlan, nextInterval);
  return (
    <fieldset className="subscription-plan-change">
      <legend>Change plan</legend>
      <label>
        <span>Plan</span>
        <select value={nextPlan} onChange={(e) => onChoose(e.target.value as PlanId, nextInterval)}>
          <option value="supersonic">Supersonic</option>
          <option value="wing">Wing</option>
        </select>
      </label>
      <label>
        <span>Billing</span>
        <select
          value={nextInterval}
          onChange={(e) => onChoose(nextPlan, e.target.value as PlanInterval)}
        >
          <option value="month">Monthly</option>
          <option value="year">Yearly</option>
        </select>
      </label>
      {!sameSelection && (
        <p className="subscription-plan-blurb">
          {planChangeSummary(currentPlan, currentInterval, nextPlan, nextInterval)}
        </p>
      )}
      {!sameSelection && targetPrice && (
        <p className="subscription-plan-blurb">
          New plan price:{' '}
          {formatPrice(
            stripe.founders_rate
              ? foundersAmount(targetPrice.unit_amount)
              : targetPrice.unit_amount,
            'usd'
          )}
          {intervalSuffix(nextInterval)}
          {stripe.founders_rate ? ' with your Founders rate.' : '.'}
        </p>
      )}
      {!sameSelection && (
        <p className="subscription-plan-blurb" aria-live="polite">
          {quoteLine(previewing, previewError, selectedQuote)}
        </p>
      )}
      <button
        type="button"
        className="subscription-button"
        disabled={sameSelection || !selectedQuote || changing || opening !== null}
        onClick={() => {
          if (selectedQuote) void changePlan(nextPlan, nextInterval, selectedQuote);
        }}
      >
        {changing ? 'Saving plan change…' : 'Confirm plan change'}
      </button>
    </fieldset>
  );
};

const PendingPlanChange: React.FC<{ stripe: StripeRow }> = ({ stripe }) => {
  const changing = useBillingStore((s) => s.changing);
  const keepCurrentPlan = useBillingStore((s) => s.keepCurrentPlan);
  return (
    <div className="subscription-pending-change">
      <p className="subscription-plan-blurb">
        Changes to {planDisplayName(stripe.pending_plan)}{' '}
        {stripe.pending_interval === 'year' ? 'yearly' : 'monthly'} on{' '}
        {formatExpiry(stripe.pending_change_at)}.
      </p>
      <p className="subscription-plan-blurb">
        To choose a different plan or billing interval, keep your current plan first.
      </p>
      <button
        type="button"
        className="subscription-text-button"
        disabled={changing}
        onClick={() => {
          void keepCurrentPlan();
        }}
      >
        Keep current plan
      </button>
    </div>
  );
};

interface StripeActionsProps {
  stripe: StripeRow;
  useInAppCancel: boolean;
  portalButton: PortalButton;
  manageRef: React.Ref<HTMLButtonElement>;
}

const StripeActions: React.FC<StripeActionsProps> = ({
  stripe,
  useInAppCancel,
  portalButton,
  manageRef,
}) => {
  const changing = useBillingStore((s) => s.changing);
  const cancelSubscription = useBillingStore((s) => s.cancelSubscription);
  const [confirmCancellation, setConfirmCancellation] = useState(false);
  return (
    <>
      <div className="subscription-plan-actions">
        {useInAppCancel
          ? portalButton(
              'update-card',
              'Update card',
              'payment_method_update',
              'subscription-button',
              manageRef
            )
          : portalButton(
              'manage',
              'Manage subscription',
              undefined,
              'subscription-button',
              manageRef
            )}
        {useInAppCancel ? (
          <button
            type="button"
            className="subscription-text-button"
            disabled={changing}
            onClick={() => setConfirmCancellation(true)}
          >
            Cancel subscription
          </button>
        ) : (
          portalButton(
            'cancel',
            'Cancel subscription',
            'subscription_cancel',
            'subscription-text-button'
          )
        )}
      </div>
      {useInAppCancel && confirmCancellation && (
        <div className="subscription-cancel-confirmation">
          <p>
            {stripe.pending_plan
              ? 'Cancel at the end of your paid period? This also clears your booked plan change.'
              : 'Cancel at the end of your paid period? You’ll keep access until then.'}
          </p>
          <button
            type="button"
            className="subscription-button"
            disabled={changing}
            onClick={() => {
              void cancelSubscription();
              setConfirmCancellation(false);
            }}
          >
            Confirm cancellation
          </button>
          <button
            type="button"
            className="subscription-text-button"
            disabled={changing}
            onClick={() => setConfirmCancellation(false)}
          >
            Keep subscription
          </button>
        </div>
      )}
      <p className="subscription-plan-hint">
        {cancelHint(useInAppCancel, Boolean(stripe.pending_plan))}
      </p>
    </>
  );
};

interface ActiveStateProps extends StripeStateProps {
  periodEnd: string | undefined;
  useInAppCancel: boolean;
  nextPlan: PlanId;
  nextInterval: PlanInterval;
  onChoose: (plan: PlanId, interval: PlanInterval) => void;
}

const ActiveState: React.FC<ActiveStateProps> = ({
  stripe,
  name,
  codeLine,
  shellNote,
  paidControls,
  activated,
  portalButton,
  manageRef,
  periodEnd,
  useInAppCancel,
  nextPlan,
  nextInterval,
  onChoose,
}) => {
  const plans = useBillingStore((s) => s.plans);
  const priceLine = stripePriceLine(
    stripe,
    stripe.plan ? findPlanPrice(plans, stripe.plan, stripe.interval ?? 'month') : undefined,
    periodEnd
  );
  const pendingChange = stripe.pending_plan && stripe.pending_interval && stripe.pending_change_at;
  const canChange = paidControls && !stripe.pending_plan && plans;
  return (
    <>
      <PlanHeader name={name}>
        <StateWord icon={CircleCheck}>Active</StateWord>
      </PlanHeader>
      {activated && <span className="subscription-activation-rule" aria-hidden="true" />}
      <div className="subscription-plan-details">
        {(priceLine || stripe.founders_rate) && (
          <p className="subscription-plan-blurb">
            {priceLine}
            {stripe.founders_rate && (
              <span className="subscription-founders-chip">{BILLING_COPY.foundersChip}</span>
            )}
          </p>
        )}
        {codeLine}
        {pendingChange && <PendingPlanChange stripe={stripe} />}
        {shellNote}
      </div>
      {canChange && stripe.plan && stripe.interval && (
        <PlanChangeForm
          stripe={stripe}
          currentPlan={stripe.plan}
          currentInterval={stripe.interval}
          nextPlan={nextPlan}
          nextInterval={nextInterval}
          onChoose={onChoose}
        />
      )}
      {paidControls && (
        <StripeActions
          stripe={stripe}
          useInAppCancel={useInAppCancel}
          portalButton={portalButton}
          manageRef={manageRef}
        />
      )}
    </>
  );
};

const ConfirmingHeader: React.FC<{ name: string }> = ({ name }) => (
  <PlanHeader name={name}>
    <span className="subscription-plan-state">
      <LoaderCircle
        size={16}
        className="subscription-plan-state-icon subscription-confirming-spinner"
        aria-hidden="true"
      />
      <Clock
        size={16}
        className="subscription-plan-state-icon subscription-confirming-still"
        aria-hidden="true"
      />
      Confirming
    </span>
  </PlanHeader>
);

const UnconfirmedState: React.FC<{ info: SubscriptionInfo }> = ({ info }) => {
  const entitlementHydrated = useSubscriptionStore((s) => s.hydrated);
  const entitlementTier = useSubscriptionStore((s) => s.entitlement.tier);
  return (
    <>
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
    </>
  );
};

const SeatMemberState: React.FC<{ info: SubscriptionInfo; suspended: boolean }> = ({
  info,
  suspended,
}) => {
  const leaveSeat = useBillingStore((s) => s.leaveSeat);
  const [leaving, setLeaving] = useState(false);
  const seatPayerName = useFriendName(info.seat?.counterpart_user_id, SEAT_FALLBACK_PAYER);
  return (
    <>
      <PlanHeader name={tierDisplayName(suspended ? info.tier : 'premium')}>
        <StateWord icon={Users}>
          {suspended ? seatOnHold(seatPayerName) : seatIncludedWith(seatPayerName)}
        </StateWord>
      </PlanHeader>
      <div className="subscription-plan-details">
        {info.codeExpiry !== undefined && (
          <p className="subscription-plan-blurb">
            {alsoFromCode(formatExpiry(info.codeExpiry ?? undefined))}
          </p>
        )}
      </div>
      <div className="subscription-plan-actions">
        <button type="button" className="subscription-button" onClick={() => setLeaving(true)}>
          {SEAT_COPY.leave}
        </button>
      </div>
      <ConfirmActionModal
        isOpen={leaving}
        onClose={() => setLeaving(false)}
        title={SEAT_COPY.leave}
        message={seatLeaveConfirm(seatPayerName)}
        confirmLabel={SEAT_COPY.leave}
        loadingLabel="Leaving…"
        onConfirm={async () => {
          const result = await leaveSeat();
          if (!result.ok && result.error !== 'no_seat') throw new Error(SEAT_COPY.actionFailed);
        }}
      />
    </>
  );
};

const CodeState: React.FC<{ info: SubscriptionInfo; shellNote: React.ReactNode }> = ({
  info,
  shellNote,
}) => {
  const expiry = formatExpiry(info.expiry);
  return (
    <>
      <PlanHeader name={tierDisplayName(info.tier)}>
        <StateWord icon={Ticket}>{sourceDisplay('code')}</StateWord>
      </PlanHeader>
      <div className="subscription-plan-details">
        {expiry && (
          <p className="subscription-plan-expiry">
            Access ends <strong>{expiry}</strong>
          </p>
        )}
        {shellNote}
      </div>
    </>
  );
};

const FreeState: React.FC<{
  info: SubscriptionInfo;
  stateLabel: string;
  shellNote: React.ReactNode;
}> = ({ info, stateLabel, shellNote }) => (
  <>
    <PlanHeader name={tierDisplayName(info.tier)}>
      <StateWord>{stateLabel}</StateWord>
    </PlanHeader>
    <div className="subscription-plan-details">
      <p className="subscription-plan-blurb">You&rsquo;re on Sonic, the free plan.</p>
      {shellNote}
    </div>
  </>
);

// A status this card has no dedicated state for, such as a Stripe source from a
// server that sends no `stripe` detail: name it without inventing renewal or
// management.
const OtherState: React.FC<{ info: SubscriptionInfo; stateLabel: string }> = ({
  info,
  stateLabel,
}) => {
  const source =
    info.source === 'stripe' && info.status !== 'active'
      ? 'Stripe subscription'
      : sourceDisplay(info.source);
  const expiry = formatExpiry(info.expiry);
  return (
    <>
      <PlanHeader name={tierDisplayName(info.tier)}>
        <StateWord>{stateLabel}</StateWord>
      </PlanHeader>
      <div className="subscription-plan-details">
        {source && <p className="subscription-plan-source">{source}</p>}
        {expiry && info.source && (
          <p className="subscription-plan-expiry">
            {info.source === 'stripe' ? 'Current period ends' : 'Access ends'}{' '}
            <strong>{expiry}</strong>
          </p>
        )}
      </div>
    </>
  );
};

const PlanCard: React.FC<PlanCardProps> = ({ info }) => {
  const pending = useBillingStore((s) => s.pending);
  const pendingStalled = useBillingStore((s) => s.pendingStalled);
  const activated = useBillingStore((s) => s.activated);
  const opening = useBillingStore((s) => s.opening);
  const plans = useBillingStore((s) => s.plans);
  const openPortal = useBillingStore((s) => s.openPortal);
  const previewPlanChange = useBillingStore((s) => s.previewPlanChange);
  const clearPlanPreview = useBillingStore((s) => s.clearPlanPreview);
  const checkAgain = useBillingStore((s) => s.checkAgain);
  const manageRef = useRef<HTMLButtonElement>(null);
  const [clicked, setClicked] = useState<string | null>(null);
  const [choice, setChoice] = useState<PlanChoice | null>(null);

  // When Confirming ends, the picker unmounts. Focus that was inside it falls to
  // <body>; move it to Manage. Focus the user put anywhere else stays put.
  useEffect(() => {
    if (!activated) return;
    const active = document.activeElement;
    if (active === null || active === document.body) manageRef.current?.focus();
  }, [activated]);

  const bridge = isBillingLauncherAvailable();
  const paidControls = info.billing === 'available' && bridge;
  const useInAppCancel = Boolean(info.stripe?.pending_plan || info.stripe?.schedule_attached);
  const currentPlan = info.stripe?.plan ?? null;
  const currentInterval = info.stripe?.interval ?? null;
  const choiceApplies =
    choice?.sourcePlan === currentPlan && choice.sourceInterval === currentInterval;
  const nextPlan = choiceApplies ? choice.plan : (currentPlan ?? 'supersonic');
  const nextInterval = choiceApplies ? choice.interval : (currentInterval ?? 'month');
  const previewWanted =
    paidControls &&
    !info.stripe?.pending_plan &&
    currentPlan !== null &&
    currentInterval !== null &&
    (nextPlan !== currentPlan || nextInterval !== currentInterval);
  useEffect(() => {
    if (previewWanted) void previewPlanChange(nextPlan, nextInterval);
    else clearPlanPreview();
  }, [
    previewWanted,
    info.stripe?.current_period_end,
    nextPlan,
    nextInterval,
    previewPlanChange,
    clearPlanPreview,
  ]);
  const shellTooOld =
    info.billing === 'available' &&
    !bridge &&
    (info.stripe !== undefined || plans?.purchase_available === true);

  const launchPortal = async (
    key: string,
    flow: PortalFlow | undefined,
    e: React.MouseEvent<HTMLButtonElement>
  ): Promise<void> => {
    if (opening !== null) return;
    const trigger = e.currentTarget;
    setClicked(key);
    await openPortal(flow);
    setClicked(null);
    // The sheet hands focus back to the window; put it back on the trigger.
    if (trigger.isConnected) trigger.focus();
  };

  const portalButton: PortalButton = (key, label, flow, className, ref) => (
    <button
      type="button"
      ref={ref}
      className={className}
      aria-disabled={opening !== null}
      onClick={(e) => {
        void launchPortal(key, flow, e);
      }}
    >
      {opening === 'portal' && clicked === key ? BILLING_COPY.openingPortal : label}
    </button>
  );

  const stateLabel = stateLabelFor(info);
  const shellNote = shellTooOld ? (
    <p className="subscription-plan-blurb">{BILLING_COPY.shellTooOld}</p>
  ) : null;

  const renderStripe = (stripe: StripeRow): React.ReactNode => {
    const props: StripeStateProps = {
      stripe,
      name: planDisplayName(stripe.plan) ?? tierDisplayName(info.tier),
      codeLine:
        info.codeExpiry === undefined ? null : (
          <p className="subscription-plan-blurb">
            {alsoFromCode(formatExpiry(info.codeExpiry ?? undefined))}
          </p>
        ),
      shellNote,
      paidControls,
      activated,
      portalButton,
      manageRef,
    };
    if (stripe.status === 'past_due') return <PastDueState {...props} />;
    if (stripe.status === 'trialing') return <TrialState {...props} />;
    if (stripe.canceling) return <CancelingState {...props} />;
    return (
      <ActiveState
        {...props}
        periodEnd={formatExpiry(stripe.current_period_end)}
        useInAppCancel={useInAppCancel}
        nextPlan={nextPlan}
        nextInterval={nextInterval}
        onChoose={(plan, interval) => {
          if (currentPlan && currentInterval) {
            setChoice({ sourcePlan: currentPlan, sourceInterval: currentInterval, plan, interval });
          }
        }}
      />
    );
  };

  const renderBody = (): React.ReactNode => {
    if (info.loading) {
      return (
        <p className="subscription-plan-blurb" aria-live="polite">
          Checking your subscription access…
        </p>
      );
    }
    if (pending) return <ConfirmingHeader name={tierDisplayName(info.tier)} />;
    if (info.error || !stateLabel) return <UnconfirmedState info={info} />;
    if (info.billing === 'self_hosted') {
      return <p className="subscription-plan-blurb">{BILLING_COPY.selfHosted}</p>;
    }
    if (info.stripe) return renderStripe(info.stripe);
    if (info.seat?.role === 'member') {
      return <SeatMemberState info={info} suspended={Boolean(info.seat.suspended)} />;
    }
    if (info.tier === 'premium' && info.source === 'code') {
      return <CodeState info={info} shellNote={shellNote} />;
    }
    if (info.tier === 'free' && info.status === 'none') {
      return <FreeState info={info} stateLabel={stateLabel} shellNote={shellNote} />;
    }
    return <OtherState info={info} stateLabel={stateLabel} />;
  };

  const pastDue = !pending && info.stripe?.status === 'past_due' && !info.error;
  const showSeatWell = !pending && info.stripe && info.seat?.role === 'payer';

  return (
    <div
      className={
        pastDue ? 'subscription-plan-card subscription-plan-card-alert' : 'subscription-plan-card'
      }
      data-tier={info.loading ? undefined : info.tier}
    >
      {renderBody()}
      {showSeatWell && info.seat && info.billing === 'available' && (
        <WingSeatPanel seat={info.seat} />
      )}
      {/* Always mounted, so the change from Confirming to Active is announced. */}
      <output className={pending ? 'subscription-plan-live' : 'subscription-sr-only'}>
        {liveText(info, pending, pendingStalled, activated)}
      </output>
      {/* After the output, so Check again follows the message it answers. */}
      {pending && pendingStalled && (
        <div className="subscription-plan-actions">
          <button
            type="button"
            className="subscription-button"
            onClick={() => {
              void checkAgain();
            }}
          >
            {BILLING_COPY.checkAgain}
          </button>
        </div>
      )}
    </div>
  );
};

export default PlanCard;
