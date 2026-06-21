import React, {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  useCallback,
  useImperativeHandle,
  forwardRef,
} from 'react';
import { MessageWithStatus, type ChatContextType } from '../../types/chat';
import Message from './Message';
import { CallEventMessage } from '../DirectMessages/CallEventMessage';
import { useChannelScrollStore } from '../../stores/channelScrollStore';
import { useDMStore } from '../../stores/dmStore';
import './MessageList.css';

export interface MessageListProps {
  messages: MessageWithStatus[];
  currentUserId: string;
  channelName?: string;
  chatContext?: ChatContextType;
  isLoading?: boolean;
  hasMore?: boolean;
  onLoadMore?: () => void;
  onEditMessage?: (messageId: string, newContent: string) => void;
  onDeleteMessage?: (messageId: string) => void;
  onUnseenOnLeave?: (count: number) => void;
  onReply?: (message: MessageWithStatus) => void;
  onPinToggle?: (message: MessageWithStatus) => void;
  canPin?: boolean;
  onScrollToMessage?: (messageId: string) => void;
  /**
   * Optional key for scroll-position preservation across remounts.
   * Pass the active channel ID for server channels or the DM conversation ID
   * for DM threads. When provided, scroll offset is saved on unmount and
   * restored on mount from channelScrollStore.
   */
  persistenceKey?: string;
}

export interface MessageListHandle {
  scrollToMessage: (messageId: string) => void;
  scrollToBottomIfNear: () => void;
}

const NEAR_BOTTOM_THRESHOLD = 150;
const SHOW_BUTTON_THRESHOLD = 300;

// eslint-disable-next-line @eslint-react/no-forward-ref -- forwardRef is intentional here; refactoring to prop-based ref would require updating all callers and is deferred
const MessageList = forwardRef<MessageListHandle, MessageListProps>(
  (
    {
      messages,
      currentUserId,
      channelName,
      chatContext = 'channel',
      isLoading = false,
      hasMore = false,
      onLoadMore,
      onEditMessage,
      onDeleteMessage,
      onUnseenOnLeave,
      onReply,
      onScrollToMessage,
      onPinToggle,
      canPin,
      persistenceKey,
    },
    ref
  ) => {
    const listRef = useRef<HTMLDivElement>(null);
    const contentRef = useRef<HTMLDivElement>(null);
    const bottomRef = useRef<HTMLDivElement>(null);
    const isNearBottomRef = useRef(true);
    const [showScrollButton, setShowScrollButton] = useState(false);
    const [shiftHeld, setShiftHeld] = useState(false);
    const [newMessageCount, setNewMessageCount] = useState(0);
    const prevLastMessageIdRef = useRef<string | null>(null);
    const newMessageCountRef = useRef(0);
    const onUnseenOnLeaveRef = useRef(onUnseenOnLeave);
    onUnseenOnLeaveRef.current = onUnseenOnLeave;
    const [highlightedMessageId, setHighlightedMessageId] = useState<string | null>(null);
    const highlightTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

    // Real group flag for DM call-event rendering (#1568): when this list is a
    // DM thread, look up the conversation by persistenceKey (its id) and read
    // its authoritative `isGroup`. Undefined for channels or unloaded DMs, in
    // which case the call-event block falls back to a participant-count heuristic.
    const dmIsGroup = useDMStore((s) =>
      chatContext === 'dm' && persistenceKey
        ? s.conversations.find((c) => c.id === persistenceKey)?.isGroup
        : undefined
    );

    // Clear highlight timeout on unmount to prevent setState-on-unmounted
    useEffect(() => {
      return () => {
        if (highlightTimerRef.current) clearTimeout(highlightTimerRef.current);
      };
    }, []);

    useImperativeHandle(ref, () => ({
      scrollToMessage: (messageId: string) => {
        const el = listRef.current?.querySelector(`[data-message-id="${messageId}"]`);
        if (el) {
          el.scrollIntoView({ behavior: 'smooth', block: 'center' });
          setHighlightedMessageId(messageId);
          if (highlightTimerRef.current) clearTimeout(highlightTimerRef.current);
          highlightTimerRef.current = setTimeout(() => setHighlightedMessageId(null), 2000);
        }
      },
      scrollToBottomIfNear: () => {
        if (isNearBottomRef.current) {
          requestAnimationFrame(() => scrollToBottom(false));
        }
      },
    }));

    useEffect(() => {
      const onKeyDown = (e: KeyboardEvent) => {
        if (e.key === 'Shift') setShiftHeld(true);
      };
      const onKeyUp = (e: KeyboardEvent) => {
        if (e.key === 'Shift') setShiftHeld(false);
      };
      globalThis.addEventListener('keydown', onKeyDown);
      globalThis.addEventListener('keyup', onKeyUp);
      return () => {
        globalThis.removeEventListener('keydown', onKeyDown);
        globalThis.removeEventListener('keyup', onKeyUp);
      };
    }, []);

    // Keep ref in sync with state so the unmount cleanup reads the latest value
    useEffect(() => {
      newMessageCountRef.current = newMessageCount;
    }, [newMessageCount]);

    // On unmount, report unseen messages so the parent can set unread badges
    useEffect(() => {
      return () => {
        if (newMessageCountRef.current > 0 && onUnseenOnLeaveRef.current) {
          onUnseenOnLeaveRef.current(newMessageCountRef.current);
        }
      };
    }, []);

    // Scroll-position preservation across channel/DM switches.
    //
    // Uses useLayoutEffect so restoration happens after layout but before
    // paint, avoiding a one-frame jump. When a saved offset exists we also
    // flip isNearBottomRef to false so the sibling auto-pin effects (which
    // watch ResizeObserver and the messages array) don't fight the restore
    // and yank the user back to the bottom. On unmount we persist whatever
    // the current scrollTop is so the next mount can restore it.
    useLayoutEffect(() => {
      if (!persistenceKey) return;
      const list = listRef.current;
      if (!list) return;
      const saved = useChannelScrollStore.getState().getScroll(persistenceKey);
      if (typeof saved === 'number') {
        list.scrollTop = saved;
        isNearBottomRef.current = false;
      }
      return () => {
        // Use the element captured at setup so cleanup records scroll on the
        // same node; listRef.current may have changed by the time this runs.
        useChannelScrollStore.getState().saveScroll(persistenceKey, list.scrollTop);
      };
    }, [persistenceKey]);

    // Auto-scroll when messages change (new message arrives) if user is near bottom.
    // Also track new messages arriving while the user is scrolled up.
    useEffect(() => {
      if (messages.length === 0) {
        prevLastMessageIdRef.current = null;
        // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets newMessageCount to 0 when messages list clears; not a render loop
        setNewMessageCount(0);
        return;
      }

      const lastMessage = messages.at(-1);
      if (!lastMessage) return; // unreachable: length check above guarantees at-least-one
      const lastId = lastMessage.id;

      if (isNearBottomRef.current) {
        // User is at the bottom — auto-scroll and reset counter
        if (listRef.current) {
          listRef.current.scrollTop = listRef.current.scrollHeight;
        }
        setNewMessageCount(0);
      } else if (prevLastMessageIdRef.current !== null && lastId !== prevLastMessageIdRef.current) {
        // User is scrolled up and a new message arrived — only count others' messages
        if (lastMessage.user_id !== currentUserId) {
          setNewMessageCount((c) => c + 1);
        }
      }

      prevLastMessageIdRef.current = lastId;
    }, [messages, currentUserId]);

    // Re-pin to bottom when the rendered content grows after initial paint
    // (e.g. a GIF embed resolves to its final size, or an image attachment's
    // bytes finish decoding and the row gets taller). Without this, late-
    // loading media stays clipped below the viewport even though the user
    // was at the bottom when the message arrived — exactly the "have to
    // manually scroll" symptom on send.
    //
    // We observe an inner content wrapper rather than the scroll container
    // because the scroll container itself is fixed by flex (its content rect
    // never changes), and observing the bottom sentinel <div> is also a
    // no-op. Only the inner wrapper grows when child message rows grow, so
    // it's the one element whose ResizeObserver actually fires on media load.
    useEffect(() => {
      const list = listRef.current;
      const content = contentRef.current;
      if (!list || !content || typeof ResizeObserver === 'undefined') return;
      const observer = new ResizeObserver(() => {
        if (isNearBottomRef.current) {
          list.scrollTop = list.scrollHeight;
        }
      });
      observer.observe(content);
      return () => observer.disconnect();
    }, []);

    const scrollToBottom = useCallback((smooth = true) => {
      setNewMessageCount(0);
      if (listRef.current) {
        listRef.current.scrollTo({
          top: listRef.current.scrollHeight,
          behavior: smooth ? 'smooth' : 'auto',
        });
      }
    }, []);

    const handleScroll = useCallback(() => {
      if (!listRef.current) return;

      const { scrollTop, scrollHeight, clientHeight } = listRef.current;
      const distanceFromBottom = scrollHeight - scrollTop - clientHeight;

      isNearBottomRef.current = distanceFromBottom < NEAR_BOTTOM_THRESHOLD;

      if (isNearBottomRef.current) {
        setNewMessageCount(0);
      }

      const shouldShow = distanceFromBottom > SHOW_BUTTON_THRESHOLD;
      if (shouldShow !== showScrollButton) {
        setShowScrollButton(shouldShow);
      }

      // Load more messages when scrolled near top
      if (hasMore && onLoadMore && scrollTop < 50 && !isLoading) {
        onLoadMore();
      }
    }, [hasMore, onLoadMore, isLoading, showScrollButton]);

    const shouldShowAvatar = (index: number): boolean => {
      if (index === 0) return true;
      const currentMessage = messages[index];
      const previousMessage = messages[index - 1];

      // Show avatar if different user or time gap > 2 minutes
      if (currentMessage.user_id !== previousMessage.user_id) return true;

      const currentTime = new Date(currentMessage.created_at).getTime();
      const previousTime = new Date(previousMessage.created_at).getTime();
      const twoMinutes = 2 * 60 * 1000;

      return currentTime - previousTime > twoMinutes;
    };

    const formatDateDivider = (date: Date): string => {
      const today = new Date();
      const yesterday = new Date(today);
      yesterday.setDate(yesterday.getDate() - 1);

      if (date.toDateString() === today.toDateString()) {
        return 'Today';
      } else if (date.toDateString() === yesterday.toDateString()) {
        return 'Yesterday';
      } else {
        return date.toLocaleDateString('en-US', {
          weekday: 'long',
          year: 'numeric',
          month: 'long',
          day: 'numeric',
        });
      }
    };

    const shouldShowDateDivider = (index: number): string | null => {
      if (index === 0) return formatDateDivider(new Date(messages[0].created_at));

      const currentDate = new Date(messages[index].created_at).toDateString();
      const previousDate = new Date(messages[index - 1].created_at).toDateString();

      if (currentDate !== previousDate) {
        return formatDateDivider(new Date(messages[index].created_at));
      }

      return null;
    };

    if (isLoading && messages.length === 0) {
      return (
        <div className="message-list-empty">
          <div className="message-list-loading">
            <div className="message-list-spinner"></div>
          </div>
          <p>Loading messages...</p>
        </div>
      );
    }

    if (messages.length === 0) {
      return (
        <div className="message-list-empty">
          <div className="empty-icon">💬</div>
          <h3>Welcome to #{channelName || 'this channel'}!</h3>
          <p>This is the beginning of your conversation.</p>
          <p className="message-list-empty-hint">Send a message to get started.</p>
        </div>
      );
    }

    return (
      <div className="message-list-container">
        <div className="message-list" ref={listRef} onScroll={handleScroll}>
          {/* Inner content wrapper exists so the ResizeObserver above can
              fire on media-load growth. The scroll container itself never
              resizes (fixed by flex), but this wrapper grows with content. */}
          <div className="message-list-content" ref={contentRef}>
            {isLoading && hasMore && (
              <div className="loading-more">
                <div className="loading-spinner small">
                  <div className="message-list-spinner"></div>
                </div>
                <span>Loading more messages...</span>
              </div>
            )}

            {messages.map((message, index) => {
              const dateDividerLabel = shouldShowDateDivider(index);
              // Call-event system rows (#1219 R7): render the dedicated
              // CallEventMessage instead of <Message>. The backend serializer
              // returns `type` + `call_event_payload`; useMessageFetch skips
              // the E2EE decrypt pass for these rows. group-vs-1:1 uses the real
              // conversation `isGroup` (dmIsGroup, resolved from persistenceKey),
              // falling back to a participant-count heuristic only when the DM
              // conversation isn't loaded (#1568 Gitar accuracy fix).
              if (message.type === 'call_event' && message.call_event_payload) {
                const isGroupConversation =
                  dmIsGroup ?? (message.call_event_payload.participant_user_ids?.length ?? 0) > 2;
                return (
                  <CallEventMessage
                    key={message.id}
                    payload={message.call_event_payload}
                    isGroup={isGroupConversation}
                  />
                );
              }
              return (
                <React.Fragment key={message.id}>
                  {dateDividerLabel && (
                    <div className="date-divider">
                      <span className="date-divider-line"></span>
                      <span className="date-divider-text">{dateDividerLabel}</span>
                      <span className="date-divider-line"></span>
                    </div>
                  )}
                  <div
                    data-message-id={message.id}
                    className={
                      highlightedMessageId === message.id ? 'message-highlight' : undefined
                    }
                  >
                    <Message
                      message={message}
                      currentUserId={currentUserId}
                      chatContext={chatContext}
                      onEdit={onEditMessage}
                      onDelete={onDeleteMessage}
                      onReply={onReply}
                      onScrollToMessage={onScrollToMessage}
                      onPinToggle={onPinToggle}
                      canPin={canPin}
                      showAvatar={shouldShowAvatar(index)}
                      shiftHeld={shiftHeld}
                    />
                  </div>
                </React.Fragment>
              );
            })}
            <div ref={bottomRef} />
          </div>
        </div>

        {showScrollButton && (
          <button
            className="scroll-to-bottom"
            onClick={() => scrollToBottom(true)}
            aria-label="Return to latest"
          >
            {newMessageCount > 0 && (
              <span className="new-message-badge">
                {newMessageCount > 99 ? '99+' : newMessageCount}
              </span>
            )}
            <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
              <path d="M10 14l-6-6h12l-6 6z" fill="currentColor" />
            </svg>
            Return to Latest
          </button>
        )}
      </div>
    );
  }
);

MessageList.displayName = 'MessageList';

export default MessageList;
