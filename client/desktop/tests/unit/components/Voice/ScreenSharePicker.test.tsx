import React from 'react';
import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { useVideoSettingsStore } from '@/renderer/stores/voice/videoSettingsStore';
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';

vi.mock('@/renderer/components/Voice/ScreenSharePicker.css', () => ({}));

// Mock CustomSelect to simplify testing. `disabled` is passed through to the
// rendered <option> -- without this every disabled-option assertion below is
// vacuous, since the mock would silently drop the very prop being asserted on.
vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    value,
    onChange,
    options,
    id,
  }: {
    value: string;
    onChange: (v: string) => void;
    options: { value: string; label: string; disabled?: boolean }[];
    id?: string;
  }) => (
    <select data-testid={id} value={value} onChange={(e) => onChange(e.target.value)}>
      {options.map((o) => (
        <option key={o.value} value={o.value} disabled={o.disabled}>
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
import { openForeignModal } from '../../../helpers/foreignModal';

/** Sources are behind tabs now; open one by its tab button. */
const openTab = (name: 'Screens' | 'Windows') =>
  fireEvent.click(screen.getByRole('tab', { name: new RegExp(`^${name}`) }));

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

  it('renders screens on the default tab and windows behind the Windows tab', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /^Screens/ })).toBeInTheDocument();
    });
    // Screens is the default tab.
    expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    expect(screen.queryByText('VS Code')).not.toBeInTheDocument();

    openTab('Windows');
    expect(screen.getByText('VS Code')).toBeInTheDocument();
    expect(screen.getByText('Chrome')).toBeInTheDocument();
    expect(screen.queryByText('Entire Screen')).not.toBeInTheDocument();
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
      streamAudio: true,
    });
  });

  // #2161 / ADR-0043: Electron's desktop audio capture is a whole-system loopback that
  // ignores chromeMediaSourceId, so a window target asking for audio would leak every
  // application's sound to the channel. The picker must never request it, whatever the
  // persisted preference says.
  it('never requests audio for a window target, even with the preference on', async () => {
    useVideoSettingsStore.getState().setScreenStreamAudio(true);
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /^Windows/ })).toBeInTheDocument();
    });
    openTab('Windows');
    fireEvent.click(screen.getByText('VS Code'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith(
      'window:1',
      expect.objectContaining({ streamAudio: false })
    );
  });

  // Replaces a test that asserted VS Code and Chrome became two application
  // HEADINGS in an Applications tab. That tab is gone -- it showed the same
  // windows the Windows tab showed, in a different layout, under names guessed
  // from window titles. This is the durable form of the same fixture.
  it('offers exactly two tabs, with every window in one flat grid', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /^Screens/ })).toBeInTheDocument();
    });

    expect(screen.getAllByRole('tab')).toHaveLength(2);
    expect(screen.queryByRole('tab', { name: /^Applications/ })).not.toBeInTheDocument();

    openTab('Windows');
    // Both mock windows -- VS Code, which reports an appIcon, and Chrome, which
    // does not -- sit in the same grid. The icon no longer decides a layout.
    expect(screen.getByText('VS Code')).toBeInTheDocument();
    expect(screen.getByText('Chrome')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: /VS Code/ })).not.toBeInTheDocument();
  });

  it('moves between tabs with the arrow keys (WAI-ARIA tabs pattern)', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /^Screens/ })).toBeInTheDocument();
    });
    fireEvent.keyDown(screen.getByRole('tab', { name: /^Screens/ }), { key: 'ArrowRight' });
    expect(screen.getByRole('tab', { name: /^Windows/ })).toHaveAttribute('aria-selected', 'true');

    // Wrap-around, which the three-tab version never reached: ArrowRight from the
    // middle tab just advanced. With two tabs it is the only way back to the first.
    fireEvent.keyDown(screen.getByRole('tab', { name: /^Windows/ }), { key: 'ArrowRight' });
    expect(screen.getByRole('tab', { name: /^Screens/ })).toHaveAttribute('aria-selected', 'true');

    fireEvent.keyDown(screen.getByRole('tab', { name: /^Screens/ }), { key: 'End' });
    expect(screen.getByRole('tab', { name: /^Windows/ })).toHaveAttribute('aria-selected', 'true');
  });

  it('enables the audio toggle for a screen target and disables it for a window', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    const audio = () => screen.getByRole('button', { name: /^Stream Audio\b/ });
    // aria-disabled, NEVER the native `disabled` attribute (#3198 PR 2, §6 a11y floor) --
    // a native `disabled` button drops out of the tab order, which would make the
    // persistent hint below keyboard/AT-unreachable.
    const hint = () => screen.getByText((_, el) => el?.id === 'screen-audio-hint');

    // Nothing selected yet -- inert, and it says why.
    expect(audio()).toHaveAttribute('aria-disabled', 'true');
    expect(audio()).not.toBeDisabled();

    fireEvent.click(screen.getByText('Entire Screen'));
    expect(audio()).toHaveAttribute('aria-disabled', 'false');
    expect(audio()).toHaveAttribute('aria-pressed', 'true');

    openTab('Windows');
    fireEvent.click(screen.getByText('VS Code'));
    expect(audio()).toHaveAttribute('aria-disabled', 'true');
    // The disabled reason is user-visible PERSISTENTLY, not just implied by the greyed
    // control and not only in a `title=` a mouse-only user would have to hover.
    expect(hint()).toHaveTextContent(/isn.t available on this computer/);
    expect(audio()).toHaveAttribute('aria-describedby', 'screen-audio-hint');

    // The activation guard refuses the click while locked (#3198 PR 2).
    fireEvent.click(audio());
    expect(audio()).toHaveAttribute('aria-pressed', 'false');

    // Prove the guard actually blocked the flip rather than merely hiding it:
    // `aria-pressed` reads false here regardless of streamAudio's real value,
    // because `audioCapable` alone gates the attribute while locked. Return to
    // a capable target and read the pill text, which is the only place the
    // underlying streamAudio value becomes visible again. If the guard were
    // removed, the click above would have flipped streamAudio true -> false
    // while locked, and this would read 'Off' instead of 'Desktop'.
    openTab('Screens');
    fireEvent.click(screen.getByText('Entire Screen'));
    expect(audio()).toHaveTextContent('Desktop');
  });

  // The pill is the ONLY place the UI states WHAT is being sent, rather than merely
  // whether anything is -- and it is therefore the one label here that can OVERCLAIM.
  // It had no test at all until this case: the 42 others assert aria state, the hint text
  // and the emitted options, so the label could have said anything and stayed green.
  it('names what is actually being sent, and never claims app audio this PR cannot deliver', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    const pill = () => screen.getByRole('button', { name: /^Stream Audio\b/ });

    // 'Desktop', never 'On': a screen share carries the WHOLE-SYSTEM mix, including the
    // other monitor and every other app (#2161). The label has to say which.
    fireEvent.click(screen.getByText('Entire Screen'));
    expect(pill()).toHaveTextContent('Desktop');
    // ACCESSIBLE NAME, not just text content -- the m1 pin. `aria-labelledby`
    // OVERRIDES element contents, so while it named only the "Stream Audio" span
    // this pill's own text was announced by NOTHING: a sighted-only signal on the
    // one control this PR made AT-reachable. `toHaveTextContent` passed throughout
    // that defect, which is exactly why the assertion has to be on the computed name.
    expect(pill()).toHaveAccessibleName('Stream Audio Desktop');

    // streamAudio is still ON here, so this 'Off' can ONLY come from the capability
    // verdict. Toggling first would let the switch produce it and the case would pass
    // whatever the verdict said -- the two causes have to be separated to pin either.
    openTab('Windows');
    fireEvent.click(screen.getByText('VS Code'));
    expect(pill()).toHaveTextContent('Off');
    expect(pill()).toHaveAccessibleName('Stream Audio Off');
    // 'per-process' is unreachable until PR 3 wires the seam (no production call
    // site passes canCarryScreenAudio's third argument), so this component can
    // never compute a verdict of 'per-process' and this assertion could not fail
    // regardless of what the pill renders -- `verdictOffersAudio('per-process')`
    // is false, which is what actually keeps 'App' from appearing here. See
    // `screenAudioVerdictAgreement.test.ts` for the test that pins that.

    // ...and on a capable target the switch alone still reaches Off, so the label tracks
    // BOTH inputs rather than collapsing to whichever one this test happened to move.
    openTab('Screens');
    fireEvent.click(screen.getByText('Entire Screen'));
    expect(pill()).toHaveTextContent('Desktop');
    fireEvent.click(pill());
    expect(pill()).toHaveTextContent('Off');
  });

  it('passes the toggled-off audio choice through for a screen target', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByRole('button', { name: /^Stream Audio\b/ }));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith(
      'screen:0',
      expect.objectContaining({ streamAudio: false })
    );
  });

  // Restored in afterEach: without that, the Linux case below leaks into every
  // subsequent test and silently makes them assert against a platform they never set.
  let origElectron: typeof globalThis.electron;
  const withPlatform = (p: string) => {
    origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      getPlatform: vi.fn().mockResolvedValue(p),
    } as unknown as typeof globalThis.electron;
  };
  afterEach(() => {
    if (origElectron !== undefined) {
      globalThis.electron = origElectron;
      origElectron = undefined as unknown as typeof globalThis.electron;
    }
  });

  // The spec's capability ladder makes every Linux target audio-incapable: this capture
  // path has no Linux loopback. Gating on the `screen:` prefix alone offered an enabled,
  // default-on control there for an operation that cannot succeed.
  it('disables the audio toggle on Linux even for a screen target', async () => {
    withPlatform('linux');
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /^Stream Audio\b/ })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
    });
    // OQ1 ruling: the Linux arm is untouched -- still its own string, never the
    // collapsed three-cause 'none' text.
    expect(screen.getByText((_, el) => el?.id === 'screen-audio-hint')).toHaveTextContent('Linux');
  });

  // Mid-share the picker is a SWITCH dialog. Seeding from the persisted preference
  // silently re-enabled audio the user had turned off with the live toggle.
  it('seeds the toggle from the LIVE share state, not the saved default', async () => {
    useVideoSettingsStore.getState().setScreenStreamAudio(true);
    // CAPABLE and off: a real user opt-out, which the picker must carry over.
    useVoiceStore.setState({
      isScreenSharing: true,
      isScreenAudioOn: false,
      screenAudioVerdict: 'system-loopback',
    });
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    expect(screen.getByRole('button', { name: /^Stream Audio\b/ })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  // The other half of that rule, and the one that shipped wrong. On a WINDOW share
  // isScreenAudioOn is false because the platform forces it, not because the user chose
  // it -- reading that as an opt-out left the toggle off after switching to a whole
  // screen, and confirming then persisted the false over a default-on preference.
  it('ignores the live state when the current target could not carry audio anyway', async () => {
    useVideoSettingsStore.getState().setScreenStreamAudio(true);
    useVoiceStore.setState({
      isScreenSharing: true,
      isScreenAudioOn: false,
      screenAudioVerdict: 'none',
    });
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    expect(screen.getByRole('button', { name: /^Stream Audio\b/ })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  // Without a production writer the preference sat at its default forever and the
  // toggle reset to On every time the dialog opened.
  it('persists the audio choice so it survives the next share', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByRole('button', { name: /^Stream Audio\b/ }));
    fireEvent.click(screen.getByText('Share'));
    expect(useVideoSettingsStore.getState().screenStreamAudio).toBe(false);
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
      streamAudio: true,
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

    it('free 2560x1080 ultrawide source injects the deliverable fps as its own option (no blank control, no snap-down)', async () => {
      // Re-fixtured from a 3440x1440 display (post-fix, 1440 > the 1080 cap, so
      // 'source' resolves to 1080p/30 and this test would lose its purpose: pinning
      // the injection of a non-listed fps with 'source' retained). 2560x1080's
      // height (1080) fits the free cap exactly, so 'source' stays unclamped both
      // before and after the fix. Its pixel-rate ceiling is
      // floor(62_208_000 / (2560*1080)) = 22fps -- verified by running this test.
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 2560, height: 1080 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // 2560x1080 'source' is unclamped (height fits the cap) => 22fps, the actual
      // deliverable ceiling. 22 is not a discrete choice, so it is injected as its
      // own option and the value holds it — the shown/captured fps equals what
      // produce delivers (22), NOT a snapped-down 15 that would under-deliver (#2172).
      const fpsSelect = screen.getByTestId('screen-framerate') as HTMLSelectElement;
      await waitFor(() => expect(fpsSelect.value).toBe('22'));
      expect(fpsSelect.selectedIndex).toBeGreaterThanOrEqual(0);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: 'source', frameRate: 22 })
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

    // Inverted (regression: free users defaulted to the Premium-gated Source Native,
    // reported 2026-09-23): this pinned the OLD display-only contract, where Source
    // Native stayed selectable/selected while merely relabeled Premium and the
    // capture still silently clamped underneath it. The fix disables the option and
    // resolves the sent value to match what capture actually produces.
    it('free over-cap display marks Source Native Premium, disables it, and sends resolution:1080p', async () => {
      // A free user on a 1440p display leaving the picker at Source Native: produceScreen
      // clamps the capture to 1080p, so the option must neither promise Native nor stay
      // selectable. resetAllStores does NOT reset the subscription store, so pin the free
      // entitlement (a prior test may have left it premium/native).
      useSubscriptionStore.setState({ entitlement: FREE_ENTITLEMENT });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 2560, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      const sourceOption = () =>
        screen.getByRole('option', { name: /Source Native/ }) as HTMLOptionElement;
      await waitFor(() => expect(sourceOption().textContent).toContain('Premium'));
      // Source Native must be disabled for free over-cap.
      expect(sourceOption().disabled).toBe(true);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      // onSelect resolution: must resolve to '1080p', never the raw 'source'.
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p' })
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

  // ─── free-tier Source Native gate ────────────────────────────────────────
  // regression: free users defaulted to the Premium-gated Source Native (reported 2026-09-23)
  //
  // Oracle: for a hydrated FREE user whose largest display is taller than the stream
  // height cap (1080), the Resolution control never holds or sends 'source' -- Source
  // Native is present, labelled Premium, and DISABLED, and the selection is '1080p'.
  // Unavailable or malformed display info counts as 4K, as resolveCaptureDims does.
  // The selection is derived, never persisted, so an upgrade restores Source Native on
  // its own. The at/under-cap display, premium, degraded-premium and pre-hydrate cells
  // (6-9) are controls: Source Native stays enabled and kept.
  describe('free-tier Source Native gate', () => {
    const resolutionSelect = () => screen.getByTestId('screen-resolution') as HTMLSelectElement;
    const sourceOption = () =>
      Array.from(resolutionSelect().querySelectorAll('option')).find(
        (o) => (o as HTMLOptionElement).value === 'source'
      ) as HTMLOptionElement;

    // Cell 1: free + hydrated + 2560x1440 (over-cap), saved 'source'/30fps.
    it('free hydrated 2560x1440 saved source/30: select value, Source Native disabled+Premium, onSelect resolution+frameRate', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 2560, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // select value: must resolve to 1080p, never the raw Premium-gated 'source'.
      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));
      // Source Native must be disabled for free over-cap.
      expect(sourceOption().disabled).toBe(true);
      expect(sourceOption().textContent).toContain('Premium');

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      // onSelect resolution + frameRate.
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p', frameRate: 30 })
      );
      // Sent, not saved: the stored preference keeps 'source' so an upgrade restores it.
      expect(useVideoSettingsStore.getState().screenResolution).toBe('source');
    });

    // Cell 2: free + hydrated + 3840x2160, getDisplayInfo resolves LATE.
    it('free hydrated 3840x2160 late-resolving display info: fails open before resolve, becomes 1080p/disabled after', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      let resolveDisplayInfo: (v: { width: number; height: number }[]) => void = () => {};
      const displayInfoPromise = new Promise<{ width: number; height: number }[]>((resolve) => {
        resolveDisplayInfo = resolve;
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockReturnValue(displayInfoPromise),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      // Before resolve: displayInfo is still null -- fail OPEN. Source Native stays
      // enabled and selected.
      expect(resolutionSelect().value).toBe('source');
      expect(sourceOption().disabled).toBe(false);

      await act(async () => {
        resolveDisplayInfo([{ width: 3840, height: 2160 }]);
        await displayInfoPromise;
      });

      // After resolve: select value becomes 1080p and Source Native is disabled.
      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));
      expect(sourceOption().disabled).toBe(true);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p' })
      );
    });

    // Cell 3: free + 3840x2160 resolved, starting PRE-HYDRATE.
    it('free 3840x2160 resolved, pre-hydrate: fails open, becomes 1080p/disabled once hydrated', async () => {
      useSubscriptionStore.setState({
        hydrated: false,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      await waitFor(() => expect(resolutionSelect().value).toBe('source'));
      expect(sourceOption().disabled).toBe(false);

      act(() => {
        useSubscriptionStore.setState({ hydrated: true });
      });

      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));
      expect(sourceOption().disabled).toBe(true);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p' })
      );
    });

    // Cell 4: frame-rate pairing, free + hydrated + 2560x1440, saved 'source'/60fps.
    it('free hydrated 2560x1440 saved source/60: sends 1080p/30, then an explicit 720p sends 720p/60', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 2560, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());
      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p', frameRate: 30 })
      );

      fireEvent.change(resolutionSelect(), { target: { value: '720p' } });
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '720p', frameRate: 60 })
      );
    });

    // Cell 5: forcing the select back to 'source' still sends the resolved '1080p'.
    it('free hydrated 2560x1440: forcing the select to source anyway still sends 1080p', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 2560, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());
      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));

      // A forced selection (bypassing the disabled option) must not survive to Share.
      fireEvent.change(resolutionSelect(), { target: { value: 'source' } });
      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p' })
      );
    });

    // Cell 5b: the fps ceiling must follow the resolution actually SENT. A 3440x1440
    // 'source' clamps to 2580x1080 (22fps budget), but 1080p admits 30 -- tiering fps
    // off the raw 'source' would send 1080p at 22, under-delivering the free tier.
    it('free hydrated 3440x1440 saved source/60: fps follows the sent 1080p (30), not the ultrawide source (22)', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3440, height: 1440 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 60 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());
      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: '1080p', frameRate: 30 })
      );
    });

    // Cell 6 (control): free + hydrated + 1920x1080 (at cap).
    it('control: free hydrated at-cap 1920x1080 -- Source Native stays enabled and source is sent', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 1920, height: 1080 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      await waitFor(() => expect(resolutionSelect().value).toBe('source'));
      expect(sourceOption().disabled).toBe(false);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: 'source' })
      );
    });

    // Cell 7 (control): premium (native stream caps) + 3840x2160.
    it('control: premium native caps 3840x2160 -- Source Native stays enabled and source is sent', async () => {
      const ent = useSubscriptionStore.getState().entitlement;
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: { ...ent, streamMaxHeight: -1, streamMaxFps: -1, streamMaxPixelRate: -1 },
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      await waitFor(() => expect(resolutionSelect().value).toBe('source'));
      expect(sourceOption().disabled).toBe(false);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: 'source' })
      );
    });

    // Cell 8 (control): pre-hydrate + 3840x2160 (stays pre-hydrate).
    it('control: pre-hydrate 3840x2160 -- fails open, Source Native stays enabled, source is sent', async () => {
      useSubscriptionStore.setState({
        hydrated: false,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      await waitFor(() => expect(resolutionSelect().value).toBe('source'));
      expect(sourceOption().disabled).toBe(false);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: 'source' })
      );
    });

    // Cell 9 (control): degraded premium + 3840x2160.
    it('control: degraded premium 3840x2160 -- fails open, Source Native stays enabled, source is kept', async () => {
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
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      await waitFor(() => expect(resolutionSelect().value).toBe('source'));
      expect(sourceOption().disabled).toBe(false);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: 'source' })
      );
    });

    // A 0-sized display report is malformed, and resolveCaptureDims captures it as 4K
    // (clamped to 1080p for free). The picker must describe that, not a 0x0 display
    // that "fits" under the cap and keeps Source Native selected. A NaN size is
    // malformed the same way (resolveCaptureDims accepts only w > 0 && h > 0).
    it.each([
      {
        label: '0-sized display: resolves to 1080p, Source Native disabled',
        displays: [{ width: 0, height: 0 }],
        expected: '1080p',
        disabled: true,
      },
      {
        label: 'NaN-sized display report: resolves to 1080p, Source Native disabled',
        displays: [{ width: Number.NaN, height: Number.NaN }],
        expected: '1080p',
        disabled: true,
      },
      {
        label:
          'control: a 0-sized display beside a real 1080p one -- the real one wins, source kept',
        displays: [
          { width: 0, height: 0 },
          { width: 1920, height: 1080 },
        ],
        expected: 'source',
        disabled: false,
        // 'source' is already the value while displays load; the 1080p display's
        // 30fps tier marks 60 FPS Premium only once it has loaded.
        loaded: true,
      },
    ])('free hydrated, $label', async ({ displays, expected, disabled, loaded }) => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue(displays),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

      if (loaded) {
        const framerate = () => screen.getByTestId('screen-framerate') as HTMLSelectElement;
        await waitFor(() =>
          expect(
            Array.from(framerate().options).some(
              (o) => o.textContent === '60 FPS \u{1F512} Premium'
            )
          ).toBe(true)
        );
      }
      await waitFor(() => expect(resolutionSelect().value).toBe(expected));
      expect(sourceOption().disabled).toBe(disabled);

      fireEvent.click(screen.getByText('Entire Screen'));
      fireEvent.click(screen.getByText('Share'));
      expect(mockOnSelect).toHaveBeenCalledWith(
        'screen:0',
        expect.objectContaining({ resolution: expected })
      );
    });

    // Unavailable display info is not pending: it counts as the 4K capture fallback.
    it.each([
      ['bridge absent', () => ({})],
      ['IPC rejects', () => ({ getDisplayInfo: vi.fn().mockRejectedValue(new Error('ipc')) })],
      ['empty display list', () => ({ getDisplayInfo: vi.fn().mockResolvedValue([]) })],
    ])(
      'free hydrated, display info unavailable (%s): resolves to 1080p, Source Native disabled, sends 1080p',
      async (_label, bridge) => {
        useSubscriptionStore.setState({
          hydrated: true,
          degraded: false,
          entitlement: FREE_ENTITLEMENT,
        });
        (globalThis as Record<string, unknown>).electron = {
          getDesktopSources: vi.fn().mockResolvedValue(mockSources),
          ...bridge(),
        };
        useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
        render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
        await waitFor(() => expect(screen.getByText('Entire Screen')).toBeInTheDocument());

        await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));
        expect(sourceOption().disabled).toBe(true);

        fireEvent.click(screen.getByText('Entire Screen'));
        fireEvent.click(screen.getByText('Share'));
        expect(mockOnSelect).toHaveBeenCalledWith(
          'screen:0',
          expect.objectContaining({ resolution: '1080p' })
        );
      }
    );

    it('free -> premium after mount: Source Native is restored with no user action', async () => {
      useSubscriptionStore.setState({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      (globalThis as Record<string, unknown>).electron = {
        ...(globalThis.electron || {}),
        getDesktopSources: vi.fn().mockResolvedValue(mockSources),
        getDisplayInfo: vi.fn().mockResolvedValue([{ width: 3840, height: 2160 }]),
      };
      useVideoSettingsStore.setState({ screenResolution: 'source', screenFrameRate: 30 });
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => expect(resolutionSelect().value).toBe('1080p'));

      act(() => {
        useSubscriptionStore.setState({
          entitlement: {
            ...FREE_ENTITLEMENT,
            tier: 'premium',
            streamMaxHeight: -1,
            streamMaxFps: -1,
            streamMaxPixelRate: -1,
          },
        });
      });

      await waitFor(() => expect(resolutionSelect().value).toBe('source'));
      expect(sourceOption().disabled).toBe(false);
    });
  });

  // -- B3: dialog semantics and focus trap ------------------------------------

  describe('dialog semantics (B3)', () => {
    it('exposes the picker as an aria-modal dialog with an accessible name', async () => {
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => {
        expect(screen.getByRole('tab', { name: /^Screens/ })).toBeInTheDocument();
      });

      const dialog = screen.getByRole('dialog');
      expect(dialog).toHaveAttribute('aria-modal', 'true');
      // Named, or a screen reader announces "dialog" and nothing else.
      expect(dialog).toHaveAccessibleName();
    });

    it('pulls Tab back inside instead of letting it reach the voice bar behind', async () => {
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => {
        expect(screen.getByRole('tab', { name: /^Screens/ })).toBeInTheDocument();
      });
      const dialog = screen.getByRole('dialog');

      // Focus sitting outside the dialog is precisely the state a trap exists for,
      // and it is reachable for real: the picker never moved focus in on open.
      (document.activeElement as HTMLElement | null)?.blur();
      expect(dialog.contains(document.activeElement)).toBe(false);

      fireEvent.keyDown(document, { key: 'Tab' });
      expect(dialog.contains(document.activeElement)).toBe(true);
    });

    it('leaves Tab and Escape to a modal dialog open in front of it', async () => {
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      await waitFor(() => {
        expect(screen.getByRole('tab', { name: /^Screens/ })).toBeInTheDocument();
      });
      const { input } = openForeignModal();

      const tabNotPrevented = fireEvent.keyDown(input, { key: 'Tab' });
      expect(tabNotPrevented, 'Tab must move within the dialog in front').toBe(true);
      expect(document.activeElement, 'focus must not be pulled back to the picker').toBe(input);

      fireEvent.keyDown(input, { key: 'Escape' });
      expect(
        mockOnCancel,
        'Escape in the dialog in front must not cancel the picker'
      ).not.toHaveBeenCalled();
    });

    it('still closes on Escape', () => {
      // A guard, not a formality. A native <dialog> only closes itself on Escape
      // when opened with showModal(); this one uses the declarative `open`
      // attribute, exactly as Modal.tsx does, and a declaratively-open dialog is
      // NON-modal -- the browser runs no cancel action for it. So the listener
      // stays, and this test fails if someone deletes it believing <dialog>
      // handles Escape natively.
      render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
      fireEvent.keyDown(document, { key: 'Escape' });
      expect(mockOnCancel).toHaveBeenCalled();
    });
  });
});
