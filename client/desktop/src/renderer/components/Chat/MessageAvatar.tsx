import React from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { MessageWithStatus } from '../../types/chat';

export interface MessageAvatarProps {
  message: MessageWithStatus;
  showAvatar: boolean;
  senderColors?: { gradient: string } | null;
  /**
   * Opens the author's profile card anchored at the given viewport point.
   * State + resolution live in the Message parent via useMessageProfileCard
   * (#226) so the avatar and the username share one card.
   */
  onOpenProfile: (position: { x: number; y: number }) => void;
}

function getInitials(username: string) {
  return username.charAt(0).toUpperCase();
}

function formatShortTime(timestamp: string) {
  const date = new Date(timestamp);
  return date.toLocaleTimeString('en-US', {
    hour: 'numeric',
    minute: '2-digit',
    hour12: true,
  });
}

const MessageAvatar: React.FC<MessageAvatarProps> = ({
  message,
  showAvatar,
  senderColors,
  onOpenProfile,
}) => {
  if (!showAvatar) {
    return (
      <div className="message-gutter">
        <span className="message-gutter-timestamp">{formatShortTime(message.created_at)}</span>
      </div>
    );
  }

  return (
    <button
      type="button"
      className="message-avatar"
      onClick={(e) => onOpenProfile({ x: e.clientX, y: e.clientY })}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          const rect = e.currentTarget.getBoundingClientRect();
          onOpenProfile({ x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 });
        }
      }}
      aria-label="View user profile"
    >
      {resolveMediaUrl(message.avatar_url) ? (
        <img className="avatar-image" src={resolveMediaUrl(message.avatar_url)} alt={message.username} />
      ) : (
        <div
          className="avatar-circle"
          style={senderColors ? { background: senderColors.gradient } : undefined}
        >
          {getInitials(message.display_name || message.username)}
        </div>
      )}
    </button>
  );
};

export default React.memo(MessageAvatar);
