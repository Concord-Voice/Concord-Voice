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
import { useUnreadStore } from '../../stores/chat/unreadStore';
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
  /**
   * Called when the user has demonstrably seen the latest message: another
   * user's message arrives while already at the bottom with the tab visible,
   * or the user reaches the bottom (scroll or Return to Latest) with unseen
   * messages pending. Wire to `useReadMarker`'s `markSeen` — this prop only
   * signals "seen", it does not itself post anything.
   */
  onLatestSeen?: () => void;
  /**
   * Fires when the list stops following the latest message (the user scrolls
   * up from the bottom). The parent flushes its pending read marker here, so
   * the server's stamp lands before anything arrives unseen.
   */
  onLatestLeft?: () => void;
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
   * saved, the new key lands fresh). It is also the key under which the
   * thread's unread count is read — once, at mount — so on a surviving
   * instance a new key would land with the old key's count; acceptable only
   * because both callers remount.
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

/** Rule 2's probe: stand on `row` only if the rows below it overflow the
 *  viewport. If they fit, reading from the bottom covers them, so the list
 *  is pinned there instead. Returns whether the list now stands on the row. */
function standOnUnread(list: HTMLElement, row: HTMLElement): boolean {
  alignRowToTop(list, row, 0);
  if (!isNearBottom(list)) return true;
  list.scrollTop = list.scrollHeight;
  return false;
}

/** How many of the last `unread` others' rows still sit below the viewport's
 *  bottom edge. The unread rows are always a suffix of others' rows — the
 *  last N at open, then arrivals — so no per-row flag is needed: count others'
 *  rows up from the bottom until one is fully in view, capped at the run. */
function unreadBelow(
  list: HTMLElement,
  rows: MessageWithStatus[],
  currentUserId: string,
  unread: number
): number {
  if (unread <= 0) return 0;
  const own = new Set(rows.filter((m) => m.user_id === currentUserId).map((m) => m.id));
  const listBottom = list.getBoundingClientRect().bottom;
  const domRows = list.querySelectorAll<HTMLElement>('[data-message-id]');
  let below = 0;
  for (let i = domRows.length - 1; i >= 0 && below < unread; i--) {
    const row = domRows[i];
    if (row.getBoundingClientRect().bottom <= listBottom) break;
    if (!own.has(row.dataset.messageId ?? '')) below++;
  }
  return below;
}

type LandingTarget = {
  row: HTMLElement;
  offset: number;
  unread: boolean;
  /** Rule 2 only: the count exceeded the mounted rows, so the true first
   *  unread is on an older page. */
  exhausted: boolean;
};

/** Where a fresh mount of a thread lands: the saved anchor when its row is
 *  still mounted, else the first unread row when there are unread rows, else
 *  nothing — applyLanding then pins to the latest row. */
function resolveLandingTarget(
  list: HTMLElement,
  rows: MessageWithStatus[],
  anchor: ScrollAnchor | undefined,
  unreadOnOpen: number,
  currentUserId: string
): LandingTarget | null {
  const anchorRow = anchor ? findRow(list, anchor.messageId) : null;
  if (anchor && anchorRow) {
    return { row: anchorRow, offset: anchor.offset, unread: false, exhausted: false };
  }
  // The server's count excludes the user's own messages, so walk back over
  // others' rows only; a count larger than what is mounted stops at the
  // topmost such row. Own rows above it are read by definition.
  let firstUnread: MessageWithStatus | undefined;
  let remaining = unreadOnOpen;
  for (let i = rows.length - 1; i >= 0 && remaining > 0; i--) {
    if (rows[i].user_id === currentUserId) continue;
    firstUnread = rows[i];
    remaining--;
  }
  const unreadRow = firstUnread ? findRow(list, firstUnread.id) : null;
  return unreadRow ? { row: unreadRow, offset: 0, unread: true, exhausted: remaining > 0 } : null;
}

/** Move the list to its landing target. Rule 2 lands only when the unread
 *  rows overflow the viewport; if they fit, reading from the bottom covers
 *  them. That IS a geometry question, measured after the align — unlike rule
 *  1, where the anchor itself says the user was above the threshold. With no
 *  target (or a rule-2 target that fits) the list opens at the latest row.
 *  Returns whether the list is now following, and the scrollTop before any
 *  align: the echo skip must be armed against where the list STARTED, and
 *  the rule-2 probe moves it. */
function applyLanding(
  list: HTMLElement,
  target: LandingTarget | null
): { following: boolean; before: number } {
  const before = list.scrollTop;
  if (target?.unread) return { following: !standOnUnread(list, target.row), before };
  if (!target) {
    list.scrollTop = list.scrollHeight;
    return { following: true, before };
  }
  alignRowToTop(list, target.row, target.offset);
  return { following: false, before };
}

/** Leaving a thread: save the topmost visible row as its anchor when the user
 *  was above the Return to Latest threshold, otherwise clear the entry so the
 *  next visit lands on the latest row. A detached node has no geometry to
 *  decide from, so it writes nothing: neither a bogus anchor nor a clear that
 *  would drop a real one. */
function recordLeave(list: HTMLElement, key: string, following: boolean): void {
  if (!list.isConnected) return;
  const store = useChannelScrollStore.getState();
  const anchor = following ? null : findTopAnchor(list);
  if (anchor) store.saveAnchor(key, anchor);
  else store.clearAnchor(key);
}

/** The thread's unread count as it stands when the list mounts. Read once, at
 *  mount: ChannelList clears the channel count in a passive effect of the same
 *  commit that mounts this list, and DMChatArea clears the DM count when the
 *  fetch completes, so by the time rows are on screen the count is gone. */
function readUnreadOnOpen(chatContext: ChatContextType, key: string | undefined): number {
  if (!key) return 0;
  if (chatContext === 'dm') {
    return useDMStore.getState().conversations.find((c) => c.id === key)?.unreadCount ?? 0;
  }
  return useUnreadStore.getState().unreadCounts.get(key) ?? 0;
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
      onLatestSeen,
      onLatestLeft,
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
    const onLatestSeenRef = useRef(onLatestSeen);
    onLatestSeenRef.current = onLatestSeen;
    const onLatestLeftRef = useRef(onLatestLeft);
    onLatestLeftRef.current = onLatestLeft;
    // Read at landing time without joining the landing effect's deps, whose
    // change would pair a leave with a fresh landing.
    const loadMoreRef = useRef({ hasMore, onLoadMore, isLoading });
    loadMoreRef.current = { hasMore, onLoadMore, isLoading };
    // Others' messages that arrived while scrolled up — the part of the badge
    // reported on leave. The badge itself is a position: the unread rows
    // still below the viewport, capped at this plus the count seeded at
    // landing, which the open-time read already covered server-side.
    const arrivedWhileAwayRef = useRef(0);
    const seededRef = useRef(0);
    // Arrivals shown at the bottom while the window was hidden or unfocused:
    // marked when both return (the regain effect below), folded into the
    // away count if the user scrolls up first, reported on unmount — never
    // dropped. Counted row by row (a batch adds all its other-user rows), and
    // the first one's row is where a burst that no longer fits is landed on.
    const unseenAtBottomRef = useRef(0);
    const firstUnseenIdRef = useRef<string | null>(null);
    // A thread whose fetch ended with no rows: its first live message is an
    // arrival, not hydration. Set only when a loading cycle ends on an empty
    // list, so a mount's initial `[]` (loading not yet begun) and the rows a
    // fetch delivers never read as arrivals.
    const emptyAfterLoadRef = useRef(false);
    const prevLoadingRef = useRef(false);
    // The ids the list held before this update: what arrived is what was not
    // here. A page that replaced an evicted tail is all new; a deleted tail
    // is not.
    const prevIdsRef = useRef<Set<string>>(new Set());
    // Whether the first loading cycle for this thread has ended. Rows that
    // cycle appends — a remount shows cached rows first, then the fetch's
    // newer ones — are hydration, never arrivals; later cycles (a reconnect
    // backfill) are.
    const hydratedRef = useRef(false);
    const [highlightedMessageId, setHighlightedMessageId] = useState<string | null>(null);
    const highlightTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
    const landedKeyRef = useRef<string | null>(null);
    // scrollTop the landing left the list at, until its scroll event echoes.
    const landingScrollTopRef = useRef<number | null>(null);
    const [unreadOnOpen] = useState(() => readUnreadOnOpen(chatContext, persistenceKey));
    // Landing reads the rows once, so keep them in a ref rather than making the
    // landing effect re-run (and re-save the anchor) on every message arrival.
    const messagesRef = useRef(messages);
    messagesRef.current = messages;

    // Leaving the latest message: arrivals only "shown" while unfocused were
    // never seen — they become unread, not lost — and the parent flushes its
    // pending read marker so the server's stamp lands before anything
    // arrives unseen.
    const leaveLatest = useCallback(() => {
      arrivedWhileAwayRef.current += unseenAtBottomRef.current;
      unseenAtBottomRef.current = 0;
      onLatestLeftRef.current?.();
    }, []);
    // The badge while not following: unread rows still below the viewport.
    const refreshBadge = useCallback(
      (list: HTMLElement) => {
        // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: the badge is re-derived from geometry after a landing or a scroll; not a render loop
        setNewMessageCount(
          unreadBelow(
            list,
            messagesRef.current,
            currentUserId,
            seededRef.current + arrivedWhileAwayRef.current
          )
        );
      },
      [currentUserId]
    );
    // Others' rows appended at once that overflow the viewport (a reconnect
    // backfill, a burst committed in one update): pinning would show only
    // their tail and mark them all. Stand on the first instead, as unread at
    // open, and count them. Returns whether the list stood.
    const standOnAppended = useCallback(
      (list: HTMLElement, others: MessageWithStatus[]): boolean => {
        if (others.length === 0) return false;
        // The earliest unseen row: one remembered from an unfocused arrival
        // if there is one, else the first of this batch — a burst that
        // started while unfocused must not be landed on past its start.
        const firstId =
          unseenAtBottomRef.current > 0 && firstUnseenIdRef.current
            ? firstUnseenIdRef.current
            : others[0].id;
        const row = findRow(list, firstId);
        if (!row || !standOnUnread(list, row)) return false;
        isNearBottomRef.current = false;
        leaveLatest();
        arrivedWhileAwayRef.current += others.length;
        refreshBadge(list);
        // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: the button is re-derived from geometry when a batch overflows; not a render loop
        setShowScrollButton(true);
        return true;
      },
      [leaveLatest, refreshBadge]
    );

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

    // On unmount, report the messages that arrived while scrolled up so the
    // parent can put them on the unread badge. The count seeded at landing is
    // excluded: the open-time read already covered it server-side, and the
    // DM parent ADDS this number to its live count.
    useEffect(() => {
      return () => {
        // Arrivals shown at the bottom while unfocused were never seen: they
        // leave as unread too, as leaveLatest folds them on a scroll-up.
        const unseen = arrivedWhileAwayRef.current + unseenAtBottomRef.current;
        if (unseen > 0 && onUnseenOnLeaveRef.current) {
          onUnseenOnLeaveRef.current(unseen);
        }
        // The owner may stay mounted under the same key (a DM switching into
        // its call view): whatever was seen was seen before now, and nothing
        // arriving while the list is absent is.
        onLatestLeftRef.current?.();
      };
    }, []);

    // Reading position across channel/DM switches (persistenceKey).
    //
    // Landing runs once per key, the first time the list has rows, and takes
    // the first rule that applies:
    //   1. a saved anchor → put that message back where it was;
    //   2. unread messages that overflow the viewport → open at the first
    //      unread so the user reads forward, with Return to Latest offered
    //      and the badge showing how many they have to go;
    //   3. otherwise → the latest message, following it as media resolves.
    // Rules 1 and 2 mark the list as not following; geometry decides only the
    // button. Leaving saves an anchor only when the user was above the Return
    // to Latest threshold; from the bottom it clears the entry, so the next
    // visit lands on the latest message. The cleanup pairs every landing with
    // a leave, so a key change on a surviving instance, a StrictMode replay,
    // and a list that empties and refills all land afresh. There is no pixel
    // offset anywhere: GIF and image rows are skeletons until their bytes
    // resolve, so a scrollTop measured after they settled lands short when
    // replayed before they do, and pinning isNearBottom=false on top of that
    // disabled the re-pin that would have corrected it — the recurring
    // "soft-lock above the bottom".
    //
    // Rule 2 was tried and pulled once already: the server read marker only
    // advanced when a thread was opened, so a message that arrived while the
    // user was viewing came back unread after a refresh and the landing
    // reproduced the soft-lock on rows the user had already read. onLatestSeen
    // (wired to useReadMarker in the parent) now advances the marker while the
    // thread stays open, so the count this rule reads is accurate again.
    //
    // Known ceiling: rule 2 indexes from the END of whatever rows are mounted,
    // so a stale local cache missing the newest messages lands a few rows
    // early — a gap the read-marker fix above does not touch, since it's about
    // the client's row cache, not the server's count.
    const hasRows = messages.length > 0;
    useLayoutEffect(() => {
      if (!persistenceKey || !hasRows) return;
      const list = listRef.current;
      if (!list) return;

      if (landedKeyRef.current !== persistenceKey) {
        landedKeyRef.current = persistenceKey;
        const anchor = useChannelScrollStore.getState().getAnchor(persistenceKey);
        const target = resolveLandingTarget(
          list,
          messagesRef.current,
          anchor,
          unreadOnOpen,
          currentUserId
        );
        const { following, before } = applyLanding(list, target);
        // Neither rule 1 nor rule 2 is "following". The ref is set from that
        // fact, not from geometry: at skeleton height the viewport can read
        // near the bottom, and a ref derived from it re-pinned the list on
        // the next media resize. Geometry decides only the button, here and
        // again on every resize. Explicit for the bottom landing too: the ref
        // survives a key change on a mounted instance and a StrictMode
        // replay, and applyLanding pinned the list rather than leaving it to
        // the messages effect, whose deps do not change on a key change.
        isNearBottomRef.current = following;
        setShowScrollButton(!following && !isNearBottom(list));
        // For both rules the badge shows how many unread rows are still
        // below the viewport, re-counted on every scroll: the open-time read
        // already marked them read server-side, so they are not reported on
        // leave and the badge does not survive a restart.
        if (!following) {
          seededRef.current = unreadOnOpen;
          refreshBadge(list);
        }
        // A count beyond the mounted rows means the true first unread is on
        // an older page. Landing at the top of the loaded page assigns a
        // scrollTop that is already 0, which fires no scroll event, so the
        // load-more check in handleScroll would never run: request it here.
        // The landing does not re-run when that page prepends; the user reads
        // up from where the loaded history starts.
        const { hasMore, onLoadMore, isLoading } = loadMoreRef.current;
        if (target?.exhausted && hasMore && onLoadMore && !isLoading) onLoadMore();
        // The align fires a scroll event a frame later. The handler must
        // recognise that echo and not re-decide from whatever geometry the
        // viewport has at that instant (siblings below the list are still
        // settling their mount-time layout; StrictMode replays every one of
        // them in dev). Armed only when the list moved, and never cleared
        // here, so a StrictMode replay that lands on the same scrollTop keeps
        // the first landing's pending echo armed. Not armed for a bottom
        // landing: there the ref is true, and eating the first user scroll-up
        // would keep it true.
        if (!following && list.scrollTop !== before) landingScrollTopRef.current = list.scrollTop;
      }

      // Use the element captured at setup so cleanup measures the same node;
      // it is still attached when a deleted component's layout cleanup runs.
      return () => {
        landedKeyRef.current = null;
        recordLeave(list, persistenceKey, isNearBottomRef.current);
      };
    }, [persistenceKey, hasRows, unreadOnOpen, currentUserId, refreshBadge]);

    // Others' rows arriving while following: seen if the window is visible
    // AND actually being looked at (focused) — advance the read marker.
    // Visible-but-unfocused (alt-tabbed, another desktop) is not "seen"; every
    // row is remembered and marked when focus returns, or counted as unread
    // if the user scrolls up or leaves first. Own rows are filtered out by the
    // caller: the server excludes them.
    const noteArrivalAtBottom = useCallback((others: MessageWithStatus[]) => {
      if (others.length === 0) return;
      if (document.visibilityState === 'visible' && document.hasFocus()) {
        unseenAtBottomRef.current = 0;
        onLatestSeenRef.current?.();
      } else {
        if (unseenAtBottomRef.current === 0) firstUnseenIdRef.current = others[0].id;
        unseenAtBottomRef.current += others.length;
      }
    }, []);

    // The empty list: nothing to follow, plus the bookkeeping that tells an
    // empty thread's first live message from hydration (emptyAfterLoadRef).
    const noteEmptyList = useCallback(() => {
      // A list that empties while mounted (a purge) is a leave: whatever was
      // seen was seen before now, so the parent flushes its pending marker
      // before a replacement row can be committed under it; arrivals that
      // were only shown are forgotten, since nothing remains to report.
      const purged = prevIdsRef.current.size > 0;
      if (purged) onLatestLeftRef.current?.();
      unseenAtBottomRef.current = 0;
      firstUnseenIdRef.current = null;
      prevLastMessageIdRef.current = null;
      prevIdsRef.current = new Set();
      arrivedWhileAwayRef.current = 0;
      seededRef.current = 0;
      // Loaded empty, or purged after rows were shown: either way the thread
      // is past hydration, so its next row is a live arrival. Without the
      // purge case the first row after a purge would be read as hydration
      // (prevId is null again) and never marked or counted.
      if (isLoading) emptyAfterLoadRef.current = false;
      else if (prevLoadingRef.current || purged) {
        emptyAfterLoadRef.current = true;
        hydratedRef.current = true;
      }
      prevLoadingRef.current = isLoading;
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets newMessageCount to 0 when messages list clears; not a render loop
      setNewMessageCount(0);
    }, [isLoading]);

    // Follow the latest row: pin, clear the badge, and note an arrival from
    // another user (seen if focused, remembered if not).
    const followLatest = useCallback(
      (list: HTMLElement | null, others: MessageWithStatus[]) => {
        isNearBottomRef.current = true;
        if (list) list.scrollTop = list.scrollHeight;
        // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: the badge clears when the list follows the latest row; not a render loop
        setNewMessageCount(0);
        arrivedWhileAwayRef.current = 0;
        seededRef.current = 0;
        // New rows (not the same row re-rendering, e.g. an edit).
        noteArrivalAtBottom(others);
      },
      [noteArrivalAtBottom]
    );

    // Whether a change of the latest row is a live arrival: not during the
    // first loading cycle for this thread (hydration), and not on the very
    // first rows unless the thread had finished loading empty.
    const isArrival = useCallback(
      (prevId: string | null): boolean => {
        const hydration = (isLoading || prevLoadingRef.current) && !hydratedRef.current;
        if (hydration) return false;
        return prevId !== null || emptyAfterLoadRef.current;
      },
      [isLoading]
    );

    // A key change on a surviving instance is a new thread, never an arrival:
    // forget the previous thread's rows and loading history so the new one
    // hydrates as a mount would. Declared before the messages effect so it
    // runs first in the same commit.
    useEffect(() => {
      prevLastMessageIdRef.current = null;
      prevIdsRef.current = new Set();
      hydratedRef.current = false;
      emptyAfterLoadRef.current = false;
    }, [persistenceKey]);

    // Auto-scroll when messages change (new message arrives) if user is near bottom.
    // Also track new messages arriving while the user is scrolled up. Runs on
    // a loading change too, so a fetch that ends on an empty list is seen.
    useEffect(() => {
      if (messages.length === 0) {
        noteEmptyList();
        return;
      }

      const lastMessage = messages.at(-1);
      if (!lastMessage) return; // unreachable: length check above guarantees at-least-one
      const lastId = lastMessage.id;
      const list = listRef.current;
      const prevId = prevLastMessageIdRef.current;
      // What arrived: the rows that were not here before, wherever they sit
      // below the first row the list already held — a reconnect inserts
      // missed rows BEFORE a preserved live tail, so the latest id alone does
      // not say. Rows ABOVE that first held row are an older page prepended
      // by pagination, history rather than arrivals; counting them would badge
      // and report old rows as unread. With nothing held, every row is new.
      const prevIds = prevIdsRef.current;
      const prevFirstIdx = messages.findIndex((m) => prevIds.has(m.id));
      const appended = messages.filter((m, i) => !prevIds.has(m.id) && i > prevFirstIdx);
      const arrived = appended.length > 0 && isArrival(prevId);
      const prevLast = arrived && list && prevId !== null ? findRow(list, prevId) : null;
      const others = arrived ? appended.filter((m) => m.user_id !== currentUserId) : [];

      // Follow the latest row, or start following when a message lands while
      // the bottom was already in view: a restored anchor can sit inside the
      // band at skeleton height, and content can shrink under a still
      // viewport. "Was" is measured on the previous latest row, because the
      // new one has already grown the content by the time this runs. Only an
      // arrival may promote geometry to following — on mount the geometry is
      // skeleton-height and the landing has decided.
      if (isNearBottomRef.current || (list && prevLast && rowNearBottom(list, prevLast))) {
        if (!(list && standOnAppended(list, others))) followLatest(list, others);
      } else if (others.length > 0) {
        // Scrolled up and others' messages arrived — count them.
        arrivedWhileAwayRef.current += others.length;
        setNewMessageCount((c) => c + others.length);
      }

      prevLastMessageIdRef.current = lastId;
      prevIdsRef.current = new Set(messages.map((m) => m.id));
      if (!isLoading && prevLoadingRef.current) hydratedRef.current = true;
      prevLoadingRef.current = isLoading;
      emptyAfterLoadRef.current = false;
    }, [
      messages,
      isLoading,
      currentUserId,
      isArrival,
      noteEmptyList,
      followLatest,
      standOnAppended,
    ]);

    // Arrivals shown at the bottom while the window was hidden or unfocused
    // are marked once it is both visible and focused again, if the list is
    // still following — and only if they are still on screen. A burst that
    // outgrows the viewport normally stands on its first row as it arrives
    // (standOnAppended); this covers a viewport that shrank under a burst
    // that had fitted: focus alone saw the tail, so the list stands on the
    // first unseen row the way unread at open is (rule 2) and the badge
    // counts what lies below.
    useEffect(() => {
      const onRegain = () => {
        if (unseenAtBottomRef.current === 0 || !isNearBottomRef.current) return;
        if (document.visibilityState !== 'visible' || !document.hasFocus()) return;
        const list = listRef.current;
        const first =
          list && firstUnseenIdRef.current ? findRow(list, firstUnseenIdRef.current) : null;
        if (list && first && standOnUnread(list, first)) {
          isNearBottomRef.current = false;
          leaveLatest();
          refreshBadge(list);
          setShowScrollButton(true);
          return;
        }
        unseenAtBottomRef.current = 0;
        onLatestSeenRef.current?.();
      };
      document.addEventListener('visibilitychange', onRegain);
      window.addEventListener('focus', onRegain);
      return () => {
        document.removeEventListener('visibilitychange', onRegain);
        window.removeEventListener('focus', onRegain);
      };
    }, [leaveLatest, refreshBadge]);

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
          // Layout moved under a still viewport: the button and the badge
          // are both geometry, so both are re-derived.
          setShowScrollButton(!isNearBottom(list));
          refreshBadge(list);
        }
      });
      observer.observe(content);
      observer.observe(list);
      return () => observer.disconnect();
    }, [messages.length, refreshBadge]);

    const scrollToBottom = useCallback((smooth = true) => {
      // Reaching the bottom with unseen messages pending counts as reading
      // them, whoever they're from — the count only ever holds others' messages.
      if (newMessageCountRef.current > 0) {
        onLatestSeenRef.current?.();
      }
      setNewMessageCount(0);
      arrivedWhileAwayRef.current = 0;
      seededRef.current = 0;
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

      const wasFollowing = isNearBottomRef.current;
      isNearBottomRef.current = isNearBottom(list);

      if (isNearBottomRef.current) {
        // Scrolling down into unseen messages counts as reading them, same
        // as Return to Latest — the count only ever holds others' messages.
        if (newMessageCountRef.current > 0) {
          onLatestSeenRef.current?.();
        }
        setNewMessageCount(0);
        arrivedWhileAwayRef.current = 0;
        seededRef.current = 0;
      } else {
        if (wasFollowing) leaveLatest();
        refreshBadge(list);
      }

      const shouldShow = !isNearBottomRef.current;
      if (shouldShow !== showScrollButton) {
        setShowScrollButton(shouldShow);
      }
    }, [hasMore, onLoadMore, isLoading, showScrollButton, leaveLatest, refreshBadge]);

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
