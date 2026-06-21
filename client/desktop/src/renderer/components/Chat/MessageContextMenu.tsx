import React from 'react';
import { MessageWithStatus } from '../../types/chat';
import { copyText } from '../../utils/clipboard';
import ContextMenu from '../ui/ContextMenu';

interface MessageContextMenuProps {
  message: MessageWithStatus;
  position: { x: number; y: number };
  isOwnMessage: boolean;
  canModify: boolean;
  onClose: () => void;
  onEdit: () => void;
  onDelete: () => void;
  onReaction?: () => void;
  onReply?: () => void;
  onPin?: () => void;
  isPinned?: boolean;
  canPin?: boolean;
}

const MessageContextMenu: React.FC<MessageContextMenuProps> = ({
  message,
  position,
  isOwnMessage: _isOwnMessage,
  canModify,
  onClose,
  onEdit,
  onDelete,
  onReaction,
  onReply,
  onPin,
  isPinned,
  canPin,
}) => {
  const handleCopyText = async () => {
    await copyText(message.content);
    onClose();
  };

  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <rect
              x="5"
              y="5"
              width="9"
              height="9"
              rx="1.5"
              stroke="currentColor"
              strokeWidth="1.5"
            />
            <path
              d="M11 5V3.5A1.5 1.5 0 009.5 2h-6A1.5 1.5 0 002 3.5v6A1.5 1.5 0 003.5 11H5"
              stroke="currentColor"
              strokeWidth="1.5"
            />
          </svg>
        }
        label="Copy Text"
        onClick={handleCopyText}
      />

      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M14 8c0 3.31-2.69 6-6 6H4l-2 2V8c0-3.31 2.69-6 6-6s6 2.69 6 6z"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <path d="M6 7h4M6 10h2" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          </svg>
        }
        label="Reply"
        disabled={!onReply}
        onClick={() => {
          onReply?.();
          onClose();
        }}
      />

      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M9 2L5 8h3v6l4-6H9V2z"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        }
        label={isPinned ? 'Unpin Message' : 'Pin Message'}
        disabled={!canPin}
        onClick={() => {
          onPin?.();
          onClose();
        }}
      />

      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <circle cx="8" cy="8" r="6.5" stroke="currentColor" strokeWidth="1.5" />
            <circle cx="5.5" cy="6.5" r="1" fill="currentColor" />
            <circle cx="10.5" cy="6.5" r="1" fill="currentColor" />
            <path
              d="M5 10c.8 1.2 2.2 2 3.5 1.8 1.3-.2 2.2-1 2.5-1.8"
              stroke="currentColor"
              strokeWidth="1.2"
              strokeLinecap="round"
            />
          </svg>
        }
        label="Add Reaction"
        onClick={() => {
          onReaction?.();
          onClose();
        }}
      />

      {canModify && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M11.5 1.5l3 3L5 14H2v-3L11.5 1.5z"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          }
          label="Edit Message"
          onClick={() => {
            onEdit();
            onClose();
          }}
        />
      )}

      {canModify && (
        <>
          <ContextMenu.Separator />
          <ContextMenu.Item
            icon={
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                <path
                  d="M2 4h12M5.33 4V2.67a1.33 1.33 0 011.34-1.34h2.66a1.33 1.33 0 011.34 1.34V4M12.67 4v9.33a1.33 1.33 0 01-1.34 1.34H4.67a1.33 1.33 0 01-1.34-1.34V4"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            }
            label="Delete Message"
            danger
            onClick={() => {
              onDelete();
              onClose();
            }}
          />
        </>
      )}
    </ContextMenu>
  );
};

export default MessageContextMenu;
