import React from 'react';
import type { RepliedToMessage } from '../../types/chat';
import './ReplyPreviewBar.css';

export interface ReplyPreviewBarProps {
  repliedTo: RepliedToMessage | null;
  isDeleted?: boolean;
  onCancel?: () => void;
  onClick?: () => void;
  variant?: 'input' | 'inline';
}

const SNIPPET_LENGTH = 100;

function truncateSnippet(text: string): string {
  const codePoints = Array.from(text);
  if (codePoints.length <= SNIPPET_LENGTH) return text;
  return codePoints.slice(0, SNIPPET_LENGTH).join('') + '...';
}

const ReplyPreviewBar: React.FC<ReplyPreviewBarProps> = ({
  repliedTo,
  isDeleted = false,
  onCancel,
  onClick,
  variant = 'inline',
}) => {
  if (!repliedTo && !isDeleted) return null;

  const displayName = repliedTo?.display_name || repliedTo?.username || 'Unknown';
  const rawContent = repliedTo?.content || '';
  const snippet = isDeleted ? 'Original message is unavailable' : truncateSnippet(rawContent);

  const className = `reply-preview-bar reply-preview-${variant} ${isDeleted ? 'reply-deleted' : ''} ${onClick ? 'reply-clickable' : ''}`;

  const inner = (
    <>
      <div className="reply-preview-indicator" />
      <div className="reply-preview-content">
        {!isDeleted && <span className="reply-preview-author">{displayName}</span>}
        <span className="reply-preview-snippet">{snippet}</span>
      </div>
      {onCancel && !onClick && (
        <button
          className="reply-preview-cancel"
          onClick={(e) => {
            e.stopPropagation();
            onCancel();
          }}
          aria-label="Cancel reply"
        >
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M4 4l8 8M12 4l-8 8"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          </svg>
        </button>
      )}
    </>
  );

  if (onClick) {
    return (
      <button type="button" className={className} onClick={onClick}>
        {inner}
      </button>
    );
  }

  return <div className={className}>{inner}</div>;
};

export default ReplyPreviewBar;
