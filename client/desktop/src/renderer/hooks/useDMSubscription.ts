/**
 * useDMSubscription - React hook for automatic DM conversation subscription management
 *
 * Mirrors useChannelSubscription but uses subscribeDM/unsubscribeDM for DM conversations.
 */

import { useEffect } from 'react';
import { getWebSocketService } from '../services/websocketService';
import { useChatStore } from '../stores/chatStore';

/**
 * Shared pending-unsubscribe timer map — module-level so useChannelSubscription
 * and useDMSubscription both see it. Keyed on the channel/conversation ID.
 *
 * When a component unmounts (e.g. during a Settings overlay nav), we delay
 * the actual unsubscribe call by UNSUBSCRIBE_DELAY_MS. If the same ID
 * re-subscribes within that window, we cancel the pending unsubscribe and
 * treat the unsubscribe+resubscribe pair as a no-op. This eliminates the
 * subscribe/unsubscribe churn visible in server logs on every navigation.
 */
export const UNSUBSCRIBE_DELAY_MS = 2000;
export const pendingUnsubscribes = new Map<string, ReturnType<typeof setTimeout>>();

/** For tests: clear all pending unsubscribe timers. */
export function __resetPendingUnsubscribes(): void {
  for (const t of pendingUnsubscribes.values()) clearTimeout(t);
  pendingUnsubscribes.clear();
}

export function useDMSubscription(conversationId: string | null | undefined) {
  const isConnected = useChatStore((s) => s.isConnected);

  useEffect(() => {
    if (!conversationId || !isConnected) {
      return;
    }

    const wsService = getWebSocketService();
    const key = `dm:${conversationId}`;

    // If a pending unsubscribe for this ID exists, cancel it — this is a
    // re-subscribe within the debounce window (no-op round-trip).
    const pending = pendingUnsubscribes.get(key);
    if (pending) {
      clearTimeout(pending);
      pendingUnsubscribes.delete(key);
    } else {
      wsService.subscribeDM(conversationId);
    }

    return () => {
      // eslint-disable-next-line @eslint-react/web-api-no-leaked-timeout -- Timer is tracked in the module-scope `pendingUnsubscribes` Map; cleanup happens either via the clearTimeout at line 45 (when the next subscribe within UNSUBSCRIBE_DELAY_MS wins the race) or by the timer firing and calling pendingUnsubscribes.delete(key). The rule doesn't recognize Map.set() as a cleanup mechanism.
      const timer = setTimeout(() => {
        pendingUnsubscribes.delete(key);
        wsService.unsubscribeDM(conversationId);
      }, UNSUBSCRIBE_DELAY_MS);
      pendingUnsubscribes.set(key, timer);
    };
  }, [conversationId, isConnected]);

  return {
    isSubscribed: !!conversationId && isConnected,
  };
}
