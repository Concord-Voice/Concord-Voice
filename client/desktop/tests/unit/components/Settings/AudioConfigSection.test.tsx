import { vi } from 'vitest';
import React from 'react';

// ─── Mock setup (BEFORE component imports) ──────────────────────────────────

const mockSetQualityTier = vi.fn();
const mockSetAdvancedMode = vi.fn();
const mockStashAndSwap = vi.fn();

const defaultAudioSettings: Record<string, unknown> = {
  noiseCancellation: true,
  echoCancellation: true,
  autoGainControl: true,
  noiseGateMode: 'auto',
  noiseGateLevel: -50,
  quietBoost: false,
  quietBoostThreshold: -38,
  musicMode: false,
};

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: vi.fn((selector) =>
    selector({
      qualityTier: 'standard' as const,
      setQualityTier: mockSetQualityTier,
    })
  ),
  AUDIO_QUALITY_TIERS: {
    minimum: {
      label: 'Minimum',
      maxBitrate: 16000,
      opusDtx: true,
      opusFec: true,
      preferredFrameSize: 60,
      premium: false,
    },
    low: {
      label: 'Low',
      maxBitrate: 32000,
      opusDtx: true,
      opusFec: true,
      preferredFrameSize: 40,
      premium: false,
    },
    moderate: {
      label: 'Moderate',
      maxBitrate: 64000,
      opusDtx: true,
      opusFec: true,
      preferredFrameSize: 20,
      premium: false,
    },
    standard: {
      label: 'Standard',
      maxBitrate: 96000,
      opusDtx: true,
      opusFec: true,
      preferredFrameSize: 20,
      premium: false,
    },
    high: {
      label: 'High',
      maxBitrate: 192000,
      opusDtx: false,
      opusFec: true,
      preferredFrameSize: 10,
      premium: false,
    },
    hifi: {
      label: 'Hi-Fi',
      maxBitrate: 256000,
      opusDtx: false,
      opusFec: false,
      preferredFrameSize: 10,
      premium: true,
    },
    studio: {
      label: 'Studio',
      maxBitrate: 510000,
      opusDtx: false,
      opusFec: false,
      preferredFrameSize: 10,
      premium: true,
    },
  },
}));

vi.mock('@/renderer/stores/audioSettingsStore', () => ({
  useAudioSettingsStore: Object.assign(
    vi.fn((s) => s({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode })),
    { getState: vi.fn(() => ({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode })) }
  ),
}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn((key: string) => defaultAudioSettings[key] ?? false),
  setDraftAudioSetting: vi.fn(),
  batchSetAudioDrafts: vi.fn(),
  useStashAndSwapAudioMode: vi.fn(() => mockStashAndSwap),
}));

vi.mock('@/renderer/components/Settings/AudioOpusSection', () => ({
  default: ({ qualityTier }: { qualityTier: string }) => (
    <div data-testid="audio-opus-section" data-tier={qualityTier}>
      AudioOpusSection
    </div>
  ),
}));

// ─── Component import (AFTER mocks) ────────────────────────────────────────

import { render, screen, fireEvent } from '../../../test-utils';
import AudioConfigSection from '@/renderer/components/Settings/AudioConfigSection';

// ─── Helpers ────────────────────────────────────────────────────────────────

/** Override useDraftAudioSetting with custom settings for a single test. */
async function overrideDraftSettings(overrides: Record<string, unknown>) {
  const merged = { ...defaultAudioSettings, ...overrides };
  const { useDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
  (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
    (key: string) => merged[key] ?? false
  );
}

/** Switch the audioSettingsStore mock to advanced mode. */
async function enableAdvancedMode() {
  const { useAudioSettingsStore } = await import('@/renderer/stores/audioSettingsStore');
  (useAudioSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) =>
      s({ advancedMode: true, setAdvancedMode: mockSetAdvancedMode })
  );
  (
    useAudioSettingsStore as unknown as { getState: ReturnType<typeof vi.fn> }
  ).getState.mockReturnValue({ advancedMode: true, setAdvancedMode: mockSetAdvancedMode });
}

/** Override useVoiceStore to return a different tier. */
async function overrideTier(tier: string) {
  const { useVoiceStore } = await import('@/renderer/stores/voiceStore');
  (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) =>
      s({ qualityTier: tier, setQualityTier: mockSetQualityTier })
  );
}

// ─── Tests ──────────────────────────────────────────────────────────────────

describe('AudioConfigSection', () => {
  beforeEach(async () => {
    vi.clearAllMocks();

    // Re-apply default mock implementations (clearAllMocks wipes mockImplementation)
    const { useVoiceStore } = await import('@/renderer/stores/voiceStore');
    (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({ qualityTier: 'standard', setQualityTier: mockSetQualityTier })
    );

    const { useAudioSettingsStore } = await import('@/renderer/stores/audioSettingsStore');
    (useAudioSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode })
    );
    (
      useAudioSettingsStore as unknown as { getState: ReturnType<typeof vi.fn> }
    ).getState.mockReturnValue({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode });

    const { useDraftAudioSetting, useStashAndSwapAudioMode } =
      await import('@/renderer/hooks/useDraftSettings');
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) => defaultAudioSettings[key] ?? false
    );
    (useStashAndSwapAudioMode as ReturnType<typeof vi.fn>).mockReturnValue(mockStashAndSwap);
  });

  // ===== 1. Basic mode rendering =====

  it('renders the Audio Configuration collapsible section', () => {
    render(<AudioConfigSection />);
    expect(screen.getByText('Audio Configuration')).toBeInTheDocument();
  });

  it('renders Quality subsection title and description', () => {
    render(<AudioConfigSection />);
    expect(screen.getByText('Quality')).toBeInTheDocument();
    expect(screen.getByText(/Higher quality uses more bandwidth/)).toBeInTheDocument();
  });

  it('renders all quality tier labels', () => {
    render(<AudioConfigSection />);
    for (const label of ['Minimum', 'Low', 'Moderate', 'Standard', 'High', 'Hi-Fi', 'Studio']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it('renders tier description in basic mode', () => {
    render(<AudioConfigSection />);
    // The tier description contains the first line "96 kbps ... Mono" from TIER_DESCRIPTIONS_BASIC
    expect(screen.getByText(/The Concord default/)).toBeInTheDocument();
  });

  it('renders kbps label', () => {
    render(<AudioConfigSection />);
    expect(screen.getByText('96 kbps')).toBeInTheDocument();
  });

  it('renders processing toggles', () => {
    render(<AudioConfigSection />);
    expect(screen.getByText('Noise Cancellation')).toBeInTheDocument();
    expect(screen.getByText('Echo Cancellation')).toBeInTheDocument();
    expect(screen.getByText('Auto Gain Control')).toBeInTheDocument();
    expect(screen.getByText('Input Noise Gate')).toBeInTheDocument();
    expect(screen.getByText('Boost Quiet Users')).toBeInTheDocument();
  });

  it('renders mode tabs with Basic active by default', () => {
    render(<AudioConfigSection />);
    const basicTab = screen.getByText('Basic Settings');
    const advancedTab = screen.getByText('Advanced Settings');
    expect(basicTab).toHaveAttribute('aria-selected', 'true');
    expect(advancedTab).toHaveAttribute('aria-selected', 'false');
  });

  it('renders tier slider with correct min/max', () => {
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    expect(slider).toBeInTheDocument();
    expect(slider).toHaveAttribute('min', '0');
    expect(slider).toHaveAttribute('max', '6');
  });

  it('does not render AudioOpusSection in basic mode', () => {
    render(<AudioConfigSection />);
    expect(screen.queryByTestId('audio-opus-section')).not.toBeInTheDocument();
  });

  // ===== 2. Advanced mode =====

  it('shows advanced mode notice banner', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/These settings override the quality tier presets/)
    ).toBeInTheDocument();
  });

  it('shows AudioOpusSection in advanced mode', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    expect(screen.getByTestId('audio-opus-section')).toBeInTheDocument();
    expect(screen.getByTestId('audio-opus-section')).toHaveAttribute('data-tier', 'standard');
  });

  it('hides tier description in advanced mode', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    expect(screen.queryByText(/The Concord default/)).not.toBeInTheDocument();
  });

  it('advanced tab is active in advanced mode', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    expect(screen.getByText('Advanced Settings')).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByText('Basic Settings')).toHaveAttribute('aria-selected', 'false');
  });

  // ===== 3. handleTierSlider =====

  it('calls setQualityTier and batchSetAudioDrafts in basic mode when slider changes', async () => {
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '0' } });
    expect(mockSetQualityTier).toHaveBeenCalledWith('minimum');
    expect(batchSetAudioDrafts).toHaveBeenCalledWith(
      expect.objectContaining({
        silenceDetection: true,
        inlineFec: true,
        frameSize: 0,
        stereoOverride: null,
      })
    );
  });

  it('calls setQualityTier but NOT batchSetAudioDrafts in advanced mode when slider changes', async () => {
    await enableAdvancedMode();
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '2' } });
    expect(mockSetQualityTier).toHaveBeenCalledWith('moderate');
    expect(batchSetAudioDrafts).not.toHaveBeenCalled();
  });

  it('ignores NaN slider value', async () => {
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    // NaN fails the >= 0 guard, so neither setQualityTier nor batchSetAudioDrafts is called
    fireEvent.change(slider, { target: { value: 'abc' } });
    expect(mockSetQualityTier).not.toHaveBeenCalled();
    expect(batchSetAudioDrafts).not.toHaveBeenCalled();
  });

  // ===== 4. handleAdvancedToggle =====

  it('calls setAdvancedMode and stashAndSwapAudioMode when clicking Advanced Settings tab', () => {
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByText('Advanced Settings'));
    expect(mockSetAdvancedMode).toHaveBeenCalledWith(true);
    expect(mockStashAndSwap).toHaveBeenCalledWith(true, 'standard');
  });

  it('calls setAdvancedMode(false) when clicking Basic Settings tab', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByText('Basic Settings'));
    expect(mockSetAdvancedMode).toHaveBeenCalledWith(false);
    expect(mockStashAndSwap).toHaveBeenCalledWith(false, 'standard');
  });

  // ===== 5. Tier label onClick =====

  it('sets quality tier when clicking a tier label in basic mode', async () => {
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByText('High'));
    expect(mockSetQualityTier).toHaveBeenCalledWith('high');
    expect(batchSetAudioDrafts).toHaveBeenCalled();
  });

  it('sets quality tier without batching drafts in advanced mode', async () => {
    await enableAdvancedMode();
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByText('Low'));
    expect(mockSetQualityTier).toHaveBeenCalledWith('low');
    expect(batchSetAudioDrafts).not.toHaveBeenCalled();
  });

  // ===== 6. Tier label onKeyDown =====

  it('triggers tier click on Enter key', async () => {
    render(<AudioConfigSection />);
    const label = screen.getByText('Minimum');
    fireEvent.keyDown(label, { key: 'Enter' });
    expect(mockSetQualityTier).toHaveBeenCalledWith('minimum');
  });

  it('triggers tier click on Space key', async () => {
    render(<AudioConfigSection />);
    const label = screen.getByText('Low');
    fireEvent.keyDown(label, { key: ' ' });
    expect(mockSetQualityTier).toHaveBeenCalledWith('low');
  });

  it('does not trigger tier click on other keys', () => {
    render(<AudioConfigSection />);
    const label = screen.getByText('Minimum');
    fireEvent.keyDown(label, { key: 'Tab' });
    expect(mockSetQualityTier).not.toHaveBeenCalled();
  });

  // ===== 7. Mode pill onKeyDown =====

  it('toggles advanced mode on Enter key on mode pill', () => {
    render(<AudioConfigSection />);
    const advPill = screen.getByText('Advanced Settings');
    fireEvent.keyDown(advPill, { key: 'Enter' });
    expect(mockSetAdvancedMode).toHaveBeenCalledWith(true);
    expect(mockStashAndSwap).toHaveBeenCalledWith(true, 'standard');
  });

  it('toggles advanced mode on Space key on mode pill', () => {
    render(<AudioConfigSection />);
    const advPill = screen.getByText('Advanced Settings');
    fireEvent.keyDown(advPill, { key: ' ' });
    expect(mockSetAdvancedMode).toHaveBeenCalledWith(true);
    expect(mockStashAndSwap).toHaveBeenCalledWith(true, 'standard');
  });

  it('does not toggle mode on other keys', () => {
    render(<AudioConfigSection />);
    const advPill = screen.getByText('Advanced Settings');
    fireEvent.keyDown(advPill, { key: 'Tab' });
    expect(mockSetAdvancedMode).not.toHaveBeenCalled();
  });

  // ===== 8. processingHint() =====

  it('shows "Locked by Music Mode" text when musicMode is true', async () => {
    await overrideDraftSettings({ musicMode: true });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/Locked by Music Mode\. Noise cancellation is forced off/)
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Locked by Music Mode\. Echo cancellation is forced off/)
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Locked by Music Mode\. Automatic gain control is forced off/)
    ).toBeInTheDocument();
  });

  it('shows enabled processing hint when musicMode=false and toggle enabled', async () => {
    await overrideDraftSettings({ musicMode: false, noiseCancellation: true });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/Background noise from your microphone is actively filtered/)
    ).toBeInTheDocument();
  });

  it('shows disabled processing hint when musicMode=false and toggle disabled', async () => {
    await overrideDraftSettings({
      musicMode: false,
      noiseCancellation: false,
      echoCancellation: false,
      autoGainControl: false,
    });
    render(<AudioConfigSection />);
    expect(screen.getByText(/No noise filtering is applied/)).toBeInTheDocument();
    expect(screen.getByText(/No echo cancellation is applied/)).toBeInTheDocument();
    expect(
      screen.getByText(/Your microphone level is not automatically adjusted/)
    ).toBeInTheDocument();
  });

  // ===== 9. Music mode locked =====

  it('disables processing toggles when musicMode is true', async () => {
    await overrideDraftSettings({ musicMode: true });
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // First three checkboxes correspond to noise/echo/agc and should be disabled
    const disabledCheckboxes = Array.from(checkboxes).filter(
      (cb) => (cb as HTMLInputElement).disabled
    );
    expect(disabledCheckboxes.length).toBeGreaterThanOrEqual(3);
  });

  it('processing toggles are unchecked when musicMode is true', async () => {
    await overrideDraftSettings({
      musicMode: true,
      noiseCancellation: true,
      echoCancellation: true,
      autoGainControl: true,
    });
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // The first three are noise, echo, agc -- checked = !musicMode && value, so false
    for (let i = 0; i < 3; i++) {
      expect((checkboxes[i] as HTMLInputElement).checked).toBe(false);
    }
  });

  // ===== 10. Noise gate: manual vs auto =====

  it('shows gate threshold slider when noiseGateMode is manual', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -50 });
    render(<AudioConfigSection />);
    expect(screen.getByText('Gate Threshold')).toBeInTheDocument();
    expect(screen.getByText('-50 dBFS')).toBeInTheDocument();
  });

  it('hides gate threshold slider when noiseGateMode is auto', () => {
    render(<AudioConfigSection />);
    expect(screen.queryByText('Gate Threshold')).not.toBeInTheDocument();
  });

  it('shows correct hint for manual noise gate', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -50 });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/Input below -50 dBFS is muted with a hard cutoff/)
    ).toBeInTheDocument();
  });

  it('shows correct hint for auto noise gate', () => {
    render(<AudioConfigSection />);
    expect(screen.getByText(/No hard cutoff is applied to your input/)).toBeInTheDocument();
  });

  it('calls setDraftAudioSetting to toggle noise gate mode', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    // The noise gate toggle is the 4th checkbox (after noise, echo, agc)
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    fireEvent.click(checkboxes[3]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('noiseGateMode', 'manual');
  });

  it('calls setDraftAudioSetting when gate threshold slider changes', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -50 });
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const sliders = document.querySelectorAll('.settings-slider');
    expect(sliders.length).toBeGreaterThanOrEqual(1);
    fireEvent.change(sliders[0], { target: { value: '-35' } });
    expect(setDraftAudioSetting).toHaveBeenCalledWith('noiseGateLevel', -35);
  });

  // ===== 11. gateThresholdHint() — all 5 branches =====

  it('gate hint: >= -25 (loud close-mic)', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -20 });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/-20 dBFS.*Gates everything except loud, close-mic speech/)
    ).toBeInTheDocument();
  });

  it('gate hint: >= -35 (background noise)', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -30 });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/-30 dBFS.*Gates background noise and quiet sounds/)
    ).toBeInTheDocument();
  });

  it('gate hint: >= -50 (ambient room noise)', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -45 });
    render(<AudioConfigSection />);
    expect(screen.getByText(/-45 dBFS.*Gates ambient room noise/)).toBeInTheDocument();
  });

  it('gate hint: >= -65 (faint background hum)', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -60 });
    render(<AudioConfigSection />);
    expect(screen.getByText(/-60 dBFS.*Gates only faint background hum/)).toBeInTheDocument();
  });

  it('gate hint: < -65 (near-total silence)', async () => {
    await overrideDraftSettings({ noiseGateMode: 'manual', noiseGateLevel: -70 });
    render(<AudioConfigSection />);
    expect(screen.getByText(/-70 dBFS.*Gates only near-total silence/)).toBeInTheDocument();
  });

  // ===== 12. Quiet boost: on/off =====

  it('shows boost threshold slider when quietBoost is true', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -38 });
    render(<AudioConfigSection />);
    expect(screen.getByText('Boost Threshold')).toBeInTheDocument();
    expect(screen.getByText('-38 dBFS')).toBeInTheDocument();
  });

  it('hides boost threshold slider when quietBoost is false', () => {
    render(<AudioConfigSection />);
    expect(screen.queryByText('Boost Threshold')).not.toBeInTheDocument();
  });

  it('shows correct hint when quietBoost is enabled', async () => {
    await overrideDraftSettings({ quietBoost: true });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(
        /Participants whose audio falls below the threshold are dynamically amplified/
      )
    ).toBeInTheDocument();
  });

  it('shows correct hint when quietBoost is disabled', () => {
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/All participants play at their natural volume level/)
    ).toBeInTheDocument();
  });

  it('calls setDraftAudioSetting when quiet boost toggle is clicked', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    // Quiet boost toggle is the 5th checkbox (noise, echo, agc, gate, boost)
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    fireEvent.click(checkboxes[4]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('quietBoost', true);
  });

  it('calls setDraftAudioSetting when boost threshold slider changes', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -38 });
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const sliders = document.querySelectorAll('.settings-slider');
    expect(sliders.length).toBeGreaterThanOrEqual(1);
    fireEvent.change(sliders[0], { target: { value: '-30' } });
    expect(setDraftAudioSetting).toHaveBeenCalledWith('quietBoostThreshold', -30);
  });

  // ===== 13. boostThresholdHint() — all 5 branches =====

  it('boost hint: >= -24 (not talking at mic)', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -20 });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/-20 dBFS.*Boosts anyone not talking directly at their mic/)
    ).toBeInTheDocument();
  });

  it('boost hint: >= -30 (turned away)', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -28 });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/-28 dBFS.*Boosts participants who sound turned away/)
    ).toBeInTheDocument();
  });

  it('boost hint: >= -38 (noticeably quiet)', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -35 });
    render(<AudioConfigSection />);
    expect(screen.getByText(/-35 dBFS.*Boosts noticeably quiet participants/)).toBeInTheDocument();
  });

  it('boost hint: >= -45 (very quiet)', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -42 });
    render(<AudioConfigSection />);
    expect(screen.getByText(/-42 dBFS.*Boosts only very quiet participants/)).toBeInTheDocument();
  });

  it('boost hint: < -45 (barely audible)', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -50 });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/-50 dBFS.*Boosts only barely-audible participants/)
    ).toBeInTheDocument();
  });

  // ===== 14. Premium badge =====

  it('renders premium badge for hifi tier', async () => {
    await overrideTier('hifi');
    render(<AudioConfigSection />);
    expect(screen.getByText('Premium')).toBeInTheDocument();
  });

  it('does not render premium badge for standard tier', () => {
    render(<AudioConfigSection />);
    expect(screen.queryByText('Premium')).not.toBeInTheDocument();
  });

  // ===== 15. Toggle onChange handlers =====

  it('calls setDraftAudioSetting for noise cancellation toggle', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Uncheck noise cancellation (index 0)
    fireEvent.click(checkboxes[0]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('noiseCancellation', false);
  });

  it('calls setDraftAudioSetting for echo cancellation toggle', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Uncheck echo cancellation (index 1)
    fireEvent.click(checkboxes[1]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('echoCancellation', false);
  });

  it('calls setDraftAudioSetting for auto gain control toggle', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Uncheck auto gain control (index 2)
    fireEvent.click(checkboxes[2]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('autoGainControl', false);
  });

  it('does not call setDraftAudioSetting for processing toggles when musicMode is true', async () => {
    await overrideDraftSettings({ musicMode: true });
    const { setDraftAudioSetting } = await import('@/renderer/hooks/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Attempt to click noise cancellation toggle (disabled)
    fireEvent.click(checkboxes[0]);
    // The onChange guard checks !musicMode, so setDraftAudioSetting should not be called
    // for noiseCancellation, echoCancellation, or autoGainControl
    expect(setDraftAudioSetting).not.toHaveBeenCalledWith('noiseCancellation', expect.anything());
  });

  // ===== Processing subsection title =====

  it('renders Processing subsection title', () => {
    render(<AudioConfigSection />);
    expect(screen.getByText('Processing')).toBeInTheDocument();
  });
});
