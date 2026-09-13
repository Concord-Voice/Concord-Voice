import React, { useState, useEffect, useId } from 'react';
import { useShallow } from 'zustand/react/shallow';
import { resolveMediaUrl } from '../../utils/ui/resolveMediaUrl';
import { Settings } from 'lucide-react';
import { useAuthStore } from '../../stores/auth/authStore';
import { useSettingsOverlayStore } from '../../stores/ui/settingsOverlayStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useMemberStore, PresenceStatus } from '../../stores/chat/memberStore';
import {
  selectLocalRichPresenceActivity,
  useRichPresenceStore,
} from '../../stores/ui/richPresenceStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { presentSelfActivity } from '../../utils/ui/richPresencePresentation';
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

interface UserPanelStatusProps {
  activity: ReturnType<typeof presentSelfActivity>;
  customText?: string;
  customTextEmoji?: string;
  status: PresenceStatus;
  activityDescriptionId: string;
}

const UserPanelStatus: React.FC<UserPanelStatusProps> = ({
  activity,
  customText,
  customTextEmoji,
  status,
  activityDescriptionId,
}) => {
  if (activity) {
    return (
      <span className="user-panel-activity" id={activityDescriptionId}>
        <span className="user-panel-activity-headline">{activity.headline}</span>
        <span className="user-panel-activity-policy">
          <span>Eligible audience: {activity.eligibility}</span>
          {activity.deliveryNote && (
            <span className="user-panel-activity-note">{activity.deliveryNote}</span>
          )}
        </span>
      </span>
    );
  }

  if (customText) {
    return (
      <span className="user-panel-custom-status">
        {customTextEmoji && (
          <span className="user-panel-custom-status-emoji">{customTextEmoji}</span>
        )}
        <span className="user-panel-custom-status-text">{customText}</span>
      </span>
    );
  }

  return (
    <span className={`user-panel-status ${statusClassMap[status]}`}>{statusLabelMap[status]}</span>
  );
};

const UserPanel: React.FC<UserPanelProps> = ({ compact = false }) => {
  const [isPopoverOpen, setIsPopoverOpen] = useState(false);
  const [isFeedbackOpen, setIsFeedbackOpen] = useState(false);
  const [isCustomStatusOpen, setIsCustomStatusOpen] = useState(false);
  const accessToken = useAuthStore((state) => state.accessToken);
  const user = useUserStore((state) => state.user);
  const isLoading = useUserStore((state) => state.isLoading);
  const fetchUser = useUserStore((state) => state.fetchUser);
  const selfStatus = useMemberStore((state) => state.selfStatus);
  const confirmedPresenceSettings = useRichPresenceStore(
    (state) => state.confirmedPresenceSettings
  );
  const selfCustomText = useRichPresenceStore((state) => state.self.customText);
  const selfCustomTextEmoji = useRichPresenceStore((state) => state.self.customTextEmoji);
  const selfActivityDescriptionId = useId();
  const selfActivity = useVoiceStore(
    useShallow((state) => {
      if (compact) return null;

      return presentSelfActivity(
        selectLocalRichPresenceActivity(state),
        confirmedPresenceSettings,
        selfStatus
      );
    })
  );

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
              aria-describedby={selfActivity ? selfActivityDescriptionId : undefined}
            >
              <span className={avatarClassName} aria-hidden="true">
                {avatarContent}
              </span>
              <span className="user-panel-info">
                <span className="user-panel-username">{user.username}</span>
                <span className="user-panel-status-line">
                  <UserPanelStatus
                    activity={selfActivity}
                    customText={selfCustomText}
                    customTextEmoji={selfCustomTextEmoji}
                    status={selfStatus}
                    activityDescriptionId={selfActivityDescriptionId}
                  />
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
