import { vi } from 'vitest';

// ── Mocks (before any component imports) ────────────────────────────────

const defaultSettings: Record<string, unknown> = {
  musicMode: false,
  frameSize: 20,
  silenceDetection: true,
  inlineFec: true,
  fecHeadroom: false,
  opusNack: false,
  adaptivePtime: false,
  audioPriority: 'medium',
  stereoOverride: null,
};

const mockSetDraftAudioSetting = vi.fn();

vi.mock('@/renderer/stores/voiceStore', () => ({
  AUDIO_QUALITY_TIERS: {
    standard: {
      preferredFrameSize: 20,
      opusStereo: false,
      label: 'Standard',
      maxBitrate: 96000,
      opusDtx: true,
      opusFec: true,
      premium: false,
    },
  },
}));

vi.mock('@/renderer/stores/audioSettingsStore', () => ({}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn((key: string) => defaultSettings[key] ?? false),
  setDraftAudioSetting: (...args: unknown[]) => mockSetDraftAudioSetting(...args),
}));

// Real CustomSelect (a native <select>) so option labels carrying the lock
// marker are inspectable, and onChange snap-back can be exercised via change.

// Entitlement: default to the FREE floor (music mode off, minPtime 20). Tests
// flip to premium where passthrough must be asserted.
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return { allowMusicMode: false, minPtimeMs: 20, ...entitlementOverrides };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ── Imports (after mocks) ───────────────────────────────────────────────

import { render, screen, fireEvent } from '../../../test-utils';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import AudioOpusSection from '@/renderer/components/Settings/AudioOpusSection';

function setEntitlement(overrides: Record<string, unknown>) {
  for (const k of Object.keys(entitlementOverrides)) delete entitlementOverrides[k];
  Object.assign(entitlementOverrides, overrides);
}

function frameSizeSelect(): HTMLSelectElement {
  // First custom-select in the section is Frame Size (ptime).
  return document.querySelectorAll('select.settings-select')[0] as HTMLSelectElement;
}

beforeEach(() => {
  vi.clearAllMocks();
  setEntitlement({});
  useSettingsNavStore.getState().clearFocusRequest();
});

// ── L3: Music Mode binary lock (dim gate) ───────────────────────────────

describe('AudioOpusSection — L3 Music Mode lock', () => {
  it('locked (free): renders the 🔒 + "Premium" affordance', () => {
    render(<AudioOpusSection qualityTier="standard" />);
    expect(screen.getByText('Premium')).toBeInTheDocument();
    expect(screen.getByLabelText('Premium feature')).toBeInTheDocument();
  });

  it('locked (free): the Music Mode toggle stays focusable, never disabled (O1)', () => {
    const { container } = render(<AudioOpusSection qualityTier="standard" />);
    const gate = container.querySelector('.premium-gate') as HTMLElement;
    // After the S6819 fix the wrapper is a plain container — no role/tabindex/
    // aria-disabled on it; the CONTROL (the toggle's input) carries the ARIA.
    expect(gate).not.toHaveAttribute('role');
    expect(gate).not.toHaveAttribute('tabindex');
    expect(gate).not.toHaveAttribute('aria-disabled');
    const musicRow = screen.getByText('Music Mode').closest('.settings-row')!;
    const checkbox = musicRow.querySelector('input[type="checkbox"]') as HTMLInputElement;
    // The wrapped toggle checkbox stays focusable: aria-disabled (NOT the HTML
    // disabled attribute) marks it dormant, and aria-describedby points at the chip.
    expect(checkbox).not.toBeDisabled();
    expect(checkbox).toHaveAttribute('aria-disabled', 'true');
    expect(checkbox).toHaveAttribute('aria-describedby');
    expect(gate.style.pointerEvents).not.toBe('none');
  });

  it('locked (free): clicking the toggle routes to Subscription, never toggles music mode', () => {
    const musicRow = render(<AudioOpusSection qualityTier="standard" />);
    void musicRow;
    const row = screen.getByText('Music Mode').closest('.settings-row')!;
    const checkbox = row.querySelector('input[type="checkbox"]') as HTMLInputElement;
    fireEvent.click(checkbox);
    expect(mockSetDraftAudioSetting).not.toHaveBeenCalledWith('musicMode', expect.anything());
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('entitled (premium): toggle behaves natively — no chip, mutates the store', () => {
    setEntitlement({ allowMusicMode: true });
    render(<AudioOpusSection qualityTier="standard" />);
    // No lock affordance when entitled.
    const row = screen.getByText('Music Mode').closest('.settings-row')!;
    expect(row.querySelector('.premium-gate')).toBeNull();
    const checkbox = row.querySelector('input[type="checkbox"]') as HTMLInputElement;
    fireEvent.click(checkbox);
    expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('musicMode', true);
  });
});

// ── L2: Frame Size (ptime) premium options + snap-back ───────────────────

describe('AudioOpusSection — L2 ptime option lock', () => {
  it('locked (free): the 10 ms option carries the lock marker in its label', () => {
    render(<AudioOpusSection qualityTier="standard" />);
    const tenMs = screen.getByRole('option', { name: /10 ms/ }) as HTMLOptionElement;
    expect(tenMs.textContent).toContain('Premium');
    expect(tenMs.textContent).toContain('\u{1F512}');
  });

  it('locked (free): 20 ms / 40 ms / 60 ms options are NOT marked premium', () => {
    render(<AudioOpusSection qualityTier="standard" />);
    expect(
      (screen.getByRole('option', { name: '20 ms' }) as HTMLOptionElement).textContent
    ).not.toContain('Premium');
    expect(
      (screen.getByRole('option', { name: /40 ms/ }) as HTMLOptionElement).textContent
    ).not.toContain('Premium');
  });

  it('locked (free): selecting the 10 ms option snaps back to 20 ms and shows the chip', () => {
    render(<AudioOpusSection qualityTier="standard" />);
    fireEvent.change(frameSizeSelect(), { target: { value: '10' } });
    // Snap-back: store receives the highest free value, not 10.
    expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('frameSize', 20);
    expect(mockSetDraftAudioSetting).not.toHaveBeenCalledWith('frameSize', 10);
    // The snap-back chip (interactive button) now renders. Target it by its
    // distinctive "lower latency" label — the music-mode PremiumGate also renders
    // an interactive chip button (post-S6819 fix) which would otherwise match /Premium/.
    expect(screen.getByRole('button', { name: /lower latency/ })).toBeInTheDocument();
  });

  it('locked (free): selecting a FREE option (40 ms) passes through unchanged', () => {
    render(<AudioOpusSection qualityTier="standard" />);
    fireEvent.change(frameSizeSelect(), { target: { value: '40' } });
    expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('frameSize', 40);
  });

  it('entitled (premium minPtime 10): the 10 ms option is NOT marked premium and passes through', () => {
    setEntitlement({ minPtimeMs: 10 });
    render(<AudioOpusSection qualityTier="standard" />);
    const tenMs = screen.getByRole('option', {
      name: '10 ms (lowest latency)',
    }) as HTMLOptionElement;
    expect(tenMs.textContent).not.toContain('Premium');
    fireEvent.change(frameSizeSelect(), { target: { value: '10' } });
    expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('frameSize', 10);
  });
});
