import React from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useVideoSettingsStore } from '@/renderer/stores/voice/videoSettingsStore';
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/ScreenSharePicker.css', () => ({}));

// Mock CustomSelect to simplify testing
vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    value,
    onChange,
    options,
    id,
  }: {
    value: string;
    onChange: (v: string) => void;
    options: { value: string; label: string }[];
    id?: string;
  }) => (
    <select data-testid={id} value={value} onChange={(e) => onChange(e.target.value)}>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  ),
}));

const mockSources = [
  { id: 'screen:0', name: 'Entire Screen', thumbnail: 'thumb1', appIcon: null },
  { id: 'window:1', name: 'VS Code', thumbnail: 'thumb2', appIcon: 'icon1' },
  { id: 'window:2', name: 'Chrome', thumbnail: 'thumb3', appIcon: null },
];

import ScreenSharePicker from '@/renderer/components/Voice/ScreenSharePicker';

describe('ScreenSharePicker', () => {
  const mockOnSelect = vi.fn();
  const mockOnCancel = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();

    // Mock electron.getDesktopSources — electron is already writable from setup.ts
    (globalThis as Record<string, unknown>).electron = {
      ...(globalThis.electron || {}),
      getDesktopSources: vi.fn().mockResolvedValue(mockSources),
    };
  });

  it('renders loading state initially', () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    expect(screen.getByText('Loading sources...')).toBeInTheDocument();
  });

  it('renders screens and windows after loading', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Screens')).toBeInTheDocument();
      expect(screen.getByText('Windows')).toBeInTheDocument();
    });
    expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    expect(screen.getByText('VS Code')).toBeInTheDocument();
    expect(screen.getByText('Chrome')).toBeInTheDocument();
  });

  it('renders title with Share Your Screen', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
  });

  it('selects a source on click', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    // Share button should now be enabled
    const shareBtn = screen.getByText('Share');
    expect(shareBtn).not.toBeDisabled();
  });

  it('Share button is disabled when no source is selected', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Share')).toBeInTheDocument();
    });
    expect(screen.getByText('Share')).toBeDisabled();
  });

  it('calls onSelect with source ID and options on confirm', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith('screen:0', {
      resolution: 'source',
      frameRate: 30,
      contentType: 'auto',
    });
  });

  it('calls onCancel when Cancel button is clicked', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Cancel')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('calls onCancel when close button is clicked', async () => {
    const { container } = render(
      <ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />
    );
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
    const closeBtn = container.querySelector('.screen-picker__close');
    expect(closeBtn).toBeInTheDocument();
    fireEvent.click(closeBtn!);
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('calls onCancel when Escape key is pressed', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('calls onCancel when overlay background is clicked', async () => {
    const { container } = render(
      <ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />
    );
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
    const overlay = container.querySelector('.screen-picker-overlay');
    fireEvent.click(overlay!);
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('reads default settings from video settings store', async () => {
    // 720p60 is in-tier for free (60fps reserved for 720p and below, #2163), so
    // the stored defaults flow through unclamped and this exercises store reading
    // without the tiered-fps clamp interfering.
    useVideoSettingsStore.setState({
      screenResolution: '720p',
      screenFrameRate: 60,
      screenContentType: 'motion',
    });

    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });

    // Select a source and confirm to verify options use store defaults
    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith('screen:0', {
      resolution: '720p',
      frameRate: 60,
      contentType: 'motion',
    });
  });

  it('changes local resolution when user selects from dropdown', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });

    const resolutionSelect = screen.getByTestId('screen-resolution');
    fireEvent.change(resolutionSelect, { target: { value: '720p' } });

    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith(
      'screen:0',
      expect.objectContaining({
        resolution: '720p',
      })
    );
  });

  it('handles missing electron.getDesktopSources gracefully', async () => {
    (globalThis as Record<string, unknown>).electron = {};

    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    // Should stop loading without crashing
    await waitFor(() => {
      expect(screen.queryByText('Loading sources...')).not.toBeInTheDocument();
    });
  });

  it('logs error when getDesktopSources throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    (globalThis as Record<string, unknown>).electron = {
      getDesktopSources: vi.fn().mockRejectedValue(new Error('IPC error')),
    };

    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to get desktop sources:', 'IPC error');
    });
    // Loading ends even on error
    expect(screen.queryByText('Loading sources...')).not.toBeInTheDocument();
    consoleSpy.mockRestore();
  });

  // ─── #2163: tier the per-share picker to the stream entitlement ──────────
  // The picker must mirror the produce-boundary clamp so it never offers an fps
  // the capture will silently drop. Default entitlement (after resetAllStores) is
  // FREE (streamMaxPixelRate = 1080p30; 720p60 admitted, 1080p60 rejected).
  describe('#2163 resolution-tiered fps', () => {
    // The tiered clamp requires an AUTHORITATIVE entitlement — the picker fails open
    // pre-hydrate (#2172). Mark the store hydrated so free/premium ceilings actually
    // apply; the pre-hydrate fail-open + degraded-premium cases have their own tests below.
    beforeEach(() => {
      useSubscriptionStore.setState({ hydrated: true, degraded: false });
    });

    it('free 1080p marks 60 FPS premium and offers 30 FPS unmarked', async () => {
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      const sixty = screen.getByRole('option', { name: /60 FPS/ });
      expect(sixty.textContent).toContain('Premium');
      // 30 is in-tier, so it renders as a plain (unmarked) option.
      expect(screen.getByRole('option', { name: '30 FPS' })).toBeInTheDocument();
    });

    it('free 720p offers 60 FPS unmarked (60fps reserved for 720p and below)', async () => {
      useVideoSettingsStore.setState({ screenResolution: '720p', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      const sixty = screen.getByRole('option', { name: '60 FPS' });
      expect(sixty.textContent).not.toContain('Premium');
    });

    it('free 1080p + persisted 60fps confirms the clamped 30fps (matches capture)', async () => {
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p', frameRate: 30 })
      );
    });

    it('free 1080p snaps back a selected over-cap 60fps to the tier ceiling', async () => {
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      fireEvent.change(screen.getByTestId('screen-framerate'), { target: { value: '60' } });
      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 30 })
      );
    });

    it('free ultrawide source injects the deliverable fps as its own option (no blank control, no snap-down)', async () => {
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3440, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // 3440x1440 'source' clamps to 2580x1080 => 22fps, the actual deliverable ceiling.
      // 22 is not a discrete choice, so it is injected as its own option and the value
      // holds it — the shown/captured fps equals what produce delivers (22), NOT a
      // snapped-down 15 that would under-deliver (#2172).
      const fpsSelect = screen.getByTestId('screen-framerate') as HTMLSelectElement;
      await waitFor(() => expect(fpsSelect.value).toBe('22'));
      expect(fpsSelect.selectedIndex).toBeGreaterThanOrEqual(0);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 22 })
      );
    });

    it('injects a Settings-only persisted fps (24) that is within the tier ceiling', async () => {
      // 24fps is offered in the Settings UI but not the picker's {5,15,30,60}. On a
      // 16:9 source (free 1080p ceiling 30) it is within tier, so it is injected and
      // preserved — NOT snapped down to 15, which would under-deliver (#2172).
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 24 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      const fpsSelect = screen.getByTestId('screen-framerate') as HTMLSelectElement;
      expect(fpsSelect.value).toBe('24');
      expect(fpsSelect.selectedIndex).toBeGreaterThanOrEqual(0);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 24 })
      );
    });

    it('free source fails open (no 4K-fallback truncation) until getDisplayInfo resolves (#2172 Codex)', async () => {
      // Before the real display dims are known, 'source' must NOT be tiered against the
      // 4K fallback — that would truncate a free 720p-display Native share to 30fps even
      // though produce (which resolves the real dims) allows 60. Fail open; produce is
      // authoritative. Here getDisplayInfo never resolves, so the picker holds 60.
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockReturnValue(new Promise(() => {})),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      const fpsSelect = screen.getByTestId('screen-framerate') as HTMLSelectElement;
      expect(fpsSelect.value).toBe('60');

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 60 })
      );
    });

    it('free source with NO getDisplayInfo bridge tiers to the 4K default, not a crash or fail-open (#2172 Codex FP)', async () => {
      // Dev/web: electron exists but has no getDisplayInfo. The effect must not throw
      // (optional-chain short-circuits) AND must resolve displayInfo to [] so 'source'
      // tiers against the conservative 4K fallback (free → 30) — display == capture,
      // matching produceScreen's own 4K fallback. NOT the pending-race fail-open above.
      (globalThis as Record<string, unknown>).electron = {
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      const fpsSelect = screen.getByTestId('screen-framerate') as HTMLSelectElement;
      await waitFor(() => expect(fpsSelect.value).toBe('30'));

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 30 })
      );
    });

    it('premium: preserves a persisted over-60 fps (120) instead of snapping to the 60-max list (#2172 regression guard)', async () => {
      const ent = useSubscriptionStore.getState().entitlement;
      useSubscriptionStore.setState({
        entitlement: { ...ent, streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 },
      });
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 120 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // Premium native caps => Infinity ceiling => 120 is in-tier. It is not one of the
      // {5,15,30,60} choices, so it is injected and preserved; the picker must NOT snap
      // it down to 60 and silently halve the entitled capture rate — the merge-induced
      // regression this test locks out.
      const fpsSelect = screen.getByTestId('screen-framerate') as HTMLSelectElement;
      expect(fpsSelect.value).toBe('120');
      expect(screen.getByRole('option', { name: '120 FPS' })).toBeInTheDocument();

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 120 })
      );
    });

    it('premium (native stream caps) leaves 1080p60 unmarked and unclamped', async () => {
      const ent = useSubscriptionStore.getState().entitlement;
      useSubscriptionStore.setState({
        entitlement: { ...ent, streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 },
      });
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      expect(screen.getByRole('option', { name: '60 FPS' }).textContent).not.toContain('Premium');
      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p', frameRate: 60 })
      );
    });

    it('pre-hydrate: fails open, does NOT clamp a premium fps against the free floor (#2172)', async () => {
      // Before the entitlement hydrates, the store holds the free floor. The picker must
      // fail open (like the produce boundary) rather than clamp a premium user's saved
      // 1080p60 down to 30 — otherwise an immediate share after login under-delivers.
      useSubscriptionStore.setState({ hydrated: false, degraded: false });
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // 60 FPS offered UNMARKED (no tier known yet) and confirmed unchanged.
      expect(screen.getByRole('option', { name: '60 FPS' }).textContent).not.toContain('Premium');
      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p', frameRate: 60 })
      );
    });

    it('degraded premium: fails open, a transient reconnect failure does not clamp premium (#2172)', async () => {
      // The store preserves the last-known premium tier on a degraded reconnect, so a
      // degraded-premium user keeps tier:'premium' and effectiveStreamAxis fails open —
      // their share is not clamped to free by a transient /entitlements failure.
      const ent = useSubscriptionStore.getState().entitlement;
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: true,
        entitlement: {
          ...ent,
          tier: 'premium',
          streamMaxHeight: -1,
          streamMaxFps: -1,
          streamMaxPixelRate: -1,
        },
      });
      useVideoSettingsStore.setState({ screenResolution: '1080p', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      expect(screen.getByRole('option', { name: '60 FPS' }).textContent).not.toContain('Premium');
      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ frameRate: 60 })
      );
    });

    it('free over-cap display marks Source Native Premium but still sends resolution:source (#2172 Codex)', async () => {
      // A free user on a 1440p display leaving the picker at Source Native: produceScreen
      // clamps the capture to 1080p, so the label must not promise Native. Mark it Premium
      // to match what capture produces, but keep sending 'source' (display-only, the
      // produce boundary stays authoritative). resetAllStores does NOT reset the
      // subscription store, so pin the free entitlement (a prior test may have left it
      // premium/native).
      useSubscriptionStore.setState({ entitlement: FREE_ENTITLEMENT });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 2560, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      await waitFor(() =>
        expect(screen.getByRole('option', { name: /Source Native/ }).textContent).toContain(
          'Premium'
        )
      );
      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: 'source' })
      );
    });

    it('free at-cap 1080p display leaves Source Native unmarked (capture is native)', async () => {
      useSubscriptionStore.setState({ entitlement: FREE_ENTITLEMENT });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 1920, height: 1080 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // Height fits the cap, so nothing is clamped and the label stays plain. Wait for the
      // fps option to settle (displayInfo resolved) before asserting the resolution label.
      await waitFor(() =>
        expect(screen.getByRole('option', { name: '30 FPS' })).toBeInTheDocument()
      );
      expect(screen.getByRole('option', { name: /Source Native/ }).textContent).not.toContain(
        'Premium'
      );
    });

    it('pre-hydrate over-cap display: Source Native stays unmarked (fails open) (#2172)', async () => {
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useSubscriptionStore.setState({ hydrated: false, degraded: false });
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // Fail open: no authoritative tier yet, so 60 FPS is unmarked and Source Native is
      // not gated even though the raw display exceeds the free height cap.
      expect(screen.getByRole('option', { name: '60 FPS' }).textContent).not.toContain('Premium');
      expect(screen.getByRole('option', { name: /Source Native/ }).textContent).not.toContain(
        'Premium'
      );
    });

    it('premium (native caps): Source Native stays unmarked on an over-cap display', async () => {
      const ent = useSubscriptionStore.getState().entitlement;
      useSubscriptionStore.setState({
        entitlement: { ...ent, streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 },
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      expect(screen.getByRole('option', { name: /Source Native/ }).textContent).not.toContain(
        'Premium'
      );
    });
  });
});
