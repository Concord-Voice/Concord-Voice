import { vi } from 'vitest';
import React from 'react';

// ─── Mocks (before component imports) ───────────────────────────────────────

const mockSetQualityTier = vi.fn();
const mockSetAdvancedMode = vi.fn();
const mockStashAndSwap = vi.fn();

let currentTier = 'standard';

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: vi.fn((selector) =>
    selector({ qualityTier: currentTier, setQualityTier: mockSetQualityTier })
  ),
  AUDIO_QUALITY_TIERS: {
    minimum: { label: 'Minimum', maxBitrate: 16000, opusDtx: true, opusFec: true, premium: false },
    low: { label: 'Low', maxBitrate: 32000, opusDtx: true, opusFec: true, premium: false },
    moderate: {
      label: 'Moderate',
      maxBitrate: 64000,
      opusDtx: true,
      opusFec: true,
      premium: false,
    },
    standard: {
      label: 'Standard',
      maxBitrate: 96000,
      opusDtx: true,
      opusFec: true,
      premium: false,
    },
    high: { label: 'High', maxBitrate: 192000, opusDtx: false, opusFec: true, premium: true },
    hifi: { label: 'Hi-Fi', maxBitrate: 256000, opusDtx: false, opusFec: false, premium: true },
    studio: { label: 'Studio', maxBitrate: 510000, opusDtx: false, opusFec: false, premium: true },
  },
}));

vi.mock('@/renderer/stores/audioSettingsStore', () => ({
  useAudioSettingsStore: Object.assign(
    vi.fn((s) => s({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode })),
    { getState: vi.fn(() => ({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode })) }
  ),
}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn(() => false),
  setDraftAudioSetting: vi.fn(),
  batchSetAudioDrafts: vi.fn(),
  useStashAndSwapAudioMode: vi.fn(() => mockStashAndSwap),
}));

vi.mock('@/renderer/components/Settings/AudioOpusSection', () => ({
  default: () => <div data-testid="audio-opus-section" />,
}));

// Entitlement: FREE floor by default → high/hifi/studio are locked.
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return {
    allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
    ...entitlementOverrides,
  };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent } from '../../../test-utils';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import AudioConfigSection from '@/renderer/components/Settings/AudioConfigSection';

function setEntitlement(overrides: Record<string, unknown>) {
  for (const k of Object.keys(entitlementOverrides)) delete entitlementOverrides[k];
  Object.assign(entitlementOverrides, overrides);
}

function tierLabel(name: string): HTMLElement {
  return screen.getByText(name).closest('.settings-tier-label') as HTMLElement;
}

function tierSlider(): HTMLInputElement {
  return document.querySelector('.settings-tier-slider') as HTMLInputElement;
}

beforeEach(() => {
  vi.clearAllMocks();
  currentTier = 'standard';
  setEntitlement({});
  useSettingsNavStore.getState().clearFocusRequest();
});

// ─── L1: audio quality tier clamp ───────────────────────────────────────────

describe('AudioConfigSection — L1 audio tier clamp', () => {
  it('locked (free): premium tier labels (High/Hi-Fi/Studio) render the 🔒 glyph', () => {
    render(<AudioConfigSection />);
    expect(screen.getAllByLabelText('Premium feature')).toHaveLength(3);
    expect(tierLabel('High').className).toContain('settings-tier-label-locked');
    expect(tierLabel('Hi-Fi').className).toContain('settings-tier-label-locked');
    expect(tierLabel('Studio').className).toContain('settings-tier-label-locked');
  });

  it('locked (free): free tier labels are NOT locked', () => {
    render(<AudioConfigSection />);
    for (const t of ['Minimum', 'Low', 'Moderate', 'Standard']) {
      expect(tierLabel(t).className).not.toContain('settings-tier-label-locked');
    }
  });

  it('locked (free): premium tier labels stay focusable + aria-disabled, never disabled (O1)', () => {
    render(<AudioConfigSection />);
    const high = tierLabel('High');
    expect(high).toHaveAttribute('aria-disabled', 'true');
    expect(high).toHaveAttribute('tabindex', '0');
    expect(high).not.toHaveAttribute('disabled');
  });

  it('locked (free): clicking a premium label snaps back to Standard + shows the chip', () => {
    render(<AudioConfigSection />);
    fireEvent.click(tierLabel('Hi-Fi'));
    // Snap-back: store gets the highest free tier, never the premium one.
    expect(mockSetQualityTier).toHaveBeenCalledWith('standard');
    expect(mockSetQualityTier).not.toHaveBeenCalledWith('hifi');
    expect(screen.getByText(/High-fidelity tiers need a subscription/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Premium/ })).toBeInTheDocument();
  });

  it('locked (free): dragging the slider onto a premium index snaps back to Standard', () => {
    render(<AudioConfigSection />);
    // index 5 = hifi (TIER_ORDER: minimum,low,moderate,standard,high,hifi,studio)
    fireEvent.change(tierSlider(), { target: { value: '5' } });
    expect(mockSetQualityTier).toHaveBeenCalledWith('standard');
    expect(mockSetQualityTier).not.toHaveBeenCalledWith('hifi');
  });

  it('locked (free): selecting a FREE tier passes through unchanged', () => {
    render(<AudioConfigSection />);
    fireEvent.click(tierLabel('Low'));
    expect(mockSetQualityTier).toHaveBeenCalledWith('low');
  });

  it('locked (free): clicking a premium label routes to the Subscription page via the chip', () => {
    render(<AudioConfigSection />);
    fireEvent.click(tierLabel('Studio'));
    fireEvent.click(screen.getByRole('button', { name: /Premium/ }));
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });
});

describe('AudioConfigSection — L1 entitled (premium) passthrough', () => {
  it('entitled: no tier is locked and premium tiers pass through', () => {
    setEntitlement({
      allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
    });
    render(<AudioConfigSection />);
    expect(screen.queryByLabelText('Premium feature')).not.toBeInTheDocument();
    fireEvent.click(tierLabel('Studio'));
    expect(mockSetQualityTier).toHaveBeenCalledWith('studio');
  });
});
