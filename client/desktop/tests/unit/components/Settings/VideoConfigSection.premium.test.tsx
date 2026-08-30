import { vi } from 'vitest';

// ─── Default draft video settings ───────────────────────────────────────────
const defaultVideoSettings: Record<string, unknown> = {
  cameraPreset: 'system',
  cameraBitrate: 0,
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

vi.mock('@/renderer/stores/voice/voiceStore', () => ({
  useVoiceStore: Object.assign(
    vi.fn((s: (state: Record<string, unknown>) => unknown) =>
      s({ activeCameraCodec: null, activeScreenCodec: null })
    ),
    { getState: () => ({ reset: vi.fn() }), setState: vi.fn() }
  ),
}));

let videoAdvancedMode = false;
vi.mock('@/renderer/stores/voice/videoSettingsStore', () => ({
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
  // works. Free CAMERA axis (#1602): 720p60, so 720p60 is the highest free camera
  // preset; 720p30 is free; 1080p30/1080p60/1440p/4K all exceed the camera height
  // (720) and are paid.
  VIDEO_QUALITY_PRESETS: {
    system: { label: 'System Default', width: 0, height: 0, frameRate: 0 },
    '720p30': { label: '720p 30fps', width: 1280, height: 720, frameRate: 30 },
    '720p60': { label: '720p 60fps', width: 1280, height: 720, frameRate: 60 },
    '1080p30': { label: '1080p 30fps', width: 1920, height: 1080, frameRate: 30 },
    '1080p60': { label: '1080p 60fps', width: 1920, height: 1080, frameRate: 60 },
    '1440p60': { label: '1440p 60fps', width: 2560, height: 1440, frameRate: 60 },
    '4K60': { label: '4K 60fps', width: 3840, height: 2160, frameRate: 60 },
  },
}));

vi.mock('@/renderer/hooks/ui/useDraftSettings', () => ({
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

// FREE entitlement floor (split axes, #1602): stream 1080p30/≤5M, camera 720p60/≤2.5M.
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return {
    tier: 'free',
    streamMaxHeight: 1080,
    streamMaxFps: 60, // #2163: absolute ceiling; streamMaxPixelRate tiers it
    streamMaxPixelRate: 62208000, // = 1080p30; admits 720p60, rejects 1080p60
    streamMaxBitrate: 5000000,
    cameraMaxHeight: 720,
    cameraMaxFps: 60,
    cameraMaxBitrate: 2500000,
    maxManualBitrateBps: 5000000,
    ...entitlementOverrides,
  };
}
vi.mock('@/renderer/hooks/ui/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
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
  resetAllStores();
  vi.clearAllMocks();
  videoAdvancedMode = false;
  setEntitlement({});
  // Reset draft snapshot to baseline.
  Object.assign(defaultVideoSettings, {
    cameraPreset: 'system',
    cameraBitrate: 0,
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
  // The destructive fps/preset snap-backs gate on an AUTHORITATIVE entitlement (#2172);
  // mark the store hydrated so the existing snap tests exercise the snap. Pre-hydrate
  // no-snap behaviour has its own test.
  useSubscriptionStore.setState({ hydrated: true, degraded: false });
});

function cameraPresetSelect(): HTMLSelectElement {
  // Camera Preset is the first select in the (always-visible) Camera row.
  return document.querySelectorAll('select.settings-select')[0] as HTMLSelectElement;
}

// ─── L2: camera-preset resolution/fps option lock ───────────────────────────

describe('VideoConfigSection — L2 camera-preset lock (camera axis 720p60, #1602)', () => {
  it('locked (free): presets above the camera axis (1080p/1440p/4K) carry the lock marker', () => {
    render(<VideoConfigSection />);
    openDetails();
    const presetOptions = Array.from(cameraPresetSelect().options);
    const findOpt = (re: RegExp) => presetOptions.find((o) => re.test(o.textContent ?? ''))!;
    expect(findOpt(/1080p 30fps/).textContent).toContain('Premium');
    expect(findOpt(/1440p/).textContent).toContain('Premium');
    expect(findOpt(/4K/).textContent).toContain('Premium');
  });

  it('locked (free): 720p30 / 720p60 / System Default presets are NOT marked premium', () => {
    render(<VideoConfigSection />);
    openDetails();
    const presetOptions = Array.from(cameraPresetSelect().options);
    const findOpt = (re: RegExp) => presetOptions.find((o) => re.test(o.textContent ?? ''))!;
    expect(findOpt(/720p 30fps/).textContent).not.toContain('Premium');
    expect(findOpt(/720p 60fps/).textContent).not.toContain('Premium');
    expect(findOpt(/System Default/).textContent).not.toContain('Premium');
  });

  it('locked (free): 1080p60 exceeds the camera height (720) → marked premium', () => {
    render(<VideoConfigSection />);
    openDetails();
    const opt = Array.from(cameraPresetSelect().options).find((o) =>
      /1080p 60fps/.test(o.textContent ?? '')
    )!;
    expect(opt.textContent).toContain('Premium');
  });

  it('locked (free): selecting a premium preset snaps back to the highest free camera preset + chip', () => {
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '4K60' } });
    // Highest free camera preset is 720p60 (the camera axis is 720p60).
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '720p60');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('cameraPreset', '4K60');
    expect(screen.getByRole('button', { name: /Premium/ })).toBeInTheDocument();
  });

  it('pre-hydrate: does NOT snap a camera preset back (no premium downgrade persisted) (#2172)', () => {
    // Pre-hydrate the snap-back fails open (a premium user's real entitlement has not
    // arrived), so 4K60 passes through unchanged instead of being snapped to 720p60; the
    // snap re-fires once the entitlement is authoritative.
    useSubscriptionStore.setState({ hydrated: false, degraded: false });
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '4K60' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '4K60');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('cameraPreset', '720p60');
  });

  it('degraded free (first-load failure): STILL snaps a premium preset back to the highest free preset (#2172)', () => {
    // Degraded-free is a FIRST-LOAD entitlements fetch failure: the store falls closed to
    // the free floor (hydrated:false, degraded:true, tier:free). Camera has no
    // produce-boundary clamp and launch-reset is hydration-gated, so this snap-back is the
    // sole enforcement seam and MUST fire here, closing the monetization escape a
    // hydrated-only gate left open.
    useSubscriptionStore.setState({ hydrated: false, degraded: true });
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '4K60' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '720p60');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('cameraPreset', '4K60');
  });

  it('degraded premium (reconnect failure): does NOT snap a preset back, fails open on the preserved tier (#2172)', () => {
    // A reconnect failure preserves the last-known PREMIUM tier (degraded:true,
    // tier !== free), so the shared gate fails OPEN even though the free-shaped caps would
    // mark 4K60 over-cap. This isolates the gate (not the caps): 4K60 passes through.
    setEntitlement({ tier: 'premium' });
    useSubscriptionStore.setState({ hydrated: true, degraded: true });
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '4K60' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '4K60');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('cameraPreset', '720p60');
  });

  it('locked (free): selecting a FREE camera preset passes through unchanged', () => {
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(cameraPresetSelect(), { target: { value: '720p30' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraPreset', '720p30');
  });

  it('entitled (native camera caps): no preset is locked; 4K passes through', () => {
    setEntitlement({ cameraMaxHeight: -1, cameraMaxFps: -1 });
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

  it('locked (free): shows the note when only the TIERED fps is exceeded on a 1080p/60Hz display (#2172)', async () => {
    // A 1080p display fits the free height cap (1080) — the height arm does NOT exceed.
    // But 1080p is pixel-rate-tiered to 30fps and the device does 60Hz, so it genuinely
    // exceeds its tier via fps. The note must show even though height fits, matching the
    // fps dropdown (which tiers to 30). Regression guard for nativeExceedsFree using the
    // raw axis fps (60) instead of the resolution-tiered ceiling (30).
    globalThis.electron = {
      getDisplayInfo: vi
        .fn()
        .mockResolvedValue([
          { width: 1920, height: 1080, refreshRate: 60, scaleFactor: 1, isPrimary: true },
        ]),
    } as unknown as typeof globalThis.electron;
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() =>
      expect(document.querySelector('.settings-native-exceeds-note')).toBeInTheDocument()
    );
    // 60 FPS is not offered at 1080p (tiered to 30), confirming the exceed is fps-only.
    expect(screen.queryByRole('option', { name: '60 FPS' })).not.toBeInTheDocument();
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

  it('locked (free): source/native on a 4K display is tiered to 30fps (no 60/90/120)', async () => {
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      expect(screen.queryByRole('option', { name: '120 FPS' })).not.toBeInTheDocument();
      expect(screen.queryByRole('option', { name: '90 FPS' })).not.toBeInTheDocument();
    });
    // #2163: source resolves to the 4K display, clamped to 1080p for free → 30fps
    // max, so 60 FPS is NOT offered; 30 FPS is.
    expect(screen.queryByRole('option', { name: '60 FPS' })).not.toBeInTheDocument();
    expect(screen.getByRole('option', { name: '30 FPS' })).toBeInTheDocument();
  });

  it('entitled (native stream caps): native-exceeds note hidden; 4K / 120 FPS offered', async () => {
    setEntitlement({ streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() =>
      expect(screen.getByRole('option', { name: '4K (3840×2160)' })).toBeInTheDocument()
    );
    expect(screen.queryByText(/Your device supports more/)).not.toBeInTheDocument();
    expect(screen.getByRole('option', { name: '120 FPS' })).toBeInTheDocument();
  });
});

// ─── #2163: resolution-tiered screenshare fps ───────────────────────────────

const screenResolutionSelect = (): HTMLSelectElement =>
  document.querySelectorAll('select.settings-select')[1] as HTMLSelectElement;

const screenFrameRateSelect = (): HTMLSelectElement =>
  document.querySelectorAll('select.settings-select')[2] as HTMLSelectElement;

describe('VideoConfigSection — #2163 resolution-tiered screenshare fps', () => {
  it('free 720p offers 60 FPS (60fps reserved for 720p and below)', async () => {
    mockDraft({ screenResolution: '720p' });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => expect(screen.getByRole('option', { name: '60 FPS' })).toBeInTheDocument());
  });

  it('free 1080p does NOT offer 60 FPS (offers 30)', async () => {
    mockDraft({ screenResolution: '1080p' });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      expect(screen.queryByRole('option', { name: '60 FPS' })).not.toBeInTheDocument();
      expect(screen.getByRole('option', { name: '30 FPS' })).toBeInTheDocument();
    });
  });

  it('free ultrawide (custom 2560x1080) gates 24/15/5 too — budget admits only 22fps', async () => {
    // The pixel-rate budget (62.2 Mpx/s / 2,764,800 px) admits ~22fps, so even the
    // low fixed fps values (24/30/60) are over budget and must be omitted, not just
    // silently clamped at the produce boundary. Only 15/5 (≤22) survive.
    mockDraft({ screenResolution: '2560x1080' });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      expect(screen.queryByRole('option', { name: '24 FPS (Cinematic)' })).not.toBeInTheDocument();
      expect(screen.queryByRole('option', { name: '30 FPS' })).not.toBeInTheDocument();
      expect(screen.queryByRole('option', { name: '60 FPS' })).not.toBeInTheDocument();
      expect(screen.getByRole('option', { name: '15 FPS' })).toBeInTheDocument();
      expect(screen.getByRole('option', { name: '5 FPS (Slideshow)' })).toBeInTheDocument();
    });
  });

  it('snaps fps down to the tiered max + shows chip when switching 720p60 → 1080p', () => {
    mockDraft({ screenResolution: '720p', screenFrameRate: 60 });
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(screenResolutionSelect(), { target: { value: '1080p' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenResolution', '1080p');
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenFrameRate', 30);
    expect(screen.getByRole('button', { name: /Premium/ })).toBeInTheDocument();
  });

  it('pre-hydrate: does NOT snap fps on resolution change (no premium downgrade persisted) (#2172)', () => {
    // Before the entitlement hydrates, the ceiling is the FREE default. Snapping would
    // persist a downgraded 30fps for a premium user that launch-reset never restores, so
    // the snap is gated on `hydrated`; the resolution change still applies.
    useSubscriptionStore.setState({ hydrated: false, degraded: false });
    mockDraft({ screenResolution: '720p', screenFrameRate: 60 });
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(screenResolutionSelect(), { target: { value: '1080p' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenResolution', '1080p');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('screenFrameRate', expect.anything());
  });

  it('pre-hydrate: offers the full fps ladder at 1080p with no upsell (fails open like the picker) (#2172)', async () => {
    // Before the entitlement hydrates, the display seams must fail OPEN through the shared
    // effectiveStreamAxis gate, exactly like the picker and produce boundary, so a premium
    // user whose entitlement has not arrived is never transiently shown a FREE fps ceiling.
    // 1080p then offers 60 FPS unmarked and the native-exceeds upsell note stays hidden.
    // Regression guard against Settings tiering fps off the raw pre-hydrate FREE axis while
    // the picker fails open (the seam divergence in #2172).
    useSubscriptionStore.setState({ hydrated: false, degraded: false });
    mockDraft({ screenResolution: '1080p' });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      const opt = screen.getByRole('option', { name: '60 FPS' }) as HTMLOptionElement;
      expect(opt.textContent).not.toContain('Premium');
    });
    expect(screen.queryByText(/Your device supports more/)).not.toBeInTheDocument();
  });

  it('does NOT snap fps when switching to a resolution that still admits the current fps', () => {
    mockDraft({ screenResolution: '1080p', screenFrameRate: 30 });
    render(<VideoConfigSection />);
    openDetails();
    fireEvent.change(screenResolutionSelect(), { target: { value: '720p' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('screenResolution', '720p');
    expect(mockSetDraftVideoSetting).not.toHaveBeenCalledWith('screenFrameRate', expect.anything());
  });

  it('premium offers 60 FPS at 1080p (native — no pixel-rate tier)', async () => {
    setEntitlement({ streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 });
    mockDraft({ screenResolution: '1080p' });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => expect(screen.getByRole('option', { name: '60 FPS' })).toBeInTheDocument());
  });

  it('does NOT mark an in-tier injected fps as Premium (free ultrawide 22fps ceiling, #2172 Codex)', async () => {
    // On a free 2560x1080 ultrawide the tiered ceiling is 22fps. When the resolution
    // change writes screenFrameRate:22, that value is injected as its own option (not
    // a fixed choice). It is the allowed ceiling the clamp just selected, so it must
    // render PLAIN — not with a paywall marker on the value capture will actually use.
    mockDraft({ screenResolution: '2560x1080', screenFrameRate: 22 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      const opt = screen.getByRole('option', { name: /22 FPS/ }) as HTMLOptionElement;
      expect(opt.value).toBe('22');
      expect(opt.textContent).not.toContain('Premium');
    });
  });

  it("keeps a persisted over-cap fps ('source' + 60 on free) selectable as a 🔒 Premium option (no blank select)", async () => {
    // A downgraded free user with screenResolution:'source' + 60fps: the tiered
    // ceiling for source (clamped to 1080p) is 30, so 60 is not a normal option.
    // The launch-reset clamp leaves 'source'/native fps to the produce boundary,
    // so the value survives — it must still render as a (locked) option, otherwise
    // the frame-rate <select> holds "60" with no matching <option> and goes blank.
    mockDraft({ screenResolution: 'source', screenFrameRate: 60 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      const opt = screen.getByRole('option', { name: /60 FPS/ }) as HTMLOptionElement;
      expect(opt.value).toBe('60');
      expect(opt.textContent).toContain('Premium');
    });
    // The select actually resolves to the injected option (value present, not blank).
    expect(screenFrameRateSelect().value).toBe('60');
  });

  it('does NOT inject a locked fps option when the persisted fps is within the tiered ceiling', async () => {
    // Free 'source' (→ 1080p) admits 30fps, so 30 is a normal option and no 🔒
    // duplicate is added; 60 remains absent.
    mockDraft({ screenResolution: 'source', screenFrameRate: 30 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => expect(screen.getByRole('option', { name: '30 FPS' })).toBeInTheDocument());
    // No over-cap fps option is injected (30 is already offered). '60 FPS' here is
    // the frame-rate axis only — camera presets use lowercase "60fps".
    expect(screen.queryByRole('option', { name: /60 FPS/ })).not.toBeInTheDocument();
  });

  it('free source on a 144Hz display marks the Native fps option premium', async () => {
    // Native resolves to the display refresh rate (144) at produce time, then the
    // boundary clamps it to the tiered ceiling (source → 1080p → 30). The Native
    // option label must say Premium so it matches what is actually captured.
    mockDraft({ screenResolution: 'source', screenFrameRate: 0 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      const opt = screen.getByRole('option', { name: /Native \(\d+ Hz\)/ }) as HTMLOptionElement;
      expect(opt.value).toBe('0');
      expect(opt.textContent).toContain('Premium');
    });
  });

  it('premium leaves the Native fps option unmarked (no tiered ceiling)', async () => {
    setEntitlement({ streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 });
    mockDraft({ screenResolution: 'source', screenFrameRate: 0 });
    render(<VideoConfigSection />);
    openDetails();
    await waitFor(() => {
      const opt = screen.getByRole('option', { name: /Native \(\d+ Hz\)/ }) as HTMLOptionElement;
      expect(opt.textContent).not.toContain('Premium');
    });
  });
});

// ─── L5: manual screen-share (stream) bitrate clamp ─────────────────────────

describe('VideoConfigSection — L5 manual STREAM bitrate clamp', () => {
  beforeEach(() => {
    videoAdvancedMode = true; // the screen-share bitrate slider lives in the advanced section
    mockDraft({ screenShareBitrate: 4_000_000 }); // manual cap, within free range
  });

  it('locked (free): the stream bitrate slider max is fenced at the free stream cap (5 Mbps)', () => {
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    expect(slider).toBeInTheDocument();
    expect(slider.max).toBe('5');
  });

  it('locked (free): renders the "beyond 5 Mbps" ghost-zone with the lock chip', () => {
    render(<VideoConfigSection />);
    openDetails();
    const ghostZone = document.querySelector('.settings-bitrate-ghost-zone') as HTMLElement;
    expect(ghostZone).toBeInTheDocument();
    expect(screen.getByText(/beyond 5 Mbps/)).toBeInTheDocument();
    // Scope the lock-glyph check to the ghost-zone: the screen-share Resolution
    // row also carries a "device supports more" premium chip (native-exceeds L6),
    // so a document-wide getByLabelText would be ambiguous.
    expect(ghostZone.querySelector('[aria-label="Premium feature"]')).toBeInTheDocument();
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
      section: 'subscriptions',
      controlId: 'section-current-plan',
    });
  });

  it('entitled (premium tier): the slider max is the premium stream cap (20 Mbps), no ghost-zone', () => {
    // Premium: tier drives the ghost-zone (not the axis value), so a premium user
    // at their finite 20 Mbps stream ceiling sees no upsell.
    setEntitlement({ tier: 'premium', streamMaxBitrate: 20_000_000 });
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    expect(slider.max).toBe('20');
    expect(document.querySelector('.settings-bitrate-ghost-zone')).not.toBeInTheDocument();
  });
});

// ─── L5b: manual CAMERA bitrate clamp (split camera axis, #1602) ─────────────

describe('VideoConfigSection — L5 manual CAMERA bitrate clamp', () => {
  beforeEach(() => {
    // The camera bitrate control lives in the (always-visible) Camera section.
    mockDraft({ cameraBitrate: 2_000_000 }); // manual cap, within free camera range
  });

  it('locked (free): the camera bitrate slider max is fenced at the free camera cap (2.5 Mbps)', () => {
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    expect(slider).toBeInTheDocument();
    expect(slider.max).toBe('2.5');
  });

  it('locked (free): renders the "beyond 2.5 Mbps" camera ghost-zone with the lock chip', () => {
    render(<VideoConfigSection />);
    openDetails();
    expect(document.querySelector('.settings-bitrate-ghost-zone')).toBeInTheDocument();
    expect(screen.getByText(/beyond 2.5 Mbps/)).toBeInTheDocument();
  });

  it('locked (free): a camera value above the cap clamps to the free camera cap', () => {
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    fireEvent.change(slider, { target: { value: '2.5' } });
    expect(mockSetDraftVideoSetting).toHaveBeenCalledWith('cameraBitrate', 2_500_000);
  });

  it('entitled (premium tier): the camera slider max is 6 Mbps, no ghost-zone', () => {
    setEntitlement({ tier: 'premium', cameraMaxBitrate: 6_000_000 });
    render(<VideoConfigSection />);
    openDetails();
    const slider = document.querySelector('.settings-volume-slider') as HTMLInputElement;
    expect(slider.max).toBe('6');
    expect(document.querySelector('.settings-bitrate-ghost-zone')).not.toBeInTheDocument();
  });
});
