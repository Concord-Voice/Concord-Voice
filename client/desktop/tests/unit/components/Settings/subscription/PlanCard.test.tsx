import { beforeEach, describe, expect, it } from 'vitest';
import { render, screen } from '../../../../test-utils';
import { resetAllStores } from '../../../../helpers/store-helpers';
import PlanCard from '@/renderer/components/Settings/subscription/PlanCard';
import type { SubscriptionInfo } from '@/renderer/hooks/useSubscription';

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
    expect(screen.getByText(/Renews \/ expires on/i)).toBeInTheDocument();
  });

  it('renders a Stripe plan with neutral active-subscription copy', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active', source: 'stripe' })} />);

    expect(screen.getByText('Active subscription')).toBeInTheDocument();
    expect(screen.getByText(/No expiry/i)).toBeInTheDocument();
  });

  it('never renders the raw wire tier string', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active' })} />);

    expect(screen.queryByText('premium')).not.toBeInTheDocument();
    expect(screen.queryByText('free')).not.toBeInTheDocument();
  });
});
