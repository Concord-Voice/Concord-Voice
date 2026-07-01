import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import MessageList, { type MessageListHandle } from './MessageList';
import MessageInput from './MessageInput';
import TypingIndicator from './TypingIndicator';
import PinnedMessagesPanel from './PinnedMessagesPanel';
import SearchPanel from './SearchPanel';
import { pinMessage, unpinMessage, getChannelPins } from '../../services/pinService';
import { useChannelStore } from '../../stores/channelStore';
import { useUserStore } from '../../stores/userStore';
import { useChannelSubscription } from '../../hooks/useChannelSubscription';
import { errorMessage } from '../../utils/redactError';
import { useMessageFetch } from '../../hooks/useMessageFetch';
import { useChatController } from '../../hooks/useChatController';
import { useUnreadStore } from '../../stores/unreadStore';
import { useServerStore } from '../../stores/serverStore';
import type { ChatContext, MessageWithStatus } from '../../types/chat';
import './ChatView.css';

const ChatView: React.FC = () => {
  const activeChannelId = useChannelStore((s) => s.activeChannelId);
  const channels = useChannelStore((s) => s.channels);
  const user = useUserStore((s) => s.user);
  const activeServerId = useServerStore((s) => s.activeServerId);

  // Subscribe to active channel for full message delivery
  useChannelSubscription(activeChannelId);

  // Shared fetch/decrypt/paginate logic
  const { messages, isLoading, hasMore, error, handleLoadMore } = useMessageFetch(activeChannelId, {
    type: 'channel',
  });

  // Active channel info
  const activeChannel = channels.find((c) => c.id === activeChannelId);
  const currentUserId = user?.id || '';

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
      if (serverId) {
        useUnreadStore.getState().markServerUnread(serverId);
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

  // Toggle search panel via keyboard shortcut (#176)
  useEffect(() => {
    const handler = () => setShowSearchPanel((prev) => !prev);
    globalThis.addEventListener('concord:toggle-search', handler);
    return () => globalThis.removeEventListener('concord:toggle-search', handler);
  }, []);

  // Fetch pin count from API when channel changes
  useEffect(() => {
    if (!activeChannelId) return;
    getChannelPins(activeChannelId)
      .then((pins) => setPinnedCount(pins.length))
      .catch(() => setPinnedCount(0));
  }, [activeChannelId]);

  // Pin toggle with local count tracking (wraps hook's generic handler)
  const handlePinToggle = useCallback(
    async (message: MessageWithStatus) => {
      if (!activeChannelId) return;
      try {
        if (message.pinned_at) {
          await unpinMessage(message.id);
          setPinnedCount((c) => Math.max(0, c - 1));
        } else {
          await pinMessage(message.id);
          setPinnedCount((c) => c + 1);
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
    </div>
  );
};

export default ChatView;
