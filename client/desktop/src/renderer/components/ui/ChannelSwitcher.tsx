import React, { useState, useRef, useEffect, useMemo, useCallback } from 'react';
import { Lock, Users, MessageSquare } from 'lucide-react';
import { useKeyboardShortcutStore } from '../../stores/keyboardShortcutStore';
import { useChannelStore } from '../../stores/channelStore';
import { useDMStore } from '../../stores/dmStore';
import { useServerStore } from '../../stores/serverStore';
import { useUserStore } from '../../stores/userStore';
import { useNavigate } from 'react-router-dom';
import './ChannelSwitcher.css';

interface SwitcherItem {
  id: string;
  name: string;
  type: 'channel' | 'dm';
  icon: React.ReactNode;
  context?: string; // server name for channels
}

const ChannelSwitcher: React.FC = () => {
  const isOpen = useKeyboardShortcutStore((s) => s.channelSwitcherOpen);
  const closeSwitcher = useKeyboardShortcutStore((s) => s.closeChannelSwitcher);
  const channels = useChannelStore((s) => s.channels);
  const currentServerId = useChannelStore((s) => s.currentServerId);
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel);
  const servers = useServerStore((s) => s.servers);
  const conversations = useDMStore((s) => s.conversations);
  const setActiveConversation = useDMStore((s) => s.setActiveConversation);
  const currentUserId = useUserStore((s) => s.user?.id) || '';
  const navigate = useNavigate();

  const [query, setQuery] = useState('');
  const [selectedIndex, setSelectedIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);

  // Build the combined list of channels + DMs
  const allItems = useMemo((): SwitcherItem[] => {
    const serverMap = new Map(servers.map((s) => [s.id, s.name]));

    const channelItems: SwitcherItem[] = channels
      .filter((c) => c.type === 'text') // Only text channels
      .map((c) => ({
        id: c.id,
        name: c.name,
        type: 'channel' as const,
        icon: <Lock size={14} />,
        context: serverMap.get(currentServerId || '') || '',
      }));

    const dmItems: SwitcherItem[] = conversations
      .filter((c) => !c.isPersonal)
      .map((conv) => {
        const name = conv.isGroup
          ? conv.name || conv.participants.map((p) => p.displayName || p.username).join(', ')
          : conv.participants.find((p) => p.userId !== currentUserId)?.displayName ||
            conv.participants.find((p) => p.userId !== currentUserId)?.username ||
            'Unknown';
        return {
          id: conv.id,
          name,
          type: 'dm' as const,
          icon: conv.isGroup ? <Users size={14} /> : <MessageSquare size={14} />,
        };
      });

    return [...channelItems, ...dmItems];
  }, [channels, conversations, servers, currentServerId, currentUserId]);

  // Filter by substring match on name
  const filtered = useMemo(() => {
    if (!query) return allItems;
    const lower = query.toLowerCase();
    return allItems.filter((item) => item.name.toLowerCase().includes(lower));
  }, [allItems, query]);

  // Reset selection on filter change
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets selectedIndex to 0 when the user changes the search query — NOT on background store churn that merely rebuilds the `filtered` reference; not a render loop
    setSelectedIndex(0);
  }, [query]);

  // Focus input when opened
  useEffect(() => {
    if (isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets query when switcher opens; not a render loop
      setQuery('');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets selectedIndex when switcher opens; not a render loop
      setSelectedIndex(0);
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [isOpen]);

  // Close on backdrop click (dialog element itself, not children)
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog || !isOpen) return;
    const handleClick = (e: MouseEvent) => {
      if (e.target === dialog) closeSwitcher();
    };
    dialog.addEventListener('click', handleClick);
    return () => dialog.removeEventListener('click', handleClick);
  }, [isOpen, closeSwitcher]);

  // Scroll selected item into view
  useEffect(() => {
    const list = listRef.current;
    if (!list) return;
    const selected = list.children[selectedIndex] as HTMLElement;
    if (selected) {
      selected.scrollIntoView?.({ block: 'nearest' });
    }
  }, [selectedIndex]);

  const selectItem = useCallback(
    (item: SwitcherItem) => {
      if (item.type === 'channel') {
        setActiveChannel(item.id);
        navigate('/app');
      } else {
        setActiveConversation(item.id);
        navigate('/app/dms');
      }
      closeSwitcher();
    },
    [setActiveChannel, setActiveConversation, navigate, closeSwitcher]
  );

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          setSelectedIndex((i) =>
            filtered.length === 0 ? 0 : Math.min(i + 1, filtered.length - 1)
          );
          break;
        case 'ArrowUp':
          e.preventDefault();
          setSelectedIndex((i) => (filtered.length === 0 ? 0 : Math.max(i - 1, 0)));
          break;
        case 'Enter':
          e.preventDefault();
          if (filtered[selectedIndex]) {
            selectItem(filtered[selectedIndex]);
          }
          break;
        case 'Escape':
          e.preventDefault();
          closeSwitcher();
          break;
      }
    },
    [filtered, selectedIndex, selectItem, closeSwitcher]
  );

  if (!isOpen) return null;

  return (
    <dialog ref={dialogRef} className="channel-switcher-overlay" open>
      <div className="channel-switcher">
        <input
          ref={inputRef}
          type="text"
          className="channel-switcher-input"
          placeholder="Search channels and DMs..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={handleKeyDown}
          aria-label="Search channels"
        />
        <div className="channel-switcher-list" ref={listRef}>
          {filtered.length === 0 && <div className="channel-switcher-empty">No results found</div>}
          {filtered.map((item, index) => (
            <button
              key={item.id}
              type="button"
              className={`channel-switcher-item${index === selectedIndex ? ' selected' : ''}`}
              onClick={() => selectItem(item)}
            >
              <span className="channel-switcher-icon">{item.icon}</span>
              <span className="channel-switcher-name">{item.name}</span>
              {item.context && <span className="channel-switcher-context">{item.context}</span>}
              <span className={`channel-switcher-type-badge ${item.type}`}>
                {item.type === 'channel' ? 'Channel' : 'DM'}
              </span>
            </button>
          ))}
        </div>
      </div>
    </dialog>
  );
};

export default ChannelSwitcher;
