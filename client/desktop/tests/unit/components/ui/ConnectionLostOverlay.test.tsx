import React from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/ui/ConnectionLostOverlay.css', () => ({}));

// Mock dynamic imports for recoveryService and resetService
const mockRunPreflight = vi.fn();
const mockSoftRestart = vi.fn();

// Mock websocketService singleton
const mockDisconnect = vi.fn();
const mockResetReconnectState = vi.fn();
const mockConnect = vi.fn();
const mockGetWebSocketService = vi.fn(() => ({
  disconnect: mockDisconnect,
  resetReconnectState: mockResetReconnectState,
  connect: mockConnect,
}));

vi.mock('@/renderer/services/recoveryService', () => ({
  runPreflight: (...args: unknown[]) => mockRunPreflight(...args),
}));

vi.mock('@/renderer/services/resetService', () => ({
  softRestart: (...args: unknown[]) => mockSoftRestart(...args),
}));

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: (...args: unknown[]) => mockGetWebSocketService(...args),
}));

import ConnectionLostOverlay from '@/renderer/components/ui/ConnectionLostOverlay';

describe('ConnectionLostOverlay', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders nothing when phase is stable', () => {
    useConnectionStore.setState({ phase: 'stable', diagnostics: null });
    const { container } = render(<ConnectionLostOverlay />);
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing when phase is grace_period', () => {
    useConnectionStore.setState({ phase: 'grace_period', diagnostics: null });
    const { container } = render(<ConnectionLostOverlay />);
    expect(container.firstChild).toBeNull();
  });

  it('renders preflight phase with diagnostics message', () => {
    useConnectionStore.setState({ phase: 'preflight', diagnostics: null });
    render(<ConnectionLostOverlay />);
    expect(screen.getByText('Running Diagnostics')).toBeInTheDocument();
  });

  it('renders recovery_a phase with No Internet title when internet is false', () => {
    useConnectionStore.setState({
      phase: 'recovery_a',
      diagnostics: {
        internet: 'failed',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      },
    });
    render(<ConnectionLostOverlay />);
    expect(screen.getByText('No Internet')).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
    expect(screen.getByText('Exit App')).toBeInTheDocument();
  });

  it('renders recovery_a phase with Waiting for Server when internet is ok', () => {
    useConnectionStore.setState({
      phase: 'recovery_a',
      diagnostics: {
        internet: 'ok',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      },
    });
    render(<ConnectionLostOverlay />);
    expect(screen.getByText('Waiting for Server')).toBeInTheDocument();
  });

  it('renders recovery_b phase with Restarting Client title', () => {
    useConnectionStore.setState({ phase: 'recovery_b', diagnostics: null });
    render(<ConnectionLostOverlay />);
    expect(screen.getByText('Restarting Client')).toBeInTheDocument();
  });

  it('renders fatal phase with Session Revoked when sessionRevoked is true', () => {
    useConnectionStore.setState({
      phase: 'fatal',
      diagnostics: {
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: true,
        rendererStable: 'ok',
      },
    });
    render(<ConnectionLostOverlay />);
    expect(screen.getByText('Session Revoked')).toBeInTheDocument();
    expect(screen.getByText('Restart')).toBeInTheDocument();
    expect(screen.getByText('Exit App')).toBeInTheDocument();
  });

  it('renders fatal phase with Connection Failed when session not revoked', () => {
    useConnectionStore.setState({
      phase: 'fatal',
      diagnostics: {
        internet: 'failed',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      },
    });
    render(<ConnectionLostOverlay />);
    expect(screen.getByText('Connection Failed')).toBeInTheDocument();
  });

  describe('DiagnosticDisplay', () => {
    it('shows diagnostic checklist with OK/Failed status', () => {
      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: {
          internet: 'ok',
          serverReachable: 'failed',
          tokenValid: 'ok',
          sessionRevoked: false,
          rendererStable: 'ok',
        },
      });
      render(<ConnectionLostOverlay />);
      // Check that diagnostics are displayed
      expect(screen.getByText('Internet')).toBeInTheDocument();
      expect(screen.getByText('Server')).toBeInTheDocument();
      expect(screen.getByText('Token')).toBeInTheDocument();
      expect(screen.getByText('Session')).toBeInTheDocument();

      // Check status values
      const okElements = screen.getAllByText('OK');
      const failedElements = screen.getAllByText('Failed');
      expect(okElements.length).toBe(3); // internet OK, token OK, session OK (not revoked)
      expect(failedElements.length).toBe(1); // server failed
    });

    it('renders -- and applies unknown class when a check did not run', () => {
      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: {
          internet: 'unknown', // check did not run
          serverReachable: 'failed',
          tokenValid: 'ok',
          sessionRevoked: undefined, // also not run — Session row should show --
          rendererStable: 'ok',
        },
      });
      render(<ConnectionLostOverlay />);

      // -- text appears for both the un-run check rows
      const dashes = screen.getAllByText('--');
      expect(dashes.length).toBeGreaterThanOrEqual(2); // internet + session

      // The unknown class is applied via DiagnosticDisplay's statusClass
      // (visible via CSS class — assert via getByText then check parent).
      const internetDash = dashes.find(
        (el) => el.previousElementSibling?.textContent === 'Internet'
      );
      expect(internetDash).toBeDefined();
      expect(internetDash!.className).toContain('unknown');
    });
  });

  describe('Retry button behavior', () => {
    it('calls runPreflight on Retry click', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'fresh-token' });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: {
          internet: 'failed',
          serverReachable: 'failed',
          tokenValid: 'failed',
          sessionRevoked: false,
          rendererStable: 'ok',
        },
      });
      render(<ConnectionLostOverlay />);

      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(mockRunPreflight).toHaveBeenCalled();
      });
    });

    it('enters fatal phase when session is revoked after retry', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: true,
        rendererStable: 'ok',
      });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: null,
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(useConnectionStore.getState().phase).toBe('fatal');
      });
    });

    it('disconnects WS, resets store, and reconnects with fresh token on successful retry', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'fresh-jwt-token' });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: null,
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(mockDisconnect).toHaveBeenCalled();
        expect(mockResetReconnectState).toHaveBeenCalled();
        expect(mockConnect).toHaveBeenCalledWith('fresh-jwt-token');
        expect(useConnectionStore.getState().phase).toBe('stable');
      });
    });

    it('enters fatal when diagnostics pass but no access token available', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: null });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: null,
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(useConnectionStore.getState().phase).toBe('fatal');
      });
      // Should NOT attempt to connect without a token
      expect(mockConnect).not.toHaveBeenCalled();
    });

    it('enters recovery_a when server is unreachable (with token present; escape fires)', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'token' });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: null,
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(useConnectionStore.getState().phase).toBe('recovery_a');
      });
      // Escape path: connect WAS called even though server is unreachable.
      // Preflight is diagnostic-only — it never gates the action.
      expect(mockConnect).toHaveBeenCalledWith('token');
    });

    it('increments recovery attempts on failed retry (server unreachable, token present)', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'token' });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: null,
        recoveryAttempts: 0,
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(useConnectionStore.getState().recoveryAttempts).toBe(1);
        expect(useConnectionStore.getState().phase).toBe('recovery_a');
      });
    });

    it('does not increment recovery attempts on successful reconnect', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'token' });

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: null,
        recoveryAttempts: 2,
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        // reset() clears recoveryAttempts on success path
        expect(useConnectionStore.getState().recoveryAttempts).toBe(0);
        expect(useConnectionStore.getState().phase).toBe('stable');
      });
    });

    it('always attempts WS connect on Retry click, even when server unreachable', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'failed',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'stale-but-present-token' });
      useConnectionStore.setState({ phase: 'recovery_a', diagnostics: null });
      render(<ConnectionLostOverlay />);

      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(mockDisconnect).toHaveBeenCalled();
        expect(mockResetReconnectState).toHaveBeenCalled();
        expect(mockConnect).toHaveBeenCalledWith('stale-but-present-token');
      });
      // Preflight still reports degraded — UI stays in recovery_a.
      expect(useConnectionStore.getState().phase).toBe('recovery_a');
    });

    it('does NOT attempt connect when session is revoked, regardless of preflight', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'ok',
        sessionRevoked: true,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: 'token' });
      useConnectionStore.setState({ phase: 'recovery_a', diagnostics: null });
      render(<ConnectionLostOverlay />);

      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(useConnectionStore.getState().phase).toBe('fatal');
      });
      expect(mockConnect).not.toHaveBeenCalled();
    });

    it('does NOT attempt connect when no token is present (escape requires token)', async () => {
      mockRunPreflight.mockResolvedValue({
        internet: 'failed',
        serverReachable: 'failed',
        tokenValid: 'failed',
        sessionRevoked: false,
        rendererStable: 'ok',
      });
      useAuthStore.setState({ accessToken: null });
      useConnectionStore.setState({ phase: 'recovery_a', diagnostics: null });
      render(<ConnectionLostOverlay />);

      fireEvent.click(screen.getByText('Retry'));

      await waitFor(() => {
        expect(useConnectionStore.getState().phase).toBe('fatal');
      });
      expect(mockConnect).not.toHaveBeenCalled();
    });
  });

  describe('Exit App button', () => {
    it('calls electron.quitApp when available', () => {
      const mockQuitApp = vi.fn();
      (globalThis as any).electron = { ...(globalThis as any).electron, quitApp: mockQuitApp };

      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: {
          internet: 'failed',
          serverReachable: 'failed',
          tokenValid: 'failed',
          sessionRevoked: false,
          rendererStable: 'ok',
        },
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Exit App'));
      expect(mockQuitApp).toHaveBeenCalled();
    });
  });

  describe('Restart button in fatal phase', () => {
    it('calls softRestart when Restart is clicked', async () => {
      useConnectionStore.setState({
        phase: 'fatal',
        diagnostics: {
          internet: 'ok',
          serverReachable: 'ok',
          tokenValid: 'ok',
          sessionRevoked: true,
          rendererStable: 'ok',
        },
      });
      render(<ConnectionLostOverlay />);
      fireEvent.click(screen.getByText('Restart'));

      await waitFor(() => {
        expect(mockSoftRestart).toHaveBeenCalled();
      });
    });
  });
});
