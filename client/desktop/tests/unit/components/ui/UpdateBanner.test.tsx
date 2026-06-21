import { render, screen, userEvent } from '../../../test-utils';
import { vi, beforeEach, describe, it, expect } from 'vitest';

const eventCallbacks: Record<string, (...args: unknown[]) => void> = {};

const mockDownloadUpdate = vi.fn().mockResolvedValue(undefined);
const mockInstallUpdate = vi.fn();
const mockCheckForUpdates = vi.fn().mockResolvedValue(undefined);
const mockWriteClipboard = vi.fn().mockResolvedValue(undefined);

beforeEach(() => {
  localStorage.clear();
  (window as any).electron = {
    ...window.electron,
    downloadUpdate: mockDownloadUpdate,
    installUpdate: mockInstallUpdate,
    checkForUpdates: mockCheckForUpdates,
    writeClipboard: mockWriteClipboard,
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
    onUpdateRollback: vi.fn((cb) => {
      eventCallbacks.updateRollback = cb;
      return vi.fn();
    }),
  };
});

import UpdateBanner from '@/renderer/components/ui/UpdateBanner';

describe('UpdateBanner', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    for (const key of Object.keys(eventCallbacks)) delete eventCallbacks[key];
  });

  // ─── Hidden state ──────────────────────────────────────────────────

  it('renders nothing initially (hidden state)', () => {
    const { container } = render(<UpdateBanner />);
    expect(container.querySelector('.update-banner')).toBeNull();
  });

  // ─── Available state ───────────────────────────────────────────────

  it('shows banner when update is available', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Update available: v1.2.0')).toBeInTheDocument();
    });
  });

  it('shows Download button when update is available', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Download')).toBeInTheDocument();
    });
  });

  it('calls downloadUpdate when Download is clicked', async () => {
    const user = userEvent.setup();
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Download')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Download'));
    expect(mockDownloadUpdate).toHaveBeenCalled();
  });

  it('shows dismiss button when update is available', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Dismiss')).toBeInTheDocument();
    });
  });

  // ─── Downloading state ─────────────────────────────────────────────

  it('shows download progress', async () => {
    render(<UpdateBanner />);
    eventCallbacks.downloadProgress({
      percent: 42,
      transferred: 1000,
      total: 2380,
      bytesPerSecond: 500,
    });
    await vi.waitFor(() => {
      expect(screen.getByText(/Downloading update/)).toBeInTheDocument();
      expect(screen.getByText(/42%/)).toBeInTheDocument();
    });
  });

  it('renders progress bar during download', async () => {
    const { container } = render(<UpdateBanner />);
    eventCallbacks.downloadProgress({
      percent: 50,
      transferred: 500,
      total: 1000,
      bytesPerSecond: 100,
    });
    await vi.waitFor(() => {
      const bar = container.querySelector('.update-banner__progress');
      expect(bar).not.toBeNull();
      expect((bar as HTMLElement).style.width).toBe('50%');
    });
  });

  // ─── Downloaded state ──────────────────────────────────────────────

  it('shows install prompt when update downloaded', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateDownloaded({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('v1.2.0 ready to install')).toBeInTheDocument();
      expect(screen.getByText('Restart Now')).toBeInTheDocument();
    });
  });

  it('calls installUpdate on Restart Now click', async () => {
    const user = userEvent.setup();
    render(<UpdateBanner />);
    eventCallbacks.updateDownloaded({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Restart Now')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Restart Now'));
    expect(mockInstallUpdate).toHaveBeenCalled();
  });

  // ─── Rollback state (#384) ─────────────────────────────────────────

  it('shows rollback banner with message', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Update to v1.2.0 failed. You are still on v1.0.0.',
    });
    await vi.waitFor(() => {
      expect(
        screen.getByText('Update to v1.2.0 failed. You are still on v1.0.0.')
      ).toBeInTheDocument();
    });
  });

  it('shows Retry button on rollback', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Update failed.',
    });
    await vi.waitFor(() => {
      expect(screen.getByText('Retry')).toBeInTheDocument();
    });
  });

  it('does not show dismiss button on rollback', async () => {
    render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Update failed.',
    });
    await vi.waitFor(() => {
      expect(screen.getByText('Retry')).toBeInTheDocument();
    });
    expect(screen.queryByLabelText('Dismiss')).toBeNull();
  });

  it('Retry hides banner and calls checkForUpdates', async () => {
    const user = userEvent.setup();
    const { container } = render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Update failed.',
    });
    await vi.waitFor(() => {
      expect(screen.getByText('Retry')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Retry'));
    expect(mockCheckForUpdates).toHaveBeenCalled();
    // Banner should be hidden
    expect(container.querySelector('.update-banner')).toBeNull();
  });

  it('Retry restores rollback state when checkForUpdates fails', async () => {
    mockCheckForUpdates.mockRejectedValueOnce(new Error('Network error'));
    const user = userEvent.setup();
    render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Original rollback message.',
    });
    await vi.waitFor(() => {
      expect(screen.getByText('Retry')).toBeInTheDocument();
    });
    await user.click(screen.getByText('Retry'));
    await vi.waitFor(() => {
      // Should restore rollback state with error message
      expect(
        screen.getByText('Failed to check for updates. Please try again later.')
      ).toBeInTheDocument();
    });
  });

  it('rollback banner has rollback CSS class', async () => {
    const { container } = render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Update failed.',
    });
    await vi.waitFor(() => {
      expect(container.querySelector('.update-banner--rollback')).not.toBeNull();
    });
  });

  // ─── Error state ───────────────────────────────────────────────────

  it('resets to available on error during download', async () => {
    render(<UpdateBanner />);
    // First trigger available, then downloading, then error
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Update available: v1.2.0')).toBeInTheDocument();
    });
    eventCallbacks.downloadProgress({
      percent: 10,
      transferred: 100,
      total: 1000,
      bytesPerSecond: 50,
    });
    await vi.waitFor(() => {
      expect(screen.getByText(/Downloading/)).toBeInTheDocument();
    });
    eventCallbacks.updateError({ message: 'Network error' });
    await vi.waitFor(() => {
      // Should reset back to available
      expect(screen.getByText('Update available: v1.2.0')).toBeInTheDocument();
    });
  });

  // ─── Dismiss behavior ─────────────────────────────────────────────

  it('hides banner when dismissed', async () => {
    const user = userEvent.setup();
    const { container } = render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Dismiss')).toBeInTheDocument();
    });
    await user.click(screen.getByLabelText('Dismiss'));
    expect(container.querySelector('.update-banner')).toBeNull();
  });

  it('persists dismissed version to localStorage', async () => {
    const user = userEvent.setup();
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Dismiss')).toBeInTheDocument();
    });
    await user.click(screen.getByLabelText('Dismiss'));
    const stored = JSON.parse(localStorage.getItem('concord:update-banner-dismissed')!);
    expect(stored.version).toBe('1.2.0');
    expect(stored.at).toBeGreaterThan(0);
  });

  it('stays hidden for dismissed version on re-render', async () => {
    // Pre-dismiss the version
    localStorage.setItem(
      'concord:update-banner-dismissed',
      JSON.stringify({ version: '1.2.0', at: Date.now() })
    );
    const { container } = render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      // Should remain hidden because dismissed
      expect(container.querySelector('.update-banner')).toBeNull();
    });
  });

  it('re-shows banner after dismiss TTL expires', async () => {
    // Set dismissed with expired TTL
    localStorage.setItem(
      'concord:update-banner-dismissed',
      JSON.stringify({ version: '1.2.0', at: Date.now() - 8 * 24 * 60 * 60 * 1000 })
    );
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.0' });
    await vi.waitFor(() => {
      expect(screen.getByText('Update available: v1.2.0')).toBeInTheDocument();
    });
  });

  it('rollback banner is NOT dismissable even with dismissed version', async () => {
    localStorage.setItem(
      'concord:update-banner-dismissed',
      JSON.stringify({ version: '1.2.0', at: Date.now() })
    );
    render(<UpdateBanner />);
    eventCallbacks.updateRollback({
      fromVersion: '1.0.0',
      toVersion: '1.2.0',
      message: 'Update failed.',
    });
    await vi.waitFor(() => {
      // Rollback should still show despite version being dismissed
      expect(screen.getByText('Update failed.')).toBeInTheDocument();
    });
  });

  // ─── Not-available event ───────────────────────────────────────────

  it('stays hidden on update-not-available', () => {
    const { container } = render(<UpdateBanner />);
    eventCallbacks.updateNotAvailable();
    expect(container.querySelector('.update-banner')).toBeNull();
  });

  // ─── Corrupted localStorage ────────────────────────────────────────

  it('handles corrupted dismissed JSON gracefully', () => {
    localStorage.setItem('concord:update-banner-dismissed', '{bad json!!');
    const { container } = render(<UpdateBanner />);
    // Should not crash
    expect(container).toBeTruthy();
    // Corrupted value should be removed
    expect(localStorage.getItem('concord:update-banner-dismissed')).toBeNull();
  });

  // ─── Version validation boundary (defense-in-depth) ────────────────────
  // The auto-updater IPC is an untrusted source per SonarQube taint analysis.
  // The validator gates both writes to localStorage and reads back from it.

  it('persists a valid semver on dismiss (happy path)', async () => {
    const user = userEvent.setup();
    render(<UpdateBanner />);
    eventCallbacks.updateAvailable({ version: '1.2.3-rc.1+build.7' });
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Dismiss')).toBeInTheDocument();
    });
    await user.click(screen.getByLabelText('Dismiss'));
    const stored = JSON.parse(localStorage.getItem('concord:update-banner-dismissed')!);
    expect(stored.version).toBe('1.2.3-rc.1+build.7');
  });

  it('refuses to persist a non-semver version (rejects malformed input)', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const user = userEvent.setup();
      const { container } = render(<UpdateBanner />);
      eventCallbacks.updateAvailable({ version: 'not-a-version' });
      await vi.waitFor(() => {
        expect(screen.getByLabelText('Dismiss')).toBeInTheDocument();
      });
      await user.click(screen.getByLabelText('Dismiss'));
      // Validator rejected the malformed version — no localStorage write
      expect(localStorage.getItem('concord:update-banner-dismissed')).toBeNull();
      // …but the validator-rejection branch DID run (proves it wasn't an upstream throw)
      expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('dismiss skipped persistence'));
      // …and the banner is hidden for this session so the dismiss click isn't a silent no-op
      expect(container.querySelector('.update-banner')).toBeNull();
    } finally {
      warnSpy.mockRestore();
    }
  });

  it('refuses to persist an oversized version string', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const user = userEvent.setup();
      const { container } = render(<UpdateBanner />);
      // Construct a "semver-shaped" but length-bombed version (over 64 chars)
      const oversized = `1.0.0-${'A'.repeat(80)}`;
      eventCallbacks.updateAvailable({ version: oversized });
      await vi.waitFor(() => {
        expect(screen.getByLabelText('Dismiss')).toBeInTheDocument();
      });
      await user.click(screen.getByLabelText('Dismiss'));
      expect(localStorage.getItem('concord:update-banner-dismissed')).toBeNull();
      expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('dismiss skipped persistence'));
      expect(container.querySelector('.update-banner')).toBeNull();
    } finally {
      warnSpy.mockRestore();
    }
  });

  it('discards malformed persisted version on read', () => {
    // Pre-seed localStorage with a shape-valid JSON but invalid version
    localStorage.setItem(
      'concord:update-banner-dismissed',
      JSON.stringify({ version: 'malicious"; alert(1); //', at: Date.now() })
    );
    render(<UpdateBanner />);
    // Validator rejects the persisted value and clears it on read
    expect(localStorage.getItem('concord:update-banner-dismissed')).toBeNull();
  });
});
