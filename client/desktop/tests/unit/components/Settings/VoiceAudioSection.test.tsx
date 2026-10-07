import { render, screen, fireEvent, userEvent, within } from '../../../test-utils';
import { vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';

const mockSetQualityTier = vi.fn();
let mockConnectionState = 'disconnected';
let mockLocalIsTesting = false;
vi.mock('@/renderer/stores/voice/voiceStore', () => ({
  useVoiceStore: Object.assign(
    vi.fn((selector) =>
      selector({
        qualityTier: 'standard' as const,
        setQualityTier: mockSetQualityTier,
        activeCameraCodec: null,
        activeScreenCodec: null,
        connectionState: mockConnectionState,
        localIsTesting: mockLocalIsTesting,
        audioInputDeviceId: null,
        audioOutputDeviceId: null,
        videoDeviceId: null,
        setAudioInputDevice: vi.fn(),
        setAudioOutputDevice: vi.fn(),
        setVideoDevice: vi.fn(),
      })
    ),
    { getState: vi.fn(() => ({ reset: vi.fn() })), setState: vi.fn() }
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
vi.mock('@/renderer/stores/audio/audioSettingsStore', () => ({
  useAudioSettingsStore: Object.assign(
    vi.fn((s) => s({ advancedMode: false, setAdvancedMode: vi.fn() })),
    { getState: vi.fn(() => ({ advancedMode: false, setAdvancedMode: vi.fn() })) }
  ),
}));
vi.mock('@/renderer/stores/voice/videoSettingsStore', () => ({
  useVideoSettingsStore: vi.fn((s) =>
    s({ codecCapabilities: [], gpuInfo: null, videoAdvancedMode: false, systemHdr: false })
  ),
  VIDEO_QUALITY_PRESETS: {},
}));
vi.mock('@/renderer/hooks/ui/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn(
    (key: string) =>
      ({
        stereoOverride: null,
        noiseCancellation: true,
        echoCancellation: true,
        autoGainControl: true,
        noiseGateMode: 'auto',
        noiseGateLevel: -50,
        inputVolume: 100,
        outputVolume: 100,
        quietBoost: false,
        quietBoostThreshold: -38,
        musicMode: false,
        frameSize: 0,
        silenceDetection: true,
        inlineFec: true,
        fecHeadroom: true,
        opusNack: false,
        adaptivePtime: true,
        audioPriority: 'medium',
      })[key] ?? false
  ),
  useDraftVideoSetting: vi.fn(
    (key: string) =>
      ({
        cameraPreset: '720p',
        screenResolution: '1080p',
        screenFrameRate: 30,
        screenContentType: 'detail',
        preferredVideoCodec: '',
        screenSharePriority: 'medium',
        screenShareBitrate: 3000000,
        cameraPriority: 'medium',
        degradationPreference: 'balanced',
        hardwareAcceleration: true,
        hdrEncoding: false,
      })[key] ?? false
  ),
  setDraftAudioSetting: vi.fn(),
  setDraftVideoSetting: vi.fn(),
  batchSetAudioDrafts: vi.fn(),
  useStashAndSwapAudioMode: vi.fn(() => vi.fn()),
}));
const mockStartTest = vi.fn();
const mockStopTest = vi.fn();
const mockPlayOutputTest = vi.fn().mockResolvedValue(undefined);
const mockStopOutputTest = vi.fn();
const mockToggleCameraTest = vi.fn().mockResolvedValue(undefined);
const mockStopCameraTest = vi.fn();
let outputTestState = {
  isTesting: false,
  error: null as string | null,
  playTestTone: mockPlayOutputTest,
  stopTest: mockStopOutputTest,
};
let cameraTestState = {
  isTesting: false,
  error: null as string | null,
  stream: null as MediaStream | null,
  toggleTest: mockToggleCameraTest,
  stopTest: mockStopCameraTest,
};
vi.mock('@/renderer/hooks/device/useMicTest', () => ({
  useMicTest: vi.fn(() => ({
    isTesting: false,
    dbfsLevel: -80,
    error: null,
    startTest: mockStartTest,
    stopTest: mockStopTest,
  })),
}));
vi.mock('@/renderer/hooks/device/useOutputTest', () => ({
  useOutputTest: vi.fn(() => outputTestState),
}));
vi.mock('@/renderer/hooks/device/useCameraTest', () => ({
  useCameraTest: vi.fn(() => cameraTestState),
}));
vi.mock('@/renderer/services/voice/mediaCapabilities', () => ({
  codecKey: vi.fn(),
  codecKeyMime: vi.fn(),
  getCodecInfo: vi.fn(),
}));
vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    options,
    value,
    onChange,
    disabled,
    className,
    id,
  }: {
    options: { value: string; label: string }[];
    value: string;
    onChange: (v: string) => void;
    disabled?: boolean;
    className?: string;
    id?: string;
  }) => (
    <select
      id={id}
      data-testid="custom-select"
      className={className}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  ),
}));
Object.defineProperty(navigator, 'mediaDevices', {
  value: {
    enumerateDevices: vi.fn().mockResolvedValue([]),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  },
  configurable: true,
});

import VoiceAudioSection from '@/renderer/components/Settings/VoiceAudioSection';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useVideoSettingsStore } from '@/renderer/stores/voice/videoSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useDraftAudioSetting } from '@/renderer/hooks/ui/useDraftSettings';
import { useMicTest } from '@/renderer/hooks/device/useMicTest';

function setAudioAdvancedMode(advancedMode: boolean) {
  (useAudioSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) =>
      s({ advancedMode, setAdvancedMode: vi.fn() })
  );
}

describe('VoiceAudioSection', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    outputTestState = {
      isTesting: false,
      error: null,
      playTestTone: mockPlayOutputTest,
      stopTest: mockStopOutputTest,
    };
    cameraTestState = {
      isTesting: false,
      error: null,
      stream: null,
      toggleTest: mockToggleCameraTest,
      stopTest: mockStopCameraTest,
    };
    mockConnectionState = 'disconnected';
    mockLocalIsTesting = false;
    (useAudioSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({ advancedMode: false, setAdvancedMode: vi.fn() })
    );
    (useVideoSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({ codecCapabilities: [], gpuInfo: null, videoAdvancedMode: false, systemHdr: false })
    );
    (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          qualityTier: 'standard',
          setQualityTier: mockSetQualityTier,
          activeCameraCodec: null,
          activeScreenCodec: null,
          connectionState: mockConnectionState,
          localIsTesting: mockLocalIsTesting,
          audioInputDeviceId: null,
          audioOutputDeviceId: null,
          videoDeviceId: null,
          setAudioInputDevice: vi.fn(),
          setAudioOutputDevice: vi.fn(),
          setVideoDevice: vi.fn(),
        })
    );
    (useDraftAudioSetting as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) =>
        ({
          stereoOverride: null,
          noiseCancellation: true,
          echoCancellation: true,
          autoGainControl: true,
          noiseGateMode: 'auto',
          noiseGateLevel: -50,
          inputVolume: 100,
          outputVolume: 100,
          quietBoost: false,
          quietBoostThreshold: -38,
          musicMode: false,
          frameSize: 0,
          silenceDetection: true,
          inlineFec: true,
          fecHeadroom: true,
          opusNack: false,
          adaptivePtime: true,
          audioPriority: 'medium',
        })[key] ?? false
    );
    (useMicTest as ReturnType<typeof vi.fn>).mockReturnValue({
      isTesting: false,
      dbfsLevel: -80,
      error: null,
      startTest: mockStartTest,
      stopTest: mockStopTest,
    });
  });

  it('removes the separate device configuration section', () => {
    render(<VoiceAudioSection />);
    expect(screen.queryByText('Device Configuration')).not.toBeInTheDocument();
  });
  it('renders audio configuration section', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Audio Configuration')).toBeInTheDocument();
  });
  it('renders video configuration section', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Video Configuration')).toBeInTheDocument();
  });
  it('renders subsection titles', () => {
    render(<VoiceAudioSection />);
    expect(screen.getAllByText('Input').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('Output').length).toBeGreaterThanOrEqual(1);
  });
  it.each([
    ['Basic', false],
    ['Advanced', true],
  ])('keeps device selectors in Audio and Video Configuration in %s mode', (_mode, advanced) => {
    if (advanced) setAudioAdvancedMode(true);
    render(<VoiceAudioSection />);
    const audio = screen.getByText('Audio Configuration').closest('details')!;
    const video = screen.getByText('Video Configuration').closest('details')!;
    expect(within(audio).getByRole('combobox', { name: 'Microphone' })).toHaveValue('');
    expect(within(audio).getByRole('combobox', { name: 'Speaker' })).toHaveValue('');
    expect(within(video).getByRole('combobox', { name: 'Camera' })).toHaveValue('');
    for (const selector of [
      within(audio).getByRole('combobox', { name: 'Microphone' }),
      within(audio).getByRole('combobox', { name: 'Speaker' }),
      within(video).getByRole('combobox', { name: 'Camera' }),
    ]) {
      expect(within(selector).getByRole('option', { name: 'Default' })).toBeInTheDocument();
    }
    const microphone = within(audio).getByRole('combobox', { name: 'Microphone' });
    const speaker = within(audio).getByRole('combobox', { name: 'Speaker' });
    expect(microphone.closest('.settings-audio-devices')).not.toBeNull();
    expect(microphone.closest('.settings-audio-devices')).toBe(
      speaker.closest('.settings-audio-devices')
    );
    const agc = within(audio).getByText('Auto Gain Control');
    expect(microphone.compareDocumentPosition(agc) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const camera = within(video).getByRole('combobox', { name: 'Camera' });
    const cameraPreset = within(video).getByText('Camera Preset');
    expect(
      camera.compareDocumentPosition(cameraPreset) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
  });

  it('keeps all three device tests available in their configuration sections', () => {
    render(<VoiceAudioSection />);
    const audio = screen.getByText('Audio Configuration').closest('details')!;
    const video = screen.getByText('Video Configuration').closest('details')!;
    const inputGroup = within(audio)
      .getByRole('heading', { name: 'Input' })
      .closest('.settings-audio-device-group')!;
    const outputGroup = within(audio)
      .getByRole('heading', { name: 'Output' })
      .closest('.settings-audio-device-group')!;
    expect(within(inputGroup).getByRole('button', { name: /^Test$/ })).toBeInTheDocument();
    expect(within(outputGroup).getByRole('button', { name: /^Test$/ })).toBeInTheDocument();
    expect(within(outputGroup).getByText('Output Volume')).toBeInTheDocument();
    expect(within(video).getByRole('button', { name: /^Test$/ })).toBeInTheDocument();
  });

  it('routes each Test button to its owning device test', () => {
    render(<VoiceAudioSection />);
    const audio = screen.getByText('Audio Configuration').closest('details')!;
    const video = screen.getByText('Video Configuration').closest('details')!;
    const inputGroup = within(audio)
      .getByRole('heading', { name: 'Input' })
      .closest('.settings-audio-device-group')!;
    const outputGroup = within(audio)
      .getByRole('heading', { name: 'Output' })
      .closest('.settings-audio-device-group')!;
    fireEvent.click(within(inputGroup).getByRole('button', { name: /^Test$/ }));
    fireEvent.click(within(outputGroup).getByRole('button', { name: /^Test$/ }));
    fireEvent.click(within(video).getByRole('button', { name: /^Test$/ }));
    expect(mockStartTest).toHaveBeenCalledOnce();
    expect(mockPlayOutputTest).toHaveBeenCalledOnce();
    expect(mockToggleCameraTest).toHaveBeenCalledOnce();
  });

  it('keeps microphone and speaker tests available during a call', () => {
    mockConnectionState = 'connected';
    render(<VoiceAudioSection />);
    const audio = screen.getByText('Audio Configuration').closest('details')!;
    const video = screen.getByText('Video Configuration').closest('details')!;
    const inputGroup = within(audio)
      .getByRole('heading', { name: 'Input' })
      .closest('.settings-audio-device-group')!;
    const outputGroup = within(audio)
      .getByRole('heading', { name: 'Output' })
      .closest('.settings-audio-device-group')!;
    expect(within(inputGroup).getByRole('button', { name: /^Test$/ })).not.toBeDisabled();
    expect(within(outputGroup).getByRole('button', { name: /^Test$/ })).not.toBeDisabled();
    expect(within(video).getByRole('button', { name: /^Test$/ })).toBeDisabled();
  });

  it('keeps microphone and speaker tests disabled while another test owns audio', () => {
    mockConnectionState = 'disconnected';
    mockLocalIsTesting = true;
    render(<VoiceAudioSection />);
    const audio = screen.getByText('Audio Configuration').closest('details')!;
    const video = screen.getByText('Video Configuration').closest('details')!;
    const inputGroup = within(audio)
      .getByRole('heading', { name: 'Input' })
      .closest('.settings-audio-device-group')!;
    const outputGroup = within(audio)
      .getByRole('heading', { name: 'Output' })
      .closest('.settings-audio-device-group')!;
    expect(within(inputGroup).getByRole('button', { name: /^Test$/ })).toBeDisabled();
    expect(within(outputGroup).getByRole('button', { name: /^Test$/ })).toBeDisabled();
    expect(within(video).getByRole('button', { name: /^Test$/ })).not.toBeDisabled();
  });

  it('retains output test status and camera preview while active', () => {
    const stream = { getTracks: () => [] } as unknown as MediaStream;
    outputTestState = { ...outputTestState, isTesting: true };
    cameraTestState = { ...cameraTestState, isTesting: true, stream };
    render(<VoiceAudioSection />);
    expect(screen.getByRole('button', { name: /Playing/ })).toBeDisabled();
    expect(screen.getByRole('button', { name: /Stop Preview/ })).toBeInTheDocument();
    expect(document.querySelector('video')).toBeInTheDocument();
  });

  it('keeps output and camera failure guidance visible', () => {
    outputTestState = { ...outputTestState, error: 'Failed to play test tone' };
    cameraTestState = { ...cameraTestState, error: 'Camera access denied' };
    render(<VoiceAudioSection />);
    expect(screen.getByText('Failed to play test tone')).toBeInTheDocument();
    expect(screen.getByText('Camera access denied')).toBeInTheDocument();
  });

  it('does not start device capture when configuration sections mount', () => {
    render(<VoiceAudioSection />);
    expect(mockStartTest).not.toHaveBeenCalled();
    expect(mockPlayOutputTest).not.toHaveBeenCalled();
    expect(mockToggleCameraTest).not.toHaveBeenCalled();
  });

  it('stops audio device tests when Audio Configuration closes', () => {
    render(<VoiceAudioSection />);
    const audio = screen.getByText('Audio Configuration').closest('details')!;
    audio.open = true;
    fireEvent(audio, new Event('toggle'));
    audio.open = false;
    fireEvent(audio, new Event('toggle'));
    expect(mockStopTest).toHaveBeenCalledOnce();
    expect(mockStopOutputTest).toHaveBeenCalledOnce();
  });

  it('stops the camera preview when Video Configuration closes', () => {
    render(<VoiceAudioSection />);
    const video = screen.getByText('Video Configuration').closest('details')!;
    video.open = true;
    fireEvent(video, new Event('toggle'));
    video.open = false;
    fireEvent(video, new Event('toggle'));
    expect(mockStopCameraTest).toHaveBeenCalledOnce();
  });

  it.each([
    ['AGC disabled', { autoGainControl: false, musicMode: false }],
    ['Music Mode enabled', { autoGainControl: true, musicMode: true }],
  ])('shows one labeled Microphone Level range when %s', async (_case, overrides) => {
    const { useDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    const values = {
      autoGainControl: true,
      musicMode: false,
      inputVolume: 100,
      ...overrides,
    };
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) => values[key as keyof typeof values] ?? false
    );
    render(<VoiceAudioSection />);
    const level = screen.getByRole('slider', { name: 'Microphone Level' });
    expect(screen.getAllByRole('slider', { name: 'Microphone Level' })).toHaveLength(1);
    expect(level).toHaveAttribute('min', '0');
    expect(level).toHaveAttribute('max', '200');
    expect(level).toHaveAttribute('step', '1');
    expect(level).toHaveValue('100');
    expect(screen.getByText('100%')).toBeInTheDocument();
    const agcRow = screen.getByText('Auto Gain Control').closest('.settings-row');
    expect(agcRow?.nextElementSibling).toBe(level.closest('.settings-volume-row'));
  });

  it('drafts Microphone Level changes in app percentage points', async () => {
    const { useDraftAudioSetting, setDraftAudioSetting } =
      await import('@/renderer/hooks/ui/useDraftSettings');
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) =>
        ({ autoGainControl: false, musicMode: false, inputVolume: 100 })[key] ?? false
    );
    render(<VoiceAudioSection />);
    fireEvent.change(screen.getByRole('slider', { name: 'Microphone Level' }), {
      target: { value: '200' },
    });
    expect(setDraftAudioSetting).toHaveBeenCalledWith('inputVolume', 200);
  });

  it('hides Microphone Level while AGC is effective', async () => {
    const { useDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) => ({ autoGainControl: true, musicMode: false, inputVolume: 100 })[key] ?? false
    );
    render(<VoiceAudioSection />);
    expect(screen.queryByRole('slider', { name: 'Microphone Level' })).not.toBeInTheDocument();
  });
  it('renders output volume label', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Output Volume')).toBeInTheDocument();
  });
  it('calls setDraftAudioSetting on output volume change', async () => {
    const { setDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    render(<VoiceAudioSection />);
    const outputRow = screen.getByText('Output Volume').closest('.settings-volume-row')!;
    fireEvent.change(within(outputRow as HTMLElement).getByRole('slider'), {
      target: { value: '80' },
    });
    expect(setDraftAudioSetting).toHaveBeenCalledWith('outputVolume', 80);
  });
  it('renders microphone test button', () => {
    render(<VoiceAudioSection />);
    // Multiple "Test" buttons exist now (mic, output, camera) — target the mic one.
    expect(document.querySelector('.settings-mic-test-btn')).toBeInTheDocument();
  });
  it('calls startTest when test button clicked', () => {
    render(<VoiceAudioSection />);
    fireEvent.click(document.querySelector('.settings-mic-test-btn')!);
    expect(mockStartTest).toHaveBeenCalled();
  });
  it('shows Stop Testing when mic test active', async () => {
    const { useMicTest } = await import('@/renderer/hooks/device/useMicTest');
    (useMicTest as ReturnType<typeof vi.fn>).mockReturnValue({
      isTesting: true,
      dbfsLevel: -40,
      error: null,
      startTest: mockStartTest,
      stopTest: mockStopTest,
    });
    render(<VoiceAudioSection />);
    expect(screen.getByText('Stop Testing')).toBeInTheDocument();
  });
  it('shows meter when mic test active', async () => {
    const { useMicTest } = await import('@/renderer/hooks/device/useMicTest');
    (useMicTest as ReturnType<typeof vi.fn>).mockReturnValue({
      isTesting: true,
      dbfsLevel: -40,
      error: null,
      startTest: mockStartTest,
      stopTest: mockStopTest,
    });
    render(<VoiceAudioSection />);
    expect(document.querySelector('.settings-mic-meter-container')).toBeInTheDocument();
  });
  it('shows mic test error', async () => {
    const { useMicTest } = await import('@/renderer/hooks/device/useMicTest');
    (useMicTest as ReturnType<typeof vi.fn>).mockReturnValue({
      isTesting: false,
      dbfsLevel: -80,
      error: 'Microphone access denied',
      startTest: mockStartTest,
      stopTest: mockStopTest,
    });
    render(<VoiceAudioSection />);
    expect(screen.getByText('Microphone access denied')).toBeInTheDocument();
  });
  it('keeps mic test button enabled during voice call', async () => {
    const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
    (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          qualityTier: 'standard',
          setQualityTier: mockSetQualityTier,
          activeCameraCodec: null,
          activeScreenCodec: null,
          connectionState: 'connected',
        })
    );
    render(<VoiceAudioSection />);
    expect(document.querySelector('.settings-mic-test-btn')).not.toBeDisabled();
  });
  it('renders quality section', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Quality')).toBeInTheDocument();
    expect(screen.getByText('Standard')).toBeInTheDocument();
  });
  it('renders all quality tier labels', () => {
    render(<VoiceAudioSection />);
    for (const l of ['Minimum', 'Low', 'Moderate', 'Standard', 'High', 'Hi-Fi', 'Studio'])
      expect(screen.getByText(l)).toBeInTheDocument();
  });
  it('renders tier slider', () => {
    render(<VoiceAudioSection />);
    const s = document.querySelector('.settings-tier-slider');
    expect(s).toHaveAttribute('min', '0');
    expect(s).toHaveAttribute('max', '6');
  });
  it('renders processing toggles', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Noise Cancellation')).toBeInTheDocument();
    expect(screen.getByText('Echo Cancellation')).toBeInTheDocument();
    expect(screen.getByText('Auto Gain Control')).toBeInTheDocument();
    expect(screen.getByText('Input Noise Gate')).toBeInTheDocument();
    expect(screen.getByText('Boost Quiet Users')).toBeInTheDocument();
  });
  it('renders an audio and a video mode radio group', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByRole('group', { name: 'Audio settings mode' })).toBeInTheDocument();
    expect(screen.getByRole('group', { name: 'Video settings mode' })).toBeInTheDocument();
  });
  it('audio Basic radio is checked by default', () => {
    render(<VoiceAudioSection />);
    const audio = screen.getByRole('group', { name: 'Audio settings mode' });
    expect(within(audio).getByRole('radio', { name: 'Basic Settings' })).toBeChecked();
  });
  it('audio Advanced radio is unchecked by default', () => {
    render(<VoiceAudioSection />);
    const audio = screen.getByRole('group', { name: 'Audio settings mode' });
    expect(within(audio).getByRole('radio', { name: 'Advanced Settings' })).not.toBeChecked();
  });
  it('hides advanced sections in basic mode', () => {
    render(<VoiceAudioSection />);
    expect(screen.queryByText('Opus Codec')).not.toBeInTheDocument();
  });
  it('shows advanced sections when enabled', async () => {
    setAudioAdvancedMode(true);
    render(<VoiceAudioSection />);
    expect(screen.getByText('Opus Codec')).toBeInTheDocument();
    expect(screen.getByText('Music Mode')).toBeInTheDocument();
  });

  // ===== Advanced audio settings visibility =====

  // Note: Additional advanced audio mode tests omitted — mock store re-implementation
  // conflicts with vi.mock() module cache. The "shows advanced sections when enabled"
  // test above validates the advanced mode toggle. Deeper advanced-section tests
  // require a test infrastructure refactor (real stores or factory mocks).

  // ===== Video configuration =====

  it('renders camera preset selector', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Camera Preset')).toBeInTheDocument();
  });

  it('renders screen share settings', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Screen Share')).toBeInTheDocument();
  });

  it('renders screen resolution setting', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Resolution')).toBeInTheDocument();
  });

  it('renders frame rate setting', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Frame Rate')).toBeInTheDocument();
  });

  it('renders content type setting', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Content Type')).toBeInTheDocument();
  });

  // ===== Video advanced mode =====

  // Note: Codec grid and video advanced mode tests omitted — same mock cache issue.
  // The component's video advanced sections (codec grid, HW accel, QoS) are validated
  // by the existing "shows advanced sections" pattern. Deeper tests need factory mocks.

  // HW accel, congestion priority, and QoS tests removed — same mock cache limitation.

  // ===== Processing toggles =====

  it('renders noise cancellation toggle', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Noise Cancellation')).toBeInTheDocument();
  });

  it('renders echo cancellation toggle', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Echo Cancellation')).toBeInTheDocument();
  });

  it('renders auto gain control toggle', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Auto Gain Control')).toBeInTheDocument();
  });

  it('renders noise gate setting', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Input Noise Gate')).toBeInTheDocument();
  });

  // ===== GPU vendor icon =====

  // GPU vendor icon test removed — same mock cache limitation.

  // ===== Mode radio groups: independence =====

  // Both groups render on one page, and radios that share a `name` form ONE group
  // however they are nested. Arrow keys walk the whole group, so with a shared name
  // ArrowRight from the last audio option would land in the video control instead of
  // wrapping to the first audio option. (A render-only check cannot catch a shared
  // name here: jsdom never unchecks the rest of a group when a checked radio is
  // attached, so both groups still read as checked. Chromium would uncheck one.)
  it('arrow keys wrap within the audio group, never into the video group', async () => {
    const user = userEvent.setup();
    render(<VoiceAudioSection />);
    const audio = screen.getByRole('group', { name: 'Audio settings mode' });
    within(audio).getByRole('radio', { name: 'Advanced Settings' }).focus();
    await user.keyboard('{ArrowRight}');
    expect(within(audio).getByRole('radio', { name: 'Basic Settings' })).toHaveFocus();
  });

  // ===== Quiet boost setting =====

  it('renders quiet boost toggle', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Boost Quiet Users')).toBeInTheDocument();
  });

  // ===== Quality tier description =====

  it('renders quality tier description in basic mode', () => {
    render(<VoiceAudioSection />);
    // The basic-mode description for the Standard tier. (This used to match /96 kbps/,
    // which only passed while an earlier test's advanced-mode override leaked here.)
    expect(screen.getByText(/The Concord default/)).toBeInTheDocument();
  });

  it('renders kbps label', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('96 kbps')).toBeInTheDocument();
  });

  it('renders premium badge description text', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText(/Higher quality uses more bandwidth/)).toBeInTheDocument();
  });

  // ===== Slider interaction =====

  it('fires setQualityTier when tier slider changes', async () => {
    render(<VoiceAudioSection />);
    const slider = document.querySelector('.settings-tier-slider') as HTMLInputElement;
    expect(slider).toBeInTheDocument();
    fireEvent.change(slider, { target: { value: '0' } });
    expect(mockSetQualityTier).toHaveBeenCalledWith('minimum');
  });

  // ===== Mode radio semantics =====

  it('exposes the mode options as native radios, never as tabs', () => {
    render(<VoiceAudioSection />);
    expect(screen.getAllByRole('radio', { name: 'Advanced Settings' })).toHaveLength(2);
    expect(screen.queryAllByRole('tab', { name: /^(Basic|Advanced) Settings$/ })).toHaveLength(0);
  });

  // ===== Volume slider rendering =====

  it('renders volume percentages', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Output Volume')).toBeInTheDocument();
    expect(screen.getByText(/Scales all incoming audio/)).toBeInTheDocument();
  });

  it('renders Output Volume hint text', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText(/Scales all incoming audio/)).toBeInTheDocument();
  });

  // ===== Camera section =====

  it('renders Camera subsection title', () => {
    render(<VoiceAudioSection />);
    // Camera also labels its selector.
    expect(screen.getAllByText('Camera').length).toBeGreaterThanOrEqual(1);
  });

  it('renders video input device selector', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByRole('combobox', { name: 'Camera' })).toBeInTheDocument();
  });

  // ===== Processing hints =====

  it('renders noise cancellation hint text', () => {
    render(<VoiceAudioSection />);
    expect(
      screen.getByText(/Background noise from your microphone is actively filtered/)
    ).toBeInTheDocument();
  });

  it('renders echo cancellation hint text', () => {
    render(<VoiceAudioSection />);
    expect(
      screen.getByText(/Acoustic echo cancellation prevents your speakers from feeding back/)
    ).toBeInTheDocument();
  });

  it('renders auto gain control hint text', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText(/Automatically normalizes your microphone volume/)).toBeInTheDocument();
  });

  // ===== Screen share section =====

  it('renders screen resolution label', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Resolution')).toBeInTheDocument();
  });

  it('renders frame rate label', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Frame Rate')).toBeInTheDocument();
  });

  it('renders content type label', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Content Type')).toBeInTheDocument();
  });

  it('renders camera preset label', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Camera Preset')).toBeInTheDocument();
  });

  // Note: stopTest interaction test omitted — same vi.mock() module cache issue
  // documented in the existing advanced mode tests. The "shows Stop Testing when
  // mic test active" test above validates the state-driven UI change.

  // ===== AudioConfigSection: Advanced mode features =====

  it('shows advanced audio features: FEC, NACK, Silence Detection, Frame Size, QoS, Stereo', async () => {
    setAudioAdvancedMode(true);
    render(<VoiceAudioSection />);
    expect(screen.getByText('Music Mode')).toBeInTheDocument();
    expect(screen.getByText('Adaptive Frame Size (AFS)')).toBeInTheDocument();
    expect(screen.getByText('Frame Size (ptime)')).toBeInTheDocument();
    expect(screen.getByText('Silence Detection (DTX)')).toBeInTheDocument();
    expect(screen.getByText('Mic Channel Mode')).toBeInTheDocument();
    expect(screen.getByText('In-Line Forward Error Correction (FEC)')).toBeInTheDocument();
    expect(screen.getByText('NACK (Retransmission)')).toBeInTheDocument();
    expect(screen.getByText('Quality of Service for Audio')).toBeInTheDocument();
  });

  it('shows Error Correction & Reliability and Transport subsection headers in advanced mode', async () => {
    setAudioAdvancedMode(true);
    render(<VoiceAudioSection />);
    expect(screen.getByText('Error Correction & Reliability')).toBeInTheDocument();
    expect(screen.getByText('Transport')).toBeInTheDocument();
  });

  it('shows advanced mode notice banner', async () => {
    setAudioAdvancedMode(true);
    render(<VoiceAudioSection />);
    expect(
      screen.getByText(/These settings override the quality tier presets/)
    ).toBeInTheDocument();
  });

  // ===== AudioConfigSection: Noise gate manual mode =====

  it('shows gate threshold slider when noise gate is manual', async () => {
    const { useDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) =>
        ({
          stereoOverride: null,
          noiseCancellation: true,
          echoCancellation: true,
          autoGainControl: true,
          noiseGateMode: 'manual',
          noiseGateLevel: -50,
          inputVolume: 100,
          outputVolume: 100,
          quietBoost: false,
          quietBoostThreshold: -38,
          musicMode: false,
          frameSize: 0,
          silenceDetection: true,
          inlineFec: true,
          fecHeadroom: true,
          opusNack: false,
          adaptivePtime: true,
          audioPriority: 'medium',
        })[key] ?? false
    );
    render(<VoiceAudioSection />);
    expect(screen.getByText('Gate Threshold')).toBeInTheDocument();
    expect(screen.getByText('-50 dBFS')).toBeInTheDocument();
  });

  // ===== AudioConfigSection: Quiet boost enabled =====

  it('shows boost threshold slider when quiet boost is enabled', async () => {
    const { useDraftAudioSetting } = await import('@/renderer/hooks/ui/useDraftSettings');
    (useDraftAudioSetting as ReturnType<typeof vi.fn>).mockImplementation(
      (key: string) =>
        ({
          stereoOverride: null,
          noiseCancellation: true,
          echoCancellation: true,
          autoGainControl: true,
          noiseGateMode: 'auto',
          noiseGateLevel: -50,
          inputVolume: 100,
          outputVolume: 100,
          quietBoost: true,
          quietBoostThreshold: -38,
          musicMode: false,
          frameSize: 0,
          silenceDetection: true,
          inlineFec: true,
          fecHeadroom: true,
          opusNack: false,
          adaptivePtime: true,
          audioPriority: 'medium',
        })[key] ?? false
    );
    render(<VoiceAudioSection />);
    expect(screen.getByText('Boost Threshold')).toBeInTheDocument();
    expect(screen.getByText('-38 dBFS')).toBeInTheDocument();
  });

  // ===== VideoConfigSection: Basic mode rendering =====

  it('renders video basic mode elements: camera preset, resolution, frame rate, content type', () => {
    render(<VoiceAudioSection />);
    expect(screen.getByText('Camera Preset')).toBeInTheDocument();
    expect(screen.getByText('Resolution')).toBeInTheDocument();
    expect(screen.getByText('Frame Rate')).toBeInTheDocument();
    expect(screen.getByText('Content Type')).toBeInTheDocument();
    expect(screen.getByText('Screen Share')).toBeInTheDocument();
  });

  it('does not show advanced video features in basic mode', () => {
    render(<VoiceAudioSection />);
    expect(screen.queryByText('Hardware Acceleration')).not.toBeInTheDocument();
    expect(screen.queryByText('Congestion Priority')).not.toBeInTheDocument();
    expect(screen.queryByText('Enable HDR')).not.toBeInTheDocument();
    expect(screen.queryByText('Prioritize HDR Codecs')).not.toBeInTheDocument();
    expect(screen.queryByText('Codec & Hardware')).not.toBeInTheDocument();
  });

  // ===== VideoConfigSection: Advanced mode rendering =====

  it('shows video advanced features when videoAdvancedMode is true', async () => {
    const { useVideoSettingsStore } = await import('@/renderer/stores/voice/videoSettingsStore');
    (useVideoSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          codecCapabilities: [],
          gpuInfo: null,
          videoAdvancedMode: true,
          systemHdr: false,
        })
    );
    render(<VoiceAudioSection />);
    expect(screen.getByText('Hardware Acceleration')).toBeInTheDocument();
    expect(screen.getByText('Enable HDR')).toBeInTheDocument();
    expect(screen.getByText('Prioritize HDR Codecs')).toBeInTheDocument();
    expect(screen.getByText('Congestion Priority')).toBeInTheDocument();
    expect(screen.getByText('Video Codec')).toBeInTheDocument();
    expect(screen.getByText('Codec & Hardware')).toBeInTheDocument();
  });

  it('shows video transport QoS selectors in advanced mode', async () => {
    const { useVideoSettingsStore } = await import('@/renderer/stores/voice/videoSettingsStore');
    (useVideoSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          codecCapabilities: [],
          gpuInfo: null,
          videoAdvancedMode: true,
          systemHdr: false,
        })
    );
    render(<VoiceAudioSection />);
    expect(screen.getByText('Quality of Service for Camera')).toBeInTheDocument();
    expect(screen.getByText('Quality of Service for Screen Share')).toBeInTheDocument();
  });

  it('shows Bandwidth subsection in video advanced mode', async () => {
    const { useVideoSettingsStore } = await import('@/renderer/stores/voice/videoSettingsStore');
    (useVideoSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (s: (state: Record<string, unknown>) => unknown) =>
        s({
          codecCapabilities: [],
          gpuInfo: null,
          videoAdvancedMode: true,
          systemHdr: false,
        })
    );
    render(<VoiceAudioSection />);
    expect(screen.getByText('Bandwidth')).toBeInTheDocument();
    expect(screen.getByText('Automatic Bitrate')).toBeInTheDocument();
  });
});
