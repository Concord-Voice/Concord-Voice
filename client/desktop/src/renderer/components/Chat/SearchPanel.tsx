import React, { useState, useCallback, useRef, useEffect, useId } from 'react';
import { X, Search } from 'lucide-react';
import {
  useChannelSearch,
  type SearchResultItem,
  type SearchStatus,
} from '../../hooks/messaging/useChannelSearch';
import {
  JUMP_FAILURE_COPY,
  type JumpFailure,
  type JumpOutcome,
} from '../../hooks/messaging/useJumpToMessage';
import { ACTIVE_SEARCH_PROFILE } from '../../services/messaging/searchProfile';
import { focusSurfaceComposer } from './chatSurface';
import './SearchPanel.css';

export interface SearchPanelProps {
  channelId: string;
  isOpen: boolean;
  onClose: () => void;
  /** Jumps to a message, switching channel first when it lives in another one (#3894). */
  onJump: (channelId: string, messageId: string, signal?: AbortSignal) => Promise<JumpOutcome>;
  /** The owner's chat surface, so a jump that leaves this channel can focus that surface's composer. */
  surfaceId: string;
  /** Channel IDs for server-wide search (only in server context, not DMs) */
  accessibleChannelIds?: string[];
  /** Whether to show server-wide toggle (false for DMs) */
  showServerWideToggle?: boolean;
  /**
   * Where focus goes when the panel closes itself (Close, the backdrop, Escape): the control that
   * opened it. A Jump is left out, so the scroll it asks for decides where focus lands.
   */
  returnFocusRef?: React.RefObject<HTMLElement | null>;
}

/** What the one card whose Jump was pressed shows. */
type CardJump =
  { id: string; phase: 'busy' } | { id: string; phase: 'failed'; outcome: JumpFailure };

function truncateContent(content: string, maxLength = 120): string {
  const codePoints = Array.from(content);
  if (codePoints.length <= maxLength) return content;
  return codePoints.slice(0, maxLength).join('') + '...';
}

const formatDay = (iso: string) =>
  new Date(iso).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });

/** A channel the index cannot hold more of: how far it reached, and why it stopped. */
function fullText(reachedAt: string | null): string {
  const why = "Older messages aren't searched, to limit memory use.";
  return reachedAt ? `Searched back to ${formatDay(reachedAt)}. ${why}` : why;
}

/** A chunk that ended with more to search: how far it reached, or that nothing was searched. */
function moreText(reachedAt: string | null, hasResults: boolean): string {
  if (!reachedAt) return "Older messages haven't been searched.";
  return hasResults
    ? `Searched back to ${formatDay(reachedAt)}.`
    : `No matches since ${formatDay(reachedAt)}.`;
}

function finishedText(hasResults: boolean): string {
  return hasResults ? 'Searched all messages.' : 'No results found. Searched all messages.';
}

// Spoken once per change by the always-mounted live region. The paused countdown
// and the visible status lines stay outside it, so a tick never re-announces.
const ANNOUNCEMENTS: Partial<Record<SearchStatus['kind'], string>> = {
  running: 'Searching older messages',
  paused: 'Search paused to avoid overloading the server.',
};

// Where the visible status line has a sentence, the live region speaks the same one, so a screen
// reader hears how a chunk ended. A failure is the line's own alert, not announced here.
function announcement(status: SearchStatus, hasResults: boolean): string {
  switch (status.kind) {
    case 'more':
      return moreText(status.reachedAt, hasResults);
    case 'full':
      return fullText(status.reachedAt);
    case 'finished':
      return finishedText(hasResults);
    default:
      return ANNOUNCEMENTS[status.kind] ?? '';
  }
}

function PausedLine({ resumeAt }: Readonly<{ resumeAt: number }>) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  const seconds = Math.max(0, Math.ceil((resumeAt - now) / 1000));
  return (
    <p className="search-panel-status">
      Paused to avoid overloading the server. Resuming in {seconds} s.
    </p>
  );
}

function SearchStatusLine({
  status,
  hasResults,
  onMore,
}: Readonly<{ status: SearchStatus; hasResults: boolean; onMore: () => void }>) {
  switch (status.kind) {
    case 'running':
      return (
        <p className="search-panel-status">
          {status.reachedAt ? `Searching… back to ${formatDay(status.reachedAt)}` : 'Searching…'}
        </p>
      );
    case 'more':
      return (
        <div className="search-panel-more">
          <p className="search-panel-status">{moreText(status.reachedAt, hasResults)}</p>
          <button type="button" className="search-panel-more-button" onClick={onMore}>
            Search older messages
          </button>
        </div>
      );
    case 'paused':
      return <PausedLine resumeAt={status.resumeAt} />;
    case 'failed':
      return (
        <div className="search-panel-more">
          <p className="search-panel-status" role="alert">
            Search stopped.
          </p>
          <button type="button" className="search-panel-more-button" onClick={onMore}>
            Try again
          </button>
        </div>
      );
    case 'full':
      return <p className="search-panel-status">{fullText(status.reachedAt)}</p>;
    case 'finished':
      return <p className="search-panel-status">{finishedText(hasResults)}</p>;
    default:
      return null;
  }
}

const SearchPanel: React.FC<SearchPanelProps> = ({
  channelId,
  isOpen,
  onClose,
  onJump,
  surfaceId,
  accessibleChannelIds,
  showServerWideToggle = false,
  returnFocusRef,
}) => {
  const headingId = useId();
  const [query, setQuery] = useState('');
  const [serverWide, setServerWide] = useState(false);
  const [cardJump, setCardJump] = useState<CardJump | null>(null);
  const backdropRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const statusLineRef = useRef<HTMLDivElement>(null);
  const jumpAbortRef = useRef<AbortController | null>(null);

  const scopes = serverWide && accessibleChannelIds ? accessibleChannelIds : undefined;
  const { results, status, search, loadMore, cancel, endSession } = useChannelSearch(channelId, {
    scopes,
  });

  // A card's jump state ends with the card: closing the panel, or results that no
  // longer include it. Dropped during render, like the hook's channel notice, so no
  // effect sets state and a card that comes back later does not show a stale line.
  if (cardJump !== null && (!isOpen || !results.some((r) => r.id === cardJump.id))) {
    setCardJump(null);
  }

  // Nothing is left to report to once the state is gone. A jump that ended some
  // other way is already settled, so aborting it again changes nothing.
  useEffect(() => {
    if (cardJump === null) jumpAbortRef.current?.abort();
  }, [cardJump]);

  // Scroll-driven, never visibility-driven: a short list cannot start a chain of chunks.
  const handleResultsScroll = useCallback(
    (e: React.UIEvent<HTMLDivElement>) => {
      if (status.kind !== 'more') return;
      const el = e.currentTarget;
      const fromEnd = el.scrollHeight - el.scrollTop - el.clientHeight;
      if (fromEnd < ACTIVE_SEARCH_PROFILE.prefetchViewports * el.clientHeight) loadMore();
    },
    [status.kind, loadMore]
  );

  // "Search older messages" and "Try again" unmount once the chunk starts. Focus goes to
  // the status line around them, which stays mounted, instead of falling to <body>.
  const handleMore = useCallback(() => {
    loadMore();
    statusLineRef.current?.focus();
  }, [loadMore]);

  const handleQueryChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const newQuery = e.target.value;
      setQuery(newQuery);
      setCardJump(null);
      if (newQuery.trim()) {
        search(newQuery);
      } else {
        cancel();
      }
    },
    [search, cancel]
  );

  const handleJump = useCallback(
    (result: SearchResultItem) => {
      if (cardJump?.phase === 'busy' && cardJump.id === result.id) return;
      jumpAbortRef.current?.abort();
      const crossing = result.channelId !== channelId;
      const controller = new AbortController();
      jumpAbortRef.current = controller;
      setCardJump({ id: result.id, phase: 'busy' });
      const outcome = onJump(result.channelId, result.id, crossing ? undefined : controller.signal);
      if (crossing) {
        // The switch ends this search session, so the jump is not tied to the panel and
        // is not aborted when it closes. Focus moves now so it is never on <body>.
        onClose();
        focusSurfaceComposer(surfaceId);
      }
      void outcome.then((o) => {
        if (crossing || controller.signal.aborted) return;
        if (o === 'reached') {
          onClose();
          setCardJump(null);
        } else if (o === 'cancelled') {
          setCardJump(null);
        } else {
          setCardJump({ id: result.id, phase: 'failed', outcome: o });
        }
      });
    },
    [cardJump, channelId, onJump, onClose, surfaceId]
  );

  // The panel closes itself here (Close, the backdrop, Escape). Its focused control unmounts with
  // it, so focus goes back to the control that opened it instead of falling to <body>.
  const handleClose = useCallback(() => {
    endSession();
    setQuery('');
    onClose();
    returnFocusRef?.current?.focus();
  }, [endSession, onClose, returnFocusRef]);

  // Escape closes the panel when it is typed inside it. The listener sits on the panel's own
  // element, not the page: it hears only keys aimed at the panel, so a modal dialog in front keeps
  // its own Escape (frontend.md rule 8). Imperative rather than a React onKeyDown on the backdrop
  // div, which SonarQube flags as a listener on a non-interactive element (S6847).
  useEffect(() => {
    const root = backdropRef.current;
    if (!isOpen || !root) return;
    const onKeyDown = (e: KeyboardEvent) => {
      // The Escape that ends an IME composition belongs to the composition.
      if (e.key !== 'Escape' || e.isComposing) return;
      e.stopPropagation();
      handleClose();
    };
    root.addEventListener('keydown', onKeyDown);
    return () => root.removeEventListener('keydown', onKeyDown);
  }, [isOpen, handleClose]);

  // Auto-focus input when panel opens
  useEffect(() => {
    if (isOpen && inputRef.current) {
      inputRef.current.focus();
    }
  }, [isOpen]);

  // Clear the query when the channel changes; the hook ends its own session then
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets query when channel changes to avoid stale results; not a render loop
    setQuery('');
  }, [channelId]);

  // End the search session (cancel, release records) when the panel closes
  useEffect(() => {
    if (!isOpen) {
      endSession();
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears query when panel closes; not a render loop
      setQuery('');
    }
  }, [isOpen, endSession]);

  // Re-run search when server-wide scope changes
  useEffect(() => {
    if (query.trim()) {
      search(query);
    }
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- intentional: effect should only refire when the serverWide toggle changes, not on every keystroke that updates `query` or every new `search` identity; `query` is read as a snapshot-at-toggle
  }, [serverWide]);

  if (!isOpen) return null;

  return (
    <div ref={backdropRef} className="search-panel-backdrop">
      <button
        type="button"
        className="search-panel-backdrop-dismiss"
        onClick={handleClose}
        aria-label="Close search"
      />
      <section className="search-panel" aria-labelledby={headingId}>
        <div className="search-panel-header">
          <h3 id={headingId}>Search Messages</h3>
          <button className="search-panel-close" onClick={handleClose} aria-label="Close">
            <X size={18} />
          </button>
        </div>

        <div className="search-panel-input-wrapper">
          <Search size={16} className="search-panel-input-icon" />
          <input
            ref={inputRef}
            type="text"
            className="search-panel-input"
            placeholder="Search messages..."
            aria-label="Search messages"
            value={query}
            onChange={handleQueryChange}
            autoComplete="off"
          />
        </div>

        {showServerWideToggle && (
          <label className="search-panel-toggle">
            <input
              type="checkbox"
              checked={serverWide}
              onChange={(e) => setServerWide(e.target.checked)}
            />{' '}
            <span>Search all channels in this server</span>
          </label>
        )}

        <div className="search-panel-results" onScroll={handleResultsScroll}>
          {results.map((result) => (
            <SearchResultCard
              key={result.id}
              result={result}
              jump={cardJump?.id === result.id ? cardJump : undefined}
              onJump={handleJump}
            />
          ))}

          {query.trim() && (
            <div ref={statusLineRef} tabIndex={-1} className="search-panel-status-line">
              <SearchStatusLine
                status={status}
                hasResults={results.length > 0}
                onMore={handleMore}
              />
            </div>
          )}
        </div>

        <output className="search-panel-live">{announcement(status, results.length > 0)}</output>
      </section>
    </div>
  );
};

function SearchResultCard({
  result,
  jump,
  onJump,
}: Readonly<{
  result: SearchResultItem;
  jump: CardJump | undefined;
  onJump: (result: SearchResultItem) => void;
}>) {
  const errorId = useId();
  const errorRef = useRef<HTMLParagraphElement>(null);
  const busy = jump?.phase === 'busy';
  const failure = jump?.phase === 'failed' ? jump.outcome : null;
  // A card scrolled partly out of the results list would hide the line that says why Jump failed.
  useEffect(() => {
    if (failure) errorRef.current?.scrollIntoView({ block: 'nearest' });
  }, [failure]);
  return (
    <div className="search-result-card">
      <div className="search-result-meta">
        <span className="search-result-author">{result.authorName}</span>
        <span className="search-result-time">
          {new Date(result.createdAt).toLocaleDateString()}
        </span>
      </div>
      <div className="search-result-content">{truncateContent(result.content)}</div>
      {/* aria-disabled, never disabled: a natively disabled button drops focus to <body>. */}
      <button
        type="button"
        className="search-result-jump"
        aria-disabled={busy ? 'true' : undefined}
        aria-describedby={failure ? errorId : undefined}
        onClick={() => onJump(result)}
      >
        {busy ? 'Jumping…' : 'Jump'}
      </button>
      {failure && (
        <p id={errorId} ref={errorRef} role="alert" className="search-result-jump-error">
          {JUMP_FAILURE_COPY[failure]}
        </p>
      )}
    </div>
  );
}

export default SearchPanel;
