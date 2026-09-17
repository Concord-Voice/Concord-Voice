/**
 * useWebSocket - React hook for WebSocket connection management
 *
 * Provides:
 * - Connection state and control
 * - Message sending
 * - Channel subscriptions
 * - Typing indicators
 * - Integration with chat store
 *
 * Orchestrates useConnectionRecovery (phased reconnection) and
 * useWebSocketMessages (all event handler subscriptions).
 */

import { useEffect, useCallback, useRef } from 'react';
import { useAuthStore } from '../../stores/auth/authStore';
import { useE2EEStore } from '../../stores/auth/e2eeStore';
import { useAttestationFailureStore } from '../../stores/auth/attestationFailureStore';
import { useChatStore } from '../../stores/chat/chatStore';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useDMStore, type DMConversation } from '../../stores/chat/dmStore';
import { fetchParticipantPublicKeys } from '../../services/e2ee/participantPublicKeys';
import { getWebSocketService, ConnectionState } from '../../services/messaging/websocketService';
import { e2eeService, type E2EEChannelOperationGuard } from '../../services/e2ee/e2eeService';
import { E2EEEpochClaimStaleError } from '../../services/e2ee/e2eeErrors';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
} from '../../services/system/runtimeServerBase';
import { useConnectionRecovery } from '../voice/useConnectionRecovery';
import { useWebSocketMessages } from './useWebSocketMessages';

const maxEpochsPerValidationRequest = 500;
const memberPublicKeyCacheTTLms = 5_000;

interface MemberPublicKey {
  publicKey: string;
  keyVersion: number;
}

function createRotationOperationGuard(
  authGeneration: number,
  serverSelection: ReturnType<typeof captureRuntimeServerSelection>,
  e2eeGuard: E2EEChannelOperationGuard
): E2EEChannelOperationGuard {
  return {
    assertCurrent() {
      if (
        useAuthStore.getState().authGeneration !== authGeneration ||
        !runtimeServerSelectionIsCurrent(serverSelection)
      ) {
        throw new Error('E2EE rotation context changed');
      }
      e2eeGuard.assertCurrent();
    },
  };
}

export function useWebSocket() {
  const accessToken = useAuthStore((state) => state.accessToken);
  const clientVersionBlocked = useAttestationFailureStore(
    (state) => state.visible && state.code === 'CLIENT_VERSION_TOO_OLD'
  );
  const wsService = useRef(getWebSocketService()).current;
  const epochValidationGenerationRef = useRef(0);

  const setConnectionStatus = useChatStore((s) => s.setConnectionStatus);

  /**
   * Validate key epochs on reconnect — pull-based catch-up for missed key_revocation events.
   * Sends current cached epochs to the server and processes any revocations.
   */
  const validateEpochsOnReconnect = useCallback(async () => {
    const validationGeneration = ++epochValidationGenerationRef.current;
    // Build epochs map from cached channel keys
    const epochs: Record<string, number> = {};
    // Validate all known channel IDs, including cached channels from servers
    // that are not currently rendered after a server switch.
    const channelIds = new Set(Object.values(useChannelStore.getState().channelIdsByServer).flat());
    for (const channelId of channelIds) {
      const version = e2eeService.getCurrentKeyVersion(channelId);
      if (version > 0) {
        epochs[channelId] = version;
      }
    }

    if (Object.keys(epochs).length === 0) return;

    const serverSelection = captureRuntimeServerSelection();
    const authGeneration = useAuthStore.getState().authGeneration;
    const validationIsCurrent = () =>
      e2eeService.isInitialized &&
      epochValidationGenerationRef.current === validationGeneration &&
      useAuthStore.getState().authGeneration === authGeneration &&
      runtimeServerSelectionIsCurrent(serverSelection);
    const epochEntries = Object.entries(epochs);
    for (let start = 0; start < epochEntries.length; start += maxEpochsPerValidationRequest) {
      if (!validationIsCurrent()) return;
      const epochsBatch = Object.fromEntries(
        epochEntries.slice(start, start + maxEpochsPerValidationRequest)
      );
      const res = await apiFetch('/api/v1/e2ee/validate-epochs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ epochs: epochsBatch }),
      });

      if (!validationIsCurrent()) return;
      if (!res.ok) {
        console.debug('[WebSocket] Epoch validation batch failed:', res.status);
        return;
      }
      const data = await safeJson<{
        revocations?: Array<{
          channel_id: string;
          revoked_epoch: number;
          successor_epoch: number;
          reason?: string;
        }>;
        access_lost?: string[];
      }>(res);
      if (!validationIsCurrent()) return;
      const revocations = data.revocations || [];

      for (const rev of revocations) {
        console.debug(
          '[WebSocket] Stale epoch detected:',
          rev.channel_id,
          'revoked:',
          rev.revoked_epoch,
          '→',
          rev.successor_epoch
        );
        e2eeService.invalidateChannelKey(rev.channel_id);

        // Trigger rotation coordinator
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: {
              channelId: rev.channel_id,
              newEpoch: rev.successor_epoch,
              reason: rev.reason,
            },
          })
        );
      }

      // The server returns inaccessible and unknown submitted UUIDs alike,
      // allowing this client-side purge without creating a channel oracle.
      (data.access_lost || []).forEach((channelId) =>
        useChannelStore.getState().removeChannel(channelId)
      );
    }
  }, []);

  // Track rotation timeouts so they can be cleared on unmount
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const rotationTimers = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());
  const memberPublicKeyCacheRef = useRef(
    new Map<string, { expiresAt: number; keys: Map<string, MemberPublicKey> }>()
  );
  const memberPublicKeyFetchesRef = useRef(
    new Map<string, Promise<Map<string, MemberPublicKey> | null>>()
  );

  const getMemberPublicKeys = useCallback(async (serverId: string) => {
    const authGeneration = useAuthStore.getState().authGeneration;
    const serverSelection = captureRuntimeServerSelection();
    const requestIsCurrent = () =>
      useAuthStore.getState().authGeneration === authGeneration &&
      runtimeServerSelectionIsCurrent(serverSelection);
    const cacheKey = `${authGeneration}:${serverSelection.epoch}:${serverId}`;
    const now = Date.now();
    for (const [key, entry] of memberPublicKeyCacheRef.current) {
      if (entry.expiresAt <= now) memberPublicKeyCacheRef.current.delete(key);
    }
    const cached = memberPublicKeyCacheRef.current.get(cacheKey);
    if (cached) return cached.keys;

    const inFlight = memberPublicKeyFetchesRef.current.get(cacheKey);
    if (inFlight) return inFlight;

    const promise = (async () => {
      const memberKeysRes = await apiFetch(`/api/v1/servers/${serverId}/member-public-keys`);
      if (!memberKeysRes.ok || !requestIsCurrent()) return null;
      const memberKeysData = await safeJson<{
        members?: Array<{ user_id: string; public_key: string; key_version: number }>;
      }>(memberKeysRes);
      if (!requestIsCurrent()) return null;
      const keys = new Map(
        (memberKeysData.members || []).map(({ user_id, public_key, key_version }) => [
          user_id,
          { publicKey: public_key, keyVersion: key_version },
        ])
      );
      memberPublicKeyCacheRef.current.set(cacheKey, {
        expiresAt: Date.now() + memberPublicKeyCacheTTLms,
        keys,
      });
      return keys;
    })();
    memberPublicKeyFetchesRef.current.set(cacheKey, promise);
    try {
      return await promise;
    } finally {
      if (memberPublicKeyFetchesRef.current.get(cacheKey) === promise) {
        memberPublicKeyFetchesRef.current.delete(cacheKey);
      }
    }
  }, []);

  // Establish the successor epoch of a DM conversation. A DM has no server to
  // resolve members through, so the channel branch below returned before it
  // posted anything and a DM revocation never got its successor — every
  // participant then held only the revoked epoch (prod, 2026-09-17). The
  // successor batch must wrap for EVERY participant, so a participant whose
  // public key cannot be fetched ends the attempt: the server refuses a
  // partial batch, and a partial claim that succeeded would strand the rest.
  const performDMKeyRotation = useCallback(
    async (
      conversation: DMConversation,
      newEpoch: number,
      operationGuard: E2EEChannelOperationGuard
    ) => {
      const { keys, versions, missing } = await fetchParticipantPublicKeys(
        conversation.participants.map((p) => p.userId)
      );
      operationGuard.assertCurrent();
      if (missing.length > 0 || keys.size === 0) {
        console.debug('[E2EE] DM key rotation skipped: participant key unavailable', {
          conversationId: conversation.id,
          missing: missing.length,
        });
        return;
      }
      if (versions.size !== keys.size) {
        // Mirror rotateDMKey's guard: this is an established-epoch successor
        // claim, so every wrap must carry its recipient's identity-key version
        // to arm the server's #2420 freshness check. A versionless recipient
        // would be posted unchecked; skip this cycle (the next cue retries).
        console.debug('[E2EE] DM key rotation skipped: participant key version unavailable', {
          conversationId: conversation.id,
        });
        return;
      }
      const wrappedKeyVersions = Object.fromEntries(versions);
      try {
        await e2eeService.rotateChannelKey(
          conversation.id,
          newEpoch,
          keys,
          wrappedKeyVersions,
          operationGuard
        );
      } catch (err) {
        // The claimed epoch was not the conversation's next one. The refusal
        // names the real current epoch, so claim ITS successor — once. A
        // second refusal means the epoch is moving under us; the next
        // revocation or key miss cues another attempt with fresh numbers.
        if (!(err instanceof E2EEEpochClaimStaleError)) throw err;
        // The conversation is already at or past the epoch this cue asked
        // for: the goal is met, another claimant won. Re-keying on top would
        // rotate everyone a second time for nothing.
        if (err.currentVersion >= newEpoch) return;
        const successor = err.currentVersion + 1;
        operationGuard.assertCurrent();
        await e2eeService.rotateChannelKey(
          conversation.id,
          successor,
          keys,
          wrappedKeyVersions,
          operationGuard
        );
      }
      console.debug('[E2EE] DM key rotation completed for', conversation.id);
    },
    []
  );

  // Perform key rotation for a single channel — extracted to reduce nesting depth
  const performKeyRotation = useCallback(
    async (channelId: string, newEpoch: number, operationGuard: E2EEChannelOperationGuard) => {
      operationGuard.assertCurrent();
      // Check if another client already rotated to this epoch
      const checkRes = await apiFetch(`/api/v1/e2ee/keys/${channelId}`);
      operationGuard.assertCurrent();
      if (checkRes.ok) {
        const checkData = await safeJson<{ key?: { key_version: number } }>(checkRes);
        operationGuard.assertCurrent();
        if (checkData.key && checkData.key.key_version >= newEpoch) {
          console.debug('[E2EE] Key rotation already done for', channelId, 'epoch', newEpoch);
          e2eeService.invalidateChannelKey(channelId);
          return;
        }
      }

      const dmConversation = useDMStore
        .getState()
        .conversations.find((conversation) => conversation.id === channelId);
      if (dmConversation) {
        await performDMKeyRotation(dmConversation, newEpoch, operationGuard);
        return;
      }

      const channelState = useChannelStore.getState();
      const serverId =
        channelState.channels.find((channel) => channel.id === channelId)?.server_id ??
        Object.entries(channelState.channelIdsByServer).find(([, channelIds]) =>
          channelIds.includes(channelId)
        )?.[0];
      if (!serverId) return;

      const memberPublicKeys = await getMemberPublicKeys(serverId);
      operationGuard.assertCurrent();
      if (!memberPublicKeys || memberPublicKeys.size === 0) return;

      const publicKeys = new Map(
        [...memberPublicKeys].map(([userId, key]) => [userId, key.publicKey])
      );
      const wrappedKeyVersions = Object.fromEntries(
        [...memberPublicKeys].map(([userId, key]) => [userId, key.keyVersion])
      );
      await e2eeService.rotateChannelKey(
        channelId,
        newEpoch,
        publicKeys,
        wrappedKeyVersions,
        operationGuard
      );
      console.debug('[E2EE] Key rotation completed for', channelId, 'epoch', newEpoch);
    },
    [getMemberPublicKeys, performDMKeyRotation]
  );

  // Rotation coordinator — listens for key rotation events and distributes new keys
  useEffect(() => {
    const timers = rotationTimers.current;
    const handleKeyRotation = (event: Event) => {
      const { channelId, newEpoch } = (event as CustomEvent).detail;
      if (!channelId || !newEpoch || !e2eeService.isInitialized) return;
      const authGeneration = useAuthStore.getState().authGeneration;
      const serverSelection = captureRuntimeServerSelection();
      const e2eeGuard = e2eeService.createChannelRotationGuard(channelId);
      const operationGuard = createRotationOperationGuard(
        authGeneration,
        serverSelection,
        e2eeGuard
      );

      // Non-fatal: another client may handle the rotation (first-response-wins)
      const onRotationError = (error: unknown) =>
        console.debug('[E2EE] Key rotation failed', { channelId, newEpoch, error });

      // Jitter: 0-2s random delay to avoid N clients racing simultaneously
      const jitterMs = Math.random() * 2000;
      // eslint-disable-next-line @eslint-react/web-api-no-leaked-timeout -- Timer is tracked in the effect-local `timers` Set; cleanup at line 159-160 iterates the Set and clearTimeout()s each pending rotation on unmount. The rule doesn't recognize Set.add() as a cleanup mechanism.
      const timerId = setTimeout(() => {
        timers.delete(timerId);
        if (
          useAuthStore.getState().authGeneration !== authGeneration ||
          !runtimeServerSelectionIsCurrent(serverSelection)
        )
          return;
        performKeyRotation(channelId, newEpoch, operationGuard).catch(onRotationError);
      }, jitterMs);
      timers.add(timerId);
    };

    globalThis.addEventListener('e2ee-key-rotation', handleKeyRotation);
    return () => {
      globalThis.removeEventListener('e2ee-key-rotation', handleKeyRotation);
      // Clear any pending rotation timers on unmount
      for (const id of timers) clearTimeout(id);
      timers.clear();
    };
  }, [performKeyRotation]);

  // Manage WebSocket lifecycle in response to auth-state changes.
  //
  // The previous shape returned `wsService.disconnect()` from a cleanup
  // function that ran on every accessToken change, which tore down and
  // re-established the WS on every JWT refresh (every ~14 minutes). Each
  // cycle re-emitted subscribe frames for all channels/DMs and stopped and
  // restarted the message queue. The server-side handshake doesn't need
  // re-authentication on token rotation — the WS frame is already
  // authenticated — so this churn was wasted work.
  //
  // New shape: connect once when a token first becomes available, push
  // future tokens via wsService.updateToken (which mutates the stored
  // value without disrupting the open socket — the new token is read by
  // the next ws-ticket fetch on reconnect), and disconnect only when the
  // token disappears (logout) or the hook unmounts.
  useEffect(() => {
    if (!accessToken) {
      wsService.disconnect();
      return;
    }
    // A ws-ticket version denial is terminal until a later authoritative
    // client-config response retracts the floor. Re-running this effect when
    // that store flag clears restarts an ERROR socket with the current token.
    if (clientVersionBlocked) {
      wsService.disconnect();
      return;
    }

    const state = wsService.getState();
    // RECONNECTING and CONNECTING both can carry an in-flight ws-ticket
    // request with the prior token. Restart them with the fresh token so
    // startup/session-restore races don't burn a stale ticket request first.
    // ERROR follows the same path for the same reason.
    if (
      state === ConnectionState.DISCONNECTED ||
      state === ConnectionState.ERROR ||
      state === ConnectionState.RECONNECTING ||
      state === ConnectionState.CONNECTING
    ) {
      wsService.resetReconnectState();
      wsService.connect(accessToken);
    } else {
      // CONNECTED: keep the open socket and refresh the stored token so any
      // future reconnect uses it.
      wsService.updateToken(accessToken);
    }
  }, [accessToken, clientVersionBlocked, wsService]);

  // Unmount-only cleanup. Kept in a separate effect (no accessToken in
  // deps) so token rotation doesn't trigger a disconnect.
  useEffect(() => {
    return () => {
      wsService.disconnect();
    };
  }, [wsService]);

  // Connection recovery handler
  const handleRecovery = useConnectionRecovery(wsService, validateEpochsOnReconnect);

  // Run the pending rewrap queue once this device is a reachable holder. A
  // peer who opened a conversation while this device was away enrolled a
  // request that only a holder can serve, and nothing ran the queue at login:
  // it ran on a key_needed push (server channels; DMs since this change) or
  // on a reconnect after an outage. Keyed on both flags because login
  // publishes the token — and connects the socket — BEFORE the keys unwrap,
  // so neither transition alone can see the other.
  const e2eeReady = useE2EEStore((s) => s.ready);
  const wsConnected = useChatStore((s) => s.isConnected);
  useEffect(() => {
    if (!e2eeReady || !wsConnected) return;
    e2eeService.processPendingKeyRequests().catch((err) => {
      console.debug('[WebSocket] processPendingKeyRequests failed:', err);
    });
  }, [e2eeReady, wsConnected]);

  // Connection state listener
  useEffect(() => {
    const unsub = wsService.onConnectionChange((state) => {
      const isConnected = state === ConnectionState.CONNECTED;
      const connectionInfo = wsService.getConnectionInfo();

      // Map WS states to simplified UI states
      let uiState: 'connected' | 'connecting' | 'disconnected' = 'disconnected';
      if (state === ConnectionState.CONNECTED) uiState = 'connected';
      else if (state === ConnectionState.CONNECTING || state === ConnectionState.RECONNECTING)
        uiState = 'connecting';

      setConnectionStatus(isConnected, connectionInfo?.clientId, uiState);

      console.debug('[WebSocket] state →', state, connectionInfo);

      handleRecovery(state);
    });
    return unsub;
  }, [wsService, setConnectionStatus, handleRecovery]);

  // Message handlers (self-contained hook)
  useWebSocketMessages(wsService);

  // Subscribe to a channel
  const subscribe = useCallback(
    (channelId: string) => {
      wsService.subscribe(channelId);
    },
    [wsService]
  );

  // Unsubscribe from a channel
  const unsubscribe = useCallback(
    (channelId: string) => {
      wsService.unsubscribe(channelId);
    },
    [wsService]
  );

  // Send typing indicator
  const sendTyping = useCallback(
    (channelId: string, isTyping: boolean) => {
      wsService.sendTypingIndicator(channelId, isTyping);
    },
    [wsService]
  );

  // Get connection state
  const getState = useCallback(() => {
    return wsService.getState();
  }, [wsService]);

  // #2843: `sendMessage` deliberately absent. It used to be exported here and
  // called wsService.sendMessage(channelId, content) with no encryption and no
  // keyVersion — residue from before #1024, when an unencrypted channel could
  // legitimately send plaintext. Nothing consumed it (App.tsx discards this
  // hook's return value), but the name invited exactly the wrong call, and this
  // hook holds no e2eeService key material, so there is no correct version of
  // it to keep. Encrypted sends go through useMessaging / dmMessageSender.
  return {
    subscribe,
    unsubscribe,
    sendTyping,
    getState,
  };
}
