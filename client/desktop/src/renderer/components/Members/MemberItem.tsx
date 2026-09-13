import React from 'react';
import { resolveMediaUrl } from '../../utils/ui/resolveMediaUrl';
import type { ServerMember, PresenceStatus } from '../../stores/chat/memberStore';
import { useRichPresenceStore } from '../../stores/ui/richPresenceStore';
import { resolveUserAccentColors } from '../../utils/ui/schemeColors';
import { presentRemoteActivities } from '../../utils/ui/richPresencePresentation';

interface MemberItemProps {
  member: ServerMember;
  status: PresenceStatus;
  onClick: (e: React.MouseEvent, member: ServerMember) => void;
  onContextMenu: (e: React.MouseEvent, member: ServerMember) => void;
  compact?: boolean;
}

function getStatusLabel(status: PresenceStatus): string {
  if (status === 'dnd') return 'Do Not Disturb';
  if (status === 'offline' || status === 'invisible') return 'Offline';
  return 'Online';
}

const MemberItem: React.FC<MemberItemProps> = ({
  member,
  status,
  onClick,
  onContextMenu,
  compact = false,
}) => {
  const memberColors = resolveUserAccentColors(member.color_scheme);
  const presenceEntries = useRichPresenceStore((state) => state.otherByUser[member.user_id]);
  const activities = presentRemoteActivities(presenceEntries);
  const primaryActivity = activities[0];
  const additionalActivityCount = Math.max(activities.length - 1, 0);
  const topDisplayRole = member.roles?.length
    ? ([...member.roles]
        .filter((r) => r.display_separately)
        .sort((a, b) => b.position - a.position)[0] ?? null)
    : null;
  const roleColor = topDisplayRole?.role_color ?? null;
  const displayName = member.display_name || member.username;
  const statusLabel = getStatusLabel(status);
  return (
    <button
      type="button"
      className={`member-item ${status}${compact ? ' member-item--compact' : ''}`}
      aria-label={compact ? `${displayName} — ${statusLabel}` : undefined}
      onClick={(e) => onClick(e, member)}
      onMouseDown={(e) => e.stopPropagation()}
      onContextMenu={(e) => onContextMenu(e, member)}
      title={displayName}
    >
      <div className="member-avatar">
        {resolveMediaUrl(member.avatar_url) ? (
          <img
            src={resolveMediaUrl(member.avatar_url)}
            alt={member.username}
            className="member-avatar-img"
          />
        ) : (
          <span
            className="member-avatar-initial"
            style={memberColors ? { background: memberColors.gradient, color: '#fff' } : undefined}
          >
            {member.username.charAt(0).toUpperCase()}
          </span>
        )}
        <span className={`member-status-dot ${status}`} />
      </div>
      {!compact && (
        <div className="member-item-text">
          <span className="member-username" style={roleColor ? { color: roleColor } : undefined}>
            {displayName}
          </span>
          {primaryActivity && (
            <span className="member-rich-presence">
              <span className="member-rich-presence-main">
                {primaryActivity.category === 'custom_text' ? (
                  <span className="member-custom-status">
                    {primaryActivity.emoji && (
                      <span className="member-custom-status-emoji">{primaryActivity.emoji}</span>
                    )}
                    <span className="member-custom-status-text">{primaryActivity.headline}</span>
                  </span>
                ) : (
                  <>
                    <span className="member-rich-presence-headline">
                      {primaryActivity.headline}
                    </span>
                    {primaryActivity.detail && (
                      <span className="member-rich-presence-detail">{primaryActivity.detail}</span>
                    )}
                  </>
                )}
              </span>
              {additionalActivityCount > 0 && (
                <span className="member-rich-presence-count">
                  <span aria-hidden="true">+{additionalActivityCount}</span>
                  <span className="sr-only">
                    plus {additionalActivityCount} additional activities
                  </span>
                </span>
              )}
            </span>
          )}
        </div>
      )}
    </button>
  );
};

export default React.memo(MemberItem);
