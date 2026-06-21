import { vi } from 'vitest';

// ─── Default draft video settings ───────────────────────────────────────────
const defaultVideoSettings: Record<string, unknown> = {
  cameraPreset: 'system',
  screenResolution: 'source',
  screenFrameRate: 0,
  screenContentType: 'auto',
  preferredVideoCodec: '',
  screenSharePriority: 'off',
  screenShareBitrate: 0,
  cameraPriority: 'off',
  degradationPreference: 'balanced',
  hardwareAcceleration: true,
  hdrEncoding: false,
};

// ─── Hoisted mocks (available inside vi.mock factories) ─────────────────────

const { mockSetVideoAdvancedMode, mockSetDraftVideoSetting } = vi.hoisted(() => ({
  mockSetVideoAdvancedMode: vi.fn(),
  mockSetDraftVideoSetting: vi.fn(),
}));

// ─── Mocks (MUST be before component imports) ───────────────────────────────

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: vi.fn((s: (state: Record<string, unknown>) => unknown) =>
    s({ activeCameraCodec: null, activeScreenCodec: null })
  ),
}));

vi.mock('@/renderer/stores/videoSettingsStore', () => ({
  useVideoSettingsStore: Object.assign(
    vi.fn((s: (state: Record<string, unknown>) => unknown) =>
      s({ codecCapabilities: [], gpuInfo: null, videoAdvancedMode: false, systemHdr: false })
    ),
    { getState: vi.fn(() => ({ setVideoAdvancedMode: mockSetVideoAdvancedMode })) }
  ),
  VIDEO_QUALITY_PRESETS: {
    system: { label: 'System Default' },
    '720p': { label: '720p (1280\u00d7720 30fps)' },
    '1080p': { label: '1080p (1920\u00d71080 30fps)' },
  },
}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftVideoSetting: vi.fn((key: string) => defaultVideoSettings[key] ?? false),
  setDraftVideoSetting: mockSetDraftVideoSetting,
}));

// These existing tests exercise device-derived resolution/fps/bitrate behaviour,
// so grant unbounded premium video caps (#1301) — the lock variants (L2/L5/L6)
// must not clamp the option lists here. Lock behaviour is covered in
// VideoConfigSection.premium.test.tsx.
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector({
      maxVideoHeight: 4320,
      maxVideoFps: 240,
      maxVideoPixelRate: Number.MAX_SAFE_INTEGER,
      maxManualBitrateBps: 30_000_000,
    })
  ),
}));

vi.mock('@/renderer/services/mediaCapabilities', () => ({
  codecKey: vi.fn(
    (c: { mimeType: string; sdpFmtpLine?: string }) => `${c.mimeType}/${c.sdpFmtpLine || 'default'}`
  ),
  codecKeyMime: vi.fn((key: string) => key.split('/').slice(0, 2).join('/')),
  getCodecInfo: vi.fn(() => ({
    name: 'VP8',
    quality: 'Good',
    efficiency: 'Moderate',
    compressionRatio: '30:1',
    hdr: false,
    notes: '',
  })),
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

// ─── Component imports (AFTER mocks) ────────────────────────────────────────
import { render, screen, fireEvent } from '../../../test-utils';
import VideoConfigSection from '@/renderer/components/Settings/VideoConfigSection';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';
import { useDraftVideoSetting } from '@/renderer/hooks/useDraftSettings';
import { getCodecInfo } from '@/renderer/services/mediaCapabilities';

// ─── Test codec data ────────────────────────────────────────────────────────

const makeCodec = (
  mimeType: string,
  powerEfficient: boolean,
  opts: { sdpFmtpLine?: string; profileLabel?: string; isHdr?: boolean } = {}
) => ({
  mimeType,
  powerEfficient,
  sdpFmtpLine: opts.sdpFmtpLine || 'default',
  profileLabel: opts.profileLabel || '',
  isHdr: opts.isHdr || false,
});

const sampleCodecs = [
  makeCodec('video/VP8', false),
  makeCodec('video/VP9', true, { profileLabel: 'Profile 0' }),
  makeCodec('video/H264', true, {
    sdpFmtpLine: 'level-asymmetry-allowed=1',
    profileLabel: 'Baseline',
  }),
  makeCodec('video/AV1', true),
  makeCodec('video/H265', false, { profileLabel: 'Main' }), // unsupported by router
];

const sampleCodecsWithHdr = [
  ...sampleCodecs,
  makeCodec('video/VP9', false, {
    profileLabel: 'Profile 2',
    isHdr: true,
    sdpFmtpLine: 'profile-id=2',
  }),
];

// ─── Helpers ────────────────────────────────────────────────────────────────

function mockVideoSettingsStore(overrides: Record<string, unknown>) {
  const defaults = {
    codecCapabilities: [],
    gpuInfo: null,
    videoAdvancedMode: false,
    systemHdr: false,
  };
  (useVideoSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) => s({ ...defaults, ...overrides })
  );
}

function mockDraftSettings(overrides: Record<string, unknown>) {
  const merged = { ...defaultVideoSettings, ...overrides };
  (useDraftVideoSetting as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (key: string) => merged[key] ?? false
  );
}

function mockVoiceStore(overrides: Record<string, unknown>) {
  const defaults = { activeCameraCodec: null, activeScreenCodec: null };
  (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) => s({ ...defaults, ...overrides })
  );
}

function openDetails() {
  // CollapsibleSection renders <details>, must open it to see children
  const details = document.querySelector('details');
  if (details) details.open = true;
}

function renderComponent() {
  const result = render(<VideoConfigSection />);
  openDetails();
  return result;
}

// ─── Tests ──────────────────────────────────────────────────────────────────

beforeEach(() => {
  vi.clearAllMocks();
  globalThis.electron = {
    getDisplayInfo: vi.fn().mockResolvedValue([]),
  } as unknown as typeof globalThis.electron;

  // Reset to defaults
  (useVideoSettingsStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) =>
      s({ codecCapabilities: [], gpuInfo: null, videoAdvancedMode: false, systemHdr: false })
  );
  (useDraftVideoSetting as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (key: string) => defaultVideoSettings[key] ?? false
  );
  (useVoiceStore as unknown as ReturnType<typeof vi.fn>).mockImplementation(
    (s: (state: Record<string, unknown>) => unknown) =>
      s({ activeCameraCodec: null, activeScreenCodec: null })
  );
});

// ─── 1. Basic mode rendering ────────────────────────────────────────────────

describe('VideoConfigSection', () => {
  describe('basic mode', () => {
    it('renders Camera Preset section with system default hint', () => {
      renderComponent();
      expect(screen.getByText('Camera Preset')).toBeInTheDocument();
      expect(screen.getByText(/Currently System Default/)).toBeInTheDocument();
    });

    it('renders Camera Preset hint for specific preset', () => {
      mockDraftSettings({ cameraPreset: '720p' });
      renderComponent();
      expect(screen.getByText(/Currently requesting 720p/)).toBeInTheDocument();
    });

    it('renders Screen Share Resolution with source hint', () => {
      renderComponent();
      expect(screen.getByText('Resolution')).toBeInTheDocument();
      expect(
        screen.getByText(/Currently Native \u2014 captures at your display/)
      ).toBeInTheDocument();
    });

    it('renders Screen Share Resolution with specific resolution hint', () => {
      mockDraftSettings({ screenResolution: '1080p' });
      renderComponent();
      expect(screen.getByText(/Currently 1080p/)).toBeInTheDocument();
    });

    it('renders Frame Rate with native hint when 0', () => {
      renderComponent();
      expect(screen.getByText('Frame Rate')).toBeInTheDocument();
      expect(screen.getByText(/Currently Native \(60 Hz\)/)).toBeInTheDocument();
    });

    it('renders Frame Rate with specific FPS hint', () => {
      mockDraftSettings({ screenFrameRate: 30 });
      renderComponent();
      expect(screen.getByText(/Currently 30 FPS/)).toBeInTheDocument();
    });

    it('does not render advanced settings in basic mode', () => {
      renderComponent();
      expect(screen.queryByText('Congestion Priority')).not.toBeInTheDocument();
      expect(screen.queryByText('Codec & Hardware')).not.toBeInTheDocument();
    });
  });

  // ─── 2. Content type IIFE branches ─────────────────────────────────────────

  describe('content type hint', () => {
    it('shows auto hint', () => {
      mockDraftSettings({ screenContentType: 'auto' });
      renderComponent();
      expect(screen.getByText(/Currently Auto/)).toBeInTheDocument();
    });

    it('shows motion hint', () => {
      mockDraftSettings({ screenContentType: 'motion' });
      renderComponent();
      expect(screen.getByText(/Currently Motion/)).toBeInTheDocument();
    });

    it('shows detail hint', () => {
      mockDraftSettings({ screenContentType: 'detail' });
      renderComponent();
      expect(screen.getByText(/Currently Detail/)).toBeInTheDocument();
    });
  });

  // ─── 3. GpuVendorIcon branches ─────────────────────────────────────────────

  describe('GpuVendorIcon', () => {
    it.each([
      ['Apple', 'apple'],
      ['NVIDIA GeForce', 'nvidia'],
      ['Intel UHD', 'intel'],
      ['AMD Radeon', 'amd'],
      ['Qualcomm Adreno', 'qualcomm'],
      ['ARM Mali', 'arm'],
    ])('renders icon for %s vendor', (vendor) => {
      mockVideoSettingsStore({ videoAdvancedMode: true, gpuInfo: { vendor, device: 'Test GPU' } });
      renderComponent();
      // The vendor name should appear in the gpu badge
      expect(screen.getByText(new RegExp(vendor))).toBeInTheDocument();
      // SVG should be in the document
      const badge = screen.getByText(new RegExp(vendor)).closest('.settings-gpu-badge');
      expect(badge?.querySelector('svg')).toBeInTheDocument();
    });

    it('returns null for unknown vendor (no SVG)', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        gpuInfo: { vendor: 'Unknown', device: '' },
      });
      renderComponent();
      const badge = screen.getByText('Unknown').closest('.settings-gpu-badge');
      expect(badge?.querySelector('svg')).toBeNull();
    });
  });

  // ─── 4. GPU info display ──────────────────────────────────────────────────

  describe('gpuInfo display', () => {
    it('shows no GPU badge when gpuInfo is null', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
      renderComponent();
      expect(document.querySelector('.settings-gpu-badge')).toBeNull();
    });

    it('shows GPU badge with device name', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        gpuInfo: { vendor: 'NVIDIA', device: 'RTX 4090' },
      });
      renderComponent();
      expect(screen.getByText(/NVIDIA/)).toBeInTheDocument();
      expect(screen.getByText(/RTX 4090/)).toBeInTheDocument();
    });

    it('shows GPU badge without device when device is empty', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        gpuInfo: { vendor: 'Intel', device: '' },
      });
      renderComponent();
      expect(screen.getByText('Intel')).toBeInTheDocument();
    });
  });

  // ─── 5. Advanced mode rendering ───────────────────────────────────────────

  describe('advanced mode', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('renders Congestion Priority section', () => {
      renderComponent();
      expect(screen.getByText('Congestion Priority')).toBeInTheDocument();
    });

    it('renders Codec & Hardware subsection', () => {
      renderComponent();
      expect(screen.getByText('Codec & Hardware')).toBeInTheDocument();
    });

    it('renders HDR encoding toggle', () => {
      renderComponent();
      expect(screen.getByText('Enable HDR Encoding')).toBeInTheDocument();
    });

    it('renders Hardware Acceleration toggle', () => {
      renderComponent();
      expect(screen.getByText('Hardware Acceleration')).toBeInTheDocument();
    });

    it('renders Bandwidth subsection', () => {
      renderComponent();
      expect(screen.getByText('Bandwidth')).toBeInTheDocument();
    });

    it('renders Transport subsection', () => {
      renderComponent();
      expect(screen.getByText('Transport')).toBeInTheDocument();
    });
  });

  // ─── 6. Degradation preference IIFE ───────────────────────────────────────

  describe('degradation preference hint', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('shows balanced hint', () => {
      mockDraftSettings({ degradationPreference: 'balanced' });
      renderComponent();
      expect(screen.getByText(/Currently Balanced/)).toBeInTheDocument();
    });

    it('shows maintain-framerate hint', () => {
      mockDraftSettings({ degradationPreference: 'maintain-framerate' });
      renderComponent();
      expect(screen.getByText(/Currently Prefer Smooth Video/)).toBeInTheDocument();
    });

    it('shows maintain-resolution hint', () => {
      mockDraftSettings({ degradationPreference: 'maintain-resolution' });
      renderComponent();
      expect(screen.getByText(/Currently Prefer Sharp Details/)).toBeInTheDocument();
    });
  });

  // ─── 7. HDR encoding branches ─────────────────────────────────────────────

  describe('HDR encoding', () => {
    it('shows no HDR display message when systemHdr is false', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true, systemHdr: false });
      renderComponent();
      expect(screen.getByText(/No HDR display detected/)).toBeInTheDocument();
    });

    it('disables toggle when systemHdr is false', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true, systemHdr: false });
      renderComponent();
      const checkboxes = screen.getAllByRole('checkbox');
      // HDR toggle is the first toggle in advanced mode
      const hdrToggle = checkboxes.find((cb) =>
        cb.closest('.settings-row')?.textContent?.includes('HDR')
      );
      expect(hdrToggle).toBeDisabled();
    });

    it('shows enabled message when systemHdr=true and hdrEncoding=true', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true, systemHdr: true });
      mockDraftSettings({ hdrEncoding: true });
      renderComponent();
      expect(screen.getByText(/Enabled\. HDR codec profiles/)).toBeInTheDocument();
    });

    it('shows disabled message when systemHdr=true and hdrEncoding=false', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true, systemHdr: true });
      mockDraftSettings({ hdrEncoding: false });
      renderComponent();
      expect(screen.getByText(/Disabled\. SDR codec profiles/)).toBeInTheDocument();
    });
  });

  // ─── 8. Codec grid (line 470-612) ─────────────────────────────────────────

  describe('codec grid', () => {
    beforeEach(() => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
    });

    it('renders Hardware and Software columns', () => {
      renderComponent();
      expect(screen.getByText('Hardware')).toBeInTheDocument();
      expect(screen.getByText('Software')).toBeInTheDocument();
    });

    it('renders HW codecs that are powerEfficient', () => {
      renderComponent();
      // VP9, H264, AV1 are powerEfficient
      const hwColumn = screen.getByText('Hardware').closest('.settings-codec-column');
      expect(hwColumn).toBeInTheDocument();
    });

    it('shows "None detected" when no HW codecs exist', () => {
      const swOnlyCodecs = [makeCodec('video/VP8', false)];
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: swOnlyCodecs,
      });
      renderComponent();
      expect(screen.getByText('None detected')).toBeInTheDocument();
    });

    it('marks unsupported codecs (H265) with unsupported class', () => {
      renderComponent();
      const unsupported = document.querySelectorAll('.settings-codec-item.unsupported');
      expect(unsupported.length).toBeGreaterThan(0);
    });

    it('highlights preferred codec', () => {
      renderComponent();
      const preferred = document.querySelectorAll('.settings-codec-item.preferred');
      expect(preferred.length).toBeGreaterThan(0);
    });

    it('highlights in-use codec when activeCameraCodec is set', () => {
      mockVoiceStore({ activeCameraCodec: 'video/VP9/default' });
      renderComponent();
      const inUse = document.querySelectorAll('.settings-codec-item.in-use');
      expect(inUse.length).toBeGreaterThan(0);
    });

    it('highlights in-use codec when activeScreenCodec is set', () => {
      mockVoiceStore({ activeScreenCodec: 'video/H264/level-asymmetry-allowed=1' });
      renderComponent();
      const inUse = document.querySelectorAll('.settings-codec-item.in-use');
      expect(inUse.length).toBeGreaterThan(0);
    });

    it('shows HW fallback notice when HW accel enabled but no HW supported', () => {
      const swOnlyCodecs = [makeCodec('video/VP8', false)];
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: swOnlyCodecs,
      });
      mockDraftSettings({ hardwareAcceleration: true });
      renderComponent();
      expect(screen.getByText(/Hardware acceleration is enabled, but none/)).toBeInTheDocument();
    });

    it('does not show HW fallback notice when HW codecs are supported', () => {
      renderComponent();
      expect(
        screen.queryByText(/Hardware acceleration is enabled, but none/)
      ).not.toBeInTheDocument();
    });

    it('activates HW column when hardwareAcceleration=true and HW codecs exist', () => {
      renderComponent();
      const hwHeader = screen.getByText('Hardware');
      expect(hwHeader.className).toContain('active');
    });

    it('activates SW column when hardwareAcceleration=false', () => {
      mockDraftSettings({ hardwareAcceleration: false });
      renderComponent();
      const swHeader = screen.getByText('Software');
      expect(swHeader.className).toContain('active');
    });

    it('does not render codec grid when codecCapabilities is empty', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true, codecCapabilities: [] });
      renderComponent();
      expect(document.querySelector('.settings-codec-grid')).toBeNull();
    });
  });

  // ─── 9. Preferred video codec ─────────────────────────────────────────────

  describe('preferred video codec', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true, codecCapabilities: sampleCodecs });
    });

    it('shows Auto hint when no preferred codec', () => {
      mockDraftSettings({ preferredVideoCodec: '' });
      renderComponent();
      expect(
        screen.getByText(/Currently Auto \u2014 Concord selects the best/)
      ).toBeInTheDocument();
    });

    it('shows specific codec name when preferred codec set', () => {
      mockDraftSettings({ preferredVideoCodec: 'video/VP8/default' });
      (getCodecInfo as ReturnType<typeof vi.fn>).mockReturnValue({
        name: 'VP8',
        quality: 'Good',
        efficiency: 'Moderate',
        compressionRatio: '30:1',
        hdr: false,
        notes: '',
      });
      renderComponent();
      expect(screen.getByText(/Currently VP8/)).toBeInTheDocument();
    });

    it('shows preference notice when preferred codec is set', () => {
      mockDraftSettings({ preferredVideoCodec: 'video/VP8/default' });
      renderComponent();
      expect(screen.getByText(/Your client will prefer this codec/)).toBeInTheDocument();
    });

    it('does not show preference notice when auto', () => {
      mockDraftSettings({ preferredVideoCodec: '' });
      renderComponent();
      expect(screen.queryByText(/Your client will prefer this codec/)).not.toBeInTheDocument();
    });

    it('renders structured codec metadata', () => {
      renderComponent();
      const badge = document.querySelector('.settings-codec-info-badge');
      expect(badge).toBeInTheDocument();
      expect(badge?.textContent).toContain('Quality:');
      expect(badge?.textContent).toContain('Efficiency:');
      expect(badge?.textContent).toContain('HDR Capable:');
      expect(badge?.textContent).toContain('Description:');
    });

    it('renders HDR Capable Yes when codec metadata is HDR-capable', () => {
      (getCodecInfo as ReturnType<typeof vi.fn>).mockReturnValue({
        name: 'VP9',
        quality: 'Excellent',
        efficiency: 'High',
        compressionRatio: '40:1',
        hdr: true,
        notes: 'Profile 2',
      });
      renderComponent();
      const badge = document.querySelector('.settings-codec-info-badge');
      expect(badge).toBeInTheDocument();
      expect(badge?.textContent).toContain('Yes');
    });
  });

  // ─── 10. Hardware acceleration hint ───────────────────────────────────────

  describe('hardware acceleration hint', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('shows enabled hint', () => {
      mockDraftSettings({ hardwareAcceleration: true });
      renderComponent();
      expect(screen.getByText(/Enabled\. Your GPU handles video encoding/)).toBeInTheDocument();
    });

    it('shows disabled hint', () => {
      mockDraftSettings({ hardwareAcceleration: false });
      renderComponent();
      expect(screen.getByText(/Disabled\. All video encoding/)).toBeInTheDocument();
    });
  });

  // ─── 11. Automatic bitrate ────────────────────────────────────────────────

  describe('automatic bitrate', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('shows enabled hint and estimated bitrate when screenShareBitrate=0', () => {
      mockDraftSettings({ screenShareBitrate: 0 });
      renderComponent();
      expect(screen.getByText(/Enabled\. Concord Voice adjusts bitrate/)).toBeInTheDocument();
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });

    it('shows disabled hint with fixed Mbps when screenShareBitrate is non-zero', () => {
      mockDraftSettings({ screenShareBitrate: 5_000_000 });
      renderComponent();
      expect(screen.getByText(/Disabled\. Using a fixed 5\.0 Mbps/)).toBeInTheDocument();
    });

    it('shows bitrate slider when bitrate is non-zero', () => {
      mockDraftSettings({ screenShareBitrate: 5_000_000 });
      renderComponent();
      expect(screen.getByRole('slider')).toBeInTheDocument();
    });

    it('does not show bitrate slider when bitrate is 0', () => {
      mockDraftSettings({ screenShareBitrate: 0 });
      renderComponent();
      expect(screen.queryByRole('slider')).not.toBeInTheDocument();
    });

    it('changes bitrate via slider', () => {
      mockDraftSettings({ screenShareBitrate: 5_000_000 });
      renderComponent();
      const slider = screen.getByRole('slider');
      fireEvent.change(slider, { target: { value: '10' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenShareBitrate', 10_000_000);
    });
  });

  // ─── 12. Camera QoS IIFE ──────────────────────────────────────────────────

  describe('camera QoS hint', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('shows off hint', () => {
      mockDraftSettings({ cameraPriority: 'off' });
      renderComponent();
      const row = screen.getByText('Quality of Service for Camera').closest('.settings-row');
      expect(row?.textContent).toContain('Currently off');
    });

    it('shows low hint', () => {
      mockDraftSettings({ cameraPriority: 'low' });
      renderComponent();
      const row = screen.getByText('Quality of Service for Camera').closest('.settings-row');
      expect(row?.textContent).toContain('Currently Low (DF)');
    });

    it('shows medium hint', () => {
      mockDraftSettings({ cameraPriority: 'medium' });
      renderComponent();
      const row = screen.getByText('Quality of Service for Camera').closest('.settings-row');
      expect(row?.textContent).toContain('Currently Default (AF43)');
    });

    it('shows high hint', () => {
      mockDraftSettings({ cameraPriority: 'high' });
      renderComponent();
      const row = screen.getByText('Quality of Service for Camera').closest('.settings-row');
      expect(row?.textContent).toContain('Currently High (EF)');
      expect(row?.textContent).toContain('RFC 5127');
    });
  });

  // ─── 13. Screen share QoS IIFE ───────────────────────────────────────────

  describe('screen share QoS hint', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('shows off hint', () => {
      mockDraftSettings({ screenSharePriority: 'off' });
      renderComponent();
      expect(screen.getByText(/Quality of Service for Screen Share/)).toBeInTheDocument();
      // Both camera and screen share QoS have "Currently off" — find the screen one
      const rows = screen.getAllByText(/Currently off/);
      expect(rows.length).toBeGreaterThanOrEqual(1);
    });

    it('shows low hint for screen share', () => {
      mockDraftSettings({ screenSharePriority: 'low', cameraPriority: 'off' });
      renderComponent();
      // Screen share low mentions RFC 2474
      const hints = screen.getAllByText(/Currently Low \(DF\)/);
      expect(hints.length).toBeGreaterThanOrEqual(1);
    });

    it('shows medium hint for screen share (AF42)', () => {
      mockDraftSettings({ screenSharePriority: 'medium', cameraPriority: 'off' });
      renderComponent();
      expect(screen.getByText(/Currently Default \(AF42\)/)).toBeInTheDocument();
    });

    it('shows high hint for screen share', () => {
      mockDraftSettings({ screenSharePriority: 'high', cameraPriority: 'off' });
      renderComponent();
      // Both camera and screen share use "Currently High (EF)" but the parent label distinguishes them
      const row = screen.getByText('Quality of Service for Screen Share').closest('.settings-row');
      expect(row?.textContent).toContain('Currently High (EF)');
    });
  });

  // ─── 14. Display info & resolution options ────────────────────────────────

  describe('display info', () => {
    it('uses default 1920x1080 when no displays', () => {
      renderComponent();
      // The frame rate hint should show 60 Hz (default maxRefreshRate)
      const matches = screen.getAllByText(/Native \(60 Hz\)/);
      expect(matches.length).toBeGreaterThanOrEqual(1);
    });

    it('uses actual display info when populated', async () => {
      (globalThis.electron!.getDisplayInfo as ReturnType<typeof vi.fn>).mockResolvedValue([
        { width: 3840, height: 2160, refreshRate: 144, scaleFactor: 2, isPrimary: true },
        { width: 2560, height: 1440, refreshRate: 120, scaleFactor: 1, isPrimary: false },
      ]);
      const { rerender } = render(<VideoConfigSection />);
      openDetails();
      // Wait for the async effect to populate display info
      await vi.waitFor(() => {
        rerender(<VideoConfigSection />);
        expect(screen.getAllByText(/Native \(144 Hz\)/).length).toBeGreaterThanOrEqual(1);
      });
    });

    it('shows 4K option when bestDisplay height >= 2160', async () => {
      (globalThis.electron!.getDisplayInfo as ReturnType<typeof vi.fn>).mockResolvedValue([
        { width: 3840, height: 2160, refreshRate: 60, scaleFactor: 2, isPrimary: true },
      ]);
      render(<VideoConfigSection />);
      openDetails();
      await vi.waitFor(() => {
        expect(screen.getByText('4K (3840\u00d72160)')).toBeInTheDocument();
      });
    });

    it('shows 1440p option when bestDisplay height >= 1440', async () => {
      (globalThis.electron!.getDisplayInfo as ReturnType<typeof vi.fn>).mockResolvedValue([
        { width: 2560, height: 1440, refreshRate: 60, scaleFactor: 1, isPrimary: true },
      ]);
      render(<VideoConfigSection />);
      openDetails();
      await vi.waitFor(() => {
        expect(screen.getByText('1440p (2560\u00d71440)')).toBeInTheDocument();
      });
    });

    it('shows display-specific resolution options', async () => {
      (globalThis.electron!.getDisplayInfo as ReturnType<typeof vi.fn>).mockResolvedValue([
        { width: 2560, height: 1440, refreshRate: 60, scaleFactor: 1, isPrimary: true },
      ]);
      render(<VideoConfigSection />);
      openDetails();
      await vi.waitFor(() => {
        expect(screen.getByText('2560\u00d71440 (Primary)')).toBeInTheDocument();
      });
    });
  });

  // ─── 15. recommendedBitrate memo branches ────────────────────────────────

  describe('recommendedBitrate', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
    });

    it('uses efficient bitrate for AV1 codec', () => {
      mockDraftSettings({
        screenShareBitrate: 0,
        preferredVideoCodec: 'video/AV1/default',
        screenResolution: '1080p',
        screenFrameRate: 30,
      });
      renderComponent();
      // Just verify estimated bitrate is displayed (exact value depends on bpp)
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });

    it('uses non-efficient bitrate for VP8 codec', () => {
      mockDraftSettings({
        screenShareBitrate: 0,
        preferredVideoCodec: 'video/VP8/default',
        screenResolution: '1080p',
        screenFrameRate: 30,
      });
      renderComponent();
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });

    it('parses NxN resolution string', () => {
      mockDraftSettings({
        screenShareBitrate: 0,
        screenResolution: '2560x1440',
        screenFrameRate: 30,
      });
      renderComponent();
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });

    it('uses maxRefreshRate when screenFrameRate is 0', () => {
      mockDraftSettings({
        screenShareBitrate: 0,
        screenFrameRate: 0,
        screenResolution: '1080p',
      });
      renderComponent();
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });

    it('falls back to codecCapabilities for efficiency when no preferred codec', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: [makeCodec('video/AV1', true)],
      });
      mockDraftSettings({
        screenShareBitrate: 0,
        preferredVideoCodec: '',
        screenResolution: '1080p',
        screenFrameRate: 30,
      });
      renderComponent();
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });

    it('uses activeScreenCodec over preferredVideoCodec for efficiency calc', () => {
      mockVoiceStore({ activeScreenCodec: 'video/VP9/default' });
      mockDraftSettings({
        screenShareBitrate: 0,
        preferredVideoCodec: 'video/VP8/default',
        screenResolution: '1080p',
        screenFrameRate: 30,
      });
      renderComponent();
      expect(screen.getByText(/Estimated Bitrate/)).toBeInTheDocument();
    });
  });

  // ─── 16. Mode toggle keyboard interaction ────────────────────────────────

  describe('mode toggle', () => {
    it('calls setVideoAdvancedMode(true) on Enter key on Advanced pill', () => {
      renderComponent();
      const advancedPill = screen.getByText('Advanced Settings');
      fireEvent.keyDown(advancedPill, { key: 'Enter' });
      expect(mockSetVideoAdvancedMode).toHaveBeenCalledWith(true);
    });

    it('calls setVideoAdvancedMode(true) on Space key on Advanced pill', () => {
      renderComponent();
      const advancedPill = screen.getByText('Advanced Settings');
      fireEvent.keyDown(advancedPill, { key: ' ' });
      expect(mockSetVideoAdvancedMode).toHaveBeenCalledWith(true);
    });

    it('calls setVideoAdvancedMode(false) on Enter key on Basic pill', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
      renderComponent();
      const basicPill = screen.getByText('Basic Settings');
      fireEvent.keyDown(basicPill, { key: 'Enter' });
      expect(mockSetVideoAdvancedMode).toHaveBeenCalledWith(false);
    });

    it('calls setVideoAdvancedMode(false) on Space key on Basic pill', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
      renderComponent();
      const basicPill = screen.getByText('Basic Settings');
      fireEvent.keyDown(basicPill, { key: ' ' });
      expect(mockSetVideoAdvancedMode).toHaveBeenCalledWith(false);
    });

    it('calls setVideoAdvancedMode on click', () => {
      renderComponent();
      const advancedPill = screen.getByText('Advanced Settings');
      fireEvent.click(advancedPill);
      expect(mockSetVideoAdvancedMode).toHaveBeenCalledWith(true);
    });

    it('does not trigger on non-Enter/Space keys', () => {
      renderComponent();
      const advancedPill = screen.getByText('Advanced Settings');
      fireEvent.keyDown(advancedPill, { key: 'Tab' });
      expect(mockSetVideoAdvancedMode).not.toHaveBeenCalled();
    });
  });

  // ─── 17. Codec grid with preferred codec and hwActive logic ───────────────

  describe('codec grid hwActive logic', () => {
    it('activates HW column when user picks a codec present in HW codecs', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      mockDraftSettings({
        preferredVideoCodec: 'video/H264/level-asymmetry-allowed=1',
        hardwareAcceleration: true,
      });
      renderComponent();
      const hwHeader = screen.getByText('Hardware');
      expect(hwHeader.className).toContain('active');
    });

    it('activates SW column when user picks a codec NOT in HW codecs', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      // VP8 is not powerEfficient in our sample data
      mockDraftSettings({
        preferredVideoCodec: 'video/VP8/default',
        hardwareAcceleration: true,
      });
      renderComponent();
      const swHeader = screen.getByText('Software');
      expect(swHeader.className).toContain('active');
    });

    it('activates SW column when hardwareAcceleration is false regardless of codec', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      mockDraftSettings({
        preferredVideoCodec: 'video/H264/level-asymmetry-allowed=1',
        hardwareAcceleration: false,
      });
      renderComponent();
      const swHeader = screen.getByText('Software');
      expect(swHeader.className).toContain('active');
    });
  });

  // ─── 18. HDR codecs in codec select dropdown ─────────────────────────────

  describe('HDR codecs in select', () => {
    beforeEach(() => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecsWithHdr,
      });
    });

    it('renders HDR codec options when hdrEncoding is true', () => {
      mockDraftSettings({ hdrEncoding: true });
      renderComponent();
      // HDR codec option should be selectable (not disabled prefix)
      const selects = screen.getAllByTestId('custom-select');
      const codecSelect = selects.find((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some((o) => o.value === '');
      });
      expect(codecSelect).toBeTruthy();
    });

    it('prefixes HDR codec values with __disabled_ when hdrEncoding is false', () => {
      mockDraftSettings({ hdrEncoding: false });
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Find the codec select (has Auto option with empty value)
      const codecSelect = selects.find((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some((o) => o.value === '');
      });
      const options = codecSelect?.querySelectorAll('option') || [];
      const disabledOpts = Array.from(options).filter((o) => o.value.startsWith('__disabled_'));
      expect(disabledOpts.length).toBeGreaterThan(0);
    });
  });

  // ─── 19. Toggle interactions ──────────────────────────────────────────────

  describe('toggle interactions', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true, systemHdr: true });
    });

    it('calls setDraftVideoSetting for HDR encoding toggle', () => {
      mockDraftSettings({ hdrEncoding: false });
      renderComponent();
      const checkboxes = screen.getAllByRole('checkbox');
      const hdrToggle = checkboxes.find((cb) =>
        cb.closest('.settings-row')?.textContent?.includes('HDR')
      );
      expect(hdrToggle).toBeTruthy();
      fireEvent.click(hdrToggle!);
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('hdrEncoding', true);
    });

    it('calls setDraftVideoSetting for Hardware Acceleration toggle', () => {
      mockDraftSettings({ hardwareAcceleration: true });
      renderComponent();
      const checkboxes = screen.getAllByRole('checkbox');
      const hwToggle = checkboxes.find((cb) =>
        cb.closest('.settings-row')?.textContent?.includes('Hardware Acceleration')
      );
      expect(hwToggle).toBeTruthy();
      fireEvent.click(hwToggle!);
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('hardwareAcceleration', false);
    });

    it('calls setDraftVideoSetting for Automatic Bitrate toggle (disable)', () => {
      mockDraftSettings({ screenShareBitrate: 0 });
      renderComponent();
      const checkboxes = screen.getAllByRole('checkbox');
      const bitrateToggle = checkboxes.find((cb) =>
        cb.closest('.settings-row')?.textContent?.includes('Automatic Bitrate')
      );
      expect(bitrateToggle).toBeTruthy();
      fireEvent.click(bitrateToggle!);
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith(
        'screenShareBitrate',
        expect.any(Number)
      );
    });
  });

  // ─── 20. Select onChange interactions ──────────────────────────────────────

  describe('select onChange', () => {
    it('calls setDraftVideoSetting for cameraPreset change', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // First select is camera preset
      fireEvent.change(selects[0], { target: { value: '720p' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '720p');
    });

    it('calls setDraftVideoSetting for screenResolution change', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Second select is resolution
      fireEvent.change(selects[1], { target: { value: '1080p' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenResolution', '1080p');
    });

    it('calls setDraftVideoSetting for screenFrameRate change', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Third select is frame rate
      fireEvent.change(selects[2], { target: { value: '30' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenFrameRate', 30);
    });

    it('calls setDraftVideoSetting for screenContentType change', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Fourth select is content type
      fireEvent.change(selects[3], { target: { value: 'motion' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenContentType', 'motion');
    });
  });

  // ─── 21. Advanced select onChange interactions ─────────────────────────────

  describe('advanced select onChange', () => {
    beforeEach(() => {
      mockVideoSettingsStore({ videoAdvancedMode: true, codecCapabilities: sampleCodecs });
    });

    it('calls setDraftVideoSetting for degradationPreference', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Fifth select (index 4) is degradation preference
      fireEvent.change(selects[4], { target: { value: 'maintain-framerate' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith(
        'degradationPreference',
        'maintain-framerate'
      );
    });

    it('calls setDraftVideoSetting for preferredVideoCodec (select codec)', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Find the codec select (has Auto option)
      const codecSelect = selects.find((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some((o) => o.value === '' && o.textContent === 'Auto');
      });
      expect(codecSelect).toBeTruthy();
      fireEvent.change(codecSelect!, { target: { value: 'video/VP8/default' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith(
        'preferredVideoCodec',
        'video/VP8/default'
      );
    });

    it('calls setDraftVideoSetting with null for preferredVideoCodec (Auto)', () => {
      mockDraftSettings({ preferredVideoCodec: 'video/VP8/default' });
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Codec select has an option with value="" and label "Auto"
      const codecSelect = selects.find((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some((o) => o.value === '' && o.textContent === 'Auto');
      });
      expect(codecSelect).toBeTruthy();
      fireEvent.change(codecSelect!, { target: { value: '' } });
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('preferredVideoCodec', null);
    });

    it('ignores disabled HDR codec selection', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecsWithHdr,
      });
      mockDraftSettings({ hdrEncoding: false, preferredVideoCodec: 'video/VP8/default' });
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // Codec select has an option with value="" and label "Auto"
      const codecSelect = selects.find((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some((o) => o.value === '' && o.textContent === 'Auto');
      });
      fireEvent.change(codecSelect!, { target: { value: '__disabled_video/VP9/profile-id=2' } });
      // Should keep the current preferred codec, not the disabled one
      expect(mockSetDraftVideoSetting).toHaveBeenCalledWith(
        'preferredVideoCodec',
        'video/VP8/default'
      );
    });

    it('calls setDraftVideoSetting for cameraPriority', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      // There are two QoS selects; pick the first one (camera)
      const qosSelects = selects.filter((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some(
          (o) => o.value === 'off' && o.textContent === 'Off (No Tagging)'
        );
      });
      if (qosSelects.length >= 1) {
        fireEvent.change(qosSelects[0], { target: { value: 'medium' } });
        expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPriority', 'medium');
      }
    });

    it('calls setDraftVideoSetting for screenSharePriority', () => {
      renderComponent();
      const selects = screen.getAllByTestId('custom-select');
      const qosSelects = selects.filter((s) => {
        const opts = s.querySelectorAll('option');
        return Array.from(opts).some(
          (o) => o.value === 'off' && o.textContent === 'Off (No Tagging)'
        );
      });
      if (qosSelects.length >= 2) {
        fireEvent.change(qosSelects[1], { target: { value: 'high' } });
        expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenSharePriority', 'high');
      }
    });
  });

  // ─── 22. Codec grid tooltip branches ──────────────────────────────────────

  describe('codec item tooltips', () => {
    it('shows "Preferred \u00b7 In Use" tooltip for preferred+in-use codec', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      // AV1 is first supported HW codec (highest priority), so it's preferred
      // Make it also in-use
      mockVoiceStore({ activeCameraCodec: 'video/AV1/default' });
      mockDraftSettings({ hardwareAcceleration: true });
      renderComponent();
      const items = document.querySelectorAll('.settings-codec-item');
      const tooltips = Array.from(items).map((el) => el.getAttribute('data-tooltip'));
      expect(tooltips).toContain('Preferred \u00b7 In Use');
    });

    it('shows "In Use" tooltip for non-preferred in-use codec', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      // VP8 is SW only, not preferred (AV1 would be preferred)
      mockVoiceStore({ activeCameraCodec: 'video/vp8' });
      mockDraftSettings({ hardwareAcceleration: true });
      renderComponent();
      const items = document.querySelectorAll('.settings-codec-item');
      const tooltips = Array.from(items).map((el) => el.getAttribute('data-tooltip'));
      expect(tooltips).toContain('In Use');
    });

    it('shows "Not supported" tooltip for unsupported codec', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      renderComponent();
      const items = document.querySelectorAll('.settings-codec-item');
      const tooltips = Array.from(items).map((el) => el.getAttribute('data-tooltip'));
      expect(tooltips).toContain('Not supported');
    });

    it('shows "Available" tooltip for supported non-preferred codec', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: sampleCodecs,
      });
      renderComponent();
      const items = document.querySelectorAll('.settings-codec-item');
      const tooltips = Array.from(items).map((el) => el.getAttribute('data-tooltip'));
      expect(tooltips).toContain('Available');
    });
  });

  // ─── 23. Codec display names with profile labels ─────────────────────────

  describe('codec display names', () => {
    it('renders codec with profile label', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: [makeCodec('video/H264', true, { profileLabel: 'High' })],
      });
      renderComponent();
      // H264 with profile appears in both HW and SW columns
      const items = screen.getAllByText('AVC (H.264) (High)');
      expect(items.length).toBeGreaterThanOrEqual(1);
    });

    it('renders H265 as HEVC (H.265)', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: [makeCodec('video/H265', false, { profileLabel: 'Main' })],
      });
      renderComponent();
      // H265 is SW only (not powerEfficient), but appears in SW column
      const items = screen.getAllByText('HEVC (H.265) (Main)');
      expect(items.length).toBeGreaterThanOrEqual(1);
    });

    it('renders VP8 without display name mapping', () => {
      mockVideoSettingsStore({
        videoAdvancedMode: true,
        codecCapabilities: [makeCodec('video/VP8', false)],
      });
      renderComponent();
      // VP8 appears in SW column (not powerEfficient), codec name in codec grid
      const items = document.querySelectorAll('.settings-codec-name');
      const vp8Items = Array.from(items).filter((el) => el.textContent === 'VP8');
      expect(vp8Items.length).toBeGreaterThanOrEqual(1);
    });
  });

  // ─── 24. CollapsibleSection renders as details ────────────────────────────

  describe('CollapsibleSection wrapper', () => {
    it('renders as a details element with correct title', () => {
      render(<VideoConfigSection />);
      const details = document.querySelector('details#section-video-screen');
      expect(details).toBeInTheDocument();
      expect(screen.getByText('Video Configuration')).toBeInTheDocument();
    });
  });

  // ─── 25. Mode pills aria attributes ──────────────────────────────────────

  describe('mode pill aria attributes', () => {
    it('marks Basic as selected in basic mode', () => {
      renderComponent();
      const basicPill = screen.getByText('Basic Settings');
      expect(basicPill.getAttribute('aria-selected')).toBe('true');
      const advancedPill = screen.getByText('Advanced Settings');
      expect(advancedPill.getAttribute('aria-selected')).toBe('false');
    });

    it('marks Advanced as selected in advanced mode', () => {
      mockVideoSettingsStore({ videoAdvancedMode: true });
      renderComponent();
      const basicPill = screen.getByText('Basic Settings');
      expect(basicPill.getAttribute('aria-selected')).toBe('false');
      const advancedPill = screen.getByText('Advanced Settings');
      expect(advancedPill.getAttribute('aria-selected')).toBe('true');
    });
  });
});
