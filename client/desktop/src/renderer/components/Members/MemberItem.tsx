import React from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import type { ServerMember, PresenceStatus } from '../../stores/memberStore';
import { useRichPresenceStore } from '../../stores/richPresenceStore';
import { resolveUserAccentColors } from '../../utils/schemeColors';

interface MemberItemProps {
  member: ServerMember;
  status: PresenceStatus;
  onClick: (e: React.MouseEvent, member: ServerMember) => void;
  onContextMenu: (e: React.MouseEvent, member: ServerMember) => void;
}

const MemberItem: React.FC<MemberItemProps> = ({ member, status, onClick, onContextMenu }) => {
  const memberColors = resolveUserAccentColors(member.color_scheme);
  // Selective subscription: only this member's custom-text status (undefined if none).
  const customText = useRichPresenceStore((s) => s.customTextByUser[member.user_id]);
  const topDisplayRole = member.roles?.length
    ? ([...member.roles]
        .filter((r) => r.display_separately)
        .sort((a, b) => b.position - a.position)[0] ?? null)
    : null;
  const roleColor = topDisplayRole?.role_color ?? null;
  return (
    <button
      type="button"
      className={`member-item ${status}`}
      onClick={(e) => onClick(e, member)}
      onMouseDown={(e) => e.stopPropagation()}
      onContextMenu={(e) => onContextMenu(e, member)}
      title={member.display_name || member.username}
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
      <div className="member-item-text">
        <span className="member-username" style={roleColor ? { color: roleColor } : undefined}>
          {member.display_name || member.username}
        </span>
        {customText && (
          <span className="member-custom-status">
            {customText.emoji && (
              <span className="member-custom-status-emoji">{customText.emoji}</span>
            )}
            <span className="member-custom-status-text">{customText.text}</span>
          </span>
        )}
      </div>
    </button>
  );
};

export default React.memo(MemberItem);
