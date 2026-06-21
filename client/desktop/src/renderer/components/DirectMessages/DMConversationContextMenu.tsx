import React from 'react';
import ContextMenu from '../ui/ContextMenu';
import MuteContextMenuItem from '../Notifications/MuteContextMenuItem';
import { useRotateKey } from '../../hooks/useRotateKey';
import { useDMStore, type DMConversation } from '../../stores/dmStore';
import { useFriendStore } from '../../stores/friendStore';
import { apiFetch } from '../../services/apiClient';
import { initiateDMCall } from '../../services/voiceService/callStateMachine';

interface DMConversationContextMenuProps {
  conversation: DMConversation;
  currentUserId: string;
  position: { x: number; y: number };
  onClose: () => void;
  /**
   * Parent-supplied confirmation handlers. The parent (ConversationList)
   * owns the ConfirmActionModal state so the modal continues rendering after
   * this context menu unmounts. Mirrors the MemberContextMenu / MemberListPanel
   * pattern for Ban + Kick.
   */
  onBlockUser?: (conversation: DMConversation) => void;
  onUnfriend?: (conversation: DMConversation) => void;
  onViewProfile?: (conversation: DMConversation) => void;
}

const DMConversationContextMenu: React.FC<DMConversationContextMenuProps> = ({
  conversation,
  currentUserId,
  position,
  onClose,
  onBlockUser,
  onUnfriend,
  onViewProfile,
}) => {
  const canRotateKey = !conversation.isGroup || conversation.createdBy === currentUserId;

  const { rotateStatus, rotateMessage, handleRotate } = useRotateKey(
    `/api/v1/dm/conversations/${conversation.id}/rotate-key`,
    () => setTimeout(() => onClose(), 800)
  );

  const getRotateLabel = (): string => {
    if (rotateStatus === 'success') return 'Key Rotated!';
    if (rotateStatus === 'error') return rotateMessage;
    return 'Rotate Encryption Key';
  };

  // 1:1 DM identification — used to gate Block / Unfriend items.
  // Personal DM (self-DM) is NOT 1:1 for the purpose of block/unfriend — you
  // can't block yourself.
  const isOneOnOne = !conversation.isGroup && !conversation.isPersonal;
  const peer = isOneOnOne
    ? (conversation.participants.find((p) => p.userId !== currentUserId) ?? null)
    : null;

  // Friend status check — selective Zustand subscription so the menu re-renders
  // only when this specific peer's friendship status changes.
  const peerIsFriend = useFriendStore((s) =>
    peer ? s.friends.some((f) => f.userId === peer.userId) : false
  );

  // Store actions — extracted via useDMStore.getState() at click time so the
  // handlers don't need to subscribe (these don't read state, only invoke).
  const removeConversation = useDMStore((s) => s.removeConversation);
  const clearUnread = useDMStore((s) => s.clearUnread);
  const updateConversation = useDMStore((s) => s.updateConversation);

  const handleMarkAsRead = async () => {
    onClose();
    const previousUnread = conversation.unreadCount;
    // Optimistic local update first — matches DMChatArea.handleFetchComplete.
    clearUnread(conversation.id);
    try {
      const response = await apiFetch(`/api/v1/dm/conversations/${conversation.id}/read`, {
        method: 'POST',
      });
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
    } catch (error) {
      // Roll back the optimistic update so the unread badge re-appears if the
      // server didn't acknowledge. Mirrors the pattern in DMChatArea.
      // (err.cause leak guard — log .message only per observability rule.)
      console.error('Mark as read failed:', error instanceof Error ? error.message : 'unknown');
      if (previousUnread > 0) {
        updateConversation(conversation.id, { unreadCount: previousUnread });
      }
    }
  };

  const handleCloseConversation = () => {
    onClose();
    // Client-side hide. The conversation reappears if the peer sends a new
    // message — there is no server-side "close" for 1:1 DMs. For groups, this
    // hides locally; the user can rejoin via invite link.
    removeConversation(conversation.id);
  };

  const handleBlockUser = () => {
    if (!peer || !onBlockUser) return;
    onClose();
    onBlockUser(conversation);
  };

  const handleUnfriend = () => {
    if (!peer || !onUnfriend) return;
    onClose();
    onUnfriend(conversation);
  };

  const handleViewProfile = () => {
    if (!isOneOnOne || !peer || !onViewProfile) return;
    onViewProfile(conversation);
    onClose();
  };

  return (
    <ContextMenu position={position} onClose={onClose}>
      {/* View Profile — 1:1 DMs only. Opens DMProfileModal via the parent
          (ConversationList) which owns the lifted modal state. Per #1208. */}
      {isOneOnOne && peer && onViewProfile && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
              <circle cx="8" cy="6" r="2.5" stroke="currentColor" strokeWidth="1.5" />
              <path
                d="M3 13c0-2.5 2.25-4 5-4s5 1.5 5 4"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label="View Profile"
          onClick={handleViewProfile}
        />
      )}

      {/* Mark as Read — only visible when there are unread messages */}
      {conversation.unreadCount > 0 && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M13 4l-7 7-3-3"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          }
          label="Mark as Read"
          onClick={() => {
            void handleMarkAsRead();
          }}
        />
      )}

      {/* Mute / Unmute Conversation — always available regardless of rotate-key
          permissions; muting is user-scoped, not gated on group admin role. */}
      <MuteContextMenuItem
        targetType="dm"
        targetId={conversation.id}
        kindLabel="Conversation"
        onAction={onClose}
      />

      {/* Voice Call — #1209 (1:1) + #1219 (group). Shown for any non-personal
          DM: 1:1 and group conversations. Hidden for personal (self-notes) —
          you can't call yourself. The group ring is presence-filtered
          server-side (#1219 B2). */}
      {!conversation.isPersonal && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M3 4a1 1 0 011-1h2.28a1 1 0 01.95.68l1 3a1 1 0 01-.27 1.03L5.6 9.31a8 8 0 003.09 3.09l1.6-1.36a1 1 0 011.03-.27l3 1a1 1 0 01.68.95V15a1 1 0 01-1 1h-1C7.4 16 0 8.6 0 0v-1z"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinejoin="round"
                transform="translate(0.5, 0)"
              />
            </svg>
          }
          label="Voice Call"
          onClick={() => {
            void initiateDMCall(conversation.id).catch((err: unknown) => {
              // initiateDMCall rolls state back to idle on failure (see
              // callStateMachine). .catch here prevents unhandled-rejection
              // at the menu-click boundary (Copilot #1231 finding C8).
              console.error(
                'Voice Call menu item:',
                err instanceof Error ? err.message : 'non-Error thrown'
              );
            });
            onClose();
          }}
        />
      )}

      {/* Block User — 1:1 DMs only. Destructive; routes through parent-owned
          confirmation modal (lifted state per MemberListPanel Ban/Kick
          pattern). */}
      {isOneOnOne && peer && onBlockUser && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <circle cx="8" cy="8" r="6.5" stroke="currentColor" strokeWidth="1.5" />
              <path
                d="M3.5 3.5l9 9"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label="Block User"
          onClick={handleBlockUser}
        />
      )}

      {/* Unfriend — 1:1 DMs only AND only when currently friends with the peer.
          Hiding for non-friends prevents a confusing "Unfriend Bob" item when
          Bob isn't a friend (e.g., a DM from a friend-of-friend invite). */}
      {isOneOnOne && peer && peerIsFriend && onUnfriend && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M5 6a3 3 0 116 0 3 3 0 01-6 0zM2 14c0-2.5 2.5-4.5 6-4.5s6 2 6 4.5"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
              <path
                d="M11 3l3 3M14 3l-3 3"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label="Unfriend"
          onClick={handleUnfriend}
        />
      )}

      {/* Close Conversation — hides the conversation from the local list. For
          1:1 DMs this is a "I don't want to see this in my list" toggle. For
          groups, it's similar (leaving the group properly requires a separate
          UI). For personal DMs (self-notes) this item is hidden — you can't
          close your own notes. */}
      {!conversation.isPersonal && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M3 3l10 10M13 3L3 13"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label="Close Conversation"
          onClick={handleCloseConversation}
        />
      )}

      {canRotateKey && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M11 1.5a3.5 3.5 0 00-3.5 3.5c0 .47.1.92.27 1.33L2 12.1V14h1.9l5.77-5.77c.41.17.86.27 1.33.27A3.5 3.5 0 0014.5 5 3.5 3.5 0 0011 1.5zm0 2a1.5 1.5 0 110 3 1.5 1.5 0 010-3z"
                fill="currentColor"
              />
              <path
                d="M1.5 8.5l2-2"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label={getRotateLabel()}
          disabled={rotateStatus === 'success'}
          onClick={handleRotate}
        />
      )}
    </ContextMenu>
  );
};

export default DMConversationContextMenu;
