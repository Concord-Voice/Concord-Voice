/**
 * useChannelSubscription - React hook for automatic channel subscription management
 *
 * Automatically subscribes to a channel when the component mounts and the channel ID changes,
 * and unsubscribes when the component unmounts or channel changes.
 *
 * Uses getWebSocketService() directly (not useWebSocket()) to avoid registering
 * duplicate event handlers — App.tsx already calls useWebSocket() once for all handlers.
 */

import { useEffect } from 'react';
import { getWebSocketService } from '../services/websocketService';
import { useChatStore } from '../stores/chatStore';
import { pendingUnsubscribes, UNSUBSCRIBE_DELAY_MS } from './useDMSubscription';

export function useChannelSubscription(channelId: string | null | undefined) {
  const isConnected = useChatStore((s) => s.isConnected);

  useEffect(() => {
    if (!channelId || !isConnected) {
      return;
    }

    const wsService = getWebSocketService();
    const key = `ch:${channelId}`;

    // Cancel any pending unsubscribe for this channel — the debounce window
    // exists so brief remounts (e.g. Settings overlay) don't generate churn.
    const pending = pendingUnsubscribes.get(key);
    if (pending) {
      clearTimeout(pending);
      pendingUnsubscribes.delete(key);
    } else {
      wsService.subscribe(channelId);
    }

    return () => {
      // eslint-disable-next-line @eslint-react/web-api-no-leaked-timeout -- Timer is tracked in the module-scope `pendingUnsubscribes` Map; cleanup happens either via the clearTimeout at line 31 (when the next subscribe within UNSUBSCRIBE_DELAY_MS wins the race) or by the timer firing and calling pendingUnsubscribes.delete(key). The rule doesn't recognize Map.set() as a cleanup mechanism.
      const timer = setTimeout(() => {
        pendingUnsubscribes.delete(key);
        wsService.unsubscribe(channelId);
      }, UNSUBSCRIBE_DELAY_MS);
      pendingUnsubscribes.set(key, timer);
    };
  }, [channelId, isConnected]);

  return {
    isSubscribed: !!channelId && isConnected,
  };
}
