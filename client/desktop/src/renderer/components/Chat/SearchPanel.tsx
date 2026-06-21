import React, { useState, useCallback, useRef, useEffect } from 'react';
import { X, Search } from 'lucide-react';
import { useChannelSearch } from '../../hooks/useChannelSearch';
import type { MessageWithStatus } from '../../types/chat';
import './SearchPanel.css';

export interface SearchPanelProps {
  channelId: string;
  isOpen: boolean;
  onClose: () => void;
  onScrollToMessage: (messageId: string) => void;
  /** Channel IDs for server-wide search (only in server context, not DMs) */
  accessibleChannelIds?: string[];
  /** Whether to show server-wide toggle (false for DMs) */
  showServerWideToggle?: boolean;
}

function truncateContent(content: string, maxLength = 120): string {
  const codePoints = Array.from(content);
  if (codePoints.length <= maxLength) return content;
  return codePoints.slice(0, maxLength).join('') + '...';
}

const SearchPanel: React.FC<SearchPanelProps> = ({
  channelId,
  isOpen,
  onClose,
  onScrollToMessage,
  accessibleChannelIds,
  showServerWideToggle = false,
}) => {
  const [query, setQuery] = useState('');
  const [serverWide, setServerWide] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const scopes = serverWide && accessibleChannelIds ? accessibleChannelIds : undefined;
  const { results, isSearching, progress, search, cancel } = useChannelSearch(channelId, {
    scopes,
  });

  const handleQueryChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const newQuery = e.target.value;
      setQuery(newQuery);
      if (newQuery.trim()) {
        search(newQuery);
      } else {
        cancel();
      }
    },
    [search, cancel]
  );

  const handleJump = useCallback(
    (messageId: string) => {
      onScrollToMessage(messageId);
      onClose();
    },
    [onScrollToMessage, onClose]
  );

  const handleClose = useCallback(() => {
    cancel();
    setQuery('');
    onClose();
  }, [cancel, onClose]);

  // Auto-focus input when panel opens
  useEffect(() => {
    if (isOpen && inputRef.current) {
      inputRef.current.focus();
    }
  }, [isOpen]);

  // Clear search when channel changes
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets query when channel changes to avoid stale results; not a render loop
    setQuery('');
    cancel();
  }, [channelId, cancel]);

  // Cancel in-flight search when panel closes
  useEffect(() => {
    if (!isOpen) {
      cancel();
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears query when panel closes; not a render loop
      setQuery('');
    }
  }, [isOpen, cancel]);

  // Re-run search when server-wide scope changes
  useEffect(() => {
    if (query.trim()) {
      search(query);
    }
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- intentional: effect should only refire when the serverWide toggle changes, not on every keystroke that updates `query` or every new `search` identity; `query` is read as a snapshot-at-toggle
  }, [serverWide]);

  if (!isOpen) return null;

  return (
    <div className="search-panel-backdrop">
      <button
        type="button"
        className="search-panel-backdrop-dismiss"
        onClick={handleClose}
        aria-label="Close search"
      />
      <section className="search-panel">
        <div className="search-panel-header">
          <h3>Search Messages</h3>
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

        {isSearching && progress && (
          <div className="search-panel-progress">
            Searching... {progress.checked} messages checked
          </div>
        )}

        <div className="search-panel-results">
          {!isSearching && query.trim() && results.length === 0 && (
            <div className="search-panel-empty">No results found</div>
          )}

          {results.map((msg) => (
            <SearchResultCard key={msg.id} message={msg} onJump={handleJump} />
          ))}
        </div>
      </section>
    </div>
  );
};

function SearchResultCard({
  message,
  onJump,
}: Readonly<{
  message: MessageWithStatus;
  onJump: (id: string) => void;
}>) {
  return (
    <div className="search-result-card">
      <div className="search-result-meta">
        <span className="search-result-author">{message.display_name || message.username}</span>
        <span className="search-result-time">
          {new Date(message.created_at).toLocaleDateString()}
        </span>
      </div>
      <div className="search-result-content">{truncateContent(message.content)}</div>
      <button className="search-result-jump" onClick={() => onJump(message.id)}>
        Jump
      </button>
    </div>
  );
}

export default SearchPanel;
