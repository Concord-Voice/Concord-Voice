import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent } from '../../../../test-utils';
import { resetAllStores } from '../../../../helpers/store-helpers';
import PlanCard from '@/renderer/components/Settings/subscription/PlanCard';
import {
  formatExpiry,
  formatPrice,
} from '@/renderer/components/Settings/subscription/subscriptionCopy';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import { useBillingStore, type StripeRow } from '@/renderer/stores/auth/billingStore';
import type { SubscriptionInfo } from '@/renderer/hooks/ui/useSubscription';

const PERIOD_END = '2027-01-15T12:00:00Z';
const ENDS_AT = '2027-02-15T12:00:00Z';
const ORIGINAL = useBillingStore.getState();

type Bridge = { openBilling?: unknown };
const bridge = (): Bridge => globalThis.electron as unknown as Bridge;

function info(overrides: Partial<SubscriptionInfo> = {}): SubscriptionInfo {
  return {
    tier: 'free',
    status: 'none',
    billing: 'not_configured',
    loading: false,
    error: false,
    refetch: () => {},
    ...overrides,
  };
}

function stripeRow(overrides: Partial<StripeRow> = {}): StripeRow {
  return {
    plan: 'supersonic',
    interval: 'month',
    status: 'active',
    current_period_end: PERIOD_END,
    canceling: false,
    founders_rate: false,
    ...overrides,
  };
}

function subscriber(stripe: Partial<StripeRow> = {}, rest: Partial<SubscriptionInfo> = {}) {
  return info({
    tier: 'premium',
    status: 'active',
    source: 'stripe',
    expiry: PERIOD_END,
    billing: 'available',
    stripe: stripeRow(stripe),
    ...rest,
  });
}

const CATALOG = {
  purchase_available: true,
  founders: { active: true, ends_at: '2027-05-01T12:00:00Z' },
  plans: [
    {
      plan: 'supersonic' as const,
      interval: 'month' as const,
      currency: 'usd' as const,
      unit_amount: 899,
      founders_unit_amount: 449,
    },
    {
      plan: 'supersonic' as const,
      interval: 'year' as const,
      currency: 'usd' as const,
      unit_amount: 9889,
    },
    {
      plan: 'wing' as const,
      interval: 'month' as const,
      currency: 'usd' as const,
      unit_amount: 1499,
    },
    {
      plan: 'wing' as const,
      interval: 'year' as const,
      currency: 'usd' as const,
      unit_amount: 16489,
    },
  ],
};

describe('PlanCard (#1304, #2033 PR-4)', () => {
  let openPortal: ReturnType<typeof vi.fn>;
  let checkAgain: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetAllStores();
    openPortal = vi.fn().mockResolvedValue('returned');
    checkAgain = vi.fn().mockResolvedValue(undefined);
    useBillingStore.setState({ openPortal, checkAgain });
  });

  afterEach(() => {
    useBillingStore.setState({ openPortal: ORIGINAL.openPortal, checkAgain: ORIGINAL.checkAgain });
    delete bridge().openBilling;
  });

  describe('without paid controls', () => {
    it('P0: shows the polite loading line', () => {
      render(<PlanCard info={info({ loading: true, status: 'unknown' })} />);
      expect(screen.getByText('Checking your subscription access…')).toHaveAttribute(
        'aria-live',
        'polite'
      );
    });

    it('P1: renders the free Sonic state without an upgrade control', () => {
      render(<PlanCard info={info()} />);
      expect(screen.getByRole('heading', { name: 'Sonic' })).toBeInTheDocument();
      expect(screen.getByText('Free plan')).toBeInTheDocument();
      expect(screen.getByText('You’re on Sonic, the free plan.')).toBeInTheDocument();
      expect(screen.queryByRole('button')).not.toBeInTheDocument();
    });

    it('P2: names a code grant with the ticket glyph and its end date', () => {
      const { container } = render(
        <PlanCard
          info={info({ tier: 'premium', status: 'active', source: 'code', expiry: PERIOD_END })}
        />
      );
      expect(screen.getByRole('heading', { name: 'Supersonic' })).toBeInTheDocument();
      expect(screen.getByText('Redeemed via code')).toBeInTheDocument();
      expect(container.querySelector('.lucide-ticket')).not.toBeNull();
      expect(screen.getByText(/Access ends/i)).toHaveTextContent(
        `Access ends ${formatExpiry(PERIOD_END)}`
      );
      expect(screen.queryByText(/renew/i)).not.toBeInTheDocument();
    });

    it('P12: a self-hosted server provides premium features and offers nothing to buy', () => {
      render(
        <PlanCard info={info({ billing: 'self_hosted', tier: 'premium', status: 'active' })} />
      );
      expect(screen.getByText('Your server provides premium features.')).toBeInTheDocument();
      expect(screen.queryByRole('button')).not.toBeInTheDocument();
    });

    it('hides every paid control when billing is not configured', () => {
      bridge().openBilling = vi.fn();
      render(<PlanCard info={subscriber({}, { billing: 'not_configured' })} />);
      expect(screen.getByText('Active')).toBeInTheDocument();
      expect(screen.queryByRole('button')).not.toBeInTheDocument();
      expect(screen.queryByText(/Update Concord/)).not.toBeInTheDocument();
    });

    it('names a Stripe source from a server that sends no Stripe detail without inventing renewal', () => {
      render(<PlanCard info={info({ tier: 'premium', status: 'past_due', source: 'stripe' })} />);
      expect(screen.getByText('Payment past due')).toBeInTheDocument();
      expect(screen.getByText('Stripe subscription')).toBeInTheDocument();
      expect(screen.queryByText(/renew/i)).not.toBeInTheDocument();
    });

    it('describes a legacy Stripe period end without promising renewal', () => {
      render(
        <PlanCard
          info={info({ tier: 'premium', status: 'active', source: 'stripe', expiry: PERIOD_END })}
        />
      );
      expect(screen.getByText('Active subscription')).toBeInTheDocument();
      expect(screen.getByText(/Current period ends/i)).toBeInTheDocument();
      expect(screen.queryByText(/renew/i)).not.toBeInTheDocument();
    });
  });

  describe('P8: a shell without the billing launcher', () => {
    it('asks a subscriber to update Concord and offers no control', () => {
      render(<PlanCard info={subscriber()} />);
      expect(
        screen.getByText('Update Concord to subscribe or manage billing.')
      ).toBeInTheDocument();
      expect(screen.queryByRole('button')).not.toBeInTheDocument();
    });

    it('asks a free user to update Concord only while purchase is on', () => {
      const free = info({ billing: 'available' });
      const { rerender } = render(<PlanCard info={free} />);
      expect(screen.queryByText(/Update Concord/)).not.toBeInTheDocument();
      act(() => useBillingStore.setState({ plans: CATALOG }));
      rerender(<PlanCard info={free} />);
      expect(
        screen.getByText('Update Concord to subscribe or manage billing.')
      ).toBeInTheDocument();
    });
  });

  describe('with the billing launcher', () => {
    beforeEach(() => {
      bridge().openBilling = vi.fn();
    });

    it('shows the trial end and first paid charge, with a way to cancel', async () => {
      useBillingStore.setState({ plans: CATALOG });
      const { container } = render(
        <PlanCard info={subscriber({ status: 'trialing', founders_rate: true, plan: 'wing' })} />
      );
      expect(screen.getByRole('heading', { name: 'Wing' })).toBeInTheDocument();
      expect(screen.getByText(`Trial ends ${formatExpiry(PERIOD_END)}`)).toBeInTheDocument();
      expect(container.querySelector('.lucide-calendar-clock')).not.toBeNull();
      expect(screen.getByText(/Paid billing begins/)).toHaveTextContent(
        `Paid billing begins on ${formatExpiry(PERIOD_END)} at ${formatPrice(749, 'usd')}/month unless you cancel before then. Any redeemed code credit reduces the first paid charge.`
      );
      await userEvent.setup().click(screen.getByRole('button', { name: 'Cancel subscription' }));
      expect(openPortal).toHaveBeenCalledWith('subscription_cancel');
    });

    it('shows a canceled trial without promising a charge', async () => {
      render(<PlanCard info={subscriber({ status: 'trialing', canceling: true })} />);
      expect(screen.getByText(/You won't be charged/)).toHaveTextContent(
        `Your free trial ends ${formatExpiry(PERIOD_END)}. You won't be charged.`
      );
      expect(screen.queryByRole('button', { name: 'Cancel subscription' })).not.toBeInTheDocument();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Resume subscription' }));
      expect(openPortal).toHaveBeenCalledWith(undefined);
    });

    it('uses the subscriber price for the first paid charge during a trial', () => {
      useBillingStore.setState({ plans: CATALOG });
      render(
        <PlanCard
          info={subscriber({
            status: 'trialing',
            founders_rate: true,
            plan: 'wing',
            unit_amount: 681,
          })}
        />
      );
      expect(screen.getByText(/Paid billing begins/)).toHaveTextContent('at $6.81/month');
    });

    it('P3: shows the plan, the founders price, the renewal and both Portal actions', async () => {
      useBillingStore.setState({ plans: CATALOG });
      const { container } = render(<PlanCard info={subscriber({ founders_rate: true })} />);
      expect(screen.getByRole('heading', { name: 'Supersonic' })).toBeInTheDocument();
      expect(screen.getByText('Active')).toBeInTheDocument();
      expect(container.querySelector('.lucide-circle-check')).not.toBeNull();
      expect(
        screen.getByText(`${formatPrice(449, 'usd')}/month. Renews ${formatExpiry(PERIOD_END)}.`, {
          exact: false,
        })
      ).toBeInTheDocument();
      expect(screen.getByText('Founders rate')).toBeInTheDocument();
      expect(
        screen.getByText('Change plan, update your card, or see invoices.')
      ).toBeInTheDocument();

      const user = userEvent.setup();
      await user.click(screen.getByRole('button', { name: 'Manage subscription' }));
      expect(openPortal).toHaveBeenLastCalledWith(undefined);
      await user.click(screen.getByRole('button', { name: 'Cancel subscription' }));
      expect(openPortal).toHaveBeenLastCalledWith('subscription_cancel');
    });

    it('P3: names Wing and shows a regular price yearly', () => {
      useBillingStore.setState({ plans: CATALOG });
      render(<PlanCard info={subscriber({ plan: 'wing', interval: 'year' })} />);
      expect(screen.getByRole('heading', { name: 'Wing' })).toBeInTheDocument();
      expect(screen.getByRole('combobox', { name: 'Plan' })).toHaveValue('wing');
      expect(screen.getByRole('combobox', { name: 'Billing' })).toHaveValue('year');
      expect(
        screen.getByText(`${formatPrice(16489, 'usd')}/year. Renews ${formatExpiry(PERIOD_END)}.`)
      ).toBeInTheDocument();
      expect(screen.queryByText('Founders rate')).not.toBeInTheDocument();
    });

    it('uses the subscriber amount even when the catalog has a different price', () => {
      useBillingStore.setState({ plans: CATALOG });
      render(<PlanCard info={subscriber({ unit_amount: 437, founders_rate: true })} />);
      expect(
        screen.getByText(`${formatPrice(437, 'usd')}/month. Renews ${formatExpiry(PERIOD_END)}.`, {
          exact: false,
        })
      ).toBeInTheDocument();
    });

    it('shows the subscriber amount without a catalog', () => {
      render(<PlanCard info={subscriber({ unit_amount: 525 })} />);
      expect(
        screen.getByText(`${formatPrice(525, 'usd')}/month. Renews ${formatExpiry(PERIOD_END)}.`, {
          exact: false,
        })
      ).toBeInTheDocument();
    });

    it('requires withdrawing a booked change before choosing another', async () => {
      const keepCurrentPlan = vi.fn().mockResolvedValue(undefined);
      const previewPlanChange = vi.fn();
      useBillingStore.setState({ plans: CATALOG, keepCurrentPlan, previewPlanChange });
      render(
        <PlanCard
          info={subscriber({
            plan: 'wing',
            interval: 'year',
            pending_plan: 'supersonic',
            pending_interval: 'month',
            pending_change_at: PERIOD_END,
          })}
        />
      );
      expect(
        screen.getByText(`Changes to Supersonic monthly on ${formatExpiry(PERIOD_END)}.`)
      ).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Manage subscription' })).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Update card' })).toBeInTheDocument();
      expect(
        screen.getByText(
          'To choose a different plan or billing interval, keep your current plan first.'
        )
      ).toBeInTheDocument();
      expect(screen.queryByRole('combobox', { name: 'Plan' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Confirm plan change' })).not.toBeInTheDocument();
      expect(previewPlanChange).not.toHaveBeenCalled();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Keep current plan' }));
      expect(keepCurrentPlan).toHaveBeenCalledOnce();
    });

    it('keeps a booked plan and backs out of cancellation without changing it', async () => {
      const keepCurrentPlan = vi.fn().mockResolvedValue(undefined);
      const cancelSubscription = vi.fn().mockResolvedValue(undefined);
      useBillingStore.setState({ plans: CATALOG, keepCurrentPlan, cancelSubscription });
      render(
        <PlanCard
          info={subscriber({
            pending_plan: 'wing',
            pending_interval: 'month',
            pending_change_at: PERIOD_END,
          })}
        />
      );
      await userEvent.setup().click(screen.getByRole('button', { name: 'Cancel subscription' }));
      expect(screen.getByRole('button', { name: 'Confirm cancellation' })).toBeInTheDocument();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Keep subscription' }));
      expect(cancelSubscription).not.toHaveBeenCalled();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Keep current plan' }));
      expect(keepCurrentPlan).toHaveBeenCalledOnce();
      expect(openPortal).not.toHaveBeenCalled();
    });

    it('cancels in-app after a booked change starts while its schedule remains attached', async () => {
      const cancelSubscription = vi.fn().mockResolvedValue(undefined);
      useBillingStore.setState({ cancelSubscription });
      render(
        <PlanCard
          info={subscriber({
            plan: 'supersonic',
            interval: 'year',
            schedule_attached: true,
          })}
        />
      );
      expect(screen.getByRole('button', { name: 'Update card' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Keep current plan' })).not.toBeInTheDocument();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Cancel subscription' }));
      expect(
        screen.getByText('Cancel at the end of your paid period? You’ll keep access until then.')
      ).toBeInTheDocument();
      expect(openPortal).not.toHaveBeenCalled();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Confirm cancellation' }));
      expect(cancelSubscription).toHaveBeenCalledOnce();
      expect(openPortal).not.toHaveBeenCalled();
    });

    it('shows the exact previewed charge before an immediate confirmation', async () => {
      const quote = {
        plan: 'wing' as const,
        interval: 'month' as const,
        effective: 'immediate' as const,
        due_now: 537,
        currency: 'usd' as const,
        proration_date: 1800000000,
      };
      const previewPlanChange = vi.fn().mockImplementation(async () => {
        useBillingStore.setState({ preview: quote, previewing: false });
      });
      useBillingStore.setState({ plans: CATALOG, previewPlanChange });
      render(<PlanCard info={subscriber()} />);
      const confirm = screen.getByRole('button', { name: 'Confirm plan change' });
      expect(confirm).toBeDisabled();
      await userEvent.setup().selectOptions(screen.getByRole('combobox', { name: 'Plan' }), 'wing');
      await vi.waitFor(() =>
        expect(screen.getByText('Due now: $5.37 (Stripe invoice preview).')).toBeInTheDocument()
      );
      expect(confirm).toBeEnabled();
    });

    it('expires a scheduled quote when the paid period changes', async () => {
      const quote = {
        plan: 'supersonic' as const,
        interval: 'month' as const,
        effective: 'scheduled' as const,
        due_now: 0,
        currency: 'usd' as const,
        change_at: PERIOD_END,
      };
      const previewPlanChange = vi.fn().mockImplementation(async () => {
        useBillingStore.setState({ preview: quote, previewing: false });
      });
      useBillingStore.setState({ plans: CATALOG, previewPlanChange });
      const view = render(<PlanCard info={subscriber({ plan: 'wing', interval: 'month' })} />);
      await userEvent
        .setup()
        .selectOptions(screen.getByRole('combobox', { name: 'Plan' }), 'supersonic');
      const confirm = screen.getByRole('button', { name: 'Confirm plan change' });
      await vi.waitFor(() => expect(confirm).toBeEnabled());
      view.rerender(
        <PlanCard
          info={subscriber({
            plan: 'wing',
            interval: 'month',
            current_period_end: '2027-02-15T12:00:00Z',
          })}
        />
      );
      expect(confirm).toBeDisabled();
      expect(previewPlanChange).toHaveBeenCalledTimes(2);
    });

    it('P3: falls back to the tier name and the renewal date without a known price', () => {
      render(<PlanCard info={subscriber({ plan: null, interval: null })} />);
      expect(screen.getByRole('heading', { name: 'Supersonic' })).toBeInTheDocument();
      expect(screen.getByText(`Renews ${formatExpiry(PERIOD_END)}.`)).toBeInTheDocument();
    });

    it('P3: shows a code that also runs as a second line', () => {
      render(<PlanCard info={subscriber({}, { codeExpiry: ENDS_AT })} />);
      expect(
        screen.getByText(`Also Supersonic from a code until ${formatExpiry(ENDS_AT)}.`)
      ).toBeInTheDocument();
    });

    it('P3: a never-lapsing code reads without a date', () => {
      render(<PlanCard info={subscriber({}, { codeExpiry: null })} />);
      expect(screen.getByText('Also Supersonic from a code.')).toBeInTheDocument();
    });

    it('marks the trigger aria-disabled and says it is opening while the Portal loads', async () => {
      let finish: (value: null) => void = () => {};
      openPortal.mockImplementation(() => {
        useBillingStore.setState({ opening: 'portal' });
        return new Promise<null>((resolve) => {
          finish = (value) => {
            useBillingStore.setState({ opening: null });
            resolve(value);
          };
        });
      });
      render(<PlanCard info={subscriber()} />);
      const manage = screen.getByRole('button', { name: 'Manage subscription' });
      await userEvent.setup().click(manage);
      expect(manage).toHaveTextContent('Opening billing…');
      expect(manage).toHaveAttribute('aria-disabled', 'true');
      expect(screen.getByRole('button', { name: 'Cancel subscription' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await userEvent.setup().click(screen.getByRole('button', { name: 'Cancel subscription' }));
      expect(openPortal).toHaveBeenCalledTimes(1);
      await act(async () => finish(null));
      expect(manage).toHaveTextContent('Manage subscription');
      expect(manage).toHaveFocus();
    });

    it('P4: a canceling plan says when it ends and offers Resume', async () => {
      const { container } = render(
        <PlanCard info={subscriber({ canceling: true, ends_at: ENDS_AT, founders_rate: true })} />
      );
      const ends = formatExpiry(ENDS_AT);
      expect(screen.getByText(`Ends ${ends}`)).toBeInTheDocument();
      expect(container.querySelector('.lucide-calendar-clock')).not.toBeNull();
      expect(screen.getByText(`You keep Supersonic until ${ends}.`)).toBeInTheDocument();
      expect(screen.getByText('Your Founders rate ends with it.')).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Cancel subscription' })).not.toBeInTheDocument();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Resume subscription' }));
      expect(openPortal).toHaveBeenCalledWith(undefined);
    });

    it('P4: a period-end cancel without ends_at uses the period end', () => {
      render(<PlanCard info={subscriber({ canceling: true })} />);
      expect(screen.getByText(`Ends ${formatExpiry(PERIOD_END)}`)).toBeInTheDocument();
      expect(screen.queryByText(/Founders rate ends/)).not.toBeInTheDocument();
    });

    it('P5: past due keeps access, says so, and offers Update card', async () => {
      const { container } = render(<PlanCard info={subscriber({ status: 'past_due' })} />);
      expect(screen.getByText("Payment didn't go through")).toBeInTheDocument();
      expect(
        screen.getByText(
          "We'll keep trying, and you keep Supersonic meanwhile. Update your card to keep it."
        )
      ).toBeInTheDocument();
      expect(container.querySelector('.lucide-credit-card')).not.toBeNull();
      expect(container.querySelector('.lucide-triangle-alert')).toBeNull();
      expect(container.querySelector('.subscription-plan-card-alert')).not.toBeNull();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Update card' }));
      expect(openPortal).toHaveBeenCalledWith('payment_method_update');
    });
  });

  describe('P6: Confirming', () => {
    beforeEach(() => {
      bridge().openBilling = vi.fn();
    });

    it('shows the spinner and its reduced-motion clock with an always-mounted live output', () => {
      useBillingStore.setState({ pending: true });
      const { container } = render(<PlanCard info={info({ billing: 'available' })} />);
      expect(screen.getByText('Confirming')).toBeInTheDocument();
      expect(container.querySelector('.subscription-confirming-spinner')).not.toBeNull();
      expect(container.querySelector('.subscription-confirming-still')).not.toBeNull();
      expect(screen.getByRole('status')).toHaveTextContent('Confirming your subscription…');
      expect(screen.queryByRole('button', { name: 'Check again' })).not.toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('never shows an error while Confirming, even if a read failed', () => {
      useBillingStore.setState({ pending: true });
      render(<PlanCard info={info({ error: true, status: 'unknown', billing: 'available' })} />);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.getByText('Confirming')).toBeInTheDocument();
    });

    it('after the stall, explains and offers Check again', async () => {
      useBillingStore.setState({ pending: true, pendingStalled: true });
      render(<PlanCard info={info({ billing: 'available' })} />);
      expect(screen.getByRole('status')).toHaveTextContent(
        "We haven't received confirmation yet. If you were charged, your subscription will appear here shortly. You don't need to pay again."
      );
      await userEvent.setup().click(screen.getByRole('button', { name: 'Check again' }));
      expect(checkAgain).toHaveBeenCalledTimes(1);
    });

    it('places Check again after the message it answers', () => {
      useBillingStore.setState({ pending: true, pendingStalled: true });
      render(<PlanCard info={info({ billing: 'available' })} />);
      const message = screen.getByRole('status');
      const button = screen.getByRole('button', { name: 'Check again' });
      expect(
        message.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING
      ).toBeTruthy();
    });

    it('announces activation once, draws the rule, and moves focus from the body to Manage', () => {
      useBillingStore.setState({ pending: true });
      const { container, rerender } = render(<PlanCard info={info({ billing: 'available' })} />);
      const output = screen.getByRole('status');
      (document.activeElement as HTMLElement | null)?.blur();
      // The store commits the live read and the exit in one write.
      act(() => {
        useBillingStore.setState({ pending: false, activated: true });
        rerender(<PlanCard info={subscriber()} />);
      });
      expect(screen.getByRole('status')).toBe(output);
      expect(output).toHaveTextContent('Your subscription is active.');
      expect(container.querySelector('.subscription-activation-rule')).not.toBeNull();
      expect(screen.getByRole('button', { name: 'Manage subscription' })).toHaveFocus();
    });

    it('leaves focus the user placed elsewhere', () => {
      useBillingStore.setState({ pending: true });
      const { rerender } = render(
        <>
          <input aria-label="elsewhere" />
          <PlanCard info={info({ billing: 'available' })} />
        </>
      );
      const elsewhere = screen.getByRole('textbox', { name: 'elsewhere' });
      elsewhere.focus();
      act(() => {
        useBillingStore.setState({ pending: false, activated: true });
        rerender(
          <>
            <input aria-label="elsewhere" />
            <PlanCard info={subscriber()} />
          </>
        );
      });
      expect(elsewhere).toHaveFocus();
    });
  });

  describe('P10: a degraded read', () => {
    it.each([
      ['failed read', { error: true, status: 'unknown' }],
      ['unknown status', { status: 'unknown' }],
      ['unknown tier', { tier: 'future-tier' }],
    ])('does not assert confirmed access during %s', (_label, state) => {
      render(<PlanCard info={info(state)} />);
      expect(screen.queryByText('Active')).not.toBeInTheDocument();
      expect(screen.queryByText('Free plan')).not.toBeInTheDocument();
      expect(
        screen.queryByText(/on Sonic, the free plan|No expiry|lifetime|Renews/i)
      ).not.toBeInTheDocument();
    });

    it('labels server-confirmed cached entitlement separately when the status read fails', () => {
      useSubscriptionStore.setState({
        hydrated: true,
        entitlement: { ...useSubscriptionStore.getState().entitlement, tier: 'premium' },
      });
      render(<PlanCard info={info({ tier: 'premium', status: 'unknown', error: true })} />);
      expect(screen.getByText(/Last-known access:/i)).toHaveTextContent('Supersonic');
      expect(screen.queryByText('Active')).not.toBeInTheDocument();
    });

    it('does not describe an unhydrated free floor as last-known confirmed access', () => {
      render(<PlanCard info={info({ status: 'unknown', error: true })} />);
      expect(screen.getByRole('alert')).toHaveTextContent(
        'Could not check your subscription status.'
      );
      expect(screen.queryByText(/Last-known access:/i)).not.toBeInTheDocument();
    });

    it('offers retry after a failed read', async () => {
      const refetch = vi.fn();
      render(<PlanCard info={info({ error: true, status: 'unknown', refetch })} />);
      await userEvent.setup().click(screen.getByRole('button', { name: /retry/i }));
      expect(refetch).toHaveBeenCalledTimes(1);
    });
  });

  it('never renders the raw wire tier string', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active' })} />);
    expect(screen.queryByText('premium')).not.toBeInTheDocument();
    expect(screen.queryByText('free')).not.toBeInTheDocument();
  });
});
