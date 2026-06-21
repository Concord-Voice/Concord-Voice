import React, { useCallback } from 'react';
import { Loader, RefreshCw, LogOut, AlertTriangle, Wifi } from 'lucide-react';
import {
  useConnectionStore,
  type RecoveryPhase,
  type DiagnosticResults,
  type CheckResult,
} from '../../stores/connectionStore';
// recoveryService & resetService loaded on-demand via dynamic import() to allow code splitting
import { runRecoveryModule } from '../../utils/runRecoveryModule';
import './ConnectionLostOverlay.css';

/** Display text per CheckResult value. Extracted from the inline ternary
 *  to satisfy SonarQube's "no nested ternary" rule and to keep the
 *  status-string mapping authoritative in one place. */
const STATUS_TEXT: Record<CheckResult, string> = {
  ok: 'OK',
  failed: 'Failed',
  unknown: '--',
};

/** Diagnostic checklist shown during recovery phases */
function DiagnosticDisplay({ diagnostics }: Readonly<{ diagnostics: DiagnosticResults | null }>) {
  if (!diagnostics) return null;

  const boolToCheckResult = (b: boolean | undefined): CheckResult => {
    if (b === true) return 'ok';
    if (b === false) return 'failed';
    return 'unknown';
  };

  const invertCheckResult = (c: CheckResult): CheckResult => {
    if (c === 'ok') return 'failed';
    if (c === 'failed') return 'ok';
    return 'unknown';
  };

  const items: { label: string; result: CheckResult }[] = [
    { label: 'Internet', result: diagnostics.internet },
    { label: 'Server', result: diagnostics.serverReachable },
    { label: 'Token', result: diagnostics.tokenValid },
    { label: 'Session', result: invertCheckResult(boolToCheckResult(diagnostics.sessionRevoked)) },
  ];

  return (
    <div className="connection-lost-diagnostics">
      {items.map((item) => {
        const statusText = STATUS_TEXT[item.result];
        const statusClass = item.result; // 'ok' | 'failed' | 'unknown'

        return (
          <div key={item.label} className="connection-lost-diag-row">
            <span className="connection-lost-diag-label">{item.label}</span>
            <span className={`connection-lost-diag-value ${statusClass}`}>{statusText}</span>
          </div>
        );
      })}
    </div>
  );
}

/** Phase-specific card content */
function PhaseContent({
  phase,
  diagnostics,
}: Readonly<{
  phase: RecoveryPhase;
  diagnostics: DiagnosticResults | null;
}>) {
  const handleRetry = useCallback(async () => {
    const store = useConnectionStore.getState();

    // Wrap dynamic imports so a chunk-load failure doesn't silently no-op
    // the Retry button — exactly the UX pathology this PR is fixing.
    let getWebSocketService: typeof import('../../services/websocketService').getWebSocketService;
    let useAuthStoreImport: typeof import('../../stores/authStore').useAuthStore;
    let runPreflight: typeof import('../../services/recoveryService').runPreflight;
    try {
      ({ getWebSocketService } = await import('../../services/websocketService'));
      ({ useAuthStore: useAuthStoreImport } = await import('../../stores/authStore'));
      ({ runPreflight } = await import('../../services/recoveryService'));
    } catch {
      store.incrementRecoveryAttempts();
      store.enterFatal();
      return;
    }

    const wsService = getWebSocketService();
    store.enterPreflight();
    const diag = await runPreflight();
    store.setDiagnostics(diag);

    // Hard-stop: session terminated by server — no point attempting reconnect.
    if (diag.sessionRevoked) {
      store.incrementRecoveryAttempts();
      store.enterFatal();
      return;
    }

    // runPreflight() refreshed the token via IPC if it succeeded; pull the
    // latest value (may be null if refresh failed or session was logged out).
    const token = useAuthStoreImport.getState().accessToken;
    if (!token) {
      store.incrementRecoveryAttempts();
      store.enterFatal();
      return;
    }

    // ESCAPE PATH — unconditional reconnect attempt.
    // Preflight is diagnostic-only; it never gates the action. If the server
    // is reachable, this restores the connection (the WS hub's CONNECTED
    // event drives the transition to stable via ConnectionStateProvider).
    // If unreachable, the existing reconnect loop continues at the
    // freshly-reset cadence.
    wsService.disconnect();
    wsService.resetReconnectState();
    wsService.connect(token);

    // Phase transition reflects preflight result for UI display only.
    if (diag.serverReachable !== 'ok' || diag.tokenValid !== 'ok') {
      store.incrementRecoveryAttempts();
      store.enterRecoveryA();
      return;
    }

    // Optimistic transition — WS hub will revert if connect doesn't reach CONNECTED.
    store.reset();
  }, []);

  const handleRestart = useCallback(async () => {
    // Guard the lazy import: a stale chunk after a Pages redeploy must not make
    // the fatal-state Restart button silently no-op (it previously did — the
    // sibling handleRetry was guarded, this was not). runRecoveryModule degrades
    // to self-heal on failure.
    await runRecoveryModule(
      () => import('../../services/resetService'),
      (m) => m.softRestart(),
      'softRestart'
    );
  }, []);

  const handleExit = useCallback(() => {
    if (globalThis.electron?.quitApp) {
      globalThis.electron.quitApp();
    } else {
      globalThis.close();
    }
  }, []);

  switch (phase) {
    case 'preflight':
      return (
        <>
          <div className="connection-lost-icon spinning">
            <Loader size={48} />
          </div>
          <h2 className="connection-lost-title">Running Diagnostics</h2>
          <p className="connection-lost-message">
            Checking connectivity and session status&hellip;
          </p>
        </>
      );

    case 'recovery_a':
      return (
        <>
          <div className="connection-lost-icon">
            <Wifi size={48} />
          </div>
          <h2 className="connection-lost-title">
            {diagnostics?.internet === 'failed' ? 'No Internet' : 'Waiting for Server'}
          </h2>
          <p className="connection-lost-message">
            {diagnostics?.internet === 'failed'
              ? 'Your internet connection appears to be down. Reconnection will happen automatically when connectivity is restored.'
              : 'The server appears to be unreachable. Reconnection attempts are ongoing in the background.'}
          </p>
          <DiagnosticDisplay diagnostics={diagnostics} />
          <div className="connection-lost-actions">
            <button className="connection-lost-btn primary" onClick={handleRetry}>
              <RefreshCw size={16} />
              Retry
            </button>
            <button className="connection-lost-btn secondary" onClick={handleExit}>
              <LogOut size={16} />
              Exit App
            </button>
          </div>
        </>
      );

    case 'recovery_b':
      return (
        <>
          <div className="connection-lost-icon spinning">
            <RefreshCw size={48} />
          </div>
          <h2 className="connection-lost-title">Restarting Client</h2>
          <p className="connection-lost-message">
            Detected a client-side issue. Performing a soft restart to recover your session&hellip;
          </p>
        </>
      );

    case 'fatal':
      return (
        <>
          <div className="connection-lost-icon error">
            <AlertTriangle size={48} />
          </div>
          <h2 className="connection-lost-title">
            {diagnostics?.sessionRevoked ? 'Session Revoked' : 'Connection Failed'}
          </h2>
          <p className="connection-lost-message">
            {diagnostics?.sessionRevoked
              ? 'Your session was terminated by the server. This may happen when logging in from another device or when your session expires.'
              : 'Unable to restore your connection after multiple attempts.'}
          </p>
          <DiagnosticDisplay diagnostics={diagnostics} />
          <div className="connection-lost-actions">
            <button className="connection-lost-btn primary" onClick={handleRestart}>
              <RefreshCw size={16} />
              Restart
            </button>
            <button className="connection-lost-btn secondary" onClick={handleExit}>
              <LogOut size={16} />
              Exit App
            </button>
          </div>
        </>
      );

    default:
      return null;
  }
}

const ConnectionLostOverlay: React.FC = () => {
  const phase = useConnectionStore((s) => s.phase);
  const diagnostics = useConnectionStore((s) => s.diagnostics);

  // Only show overlay for phases beyond grace_period
  if (phase === 'stable' || phase === 'grace_period') return null;

  return (
    <div className="connection-lost-overlay">
      <div className="connection-lost-card">
        <PhaseContent phase={phase} diagnostics={diagnostics} />
      </div>
    </div>
  );
};

export default ConnectionLostOverlay;
