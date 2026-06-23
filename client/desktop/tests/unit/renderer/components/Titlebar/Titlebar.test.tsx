import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import { Titlebar } from '@/renderer/components/Titlebar/Titlebar';

const mockGet = vi.fn();
const mockOnChange = vi.fn();

beforeEach(() => {
  vi.clearAllMocks();
  (window as unknown as { electron: unknown }).electron = {
    version: {
      get: mockGet,
      onChange: mockOnChange,
    },
  };
});

describe('Titlebar', () => {
  it('renders the centered "Concord Voice" brand text', () => {
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: 'abc123' });
    mockOnChange.mockReturnValue(() => {});
    render(<Titlebar />);
    expect(screen.getByText('Concord Voice')).toBeInTheDocument();
  });

  it('fetches and displays the version + SPA hash', async () => {
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: 'abc123' });
    mockOnChange.mockReturnValue(() => {});
    render(<Titlebar />);
    await waitFor(() => {
      expect(screen.getByText(/v0\.1\.40-abc123/)).toBeInTheDocument();
    });
  });

  it('displays just the version (no hash) when SPA is in bundled mode', async () => {
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: null });
    mockOnChange.mockReturnValue(() => {});
    render(<Titlebar />);
    await waitFor(() => {
      expect(screen.getByText('v0.1.40')).toBeInTheDocument();
    });
  });

  it('subscribes to version changes and updates on receipt', async () => {
    let changeListener: ((data: { spaHash: string | null }) => void) | undefined;
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: 'abc123' });
    mockOnChange.mockImplementation((cb) => {
      changeListener = cb;
      return () => {};
    });
    render(<Titlebar />);
    await waitFor(() => {
      expect(screen.getByText(/v0\.1\.40-abc123/)).toBeInTheDocument();
    });
    changeListener!({ spaHash: 'def456' });
    await waitFor(() => {
      expect(screen.getByText(/v0\.1\.40-def456/)).toBeInTheDocument();
    });
  });

  it('marks the titlebar as a drag region via CSS class', () => {
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: null });
    mockOnChange.mockReturnValue(() => {});
    const { container } = render(<Titlebar />);
    expect(container.querySelector('.titlebar')).not.toBeNull();
  });

  it('renders brand and version inside a centered titlebar group', async () => {
    const spy = vi.spyOn(window.navigator, 'platform', 'get');
    spy.mockReturnValue('Win32');
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: 'abc123' });
    mockOnChange.mockReturnValue(() => {});
    const { container } = render(<Titlebar />);
    const center = container.querySelector('.titlebar-center');
    expect(center).not.toBeNull();
    expect(center?.querySelector('.titlebar-title')?.textContent).toBe('Concord Voice');
    await waitFor(() => {
      expect(center?.querySelector('.titlebar-version')?.textContent).toBe('v0.1.40-abc123');
    });
    spy.mockRestore();
  });

  // Coverage additions during SonarQube reconcile — exercise the catch branch
  // and the defensive early-return when the electron bridge is absent.
  it('logs error to console.error when version.get() rejects', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockGet.mockRejectedValue(new Error('IPC bridge unavailable'));
    mockOnChange.mockReturnValue(() => {});
    render(<Titlebar />);
    await waitFor(() => {
      expect(errorSpy).toHaveBeenCalledWith(
        '[Titlebar] failed to fetch version:',
        'IPC bridge unavailable'
      );
    });
    errorSpy.mockRestore();
  });

  it('renders just the brand text when window.electron.version is undefined (partial bridge)', () => {
    // Partial bridge: electron exists but no version namespace (e.g. brief
    // mount-order race in dev HMR). The defensive `?.version` early-return
    // in Titlebar's useEffect should skip the fetch + subscription.
    (window as unknown as { electron: unknown }).electron = {};
    const { container } = render(<Titlebar />);
    expect(screen.getByText('Concord Voice')).toBeInTheDocument();
    // No version label rendered because formatVersionText(null) returns ''
    expect(container.querySelector('.titlebar-version')).toBeNull();
  });

  it('does not synthesize an empty appVersion when onChange fires before get() resolves', async () => {
    let resolveGet: ((v: { appVersion: string; spaHash: string | null }) => void) | undefined;
    let changeListener: ((data: { spaHash: string | null }) => void) | undefined;
    mockGet.mockImplementation(
      () =>
        new Promise<{ appVersion: string; spaHash: string | null }>((r) => {
          resolveGet = r;
        })
    );
    mockOnChange.mockImplementation((cb) => {
      changeListener = cb;
      return () => {};
    });

    const { container } = render(<Titlebar />);

    // Fire onChange BEFORE get() resolves. Pre-fix this synthesized
    // { appVersion: '', spaHash: 'abc123' } and rendered 'v-abc123'.
    // Wrap in act() to force the React state update to flush synchronously
    // so the assertion below actually observes the (broken) rendered state.
    // Without act(), waitFor's first poll catches the unflushed DOM and
    // passes vacuously even against the buggy synthesis branch — the test
    // would then fail to detect the bug it was written to catch.
    await act(async () => {
      changeListener!({ spaHash: 'abc123' });
    });

    // The version span should NOT exist yet (appVersion is still null).
    // If the race-synthesis bug exists, this span would contain 'v-abc123'.
    expect(container.querySelector('.titlebar-version')).toBeNull();

    // Now resolve get(); the version span should appear with the correct text.
    resolveGet!({ appVersion: '0.1.43', spaHash: 'abc123' });
    await waitFor(() => {
      expect(screen.getByText('v0.1.43-abc123')).toBeInTheDocument();
    });
  });

  it('renders no version span when appVersion is empty string (degenerate upstream case)', async () => {
    // Defense-in-depth against the Gitar #1153 finding: if `versionApi.get()`
    // ever resolves with `appVersion: ''` (e.g., a misconfigured Electron
    // build where `app.getVersion()` returns empty), formatVersionText must
    // still return '' so the truthiness-gated `.titlebar-version` span is
    // absent — NOT 'v' alone, which would render the exact broken state
    // this PR aims to structurally prevent.
    mockGet.mockResolvedValue({ appVersion: '', spaHash: null });
    mockOnChange.mockReturnValue(() => {});
    const { container } = render(<Titlebar />);
    // Wait for the async get() resolution to flush, then assert.
    await waitFor(() => {
      // The async fetch ran (mock was awaited)
      expect(mockGet).toHaveBeenCalled();
    });
    // No version span — empty appVersion produces empty versionText.
    expect(container.querySelector('.titlebar-version')).toBeNull();
    // Brand text still renders.
    expect(container.querySelector('.titlebar-title')?.textContent).toBe('Concord Voice');
  });

  it('renders no version span when appVersion is empty string AND spaHash is present', async () => {
    // Stronger variant: even with a valid spaHash, an empty appVersion must
    // NOT produce 'v-<hash>'. Pre-fix's `appVersion === null` check let this
    // slip through; post-fix's truthiness check forecloses it.
    mockGet.mockResolvedValue({ appVersion: '', spaHash: 'abc123' });
    mockOnChange.mockReturnValue(() => {});
    const { container } = render(<Titlebar />);
    await waitFor(() => {
      expect(mockGet).toHaveBeenCalled();
    });
    expect(container.querySelector('.titlebar-version')).toBeNull();
  });

  it('cleans up the onChange subscription on unmount', () => {
    const unsubscribe = vi.fn();
    mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: 'abc' });
    mockOnChange.mockReturnValue(unsubscribe);
    const { unmount } = render(<Titlebar />);
    unmount();
    expect(unsubscribe).toHaveBeenCalled();
  });

  // Platform detection: jsdom provides userAgentData=undefined so the
  // implementation falls back to navigator.platform, which the spy controls.
  // This exercises the navigator.platform fallback branch (Finding 1).
  describe('platform detection (titlebar--mac modifier)', () => {
    it('applies titlebar--mac class on Mac platform', () => {
      const spy = vi.spyOn(window.navigator, 'platform', 'get');
      spy.mockReturnValue('MacIntel');
      mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: null });
      mockOnChange.mockReturnValue(() => {});
      const { container } = render(<Titlebar />);
      expect(container.querySelector('.titlebar.titlebar--mac')).not.toBeNull();
      spy.mockRestore();
    });

    it('does not apply titlebar--mac class on non-Mac platform', () => {
      const spy = vi.spyOn(window.navigator, 'platform', 'get');
      spy.mockReturnValue('Win32');
      mockGet.mockResolvedValue({ appVersion: '0.1.40', spaHash: null });
      mockOnChange.mockReturnValue(() => {});
      const { container } = render(<Titlebar />);
      expect(container.querySelector('.titlebar.titlebar--mac')).toBeNull();
      spy.mockRestore();
    });
  });
});
