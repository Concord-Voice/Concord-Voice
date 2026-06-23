import React, { useState, useEffect } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { Settings } from 'lucide-react';
import { useAuthStore } from '../../stores/authStore';
import { useSettingsOverlayStore } from '../../stores/settingsOverlayStore';
import { useUserStore } from '../../stores/userStore';
import { useMemberStore, PresenceStatus } from '../../stores/memberStore';
import { useRichPresenceStore } from '../../stores/richPresenceStore';
import UserPopover from './UserPopover';
import FeedbackModal from './FeedbackModal';
import CustomStatusPopover from './CustomStatusPopover';
import './UserPanel.css';

const statusClassMap: Record<PresenceStatus, string> = {
  online: 'online',
  dnd: 'dnd',
  invisible: 'invisible',
  offline: 'offline',
};

const statusLabelMap: Record<PresenceStatus, string> = {
  online: 'Online',
  dnd: 'Do Not Disturb',
  invisible: 'Invisible',
  offline: 'Offline',
};

interface UserPanelProps {
  /** Compact mode: smaller avatar, no separator, popover opens upward. Used in message input area. */
  compact?: boolean;
}

const UserPanel: React.FC<UserPanelProps> = ({ compact = false }) => {
  const [isPopoverOpen, setIsPopoverOpen] = useState(false);
  const [isFeedbackOpen, setIsFeedbackOpen] = useState(false);
  const [isCustomStatusOpen, setIsCustomStatusOpen] = useState(false);
  const accessToken = useAuthStore((state) => state.accessToken);
  const user = useUserStore((state) => state.user);
  const isLoading = useUserStore((state) => state.isLoading);
  const fetchUser = useUserStore((state) => state.fetchUser);
  const selfStatus = useMemberStore((state) => state.selfStatus);
  const selfCustomText = useRichPresenceStore((state) => state.self.customText);
  const selfCustomTextEmoji = useRichPresenceStore((state) => state.self.customTextEmoji);

  // Fetch user data if not already loaded (e.g., on page refresh)
  useEffect(() => {
    if (accessToken && !user) {
      fetchUser();
    }
  }, [accessToken, user, fetchUser]);

  const handleTogglePopover = () => {
    setIsCustomStatusOpen(false);
    setIsPopoverOpen((prev) => !prev);
  };

  const handleOpenCustomStatus = () => {
    setIsPopoverOpen(false);
    setIsCustomStatusOpen(true);
  };

  const panelClass = compact ? 'user-panel user-panel-compact' : 'user-panel';
  const avatarSize = compact ? 'user-avatar-btn compact' : 'user-avatar-btn';
  const avatarClassName = `${avatarSize}${isPopoverOpen ? ' active' : ''}`;
  const avatarContent = user ? (
    <>
      {resolveMediaUrl(user.avatar_url) ? (
        <img
          src={resolveMediaUrl(user.avatar_url)}
          alt={user.username}
          className="user-avatar-img"
        />
      ) : (
        <span className="user-avatar-initial">{user.username.charAt(0).toUpperCase()}</span>
      )}
      <span className={`user-status-dot ${statusClassMap[selfStatus]}`} />
    </>
  ) : null;

  return (
    <div className={panelClass}>
      {!compact && <div className="user-panel-separator" />}

      {/* Loading skeleton */}
      {isLoading && !user && (
        <div className={compact ? 'user-avatar-skeleton compact' : 'user-avatar-skeleton'} />
      )}

      {/* Loaded state */}
      {user && (
        <div className="user-avatar-wrapper">
          {compact ? (
            <button
              type="button"
              className={avatarClassName}
              onClick={handleTogglePopover}
              title={user.username}
              aria-label={`User menu for ${user.username}`}
            >
              {avatarContent}
            </button>
          ) : (
            <button
              type="button"
              className={`user-panel-menu-btn${isPopoverOpen ? ' active' : ''}`}
              onClick={handleTogglePopover}
              title={user.username}
              aria-label={`User menu for ${user.username}`}
            >
              <span className={avatarClassName} aria-hidden="true">
                {avatarContent}
              </span>
              <span className="user-panel-info">
                <span className="user-panel-username">{user.username}</span>
                <span className="user-panel-status-line">
                  {selfCustomText ? (
                    <span className="user-panel-custom-status">
                      {selfCustomTextEmoji && (
                        <span className="user-panel-custom-status-emoji">
                          {selfCustomTextEmoji}
                        </span>
                      )}
                      <span className="user-panel-custom-status-text">{selfCustomText}</span>
                    </span>
                  ) : (
                    <span className={`user-panel-status ${statusClassMap[selfStatus]}`}>
                      {statusLabelMap[selfStatus]}
                    </span>
                  )}
                </span>
              </span>
            </button>
          )}

          {!compact && isCustomStatusOpen && (
            <CustomStatusPopover onClose={() => setIsCustomStatusOpen(false)} />
          )}

          {!compact && (
            <button
              className="user-panel-settings-btn"
              onClick={() => useSettingsOverlayStore.getState().openSettings('app')}
              title="Settings"
              aria-label="Settings"
            >
              <Settings size={16} />
            </button>
          )}

          {isPopoverOpen && (
            <UserPopover
              user={user}
              onClose={() => setIsPopoverOpen(false)}
              onOpenFeedback={() => setIsFeedbackOpen(true)}
              onOpenCustomStatus={compact ? undefined : handleOpenCustomStatus}
            />
          )}
        </div>
      )}
      {/* Feedback modal (#158) — mounted at UserPanel scope so it survives
          popover close. */}
      <FeedbackModal isOpen={isFeedbackOpen} onClose={() => setIsFeedbackOpen(false)} />
    </div>
  );
};

export default UserPanel;
