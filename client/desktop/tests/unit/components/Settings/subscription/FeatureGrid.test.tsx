import { describe, it, expect, beforeEach } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { resetAllStores } from '../../../../helpers/store-helpers';
import {
  useSubscriptionStore,
  FREE_ENTITLEMENT,
  type Entitlement,
} from '@/renderer/stores/auth/subscriptionStore';
import FeatureGrid from '@/renderer/components/Settings/subscription/FeatureGrid';

// A premium entitlement fixture (mirrors entitlements.go premiumEntitlement for
// the fields the grid reads).
const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
  maxAttachmentBytes: 268435456,
  cameraMaxHeight: -1,
  allowAnimatedProfile: true,
  messageHistorySearchDays: 180,
  maxServersCreated: -1,
};

describe('FeatureGrid (#1304)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders every feature row locked for the free tier', () => {
    useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);
    render(<FeatureGrid />);
    const rows = screen.getAllByRole('listitem');
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row).toHaveClass('is-locked');
      expect(within(row).getByText('Supersonic')).toBeInTheDocument();
    }
  });

  it('renders every feature row granted for the premium tier', () => {
    useSubscriptionStore.getState().setEntitlement(PREMIUM_ENTITLEMENT);
    render(<FeatureGrid />);
    const rows = screen.getAllByRole('listitem');
    for (const row of rows) {
      expect(row).toHaveClass('is-granted');
      expect(within(row).getByText('Included')).toBeInTheDocument();
    }
  });

  it('grants only the rows the entitlement satisfies (mixed state)', () => {
    // Only animated profile granted; the rest stay on the free floor.
    useSubscriptionStore
      .getState()
      .setEntitlement({ ...FREE_ENTITLEMENT, allowAnimatedProfile: true });
    render(<FeatureGrid />);
    const animatedRow = screen.getByText(/Animated GIF avatar/i).closest('li');
    expect(animatedRow).toHaveClass('is-granted');
    const uploadsRow = screen.getByText(/256 MB file uploads/i).closest('li');
    expect(uploadsRow).toHaveClass('is-locked');
  });
});
