import React, { useEffect, useRef, useState } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import type { DMParticipant, DMConversation } from '../../stores/dmStore';
import { useFriendStore } from '../../stores/friendStore';
import type { PresenceStatus } from '../../stores/memberStore';
import { apiFetch } from '../../services/apiClient';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import { useUserThemeScope } from '../../hooks/useUserThemeScope';
import './DMProfileModal.css';

export interface DMProfileModalProps {
  isOpen: boolean;
  onClose: () => void;
  peer: DMParticipant;
  conversation: DMConversation;
  onBlockUser?: (conversation: DMConversation) => void;
  onUnfriend?: (conversation: DMConversation) => void;
  onSendMessage?: (conversation: DMConversation) => void;
  /**
   * Wire-up slot for #1209 (DM Voice Call). When undefined, the Voice Call
   * button is hidden. When provided, the button appears and invokes the
   * callback on click. No change to this component is required from #1209.
   */
  onVoiceCall?: (conversation: DMConversation) => void;
}

interface PublicProfile {
  bio?: string;
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

const statusText = (s: PresenceStatus): string => {
  if (s === 'online') return 'Online';
  if (s === 'dnd') return 'Do Not Disturb';
  return 'Offline'; // covers both 'offline' and 'invisible'
};

// Type predicate for narrowing `Promise<unknown> | void` returns without casts.
// Mirrors the helper at UserProfileModal.tsx — kept local so the two modals
// don't take a cross-file dependency on each other.
function isPromiseLike(value: unknown): value is Promise<unknown> {
  if (value === null || typeof value !== 'object') return false;
  if (!('catch' in value)) return false;
  return typeof value.catch === 'function';
}

// Client-side protocol allowlist for user-supplied profile links. Matches the
// main-process IPC handler's allowlist in src/main/ipc/openExternal.ts so the
// raw href never carries a scheme that could execute (`javascript:`, `data:`,
// `vbscript:`, etc.) even when middle-click / drag / copy bypasses the
// onClick→openExternal IPC route. Per Gitar review on PR #1214.
// Set chosen over array literal for O(1) Set.has() lookup vs O(n) includes().
const SAFE_LINK_PROTOCOLS = new Set(['http:', 'https:', 'mailto:']);

function isSafeLinkUrl(url: string): boolean {
  try {
    return SAFE_LINK_PROTOCOLS.has(new URL(url).protocol);
  } catch {
    return false;
  }
}

const DMProfileModal: React.FC<DMProfileModalProps> = ({
  isOpen,
  onClose,
  peer,
  conversation,
  onBlockUser,
  onUnfriend,
  onSendMessage,
  onVoiceCall,
}) => {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [profile, setProfile] = useState<PublicProfile | null>(null);

  // Selective subscription — only re-renders when this peer's friend record changes.
  const friend = useFriendStore((s) => s.friends.find((f) => f.userId === peer.userId));

  // Presence resolution: friendStore is authoritative (WebSocket-updated).
  // Falls back to 'offline' when peer isn't in friendStore (DM with a
  // non-friend / pre-hydration). DMParticipant.status is intentionally NOT
  // used as a fallback — its type is `string | undefined` and an unknown
  // server-side status would render an unknown CSS class. Per Copilot review
  // on PR #1214.
  const presenceStatus: PresenceStatus = friend?.status ?? 'offline';

  const displayName = peer.displayName || peer.username;
  const userColors = resolveUserAccentColors(peer.colorScheme);

  // Trim once and filter on the trimmed value so downstream consumers
  // (href, openExternal IPC) receive canonical URL strings without
  // leading/trailing whitespace. The protocol allowlist closes the
  // surface for javascript: / data: / vbscript: URIs.
  const safeLinks =
    profile?.links?.map((l) => l?.trim() ?? '').filter((l) => l.length > 0 && isSafeLinkUrl(l)) ??
    [];
  const { scopeProps } = useUserThemeScope(peer.colorScheme);

  // Open the native <dialog> imperatively. showModal() gives us native
  // modal behavior: focus trap, ::backdrop, Escape-to-close, all browser-built.
  // The close path runs through the onClose prop → parent unmounts this
  // component (via `if (!isOpen) return null` below); no dlg.close() branch
  // is needed because the dialog never persists across an isOpen→false flip.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg || !isOpen || dlg.open) return;
    dlg.showModal();
  }, [isOpen]);

  // Native dialog events:
  //   - 'close' fires on Escape and on dialog.close()
  //   - 'click' fires when the user clicks anywhere inside the dialog; the
  //     ::backdrop pseudo-element bubbles click events with target === dlg,
  //     which is how we detect backdrop-dismiss.
  // Both listeners are attached imperatively (not via JSX onClick) so the
  // jsx-a11y rule "non-interactive elements should not be assigned mouse or
  // keyboard event listeners" doesn't fire on the <dialog> JSX node.
  useEffect(() => {
    if (!isOpen) return;
    const dlg = dialogRef.current;
    if (!dlg) return;
    const handleClose = () => onClose();
    const handleBackdropClick = (e: MouseEvent) => {
      if (e.target === dlg) onClose();
    };
    dlg.addEventListener('close', handleClose);
    dlg.addEventListener('click', handleBackdropClick);
    return () => {
      dlg.removeEventListener('close', handleClose);
      dlg.removeEventListener('click', handleBackdropClick);
    };
  }, [isOpen, onClose]);

  // Fetch /profile when modal opens
  useEffect(() => {
    if (!isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears profile when modal closes; not a render loop
      setProfile(null);
      return;
    }
    let cancelled = false;
    apiFetch(`/api/v1/users/${peer.userId}/profile`)
      .then((res) => {
        if (!res.ok) throw new Error('Failed to fetch profile');
        return res.json();
      })
      .then((data) => {
        if (!cancelled) {
          setProfile({
            bio: data.user?.bio || undefined,
            links: data.user?.links || [],
            created_at: data.user?.created_at || '',
            header_image_url: data.user?.header_image_url || undefined,
          });
        }
      })
      .catch(() => {
        if (!cancelled) setProfile(null);
      });
    return () => {
      cancelled = true;
    };
  }, [isOpen, peer.userId]);

  if (!isOpen) return null;

  const labelId = `dm-profile-name-${conversation.id}`;

  return (
    <dialog
      ref={dialogRef}
      className="dm-profile-modal-container"
      aria-labelledby={labelId}
      {...scopeProps}
      style={scopeProps.style}
    >
      <div className="dm-profile-modal-inner">
        <div
          className={`dm-profile-modal-banner ${profile?.header_image_url ? 'has-image' : ''}`}
          style={
            !profile?.header_image_url && userColors
              ? { background: userColors.gradient, opacity: 0.6 }
              : undefined
          }
        >
          {resolveMediaUrl(profile?.header_image_url) && (
            <img src={resolveMediaUrl(profile?.header_image_url)} alt="" className="dm-profile-modal-banner-img" />
          )}
          <button
            className="dm-profile-modal-close"
            onClick={onClose}
            aria-label="Close profile"
            type="button"
          >
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
              <path
                d="M4 4l8 8M12 4l-8 8"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
              />
            </svg>
          </button>
        </div>

        <div className="dm-profile-modal-body">
          <div className="dm-profile-modal-header">
            <div className="dm-profile-modal-avatar">
              {resolveMediaUrl(peer.avatarUrl) ? (
                <img
                  src={resolveMediaUrl(peer.avatarUrl)}
                  alt={peer.username}
                  className="dm-profile-modal-avatar-img"
                />
              ) : (
                <span
                  className="dm-profile-modal-avatar-initial"
                  style={
                    userColors ? { background: userColors.gradient, color: '#fff' } : undefined
                  }
                >
                  {peer.username.charAt(0).toUpperCase()}
                </span>
              )}
              <span
                className={`dm-profile-modal-status-dot ${presenceStatus}`}
                aria-label={`Status: ${statusText(presenceStatus)}`}
              />
            </div>
          </div>

          <div className="dm-profile-modal-identity">
            <div className="dm-profile-modal-name" id={labelId}>
              {displayName}
            </div>
            <div className="dm-profile-modal-username">@{peer.username}</div>
          </div>

          <div className="dm-profile-modal-section">
            <div className="dm-profile-modal-section-title">Info</div>
            <div className="dm-profile-modal-details">
              <div className="dm-profile-modal-detail-row">
                <span className="dm-profile-modal-detail-label">Status</span>
                <span className="dm-profile-modal-detail-value">{statusText(presenceStatus)}</span>
              </div>

              {friend?.createdAt && (
                <div className="dm-profile-modal-detail-row">
                  <span className="dm-profile-modal-detail-label">Friends since</span>
                  <span className="dm-profile-modal-detail-value">
                    {formatDate(friend.createdAt)}
                  </span>
                </div>
              )}

              {profile?.created_at && (
                <div className="dm-profile-modal-detail-row">
                  <span className="dm-profile-modal-detail-label">Member since</span>
                  <span className="dm-profile-modal-detail-value">
                    {formatDate(profile.created_at)}
                  </span>
                </div>
              )}

              <div className="dm-profile-modal-detail-row">
                <span className="dm-profile-modal-detail-label">Conversation started</span>
                <span className="dm-profile-modal-detail-value">
                  {formatDate(conversation.createdAt)}
                </span>
              </div>
            </div>
          </div>

          {profile?.bio && (
            <div className="dm-profile-modal-section">
              <div className="dm-profile-modal-section-title">About Me</div>
              <p className="dm-profile-modal-bio-text">{profile.bio}</p>
            </div>
          )}

          {safeLinks.length > 0 && (
            <div className="dm-profile-modal-section">
              <div className="dm-profile-modal-section-title">Links</div>
              <div className="dm-profile-modal-links-list">
                {safeLinks.map((link, idx) => (
                  <a
                    // eslint-disable-next-line @eslint-react/no-array-index-key -- links are user-supplied strings with no stable unique id; list does not reorder so index is safe
                    key={`${idx}-${link}`}
                    className="dm-profile-modal-link"
                    href={link}
                    target="_blank"
                    rel="noopener noreferrer"
                    onClick={(e) => {
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
                            /* logged in main process */
                          });
                        }
                      }
                    }}
                  >
                    {link}
                  </a>
                ))}
              </div>
            </div>
          )}

          <div className="dm-profile-modal-actions">
            {onSendMessage && (
              <button
                type="button"
                className="dm-profile-modal-action dm-profile-modal-action-primary"
                onClick={() => onSendMessage(conversation)}
              >
                Send Message
              </button>
            )}
            {onVoiceCall && (
              <button
                type="button"
                className="dm-profile-modal-action dm-profile-modal-action-primary"
                onClick={() => onVoiceCall(conversation)}
              >
                Voice Call
              </button>
            )}
            {friend && onUnfriend && (
              <button
                type="button"
                className="dm-profile-modal-action dm-profile-modal-action-destructive"
                onClick={() => onUnfriend(conversation)}
              >
                Unfriend
              </button>
            )}
            {onBlockUser && (
              <button
                type="button"
                className="dm-profile-modal-action dm-profile-modal-action-destructive"
                onClick={() => onBlockUser(conversation)}
              >
                Block
              </button>
            )}
          </div>
        </div>
      </div>
    </dialog>
  );
};

export default DMProfileModal;
