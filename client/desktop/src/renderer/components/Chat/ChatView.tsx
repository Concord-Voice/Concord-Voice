import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import MessageList, { type MessageListHandle } from './MessageList';
import MessageInput from './MessageInput';
import TypingIndicator from './TypingIndicator';
import PinnedMessagesPanel from './PinnedMessagesPanel';
import SearchPanel from './SearchPanel';
import { pinMessage, unpinMessage, getChannelPins } from '../../services/messaging/pinService';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useAuthStore } from '../../stores/auth/authStore';
import { useChannelSubscription } from '../../hooks/messaging/useChannelSubscription';
import { errorMessage } from '../../utils/runtime/redactError';
import { useMessageFetch } from '../../hooks/messaging/useMessageFetch';
import { useChatController } from '../../hooks/messaging/useChatController';
import { useUnreadStore } from '../../stores/chat/unreadStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { isChannelMuted } from '../../stores/ui/notificationPrefsStore';
import { useExpirationPolicy } from '../../hooks/messaging/useExpirationPolicy';
import MessageExpirationEditor from '../Expiration/MessageExpirationEditor';
import MessageExpirationPolicySummary from '../Expiration/MessageExpirationPolicySummary';
import Modal from '../ui/Modal';
import PurgeMessagesModal from '../Purge/PurgeMessagesModal';
import {
  MANAGE_ALL_MESSAGES,
  MANAGE_OWN_MESSAGES,
  hasPermission,
} from '../../utils/policy/permissions';
import type { ChatContext, MessageWithStatus } from '../../types/chat';
import './ChatView.css';

interface ChannelPurgeTarget {
  id: string;
  name: string;
  selfScopeOnly: boolean;
}

const ChatView: React.FC = () => {
  const activeChannelId = useChannelStore((s) => s.activeChannelId);
  const channels = useChannelStore((s) => s.channels);
  const user = useUserStore((s) => s.user);
  const activeServerId = useServerStore((s) => s.activeServerId);
  const authGeneration = useAuthStore((s) => s.authGeneration);
  const [showExpirationEditor, setShowExpirationEditor] = useState(false);
  const [purgeTarget, setPurgeTarget] = useState<ChannelPurgeTarget | null>(null);
  const expirationSummaryRef = useRef<HTMLElement>(null);

  // Subscribe to active channel for full message delivery
  useChannelSubscription(activeChannelId);

  // Shared fetch/decrypt/paginate logic
  const { messages, isLoading, hasMore, error, handleLoadMore } = useMessageFetch(activeChannelId, {
    type: 'channel',
  });

  // Active channel info
  const activeChannel = channels.find((c) => c.id === activeChannelId);
  const currentUserId = user?.id || '';
  const expirationScope =
    activeChannelId && activeChannel?.type === 'text'
      ? { kind: 'channel' as const, id: activeChannelId }
      : null;
  const expiration = useExpirationPolicy(expirationScope, activeChannel?.server_id);
  const channelPermissions = usePermissionStore((s) =>
    activeChannelId ? s.channelPermissions[activeChannelId] : undefined
  );
  const serverPermissions = usePermissionStore((s) =>
    activeChannel?.server_id ? s.serverPermissions[activeChannel.server_id] : undefined
  );
  const purgePermissions = channelPermissions ?? serverPermissions ?? 0n;
  const canPurge =
    hasPermission(purgePermissions, MANAGE_OWN_MESSAGES) ||
    hasPermission(purgePermissions, MANAGE_ALL_MESSAGES);
  const purgeSelfScopeOnly = canPurge && !hasPermission(purgePermissions, MANAGE_ALL_MESSAGES);

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- close local controls when their channel or auth owner changes
    setShowExpirationEditor(false);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- close local controls when their channel or auth owner changes
    setPurgeTarget(null);
  }, [activeChannelId, authGeneration]);

  useEffect(() => {
    if (!purgeTarget) return;
    if (
      !canPurge ||
      purgeTarget.id !== activeChannelId ||
      purgeTarget.selfScopeOnly !== purgeSelfScopeOnly
    ) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- close destructive consent when the permission-derived scope no longer matches the consented scope
      setPurgeTarget(null);
    }
  }, [activeChannelId, canPurge, purgeSelfScopeOnly, purgeTarget]);

  const openExpirationEditor = () => {
    if (!expirationScope || !expiration.canEdit) return;
    setShowExpirationEditor(true);
    void expiration.onRefresh();
  };

  const reviewExpiration = () => {
    if (expiration.canEdit) {
      openExpirationEditor();
      return;
    }
    expirationSummaryRef.current?.focus();
  };

  // Chat controller — unified send/edit/delete/reply/pin/typing
  const ctx: ChatContext = useMemo(
    () => ({
      type: 'channel' as const,
      id: activeChannelId || '',
      serverId: activeChannel?.server_id ?? activeServerId ?? undefined,
    }),
    [activeChannelId, activeChannel?.server_id, activeServerId]
  );

  const {
    sendMessage,
    editMessage,
    deleteMessage,
    replyingTo,
    handleReply,
    cancelReply,
    canPin,
    sendTyping,
  } = useChatController(ctx);

  // Called by MessageList on unmount when the user left with unseen messages
  const handleUnseenOnLeave = useCallback(
    (count: number) => {
      const channelId = activeChannelId;
      if (!channelId || count <= 0) return;
      useUnreadStore.getState().setUnreadCount(channelId, count);
      const serverId = useServerStore.getState().activeServerId;
      // This is a concrete per-channel count, so it is mute-resolved: light the
      // server dot only when THIS channel is not effectively muted, and mark it
      // precise so an explicit channel-unmute under a muted server still shows
      // the dot (channel-wins). A muted channel's leftover unread must not light
      // the server (#84 acceptance criterion; epic #1029 close audit, P2 review).
      if (serverId && !isChannelMuted(channelId, serverId)) {
        useUnreadStore.getState().markServerUnread(serverId, true);
      }
    },
    [activeChannelId]
  );

  // Scroll handling
  const messageListRef = useRef<MessageListHandle>(null);

  const handleScrollToMessage = useCallback((messageId: string) => {
    messageListRef.current?.scrollToMessage(messageId);
  }, []);

  // Auto-scroll to bottom when the reply bar appears
  useEffect(() => {
    if (replyingTo) {
      messageListRef.current?.scrollToBottomIfNear();
    }
  }, [replyingTo]);

  // Send message adapter (MessageInput callback signature)
  const handleSendMessage = useCallback(
    (
      content: string,
      mentionMeta?: string,
      replyToId?: string,
      attachmentIds?: string[],
      attachments?: import('../../types/chat').AttachmentSummary[],
      gifSlug?: string
    ) => {
      if (!activeChannelId) return;
      sendMessage(content, { mentionMeta, replyToId, attachmentIds, attachments, gifSlug });
    },
    [activeChannelId, sendMessage]
  );

  // Pinned messages panel state — ChatView-specific UI (count badge + panel)
  const [showPinnedPanel, setShowPinnedPanel] = useState(false);
  const [showSearchPanel, setShowSearchPanel] = useState(false);
  const [pinnedCount, setPinnedCount] = useState(0);
  const [pinRefreshKey, setPinRefreshKey] = useState(0);
  const pinGenerationRef = useRef(0);

  useEffect(() => {
    return () => {
      pinGenerationRef.current += 1;
    };
  }, [activeChannelId]);

  // Toggle search panel via keyboard shortcut (#176)
  useEffect(() => {
    const handler = () => setShowSearchPanel((prev) => !prev);
    globalThis.addEventListener('concord:toggle-search', handler);
    return () => globalThis.removeEventListener('concord:toggle-search', handler);
  }, []);

  // Fetch pin count from API when channel changes
  useEffect(() => {
    if (!activeChannelId) return;
    let cancelled = false;
    const observedGeneration = pinGenerationRef.current;
    getChannelPins(activeChannelId)
      .then((pins) => {
        if (!cancelled && pinGenerationRef.current === observedGeneration)
          setPinnedCount(pins.length);
      })
      .catch(() => {
        if (!cancelled && pinGenerationRef.current === observedGeneration) setPinnedCount(0);
      });
    return () => {
      cancelled = true;
    };
  }, [activeChannelId, pinRefreshKey]);

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent<{ scopeId?: string | null }>).detail;
      if (detail?.scopeId !== activeChannelId && detail?.scopeId !== null) return;
      pinGenerationRef.current += 1;
      setPinnedCount(0);
      setPinRefreshKey((value) => value + 1);
    };
    globalThis.addEventListener('messages-purged', handler);
    return () => globalThis.removeEventListener('messages-purged', handler);
  }, [activeChannelId]);

  // Pin toggle with local count tracking (wraps hook's generic handler)
  const handlePinToggle = useCallback(
    async (message: MessageWithStatus) => {
      if (!activeChannelId) return;
      const observedGeneration = pinGenerationRef.current;
      try {
        if (message.pinned_at) {
          await unpinMessage(message.id);
          if (pinGenerationRef.current === observedGeneration) {
            setPinnedCount((c) => Math.max(0, c - 1));
          }
        } else {
          await pinMessage(message.id);
          if (pinGenerationRef.current === observedGeneration) {
            setPinnedCount((c) => c + 1);
          }
        }
      } catch (err) {
        console.error('Failed to toggle pin:', errorMessage(err));
      }
    },
    [activeChannelId]
  );

  if (!activeChannelId) {
    return null;
  }

  return (
    <div className="chat-view">
      <div className="chat-header">
        {activeChannel?.emoji ? (
          <span className="chat-header-emoji">{activeChannel.emoji}</span>
        ) : (
          <svg
            className="chat-header-icon"
            width="20"
            height="20"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <line x1="4" y1="9" x2="20" y2="9" />
            <line x1="4" y1="15" x2="20" y2="15" />
            <line x1="10" y1="3" x2="8" y2="21" />
            <line x1="16" y1="3" x2="14" y2="21" />
          </svg>
        )}
        <span className="chat-header-name">{activeChannel?.name || 'Channel'}</span>
        {expirationScope && expiration.canEdit && (
          <button
            type="button"
            className="chat-header-search-button message-expiration-header-action"
            onClick={openExpirationEditor}
          >
            Message expiration
          </button>
        )}
        <button
          className="chat-header-search-button"
          onClick={() => setShowSearchPanel(!showSearchPanel)}
          title="Search messages"
          aria-label="Search messages"
          aria-expanded={showSearchPanel}
        >
          <svg
            width="18"
            height="18"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
        </button>
        <button
          className="chat-header-pin-button"
          onClick={() => setShowPinnedPanel(!showPinnedPanel)}
          title="Pinned messages"
          aria-label="Pinned messages"
          aria-expanded={showPinnedPanel}
        >
          <svg width="18" height="18" viewBox="0 0 16 16" fill="none">
            <path
              d="M9 2L5 8h3v6l4-6H9V2z"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
          {pinnedCount > 0 && <span className="pin-count-badge">{pinnedCount}</span>}
        </button>
      </div>

      {expirationScope && (
        <MessageExpirationPolicySummary
          ref={expirationSummaryRef}
          policy={expiration.policy}
          policyState={expiration.policyState}
          showChangedNotice={expiration.showChangedNotice}
          onReview={reviewExpiration}
          onDismissNotice={expiration.onDismissNotice}
          onManageMessages={
            canPurge && activeChannel
              ? () =>
                  setPurgeTarget({
                    id: activeChannel.id,
                    name: activeChannel.name,
                    selfScopeOnly: purgeSelfScopeOnly,
                  })
              : undefined
          }
          manageMessagesUnavailableDescription={
            canPurge ? undefined : 'You do not have permission to manage messages in this channel.'
          }
        />
      )}

      {error && <div className="chat-error">{error}</div>}

      <div className="chat-messages">
        <MessageList
          ref={messageListRef}
          key={activeChannelId}
          messages={messages}
          currentUserId={currentUserId}
          channelName={activeChannel?.name}
          isLoading={isLoading}
          hasMore={hasMore}
          onLoadMore={handleLoadMore}
          onEditMessage={editMessage}
          onDeleteMessage={deleteMessage}
          onUnseenOnLeave={handleUnseenOnLeave}
          onReply={handleReply}
          onScrollToMessage={handleScrollToMessage}
          onPinToggle={handlePinToggle}
          canPin={canPin}
          persistenceKey={activeChannelId || undefined}
        />
      </div>

      <TypingIndicator channelId={activeChannelId} />

      <div className="chat-input">
        <MessageInput
          onSendMessage={handleSendMessage}
          onTyping={sendTyping}
          channelName={activeChannel?.name}
          disabled={!currentUserId}
          serverId={activeChannel?.server_id}
          channelId={activeChannelId || undefined}
          replyingTo={replyingTo}
          onCancelReply={cancelReply}
        />
      </div>

      <PinnedMessagesPanel
        channelId={activeChannelId}
        isOpen={showPinnedPanel}
        onClose={() => setShowPinnedPanel(false)}
        onScrollToMessage={handleScrollToMessage}
        canPin={canPin}
      />

      <SearchPanel
        channelId={activeChannelId}
        isOpen={showSearchPanel}
        onClose={() => setShowSearchPanel(false)}
        onScrollToMessage={handleScrollToMessage}
        accessibleChannelIds={channels
          .filter((c) => c.server_id === activeChannel?.server_id)
          .map((c) => c.id)}
        showServerWideToggle={!!activeChannel?.server_id}
      />

      {expirationScope && (
        <Modal
          isOpen={showExpirationEditor}
          onClose={() => setShowExpirationEditor(false)}
          title="Message expiration"
        >
          <MessageExpirationEditor
            scope={expirationScope}
            policy={expiration.policy}
            policyState={expiration.policyState}
            canEdit={expiration.canEdit}
            lockedDescription={expiration.lockedDescription}
            onRefresh={expiration.onRefresh}
            onApplyPolicy={expiration.onApplyPolicy}
            onMarkSeen={expiration.onMarkSeen}
            onClose={() => setShowExpirationEditor(false)}
          />
        </Modal>
      )}

      {purgeTarget && (
        <PurgeMessagesModal
          context="channel"
          isOpen={true}
          onClose={() => setPurgeTarget(null)}
          scopeId={purgeTarget.id}
          scopeName={purgeTarget.name}
          selfScopeOnly={purgeTarget.selfScopeOnly}
        />
      )}
    </div>
  );
};

export default ChatView;
