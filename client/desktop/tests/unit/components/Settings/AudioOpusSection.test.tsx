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
    hifi: {
      preferredFrameSize: 10,
      opusStereo: true,
      label: 'Hi-Fi',
      maxBitrate: 256000,
      opusDtx: false,
      opusFec: false,
      premium: true,
    },
  },
}));

vi.mock('@/renderer/stores/audioSettingsStore', () => ({}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAudioSetting: vi.fn((key: string) => defaultSettings[key] ?? false),
  setDraftAudioSetting: (...args: unknown[]) => mockSetDraftAudioSetting(...args),
}));

vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    options,
    value,
    onChange,
    disabled,
    className,
  }: {
    options: { value: string; label: string }[];
    value: string;
    onChange: (v: string) => void;
    disabled?: boolean;
    className?: string;
  }) => (
    <select
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

// ── Imports (after mocks) ───────────────────────────────────────────────

import { render, screen, fireEvent } from '../../../test-utils';
import { useDraftAudioSetting } from '@/renderer/hooks/useDraftSettings';
import AudioOpusSection, {
  frameSizeHint,
  qosPriorityHint,
} from '@/renderer/components/Settings/AudioOpusSection';

// ── Helpers ─────────────────────────────────────────────────────────────

const mockUseDraftAudioSetting = vi.mocked(useDraftAudioSetting);

function overrideMock(overrides: Record<string, unknown>) {
  mockUseDraftAudioSetting.mockImplementation(
    (key: string) => ({ ...defaultSettings, ...overrides })[key] as never
  );
}

function getCheckboxByLabel(labelText: string): HTMLInputElement {
  const label = screen.getByText(labelText);
  const row = label.closest('.settings-row')!;
  return row.querySelector('input[type="checkbox"]') as HTMLInputElement;
}

function getSelectByIndex(index: number): HTMLSelectElement {
  return screen.getAllByTestId('custom-select')[index] as HTMLSelectElement;
}

// ── Tests ───────────────────────────────────────────────────────────────

describe('AudioOpusSection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    overrideMock({});
  });

  // ── frameSizeHint (exported function) ───────────────────────────────

  describe('frameSizeHint', () => {
    it('returns adaptive lock message when adaptivePtime is true', () => {
      expect(frameSizeHint(true, 20, 20)).toContain('Locked by Adaptive Frame Size');
    });

    it('returns preferred size message when frameSize is 0', () => {
      expect(frameSizeHint(false, 0, 20)).toContain(
        "Currently using the tier's preferred size (20ms)"
      );
    });

    it('returns 10ms hint', () => {
      expect(frameSizeHint(false, 10, 20)).toContain('Currently 10ms');
      expect(frameSizeHint(false, 10, 20)).toContain('lowest latency');
    });

    it('returns 20ms hint', () => {
      expect(frameSizeHint(false, 20, 20)).toContain('Currently 20ms');
      expect(frameSizeHint(false, 20, 20)).toContain('standard balance');
    });

    it('returns 40ms hint', () => {
      expect(frameSizeHint(false, 40, 20)).toContain('Currently 40ms');
      expect(frameSizeHint(false, 40, 20)).toContain('reduced overhead');
    });

    it('returns 60ms hint (default branch)', () => {
      expect(frameSizeHint(false, 60, 20)).toContain('Currently 60ms');
      expect(frameSizeHint(false, 60, 20)).toContain('maximum efficiency');
    });
  });

  // ── qosPriorityHint (exported function) ─────────────────────────────

  describe('qosPriorityHint', () => {
    it('returns off hint', () => {
      expect(qosPriorityHint('off')).toContain('Currently off');
    });

    it('returns low hint', () => {
      expect(qosPriorityHint('low')).toContain('Currently Low (DF)');
    });

    it('returns medium hint', () => {
      expect(qosPriorityHint('medium')).toContain('Currently Default (AF41)');
    });

    it('returns high hint (default branch)', () => {
      expect(qosPriorityHint('high')).toContain('Currently High (EF)');
    });
  });

  // ── Music Mode toggle ───────────────────────────────────────────────

  describe('Music Mode', () => {
    it('shows disabled hint when musicMode is false', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Standard voice processing is active/)).toBeInTheDocument();
    });

    it('shows enabled hint when musicMode is true', () => {
      overrideMock({ musicMode: true });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Noise suppression, echo cancellation/)).toBeInTheDocument();
    });

    it('calls setDraftAudioSetting when toggled', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const checkbox = getCheckboxByLabel('Music Mode');
      fireEvent.click(checkbox);
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('musicMode', true);
    });
  });

  // ── Adaptive Frame Size toggle ──────────────────────────────────────

  describe('Adaptive Frame Size', () => {
    it('shows disabled hint when adaptivePtime is false', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(
        screen.getByText(/Packet duration is fixed at the configured Frame Size/)
      ).toBeInTheDocument();
    });

    it('shows enabled hint when adaptivePtime is true', () => {
      overrideMock({ adaptivePtime: true });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/dynamically adjusts packet duration/)).toBeInTheDocument();
    });

    it('disables Frame Size select when adaptivePtime is true', () => {
      overrideMock({ adaptivePtime: true });
      render(<AudioOpusSection qualityTier="standard" />);
      // Frame Size select is the first custom-select
      const frameSizeSelect = getSelectByIndex(0);
      expect(frameSizeSelect).toBeDisabled();
    });

    it('enables Frame Size select when adaptivePtime is false', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const frameSizeSelect = getSelectByIndex(0);
      expect(frameSizeSelect).not.toBeDisabled();
    });

    it('calls setDraftAudioSetting when toggled', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const checkbox = getCheckboxByLabel('Adaptive Frame Size (AFS)');
      fireEvent.click(checkbox);
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('adaptivePtime', true);
    });
  });

  // ── Frame Size select ───────────────────────────────────────────────

  describe('Frame Size select', () => {
    it('calls setDraftAudioSetting with numeric value on change', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const frameSizeSelect = getSelectByIndex(0);
      fireEvent.change(frameSizeSelect, { target: { value: '40' } });
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('frameSize', 40);
    });

    it('shows tier preferred size in default option label', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText('Default (20 ms)')).toBeInTheDocument();
    });

    it('shows hifi tier preferred size in default option label', () => {
      render(<AudioOpusSection qualityTier="hifi" />);
      expect(screen.getByText('Default (10 ms)')).toBeInTheDocument();
    });
  });

  // ── Silence Detection toggle ────────────────────────────────────────

  describe('Silence Detection', () => {
    it('shows enabled hint when silenceDetection is true', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/discontinuous transmission/)).toBeInTheDocument();
    });

    it('shows disabled hint when silenceDetection is false', () => {
      overrideMock({ silenceDetection: false });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Packets are sent continuously/)).toBeInTheDocument();
    });

    it('calls setDraftAudioSetting when toggled', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const checkbox = getCheckboxByLabel('Silence Detection (DTX)');
      fireEvent.click(checkbox);
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('silenceDetection', false);
    });
  });

  // ── Mic Channel Mode select ─────────────────────────────────────────

  describe('Mic Channel Mode', () => {
    it('defaults to mono for standard tier with stereoOverride=null', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Currently Mono/)).toBeInTheDocument();
    });

    it('defaults to stereo for hifi tier with stereoOverride=null', () => {
      render(<AudioOpusSection qualityTier="hifi" />);
      expect(screen.getByText(/Currently Stereo/)).toBeInTheDocument();
    });

    it('shows stereo when stereoOverride is true', () => {
      overrideMock({ stereoOverride: true });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Currently Stereo/)).toBeInTheDocument();
    });

    it('shows mono when stereoOverride is false', () => {
      overrideMock({ stereoOverride: false });
      render(<AudioOpusSection qualityTier="hifi" />);
      expect(screen.getByText(/Currently Mono/)).toBeInTheDocument();
    });

    it('calls setDraftAudioSetting with true for stereo selection', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      // Mic Channel Mode is the second custom-select
      const micSelect = getSelectByIndex(1);
      fireEvent.change(micSelect, { target: { value: 'stereo' } });
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('stereoOverride', true);
    });

    it('calls setDraftAudioSetting with false for mono selection', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const micSelect = getSelectByIndex(1);
      fireEvent.change(micSelect, { target: { value: 'mono' } });
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('stereoOverride', false);
    });
  });

  // ── FEC toggle ──────────────────────────────────────────────────────

  describe('In-Line FEC', () => {
    it('shows enabled hint when inlineFec is true', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Every packet includes a low-quality copy/)).toBeInTheDocument();
    });

    it('shows disabled hint when inlineFec is false', () => {
      overrideMock({ inlineFec: false });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Packets carry only primary audio/)).toBeInTheDocument();
    });

    it('calls setDraftAudioSetting when toggled', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const checkbox = getCheckboxByLabel('In-Line Forward Error Correction (FEC)');
      fireEvent.click(checkbox);
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('inlineFec', false);
    });
  });

  // ── FEC Headroom (conditional render) ───────────────────────────────

  describe('FEC Headroom', () => {
    it('renders FEC Headroom row when inlineFec is true', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText('Grant FEC Headroom')).toBeInTheDocument();
    });

    it('does not render FEC Headroom row when inlineFec is false', () => {
      overrideMock({ inlineFec: false });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.queryByText('Grant FEC Headroom')).not.toBeInTheDocument();
    });

    it('shows disabled hint when fecHeadroom is false', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/The bitrate ceiling stays fixed/)).toBeInTheDocument();
    });

    it('shows enabled hint when fecHeadroom is true', () => {
      overrideMock({ fecHeadroom: true });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/the bitrate ceiling is raised/)).toBeInTheDocument();
    });

    it('calls setDraftAudioSetting when toggled', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const checkbox = getCheckboxByLabel('Grant FEC Headroom');
      fireEvent.click(checkbox);
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('fecHeadroom', true);
    });
  });

  // ── NACK toggle ─────────────────────────────────────────────────────

  describe('NACK', () => {
    it('shows disabled hint when opusNack is false', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/Lost packets are not retransmitted/)).toBeInTheDocument();
    });

    it('shows enabled hint when opusNack is true', () => {
      overrideMock({ opusNack: true });
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText(/requests retransmission of lost audio packets/)).toBeInTheDocument();
    });

    it('calls setDraftAudioSetting when toggled', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const checkbox = getCheckboxByLabel('NACK (Retransmission)');
      fireEvent.click(checkbox);
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('opusNack', true);
    });
  });

  // ── QoS Audio select ────────────────────────────────────────────────

  describe('QoS Audio', () => {
    it('renders with current audioPriority value', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      // QoS select is the third custom-select
      const qosSelect = getSelectByIndex(2);
      expect(qosSelect.value).toBe('medium');
    });

    it('calls setDraftAudioSetting on change', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      const qosSelect = getSelectByIndex(2);
      fireEvent.change(qosSelect, { target: { value: 'high' } });
      expect(mockSetDraftAudioSetting).toHaveBeenCalledWith('audioPriority', 'high');
    });

    it('renders all QoS options', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText('Off (No Tagging)')).toBeInTheDocument();
      expect(screen.getByText('Low (DF)')).toBeInTheDocument();
      expect(screen.getByText('Default (AF41)')).toBeInTheDocument();
      expect(screen.getByText('High (EF)')).toBeInTheDocument();
    });
  });

  // ── Section headings ────────────────────────────────────────────────

  describe('section structure', () => {
    it('renders all subsection titles', () => {
      render(<AudioOpusSection qualityTier="standard" />);
      expect(screen.getByText('Opus Codec')).toBeInTheDocument();
      expect(screen.getByText('Error Correction & Reliability')).toBeInTheDocument();
      expect(screen.getByText('Transport')).toBeInTheDocument();
    });
  });
});
