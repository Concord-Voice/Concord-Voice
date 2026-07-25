import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { SPA_FALLBACK_MESSAGE, type SpaFallbackDiagnostic } from '@/shared/spaIpcTypes';
import { resetAllStores } from '../../helpers/store-helpers';
import { SpaFallbackOverlay } from '@/renderer/components/SpaFallbackOverlay/SpaFallbackOverlay';

/**
 * Regression for #2401 — the "Could not reach Concord servers" banner survived a
 * demonstrably successful connection.
 *
 * Public report: Concord-Voice/Concord-Voice#79. The banner appeared at launch
 * even though the client was connected and messages were visible; only CTRL+R
 * cleared it, and a full restart reproduced it.
 *
 * Mechanism: `loadPackagedRenderer` (src/main/main.ts) sends
 * `app:configFetchFailed` on a 2000 ms delay when the launch-time SPA config
 * fetch loses its 5 s race. The reporter's logs show the renderer WebSocket
 * reaching `connected` ~600 ms after renderer start — so the diagnostic lands
 * AFTER the app has already proven the servers are reachable.
 *
 * The retraction is class-scoped, which is the load-bearing part: `isUnexpectedBundled`
 * fires this diagnostic for six conditions and only ONE ('unreachable') means the
 * servers were unreachable. `rejected` (a refused spaUrl, incl. the #750 poisoned
 * sentinel) and `contract` (shell too old for the deployed SPA) both fire against a
 * REACHABLE server, so connectivity must NOT retract them — doing so would silence a
 * fail-loud sentinel. A diagnostic with no `kind` comes from a pre-#2401 shell and is
 * likewise never retracted (fail closed).
 */

const UNREACHABLE = SPA_FALLBACK_MESSAGE.unreachable;

describe('SpaFallbackOverlay — stale unreachable-server banner (#2401)', () => {
  const originalElectron = (globalThis as unknown as { electron?: unknown }).electron;
  let configFetchFailedHandler: ((data: SpaFallbackDiagnostic) => void) | null = null;

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    configFetchFailedHandler = null;
    (globalThis as unknown as { electron: unknown }).electron = {
      onConfigFetchFailed: (handler: (data: SpaFallbackDiagnostic) => void) => {
        configFetchFailedHandler = handler;
        return () => {
          configFetchFailedHandler = null;
        };
      },
    };
  });

  afterEach(() => {
    (globalThis as unknown as { electron?: unknown }).electron = originalElectron;
  });

  const connect = () =>
    act(() => {
      useChatStore.getState().setConnectionStatus(true, 'client-1', 'connected');
    });

  const disconnect = () =>
    act(() => {
      useChatStore.getState().setConnectionStatus(false, undefined, 'disconnected');
    });

  const emit = (data: SpaFallbackDiagnostic) =>
    act(() => {
      configFetchFailedHandler?.(data);
    });

  it('clears an unreachable banner once the connection succeeds, without a reload', () => {
    render(<SpaFallbackOverlay />);

    emit({ reason: UNREACHABLE, kind: 'unreachable' });
    expect(screen.getByRole('alert')).toHaveTextContent(UNREACHABLE);

    connect();

    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('never shows an unreachable banner when ALREADY connected (the reported case)', () => {
    // Reporter's timeline: WS connects ~600 ms after renderer start; main's
    // app:configFetchFailed lands on its 2000 ms delay — i.e. already connected.
    connect();

    render(<SpaFallbackOverlay />);
    emit({ reason: UNREACHABLE, kind: 'unreachable' });

    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('still shows the banner while the connection has NOT succeeded', () => {
    render(<SpaFallbackOverlay />);

    emit({ reason: UNREACHABLE, kind: 'unreachable' });
    expect(screen.getByRole('alert')).toHaveTextContent(UNREACHABLE);

    // A genuine outage: the socket retries but never reaches CONNECTED.
    act(() => {
      useChatStore.getState().setConnectionStatus(false, undefined, 'connecting');
    });

    expect(screen.getByRole('alert')).toHaveTextContent(UNREACHABLE);
  });

  it('keeps the latch closed after a later disconnect (no stale re-show)', () => {
    render(<SpaFallbackOverlay />);

    // Separate acts, with an assertion between: batching both into one act()
    // would let the banner never commit to the DOM, making the retraction
    // assertion below pass trivially whether or not it actually retracted.
    emit({ reason: UNREACHABLE, kind: 'unreachable' });
    expect(screen.getByRole('alert')).toHaveTextContent(UNREACHABLE);

    connect();
    expect(screen.queryByRole('alert')).toBeNull();

    disconnect();
    expect(screen.queryByRole('alert')).toBeNull();

    // The load-bearing assertion: re-fire the diagnostic AFTER the disconnect.
    // Without this, a regression that reset hasEverConnectedRef on disconnect
    // would leave every other case green while re-showing the exact false
    // banner #2401 fixed (WS connect ~600 ms, drop, diagnostic at 2000 ms).
    emit({ reason: UNREACHABLE, kind: 'unreachable' });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('does NOT retract a rejected diagnostic — the server was reachable and refused', () => {
    // #750 poisoned sentinel / non-HTTPS spaUrl / invalid URL / 4xx. Reaching the
    // server is not counter-evidence here; with the sentinel it is the whole point.
    render(<SpaFallbackOverlay />);

    emit({ reason: SPA_FALLBACK_MESSAGE.rejected, kind: 'rejected' });
    expect(screen.getByRole('alert')).toHaveTextContent(SPA_FALLBACK_MESSAGE.rejected);

    connect();

    expect(screen.getByRole('alert')).toHaveTextContent(SPA_FALLBACK_MESSAGE.rejected);
  });

  it('does NOT suppress a rejected diagnostic that arrives while already connected', () => {
    connect();

    render(<SpaFallbackOverlay />);
    emit({ reason: SPA_FALLBACK_MESSAGE.rejected, kind: 'rejected' });

    expect(screen.getByRole('alert')).toHaveTextContent(SPA_FALLBACK_MESSAGE.rejected);
  });

  it('does NOT retract a contract diagnostic — the shell is too old regardless', () => {
    render(<SpaFallbackOverlay />);

    emit({ reason: SPA_FALLBACK_MESSAGE.contract, kind: 'contract' });
    expect(screen.getByRole('alert')).toHaveTextContent(SPA_FALLBACK_MESSAGE.contract);

    connect();

    expect(screen.getByRole('alert')).toHaveTextContent(SPA_FALLBACK_MESSAGE.contract);
  });

  it('fails closed on a diagnostic with no kind (pre-#2401 shell)', () => {
    // A newer remote SPA can run on an older shell that sends no classification.
    // Treating that as `unreachable` would silently re-open the sentinel gap.
    render(<SpaFallbackOverlay />);

    emit({ reason: UNREACHABLE });
    expect(screen.getByRole('alert')).toHaveTextContent(UNREACHABLE);

    connect();

    expect(screen.getByRole('alert')).toHaveTextContent(UNREACHABLE);
  });

  it('still dismisses on the close button for a non-retractable class', () => {
    render(<SpaFallbackOverlay />);

    emit({ reason: SPA_FALLBACK_MESSAGE.contract, kind: 'contract' });
    act(() => {
      screen.getByRole('button', { name: /dismiss/i }).click();
    });

    expect(screen.queryByRole('alert')).toBeNull();
  });
});
