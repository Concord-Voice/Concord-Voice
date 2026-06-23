import React, { useState, useCallback, useMemo, useRef, useEffect } from 'react';
import { MessageSquare, Users, Phone } from 'lucide-react';
import MessageList, { type MessageListHandle } from '../Chat/MessageList';
import DMPinnedMessagesPanel from './DMPinnedMessagesPanel';
import { getPins } from '../../services/pinService';
import MessageInput from '../Chat/MessageInput';
import TypingIndicator from '../Chat/TypingIndicator';
import { useDMStore, type DMConversation } from '../../stores/dmStore';
import { useUserStore } from '../../stores/userStore';
import { useDMSubscription } from '../../hooks/useDMSubscription';
import { errorMessage } from '../../utils/redactError';
import { useMessageFetch } from '../../hooks/useMessageFetch';
import { useChatController } from '../../hooks/useChatController';
import { apiFetch, safeJson } from '../../services/apiClient';
import { useVoiceStore } from '../../stores/voiceStore';
import { voiceService } from '../../services/voiceService';
import { usePrivacyStore } from '../../stores/privacyStore';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import GroupInfoPanel from './GroupInfoPanel';
import type { ChatContext } from '../../types/chat';
import './DirectMessages.css';

/*
 * Split view concept (future implementation):
 * When an active voice call is in progress within a DM, the chat area splits:
 *   - Top half: voice/video/screen-share view (reuse VoiceView/ParticipantGrid)
 *   - Bottom half: text chat (reuse MessageList + MessageInput)
 * If no active call, the text chat takes the full height.
 */

interface DMChatAreaProps {
  selectedThreadId: string | null;
}

/** Get display name for the header */
function getThreadName(conv: DMConversation | undefined, currentUserId: string): string {
  if (!conv) return 'Conversation';
  if (conv.isPersonal) return 'Personal Thread';
  if (conv.isGroup) {
    return conv.name || conv.participants.map((p) => p.displayName || p.username).join(', ');
  }
  const other = conv.participants.find((p) => p.userId !== currentUserId);
  return other?.displayName || other?.username || 'Unknown';
}

const DMChatArea: React.FC<DMChatAreaProps> = ({ selectedThreadId }) => {
  const [showGroupInfo, setShowGroupInfo] = useState(false);
  const [showPinnedPanel, setShowPinnedPanel] = useState(false);
  const [pinnedCount, setPinnedCount] = useState(0);
  const messageListRef = useRef<MessageListHandle>(null);

  // Fetch pin count from API when the conversation changes. Degrades
  // gracefully on 404 (getPins returns [] — no console error loop).
  useEffect(() => {
    if (!selectedThreadId) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears pinnedCount when no thread is selected; not a render loop
      setPinnedCount(0);
      return;
    }
    let cancelled = false;
    getPins(selectedThreadId)
      .then((pins) => {
        if (!cancelled) setPinnedCount(pins.length);
      })
      .catch(() => {
        if (!cancelled) setPinnedCount(0);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedThreadId]);

  // Close pinned panel when switching threads
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: closes pinned panel when switching threads; not a render loop
    setShowPinnedPanel(false);
  }, [selectedThreadId]);

  const handleScrollToMessage = useCallback((messageId: string) => {
    messageListRef.current?.scrollToMessage(messageId);
  }, []);

  const conversations = useDMStore((s) => s.conversations);
  const clearUnread = useDMStore((s) => s.clearUnread);
  const user = useUserStore((s) => s.user);
  const dmPrivacyLevel = usePrivacyStore((s) => s.settings.dmPrivacyLevel);

  // Active-DM-call roster for the open conversation (#1219 R5). Drives the
  // "Join voice call" header affordance. Subscribed selectively so the header
  // re-renders only when this conversation's call roster changes.
  const activeCall = useVoiceStore((s) =>
    selectedThreadId ? s.activeDMCalls[selectedThreadId] : undefined
  );
  // True when the local user is already in THIS DM call — suppress the Join
  // affordance (you don't join a call you're in). Backed by the store's DM
  // call fields set on join.
  const isInThisCall = useVoiceStore((s) => s.isDMCall && s.dmConversationId === selectedThreadId);

  // Subscribe to active DM conversation for real-time messages
  useDMSubscription(selectedThreadId);

  const activeConv = conversations.find((c) => c.id === selectedThreadId);
  const currentUserId = user?.id || '';
  const threadName = getThreadName(activeConv, currentUserId);

  // Hydrate the active-DM-call roster on conversation open (#1219 R4 / G4).
  // Live `dm_voice_state_update` deltas only populate the roster for events
  // that arrive while the conversation is open; a member who was offline at
  // ring (or reconnecting) would otherwise see no "N of M in call" / "Join
  // voice call" affordance. Best-effort: live deltas remain authoritative.
  const activeConvId = activeConv?.id;
  const activeConvMemberCount = activeConv?.participants.length ?? 0;
  useEffect(() => {
    if (!activeConvId) return;
    let cancelled = false;
    (async () => {
      // Snapshot the roster entry BEFORE the probe. applyDMVoiceState installs a
      // NEW entry object on every live delta, so a referential change after the
      // await means a join raced in while the probe was in flight (#1568 Gitar
      // race fix) — never wipe a live roster on a racing empty probe response.
      const before = useVoiceStore.getState().activeDMCalls[activeConvId];
      try {
        const res = await apiFetch(`/api/v1/dm/conversations/${activeConvId}/voice/participants`);
        if (!res.ok || cancelled) return;
        const body = await safeJson<{ participants?: { user_id: string }[] }>(res);
        const ids = (body.participants ?? []).map((p) => p.user_id);
        if (cancelled) return;
        if (ids.length > 0) {
          useVoiceStore.getState().seedActiveDMCall(activeConvId, ids, activeConvMemberCount);
        } else if (useVoiceStore.getState().activeDMCalls[activeConvId] === before) {
          // No live delta replaced the roster during the probe → safe to clear a
          // genuinely-ended call. If a join raced in, leave the live roster intact.
          useVoiceStore.getState().clearActiveDMCall(activeConvId);
        }
      } catch {
        // best-effort; live deltas still populate the roster
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [activeConvId, activeConvMemberCount]);

  // Chat controller — unified send/edit/delete/reply/pin/typing
  const ctx: ChatContext = useMemo(
    () => ({
      type: 'dm' as const,
      id: selectedThreadId || '',
    }),
    [selectedThreadId]
  );

  const {
    sendMessage,
    editMessage,
    deleteMessage,
    replyingTo,
    handleReply,
    cancelReply,
    handlePinToggle: handlePinToggleBase,
    sendTyping,
    chatContext,
  } = useChatController(ctx);

  // Wrap the generic pin toggle to keep the badge count in sync (mirrors ChatView pattern).
  const handlePinToggle = useCallback(
    async (msg: Parameters<typeof handlePinToggleBase>[0]) => {
      try {
        await handlePinToggleBase(msg);
        setPinnedCount((c) => (msg.pinned_at ? Math.max(0, c - 1) : c + 1));
      } catch {
        // error already logged by handlePinToggleBase
      }
    },
    [handlePinToggleBase]
  );

  // Mark conversation as read after messages are fetched
  const handleFetchComplete = useCallback(() => {
    if (!selectedThreadId) return;
    const previousUnread =
      useDMStore.getState().conversations.find((c) => c.id === selectedThreadId)?.unreadCount ?? 0;
    clearUnread(selectedThreadId);
    apiFetch(`/api/v1/dm/conversations/${selectedThreadId}/read`, { method: 'POST' }).catch(
      (error) => {
        console.error('[DMChatArea] Failed to mark conversation as read:', errorMessage(error));
        if (previousUnread > 0) {
          useDMStore
            .getState()
            .updateConversation(selectedThreadId, { unreadCount: previousUnread });
        }
      }
    );
  }, [selectedThreadId, clearUnread]);

  // Shared fetch/decrypt/paginate logic
  const { messages, isLoading, hasMore, error, handleLoadMore } = useMessageFetch(
    selectedThreadId,
    { type: 'dm', onFetchComplete: handleFetchComplete }
  );

  // Unseen count on leave (DM-specific: uses dmStore, not unreadStore)
  const handleUnseenOnLeave = useCallback(
    (count: number) => {
      if (!selectedThreadId || count <= 0) return;
      useDMStore.getState().incrementUnread(selectedThreadId);
    },
    [selectedThreadId]
  );

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
      if (!selectedThreadId) return;
      sendMessage(content, { mentionMeta, replyToId, attachmentIds, attachments, gifSlug });
    },
    [selectedThreadId, sendMessage]
  );

  if (!selectedThreadId) {
    return (
      <div className="dm-chat-empty">
        <div className="dm-chat-empty-icon">
          <MessageSquare size={48} />
        </div>
        <h3>Welcome to Concord Voice</h3>
        <p>Select a message thread or server to get started</p>
      </div>
    );
  }

  return (
    <div className="dm-chat-area-wrapper">
      <div
        className="dm-chat-placeholder"
        style={{ display: 'flex', flexDirection: 'column', height: '100%', flex: 1, minWidth: 0 }}
      >
        <div className="dm-chat-header">
          <div className="conversation-avatar" style={{ width: 28, height: 28 }}>
            <span
              className="conversation-avatar-initial"
              style={(() => {
                const other =
                  activeConv && !activeConv.isGroup && !activeConv.isPersonal
                    ? activeConv.participants.find((p) => p.userId !== currentUserId)
                    : null;
                const colors = resolveUserAccentColors(other?.colorScheme);
                return colors ? { fontSize: 12, background: colors.gradient } : { fontSize: 12 };
              })()}
            >
              {threadName.charAt(0).toUpperCase()}
            </span>
          </div>
          <div style={{ flex: 1 }}>
            <div className="dm-chat-header-name">{threadName}</div>
            <div className="dm-chat-header-status">
              {(() => {
                if (activeConv?.isPersonal) return 'Your personal notes';
                if (activeConv && !activeConv.isGroup) {
                  return (
                    activeConv.participants.find((p) => p.userId !== currentUserId)?.status ||
                    'offline'
                  );
                }
                return `${activeConv?.participants.length || 0} members`;
              })()}
            </div>
          </div>
          {/* Join voice call (#1219 R5). Shown when this is a group DM with an
              active voice call the local user is NOT already in — a member who
              was offline/declined at ring, or who closed and reopened the
              conversation, can jump in. Routes through voiceService.joinChannel
              with the 'dm' join type, mirroring acceptIncomingCall. */}
          {activeConv?.isGroup && activeCall && !isInThisCall && (
            <button
              type="button"
              className="dm-chat-header-join-call-btn"
              onClick={() => {
                if (!activeConv) return;
                void voiceService.joinChannel(activeConv.id, 'dm').catch((err: unknown) => {
                  // .catch prevents an unhandled rejection at the click
                  // boundary; joinChannel surfaces its own user-facing error.
                  console.error(
                    'Join voice call:',
                    err instanceof Error ? err.message : 'non-Error thrown'
                  );
                });
              }}
              aria-label="Join voice call"
              title="Join voice call"
            >
              <Phone size={16} />
              <span>Join voice call</span>
            </button>
          )}
          <button
            type="button"
            className="chat-header-pin-button"
            onClick={() => setShowPinnedPanel((v) => !v)}
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
          {activeConv?.isGroup && (
            <button
              type="button"
              className="dm-chat-header-group-info-btn"
              onClick={() => setShowGroupInfo((v) => !v)}
              aria-label="Toggle group info"
              title="Group Info"
            >
              <Users size={18} />
            </button>
          )}
        </div>

        {error && <div className="chat-error">{error}</div>}

        <div className="chat-messages" style={{ flex: 1, minHeight: 0 }}>
          <MessageList
            ref={messageListRef}
            key={selectedThreadId}
            messages={messages}
            currentUserId={currentUserId}
            chatContext={chatContext}
            channelName={threadName}
            isLoading={isLoading}
            hasMore={hasMore}
            onLoadMore={handleLoadMore}
            onEditMessage={editMessage}
            onDeleteMessage={deleteMessage}
            onUnseenOnLeave={handleUnseenOnLeave}
            onReply={handleReply}
            onScrollToMessage={handleScrollToMessage}
            onPinToggle={handlePinToggle}
            canPin={true}
            persistenceKey={selectedThreadId}
          />
        </div>

        <TypingIndicator channelId={selectedThreadId} />

        {dmPrivacyLevel === 0 ? (
          <div className="dm-disabled-notice">
            All DMs have been disabled. Change your privacy settings to restore DMs.
          </div>
        ) : (
          <div className="chat-input">
            <MessageInput
              onSendMessage={handleSendMessage}
              onTyping={sendTyping}
              channelName={threadName}
              disabled={!currentUserId}
              conversationId={selectedThreadId}
              replyingTo={replyingTo}
              onCancelReply={cancelReply}
            />
          </div>
        )}
      </div>

      {showGroupInfo && activeConv?.isGroup && (
        <GroupInfoPanel conversation={activeConv} onClose={() => setShowGroupInfo(false)} />
      )}

      <DMPinnedMessagesPanel
        conversationId={selectedThreadId}
        isOpen={showPinnedPanel}
        onClose={() => setShowPinnedPanel(false)}
        onScrollToMessage={handleScrollToMessage}
        canPin={true}
        onUnpin={() => setPinnedCount((c) => Math.max(0, c - 1))}
      />
    </div>
  );
};

export default DMChatArea;
