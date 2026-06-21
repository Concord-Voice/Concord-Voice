/**
 * useServerChannelSubscriptions - Manages server-level unread notifications
 *
 * Two responsibilities:
 * 1. Subscribe to ALL user's servers for lightweight unread_notify pings
 *    (enables unread dots on server icons across servers).
 * 2. Fetch per-channel unread counts for the active server
 *    (enables unread badges on channel names).
 */

import { useEffect, useRef } from 'react';
import { useServerStore } from '../stores/serverStore';
import { useChatStore } from '../stores/chatStore';
import { useChannelStore } from '../stores/channelStore';
import { useUnreadStore } from '../stores/unreadStore';
import { getWebSocketService } from '../services/websocketService';
import { apiFetch } from '../services/apiClient';
import { errorMessage } from '../utils/redactError';

export function useServerChannelSubscriptions() {
  const servers = useServerStore((s) => s.servers);
  const activeServerId = useServerStore((s) => s.activeServerId);
  const isConnected = useChatStore((s) => s.isConnected);
  const setInitialUnreads = useUnreadStore((s) => s.setInitialUnreads);
  const setInitialServerUnreads = useUnreadStore((s) => s.setInitialServerUnreads);
  const markServerUnread = useUnreadStore((s) => s.markServerUnread);
  const clearServerUnread = useUnreadStore((s) => s.clearServerUnread);
  const subscribedServersRef = useRef<Set<string>>(new Set());

  // Subscribe to ALL servers for unread notifications + fetch server-level unread status.
  // Server subscriptions are long-lived (persist until WS disconnect or server leave).
  // The ref-based diffing handles additions/removals without full unsubscribe cycles.
  // No cleanup unsubscribe — WS reconnect resubscribes via resubscribeChannels(),
  // and logout disconnects the WS entirely. Removing cleanup prevents StrictMode
  // double-mount from causing 3x subscribe/unsubscribe thrashing (~60 events → ~17).
  const unreadsFetchedRef = useRef(false);

  useEffect(() => {
    if (!isConnected || servers.length === 0) return;

    const wsService = getWebSocketService();
    const currentServerIds = new Set(servers.map((s) => s.id));

    // Subscribe to new servers
    for (const serverId of currentServerIds) {
      if (!subscribedServersRef.current.has(serverId)) {
        wsService.subscribeServer(serverId);
        subscribedServersRef.current.add(serverId);
      }
    }

    // Unsubscribe from removed servers (e.g. user left a server)
    for (const serverId of subscribedServersRef.current) {
      if (!currentServerIds.has(serverId)) {
        wsService.unsubscribeServer(serverId);
        subscribedServersRef.current.delete(serverId);
      }
    }

    // Fetch server-level unread status once per connection (not on every re-render)
    if (!unreadsFetchedRef.current) {
      unreadsFetchedRef.current = true;
      (async () => {
        try {
          const res = await apiFetch('/api/v1/servers/unread-status');
          if (res.ok) {
            const data = await res.json();
            setInitialServerUnreads(data.server_ids || []);
          }
        } catch (err) {
          console.error('Failed to fetch server unread status:', errorMessage(err));
        }
      })();
    }
  }, [isConnected, servers, setInitialServerUnreads]);

  // Reset the unread fetch flag when WS disconnects so it refetches on reconnect
  useEffect(() => {
    if (!isConnected) {
      unreadsFetchedRef.current = false;
    }
  }, [isConnected]);

  // Fetch per-channel unreads for the active server.
  // Ref guard prevents StrictMode double-mount from firing duplicate HTTP requests.
  const unreadFetchServerRef = useRef<string | null>(null);

  useEffect(() => {
    if (!isConnected || !activeServerId) return;

    // NOTE: Do NOT eagerly clearServerUnread here. The server dot should
    // only clear when all channels are actually read (handled in ChannelList).

    // StrictMode dedup: skip if we already fetched for this server
    if (unreadFetchServerRef.current === activeServerId) return;
    unreadFetchServerRef.current = activeServerId;

    // Guard against stale responses from rapid server switches or WS reconnects.
    let aborted = false;

    const fetchUnreads = async () => {
      try {
        const res = await apiFetch(`/api/v1/servers/${activeServerId}/unread`);
        if (aborted) return;
        if (res.ok) {
          const data = await res.json();
          if (aborted) return;
          const counts = new Map<string, number>();

          // Filter out the currently active channel — user is already viewing it,
          // so it shouldn't show as unread. This prevents the race condition where
          // setInitialUnreads overwrites a clearUnread that already ran.
          const currentActiveChannel = useChannelStore.getState().activeChannelId;
          for (const entry of data.unreads || []) {
            if (entry.unread_count > 0 && entry.channel_id !== currentActiveChannel) {
              counts.set(entry.channel_id, entry.unread_count);
            }
          }
          setInitialUnreads(counts);

          // Update server dot based on actual remaining unreads
          if (counts.size > 0) {
            markServerUnread(activeServerId);
          } else {
            clearServerUnread(activeServerId);
          }
        }
      } catch (err) {
        console.error('Failed to fetch unread counts:', errorMessage(err));
      }
    };
    fetchUnreads();

    // No clearAll() here — setInitialUnreads already replaces the whole map,
    // and clearing eagerly during WS reconnects can cause stale DB data to
    // resurface as phantom unread badges on already-read channels.
    return () => {
      aborted = true;
    };
  }, [activeServerId, isConnected, setInitialUnreads, markServerUnread, clearServerUnread]);
}
