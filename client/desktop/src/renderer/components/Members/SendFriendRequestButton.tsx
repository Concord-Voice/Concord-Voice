import React from 'react';
import { useFriendRequestState } from '../../hooks/useFriendRequestState';

interface SendFriendRequestButtonProps {
  userId: string | undefined;
  /**
   * Extra class for surface-specific styling. The base `.send-friend-request-btn`
   * class carries the shared look; callers layer their own (e.g. the profile
   * card passes `member-profile-action-btn` to match the View Full Profile btn).
   */
  className?: string;
  /** Called after a successful send so the surrounding surface can close itself. */
  onSent?: () => void;
}

/**
 * Self-contained "Send Friend Request" affordance used across the member
 * context menu, member profile card, and chat profile card. Reads all
 * relationship state from `useFriendRequestState`, renders inline status
 * feedback (no global toast — matches the inline-status pattern used by
 * DMConversationContextMenu's key-rotation button), and hides itself entirely
 * for the signed-in user's own profile.
 *
 * When already friends or a request is pending, it renders a disabled
 * non-interactive label ("Friends" / "Request Pending") rather than vanishing,
 * so the surface communicates the existing relationship.
 */
const SendFriendRequestButton: React.FC<SendFriendRequestButtonProps> = ({
  userId,
  className,
  onSent,
}) => {
  const { visible, canSend, status, errorMessage, label, isFriend, hasPendingRequest, send } =
    useFriendRequestState(userId);

  if (!visible) return null;

  // Non-actionable relationship states render as a disabled label.
  if (isFriend || hasPendingRequest) {
    return (
      <button
        type="button"
        className={`send-friend-request-btn send-friend-request-btn--inert ${className ?? ''}`}
        disabled
      >
        {label}
      </button>
    );
  }

  const display = (() => {
    if (status === 'sending') return 'Sending…';
    if (status === 'sent') return 'Request Sent!';
    if (status === 'error') return errorMessage ?? 'Failed — try again';
    return label;
  })();

  const handleClick = () => {
    if (!canSend) return;
    void send().then(() => {
      // Defer the surface-close so the user sees the "Request Sent!" state for
      // a beat before the card/menu dismisses.
      if (onSent) setTimeout(onSent, 600);
    });
  };

  return (
    <button
      type="button"
      className={`send-friend-request-btn send-friend-request-btn--${status} ${className ?? ''}`}
      onClick={handleClick}
      disabled={status === 'sending'}
      aria-label="Send friend request"
    >
      {display}
    </button>
  );
};

export default SendFriendRequestButton;
