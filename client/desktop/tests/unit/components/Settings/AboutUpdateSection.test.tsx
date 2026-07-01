import { render, screen, fireEvent, userEvent, within } from '../../../test-utils';
import { vi } from 'vitest';

vi.mock('@/renderer/config', () => ({
  SPA_VERSION: 'a'.repeat(40),
}));

const eventCallbacks: Record<string, (...args: unknown[]) => void> = {};

const mockCheckForUpdates = vi.fn().mockResolvedValue(undefined);
const mockDownloadUpdate = vi.fn().mockResolvedValue(undefined);
const mockInstallUpdate = vi.fn();
const mockSetAllowPrerelease = vi.fn();
const mockGetDeveloperMode = vi.fn().mockResolvedValue(false);
const mockSetDeveloperMode = vi.fn().mockResolvedValue(undefined);

const mockWriteClipboard = vi.fn().mockResolvedValue(undefined);
const mockGetUpdateLogPath = vi
  .fn()
  .mockResolvedValue('/Users/test/Library/Logs/ConcordVoice/update-2026-03-25.log');

// SPA (UI) update axis mocks — default: on remote, up to date.
const mockSpaCheckForUpdate = vi.fn().mockResolvedValue({
  currentMode: 'remote',
  remoteAvailable: true,
  newerBytesAvailable: false,
  reason: 'remote SPA compatible',
});
const mockSpaReloadLatest = vi.fn().mockResolvedValue({ mode: 'remote', changed: true });

beforeEach(() => {
  // window.electron is already defined as writable in setup.ts — just assign
  (window as any).electron = {
    ...window.electron,
    getVersion: vi.fn().mockResolvedValue('0.2.0-beta.1'),
    getPlatform: vi.fn().mockResolvedValue('darwin'),
    getSystemInfo: vi.fn().mockResolvedValue({
      platform: 'darwin',
      arch: 'arm64',
      electronVersion: '33.0.0',
      chromiumVersion: '128.0.0',
      nodeVersion: '20.0.0',
    }),
    getAllowPrerelease: vi.fn().mockResolvedValue(true),
    checkForUpdates: mockCheckForUpdates,
    downloadUpdate: mockDownloadUpdate,
    installUpdate: mockInstallUpdate,
    setAllowPrerelease: mockSetAllowPrerelease,
    getDeveloperMode: mockGetDeveloperMode,
    setDeveloperMode: mockSetDeveloperMode,
    writeClipboard: mockWriteClipboard,
    getUpdateLogPath: mockGetUpdateLogPath,
    spaUpdate: {
      checkForUpdate: mockSpaCheckForUpdate,
      reloadLatest: mockSpaReloadLatest,
    },
    onUpdateAvailable: vi.fn((cb) => {
      eventCallbacks.updateAvailable = cb;
      return vi.fn();
    }),
    onUpdateNotAvailable: vi.fn((cb) => {
      eventCallbacks.updateNotAvailable = cb;
      return vi.fn();
    }),
    onUpdateDownloadProgress: vi.fn((cb) => {
      eventCallbacks.downloadProgress = cb;
      return vi.fn();
    }),
    onUpdateDownloaded: vi.fn((cb) => {
      eventCallbacks.updateDownloaded = cb;
      return vi.fn();
    }),
    onUpdateError: vi.fn((cb) => {
      eventCallbacks.updateError = cb;
      return vi.fn();
    }),
  };
});

import AboutUpdateSection from '@/renderer/components/Settings/AboutUpdateSection';

describe('AboutUpdateSection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    for (const key of Object.keys(eventCallbacks)) delete eventCallbacks[key];
  });

  it('renders Client Info section title', () => {
    render(<AboutUpdateSection />);
    expect(screen.getByText('Client Info')).toBeInTheDocument();
  });
  it('renders App Version label', () => {
    render(<AboutUpdateSection />);
    expect(screen.getByText('App Version')).toBeInTheDocument();
  });
  it('renders legal document entries for all canonical source documents', () => {
    render(<AboutUpdateSection />);
    expect(screen.getAllByText('LICENSE').length).toBeGreaterThan(0);
    expect(screen.getByText('Privacy Policy')).toBeInTheDocument();
    expect(screen.getByText('Terms of Service')).toBeInTheDocument();
    expect(screen.getAllByText('NOTICE.md').length).toBeGreaterThan(0);
  });

  it('opens Privacy Policy content with the source draft marker preserved', () => {
    render(<AboutUpdateSection />);
    const summary = screen.getByText('Privacy Policy');
    const details = summary.closest('details') as HTMLDetailsElement;
    fireEvent.click(summary);
    expect(details.open).toBe(true);
    expect(details.querySelector('.about-legal-content')?.textContent).toContain(
      'TO BE SET ON PUBLICATION — currently 2026-05-30 in draft'
    );
  });

  it('opens Terms of Service content with the source draft marker preserved', () => {
    render(<AboutUpdateSection />);
    const summary = screen.getByText('Terms of Service');
    const details = summary.closest('details') as HTMLDetailsElement;
    fireEvent.click(summary);
    expect(details.open).toBe(true);
    expect(details.querySelector('.about-legal-content')?.textContent).toContain(
      'Last Updated:** _[TO BE SET ON PUBLICATION]_'
    );
  });

  it('shortens full SPA build hashes in Client Info', () => {
    render(<AboutUpdateSection />);
    expect(screen.getByText('aaaaaaa')).toBeInTheDocument();
    expect(screen.queryByText('a'.repeat(40))).toBeNull();
  });

  it('renders app version after loading', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('v0.2.0-beta.1')).toBeInTheDocument();
    });
  });

  it('renders system info after loading', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Platform')).toBeInTheDocument();
      expect(screen.getByText('macOS (ARM64)')).toBeInTheDocument();
    });
  });

  it('renders Electron version', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('v33.0.0')).toBeInTheDocument();
    });
  });

  it('renders Chromium version', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('v128.0.0')).toBeInTheDocument();
    });
  });

  it('renders Node.js version', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('v20.0.0')).toBeInTheDocument();
    });
  });

  it('formats win32 platform as Windows', async () => {
    (window as any).electron.getSystemInfo = vi.fn().mockResolvedValue({
      platform: 'win32',
      arch: 'x64',
      electronVersion: '33.0.0',
      chromiumVersion: '128.0.0',
      nodeVersion: '20.0.0',
    });
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Windows (x86_64)')).toBeInTheDocument();
    });
  });

  it('formats linux platform as Linux', async () => {
    (window as any).electron.getSystemInfo = vi.fn().mockResolvedValue({
      platform: 'linux',
      arch: 'x64',
      electronVersion: '33.0.0',
      chromiumVersion: '128.0.0',
      nodeVersion: '20.0.0',
    });
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Linux (x86_64)')).toBeInTheDocument();
    });
  });

  it('renders Update Settings section title', () => {
    render(<AboutUpdateSection />);
    expect(screen.getByText('Update Settings')).toBeInTheDocument();
  });
  it('Update Settings section is collapsed by default (#4)', () => {
    render(<AboutUpdateSection />);
    const details = screen.getByText('Update Settings').closest('details');
    expect(details?.hasAttribute('open')).toBe(false);
  });
  it('renders pre-release toggle', () => {
    render(<AboutUpdateSection />);
    expect(screen.getByText('Allow Pre-release Updates')).toBeInTheDocument();
  });
  it('renders Check for Updates button', () => {
    render(<AboutUpdateSection />);
    expect(screen.getByText('Check for Updates')).toBeInTheDocument();
  });

  it('does not render the hidden Client Info interface update row', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('✓ Up to date')).toBeInTheDocument();
    });
    const clientInfo = screen.getByText('Client Info').closest('details') as HTMLElement;
    expect(within(clientInfo).queryByText('Interface')).toBeNull();
    expect(screen.queryByRole('button', { name: /Load latest UI/ })).toBeNull();
  });

  it('shows pre-release enabled description when on', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(
        screen.getByText(/Update checks will pull from the pre-release branch/)
      ).toBeInTheDocument();
    });
  });

  it('calls setAllowPrerelease when toggle is clicked', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(
        screen.getByText(/Update checks will pull from the pre-release branch/)
      ).toBeInTheDocument();
    });
    const checkbox = screen
      .getByText('Allow Pre-release Updates')
      .closest('.about-setting-row')
      ?.querySelector('input[type="checkbox"]');
    fireEvent.click(checkbox!);
    expect(mockSetAllowPrerelease).toHaveBeenCalledWith(false);
  });

  it('calls checkForUpdates when button is clicked', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    await user.click(screen.getByText('Check for Updates'));
    expect(mockCheckForUpdates).toHaveBeenCalled();
  });

  it('checks desktop and interface updates from the unified button', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    await user.click(screen.getByText('Check for Updates'));
    expect(mockCheckForUpdates).toHaveBeenCalledTimes(1);
    expect(mockSpaCheckForUpdate).toHaveBeenCalledTimes(2);
  });

  it('shows Checking text while checking for updates', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    await user.click(screen.getByText('Check for Updates'));
    expect(screen.getByText('Checking...')).toBeInTheDocument();
  });

  it('disables button while checking', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    await user.click(screen.getByText('Check for Updates'));
    expect(screen.getByText('Checking...')).toBeDisabled();
  });

  it('shows update available message from event', async () => {
    render(<AboutUpdateSection />);
    eventCallbacks.updateAvailable({ version: '0.3.0-beta.1' });
    await vi.waitFor(() => {
      expect(screen.getByText('Update available: v0.3.0-beta.1')).toBeInTheDocument();
    });
  });

  it('shows Download button when update is available', async () => {
    render(<AboutUpdateSection />);
    eventCallbacks.updateAvailable({ version: '0.3.0-beta.1' });
    await vi.waitFor(() => {
      expect(screen.getByText('Download')).toBeInTheDocument();
    });
  });

  it('calls downloadUpdate when Download is clicked', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    eventCallbacks.updateAvailable({ version: '0.3.0-beta.1' });
    await vi.waitFor(() => {
      expect(screen.getByText('Download')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Download'));
    expect(mockDownloadUpdate).toHaveBeenCalled();
  });

  it('shows up-to-date message from event', async () => {
    render(<AboutUpdateSection />);
    eventCallbacks.updateNotAvailable();
    await vi.waitFor(() => {
      expect(screen.getByText(/up to date/)).toBeInTheDocument();
    });
  });

  it('shows install prompt when update is downloaded', async () => {
    render(<AboutUpdateSection />);
    eventCallbacks.updateDownloaded({ version: '0.3.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('v0.3.0 ready to install')).toBeInTheDocument();
      expect(screen.getByText('Restart Now')).toBeInTheDocument();
    });
  });

  it('opens Update Settings from the App Version update indicator', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    eventCallbacks.updateDownloaded({ version: '0.3.0' });
    const updateSettings = screen
      .getByText('Update Settings')
      .closest('details') as HTMLDetailsElement;

    expect(updateSettings.open).toBe(false);
    await user.click(await screen.findByRole('button', { name: /App update ready/i }));

    expect(updateSettings.open).toBe(true);
    expect(screen.getByText('Restart Now')).toBeInTheDocument();
  });

  it('calls installUpdate when Restart Now is clicked', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    eventCallbacks.updateDownloaded({ version: '0.3.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Restart Now')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Restart Now'));
    expect(mockInstallUpdate).toHaveBeenCalled();
  });

  it('shows error message from update error event', async () => {
    render(<AboutUpdateSection />);
    eventCallbacks.updateError({ message: 'Network connection failed' });
    await vi.waitFor(() => {
      expect(screen.getByText('Update error: Network connection failed')).toBeInTheDocument();
    });
  });

  it('shows error when checkForUpdates throws', async () => {
    mockCheckForUpdates.mockRejectedValueOnce(new Error('Check failed'));
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    await user.click(screen.getByText('Check for Updates'));
    await vi.waitFor(() => {
      expect(screen.getByText('Update error: Failed to check for updates.')).toBeInTheDocument();
    });
  });

  it('shows last checked timestamp after receiving update event', async () => {
    render(<AboutUpdateSection />);
    eventCallbacks.updateNotAvailable();
    await vi.waitFor(() => {
      expect(screen.getByText(/Last checked:/)).toBeInTheDocument();
    });
  });

  // ─── Update Log path (#383) ──────────────────────────────────────

  it('renders Update Log label after loading', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Update Log')).toBeInTheDocument();
    });
  });

  it('renders full log path', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(
        screen.getByText('/Users/test/Library/Logs/ConcordVoice/update-2026-03-25.log')
      ).toBeInTheDocument();
    });
  });

  it('renders Copy Path button', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Copy Path')).toBeInTheDocument();
    });
  });

  it('copies log path to clipboard and shows Copied', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Copy Path')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Copy Path'));
    expect(mockWriteClipboard).toHaveBeenCalledWith(
      '/Users/test/Library/Logs/ConcordVoice/update-2026-03-25.log'
    );
    expect(screen.getByText('Copied')).toBeInTheDocument();
  });

  // --- Developer Mode (TEMPORARY — remove before BETA) ---
  it('renders Developer Mode toggle and reflects current pref', async () => {
    mockGetDeveloperMode.mockResolvedValueOnce(true);
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      const toggle = screen.getByRole('checkbox', { name: /Developer Mode/i });
      expect((toggle as HTMLInputElement).checked).toBe(true);
    });
  });

  it('toggles Developer Mode and calls setDeveloperMode', async () => {
    const user = userEvent.setup();
    render(<AboutUpdateSection />);
    const toggle = await screen.findByRole('checkbox', { name: /Developer Mode/i });
    await user.click(toggle);
    expect(mockSetDeveloperMode).toHaveBeenCalledWith(true);
  });

  it('does not render log path row when getUpdateLogPath returns null', async () => {
    mockGetUpdateLogPath.mockResolvedValueOnce(null);
    render(<AboutUpdateSection />);
    // Wait for other content to load, then check log path is absent
    await vi.waitFor(() => {
      expect(screen.getByText('App Version')).toBeInTheDocument();
    });
    expect(screen.queryByText('Update Log')).toBeNull();
  });

  // ── SPA (UI) update axis ────────────────────────────────────────────────
  it('shows "up to date" for a remote SPA with no newer bytes (default)', async () => {
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('✓ Up to date')).toBeInTheDocument();
    });
    expect(screen.queryByRole('button', { name: /Load latest UI/ })).toBeNull();
  });

  it('shows the offline-fallback notice + a Load latest UI button when on bundled', async () => {
    mockSpaCheckForUpdate.mockResolvedValueOnce({
      currentMode: 'bundled',
      remoteAvailable: true,
      newerBytesAvailable: null,
      reason: 'config fetch failed: timeout after 5000ms',
    });
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Offline fallback UI')).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: /Load latest UI/ })).toBeInTheDocument();
  });

  it('opens Update Settings from the SPA Build update indicator', async () => {
    const user = userEvent.setup();
    mockSpaCheckForUpdate.mockResolvedValueOnce({
      currentMode: 'remote',
      remoteAvailable: true,
      newerBytesAvailable: true,
      reason: 'remote SPA compatible',
    });
    render(<AboutUpdateSection />);
    const updateSettings = screen
      .getByText('Update Settings')
      .closest('details') as HTMLDetailsElement;

    expect(updateSettings.open).toBe(false);
    await user.click(await screen.findByRole('button', { name: /Interface update available/i }));

    expect(updateSettings.open).toBe(true);
    expect(screen.getByRole('button', { name: /Load latest UI/ })).toBeInTheDocument();
  });

  it('clicking "Load latest UI" calls spaUpdate.reloadLatest', async () => {
    mockSpaCheckForUpdate.mockResolvedValueOnce({
      currentMode: 'bundled',
      remoteAvailable: true,
      newerBytesAvailable: null,
      reason: 'config fetch failed: timeout after 5000ms',
    });
    render(<AboutUpdateSection />);
    const btn = await screen.findByRole('button', { name: /Load latest UI/ });
    fireEvent.click(btn);
    await vi.waitFor(() => {
      expect(mockSpaReloadLatest).toHaveBeenCalledTimes(1);
    });
  });

  it('shows "newer UI available" + button for a remote SPA with newer bytes', async () => {
    mockSpaCheckForUpdate.mockResolvedValueOnce({
      currentMode: 'remote',
      remoteAvailable: true,
      newerBytesAvailable: true,
      reason: 'remote SPA compatible',
    });
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText('Newer UI available')).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: /Load latest UI/ })).toBeInTheDocument();
  });

  it('degrades to an actionable error state when the SPA check fails', async () => {
    mockSpaCheckForUpdate.mockRejectedValueOnce(new Error('ipc failed'));
    render(<AboutUpdateSection />);
    await vi.waitFor(() => {
      expect(screen.getByText("Couldn't check")).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: /Load latest UI/ })).toBeInTheDocument();
  });
});
