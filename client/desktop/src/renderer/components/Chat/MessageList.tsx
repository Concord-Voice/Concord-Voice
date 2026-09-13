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
import { useChannelScrollStore, type ScrollAnchor } from '../../stores/chat/channelScrollStore';
import { useDMStore } from '../../stores/chat/dmStore';
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
   * Optional key for reading-position preservation across remounts.
   * Pass the active channel ID for server channels or the DM conversation ID
   * for DM threads. When provided, the topmost visible message is saved as an
   * anchor on unmount (only when the user was above the Return to Latest
   * threshold) and restored on mount from channelScrollStore. Both callers
   * key the component by this id, so a change remounts it; a key change on a
   * surviving instance is handled the same way (the old key's anchor is
   * saved, the new key lands fresh).
   */
  persistenceKey?: string;
}

export interface MessageListHandle {
  scrollToMessage: (messageId: string) => void;
  scrollToBottomIfNear: () => void;
}

const NEAR_BOTTOM_THRESHOLD = 150;

/** Single definition of "following the latest message": inside this band the
 *  Return to Latest button is hidden, new messages auto-scroll, and leaving
 *  the thread clears its saved anchor. */
function isNearBottom(list: HTMLElement): boolean {
  return list.scrollHeight - list.scrollTop - list.clientHeight < NEAR_BOTTOM_THRESHOLD;
}

/** isNearBottom for one row: its bottom edge sits inside the band above the
 *  viewport's bottom edge. Asked of the previous latest row once a new one
 *  has already grown the content, when scrollHeight no longer tells. */
function rowNearBottom(list: HTMLElement, row: HTMLElement): boolean {
  return (
    row.getBoundingClientRect().bottom - list.getBoundingClientRect().bottom < NEAR_BOTTOM_THRESHOLD
  );
}

/** Attribute comparison, not a built selector: ids are server-issued, but a
 *  quote in one must never reach querySelector as syntax. */
function findRow(list: HTMLElement, messageId: string): HTMLElement | null {
  for (const row of list.querySelectorAll<HTMLElement>('[data-message-id]')) {
    if (row.dataset.messageId === messageId) return row;
  }
  return null;
}

/** The message row nearest the top edge of the viewport, with how far its top
 *  sits above that edge. Null when no row is in view (empty or detached list). */
function findTopAnchor(list: HTMLElement): ScrollAnchor | null {
  const listTop = list.getBoundingClientRect().top;
  for (const row of list.querySelectorAll<HTMLElement>('[data-message-id]')) {
    const rect = row.getBoundingClientRect();
    const messageId = row.dataset.messageId;
    if (messageId && rect.bottom > listTop) return { messageId, offset: listTop - rect.top };
  }
  return null;
}

/** Scroll so `row`'s top sits `offset` px above the viewport's top edge — the
 *  inverse of findTopAnchor. Relative, so it is correct whatever scrollTop is. */
function alignRowToTop(list: HTMLElement, row: HTMLElement, offset: number): void {
  list.scrollTop += row.getBoundingClientRect().top - list.getBoundingClientRect().top + offset;
}

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
    const landedKeyRef = useRef<string | null>(null);
    // scrollTop the landing left the list at, until its scroll event echoes.
    const landingScrollTopRef = useRef<number | null>(null);

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
        const el = listRef.current ? findRow(listRef.current, messageId) : null;
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

    // Reading position across channel/DM switches (persistenceKey).
    //
    // Landing runs once per key, the first time the list has rows: a saved
    // anchor puts that message back where it was and marks the list as not
    // following; otherwise the list opens at the latest row and follows it as
    // media resolves. Leaving saves an anchor only when the user was above
    // the Return to Latest threshold; from the bottom it clears the entry, so
    // the next visit lands on the latest row. The cleanup pairs every landing
    // with a leave, so a key change on a surviving instance, a StrictMode
    // replay, and a list that empties and refills all land afresh.
    // There is no pixel offset anywhere: GIF and image rows are skeletons
    // until their bytes resolve, so a scrollTop measured after they settled
    // lands short when replayed before they do, and pinning isNearBottom=false
    // on top of that disabled the re-pin that would have corrected it — the
    // recurring "soft-lock above the bottom".
    //
    // A first-unread landing was tried here and pulled: the server read marker
    // advances only when a thread is opened, so anything that arrived while
    // the user was viewing reads as unread after a refresh, and the landing
    // reproduced the soft-lock on messages they had already seen.
    const hasRows = messages.length > 0;
    useLayoutEffect(() => {
      if (!persistenceKey || !hasRows) return;
      const list = listRef.current;
      if (!list) return;

      if (landedKeyRef.current !== persistenceKey) {
        landedKeyRef.current = persistenceKey;
        const anchor = useChannelScrollStore.getState().getAnchor(persistenceKey);
        const anchorRow = anchor ? findRow(list, anchor.messageId) : null;

        if (anchor && anchorRow) {
          const before = list.scrollTop;
          alignRowToTop(list, anchorRow, anchor.offset);
          // An anchor exists only because the user left from above the
          // threshold, so a restore is never "following". The ref is set from
          // that fact, not from geometry: at skeleton height the viewport can
          // read near the bottom, and a ref derived from it re-pinned the list
          // on the next media resize. Geometry decides only the button, here
          // and again on every resize.
          isNearBottomRef.current = false;
          setShowScrollButton(!isNearBottom(list));
          // The assignment above fires a scroll event a frame later. The
          // handler must recognise that echo and not re-decide from whatever
          // geometry the viewport has at that instant (siblings below the list
          // are still settling their mount-time layout; StrictMode replays
          // every one of them in dev). Armed only when the list moved, and
          // never cleared here, so a StrictMode replay that lands on the same
          // scrollTop keeps the first landing's pending echo armed. Not armed
          // for a bottom landing: there the ref is true, and eating the first
          // user scroll-up would keep it true.
          if (list.scrollTop !== before) landingScrollTopRef.current = list.scrollTop;
        } else {
          // Explicit, not assumed: the ref survives a key change on a mounted
          // instance and a StrictMode replay. Pin here rather than leaving it
          // to the messages effect, whose deps do not change on a key change.
          isNearBottomRef.current = true;
          setShowScrollButton(false);
          list.scrollTop = list.scrollHeight;
        }
      }

      // Use the element captured at setup so cleanup measures the same node;
      // it is still attached when a deleted component's layout cleanup runs.
      // A detached node has no geometry to decide from, so it writes nothing:
      // neither a bogus anchor nor a clear that would drop a real one.
      return () => {
        landedKeyRef.current = null;
        if (!list.isConnected) return;
        const store = useChannelScrollStore.getState();
        const anchor = isNearBottomRef.current ? null : findTopAnchor(list);
        if (anchor) store.saveAnchor(persistenceKey, anchor);
        else store.clearAnchor(persistenceKey);
      };
    }, [persistenceKey, hasRows]);

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
      const list = listRef.current;
      const prevId = prevLastMessageIdRef.current;
      const arrived = prevId !== null && lastId !== prevId;
      const prevLast = arrived && list ? findRow(list, prevId) : null;

      // Follow the latest row, or start following when a message lands while
      // the bottom was already in view: a restored anchor can sit inside the
      // band at skeleton height, and content can shrink under a still
      // viewport. "Was" is measured on the previous latest row, because the
      // new one has already grown the content by the time this runs. Only an
      // arrival may promote geometry to following — on mount the geometry is
      // skeleton-height and the landing has decided.
      if (isNearBottomRef.current || (list && prevLast && rowNearBottom(list, prevLast))) {
        isNearBottomRef.current = true;
        if (list) list.scrollTop = list.scrollHeight;
        setNewMessageCount(0);
      } else if (arrived && lastMessage.user_id !== currentUserId) {
        // Scrolled up and someone else's message arrived — count it.
        setNewMessageCount((c) => c + 1);
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
    // Two elements are observed. The inner content wrapper grows when child
    // message rows grow (the scroll container's own content rect never does,
    // and the bottom sentinel <div> is a no-op). The scroll container itself
    // resizes when a sibling below it — composer, banner, typing row —
    // changes height. Either moves the bottom WITHOUT a scroll event, so the
    // Return to Latest state is re-derived from geometry here as well; a
    // state refreshed only by scroll events goes stale the moment layout
    // moves under a still viewport.
    useEffect(() => {
      const list = listRef.current;
      const content = contentRef.current;
      if (!list || !content || typeof ResizeObserver === 'undefined') return;
      const observer = new ResizeObserver(() => {
        if (isNearBottomRef.current) {
          list.scrollTop = list.scrollHeight;
        } else {
          setShowScrollButton(!isNearBottom(list));
        }
      });
      observer.observe(content);
      observer.observe(list);
      return () => observer.disconnect();
    }, [messages.length]);

    const scrollToBottom = useCallback((smooth = true) => {
      setNewMessageCount(0);
      setShowScrollButton(false);
      isNearBottomRef.current = true;
      if (listRef.current) {
        listRef.current.scrollTo({
          top: listRef.current.scrollHeight,
          behavior: smooth ? 'smooth' : 'auto',
        });
      }
    }, []);

    const handleScroll = useCallback(() => {
      const list = listRef.current;
      if (!list) return;

      // Load more when scrolled near the top. Before the echo skip below: an
      // anchor restored within 50px of the top of the loaded page must still
      // fetch the page above it, and its own echo may be the only scroll
      // event that position ever produces.
      if (hasMore && onLoadMore && list.scrollTop < 50 && !isLoading) {
        onLoadMore();
      }

      // First event after a landing: the echo of the landing's own scrollTop
      // assignment (possibly clamped lower by a viewport that was transiently
      // taller). A user scrolling up from the landing reads the same way and
      // loses nothing — they are moving away from the bottom, which is the
      // state the landing already set. A scroll DOWN is never the echo.
      if (landingScrollTopRef.current !== null) {
        const echo = list.scrollTop <= landingScrollTopRef.current;
        landingScrollTopRef.current = null;
        if (echo) return;
      }

      isNearBottomRef.current = isNearBottom(list);

      if (isNearBottomRef.current) {
        setNewMessageCount(0);
      }

      const shouldShow = !isNearBottomRef.current;
      if (shouldShow !== showScrollButton) {
        setShowScrollButton(shouldShow);
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
          {/* Inner content wrapper: the ResizeObserver above watches it for
              media-load growth, since a scroll container's own box does not
              grow with its content. The container is observed too, for
              siblings changing the viewport's height. */}
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
                  <div key={message.id} data-message-id={message.id}>
                    <CallEventMessage
                      payload={message.call_event_payload}
                      isGroup={isGroupConversation}
                      currentUserId={currentUserId}
                    />
                  </div>
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
            onClick={() => scrollToBottom(false)}
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
