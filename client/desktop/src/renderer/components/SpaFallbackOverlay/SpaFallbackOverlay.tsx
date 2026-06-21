import { useEffect, useState, type ReactElement } from 'react';
import './SpaFallbackOverlay.css';

/**
 * Non-blocking diagnostic banner for #830 Option C (renderer side; #831 ships it).
 *
 * Subscribes to the app:configFetchFailed IPC event (emitted by main process when
 * spaLoader falls back to bundled for an unexpected reason — config fetch failed,
 * network issue, spaUrl rejected, IPC contract mismatch). Surfaces a top-of-screen
 * dismissable banner so silent fallback regressions fail loudly. Reason strings
 * are generic per [internal]rules/observability.md (no apiBase, no proxy details,
 * no PII). The login UI remains accessible underneath — this is observability,
 * not a blocking modal.
 *
 * Defensive: if globalThis.electron.onConfigFetchFailed is absent (e.g.,
 * shell-renderer contract mismatch where the shell predates #831), the
 * component renders nothing rather than throwing.
 */
export function SpaFallbackOverlay(): ReactElement | null {
  const [reason, setReason] = useState<string | null>(null);

  useEffect(() => {
    const sub = globalThis.electron?.onConfigFetchFailed;
    if (!sub) return;

    const unsubscribe = sub((data: { reason: string }) => {
      setReason(data.reason);
    });

    return unsubscribe;
  }, []);

  if (!reason) return null;

  return (
    <div role="alert" className="spa-fallback-overlay">
      <span className="spa-fallback-overlay__message">{reason}</span>
      <button
        type="button"
        className="spa-fallback-overlay__dismiss"
        onClick={() => setReason(null)}
        aria-label="Dismiss"
      >
        ×
      </button>
    </div>
  );
}
