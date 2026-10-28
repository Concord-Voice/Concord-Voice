/**
 * useConnectionRecovery - Connection loss detection and phased recovery logic.
 *
 * Extracted from useWebSocket to isolate the reconnection state machine.
 * Returns a callback that maps ConnectionState changes to recovery actions.
 */

import { useCallback } from 'react';
import { ConnectionState, getWebSocketService } from '../../services/messaging/websocketService';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { getVoiceActionGeneration } from '../../services/voice/voiceActionGeneration';
import { useConnectionStore, type VoiceRejoinState } from '../../stores/ui/connectionStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useAuthStore } from '../../stores/auth/authStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useMemberStore } from '../../stores/chat/memberStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { useFriendStore } from '../../stores/chat/friendStore';
import { runRecoveryModule } from '../../utils/runtime/runRecoveryModule';
import { hydratePostLogin } from '../../services/system/postLoginHydration';
import {
  beginPostLoginHydrationGuard,
  isHydrationLifecycleCurrent,
} from '../../services/system/postLoginHydrationLifecycle';

// Rejoin must wait for the teardown that invalidates the old media session.
// Otherwise a fast control-plane reconnect can build a new call which a late
// voice-service import immediately destroys.
// A later outage supersedes both the old import and any rejoin it scheduled.
let voiceRecoveryGeneration = 0;

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

  // Only NOW tear down an active voice session. The media plane is a separate
  // socket (media.concordvoice.chat, not even proxied by the same edge), so a
  // transient control-plane 1006 that reconnects in <1s says nothing about the
  // health of an in-progress call — running emergencyCleanup on the FIRST
  // disconnect event destroyed healthy joins on every blip (the 2026-08-21
  // incident: a voice join that had already encrypted its first frame was torn
  // down by a control-plane close that recovered 300ms later). A grace period
  // that expires still-down is the genuine sustained outage: past this point
  // the client may be missing key-revocation and membership events, so bounded
  // staleness (the 15s window) is the fail-closed budget for keeping the call
  // up. Capture the channel first so the recovery-path reconnect can rejoin.
  // Store the cleanup-backed ticket synchronously before yielding. An ordinary
  // connection reset cancels it; recovery reconnects explicitly preserve it
  // until the pending cleanup and rejoin complete. Cleanup keys on
  // connectionState alone (a mid-join session with no channel id still needs
  // teardown); only the ticket needs a channel id.
  const voiceState = useVoiceStore.getState();
  const generation = voiceRecoveryGeneration;
  const authGeneration = useAuthStore.getState().authGeneration;
  if (voiceState.connectionState !== 'disconnected') {
    const expectedChannelId = voiceState.activeChannelId;
    const expectedVoiceActionGeneration = getVoiceActionGeneration();
    const cleanup = import('../../services/voice/voiceService')
      .then(({ voiceService }) => {
        if (authGeneration !== useAuthStore.getState().authGeneration) {
          if (
            expectedChannelId &&
            useConnectionStore.getState().pendingVoiceRejoin?.cleanup === cleanup
          ) {
            useConnectionStore.getState().setPendingVoiceRejoin(null);
          }
          return null;
        }
        if (
          (expectedChannelId &&
            useConnectionStore.getState().pendingVoiceRejoin?.cleanup !== cleanup) ||
          (!expectedChannelId && generation !== voiceRecoveryGeneration)
        )
          return null;
        const currentVoice = useVoiceStore.getState();
        if (
          currentVoice.connectionState === 'disconnected' ||
          getVoiceActionGeneration() !== expectedVoiceActionGeneration
        ) {
          // A user action can supersede this cleanup even while the voice
          // import is pending. Internal rebuilds do not advance that action
          // counter and still need fail-closed cleanup during a long outage.
          if (
            expectedChannelId &&
            useConnectionStore.getState().pendingVoiceRejoin?.cleanup === cleanup
          ) {
            useConnectionStore.getState().setPendingVoiceRejoin(null);
          }
          return null;
        }
        // Authorization can assign a channel after preflight captured a mid-join
        // null ID. An internal rebuild can also replace the room without a user
        // action. Both still need outage teardown; only an explicit action above
        // may supersede it. Do not rejoin the old room after cleaning a new one.
        if (
          expectedChannelId &&
          currentVoice.activeChannelId !== expectedChannelId &&
          useConnectionStore.getState().pendingVoiceRejoin?.cleanup === cleanup
        ) {
          useConnectionStore.getState().setPendingVoiceRejoin(null);
        }
        // The user can mute during the lazy import. Capture the last choice
        // immediately before cleanup resets the store.
        const privacyIntent: VoiceRejoinState = {
          joinType: currentVoice.isDMCall ? 'dm' : 'channel',
          isMuted: currentVoice.isMuted,
          isDeafened: currentVoice.isDeafened,
        };
        const voiceActionGeneration = voiceService.getVoiceActionGeneration();
        voiceService.emergencyCleanup();
        return { privacyIntent, voiceActionGeneration };
      })
      .catch(() => {
        // Voice module never loaded — no teardown happened, so the session is
        // still live and needs no rejoin; retract the unjustified ticket.
        if (
          expectedChannelId &&
          useConnectionStore.getState().pendingVoiceRejoin?.cleanup === cleanup
        ) {
          useConnectionStore.getState().setPendingVoiceRejoin(null);
        }
        return null;
      });
    if (expectedChannelId) {
      store.setPendingVoiceRejoin({ channelId: expectedChannelId, authGeneration, cleanup });
    }
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
    () => import('../../services/system/recoveryService'),
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
          () => import('../../services/system/resetService'),
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
        () => import('../../services/system/resetService'),
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

  voiceRecoveryGeneration += 1;
  connStore.startGracePeriod();
  wsService.setAggressiveReconnect(true);

  // Deliberately NO voice teardown here. The control-plane WS drops with 1006
  // routinely (proxy-edge churn) and reconnects within the grace period; the
  // media-plane session rides a separate socket and stays healthy through it.
  // Teardown happens in runPreflightDiagnostics, only after the grace period
  // expires with the socket still down — see the rationale there.

  setTimeout(() => {
    runPreflightDiagnostics(wsService).catch(console.debug);
  }, 15_000);
}

/** Resume a torn-down call only for the current outage, account, and voice owner. */
function schedulePendingVoiceRejoin(): void {
  const voiceRejoin = useConnectionStore.getState().pendingVoiceRejoin;
  if (!voiceRejoin) return;
  const generation = voiceRecoveryGeneration;
  voiceRejoin.cleanup
    .then((cleanupResult) => {
      if (
        useConnectionStore.getState().pendingVoiceRejoin !== voiceRejoin ||
        generation !== voiceRecoveryGeneration
      )
        return;
      if (!cleanupResult || useAuthStore.getState().authGeneration !== voiceRejoin.authGeneration) {
        useConnectionStore.getState().setPendingVoiceRejoin(null);
        return;
      }
      return import('../../services/voice/voiceService').then(({ voiceService }) => {
        if (
          useConnectionStore.getState().pendingVoiceRejoin !== voiceRejoin ||
          generation !== voiceRecoveryGeneration
        )
          return;
        if (
          useAuthStore.getState().authGeneration !== voiceRejoin.authGeneration ||
          voiceService.getVoiceActionGeneration() !== cleanupResult.voiceActionGeneration
        ) {
          useConnectionStore.getState().setPendingVoiceRejoin(null);
          return;
        }
        // A user-initiated join while cleanup/import was pending owns voice.
        const currentVoice = useVoiceStore.getState();
        if (currentVoice.connectionState !== 'disconnected' || currentVoice.activeChannelId) {
          useConnectionStore.getState().setPendingVoiceRejoin(null);
          return;
        }
        const { privacyIntent } = cleanupResult;
        currentVoice.setMuted(privacyIntent.isMuted);
        currentVoice.setDeafened(privacyIntent.isDeafened);
        useConnectionStore.getState().setPendingVoiceRejoin(null);
        return voiceService.joinChannel(voiceRejoin.channelId, privacyIntent.joinType, {
          privacyIntent,
          expectedAuthGeneration: voiceRejoin.authGeneration,
        });
      });
    })
    .catch(() => {
      /* voice module not available */
    });
}

/**
 * Restores application state after the WebSocket reconnects.
 *
 * Performs recovery hydration and the post-outage voice rejoin for recovery
 * phases, restores pending E2EE state after a grace-period reconnect (voice is
 * untouched there — it was never torn down inside the grace window), and
 * resets incomplete connection phases.
 */
function handleReconnected(
  wsService: ReturnType<typeof getWebSocketService>,
  validateEpochsOnReconnect: () => Promise<void>
) {
  const phase = useConnectionStore.getState().phase;
  wsService.setAggressiveReconnect(false);

  if (phase === 'recovery_a' || phase === 'preflight') {
    // Preserve the cleanup-backed ticket through this recovery-owned reset.
    // An external reset discards it, so a later outage cannot replay old voice.
    // Floated intentionally — runRecoveryModule never rejects (it swallows a
    // stale-chunk import failure and triggers self-heal), so this cannot
    // surface as the Uncaught (in promise) seen in the origin-502-storm logs.
    runRecoveryModule(
      () => import('../../services/system/resetService'),
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
    useConnectionStore.getState().reset({ preserveVoiceRejoin: true });
    runRecoveryModule(
      () => import('../../services/system/recoveryService'),
      (m) => m.clearCrashFlag(),
      'clearCrashFlag'
    );
    if (e2eeService.isInitialized) {
      validateEpochsOnReconnect().catch((err) => {
        console.debug('[WebSocket] validate_epochs failed:', err);
      });
      // A holder's pending rewrap queue ran only on the grace-period path
      // below; a sustained outage is at least as likely to have queued one.
      e2eeService.processPendingKeyRequests().catch((err) => {
        console.debug('[E2EE] processPendingKeyRequests failed:', err);
      });
    }
    // Voice was torn down when the grace period expired (preflight entry), so
    // rejoin the channel the user was in. Blips shorter than the grace period
    // never reach preflight and keep their live media session — this fires
    // only after a genuine sustained outage. reset() above preserves this
    // recovery-owned ticket. It survives another outage, but is consumed before
    // starting a join so a duplicate reconnect cannot double-join.
    schedulePendingVoiceRejoin();
    return;
  }

  if (phase === 'grace_period') {
    useConnectionStore.getState().reset({ preserveVoiceRejoin: true });
    // The socket does not replay missed messages. The purge disconnect fence
    // may also have cleared the mounted chat, so request its guarded backfill.
    globalThis.dispatchEvent(new CustomEvent('connection-recovered'));
    if (e2eeService.isInitialized) {
      e2eeService.processPendingKeyRequests().catch(() => {});
      validateEpochsOnReconnect().catch((err) => {
        console.debug('[WebSocket] validate_epochs failed:', err);
      });
    }
    // A normal grace reconnect kept its media session alive. A prior sustained
    // outage may already have torn voice down and queued a rejoin when this
    // newer blip started; resume that ticket under the current generation.
    schedulePendingVoiceRejoin();
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
