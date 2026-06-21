import React, { useState, useEffect, useRef } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { ServerMember, PresenceStatus } from '../../stores/memberStore';
import { apiFetch } from '../../services/apiClient';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import { useUserThemeScope } from '../../hooks/useUserThemeScope';
import './UserProfileModal.css';

interface UserProfileModalProps {
  isOpen: boolean;
  onClose: () => void;
  member: ServerMember;
  presenceStatus: PresenceStatus;
  lastSeen?: number;
}

interface PublicProfile {
  links: string[];
  created_at: string;
  header_image_url?: string;
}

const formatDate = (dateStr: string): string => {
  const date = new Date(dateStr);
  return date.toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
  });
};

const formatLastSeen = (timestamp: number): string => {
  const now = Date.now();
  const diffMs = now - timestamp * 1000;
  const diffMinutes = Math.floor(diffMs / 60000);
  if (diffMinutes < 1) return 'Just now';
  if (diffMinutes < 60) return `${diffMinutes}m ago`;
  const diffHours = Math.floor(diffMinutes / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  const diffDays = Math.floor(diffHours / 24);
  return `${diffDays}d ago`;
};

const formatLinkDisplay = (url: string): string => {
  try {
    const parsed = new URL(url);
    return parsed.hostname + (parsed.pathname === '/' ? '' : parsed.pathname);
  } catch {
    return url;
  }
};

// Mirrors SafeLink.tsx's helper: narrow `Promise<unknown> | void` down to
// the promise branch via a type-predicate so consumers don't need casts.
function isPromiseLike(value: unknown): value is Promise<unknown> {
  if (value === null || typeof value !== 'object') return false;
  if (!('catch' in value)) return false;
  return typeof value.catch === 'function';
}

const UserProfileModal: React.FC<UserProfileModalProps> = ({
  isOpen,
  onClose,
  member,
  presenceStatus,
  lastSeen,
}) => {
  const [profile, setProfile] = useState<PublicProfile | null>(null);
  const [loadingProfile, setLoadingProfile] = useState(false);
  const overlayRef = useRef<HTMLDivElement>(null);

  const displayName = member.display_name || member.username;
  const roleLabel = member.role.charAt(0).toUpperCase() + member.role.slice(1);
  const userColors = resolveUserAccentColors(member.color_scheme);
  const { scopeProps } = useUserThemeScope(member.color_scheme);

  // Close on Escape
  useEffect(() => {
    if (!isOpen) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, onClose]);

  // Fetch extended profile data (links, created_at) from the API
  useEffect(() => {
    if (!isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears profile when modal closes; not a render loop
      setProfile(null);
      return;
    }

    let cancelled = false;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: shows loading state when fetching profile; not a render loop
    setLoadingProfile(true);

    apiFetch(`/api/v1/users/${member.user_id}/profile`)
      .then((res) => {
        if (!res.ok) throw new Error('Failed to fetch profile');
        return res.json();
      })
      .then((data) => {
        if (!cancelled) {
          setProfile({
            links: data.user?.links || [],
            created_at: data.user?.created_at || '',
            header_image_url: data.user?.header_image_url || undefined,
          });
        }
      })
      .catch(() => {
        if (!cancelled) setProfile(null);
      })
      .finally(() => {
        if (!cancelled) setLoadingProfile(false);
      });

    return () => {
      cancelled = true;
    };
  }, [isOpen, member.user_id]);

  const handleOverlayClick = (e: React.MouseEvent) => {
    if (e.target === overlayRef.current) onClose();
  };

  const getStatusText = (): string => {
    if (presenceStatus === 'online') return 'Online';
    if (presenceStatus === 'dnd') return 'Do Not Disturb';
    if (presenceStatus === 'invisible') return 'Offline';
    if (lastSeen) return `Last seen ${formatLastSeen(lastSeen)}`;
    return 'Offline';
  };

  if (!isOpen) return null;

  const links = profile?.links?.filter((l: string) => l?.trim()) || [];

  return (
    <div className="user-profile-overlay" ref={overlayRef} onClick={handleOverlayClick}>
      <div className="user-profile-container" {...scopeProps} style={scopeProps.style}>
        {/* Banner + Close Button */}
        <div
          className={`user-profile-modal-banner ${profile?.header_image_url ? 'has-image' : ''}`}
          style={
            !profile?.header_image_url && userColors
              ? { background: userColors.gradient, opacity: 0.6 }
              : undefined
          }
        >
          {resolveMediaUrl(profile?.header_image_url) && (
            <img src={resolveMediaUrl(profile?.header_image_url)} alt="" className="user-profile-modal-banner-img" />
          )}
          <button className="user-profile-modal-close" onClick={onClose} aria-label="Close">
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M4 4l8 8M12 4l-8 8"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
              />
            </svg>
          </button>
        </div>

        {/* Content */}
        <div className="user-profile-modal-body">
          {/* Avatar */}
          <div className="user-profile-modal-header">
            <div className="user-profile-modal-avatar">
              {resolveMediaUrl(member.avatar_url) ? (
                <img
                  src={resolveMediaUrl(member.avatar_url)}
                  alt={member.username}
                  className="user-profile-modal-avatar-img"
                />
              ) : (
                <span
                  className="user-profile-modal-avatar-initial"
                  style={
                    userColors ? { background: userColors.gradient, color: '#fff' } : undefined
                  }
                >
                  {member.username.charAt(0).toUpperCase()}
                </span>
              )}
              <span className={`user-profile-modal-status-dot ${presenceStatus}`} />
            </div>
          </div>

          {/* Identity */}
          <div className="user-profile-modal-identity">
            <div className="user-profile-modal-name">{displayName}</div>
            <div className="user-profile-modal-username">@{member.username}</div>
            <span className={`user-profile-modal-role-badge role-${member.role}`}>{roleLabel}</span>
          </div>

          {/* Info Section */}
          <div className="user-profile-modal-section">
            <div className="user-profile-modal-section-title">Info</div>
            <div className="user-profile-modal-details">
              <div className="user-profile-modal-detail-row">
                <span className="user-profile-modal-detail-label">Status</span>
                <span className={`user-profile-modal-detail-value status-${presenceStatus}`}>
                  {getStatusText()}
                </span>
              </div>

              {profile?.created_at && (
                <div className="user-profile-modal-detail-row">
                  <span className="user-profile-modal-detail-label">Member Since</span>
                  <span className="user-profile-modal-detail-value">
                    {formatDate(profile.created_at)}
                  </span>
                </div>
              )}

              <div className="user-profile-modal-detail-row">
                <span className="user-profile-modal-detail-label">Server Joined</span>
                <span className="user-profile-modal-detail-value">
                  {formatDate(member.joined_at)}
                </span>
              </div>

              {loadingProfile && (
                <div className="user-profile-modal-loading">
                  <div className="user-profile-modal-spinner" />
                  <span>Loading profile...</span>
                </div>
              )}
            </div>
          </div>

          {/* About Section */}
          {member.bio && (
            <div className="user-profile-modal-section">
              <div className="user-profile-modal-section-title">About Me</div>
              <p className="user-profile-modal-bio-text">{member.bio}</p>
            </div>
          )}

          {/* Links Section */}
          {links.length > 0 && (
            <div className="user-profile-modal-section">
              <div className="user-profile-modal-section-title">Links</div>
              <div className="user-profile-modal-links-list">
                {links.map((link: string, idx: number) => (
                  <a
                    // eslint-disable-next-line @eslint-react/no-array-index-key -- links are user-supplied strings with no stable unique id; list does not reorder so index is safe
                    key={`${idx}-${link}`}
                    className="user-profile-modal-link"
                    href={link}
                    target="_blank"
                    rel="noopener noreferrer"
                    onClick={(e) => {
                      // Route through window.electron.openExternal so the OS browser is
                      // used explicitly. The main-process IPC handler at
                      // src/main/ipc/openExternal.ts re-validates sender frame AND the
                      // http/https/mailto protocol allowlist — broader scheme coverage
                      // than setWindowOpenHandler's https-only policy because the
                      // explicit user click is consent. See [internal]rules/electron.md
                      // "External-link scheme policy".
                      const api = (
                        globalThis as unknown as {
                          electron?: { openExternal?: (url: string) => Promise<unknown> | void };
                        }
                      ).electron;
                      if (api && typeof api.openExternal === 'function') {
                        e.preventDefault();
                        const result: Promise<unknown> | void = api.openExternal(link);
                        if (isPromiseLike(result)) {
                          result.catch(() => {
                            /* main-process logged the failure; renderer treats as no-op */
                          });
                        }
                      }
                      // Else: default anchor activation. setWindowOpenHandler picks it
                      // up and (for https) routes to shell.openExternal. The preload
                      // bridge is always present in production (main.ts wires
                      // preload.js on every BrowserWindow); this branch exists for
                      // jsdom test cases that explicitly delete window.electron.openExternal.
                    }}
                  >
                    <svg className="user-profile-modal-link-icon" viewBox="0 0 16 16" fill="none">
                      <path
                        d="M6.5 3.5H3.5C2.95 3.5 2.5 3.95 2.5 4.5V12.5C2.5 13.05 2.95 13.5 3.5 13.5H11.5C12.05 13.5 12.5 13.05 12.5 12.5V9.5M9.5 2.5H13.5V6.5M13.5 2.5L7 9"
                        stroke="currentColor"
                        strokeWidth="1.5"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      />
                    </svg>
                    <span className="user-profile-modal-link-text">{formatLinkDisplay(link)}</span>
                  </a>
                ))}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
};

export default UserProfileModal;
