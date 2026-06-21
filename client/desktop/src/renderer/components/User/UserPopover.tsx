import React, { useEffect, useRef, useMemo } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { useNavigate } from 'react-router-dom';
import { useSettingsOverlayStore } from '../../stores/settingsOverlayStore';
import { UserProfile, useUserStore } from '../../stores/userStore';
import { useMemberStore, PresenceStatus } from '../../stores/memberStore';
import { getWebSocketService } from '../../services/websocketService';
import { useUserThemeScope } from '../../hooks/useUserThemeScope';
import { useSettingsStore } from '../../stores/settingsStore';
import './UserPopover.css';

interface UserPopoverProps {
  user: UserProfile;
  onClose: () => void;
  /** Open the feedback modal (#158). The popover closes itself first. */
  onOpenFeedback?: () => void;
}

const statusOptions: Array<{
  value: PresenceStatus;
  label: string;
  color: string;
}> = [
  { value: 'online', label: 'Online', color: 'var(--status-connected)' },
  { value: 'dnd', label: 'Do Not Disturb', color: 'var(--status-disconnected)' },
  { value: 'invisible', label: 'Invisible', color: 'var(--text-muted)' },
];

const statusColorMap: Record<PresenceStatus, string> = {
  online: 'var(--status-connected)',
  dnd: 'var(--status-disconnected)',
  invisible: 'var(--text-muted)',
  offline: 'var(--text-muted)',
};

const statusLabelMap: Record<PresenceStatus, string> = {
  online: 'Online',
  dnd: 'Do Not Disturb',
  invisible: 'Invisible',
  offline: 'Offline',
};

const UserPopover: React.FC<UserPopoverProps> = ({ user, onClose, onOpenFeedback }) => {
  const popoverRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  const logout = useUserStore((state) => state.logout);
  const selfStatus = useMemberStore((state) => state.selfStatus);
  const setSelfStatus = useMemberStore((state) => state.setSelfStatus);

  // Build color_scheme JSON from the current user's settings for identity scoping
  const colorScheme = useSettingsStore((s) => s.appearance.colorScheme);
  const customColors = useSettingsStore((s) => s.appearance.customColors);
  const theme = useSettingsStore((s) => s.appearance.theme);
  const selfColorSchemeJson = useMemo(() => {
    let resolvedTheme: string;
    if (theme === 'system') {
      resolvedTheme = globalThis.matchMedia('(prefers-color-scheme: dark)').matches
        ? 'dark'
        : 'light';
    } else {
      resolvedTheme = theme;
    }
    if (colorScheme === 'custom' && customColors) {
      return JSON.stringify({
        scheme: 'custom',
        themeMode: resolvedTheme,
        accentPrimary: customColors.accentPrimary,
        accentSecondary: customColors.accentSecondary,
      });
    }
    return JSON.stringify({ scheme: colorScheme, themeMode: resolvedTheme });
  }, [colorScheme, customColors, theme]);
  const { scopeProps } = useUserThemeScope(selfColorSchemeJson);

  // Close on outside click or Escape
  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (
        e.target instanceof Node &&
        popoverRef.current &&
        !popoverRef.current.contains(e.target)
      ) {
        onClose();
      }
    };

    const handleEscape = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
      }
    };

    // Delay adding listener to avoid immediate close from the click that opened it
    const timer = setTimeout(() => {
      document.addEventListener('mousedown', handleClickOutside);
    }, 0);
    document.addEventListener('keydown', handleEscape);

    return () => {
      clearTimeout(timer);
      document.removeEventListener('mousedown', handleClickOutside);
      document.removeEventListener('keydown', handleEscape);
    };
  }, [onClose]);

  const handleLogout = async () => {
    onClose();
    await logout();
    navigate('/');
  };

  const handleStatusChange = (status: PresenceStatus) => {
    setSelfStatus(status);
    if (status !== 'offline') {
      const ws = getWebSocketService();
      ws.sendSetStatus(status);
    }
  };

  return (
    <div ref={popoverRef} className="user-popover" {...scopeProps} style={scopeProps.style}>
      {/* User info header */}
      <div className="user-popover-header">
        <div className="user-popover-avatar">
          {resolveMediaUrl(user.avatar_url) ? (
            <img src={resolveMediaUrl(user.avatar_url)} alt={user.username} className="user-popover-avatar-img" />
          ) : (
            <span className="user-popover-avatar-initial">
              {user.username.charAt(0).toUpperCase()}
            </span>
          )}
        </div>
        <div className="user-popover-info">
          <span className="user-popover-username">{user.username}</span>
          <span className="user-popover-status" style={{ color: statusColorMap[selfStatus] }}>
            {statusLabelMap[selfStatus]}
          </span>
        </div>
      </div>

      {/* Email */}
      {user.email && <div className="user-popover-email">{user.email}</div>}

      <div className="user-popover-separator" />

      {/* Status picker */}
      <div className="user-popover-status-section">
        <div className="user-popover-status-label">Status</div>
        {statusOptions.map((opt) => (
          <button
            key={opt.value}
            className={`user-popover-status-item ${selfStatus === opt.value ? 'active' : ''}`}
            onClick={() => handleStatusChange(opt.value)}
          >
            <span className="user-popover-status-dot" style={{ backgroundColor: opt.color }} />
            <span className="user-popover-status-text">{opt.label}</span>
            {selfStatus === opt.value && (
              <svg
                className="user-popover-status-check"
                width="14"
                height="14"
                viewBox="0 0 14 14"
                fill="none"
              >
                <path
                  d="M3 7l3 3 5-5.5"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            )}
          </button>
        ))}
      </div>

      <div className="user-popover-separator" />

      {/* Menu items */}
      <button
        className="user-popover-item"
        onClick={() => {
          onClose();
          useSettingsOverlayStore.getState().openSettings('profile');
        }}
      >
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
          <circle cx="8" cy="5" r="3" stroke="currentColor" strokeWidth="1.5" />
          <path
            d="M2 14c0-2.76 2.69-5 6-5s6 2.24 6 5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
        </svg>
        My Profile
      </button>

      <button
        className="user-popover-item"
        onClick={() => {
          onClose();
          useSettingsOverlayStore.getState().openSettings('app');
        }}
      >
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
          <path
            d="M6.86 2.57a1.14 1.14 0 012.28 0 4.57 4.57 0 012.64 1.53 1.14 1.14 0 011.97 1.14 4.57 4.57 0 010 3.05 1.14 1.14 0 01-1.97 1.14 4.57 4.57 0 01-2.64 1.53 1.14 1.14 0 01-2.28 0 4.57 4.57 0 01-2.64-1.53 1.14 1.14 0 01-1.97-1.14 4.57 4.57 0 010-3.05 1.14 1.14 0 011.97-1.14A4.57 4.57 0 016.86 2.57z"
            stroke="currentColor"
            strokeWidth="1.5"
          />
          <circle cx="8" cy="8" r="2" stroke="currentColor" strokeWidth="1.5" />
        </svg>
        Settings
      </button>

      <div className="user-popover-separator" />

      {/* Logout */}
      <button className="user-popover-item user-popover-item-danger" onClick={handleLogout}>
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
          <path
            d="M6 14H3.33A1.33 1.33 0 012 12.67V3.33A1.33 1.33 0 013.33 2H6M10.67 11.33L14 8l-3.33-3.33M14 8H6"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
        Log Out
      </button>

      {/* Quit app */}
      <button
        className="user-popover-item user-popover-item-danger"
        onClick={() => globalThis.electron.quitApp()}
      >
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
          <path
            d="M4 4l8 8M12 4l-8 8"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
        </svg>
        Quit
      </button>

      {/* Feedback — own section below Log Out / Quit per #158 spec */}
      {onOpenFeedback && (
        <>
          <div className="user-popover-separator" />
          <button
            className="user-popover-item"
            onClick={() => {
              onClose();
              onOpenFeedback();
            }}
          >
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              {/* Speech-bubble glyph — matches the conversational tone of feedback */}
              <path
                d="M2 4a2 2 0 012-2h8a2 2 0 012 2v6a2 2 0 01-2 2H6l-3 2v-2H4a2 2 0 01-2-2V4z"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinejoin="round"
              />
            </svg>
            Bug Report / Feature Request
          </button>
        </>
      )}
    </div>
  );
};

export default UserPopover;
