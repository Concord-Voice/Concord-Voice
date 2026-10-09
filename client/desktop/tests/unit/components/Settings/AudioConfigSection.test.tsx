import { vi } from 'vitest';
import React from 'react';

// ─── Mock setup (BEFORE component imports) ──────────────────────────────────

const mockSetQualityTier = vi.fn();
const mockSetAdvancedMode = vi.fn();
const mockStashAndSwap = vi.fn();
const mockStartMicTest = vi.fn();
const mockStopMicTest = vi.fn();
const mockPlayTestTone = vi.fn();
const mockStopOutputTest = vi.fn();
let mockLocalIsTesting = false;
let mockCallState: { kind: string } = { kind: 'idle' };
let micTestState = {
  isTesting: false,
  dbfsLevel: -Infinity,
  inputOverloaded: false,
  error: null as string | null,
  dynamicGateStatus: null as null | { state: string; thresholdDbfs?: number; error?: string },
};
let outputTestState = { isTesting: false, error: null as string | null };

const defaultAudioSettings: Record<string, unknown> = {
  noiseCancellation: true,
  echoCancellation: true,
  autoGainControl: false,
  noiseGateMode: 'dynamic',
  noiseGateLevel: -50,
  quietBoost: false,
  quietBoostThreshold: -38,
  musicMode: false,
  inputVolume: 100,
  outputVolume: 100,
};

vi.mock('@/renderer/stores/voice/voiceStore', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/stores/voice/voiceStore')>();
  return {
    useVoiceStore: Object.assign(
      vi.fn((selector) =>
        selector({
          ...actual.useVoiceStore.getState(),
          localIsTesting: mockLocalIsTesting,
          callState: mockCallState,
          qualityTier: 'standard' as const,
          setQualityTier: mockSetQualityTier,
        })
      ),
      actual.useVoiceStore
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
  };
});

vi.mock('@/renderer/stores/audio/audioSettingsStore', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('@/renderer/stores/audio/audioSettingsStore')>();
  return {
    effectiveNoiseGateMode: actual.effectiveNoiseGateMode,
    useAudioSettingsStore: Object.assign(
      vi.fn((s) =>
        s({
          advancedMode: false,
          inputVolume: 100,
          noiseCancellation: true,
          echoCancellation: true,
          autoGainControl: true,
          noiseGateMode: 'dynamic',
          noiseGateLevel: -50,
          musicMode: false,
          setAdvancedMode: mockSetAdvancedMode,
        })
      ),
      actual.useAudioSettingsStore,
      {
        getState: vi.fn(() => ({
          ...actual.useAudioSettingsStore.getState(),
          advancedMode: false,
          setAdvancedMode: mockSetAdvancedMode,
        })),
      }
    ),
  };
});

vi.mock('@/renderer/hooks/ui/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn((key: string) => defaultAudioSettings[key] ?? false),
  setDraftAudioSetting: vi.fn(),
  batchSetAudioDrafts: vi.fn(),
  useStashAndSwapAudioMode: vi.fn(() => mockStashAndSwap),
}));

vi.mock('@/renderer/hooks/device/useMicTest', () => ({
  useMicTest: () => ({ ...micTestState, startTest: mockStartMicTest, stopTest: mockStopMicTest }),
}));
vi.mock('@/renderer/hooks/device/useOutputTest', () => ({
  useOutputTest: () => ({
    ...outputTestState,
    playTestTone: mockPlayTestTone,
    stopTest: mockStopOutputTest,
  }),
}));
vi.mock('@/renderer/components/Voice/DeviceSelector', async () => {
  const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
  return {
    default: ({ kind }: { kind: string }) => {
      const label = kind === 'audioinput' ? 'Microphone' : 'Speaker';
      const deviceId = useVoiceStore(
        (state: Record<string, unknown>) =>
          state[kind === 'audioinput' ? 'audioInputDeviceId' : 'audioOutputDeviceId']
      );
      const setDevice = useVoiceStore(
        (state: Record<string, unknown>) =>
          state[kind === 'audioinput' ? 'setAudioInputDevice' : 'setAudioOutputDevice']
      ) as (value: string) => void;
      return (
        <label>
          {label}
          <select
            aria-label={label}
            value={(deviceId as string | null) ?? ''}
            onChange={(event) => setDevice(event.currentTarget.value)}
          >
            <option value="">Default</option>
            <option value="mic-1">USB Microphone</option>
            <option value="speaker-1">USB Speaker</option>
          </select>
        </label>
      );
    },
  };
});

vi.mock('@/renderer/components/Settings/AudioOpusSection', () => ({
  default: ({ qualityTier }: { qualityTier: string }) => (
    <div data-testid="audio-opus-section" data-tier={qualityTier}>
      AudioOpusSection
    </div>
  ),
}));

// ─── Component import (AFTER mocks) ────────────────────────────────────────

import { render, screen, fireEvent, userEvent, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import AudioConfigSection from '@/renderer/components/Settings/AudioConfigSection';

// ─── Helpers ────────────────────────────────────────────────────────────────

/** Override useDraftAudioSetting with custom settings for a single test. */
async function overrideDraftSettings(overrides: Record<string, unknown>) {
  const merged = { ...defaultAudioSettings, ...overrides };
  const { useDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
  (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
    (key: string) => merged[key] ?? false
  );
}

async function overrideCommittedSettings(overrides: Record<string, unknown>) {
  const { useAudioSettingsStore } = await import('@/renderer/stores/audio/audioSettingsStore');
  const committed = {
    inputVolume: 100,
    noiseCancellation: true,
    echoCancellation: true,
    autoGainControl: true,
    noiseGateMode: 'dynamic',
    noiseGateLevel: -50,
    musicMode: false,
    ...overrides,
  };
  (useAudioSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (selector: (state: Record<string, unknown>) => unknown) =>
      selector({ ...committed, advancedMode: false, setAdvancedMode: mockSetAdvancedMode })
  );
  (
    useAudioSettingsStore as unknown as { getState: ReturnType<typeof vi.fn> }
  ).getState.mockReturnValue({
    ...committed,
    advancedMode: false,
    setAdvancedMode: mockSetAdvancedMode,
  });
}

/** Switch the audioSettingsStore mock to advanced mode. */
async function enableAdvancedMode() {
  const { useAudioSettingsStore } = await import('@/renderer/stores/audio/audioSettingsStore');
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
  const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
  (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) =>
      s({ qualityTier: tier, setQualityTier: mockSetQualityTier })
  );
}

// ─── Tests ──────────────────────────────────────────────────────────────────

describe('AudioConfigSection', () => {
  beforeEach(async () => {
    resetAllStores();
    vi.clearAllMocks();
    micTestState = {
      isTesting: false,
      dbfsLevel: -Infinity,
      inputOverloaded: false,
      error: null,
      dynamicGateStatus: null,
    };
    outputTestState = { isTesting: false, error: null };
    mockLocalIsTesting = false;
    mockCallState = { kind: 'idle' };

    // Re-apply default mock implementations (clearAllMocks wipes mockImplementation)
    const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
    (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          ...useVoiceStore.getState(),
          qualityTier: 'standard',
          localIsTesting: mockLocalIsTesting,
          callState: mockCallState,
          setQualityTier: mockSetQualityTier,
        })
    );

    const { useAudioSettingsStore } = await import('@/renderer/stores/audio/audioSettingsStore');
    (useAudioSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          advancedMode: false,
          inputVolume: 100,
          noiseCancellation: true,
          echoCancellation: true,
          autoGainControl: true,
          noiseGateMode: 'dynamic',
          noiseGateLevel: -50,
          musicMode: false,
          setAdvancedMode: mockSetAdvancedMode,
        })
    );
    (
      useAudioSettingsStore as unknown as { getState: ReturnType<typeof vi.fn> }
    ).getState.mockReturnValue({ advancedMode: false, setAdvancedMode: mockSetAdvancedMode });

    const { useDraftAudioSetting, useStashAndSwapAudioMode } =
      await import('@/renderer/hooks/ui/useDraftSettings');
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) => defaultAudioSettings[key] ?? false
    );
    (useStashAndSwapAudioMode as ReturnType<typeof vi.fn>).mockReturnValue(mockStashAndSwap);
  });

  describe('moved audio device controls', () => {
    it('renders microphone and speaker selectors with Default options', () => {
      render(<AudioConfigSection />);
      expect(screen.getByRole('combobox', { name: 'Microphone' })).toHaveTextContent('Default');
      expect(screen.getByRole('combobox', { name: 'Speaker' })).toHaveTextContent('Default');
    });

    it('applies microphone and speaker choices immediately while settings are being edited', async () => {
      await overrideDraftSettings({ noiseCancellation: false });
      render(<AudioConfigSection />);
      fireEvent.change(screen.getByRole('combobox', { name: 'Microphone' }), {
        target: { value: 'mic-1' },
      });
      fireEvent.change(screen.getByRole('combobox', { name: 'Speaker' }), {
        target: { value: 'speaker-1' },
      });

      const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
      expect(useVoiceStore.getState().audioInputDeviceId).toBe('mic-1');
      expect(useVoiceStore.getState().audioOutputDeviceId).toBe('speaker-1');
    });

    it('places device and test controls before the Basic and Advanced mode fieldset', () => {
      render(<AudioConfigSection />);
      const fieldset = screen.getByRole('group', { name: 'Audio settings mode' });
      for (const control of [
        screen.getByRole('combobox', { name: 'Microphone' }),
        screen.getByRole('combobox', { name: 'Speaker' }),
        ...screen.getAllByRole('button', { name: /^Test$/ }),
      ]) {
        expect(
          control.compareDocumentPosition(fieldset) & Node.DOCUMENT_POSITION_FOLLOWING
        ).toBeTruthy();
      }
    });

    it('starts microphone and speaker tests from their controls', () => {
      render(<AudioConfigSection />);
      const tests = screen.getAllByRole('button', { name: /^Test$/ });
      expect(tests).toHaveLength(2);
      fireEvent.click(tests[0]);
      fireEvent.click(tests[1]);
      expect(mockStartMicTest).toHaveBeenCalledTimes(1);
      expect(mockPlayTestTone).toHaveBeenCalledTimes(1);
    });

    it('retains active and error feedback for microphone and speaker tests', () => {
      micTestState = {
        isTesting: true,
        dbfsLevel: -24,
        inputOverloaded: false,
        error: 'Microphone unavailable',
      };
      outputTestState = { isTesting: true, error: 'Audio output unavailable' };
      render(<AudioConfigSection />);
      expect(screen.getByRole('button', { name: 'Stop Testing' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Playing...' })).toBeDisabled();
      expect(screen.getByText('Microphone unavailable')).toBeInTheDocument();
      expect(screen.getByText('Audio output unavailable')).toBeInTheDocument();
    });

    it('announces when the microphone input reaches full scale before peak limiting', () => {
      micTestState = {
        isTesting: true,
        dbfsLevel: -24,
        inputOverloaded: true,
        error: null,
      };
      render(<AudioConfigSection />);
      expect(screen.getByRole('alert')).toHaveTextContent(
        'Input reached or exceeded 0 dBFS before the noise gate.'
      );
      expect(screen.getByRole('alert')).toHaveTextContent('This may indicate clipping');
    });

    it('keeps both audio tests disabled while another local test is active', () => {
      mockLocalIsTesting = true;
      render(<AudioConfigSection />);
      const tests = screen.getAllByRole('button', { name: /^Test$/ });
      expect(tests[0]).toBeDisabled();
      expect(tests[1]).toBeDisabled();
    });
  });

  describe('microphone test pending settings notice', () => {
    it.each([
      ['noise cancellation', { noiseCancellation: false }, {}],
      ['echo cancellation', { echoCancellation: false }, {}],
      ['noise gate mode', { noiseGateMode: 'manual' }, {}],
      [
        'noise gate threshold',
        { noiseGateMode: 'manual', noiseGateLevel: -40 },
        { noiseGateMode: 'manual' },
      ],
    ])('shows when the %s draft differs from applied settings', async (_field, change, applied) => {
      await overrideCommittedSettings({ autoGainControl: false, ...applied });
      await overrideDraftSettings({ autoGainControl: false, ...change });
      render(<AudioConfigSection />);

      expect(
        screen.getByText(/Microphone Test uses applied settings until you select Apply/)
      ).toBeInTheDocument();
    });

    it('keeps the notice visible when a draft enables AGC and hides Microphone Level', async () => {
      await overrideCommittedSettings({ autoGainControl: false });
      await overrideDraftSettings({ autoGainControl: true });
      render(<AudioConfigSection />);

      expect(screen.queryByRole('slider', { name: 'Microphone Level' })).not.toBeInTheDocument();
      expect(
        screen.getByText(/Microphone Test uses applied settings until you select Apply/)
      ).toBeInTheDocument();
    });
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
    expect(screen.getByRole('combobox', { name: 'Noise Gate' })).toBeInTheDocument();
    expect(screen.getByText('Boost Quiet Users')).toBeInTheDocument();
  });

  // The mode is a persisted setting, not a view switch, so it is a named radio
  // group — a tablist would promise a tabpanel that does not exist.
  it('renders the mode as a named radio group with Basic checked by default', () => {
    render(<AudioConfigSection />);
    const group = screen.getByRole('group', { name: 'Audio settings mode' });
    expect(within(group).getByRole('radio', { name: 'Basic Settings' })).toBeChecked();
    expect(within(group).getByRole('radio', { name: 'Advanced Settings' })).not.toBeChecked();
    expect(screen.queryAllByRole('tab', { name: /^(Basic|Advanced) Settings$/ })).toHaveLength(0);
    expect(screen.queryByRole('tablist')).not.toBeInTheDocument();
  });

  it('renders tier slider with correct min/max', () => {
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    expect(slider).toBeInTheDocument();
    expect(slider).toHaveAttribute('min', '0');
    expect(slider).toHaveAttribute('max', '6');
  });

  // G1: the slider had no accessible name and no value text, so a screen
  // reader announced only the raw index ("3"). It needs an accessible name
  // plus aria-valuetext naming the current tier, matching the sibling
  // sliders (DMPrivacyControls / FriendRequestPrivacyControls).
  it('quality slider has an accessible name and announces the current tier as its value', () => {
    render(<AudioConfigSection />);
    const slider = screen.getByRole('slider', { name: /audio quality/i });
    expect(slider).toHaveAttribute('aria-valuetext', 'Standard');
  });

  it('quality slider aria-valuetext follows a non-default tier', async () => {
    await overrideTier('low');
    render(<AudioConfigSection />);
    const slider = screen.getByRole('slider', { name: /audio quality/i });
    expect(slider).toHaveAttribute('aria-valuetext', 'Low');
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

  it('checks the Advanced radio in advanced mode', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    expect(screen.getByRole('radio', { name: 'Advanced Settings' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Basic Settings' })).not.toBeChecked();
  });

  // ===== 3. handleTierSlider =====

  it('calls setQualityTier and batchSetAudioDrafts in basic mode when slider changes', async () => {
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '0' } });
    expect(mockSetQualityTier).toHaveBeenCalledWith('minimum');
    expect(batchSetAudioDrafts).toHaveBeenCalledWith(
      expect.objectContaining({
        inlineFec: true,
        frameSize: 0,
        stereoOverride: null,
      })
    );
    expect((batchSetAudioDrafts as ReturnType<typeof vi.fn>).mock.calls[0][0]).not.toHaveProperty(
      'fecHeadroom'
    );
    // Advanced DTX is the user's Advanced preference; Basic resolves DTX from the tier
    // at runtime, so a tier change must not write it ('minimum' has opusDtx: true).
    expect((batchSetAudioDrafts as ReturnType<typeof vi.fn>).mock.calls[0][0]).not.toHaveProperty(
      'silenceDetection'
    );
  });

  it('calls setQualityTier but NOT batchSetAudioDrafts in advanced mode when slider changes', async () => {
    await enableAdvancedMode();
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '2' } });
    expect(mockSetQualityTier).toHaveBeenCalledWith('moderate');
    expect(batchSetAudioDrafts).not.toHaveBeenCalled();
  });

  it('ignores NaN slider value', async () => {
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/ui/useDraftSettings');
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

  // stashAndSwapAudioMode is not idempotent: running it for the mode already active
  // overwrites the stash with the current values. Re-selecting the checked option
  // must therefore do nothing — true of a radio's change event, not of a click handler.
  it('clicking the already-selected Basic option changes nothing', () => {
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByRole('radio', { name: 'Basic Settings' }));
    expect(mockSetAdvancedMode).not.toHaveBeenCalled();
    expect(mockStashAndSwap).not.toHaveBeenCalled();
  });

  it('clicking the already-selected Advanced option changes nothing', async () => {
    await enableAdvancedMode();
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByRole('radio', { name: 'Advanced Settings' }));
    expect(mockSetAdvancedMode).not.toHaveBeenCalled();
    expect(mockStashAndSwap).not.toHaveBeenCalled();
  });

  // ===== 5. Tier label activation parity (click / Enter / Space) =====

  // Click, Enter and Space must all resolve to exactly one selection and one
  // basic-mode draft batch — a native <button> gets Enter/Space activation
  // for free, so this also guards against a stray onKeyDown handler that
  // would double-fire alongside it.
  it.each([
    ['click', (btn: HTMLElement) => fireEvent.click(btn)],
    [
      'Enter',
      async (btn: HTMLElement) => {
        const user = userEvent.setup();
        btn.focus();
        await user.keyboard('{Enter}');
      },
    ],
    [
      'Space',
      async (btn: HTMLElement) => {
        const user = userEvent.setup();
        btn.focus();
        await user.keyboard(' ');
      },
    ],
  ])(
    'activates a free tier via %s: selects it once and batches drafts in basic mode',
    async (_label, activate) => {
      const { batchSetAudioDrafts } = await import('@/renderer/hooks/ui/useDraftSettings');
      render(<AudioConfigSection />);
      const btn = screen.getByRole('button', { name: 'High' });
      await activate(btn);
      expect(mockSetQualityTier).toHaveBeenCalledTimes(1);
      expect(mockSetQualityTier).toHaveBeenCalledWith('high');
      expect(batchSetAudioDrafts).toHaveBeenCalled();
    }
  );

  it('sets quality tier without batching drafts in advanced mode', async () => {
    await enableAdvancedMode();
    const { batchSetAudioDrafts } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    fireEvent.click(screen.getByText('Low'));
    expect(mockSetQualityTier).toHaveBeenCalledWith('low');
    expect(batchSetAudioDrafts).not.toHaveBeenCalled();
  });

  // ===== 6. Tier labels are native buttons =====

  // The labels are shortcuts onto the slider: native buttons whose pressed
  // state marks the current tier. A tab would promise a tabpanel that does
  // not exist.
  it('renders each tier label as a native button pressed only for the current tier', () => {
    render(<AudioConfigSection />);
    expect(screen.getByRole('button', { name: 'Standard' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.getByRole('button', { name: 'Low' })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByRole('tab', { name: 'Standard' })).not.toBeInTheDocument();
  });

  // G3: the tests above only ever exercise the DEFAULT tier ('standard'), so a
  // mutant that hardcodes the pressed comparison against that default (rather
  // than the live `qualityTier`) would still pass every one of them.
  it('pressed state follows a non-default tier, and exactly one label is pressed', async () => {
    await overrideTier('low');
    const { container } = render(<AudioConfigSection />);
    expect(screen.getByRole('button', { name: 'Low' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'Standard' })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
    const labels = within(container.querySelector('.settings-tier-labels') as HTMLElement);
    expect(labels.getAllByRole('button', { pressed: true })).toHaveLength(1);
    expect(labels.getAllByRole('button', { pressed: true })[0]).toHaveTextContent('Low');
  });

  it('does not select a tier on other keys', async () => {
    const user = userEvent.setup();
    render(<AudioConfigSection />);
    screen.getByRole('button', { name: 'Minimum' }).focus();
    await user.keyboard('a');
    expect(mockSetQualityTier).not.toHaveBeenCalled();
  });

  // ===== 7. Mode radio keyboard =====

  it('ArrowRight from Basic selects and persists Advanced', async () => {
    const user = userEvent.setup();
    render(<AudioConfigSection />);
    screen.getByRole('radio', { name: 'Basic Settings' }).focus();
    await user.keyboard('{ArrowRight}');
    expect(mockSetAdvancedMode).toHaveBeenCalledTimes(1);
    expect(mockSetAdvancedMode).toHaveBeenCalledWith(true);
    expect(mockStashAndSwap).toHaveBeenCalledTimes(1);
    expect(mockStashAndSwap).toHaveBeenCalledWith(true, 'standard');
  });

  it('ArrowLeft from Advanced selects and persists Basic', async () => {
    await enableAdvancedMode();
    const user = userEvent.setup();
    render(<AudioConfigSection />);
    screen.getByRole('radio', { name: 'Advanced Settings' }).focus();
    await user.keyboard('{ArrowLeft}');
    expect(mockSetAdvancedMode).toHaveBeenCalledTimes(1);
    expect(mockSetAdvancedMode).toHaveBeenCalledWith(false);
    expect(mockStashAndSwap).toHaveBeenCalledTimes(1);
    expect(mockStashAndSwap).toHaveBeenCalledWith(false, 'standard');
  });

  // One Tab stop per group: Tab leaves the group instead of landing on the
  // unchecked option, and moving focus never changes the persisted mode.
  it('Tab leaves the mode group without stopping on the other option', async () => {
    const user = userEvent.setup();
    render(<AudioConfigSection />);
    const basic = screen.getByRole('radio', { name: 'Basic Settings' });
    basic.focus();
    expect(basic).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('radio', { name: 'Advanced Settings' })).not.toHaveFocus();
    expect(screen.getByRole('group', { name: 'Audio settings mode' })).not.toContainElement(
      document.activeElement as HTMLElement
    );
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

  // ===== 10. Dynamic noise gate modes and status =====

  it('offers Dynamic and Off when AGC is effective', async () => {
    // Fresh installs default AGC off, so this case opts in explicitly.
    await overrideDraftSettings({ autoGainControl: true });
    render(<AudioConfigSection />);
    const select = screen.getByRole('combobox', { name: 'Noise Gate' });
    expect(select).toHaveValue('dynamic');
    expect(within(select).getByRole('option', { name: 'Dynamic' })).toBeInTheDocument();
    expect(within(select).getByRole('option', { name: 'Off' })).toBeInTheDocument();
    expect(
      within(select).queryByRole('option', { name: 'Manual Calibrate' })
    ).not.toBeInTheDocument();
  });

  it('offers Manual Calibrate when AGC is ineffective and retains the fixed threshold control', async () => {
    await overrideDraftSettings({
      autoGainControl: false,
      noiseGateMode: 'manualCalibrate',
      noiseGateLevel: -50,
    });
    render(<AudioConfigSection />);
    expect(
      within(screen.getByRole('combobox', { name: 'Noise Gate' })).getByRole('option', {
        name: 'Manual Calibrate',
      })
    ).toBeInTheDocument();
    expect(screen.getByRole('slider', { name: 'Gate Threshold' })).toHaveValue('-50');
    expect(screen.getByText('Set the threshold yourself.')).toBeInTheDocument();
  });

  it('offers Manual Calibrate when Music Mode disables effective AGC', async () => {
    await overrideDraftSettings({ musicMode: true, autoGainControl: true });
    render(<AudioConfigSection />);
    expect(
      within(screen.getByRole('combobox', { name: 'Noise Gate' })).getByRole('option', {
        name: 'Manual Calibrate',
      })
    ).toBeInTheDocument();
    expect(screen.getByText(/Music Mode disables Auto Gain Control/)).toBeInTheDocument();
  });

  it('preserves legacy Off and provides a keyboard-focusable labeled native select', async () => {
    await overrideDraftSettings({ noiseGateMode: 'off' });
    render(<AudioConfigSection />);
    const select = screen.getByRole('combobox', { name: 'Noise Gate' });
    expect(select).toHaveValue('off');
    select.focus();
    expect(select).toHaveFocus();
  });

  it('reports an AGC draft switch to Dynamic until Apply', async () => {
    await overrideCommittedSettings({ autoGainControl: false, noiseGateMode: 'manualCalibrate' });
    await overrideDraftSettings({ autoGainControl: true, noiseGateMode: 'dynamic' });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/Switching to Dynamic takes effect when you select Apply/)
    ).toBeInTheDocument();
  });

  it('does not claim Dynamic is learning before an Off-to-Dynamic draft is applied', async () => {
    await overrideCommittedSettings({ noiseGateMode: 'off' });
    await overrideDraftSettings({ noiseGateMode: 'dynamic' });
    render(<AudioConfigSection />);
    expect(
      screen.getByText(/Noise Gate changes take effect when you select Apply/)
    ).toBeInTheDocument();
    expect(
      screen.queryByText('The gate learns during a call or microphone Test')
    ).not.toBeInTheDocument();
  });

  it('shows idle guidance when no call or microphone Test owns Dynamic', () => {
    render(<AudioConfigSection />);
    expect(
      screen.getByText('The gate learns during a call or microphone Test')
    ).toBeInTheDocument();
  });

  it.each([
    ['Learning', { state: 'learning' }],
    ['Adjusted', { state: 'adjusted', thresholdDbfs: -42 }],
    ['Uncertain', { state: 'uncertain' }],
  ])('announces dynamic gate %s status politely', (label, status) => {
    micTestState = { ...micTestState, isTesting: true, dynamicGateStatus: status };
    render(<AudioConfigSection />);
    const announcement = screen.getByRole('status');
    expect(announcement).toHaveAttribute('aria-live', 'polite');
    expect(announcement).toHaveTextContent(label);
    expect(screen.getAllByRole('status')).toHaveLength(1);
    if (label === 'Adjusted') {
      expect(announcement).not.toHaveTextContent(/dBFS/);
      expect(screen.getByText(/Current threshold: −42 dBFS/)).toBeInTheDocument();
    }
    if (label === 'Uncertain')
      expect(announcement).toHaveTextContent(/moving the microphone closer/);
  });

  it('shows retry guidance when microphone learning fails', () => {
    micTestState = { ...micTestState, isTesting: true, error: 'Microphone unavailable' };
    render(<AudioConfigSection />);
    expect(screen.getByRole('status')).toHaveTextContent(/Microphone unavailable.*try again/i);
  });

  it('keeps microphone Test failures visible after the Test stops', () => {
    micTestState = { ...micTestState, error: 'Microphone unavailable' };
    render(<AudioConfigSection />);
    expect(screen.getByRole('status')).toHaveTextContent(/Microphone unavailable.*try again/i);
  });

  it('shows the live processor failure when Dynamic learning stops during a call', async () => {
    const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
    useVoiceStore.setState({
      joinError: 'Microphone processing stopped. Retry your microphone or rejoin the call.',
      dynamicGateStatus: null,
    });
    mockCallState = { kind: 'in-call' };

    render(<AudioConfigSection />);

    expect(screen.getByRole('status')).toHaveTextContent(/Microphone processing stopped/i);
    expect(screen.queryByText('The gate learns during a call or microphone Test')).toBeNull();
  });

  it('prefers a current live processor failure over an earlier microphone Test error', async () => {
    const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
    micTestState = { ...micTestState, error: 'Earlier Test failure' };
    useVoiceStore.setState({
      joinError: 'Microphone processing stopped. Retry your microphone or rejoin the call.',
      dynamicGateStatus: null,
    });
    mockCallState = { kind: 'in-call' };

    render(<AudioConfigSection />);

    expect(screen.getByRole('status')).toHaveTextContent(/Microphone processing stopped/i);
    expect(screen.getByRole('status')).not.toHaveTextContent(/Earlier Test failure/i);
  });

  it('keeps status changes in one polite announcement region', () => {
    micTestState = {
      ...micTestState,
      isTesting: true,
      dynamicGateStatus: { state: 'adjusted', thresholdDbfs: -42 },
    };
    render(<AudioConfigSection />);
    expect(screen.getAllByRole('status')).toHaveLength(1);
    expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite');
  });

  it('does not acquire another microphone when Settings opens during an active call', () => {
    mockCallState = { kind: 'in-call' };
    const getUserMedia = vi.fn();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia },
    });
    render(<AudioConfigSection />);
    expect(getUserMedia).not.toHaveBeenCalled();
    expect(mockStartMicTest).not.toHaveBeenCalled();
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
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    // Quiet boost follows noise cancellation, echo cancellation, and AGC.
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    fireEvent.click(checkboxes[3]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('quietBoost', true);
  });

  it('calls setDraftAudioSetting when boost threshold slider changes', async () => {
    await overrideDraftSettings({ quietBoost: true, quietBoostThreshold: -38 });
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
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
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Uncheck noise cancellation (index 0)
    fireEvent.click(checkboxes[0]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('noiseCancellation', false);
  });

  it('calls setDraftAudioSetting for echo cancellation toggle', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Uncheck echo cancellation (index 1)
    fireEvent.click(checkboxes[1]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('echoCancellation', false);
  });

  it('calls setDraftAudioSetting for auto gain control toggle', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<AudioConfigSection />);
    const checkboxes = document.querySelectorAll('input[type="checkbox"]');
    // Enable auto gain control from its new-user Off default (index 2)
    fireEvent.click(checkboxes[2]);
    expect(setDraftAudioSetting).toHaveBeenCalledWith('autoGainControl', true);
  });

  it('does not call setDraftAudioSetting for processing toggles when musicMode is true', async () => {
    await overrideDraftSettings({ musicMode: true });
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
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
