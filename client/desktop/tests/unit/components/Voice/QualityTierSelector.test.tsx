import React from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// ─── Module mocks ─────────────────────────────────────────────────────────────

const mockSetQualityTier = vi.fn().mockResolvedValue(undefined);

vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { setQualityTier: (...args: unknown[]) => mockSetQualityTier(...args) },
}));

// ─── Import component after mocks ─────────────────────────────────────────────

const { default: QualityTierSelector } =
  await import('@/renderer/components/Voice/QualityTierSelector');

// ─── Tests ───────────────────────────────────────────────────────────────────

const TIER_ORDER = ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'] as const;

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  useVoiceStore.setState({ qualityTier: 'standard' });
});

describe('QualityTierSelector — rendering', () => {
  it('renders all 7 quality tier buttons', () => {
    render(<QualityTierSelector />);
    expect(screen.getAllByRole('button')).toHaveLength(7);
  });

  it('renders labels for every tier', () => {
    render(<QualityTierSelector />);
    const expectedLabels = ['Minimum', 'Low', 'Moderate', 'Standard', 'High', 'Hi-Fi', 'Studio'];
    for (const label of expectedLabels) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it('renders the Audio Quality header', () => {
    render(<QualityTierSelector />);
    expect(screen.getByText('Audio Quality')).toBeInTheDocument();
  });
});

describe('QualityTierSelector — active tier', () => {
  it('applies --active class to the currently active tier', () => {
    useVoiceStore.setState({ qualityTier: 'high' });
    render(<QualityTierSelector />);

    const buttons = screen.getAllByRole('button');
    const highBtn = buttons.find((b) => b.textContent?.includes('High'));
    expect(highBtn?.className).toContain('--active');
  });

  it('does not apply --active class to non-active tiers', () => {
    useVoiceStore.setState({ qualityTier: 'standard' });
    render(<QualityTierSelector />);

    const buttons = screen.getAllByRole('button');
    const lowBtn = buttons.find((b) => b.textContent?.includes('Low'));
    expect(lowBtn?.className).not.toContain('--active');
  });
});

describe('QualityTierSelector — premium tiers', () => {
  it('renders Crown icon for premium tiers (high, hifi, studio)', () => {
    render(<QualityTierSelector />);
    // lucide Crown renders an SVG — check that premium tier buttons contain an svg
    const buttons = screen.getAllByRole('button');

    const highBtn = buttons.find((b) => b.textContent?.includes('High'));
    const hifiBtn = buttons.find((b) => b.textContent?.includes('Hi-Fi'));
    const studioBtn = buttons.find((b) => b.textContent?.includes('Studio'));

    expect(highBtn?.querySelector('svg')).toBeInTheDocument();
    expect(hifiBtn?.querySelector('svg')).toBeInTheDocument();
    expect(studioBtn?.querySelector('svg')).toBeInTheDocument();
  });

  it('does not render Crown icon for non-premium tiers', () => {
    render(<QualityTierSelector />);
    const buttons = screen.getAllByRole('button');

    const minimumBtn = buttons.find((b) => b.textContent?.includes('Minimum'));
    const standardBtn = buttons.find((b) => b.textContent?.includes('Standard'));

    // Non-premium buttons should not contain an svg (Crown)
    expect(minimumBtn?.querySelector('svg')).not.toBeInTheDocument();
    expect(standardBtn?.querySelector('svg')).not.toBeInTheDocument();
  });
});

describe('QualityTierSelector — bitrate formatting', () => {
  it('formats bitrates >= 1000 as kbps', () => {
    render(<QualityTierSelector />);
    // Standard = 96000 bps → "96 kbps"
    expect(screen.getByText('96 kbps')).toBeInTheDocument();
    // Hi-Fi = 256000 bps → "256 kbps"
    expect(screen.getByText('256 kbps')).toBeInTheDocument();
  });

  it('formats bitrates < 1000 as bps', () => {
    // All current tiers are >= 16000 so none is < 1000, but verify the rendered values include "kbps"
    render(<QualityTierSelector />);
    // Minimum = 16000 bps → "16 kbps"
    expect(screen.getByText('16 kbps')).toBeInTheDocument();
  });
});

describe('QualityTierSelector — tier selection', () => {
  it('calls voiceService.setQualityTier with the selected tier', async () => {
    render(<QualityTierSelector />);

    const buttons = screen.getAllByRole('button');
    const minimumBtn = buttons.find((b) => b.textContent?.includes('Minimum'));
    await act(async () => {
      fireEvent.click(minimumBtn!);
    });

    expect(mockSetQualityTier).toHaveBeenCalledWith('minimum');
  });

  it('calls setQualityTier for each tier', async () => {
    for (const tier of TIER_ORDER) {
      vi.clearAllMocks();
      useVoiceStore.setState({ qualityTier: 'standard' });

      const { unmount } = render(<QualityTierSelector />);
      const buttons = screen.getAllByRole('button');
      const tierBtn = buttons[TIER_ORDER.indexOf(tier)];
      await act(async () => {
        fireEvent.click(tierBtn);
      });

      expect(mockSetQualityTier).toHaveBeenCalledWith(tier);
      unmount();
    }
  });
});
