import React from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { PanelRightClose, PanelRightOpen, Users } from 'lucide-react';
import { useLayoutStore } from '../../stores/layoutStore';
import { useResizablePanel } from '../../hooks/useResizablePanel';
import { useServerStore } from '../../stores/serverStore';
import { useMemberStore, PresenceStatus } from '../../stores/memberStore';
import { useUserStore } from '../../stores/userStore';
import MemberList from '../Members/MemberList';

const MemberFlexSpace: React.FC = () => {
  const memberPanelMode = useLayoutStore((s) => s.memberPanelMode);
  const cycleMemberPanelMode = useLayoutStore((s) => s.cycleMemberPanelMode);
  const setMemberPanelMode = useLayoutStore((s) => s.setMemberPanelMode);
  const interfaceLocked = useLayoutStore((s) => s.interfaceLocked);
  const activeServerId = useServerStore((s) => s.activeServerId);

  const members = useMemberStore((s) => s.members);
  const onlineUserIds = useMemberStore((s) => s.onlineUserIds);
  const userStatuses = useMemberStore((s) => s.userStatuses);
  const selfStatus = useMemberStore((s) => s.selfStatus);
  const selfUser = useUserStore((s) => s.user);

  const panel = useResizablePanel({
    defaultWidth: 260,
    minWidth: 160,
    maxWidth: 340,
    side: 'right',
    storageKey: 'concord:memberPanelWidth',
  });

  if (!activeServerId) return null;

  const getMemberStatus = (userId: string): PresenceStatus => {
    if (selfUser?.id === userId) return selfStatus;
    return userStatuses.get(userId) || (onlineUserIds.has(userId) ? 'online' : 'offline');
  };

  // Toggle button — always visible
  const toggleButton = (
    <button
      className="member-panel-toggle"
      onClick={cycleMemberPanelMode}
      title={`Members (${memberPanelMode})`}
      aria-label="Toggle member panel"
    >
      {memberPanelMode === 'hidden' ? <PanelRightOpen size={16} /> : <PanelRightClose size={16} />}
    </button>
  );

  // Hidden mode — just show toggle
  if (memberPanelMode === 'hidden') {
    return toggleButton;
  }

  // Collapsed mode — avatar strip
  if (memberPanelMode === 'collapsed') {
    return (
      <div className="member-panel-collapsed">
        <button
          onClick={() => setMemberPanelMode('expanded')}
          title="Expand member list"
          aria-label="Expand member list"
          style={{
            background: 'none',
            border: 'none',
            cursor: 'pointer',
            color: 'var(--text-muted)',
            padding: '4px 0',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            marginBottom: '4px',
          }}
        >
          <Users size={14} />
        </button>

        {members.slice(0, 30).map((member) => {
          const status = getMemberStatus(member.user_id);
          return (
            <div key={member.user_id} className="member-avatar-strip-item">
              {resolveMediaUrl(member.avatar_url) ? (
                <img src={resolveMediaUrl(member.avatar_url)} alt={member.username} />
              ) : (
                <div className="member-avatar-strip-initial">
                  {member.username.charAt(0).toUpperCase()}
                </div>
              )}
              <span className={`member-strip-status ${status}`} />
              <span className="member-strip-tooltip">{member.display_name || member.username}</span>
            </div>
          );
        })}
      </div>
    );
  }

  // Expanded mode — full member list with resize.
  // The resize handle is removed when the interface is locked (#188), freezing
  // the current member-panel width.
  return (
    <>
      {!interfaceLocked && (
        <button
          type="button"
          className="layout-resize-handle"
          onMouseDown={panel.onMouseDown}
          onKeyDown={panel.onKeyDown}
          tabIndex={0}
          aria-label="Resize member panel"
        />
      )}
      <div
        className="member-list-container"
        style={{
          width: panel.width,
          display: 'flex',
          flexDirection: 'column',
          overflow: 'hidden',
          height: '100%',
        }}
      >
        <MemberList />
      </div>
    </>
  );
};

export default MemberFlexSpace;
