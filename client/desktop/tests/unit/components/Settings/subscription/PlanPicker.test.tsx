import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent, within } from '../../../../test-utils';
import { resetAllStores } from '../../../../helpers/store-helpers';
import PlanPicker from '@/renderer/components/Settings/subscription/PlanPicker';
import {
  formatExpiry,
  formatPrice,
} from '@/renderer/components/Settings/subscription/subscriptionCopy';
import { useBillingStore, type BillingPlans } from '@/renderer/stores/auth/billingStore';

const ORIGINAL = useBillingStore.getState();
const FOUNDERS_END = '2027-05-01T12:00:00Z';

function catalog(founders: boolean): BillingPlans {
  const f = (amount: number) => (founders ? { founders_unit_amount: Math.floor(amount / 2) } : {});
  return {
    purchase_available: true,
    founders: founders ? { active: true, ends_at: FOUNDERS_END } : { active: false },
    plans: [
      { plan: 'supersonic', interval: 'month', currency: 'usd', unit_amount: 899, ...f(899) },
      { plan: 'supersonic', interval: 'year', currency: 'usd', unit_amount: 9889, ...f(9889) },
      { plan: 'wing', interval: 'month', currency: 'usd', unit_amount: 1499, ...f(1499) },
      { plan: 'wing', interval: 'year', currency: 'usd', unit_amount: 16489, ...f(16489) },
    ],
  };
}

function card(name: string): HTMLElement {
  const heading = screen.getByRole('heading', { name });
  const item = heading.closest('li');
  if (!item) throw new Error(`no card for ${name}`);
  return item;
}

describe('PlanPicker (#2033 PR-4)', () => {
  let startCheckout: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetAllStores();
    startCheckout = vi.fn().mockResolvedValue('cancel');
    useBillingStore.setState({ startCheckout });
  });

  afterEach(() => {
    useBillingStore.setState({ startCheckout: ORIGINAL.startCheckout });
  });

  it('offers Monthly and Yearly as native radios, defaulting to Monthly', () => {
    render(<PlanPicker plans={catalog(false)} />);
    const group = screen.getByRole('group', { name: 'Billing period' });
    expect(within(group).getByRole('radio', { name: 'Monthly' })).toBeChecked();
    expect(within(group).getByRole('radio', { name: 'Yearly (1 month free)' })).not.toBeChecked();
  });

  it('drops the "1 month free" claim when the catalog does not bear it out', () => {
    const plans = catalog(false);
    plans.plans[1] = { ...plans.plans[1], unit_amount: 9999 };
    render(<PlanPicker plans={plans} />);
    expect(screen.getByRole('radio', { name: 'Yearly' })).toBeInTheDocument();
  });

  it('shows both plans with their value lines and regular prices', async () => {
    render(<PlanPicker plans={catalog(false)} />);
    expect(
      within(card('Supersonic')).getByText('All Supersonic features for your account.')
    ).toBeInTheDocument();
    expect(
      within(card('Wing')).getByText('Supersonic for you and one friend you choose.')
    ).toBeInTheDocument();
    expect(
      within(card('Supersonic')).getByText(`${formatPrice(899, 'usd')}/month`)
    ).toBeInTheDocument();
    expect(screen.queryByText('Founders rate')).not.toBeInTheDocument();
    expect(screen.queryByText(/Founders rate: 50% off/)).not.toBeInTheDocument();

    await userEvent.setup().click(screen.getByRole('radio', { name: 'Yearly (1 month free)' }));
    expect(within(card('Wing')).getByText(`${formatPrice(16489, 'usd')}/year`)).toBeInTheDocument();
  });

  it('strikes the regular price under founders pricing and states the terms', () => {
    render(<PlanPicker plans={catalog(true)} />);
    const supersonic = card('Supersonic');
    const regular = supersonic.querySelector('s');
    expect(regular).toHaveTextContent(`Regular price ${formatPrice(899, 'usd')}/month`);
    expect(within(regular as HTMLElement).getByText('Regular price')).toHaveClass(
      'subscription-sr-only'
    );
    expect(within(supersonic).getByText(`${formatPrice(449, 'usd')}/month`)).toBeInTheDocument();
    expect(within(supersonic).getByText('Founders rate')).toBeInTheDocument();
    expect(
      within(supersonic).getByText('50% off while you stay subscribed. Canceling ends it.')
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        `Founders rate: 50% off for as long as you stay subscribed. If you cancel, you lose it. Available for subscriptions started before ${formatExpiry(FOUNDERS_END)}.`
      )
    ).toBeInTheDocument();
    expect(screen.getByText('Renews automatically until you cancel.')).toBeInTheDocument();
    expect(
      screen.getByText("You'll pay on Stripe's secure page. Concord never sees your card.")
    ).toBeInTheDocument();
  });

  it('notes a code that already covers the account', () => {
    render(<PlanPicker plans={catalog(false)} codeExpiry="2027-03-15T12:00:00Z" />);
    expect(
      screen.getByText(
        `Your code already covers Supersonic until ${formatExpiry('2027-03-15T12:00:00Z')}. Subscribing starts billing today.`
      )
    ).toBeInTheDocument();
  });

  it('offers an eligible account a 30-day Supersonic trial with card collection and conversion terms', async () => {
    render(<PlanPicker plans={catalog(false)} trialAvailable codeExpiry="2027-03-15T12:00:00Z" />);
    expect(screen.getByText(/Supersonic comes with a 30-day free trial/)).toHaveTextContent(
      'Supersonic comes with a 30-day free trial. It starts billing when the trial ends unless you cancel before then. Any redeemed code credit reduces the first paid charge. Wing has no trial and starts billing today.'
    );
    expect(screen.getByText(/enter your card on Stripe/)).toBeInTheDocument();
    expect(screen.getByText(/Your code already covers/)).toHaveTextContent(
      'A Supersonic trial starts today; paid billing begins when it ends unless you cancel. Wing starts billing today.'
    );
    expect(screen.queryByText(/Subscribing starts billing today/)).not.toBeInTheDocument();
    // Wing never carries the trial (D6): it keeps the paid label.
    expect(screen.queryByRole('button', { name: /free trial of Wing/ })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Subscribe to Wing' })).toBeInTheDocument();
    await userEvent
      .setup()
      .click(screen.getByRole('button', { name: 'Start 30-day free trial of Supersonic' }));
    expect(startCheckout).toHaveBeenCalledWith('supersonic', 'month');
  });

  it('notes a never-lapsing code without a date, and nothing without a code', () => {
    const { rerender } = render(<PlanPicker plans={catalog(false)} codeExpiry={null} />);
    expect(
      screen.getByText('Your code already covers Supersonic. Subscribing starts billing today.')
    ).toBeInTheDocument();
    rerender(<PlanPicker plans={catalog(false)} />);
    expect(screen.queryByText(/Your code already covers/)).not.toBeInTheDocument();
  });

  it('starts Checkout for the chosen plan and interval and returns focus to the trigger', async () => {
    render(<PlanPicker plans={catalog(false)} />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('radio', { name: 'Yearly (1 month free)' }));
    const trigger = screen.getByRole('button', { name: 'Subscribe to Wing' });
    await user.click(trigger);
    expect(startCheckout).toHaveBeenCalledWith('wing', 'year');
    expect(trigger).toHaveFocus();
  });

  it('marks the triggers aria-disabled and labels the clicked one while Checkout opens', async () => {
    let finish: () => void = () => {};
    startCheckout.mockImplementation(() => {
      useBillingStore.setState({ opening: 'checkout' });
      return new Promise((resolve) => {
        finish = () => {
          useBillingStore.setState({ opening: null });
          resolve('success');
        };
      });
    });
    render(<PlanPicker plans={catalog(false)} />);
    const user = userEvent.setup();
    const trigger = screen.getByRole('button', { name: 'Subscribe to Supersonic' });
    await user.click(trigger);
    expect(trigger).toHaveTextContent('Opening secure checkout…');
    expect(trigger).toHaveAttribute('aria-disabled', 'true');
    const wing = screen.getByRole('button', { name: 'Subscribe to Wing' });
    expect(wing).toHaveAttribute('aria-disabled', 'true');
    await user.click(wing);
    expect(startCheckout).toHaveBeenCalledTimes(1);
    await act(async () => finish());
    expect(trigger).toHaveTextContent('Subscribe to Supersonic');
    expect(trigger).toHaveFocus();
  });

  it('keeps the triggers aria-disabled while a purchase is Confirming', async () => {
    useBillingStore.setState({ pending: true });
    render(<PlanPicker plans={catalog(false)} />);
    const trigger = screen.getByRole('button', { name: 'Subscribe to Supersonic' });
    expect(trigger).toHaveAttribute('aria-disabled', 'true');
    await userEvent.setup().click(trigger);
    expect(startCheckout).not.toHaveBeenCalled();
  });
});
