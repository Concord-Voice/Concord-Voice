import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
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
  it('renders the free (Sonic) state with the locked purchase placeholder', () => {
    render(<PlanCard info={info()} />);
    expect(screen.getByText('Sonic')).toBeInTheDocument();
    // Locked placeholder is a native disabled button — NOT a live CTA.
    const locked = screen.getByRole('button', {
      name: /Upgrade to Supersonic — available at version 1\.0/i,
    });
    expect(locked).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText(/Available at v1\.0/i)).toBeInTheDocument();
  });

  it('renders the active-from-code (Supersonic) state with source + expiry', () => {
    render(
      <PlanCard
        info={info({
          tier: 'premium',
          status: 'active',
          source: 'kickstarter',
          expiry: '2027-01-01T00:00:00Z',
        })}
      />
    );
    expect(screen.getByText('Supersonic')).toBeInTheDocument();
    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByText(/Redeemed via Kickstarter/i)).toBeInTheDocument();
    expect(screen.getByText(/Renews \/ expires on/i)).toBeInTheDocument();
    // The purchase placeholder is absent in the premium state.
    expect(
      screen.queryByRole('button', { name: /Upgrade to Supersonic/i })
    ).not.toBeInTheDocument();
  });

  it('renders the active premium state with no expiry line when none is present', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active', source: 'code' })} />);
    expect(screen.getByText('Supersonic')).toBeInTheDocument();
    expect(screen.getByText(/No expiry/i)).toBeInTheDocument();
  });

  it('never renders the raw wire tier string', () => {
    render(<PlanCard info={info({ tier: 'premium', status: 'active' })} />);
    expect(screen.queryByText('premium')).not.toBeInTheDocument();
    expect(screen.queryByText('free')).not.toBeInTheDocument();
  });
});
