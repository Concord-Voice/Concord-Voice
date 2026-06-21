import React, { useState, useCallback, useRef } from 'react';
import { Smile } from 'lucide-react';
import type { ReactionSummary } from '../../types/chat';
import { toggleReaction } from '../../services/reactionService';
import LazyEmojiPicker from '../EmojiPicker/LazyEmojiPicker';
import './ReactionBar.css';

export interface ReactionBarProps {
  messageId: string;
  reactions: ReactionSummary[];
}

const ReactionBar: React.FC<ReactionBarProps> = ({ messageId, reactions }) => {
  const [showEmojiPicker, setShowEmojiPicker] = useState(false);
  const [pickerPosition, setPickerPosition] = useState<{ x: number; y: number }>({ x: 0, y: 0 });
  const addBtnRef = useRef<HTMLButtonElement>(null);

  const handleToggle = useCallback(
    async (emoji: string) => {
      try {
        await toggleReaction(messageId, emoji);
      } catch {
        // WebSocket will update store on success; errors are non-fatal for UI
      }
    },
    [messageId]
  );

  const handlePickerSelect = useCallback(
    (emoji: string) => {
      setShowEmojiPicker(false);
      handleToggle(emoji);
    },
    [handleToggle]
  );

  const openPicker = useCallback(() => {
    if (addBtnRef.current) {
      const rect = addBtnRef.current.getBoundingClientRect();
      setPickerPosition({ x: rect.left, y: rect.top - 8 });
    }
    setShowEmojiPicker(true);
  }, []);

  if (reactions.length === 0) return null;

  return (
    <div className="reaction-bar">
      {reactions.map((r) => (
        <button
          key={r.emoji}
          className={`reaction-chip ${r.me ? 'reaction-chip-active' : ''}`}
          onClick={() => handleToggle(r.emoji)}
          title={r.users.map((u) => u.display_name || u.username).join(', ')}
        >
          <span className="reaction-emoji">{r.emoji}</span>
          <span className="reaction-count">{r.count}</span>
        </button>
      ))}
      <button
        ref={addBtnRef}
        className="reaction-add-btn"
        onClick={openPicker}
        aria-label="Add reaction"
      >
        <Smile size={14} />
      </button>
      {showEmojiPicker && (
        <LazyEmojiPicker
          onSelect={handlePickerSelect}
          onClose={() => setShowEmojiPicker(false)}
          mode="popover"
          position={pickerPosition}
        />
      )}
    </div>
  );
};

export default React.memo(ReactionBar);
