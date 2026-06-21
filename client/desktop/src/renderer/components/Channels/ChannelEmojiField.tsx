import React, { useState, useRef } from 'react';
import EmojiPicker from '../EmojiPicker/LazyEmojiPicker';

interface ChannelEmojiFieldProps {
  emoji: string;
  onChange: (emoji: string) => void;
  disabled?: boolean;
  hint?: string;
}

const ChannelEmojiField: React.FC<ChannelEmojiFieldProps> = ({
  emoji,
  onChange,
  disabled = false,
  hint = 'Click to pick a channel emoji',
}) => {
  const [showEmojiPicker, setShowEmojiPicker] = useState(false);
  const emojiPickerRef = useRef<HTMLDivElement>(null);

  const handleEmojiClick = (emojiChar: string) => {
    onChange(emojiChar);
    setShowEmojiPicker(false);
  };

  return (
    <div className="channel-form-group">
      <span className="channel-form-label">Channel Emoji (Optional)</span>
      <div className="emoji-input-wrapper" ref={emojiPickerRef}>
        <div className="emoji-input-container">
          <button
            type="button"
            className={`emoji-picker-button ${emoji ? 'has-emoji' : ''}`}
            onClick={() => setShowEmojiPicker(!showEmojiPicker)}
            disabled={disabled}
            title={emoji ? 'Change emoji' : 'Pick an emoji'}
          >
            {emoji ? (
              <span className="emoji-picker-button-emoji">{emoji}</span>
            ) : (
              <span className="emoji-picker-button-placeholder">Pick an emoji</span>
            )}
          </button>
          {emoji && (
            <button
              type="button"
              className="emoji-clear-btn"
              onClick={() => onChange('')}
              disabled={disabled}
              title="Remove emoji"
            >
              ✕
            </button>
          )}
        </div>
        {showEmojiPicker && (
          <div className="emoji-picker-container">
            <EmojiPicker
              mode="inline"
              onSelect={handleEmojiClick}
              onClose={() => setShowEmojiPicker(false)}
            />
          </div>
        )}
      </div>
      <span className="channel-form-hint">{hint}</span>
    </div>
  );
};

export default ChannelEmojiField;
