import { useEffect, useRef, useState, type ReactElement } from 'react';
import { useChatStore } from '../../stores/chat/chatStore';
import type { SpaFallbackDiagnostic } from '../../../shared/spaIpcTypes';
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
 *
 * A reached server retracts an `unreachable` diagnostic — and ONLY that one
 * (#2401). main sends the diagnostic on a 2000ms delay while the WebSocket
 * typically connects well inside that window, so an `unreachable` claim
 * routinely lands on a client that has already reached the servers and had no
 * input that could falsify it (public report Concord-Voice/Concord-Voice#79:
 * only CTRL+R cleared it, and a restart brought it back). The other two classes
 * — `rejected` (a refused spaUrl, incl. the #750 poisoned sentinel) and
 * `contract` (shell too old for the deployed SPA) — fire against a REACHABLE
 * server, so connectivity does not falsify them and retracting them would
 * silence a fail-loud sentinel. A diagnostic with NO `kind` comes from a
 * pre-#2401 shell and is likewise never retracted (fail closed).
 *
 * The retraction is one-way: a launch-time diagnostic that has been falsified
 * stays falsified, so a later disconnect must not resurrect it — ongoing
 * connectivity is ConnectionLostOverlay's concern.
 *
 * `chatStore.isConnected` is the AUTHENTICATED WebSocket connection signal,
 * written from exactly one place (useWebSocket's onConnectionChange handler),
 * which mounts only inside the authenticated tree. Known residual: a banner
 * shown pre-login is not retracted by a successful /auth/login round-trip —
 * strictly stronger proof of reachability — only by the WS connect that
 * follows it. chatStore is read imperatively rather than through a selector
 * because this is a one-shot first-observation latch held in a ref: it must
 * not participate in render at all, which is also why the deviation from
 * [internal]rules/frontend.md's selective-subscription rule is warranted here
 * (same shape as the settings-store single-sink subscribers).
 */
export function SpaFallbackOverlay(): ReactElement | null {
  const [banner, setBanner] = useState<SpaFallbackDiagnostic | null>(null);
  const hasEverConnectedRef = useRef(false);

  useEffect(() => {
    // Seed from current state: with a restored session the WebSocket can already
    // be up before `isRestoring` clears and this overlay mounts, so the latch
    // must not depend on observing a transition. The subscribe path below covers
    // the fresh-login ordering, where the connection arrives after mount.
    if (useChatStore.getState().isConnected) {
      hasEverConnectedRef.current = true;
      return;
    }

    const unsubscribe = useChatStore.subscribe((state) => {
      if (!state.isConnected) return;
      hasEverConnectedRef.current = true;
      // One-shot: the latch can never re-open, so stop listening rather than
      // leaving a dead callback on the renderer's highest-churn store, where
      // every message and typing event would keep invoking it.
      unsubscribe();
      console.debug('[SpaFallback] connection observed — unreachable diagnostics no longer apply');
      setBanner((current) => (current?.kind === 'unreachable' ? null : current));
    });

    return unsubscribe;
  }, []);

  useEffect(() => {
    const sub = globalThis.electron?.onConfigFetchFailed;
    if (!sub) return;

    const unsubscribe = sub((data: SpaFallbackDiagnostic) => {
      // Only an `unreachable` claim is falsified by having reached the server.
      if (data.kind === 'unreachable' && hasEverConnectedRef.current) {
        console.debug('[SpaFallback] suppressed stale unreachable diagnostic (already connected)');
        return;
      }
      setBanner(data);
    });

    return unsubscribe;
  }, []);

  if (!banner) return null;

  return (
    <div role="alert" className="spa-fallback-overlay">
      <span className="spa-fallback-overlay__message">{banner.reason}</span>
      <button
        type="button"
        className="spa-fallback-overlay__dismiss"
        onClick={() => setBanner(null)}
        aria-label="Dismiss"
      >
        ×
      </button>
    </div>
  );
}
