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

const { mockSetDraftVideoSetting } = vi.hoisted(() => ({
  mockSetDraftVideoSetting: vi.fn(),
}));

// ─── Mocks (before component imports) ───────────────────────────────────────

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: vi.fn((s: (state: Record<string, unknown>) => unknown) =>
    s({ activeCameraCodec: null, activeScreenCodec: null })
  ),
}));

let videoAdvancedMode = false;
vi.mock('@/renderer/stores/videoSettingsStore', () => ({
  useVideoSettingsStore: Object.assign(
    vi.fn((s: (state: Record<string, unknown>) => unknown) =>
      s({
        codecCapabilities: [],
        gpuInfo: null,
        videoAdvancedMode,
        systemHdr: false,
      })
    ),
    { getState: vi.fn(() => ({ setVideoAdvancedMode: vi.fn() })) }
  ),
  // Real-shaped presets with height/width/frameRate so the L2 cap predicate
  // works. Free floor: maxVideoPixelRate 62208000 (= 1920×1080×30), so 1080p30
  // is the highest free preset; 1080p60 exceeds the pixel-rate cap and is paid.
  VIDEO_QUALITY_PRESETS: {
    system: { label: 'System Default', width: 0, height: 0, frameRate: 0 },
    '720p30': { label: '720p 30fps', width: 1280, height: 720, frameRate: 30 },
    '1080p30': { label: '1080p 30fps', width: 1920, height: 1080, frameRate: 30 },
    '1080p60': { label: '1080p 60fps', width: 1920, height: 1080, frameRate: 60 },
    '1440p60': { label: '1440p 60fps', width: 2560, height: 1440, frameRate: 60 },
    '4K60': { label: '4K 60fps', width: 3840, height: 2160, frameRate: 60 },
  },
}));

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftVideoSetting: vi.fn((key: string) => defaultVideoSettings[key] ?? false),
  setDraftVideoSetting: mockSetDraftVideoSetting,
}));

vi.mock('@/renderer/services/mediaCapabilities', () => ({
  codecKey: vi.fn(() => 'video/vp8/default'),
  codecKeyMime: vi.fn((key: string) => key),
  getCodecInfo: vi.fn(() => ({
    name: 'VP8',
    quality: 'Good',
    efficiency: 'Moderate',
    compressionRatio: '30:1',
    hdr: false,
    notes: '',
  })),
}));

// Real CustomSelect (native <select>) so option labels + onChange are testable.

// FREE entitlement floor: 1080p / 60fps / 5 Mbps.
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return {
    tier: 'free',
    maxVideoHeight: 1080,
    maxVideoFps: 60,
    maxVideoPixelRate: 62208000,
    maxManualBitrateBps: 5000000,
    ...entitlementOverrides,
  };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import VideoConfigSection from '@/renderer/components/Settings/VideoConfigSection';

function setEntitlement(overrides: Record<string, unknown>) {
  for (const k of Object.keys(entitlementOverrides)) delete entitlementOverrides[k];
  Object.assign(entitlementOverrides, overrides);
}

function mockDraft(overrides: Record<string, unknown>) {
  Object.assign(defaultVideoSettings, overrides);
}

function openDetails() {
  const details = document.querySelector('details');
  if (details) details.open = true;
}

beforeEach(() => {
  vi.clearAllMocks();
  videoAdvancedMode = false;
  setEntitlement({});
  // Reset draft snapshot to baseline.
  Object.assign(defaultVideoSettings, {
    cameraPreset: 'system',
    screenResolution: 'source',
    screenFrameRate: 0,
    screenShareBitrate: 0,
  });
  // Default display: high-res + high-refresh (so L6 native-exceeds fires under free).
  globalThis.electron = {
    getDisplayInfo: vi
      .fn()
      .mockResolvedValue([
        { width: 3840, height: 2160, refreshRate: 144, scaleFactor: 1, isPrimary: true },
      ]),
  } as unknown as typeof globalThis.electron;
  useSettingsNavStore.getState().clearFocusRequest();
});

function cameraPresetSelect(): HTMLSelectElement {
  // Camera Preset is the first select in the (always-visible) Camera row.
  return document.querySelectorAll('select.settings-select')[0] as HTMLSelectElement;
}

// ─── L2: camera-preset resolution/fps option lock ───────────────────────────

describe('VideoConfigSection — L2 camera-preset lock', () => {
  it('locked (free): presets above 1080p (1440p/4K) carry the lock marker', () => {
    render(<VideoConfigSection />);
    openDetails();
    expect(
      (screen.getByRole('option', { name: /1440p/ }) as HTMLOptionElement).textContent
    ).toContain('Premium');
    expect((screen.getByRole('option', { name: /4K/ }) as HTMLOptionElement).textContent).toContain(
      'Premium'
    );
  });

  it('locked (free): 1080p30 / 720p / System Default presets are NOT marked premium', () => {
    render(<VideoConfigSection />);
    openDetails();
    // Scope to the Camera Preset select to avoid matching the screen-resolution
    // "1080p (1920×1080)" option which also contains "1080p".
    const presetOptions = Array.from(cameraPresetSelect().options);
    const findOpt = (re: RegExp) => presetOptions.find((o) => re.test(o.textContent ?? ''))!;
    expect(findOpt(/1080p 30fps/).textContent).not.toContain('Premium');
    expect(findOpt(/720p/).textContent).not.toContain('Premium');
    expect(findOpt(/System Default/).textContent).not.toContain('Premium');
  });

  it('locked (free): 1080p60 exceeds the free pixel-rate cap → marked premium', () => {
    render(<VideoConfigSection />);
    openDetails();
    const opt = Array.from(cameraPresetSelect().options).find((o) =>
      /1080p 60fps/.test(o.textContent ?? '')
    )!;
    expect(opt.textContent).toContain('Premium');
  });

  it('locked (free): selecting a premium preset snaps back to the highest free preset + chip', () => {
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '4K60' } });
    // Highest free preset is 1080p30 (1080p60 exceeds the pixel-rate cap).
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '1080p30');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('cameraPreset', '4K60');
    expect(screen.getByRole('button', { name: /Premium/ })).toBeInTheDocument();
  });

  it('locked (free): selecting a FREE preset passes through unchanged', () => {
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '720p30' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '720p30');
  });

  it('entitled (premium caps): no preset is locked; 4K passes through', () => {
    setEntitlement({
      maxVideoHeight: 4320,
      maxVideoFps: 240,
      maxVideoPixelRate: Number.MAX_SAFE_INTEGER,
    });
    render(<VideoConfigSection />);
    openDetails();
    expect(
      (screen.getByRole('option', { name: /4K/ }) as HTMLOptionElement).textContent
    ).not.toContain('Premium');
    fireEvent.change(cameraPresetSelect(), { target: { value: '4K60' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '4K60');
  });
});

// ─── L6: native-exceeds guard for resolution / frame rate ───────────────────

describe('VideoConfigSection — L6 native-exceeds guard', () => {
  it('locked (free): shows the "your device supports more" note when native > free caps', async () => {
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() =>
      expect(document.querySelector('.settings-native-exceeds-note')).toBeInTheDocument()
    );
    expect(document.querySelector('.settings-native-exceeds-note')?.textContent).toContain(
      'Your device supports more'
    );
  });

  it('locked (free): the resolution list is clamped to the free ceiling (no 4K / 1440p)', async () => {
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      // 4K / 1440p screen-resolution options are NOT offered (clamped to 1080p).
      expect(screen.queryByRole('option', { name: '4K (3840×2160)' })).not.toBeInTheDocument();
      expect(screen.queryByRole('option', { name: '1440p (2560×1440)' })).not.toBeInTheDocument();
    });
  });

  it('locked (free): the frame-rate list is clamped to the free fps ceiling (no 120/90 FPS)', async () => {
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      expect(screen.queryByRole('option', { name: '120 FPS' })).not.toBeInTheDocument();
      expect(screen.queryByRole('option', { name: '90 FPS' })).not.toBeInTheDocument();
    });
    // 60 / 30 always remain.
    expect(screen.getByRole('option', { name: '60 FPS' })).toBeInTheDocument();
  });

  it('entitled (premium caps): native-exceeds note hidden; 4K / 120 FPS offered', async () => {
    setEntitlement({ maxVideoHeight: 4320, maxVideoFps: 240 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() =>
      expect(screen.getByRole('option', { name: '4K (3840×2160)' })).toBeInTheDocument()
    );
    expect(screen.queryByText(/Your device supports more/)).not.toBeInTheDocument();
    expect(screen.getByRole('option', { name: '120 FPS' })).toBeInTheDocument();
  });
});

// ─── L5: manual screen-share bitrate clamp ──────────────────────────────────

describe('VideoConfigSection — L5 manual bitrate clamp', () => {
  beforeEach(() => {
    videoAdvancedMode = true; // the bitrate slider lives in the advanced section
    mockDraft({ screenShareBitrate: 4_000_000 }); // manual cap, within free range
  });

  it('locked (free): the bitrate slider max is fenced at the free cap (5 Mbps)', () => {
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    expect(slider).toBeInTheDocument();
    expect(slider.max).toBe('5');
  });

  it('locked (free): renders the "beyond 5 Mbps" ghost-zone with the lock chip', () => {
    render(<VideoConfigSection />);
    openDetails();
    expect(document.querySelector('.settings-bitrate-ghost-zone')).toBeInTheDocument();
    expect(screen.getByText(/beyond 5 Mbps/)).toBeInTheDocument();
    expect(screen.getByLabelText('Premium feature')).toBeInTheDocument();
  });

  it('locked (free): the slider stays LIVE within range — a free value passes through', () => {
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '3.5' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenShareBitrate', 3_500_000);
  });

  it('locked (free): a value at/above the cap clamps to the free cap', () => {
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '5' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenShareBitrate', 5_000_000);
  });

  it('locked (free): clicking the ghost-zone routes to the Subscription page', () => {
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.click(document.querySelector('.settings-bitrate-ghost-zone') as HTMLElement);
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('entitled (premium bitrate): the slider max is the absolute 30 Mbps, no ghost-zone', () => {
    setEntitlement({ maxManualBitrateBps: 30_000_000 });
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    expect(slider.max).toBe('30');
    expect(document.querySelector('.settings-bitrate-ghost-zone')).not.toBeInTheDocument();
  });
});
