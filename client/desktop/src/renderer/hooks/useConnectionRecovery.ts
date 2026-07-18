/**
 * useConnectionRecovery - Connection loss detection and phased recovery logic.
 *
 * Extracted from useWebSocket to isolate the reconnection state machine.
 * Returns a callback that maps ConnectionState changes to recovery actions.
 */

import { useCallback } from 'react';
import { ConnectionState, getWebSocketService } from '../services/websocketService';
import { e2eeService } from '../services/e2eeService';
import { useConnectionStore } from '../stores/connectionStore';
import { useUserStore } from '../stores/userStore';
import { useVoiceStore } from '../stores/voiceStore';
import { useMemberStore } from '../stores/memberStore';
import { useServerStore } from '../stores/serverStore';
import { useFriendStore } from '../stores/friendStore';
import { runRecoveryModule } from '../utils/runRecoveryModule';
import { hydratePostLogin } from '../services/postLoginHydration';
import {
  beginPostLoginHydrationGuard,
  isHydrationLifecycleCurrent,
} from '../services/postLoginHydrationLifecycle';

/** Run preflight diagnostics after grace period expires and route to the appropriate recovery path. */
async function runPreflightDiagnostics(wsService: ReturnType<typeof getWebSocketService>) {
  if (wsService.getState() === ConnectionState.CONNECTED) return;

  wsService.setAggressiveReconnect(false);
  const store = useConnectionStore.getState();
  if (store.phase !== 'grace_period') return;

  store.enterPreflight();

  // #803: the grace period expired with the socket still down — a genuine sustained
  // disconnect, not a transient blip. Reflect the self-user as Offline, but ONLY
  // downgrade from 'online' — never overwrite a deliberate dnd/invisible choice.
  // (If we clobbered dnd→offline here, the legacy online_user_ids reconnect path,
  // which promotes 'offline'→'online', would then launder it into 'online' and lose
  // the user's choice.) The 15s grace period IS the debounce against flicker;
  // reconnection restores the real status via the presence_snapshot handler.
  if (useMemberStore.getState().selfStatus === 'online') {
    useMemberStore.getState().setSelfStatus('offline');
  }

  // Guard the lazy import: a stale SPA chunk here previously rejected and was
  // swallowed by the caller's `.catch(console.debug)`, leaving the store stuck
  // in 'preflight' with no diagnostics and no recovery. runRecoveryModule
  // triggers self-heal on failure instead. runPreflight() is throw-safe (it
  // returns a DiagnosticResults object, never throws — see recoveryService.ts),
  // so the only failure the outer guard catches is the chunk-load rejection —
  // exactly what self-heal is for. The nested resetService import is guarded
  // the same way.
  await runRecoveryModule(
    () => import('../services/recoveryService'),
    async ({ runPreflight }) => {
      const diag = await runPreflight();
      store.setDiagnostics(diag);

      if (diag.sessionRevoked) {
        store.enterFatal();
        return;
      }

      if (diag.internet !== 'ok' || diag.serverReachable !== 'ok') {
        store.enterRecoveryA();
        return;
      }

      if (diag.tokenValid === 'ok' && diag.rendererStable !== 'ok') {
        store.enterRecoveryB();
        await runRecoveryModule(
          () => import('../services/resetService'),
          (m) => m.softRestart(),
          'softRestart'
        );
        return;
      }

      if (diag.tokenValid === 'ok') {
        store.enterRecoveryA();
        return;
      }

      store.enterRecoveryB();
      await runRecoveryModule(
        () => import('../services/resetService'),
        (m) => m.softRestart(),
        'softRestart'
      );
    },
    'runPreflight'
  );
}

/** Handle recovery when connection drops (RECONNECTING state). */
function handleConnectionLoss(wsService: ReturnType<typeof getWebSocketService>) {
  const connStore = useConnectionStore.getState();
  if (connStore.phase !== 'stable') return;

  connStore.startGracePeriod();
  wsService.setAggressiveReconnect(true);

  const voiceState = useVoiceStore.getState();
  if (voiceState.connectionState !== 'disconnected' && voiceState.activeChannelId) {
    connStore.setLastVoiceChannelId(voiceState.activeChannelId);
  }

  import('../services/voiceService')
    .then(({ voiceService }) => {
      if (useVoiceStore.getState().connectionState !== 'disconnected') {
        voiceService.emergencyCleanup();
      }
    })
    .catch(() => {
      /* voice module never loaded */
    });

  setTimeout(() => {
    runPreflightDiagnostics(wsService).catch(console.debug);
  }, 15_000);
}

/**
 * Restores application state after the WebSocket reconnects.
 *
 * Performs recovery hydration for recovery phases, restores pending E2EE and voice state after a grace-period reconnect, and resets incomplete connection phases.
 */
function handleReconnected(
  wsService: ReturnType<typeof getWebSocketService>,
  validateEpochsOnReconnect: () => Promise<void>
) {
  const phase = useConnectionStore.getState().phase;
  wsService.setAggressiveReconnect(false);

  if (phase === 'recovery_a' || phase === 'preflight') {
    // Floated intentionally — runRecoveryModule never rejects (it swallows a
    // stale-chunk import failure and triggers self-heal), so this cannot
    // surface as the Uncaught (in promise) seen in the origin-502-storm logs.
    runRecoveryModule(
      () => import('../services/resetService'),
      async (m) => {
        // Same account, same token — fence in-flight E2EE work and clear NOTHING.
        // gracefulReset() here destroyed drafts, the outbound MessageQueue, and the
        // user's activeServerId on every transient blip; hydratePostLogin restores
        // none of those, and the first two have no server copy to restore from
        // (#2199). Do NOT reintroduce a logout-class reset on this path.
        m.recoveryReset();
        const guard = beginPostLoginHydrationGuard();
        await useUserStore.getState().fetchUser(guard);
        if (!isHydrationLifecycleCurrent(guard)) return;
        await hydratePostLogin(guard);
        if (!isHydrationLifecycleCurrent(guard)) return;
        // #2329: a WS reconnect replays only subscriptions + a presence snapshot,
        // so state that changed while offline is never redelivered — a server or
        // channel the user was removed from stays visible, and messages sent
        // during the outage are missing. Refresh both authoritatively, session-
        // guarded so an in-flight fetch cannot clobber a newer session (account
        // switch): fetchServers/fetchRequests re-check the guard before their
        // committing set(), and useMessageFetch re-fetches the mounted channel on
        // the `connection-recovered` event under its own aborted/channel/E2EE
        // guards. Memberships first, so a revoked channel is dropped before we
        // would re-fetch its messages.
        await useServerStore.getState().fetchServers(guard);
        if (!isHydrationLifecycleCurrent(guard)) return;
        await useFriendStore.getState().fetchRequests(guard);
        if (!isHydrationLifecycleCurrent(guard)) return;
        globalThis.dispatchEvent(new CustomEvent('connection-recovered'));
      },
      'recoveryReset'
    );
    useConnectionStore.getState().reset();
    runRecoveryModule(
      () => import('../services/recoveryService'),
      (m) => m.clearCrashFlag(),
      'clearCrashFlag'
    );
    if (e2eeService.isInitialized) validateEpochsOnReconnect().catch(() => {});
    return;
  }

  if (phase === 'grace_period') {
    useConnectionStore.getState().reset();
    if (e2eeService.isInitialized) {
      e2eeService.processPendingKeyRequests().catch(() => {});
      validateEpochsOnReconnect().catch((err) => {
        console.debug('[WebSocket] validate_epochs failed:', err);
      });
    }
    const lastVoiceId = useConnectionStore.getState().lastVoiceChannelId;
    if (lastVoiceId) {
      useConnectionStore.getState().setLastVoiceChannelId(null);
      import('../services/voiceService')
        .then(({ voiceService }) => voiceService.joinChannel(lastVoiceId))
        .catch(() => {
          /* voice module not available */
        });
    }
    return;
  }

  if (phase !== 'stable') {
    useConnectionStore.getState().reset();
  }
}

export function useConnectionRecovery(
  wsService: ReturnType<typeof getWebSocketService>,
  validateEpochsOnReconnect: () => Promise<void>
): (state: ConnectionState) => void {
  return useCallback(
    (state: ConnectionState) => {
      if (state === ConnectionState.RECONNECTING) {
        handleConnectionLoss(wsService);
      } else if (state === ConnectionState.CONNECTED) {
        handleReconnected(wsService, validateEpochsOnReconnect);
      }
    },
    [wsService, validateEpochsOnReconnect]
  );
}
