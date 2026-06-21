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
import { useAuthStore } from '../stores/authStore';
import { useChatStore } from '../stores/chatStore';
import { useChannelStore } from '../stores/channelStore';
import { getWebSocketService, ConnectionState } from '../services/websocketService';
import { e2eeService } from '../services/e2eeService';
import { apiFetch, safeJson } from '../services/apiClient';
import { useConnectionRecovery } from './useConnectionRecovery';
import { useWebSocketMessages } from './useWebSocketMessages';

export function useWebSocket() {
  const accessToken = useAuthStore((state) => state.accessToken);
  const wsService = useRef(getWebSocketService()).current;

  const setConnectionStatus = useChatStore((s) => s.setConnectionStatus);

  /**
   * Validate key epochs on reconnect — pull-based catch-up for missed key_revocation events.
   * Sends current cached epochs to the server and processes any revocations.
   */
  const validateEpochsOnReconnect = useCallback(async () => {
    // Build epochs map from cached channel keys
    const epochs: Record<string, number> = {};
    // Only validate channels we're subscribed to (active channels)
    const channels = useChannelStore.getState().channels;
    for (const ch of channels) {
      const version = e2eeService.getCurrentKeyVersion(ch.id);
      if (version > 0) {
        epochs[ch.id] = version;
      }
    }

    if (Object.keys(epochs).length === 0) return;

    const res = await apiFetch('/api/v1/e2ee/validate-epochs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ epochs }),
    });

    if (!res.ok) return;
    const data = await safeJson<{
      revocations?: Array<{
        channel_id: string;
        revoked_epoch: number;
        successor_epoch: number;
        reason?: string;
      }>;
    }>(res);
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
  }, []);

  // Track rotation timeouts so they can be cleared on unmount
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const rotationTimers = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());

  // Perform key rotation for a single channel — extracted to reduce nesting depth
  const performKeyRotation = useCallback(async (channelId: string, newEpoch: number) => {
    // Check if another client already rotated to this epoch
    const checkRes = await apiFetch(`/api/v1/e2ee/keys/${channelId}`);
    if (checkRes.ok) {
      const checkData = await safeJson<{ key?: { key_version: number } }>(checkRes);
      if (checkData.key && checkData.key.key_version >= newEpoch) {
        console.debug('[E2EE] Key rotation already done for', channelId, 'epoch', newEpoch);
        e2eeService.invalidateChannelKey(channelId);
        return;
      }
    }

    const channel = useChannelStore.getState().channels.find((c) => c.id === channelId);
    if (!channel) return;

    const membersRes = await apiFetch(`/api/v1/servers/${channel.server_id}/members`);
    if (!membersRes.ok) return;
    const membersData = await safeJson<{ members?: Array<{ user_id: string }> }>(membersRes);
    const members = membersData.members || [];

    const memberPublicKeys = new Map<string, string>();
    for (const member of members) {
      try {
        const pkRes = await apiFetch(`/api/v1/users/${member.user_id}/public-key`);
        if (pkRes.ok) {
          const pkData = await safeJson<{ public_key?: string }>(pkRes);
          if (pkData.public_key) {
            memberPublicKeys.set(member.user_id, pkData.public_key);
          }
        }
      } catch {
        // Skip members whose keys we can't fetch
      }
    }

    if (memberPublicKeys.size === 0) return;

    await e2eeService.rotateChannelKey(channelId, newEpoch, memberPublicKeys);
    console.debug('[E2EE] Key rotation completed for', channelId, 'epoch', newEpoch);
  }, []);

  // Rotation coordinator — listens for key rotation events and distributes new keys
  useEffect(() => {
    const timers = rotationTimers.current;
    const handleKeyRotation = (event: Event) => {
      const { channelId, newEpoch } = (event as CustomEvent).detail;
      if (!channelId || !newEpoch || !e2eeService.isInitialized) return;

      // Non-fatal: another client may handle the rotation (first-response-wins)
      const onRotationError = (error: unknown) =>
        console.debug('[E2EE] Key rotation failed', { channelId, newEpoch, error });

      // Jitter: 0-2s random delay to avoid N clients racing simultaneously
      const jitterMs = Math.random() * 2000;
      // eslint-disable-next-line @eslint-react/web-api-no-leaked-timeout -- Timer is tracked in the effect-local `timers` Set; cleanup at line 159-160 iterates the Set and clearTimeout()s each pending rotation on unmount. The rule doesn't recognize Set.add() as a cleanup mechanism.
      const timerId = setTimeout(() => {
        timers.delete(timerId);
        performKeyRotation(channelId, newEpoch).catch(onRotationError);
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

    const state = wsService.getState();
    // RECONNECTING is included in the connect-path because an in-flight
    // reconnect attempt (a scheduled timer or an awaited ws-ticket fetch) is
    // still using the prior token. Pre-PR behavior cancelled it via the
    // useEffect cleanup's disconnect()→connect(); the new shape preserves
    // that by routing RECONNECTING through connect() (which aborts the
    // in-flight controller in websocketService.connect) AFTER resetting the
    // backoff timer so we don't compound delay on top of the rotation.
    // ERROR follows the same path for the same reason.
    if (
      state === ConnectionState.DISCONNECTED ||
      state === ConnectionState.ERROR ||
      state === ConnectionState.RECONNECTING
    ) {
      wsService.resetReconnectState();
      wsService.connect(accessToken);
    } else {
      // CONNECTED or CONNECTING — reuse the existing socket and refresh the
      // stored token so any subsequent reconnect uses it. (CONNECTING-mid-
      // handshake rotation will self-heal: if the in-flight ticket fetch
      // 401s on the stale token, scheduleReconnect retries with the now-
      // updated this.token; see the 401-specific warn in createConnection.)
      wsService.updateToken(accessToken);
    }
  }, [accessToken, wsService]);

  // Unmount-only cleanup. Kept in a separate effect (no accessToken in
  // deps) so token rotation doesn't trigger a disconnect.
  useEffect(() => {
    return () => {
      wsService.disconnect();
    };
  }, [wsService]);

  // Connection recovery handler
  const handleRecovery = useConnectionRecovery(wsService, validateEpochsOnReconnect);

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

  // Send a message
  const sendMessage = useCallback(
    (channelId: string, content: string) => {
      wsService.sendMessage(channelId, content);
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

  return {
    subscribe,
    unsubscribe,
    sendMessage,
    sendTyping,
    getState,
  };
}
