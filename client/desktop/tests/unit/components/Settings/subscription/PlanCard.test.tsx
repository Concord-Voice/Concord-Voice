import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent } from '../../../../test-utils';
import { resetAllStores } from '../../../../helpers/store-helpers';
import PlanCard from '@/renderer/components/Settings/subscription/PlanCard';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import type { SubscriptionInfo } from '@/renderer/hooks/ui/useSubscription';

function info(overrides: Partial<SubscriptionInfo> = {}): SubscriptionInfo {
  return {
    tier: 'free',
    status: 'none',
    loading: false,
    error: false,
    refetch: () => {},
    ...overrides,
  };
}

describe('PlanCard (#1304)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders the free Sonic state without an upgrade control', () => {
    render(<PlanCard info={info()} />);

    expect(screen.getByText('Sonic')).toBeInTheDocument();
    expect(screen.getByText('You\u2019re on Sonic, the free plan.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /upgrade/i })).not.toBeInTheDocument();
  });

  it('renders a code grant with neutral source copy and expiry', () => {
    render(
      <PlanCard
        info={info({
          tier: 'premium',
          status: 'active',
          source: 'code',
          expiry: '2027-01-01T00:00:00Z',
        })}
      />
    );

    expect(screen.getByText('Supersonic')).toBeInTheDocument();
    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByText('Redeemed via code')).toBeInTheDocument();
    expect(screen.getByText(/Access ends/i)).toBeInTheDocument();
    expect(screen.queryByText(/renew/i)).not.toBeInTheDocument();
  });

  it('renders a Stripe plan without inventing lifetime access', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active', source: 'stripe' })} />);

    expect(screen.getByText('Active subscription')).toBeInTheDocument();
    expect(screen.queryByText(/No expiry|lifetime|enjoy Supersonic/i)).not.toBeInTheDocument();
  });

  it('shows a reported past-due Stripe status without asserting an active subscription', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'past_due', source: 'stripe' })} />);
    expect(screen.getByText('Payment past due')).toBeInTheDocument();
    expect(screen.getByText('Stripe subscription')).toBeInTheDocument();
    expect(screen.queryByText('Active')).not.toBeInTheDocument();
    expect(screen.queryByText('Active subscription')).not.toBeInTheDocument();
  });

  it('describes a Stripe period end without promising automatic renewal', () => {
    render(
      <PlanCard
        info={info({
          tier: 'premium',
          status: 'active',
          source: 'stripe',
          expiry: '2027-01-01T00:00:00Z',
        })}
      />
    );
    expect(screen.getByText(/Current period ends/i)).toBeInTheDocument();
    expect(screen.queryByText(/renew/i)).not.toBeInTheDocument();
  });

  it.each([
    ['loading', { loading: true, status: 'loading' }],
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
    expect(screen.queryByText(/No expiry|lifetime|Renews/i)).not.toBeInTheDocument();
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

  it('never renders the raw wire tier string', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active' })} />);

    expect(screen.queryByText('premium')).not.toBeInTheDocument();
    expect(screen.queryByText('free')).not.toBeInTheDocument();
  });
});
