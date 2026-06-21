import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';

vi.mock('@/renderer/components/ui/ForceUpdateOverlay.css', () => ({}));

import ForceUpdateOverlay from '@/renderer/components/ui/ForceUpdateOverlay';

describe('ForceUpdateOverlay', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();

    // Default: no update required
    useClientConfigStore.setState({
      minVersion: '',
      lastFetchedAt: null,
    });

    // Mock electron API (use assignment — setup.ts defines it writable but not configurable)
    globalThis.electron = {
      ...globalThis.electron,
      getVersion: vi.fn().mockResolvedValue('0.1.0'),
      checkForUpdates: vi.fn().mockResolvedValue(undefined),
      downloadUpdate: vi.fn(),
      installUpdate: vi.fn(),
      onUpdateAvailable: vi.fn().mockReturnValue(() => {}),
      onUpdateNotAvailable: vi.fn().mockReturnValue(() => {}),
      onUpdateDownloadProgress: vi.fn().mockReturnValue(() => {}),
      onUpdateDownloaded: vi.fn().mockReturnValue(() => {}),
      onUpdateError: vi.fn().mockReturnValue(() => {}),
    } as typeof globalThis.electron;
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders nothing when no update is required', async () => {
    useClientConfigStore.setState({ minVersion: '0.1.0', lastFetchedAt: Date.now() });
    const { container } = render(<ForceUpdateOverlay />);

    // Wait for ready grace period
    act(() => {
      vi.advanceTimersByTime(1100);
    });
    // Flush async getVersion
    await act(async () => {
      await vi.runAllTimersAsync();
    });

    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('renders nothing during grace period even if update needed', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });
    const { container } = render(<ForceUpdateOverlay />);

    // Before grace period
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('renders update overlay when app version is below minVersion', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });
    render(<ForceUpdateOverlay />);

    // Wait for ready + version fetch
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });

    expect(screen.getByText('Update Required')).toBeInTheDocument();
    expect(screen.getByText(/v0\.1\.0/)).toBeInTheDocument();
    expect(screen.getByText(/v1\.0\.0/)).toBeInTheDocument();
  });

  it('shows Update Now button initially', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });
    render(<ForceUpdateOverlay />);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });

    expect(screen.getByText('Update Now')).toBeInTheDocument();
  });

  it('shows checking state when Update Now is clicked', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });
    render(<ForceUpdateOverlay />);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Update Now'));
    });

    // The phase should be 'checking' after click
    expect(screen.queryByText('Update Now')).not.toBeInTheDocument();
  });

  it('renders nothing when minVersion is empty', async () => {
    useClientConfigStore.setState({ minVersion: '', lastFetchedAt: Date.now() });
    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('renders nothing when lastFetchedAt is null', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: null });
    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('handles semver comparison correctly with prerelease tags', async () => {
    // 0.1.0-beta.1 stripped to 0.1.0, which is less than 1.0.0
    (globalThis.electron as Record<string, unknown>).getVersion = vi
      .fn()
      .mockResolvedValue('0.1.0-beta.1');
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });
    render(<ForceUpdateOverlay />);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });
    expect(screen.getByText('Update Required')).toBeInTheDocument();
  });

  it('shows manual download link on error', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });

    // Make checkForUpdates throw
    (globalThis.electron as Record<string, unknown>).checkForUpdates = vi
      .fn()
      .mockRejectedValue(new Error('Network error'));

    render(<ForceUpdateOverlay />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Update Now'));
    });

    expect(screen.getByText(/Download manually/)).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();

    // Regression marker for #800: the global a[target='_blank'] CSS rule in
    // index.css depends on this attribute being present. If a future refactor
    // drops it, the manual-download link goes back to the invisible
    // UA-default blue against the dark overlay backdrop.
    const link = screen.getByText(/Download manually/).closest('a');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('shows Continue Anyway after 2 failures', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });

    (globalThis.electron as Record<string, unknown>).checkForUpdates = vi
      .fn()
      .mockRejectedValue(new Error('fail'));

    render(<ForceUpdateOverlay />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });

    // First failure
    await act(async () => {
      fireEvent.click(screen.getByText('Update Now'));
    });
    // Second failure
    await act(async () => {
      fireEvent.click(screen.getByText('Retry'));
    });

    expect(screen.getByText('Continue Anyway')).toBeInTheDocument();
  });

  it('dismisses overlay when Continue Anyway is clicked', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });

    (globalThis.electron as Record<string, unknown>).checkForUpdates = vi
      .fn()
      .mockRejectedValue(new Error('fail'));

    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100);
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Update Now'));
    });
    await act(async () => {
      fireEvent.click(screen.getByText('Retry'));
    });
    fireEvent.click(screen.getByText('Continue Anyway'));

    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });
});
