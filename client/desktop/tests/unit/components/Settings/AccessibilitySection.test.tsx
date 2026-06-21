import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';

// ── Mocks ──────────────────────────────────────────────────────────────

const mockSetDraftTtsSetting = vi.fn();
const mockSetDraftAppearanceSetting = vi.fn();

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftTtsSetting: vi.fn((key: string) => {
    const defaults: Record<string, unknown> = {
      ttsEnabled: false,
      ttsVoice: '',
      ttsRate: 1,
      ttsVolume: 1,
    };
    return defaults[key];
  }),
  setDraftTtsSetting: (...args: unknown[]) => mockSetDraftTtsSetting(...args),
  // #489 — DisplaySection lives here now; consumes useDraftAppearance.
  // Default to the same baseline as the real store so the section renders
  // without throwing in tests that don't override.
  useDraftAppearance: vi.fn(() => ({
    theme: 'dark',
    colorScheme: 'concord',
    fontSize: 'default',
    compactMode: false,
    reduceAnimations: false,
    uiScale: 1,
    highContrast: false,
    dyslexicSupport: false,
    customColors: null,
  })),
  setDraftAppearanceSetting: (...args: unknown[]) => mockSetDraftAppearanceSetting(...args),
}));

vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    options,
    value,
    onChange,
    className,
  }: {
    options: { value: string; label: string }[];
    value: string;
    onChange: (v: string) => void;
    className?: string;
  }) => (
    <select
      data-testid="custom-select"
      className={className}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  ),
}));

const mockSpeak = vi.fn();
const mockCancel = vi.fn();
const mockGetVoices = vi.fn().mockReturnValue([
  {
    voiceURI: 'en-US-Standard',
    name: 'English US',
    lang: 'en-US',
    localService: true,
    default: true,
  },
  {
    voiceURI: 'en-GB-Standard',
    name: 'English UK',
    lang: 'en-GB',
    localService: true,
    default: false,
  },
]);

// jsdom lacks SpeechSynthesisUtterance — provide a minimal stub
if (typeof globalThis.SpeechSynthesisUtterance === 'undefined') {
  (globalThis as Record<string, unknown>).SpeechSynthesisUtterance = class {
    text: string;
    rate = 1;
    volume = 1;
    voice: unknown = null;
    constructor(text?: string) {
      this.text = text ?? '';
    }
  };
}

Object.defineProperty(globalThis, 'speechSynthesis', {
  value: {
    getVoices: mockGetVoices,
    speak: mockSpeak,
    cancel: mockCancel,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  },
  writable: true,
  configurable: true,
});

import AccessibilitySection from '@/renderer/components/Settings/AccessibilitySection';

describe('AccessibilitySection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders Text-to-Speech section title', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Text-to-Speech')).toBeInTheDocument();
  });

  it('renders the Enable Dyslexic Support toggle', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Enable Dyslexic Support')).toBeInTheDocument();
  });

  it('toggling Dyslexic Support writes ONLY dyslexicSupport (never appFont — Q2-restore)', () => {
    render(<AccessibilitySection />);
    const row = screen.getByText('Enable Dyslexic Support').closest('.settings-row');
    const checkbox = row?.querySelector('input[type="checkbox"]');
    fireEvent.click(checkbox!);
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('dyslexicSupport', true);
    // Q2 invariant: the toggle must never touch appFont (would corrupt the prior pick).
    expect(mockSetDraftAppearanceSetting).not.toHaveBeenCalledWith('appFont', expect.anything());
  });

  it('renders TTS section description', () => {
    render(<AccessibilitySection />);
    expect(
      screen.getByText("Read voice text chat messages aloud while you're in a voice channel.")
    ).toBeInTheDocument();
  });

  it('renders Enable TTS Playback toggle', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Enable TTS Playback')).toBeInTheDocument();
  });

  it('renders Voice selector', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Voice')).toBeInTheDocument();
  });

  it('renders Speed slider', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Speed')).toBeInTheDocument();
  });

  it('renders Volume slider', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Volume')).toBeInTheDocument();
  });

  it('renders Preview button', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('Preview')).toBeInTheDocument();
  });

  it('shows disabled hint when TTS is off', () => {
    render(<AccessibilitySection />);
    expect(
      screen.getByText('Disabled. Voice text chat messages are displayed as text only.')
    ).toBeInTheDocument();
  });

  it('shows enabled hint when TTS is on', async () => {
    const { useDraftTtsSetting } = await import('@/renderer/hooks/useDraftSettings');
    (useDraftTtsSetting as ReturnType<typeof vi.fn>).mockImplementation((key: string) => {
      if (key === 'ttsEnabled') return true;
      if (key === 'ttsVoice') return '';
      if (key === 'ttsRate') return 1;
      if (key === 'ttsVolume') return 1;
      return undefined;
    });

    render(<AccessibilitySection />);
    expect(
      screen.getByText(
        'Enabled. Incoming voice text chat messages are read aloud while you are in a voice channel.'
      )
    ).toBeInTheDocument();
  });

  it('calls setDraftTtsSetting when TTS toggle is clicked', () => {
    render(<AccessibilitySection />);
    const row = screen.getByText('Enable TTS Playback').closest('.settings-row');
    const checkbox = row?.querySelector('input[type="checkbox"]');
    expect(checkbox).toBeInTheDocument();
    fireEvent.click(checkbox!);
    expect(mockSetDraftTtsSetting).toHaveBeenCalledWith('ttsEnabled', expect.any(Boolean));
  });

  it('renders System Default option in voice selector', () => {
    render(<AccessibilitySection />);
    const select = screen.getByTestId('custom-select');
    expect(select).toBeInTheDocument();
    const options = select.querySelectorAll('option');
    expect(options[0].textContent).toBe('System Default');
  });

  it('renders available voices from speechSynthesis', () => {
    render(<AccessibilitySection />);
    const select = screen.getByTestId('custom-select');
    const options = select.querySelectorAll('option');
    expect(options.length).toBe(3);
    expect(options[1].textContent).toBe('English US (en-US)');
    expect(options[2].textContent).toBe('English UK (en-GB)');
  });

  it('calls setDraftTtsSetting when voice is changed', () => {
    render(<AccessibilitySection />);
    const select = screen.getByTestId('custom-select');
    fireEvent.change(select, { target: { value: 'en-US-Standard' } });
    expect(mockSetDraftTtsSetting).toHaveBeenCalledWith('ttsVoice', 'en-US-Standard');
  });

  it('shows currently selected voice name in hint', async () => {
    const { useDraftTtsSetting } = await import('@/renderer/hooks/useDraftSettings');
    (useDraftTtsSetting as ReturnType<typeof vi.fn>).mockImplementation((key: string) => {
      if (key === 'ttsEnabled') return false;
      if (key === 'ttsVoice') return 'en-US-Standard';
      if (key === 'ttsRate') return 1;
      if (key === 'ttsVolume') return 1;
      return undefined;
    });

    render(<AccessibilitySection />);
    expect(screen.getByText(/Currently using English US/)).toBeInTheDocument();
  });

  it('displays current speed value', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText('1.0x')).toBeInTheDocument();
  });

  it('renders speed slider with correct range', () => {
    render(<AccessibilitySection />);
    const sliders = document.querySelectorAll('.settings-slider');
    const speedSlider = sliders[0];
    expect(speedSlider).toHaveAttribute('min', '0.5');
    expect(speedSlider).toHaveAttribute('max', '2');
    expect(speedSlider).toHaveAttribute('step', '0.1');
  });

  it('calls setDraftTtsSetting when speed slider changes', () => {
    render(<AccessibilitySection />);
    const sliders = document.querySelectorAll('.settings-slider');
    fireEvent.change(sliders[0], { target: { value: '1.5' } });
    expect(mockSetDraftTtsSetting).toHaveBeenCalledWith('ttsRate', 1.5);
  });

  it('displays current volume as percentage', () => {
    render(<AccessibilitySection />);
    // #489 — DisplaySection's UI Scale also renders "100%" by default, so
    // `getByText('100%')` finds two. Assert at least one is present (both
    // are intentional — TTS volume + UI scale value).
    const matches = screen.getAllByText('100%');
    expect(matches.length).toBeGreaterThanOrEqual(1);
  });

  it('renders volume slider with correct range', () => {
    render(<AccessibilitySection />);
    const sliders = document.querySelectorAll('.settings-slider');
    const volumeSlider = sliders[1];
    expect(volumeSlider).toHaveAttribute('min', '0');
    expect(volumeSlider).toHaveAttribute('max', '1');
    expect(volumeSlider).toHaveAttribute('step', '0.05');
  });

  it('calls setDraftTtsSetting when volume slider changes', () => {
    render(<AccessibilitySection />);
    const sliders = document.querySelectorAll('.settings-slider');
    fireEvent.change(sliders[1], { target: { value: '0.5' } });
    expect(mockSetDraftTtsSetting).toHaveBeenCalledWith('ttsVolume', 0.5);
  });

  it('calls speechSynthesis.speak when Preview is clicked', () => {
    render(<AccessibilitySection />);
    fireEvent.click(screen.getByText('Preview'));
    expect(mockCancel).toHaveBeenCalled();
    expect(mockSpeak).toHaveBeenCalled();
  });

  it('preview uses the current TTS rate and volume', () => {
    render(<AccessibilitySection />);
    fireEvent.click(screen.getByText('Preview'));
    const utterance = mockSpeak.mock.calls[0][0];
    expect(utterance).toBeTruthy();
    expect(utterance.text).toBe('This is a preview of text-to-speech in Concord Voice.');
    expect(utterance.rate).toBe(1);
    expect(utterance.volume).toBe(1);
  });

  it('speed description mentions playback speed', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText(/Playback speed for text-to-speech/)).toBeInTheDocument();
  });

  it('volume description mentions volume level', () => {
    render(<AccessibilitySection />);
    expect(screen.getByText(/Volume level for text-to-speech playback/)).toBeInTheDocument();
  });
});
