import { useState, useRef, useEffect } from 'react';
import { MoreHorizontal, Shield, ShieldOff, UserMinus } from 'lucide-react';
import type { DMParticipant } from '../../stores/dmStore';
import './DirectMessages.css';

export const GROUP_MEMBER_MENU_Z_INDEX = 100001;

interface GroupMemberItemProps {
  participant: DMParticipant;
  conversationId: string;
  createdBy?: string;
  currentUserId: string;
  isCurrentUserAdmin: boolean;
  onRoleChange: (userId: string, role: 'admin' | 'member') => void;
  onRemove: (userId: string) => void;
}

const GroupMemberItem: React.FC<GroupMemberItemProps> = ({
  participant,
  createdBy,
  currentUserId,
  isCurrentUserAdmin,
  onRoleChange,
  onRemove,
}) => {
  const [showMenu, setShowMenu] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  const isCreator = participant.userId === createdBy;
  const isSelf = participant.userId === currentUserId;
  const isAdmin = participant.role === 'admin';
  // Show actions only if current user is admin and target is not self and not the creator
  const canShowActions = isCurrentUserAdmin && !isSelf && !isCreator;

  // Close menu on outside click
  useEffect(() => {
    if (!showMenu) return;
    const handleClick = (e: MouseEvent) => {
      if (e.target instanceof Node && menuRef.current && !menuRef.current.contains(e.target)) {
        setShowMenu(false);
      }
    };
    document.addEventListener('mousedown', handleClick);
    return () => document.removeEventListener('mousedown', handleClick);
  }, [showMenu]);

  const initial = (participant.displayName || participant.username).charAt(0).toUpperCase();

  return (
    <div className="group-member-item">
      <div className="conversation-avatar" style={{ width: 32, height: 32 }}>
        <span className="conversation-avatar-initial" style={{ fontSize: 12 }}>
          {initial}
        </span>
      </div>

      <div className="group-member-info">
        <span className="group-member-name">
          {participant.displayName || participant.username}
          {isSelf && <span className="group-member-you"> (you)</span>}
        </span>
        {participant.displayName && (
          <span className="group-member-username">@{participant.username}</span>
        )}
      </div>

      {isAdmin && (
        <span className="group-member-role-badge">
          <Shield size={10} />
          Admin
        </span>
      )}

      {canShowActions && (
        <div className="group-member-actions" ref={menuRef}>
          <button
            type="button"
            className="group-member-menu-btn"
            onClick={() => setShowMenu((v) => !v)}
            aria-label="Member actions"
          >
            <MoreHorizontal size={16} />
          </button>

          {showMenu && (
            <div className="group-member-menu" style={{ zIndex: GROUP_MEMBER_MENU_Z_INDEX }}>
              <button
                type="button"
                className="group-member-menu-item"
                onClick={() => {
                  onRoleChange(participant.userId, isAdmin ? 'member' : 'admin');
                  setShowMenu(false);
                }}
              >
                {isAdmin ? (
                  <>
                    <ShieldOff size={14} /> Demote to Member
                  </>
                ) : (
                  <>
                    <Shield size={14} /> Promote to Admin
                  </>
                )}
              </button>
              <button
                type="button"
                className="group-member-menu-item group-member-menu-item-danger"
                onClick={() => {
                  onRemove(participant.userId);
                  setShowMenu(false);
                }}
              >
                <UserMinus size={14} /> Remove from Group
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
};

export default GroupMemberItem;
