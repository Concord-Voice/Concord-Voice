import { useState, useCallback } from 'react';
import { useFriendStore } from '../stores/friendStore';
import { useUserStore } from '../stores/userStore';

/**
 * Lifecycle of an in-flight "Send Friend Request" action. `sent` is a transient
 * confirmation state — the underlying friendship/pending state (which drives
 * `label` / `canSend`) updates independently via the friendStore's optimistic
 * write + WebSocket sync, so the button naturally settles into "Request
 * Pending" after a successful send.
 */
export type FriendRequestSendStatus = 'idle' | 'sending' | 'sent' | 'error';

export interface FriendRequestState {
  isFriend: boolean;
  hasPendingRequest: boolean;
  /**
   * Whether the friend-request affordance should render at all. False only for
   * self. NOTE: the privacy gate (`allow_friend_requests_from`) is intentionally
   * NOT applied here — that setting does not exist server-side yet and is
   * tracked as a follow-up. Until then the affordance is visible for every
   * non-self user (the server still rejects disallowed requests).
   */
  visible: boolean;
  /** Actionable right now: not self, not already friends, no pending, not mid-send. */
  canSend: boolean;
  status: FriendRequestSendStatus;
  errorMessage: string | null;
  /** Discord-style relationship label: Friends / Request Pending / Send Friend Request. */
  label: string;
  send: () => Promise<void>;
}

/**
 * Shared state + action for the "Send Friend Request" affordance. Consumed by
 * every surface that offers it (member context menu, member profile card, chat
 * username/avatar profile card) so the relationship-state logic and labels
 * never drift between them.
 */
export function useFriendRequestState(userId: string | undefined): FriendRequestState {
  const currentUserId = useUserStore((s) => s.user?.id);
  const isFriend = useFriendStore((s) =>
    userId ? s.friends.some((f) => f.userId === userId) : false
  );
  const hasPendingRequest = useFriendStore((s) =>
    userId ? s.pendingRequests.some((r) => r.fromUserId === userId || r.toUserId === userId) : false
  );

  const [status, setStatus] = useState<FriendRequestSendStatus>('idle');
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  const isSelf = !!userId && userId === currentUserId;
  const visible = !!userId && !isSelf;
  const canSend = visible && !isFriend && !hasPendingRequest && status !== 'sending';

  const label = (() => {
    if (isFriend) return 'Friends';
    if (hasPendingRequest) return 'Request Pending';
    return 'Send Friend Request';
  })();

  const send = useCallback(async () => {
    if (!userId) return;
    setStatus('sending');
    setErrorMessage(null);
    try {
      await useFriendStore.getState().sendRequest(userId);
      setStatus('sent');
    } catch (error) {
      setStatus('error');
      setErrorMessage(error instanceof Error ? error.message : 'Failed to send friend request');
    }
  }, [userId]);

  return {
    isFriend,
    hasPendingRequest,
    visible,
    canSend,
    status,
    errorMessage,
    label,
    send,
  };
}
