import React from 'react';
import ContextMenu from '../ui/ContextMenu';
import { errorMessage } from '../../utils/redactError';

interface MessageInputContextMenuProps {
  position: { x: number; y: number };
  onClose: () => void;
  onPaste: (text: string) => void;
  onOpenEmojiPicker?: () => void;
}

const MessageInputContextMenu: React.FC<MessageInputContextMenuProps> = ({
  position,
  onClose,
  onPaste,
  onOpenEmojiPicker,
}) => {
  const handlePaste = async () => {
    try {
      const text = await navigator.clipboard.readText();
      if (text) {
        onPaste(text);
      }
    } catch (err) {
      console.error('Failed to read clipboard:', errorMessage(err));
    }
    onClose();
  };

  const handleInsertEmoji = () => {
    onClose();
    // Small delay so the context menu closes before the picker opens
    setTimeout(() => {
      onOpenEmojiPicker?.();
    }, 50);
  };

  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M10.5 1H12a1.5 1.5 0 011.5 1.5v11A1.5 1.5 0 0112 15H4a1.5 1.5 0 01-1.5-1.5v-11A1.5 1.5 0 014 1h1.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <rect
              x="5.5"
              y="0.5"
              width="5"
              height="3"
              rx="1"
              stroke="currentColor"
              strokeWidth="1"
            />
          </svg>
        }
        label="Paste"
        onClick={handlePaste}
      />

      <ContextMenu.Separator />

      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M14 10v2.5a1.5 1.5 0 01-1.5 1.5h-9A1.5 1.5 0 012 12.5V10"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <path
              d="M8 2v7.5M5 6.5L8 2l3 4.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        }
        label="Upload File"
        disabled
        onClick={() => {}}
      />

      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <circle cx="8" cy="8" r="6.5" stroke="currentColor" strokeWidth="1.5" />
            <circle cx="6" cy="7" r="1" fill="currentColor" />
            <circle cx="10" cy="7" r="1" fill="currentColor" />
            <path
              d="M5.5 10c.5 1 1.5 1.5 2.5 1.5s2-.5 2.5-1.5"
              stroke="currentColor"
              strokeWidth="1.2"
              strokeLinecap="round"
            />
          </svg>
        }
        label="Insert Emoji"
        onClick={handleInsertEmoji}
      />
    </ContextMenu>
  );
};

export default MessageInputContextMenu;
