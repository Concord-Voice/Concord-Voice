import { useEffect, useRef } from 'react';
import { useMemberStore } from '../stores/memberStore';
import { useServerStore } from '../stores/serverStore';
import { useChannelStore } from '../stores/channelStore';
import { useUnreadStore } from '../stores/unreadStore';
import { apiFetch } from '../services/apiClient';
import { errorMessage } from '../utils/redactError';

/**
 * Watches the user's self-status and refetches unread counts for the
 * currently-active server when DND turns off.
 *
 * Rationale: while DND is on, `unread_notify` events are dropped (see
 * useWebSocketMessages.ts). Counts get stale. When the user toggles DND
 * off, refresh the visible server so they see the right state without
 * waiting for new traffic. Per the issue spec we only refresh the active
 * server — refreshing all subscribed servers on every DND toggle would be
 * a thundering-herd of requests for users in many servers.
 *
 * Mount this hook once near the root of the authenticated app shell.
 */
export function useDNDTransitionRefresh(): void {
  // useRef holds the previous status across renders so we can detect the
  // transition. Reading from state on each effect run won't tell us which
  // direction the change went.
  const prevStatusRef = useRef(useMemberStore.getState().selfStatus);

  useEffect(() => {
    // Subscribe to selfStatus changes directly. This bypasses React's
    // batched-render cycle so we react to every status flip, even if
    // several stores update in the same tick.
    const unsub = useMemberStore.subscribe((state) => {
      const prev = prevStatusRef.current;
      const next = state.selfStatus;
      prevStatusRef.current = next;
      // Only care about the dnd → anything-else transition.
      if (prev !== 'dnd' || next === 'dnd') return;

      const activeServerId = useServerStore.getState().activeServerId;
      if (!activeServerId) return;

      // Same fetch shape as useServerChannelSubscriptions. We deliberately
      // do NOT wipe the existing counts before the fetch — keeping the
      // current state until the response lands avoids a flicker where
      // badges briefly disappear then reappear.
      apiFetch(`/api/v1/servers/${activeServerId}/unread`)
        .then(async (res) => {
          if (!res.ok) return;
          const data = (await res.json()) as {
            unreads?: Array<{ channel_id: string; unread_count: number }>;
          };
          const counts = new Map<string, number>();
          const activeChannelId = useChannelStore.getState().activeChannelId;
          for (const entry of data.unreads ?? []) {
            if (entry.unread_count > 0 && entry.channel_id !== activeChannelId) {
              counts.set(entry.channel_id, entry.unread_count);
            }
          }
          useUnreadStore.getState().setInitialUnreads(counts);
          if (counts.size > 0) useUnreadStore.getState().markServerUnread(activeServerId);
          else useUnreadStore.getState().clearServerUnread(activeServerId);
        })
        .catch((err) => {
          console.warn('Failed to refresh unread counts after DND toggle-off:', errorMessage(err));
        });
    });
    return unsub;
  }, []);
}
