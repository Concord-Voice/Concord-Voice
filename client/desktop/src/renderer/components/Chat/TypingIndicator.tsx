import React, { useEffect, useState } from 'react';
import { useChatStore, TypingUser } from '../../stores/chatStore';
import './TypingIndicator.css';

export interface TypingIndicatorProps {
  channelId: string;
}

const TypingIndicator: React.FC<TypingIndicatorProps> = ({ channelId }) => {
  const getTypingUsers = useChatStore((state) => state.getTypingUsers);
  const clearOldTypingIndicators = useChatStore((state) => state.clearOldTypingIndicators);
  const [typingUsers, setTypingUsers] = useState<TypingUser[]>([]);

  // Subscribe to typing changes and periodically clean stale indicators
  useEffect(() => {
    const updateTyping = () => {
      const users = getTypingUsers(channelId);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: refreshes typingUsers from store snapshot on each poll tick and channelId change; not a render loop
      setTypingUsers(users);
    };

    // Initial check
    updateTyping();

    // Poll for updates and clean stale indicators every 2 seconds
    const interval = setInterval(() => {
      clearOldTypingIndicators(channelId);
      updateTyping();
    }, 2000);

    // Subscribe to typing map changes only (not all store changes)
    let prev = useChatStore.getState().typingByChannel.get(channelId);
    const unsub = useChatStore.subscribe((state) => {
      const next = state.typingByChannel.get(channelId);
      if (next !== prev) {
        prev = next;
        updateTyping();
      }
    });

    return () => {
      clearInterval(interval);
      unsub();
    };
  }, [channelId, getTypingUsers, clearOldTypingIndicators]);

  const formatTypingText = (users: TypingUser[]): string => {
    if (users.length === 0) return '';
    if (users.length === 1) {
      return `${users[0].username || 'Someone'} is typing`;
    }
    if (users.length === 2) {
      const name1 = users[0].username || 'Someone';
      const name2 = users[1].username || 'Someone';
      return `${name1} and ${name2} are typing`;
    }
    if (users.length === 3) {
      const name1 = users[0].username || 'Someone';
      const name2 = users[1].username || 'Someone';
      const name3 = users[2].username || 'Someone';
      return `${name1}, ${name2}, and ${name3} are typing`;
    }
    return 'Several people are typing';
  };

  return (
    <div className="typing-indicator-container">
      {typingUsers.length > 0 && (
        <span className="typing-indicator-text">
          {formatTypingText(typingUsers)}
          <span className="typing-dots">
            <span className="typing-dot" />
            <span className="typing-dot" />
            <span className="typing-dot" />
          </span>
        </span>
      )}
    </div>
  );
};

export default TypingIndicator;
