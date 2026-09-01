import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import AttestationFailedModalHost from '@/renderer/components/AttestationFailedModal';
import { useAttestationFailureStore } from '@/renderer/stores/auth/attestationFailureStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import {
  _resetClientVersionCache,
  getDesktopClientVersion,
} from '@/renderer/utils/runtime/clientVersion';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/ui/ForceUpdateOverlay.css', () => ({}));

import ForceUpdateOverlay from '@/renderer/components/ui/ForceUpdateOverlay';

describe('ForceUpdateOverlay', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    resetAllStores();
    _resetClientVersionCache();

    // Default: no update required
    useClientConfigStore.setState({
      minVersion: '',
      lastFetchedAt: null,
      configRequestRevision: 0,
      acceptedConfigRevision: 0,
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

  const acceptConfig = (requestRevision: number, minVersion: string) => {
    useClientConfigStore.getState().setConfig(
      {
        minVersion,
        featureFlags: {},
        mediaPlaneUrl: '',
        turn: { host: '', realm: '' },
        spaUrl: '',
        spaIpcContract: 0,
      },
      requestRevision
    );
  };

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

  it('renders immediately from the server version-floor failure without config or app version', () => {
    const getVersion = vi.fn(() => new Promise<string>(() => {}));
    (globalThis.electron as Record<string, unknown>).getVersion = getVersion;
    const { container } = render(
      <>
        <ForceUpdateOverlay />
        <AttestationFailedModalHost />
      </>
    );

    act(() => {
      useAttestationFailureStore.getState().showFailure({
        code: 'CLIENT_VERSION_TOO_OLD',
        requiredMinVersion: '1.2.3',
      });
    });

    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Update Now' })).toBeInTheDocument();
    expect(screen.getByText('v1.2.3')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(getVersion).toHaveBeenCalledTimes(1);
  });

  it('retains a floor when the request began before the denial', async () => {
    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      observedConfigRequestRevision: requestRevision,
    });
    acceptConfig(requestRevision, '');

    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {});

    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();
  });

  it('clears an empty floor when a request started after the denial even if version lookup never settles', async () => {
    (globalThis.electron as Record<string, unknown>).getVersion = vi.fn(
      () => new Promise<string>(() => {})
    );
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      observedConfigRequestRevision: useClientConfigStore.getState().configRequestRevision,
    });
    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    acceptConfig(requestRevision, '');

    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {});

    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('keeps a terminal floor when the admission version lookup was already cached as unavailable', async () => {
    const getVersion = vi
      .fn<() => Promise<string>>()
      .mockRejectedValueOnce(new Error('IPC unavailable'))
      .mockResolvedValueOnce('2.0.0');
    (globalThis.electron as Record<string, unknown>).getVersion = getVersion;
    await expect(getDesktopClientVersion()).resolves.toBeNull();

    const deniedRevision = useClientConfigStore.getState().configRequestRevision;
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      requiredMinVersion: '1.0.0',
      observedConfigRequestRevision: deniedRevision,
    });
    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    acceptConfig(requestRevision, '0.1.0');

    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {});

    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();
    expect(getVersion).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['equal stable floor', '0.1.0', '0.1.0', false],
    ['lower stable floor', '0.0.9', '0.1.0', false],
    ['below floor', '1.0.0', '0.1.0', true],
    ['invalid floor', '1.0.0-beta', '0.1.0', true],
    ['prerelease local version', '0.1.0', '0.1.0-beta', true],
    ['unresolved local version', '0.1.0', null, true],
  ] as const)(
    'reconciles a post-denial %s only with an exact stable version',
    async (_name, minVersion, version, retained) => {
      (globalThis.electron as Record<string, unknown>).getVersion = vi.fn(() =>
        version === null ? new Promise<string>(() => {}) : Promise.resolve(version)
      );
      useAttestationFailureStore.getState().showFailure({
        code: 'CLIENT_VERSION_TOO_OLD',
        observedConfigRequestRevision: useClientConfigStore.getState().configRequestRevision,
      });
      const requestRevision = useClientConfigStore.getState().beginConfigRequest();
      acceptConfig(requestRevision, minVersion);

      const { container } = render(<ForceUpdateOverlay />);
      await act(async () => {});

      expect(container.querySelector('.force-update-overlay') !== null).toBe(retained);
    }
  );

  it('keeps a denial blocked through reset and failed refresh until a new origin accepts an empty config', async () => {
    const deniedRevision = useClientConfigStore.getState().beginConfigRequest();
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      observedConfigRequestRevision: deniedRevision,
    });
    const unsatisfiedRevision = useClientConfigStore.getState().beginConfigRequest();
    acceptConfig(unsatisfiedRevision, '1.0.0');
    const { container } = render(<ForceUpdateOverlay />);

    await act(async () => {});
    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();

    act(() => {
      useClientConfigStore.getState().resetForRuntimeServer();
    });
    await act(async () => {});
    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();

    act(() => {
      useClientConfigStore.getState().beginConfigRequest();
    });
    await act(async () => {});
    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();

    const currentOriginRevision = useClientConfigStore.getState().beginConfigRequest();
    act(() => acceptConfig(currentOriginRevision, ''));
    await act(async () => {});
    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('does not let an older empty-config effect clear after a newer unsatisfied config arrives', async () => {
    const deniedRevision = useClientConfigStore.getState().configRequestRevision;
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      observedConfigRequestRevision: deniedRevision,
    });
    const emptyConfigRevision = useClientConfigStore.getState().beginConfigRequest();
    acceptConfig(emptyConfigRevision, '');

    const NewerConfigBeforePassiveClear: React.FC = () => {
      React.useLayoutEffect(() => {
        const requestRevision = useClientConfigStore.getState().beginConfigRequest();
        acceptConfig(requestRevision, '1.0.0');
      }, []);
      return null;
    };

    const { container } = render(
      <>
        <ForceUpdateOverlay />
        <NewerConfigBeforePassiveClear />
      </>
    );
    await act(async () => {});

    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();
  });

  it('keeps a nonempty floor blocking when version lookup rejects', async () => {
    (globalThis.electron as Record<string, unknown>).getVersion = vi
      .fn()
      .mockRejectedValue(new Error('IPC unavailable'));
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      observedConfigRequestRevision: useClientConfigStore.getState().configRequestRevision,
    });
    const requestRevision = useClientConfigStore.getState().beginConfigRequest();
    acceptConfig(requestRevision, '0.1.0');

    const { container } = render(<ForceUpdateOverlay />);
    await act(async () => {});

    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();
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

  it('falls back to the manual-download error path when the Electron bridge is absent', async () => {
    globalThis.electron = undefined as typeof globalThis.electron;
    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      requiredMinVersion: '1.0.0',
    });
    render(<ForceUpdateOverlay />);

    await act(async () => {
      fireEvent.click(screen.getByText('Update Now'));
    });

    expect(screen.getByText('Failed to check for updates.')).toBeInTheDocument();
    expect(screen.getByText(/Download manually/)).toBeInTheDocument();
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

  it('allows continuing after two updater failures for a config-only gate', async () => {
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
    expect(screen.queryByText('Continue Anyway')).not.toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByText('Retry'));
    });

    expect(screen.getByText('Continue Anyway')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Continue Anyway'));
    expect(container.querySelector('.force-update-overlay')).toBeNull();
  });

  it('re-arms a dismissed config-only gate only when the minimum version rises', async () => {
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

    act(() => acceptConfig(useClientConfigStore.getState().beginConfigRequest(), '0.9.0'));
    expect(container.querySelector('.force-update-overlay')).toBeNull();

    act(() => acceptConfig(useClientConfigStore.getState().beginConfigRequest(), '1.0.0'));
    expect(container.querySelector('.force-update-overlay')).toBeNull();

    act(() => acceptConfig(useClientConfigStore.getState().beginConfigRequest(), '2.0.0'));
    expect(container.querySelector('.force-update-overlay')).toBeInTheDocument();
  });

  it('resets stale failure state when a risen config-only floor re-arms the gate', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });
    (globalThis.electron as Record<string, unknown>).checkForUpdates = vi
      .fn()
      .mockRejectedValue(new Error('fail'));

    render(<ForceUpdateOverlay />);
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

    act(() => acceptConfig(useClientConfigStore.getState().beginConfigRequest(), '2.0.0'));
    expect(screen.queryByText('Continue Anyway')).not.toBeInTheDocument();
    expect(screen.getByText('Update Now')).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByText('Update Now'));
    });
    expect(screen.queryByText('Continue Anyway')).not.toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByText('Retry'));
    });
    expect(screen.getByText('Continue Anyway')).toBeInTheDocument();
  });

  it('makes a later client-version denial blocking after a config-only dismissal', async () => {
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

    act(() => {
      useAttestationFailureStore.getState().showFailure({
        code: 'CLIENT_VERSION_TOO_OLD',
        requiredMinVersion: '1.0.0',
      });
    });

    expect(screen.getByText('Update Required')).toBeInTheDocument();
    expect(screen.queryByText('Continue Anyway')).not.toBeInTheDocument();
  });

  it('stays blocking after repeated updater failures while Retry and manual download remain', async () => {
    useClientConfigStore.setState({ minVersion: '1.0.0', lastFetchedAt: Date.now() });

    useAttestationFailureStore.getState().showFailure({
      code: 'CLIENT_VERSION_TOO_OLD',
      requiredMinVersion: '1.0.0',
    });

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

    expect(screen.queryByText('Continue Anyway')).not.toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
    expect(screen.getByText(/Download manually/)).toBeInTheDocument();
    expect(screen.getByText('Update Required')).toBeInTheDocument();
  });
});
