import React, { useEffect, useState, useCallback, useRef, useMemo } from 'react';
import { useServerStore } from '../../stores/serverStore';
import { useUserStore } from '../../stores/userStore';
import { useMemberStore, ServerMember, PresenceStatus } from '../../stores/memberStore';
import { usePermissionStore } from '../../stores/permissionStore';
import MemberItem from './MemberItem';
import MemberProfileCard from './MemberProfileCard';
import MemberContextMenu from './MemberContextMenu';
import UserProfileModal from './UserProfileModal';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { apiFetch, safeJson } from '../../services/apiClient';
import './MemberList.css';

const MemberList: React.FC = () => {
  const activeServerId = useServerStore((state) => state.activeServerId);
  const servers = useServerStore((state) => state.servers);
  const activeServer = servers.find((s) => s.id === activeServerId) || null;
  const selfUser = useUserStore((state) => state.user);
  const members = useMemberStore((state) => state.members);
  const onlineUserIds = useMemberStore((state) => state.onlineUserIds);
  const userStatuses = useMemberStore((state) => state.userStatuses);
  const lastSeenByUser = useMemberStore((state) => state.lastSeenByUser);
  const selfStatus = useMemberStore((state) => state.selfStatus);
  const isLoading = useMemberStore((state) => state.isLoading);
  const error = useMemberStore((state) => state.error);
  const fetchMembers = useMemberStore((state) => state.fetchMembers);
  const clearMembers = useMemberStore((state) => state.clearMembers);
  const fetchRoles = usePermissionStore((state) => state.fetchRoles);
  const serverRoles = usePermissionStore((state) => state.serverRoles);

  const [selectedMember, setSelectedMember] = useState<{
    userId: string;
    position: { x: number; y: number };
  } | null>(null);

  const [contextMenu, setContextMenu] = useState<{
    member: ServerMember;
    position: { x: number; y: number };
  } | null>(null);

  const [fullProfileUserId, setFullProfileUserId] = useState<string | null>(null);
  const [banTarget, setBanTarget] = useState<ServerMember | null>(null);
  const [kickTarget, setKickTarget] = useState<ServerMember | null>(null);

  // Derive live member data from store (not stale snapshots)
  const selectedMemberData = selectedMember
    ? (members.find((m) => m.user_id === selectedMember.userId) ?? null)
    : null;

  const fullProfileMemberData = fullProfileUserId
    ? (members.find((m) => m.user_id === fullProfileUserId) ?? null)
    : null;

  // Track collapsed role groups
  // eslint-disable-next-line @eslint-react/use-state -- Set() is cheap to construct; lazy initializer would add noise without benefit
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(new Set());

  // Search filter
  const [searchQuery, setSearchQuery] = useState('');

  // Fetch members when active server changes.
  // Guard prevents StrictMode double-mount from firing duplicate HTTP requests.
  const memberFetchRef = useRef<string | null>(null);

  useEffect(() => {
    if (activeServerId) {
      if (memberFetchRef.current !== activeServerId) {
        memberFetchRef.current = activeServerId;
        fetchMembers(activeServerId);
        fetchRoles(activeServerId);
      }
    } else {
      memberFetchRef.current = null;
      clearMembers();
    }
  }, [activeServerId, fetchMembers, fetchRoles, clearMembers]);

  // Presence is handled globally in useWebSocket (presence_snapshot + presence events)

  const handleMemberClick = useCallback((e: React.MouseEvent, member: ServerMember) => {
    setSelectedMember((prev) =>
      prev?.userId === member.user_id
        ? null
        : { userId: member.user_id, position: { x: e.clientX, y: e.clientY } }
    );
  }, []);

  const handleMemberContextMenu = useCallback((e: React.MouseEvent, member: ServerMember) => {
    e.preventDefault();
    e.stopPropagation();
    setContextMenu({
      member,
      position: { x: e.clientX, y: e.clientY },
    });
  }, []);

  const toggleGroup = useCallback((groupKey: string) => {
    setCollapsedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(groupKey)) {
        next.delete(groupKey);
      } else {
        next.add(groupKey);
      }
      return next;
    });
  }, []);

  // Get status for a member (with fallback)
  // For the self user, use selfStatus as the source of truth
  const getMemberStatus = useCallback(
    (userId: string): PresenceStatus => {
      if (selfUser?.id === userId) {
        return selfStatus;
      }
      return userStatuses.get(userId) || (onlineUserIds.has(userId) ? 'online' : 'offline');
    },
    [selfUser, selfStatus, userStatuses, onlineUserIds]
  );

  // Sort: online first, dnd second, away third, offline/invisible last, then alphabetical
  const sortMembers = useCallback(
    (list: ServerMember[]) =>
      [...list].sort((a, b) => {
        const statusOrder: Record<PresenceStatus, number> = {
          online: 0,
          dnd: 1,
          invisible: 2,
          offline: 2,
        };
        const aStatus = getMemberStatus(a.user_id);
        const bStatus = getMemberStatus(b.user_id);
        const aOrder = statusOrder[aStatus];
        const bOrder = statusOrder[bStatus];
        if (aOrder !== bOrder) return aOrder - bOrder;
        return a.username.localeCompare(b.username);
      }),
    [getMemberStatus]
  );

  // Filter members by search query
  const filteredMembers = useMemo(() => {
    if (!searchQuery.trim()) return members;
    const q = searchQuery.toLowerCase();
    return members.filter((m) => {
      if (m.username.toLowerCase().includes(q)) return true;
      if (m.display_name?.toLowerCase().includes(q)) return true;
      if (m.roles?.some((r) => r.role_name.toLowerCase().includes(q))) return true;
      return false;
    });
  }, [members, searchQuery]);

  // Build role-based groups using RBAC roles with display_separately
  const roleGroups = useMemo(() => {
    const roles = activeServerId ? serverRoles[activeServerId] || [] : [];

    // Get roles with display_separately, sorted by position (highest first)
    const displayRoles = roles
      .filter((r) => r.display_separately && !r.is_default)
      .sort((a, b) => b.position - a.position);

    // Build groups: each member appears in their highest-position display_separately role
    const assignedUserIds = new Set<string>();
    const groups: Array<{
      key: string;
      label: string;
      emoji?: string;
      color?: string;
      members: ServerMember[];
    }> = [];

    for (const role of displayRoles) {
      const roleMembers = filteredMembers.filter((m) => {
        if (assignedUserIds.has(m.user_id)) return false;
        return m.roles?.some((r) => r.role_id === role.id);
      });
      if (roleMembers.length > 0) {
        for (const m of roleMembers) assignedUserIds.add(m.user_id);
        groups.push({
          key: `role-${role.id}`,
          label: role.name,
          emoji: role.emoji,
          color: role.color,
          members: sortMembers(roleMembers),
        });
      }
    }

    // Remaining members go into Online / Offline groups
    const remaining = filteredMembers.filter((m) => !assignedUserIds.has(m.user_id));
    const online = remaining.filter((m) => {
      const status = getMemberStatus(m.user_id);
      return status === 'online' || status === 'dnd';
    });
    const offline = remaining.filter((m) => {
      const status = getMemberStatus(m.user_id);
      return status === 'offline' || status === 'invisible';
    });

    if (online.length > 0) {
      groups.push({
        key: 'online',
        label: 'Online',
        members: sortMembers(online),
      });
    }
    if (offline.length > 0) {
      groups.push({
        key: 'offline',
        label: 'Offline',
        members: sortMembers(offline),
      });
    }

    return groups;
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- voice state (useVoiceStore) is read synchronously inside the computation but intentionally omitted from deps: member grouping recomputes on roster/role/server changes, and voice-presence badging is separately reactive through the store subscription on the rendered rows — including voice state here would re-run the entire grouping on every mic-unmute event
  }, [filteredMembers, serverRoles, activeServerId, sortMembers]);

  const renderGroup = (group: {
    key: string;
    label: string;
    emoji?: string;
    color?: string;
    members: ServerMember[];
  }) => {
    if (group.members.length === 0) return null;
    const isCollapsed = collapsedGroups.has(group.key);
    return (
      <div className="member-group" key={group.key}>
        <button
          className="member-group-header member-group-header--clickable"
          onClick={() => toggleGroup(group.key)}
          type="button"
        >
          <svg
            className={`member-group-chevron${isCollapsed ? ' collapsed' : ''}`}
            width="10"
            height="10"
            viewBox="0 0 10 10"
            fill="none"
          >
            <path
              d="M2.5 3.5L5 6.5L7.5 3.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
          <span style={group.color ? { color: group.color } : undefined}>
            {group.emoji && <span className="member-group-emoji">{group.emoji}</span>}
            {group.label} &mdash; {group.members.length}
          </span>
        </button>
        {!isCollapsed &&
          group.members.map((member) => (
            <MemberItem
              key={member.user_id}
              member={member}
              status={getMemberStatus(member.user_id)}
              onClick={handleMemberClick}
              onContextMenu={handleMemberContextMenu}
            />
          ))}
      </div>
    );
  };

  return (
    <div className="member-list">
      <div className="member-list-header">
        <h3>Members</h3>
      </div>

      {/* Search */}
      {members.length > 0 && (
        <div className="member-list-search">
          <input
            type="text"
            className="member-list-search-input"
            placeholder="Search members..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
          />
        </div>
      )}

      {/* Loading State */}
      {isLoading && members.length === 0 && (
        <div className="member-list-skeletons">
          <div className="member-skeleton" />
          <div className="member-skeleton" />
          <div className="member-skeleton" />
          <div className="member-skeleton" />
          <div className="member-skeleton" />
        </div>
      )}

      {/* Error State */}
      {error && members.length === 0 && !isLoading && (
        <div className="member-list-error">
          <p>{error}</p>
          <button
            onClick={() => activeServerId && fetchMembers(activeServerId)}
            className="retry-btn"
          >
            Retry
          </button>
        </div>
      )}

      {/* Empty State */}
      {!isLoading && !error && members.length === 0 && (
        <div className="member-list-empty">
          <p>No members</p>
        </div>
      )}

      {/* Search empty state */}
      {!isLoading && searchQuery && filteredMembers.length === 0 && members.length > 0 && (
        <div className="member-list-empty">
          <p>No members found</p>
        </div>
      )}

      {/* Member Groups */}
      {filteredMembers.length > 0 && <>{roleGroups.map((group) => renderGroup(group))}</>}

      {/* Profile Card */}
      {selectedMember && selectedMemberData && (
        <MemberProfileCard
          member={selectedMemberData}
          status={getMemberStatus(selectedMemberData.user_id)}
          lastSeen={lastSeenByUser.get(selectedMemberData.user_id)}
          position={selectedMember.position}
          onClose={() => setSelectedMember(null)}
          onViewFullProfile={() => {
            setFullProfileUserId(selectedMember.userId);
            setSelectedMember(null);
          }}
        />
      )}

      {/* Context Menu */}
      {contextMenu && activeServer && (
        <MemberContextMenu
          member={contextMenu.member}
          position={contextMenu.position}
          serverId={activeServer.id}
          ownerUserId={activeServer.owner_id}
          onClose={() => setContextMenu(null)}
          onViewProfile={() => {
            setFullProfileUserId(contextMenu.member.user_id);
          }}
          onBan={(m) => {
            setContextMenu(null);
            setBanTarget(m);
          }}
          onKick={(m) => {
            setContextMenu(null);
            setKickTarget(m);
          }}
        />
      )}

      {/* Full Profile Modal */}
      {fullProfileMemberData && (
        <UserProfileModal
          isOpen={!!fullProfileMemberData}
          onClose={() => setFullProfileUserId(null)}
          member={fullProfileMemberData}
          presenceStatus={getMemberStatus(fullProfileMemberData.user_id)}
          lastSeen={lastSeenByUser.get(fullProfileMemberData.user_id)}
        />
      )}

      {/* Ban Confirmation Modal */}
      {activeServer && (
        <ConfirmActionModal
          isOpen={!!banTarget}
          onClose={() => setBanTarget(null)}
          title={`Ban ${banTarget?.display_name || banTarget?.username || 'User'}`}
          message="This will permanently remove them from the server and prevent them from rejoining."
          confirmLabel="Ban"
          loadingLabel="Banning..."
          onConfirm={async () => {
            if (!banTarget) return;
            const res = await apiFetch(
              `/api/v1/servers/${activeServer.id}/bans/${banTarget.user_id}`,
              { method: 'POST' }
            );
            if (!res.ok) {
              const data = await safeJson<{ error?: string }>(res);
              throw new Error(data?.error || 'Ban failed');
            }
            useMemberStore.getState().removeMember(banTarget.user_id);
          }}
        />
      )}

      {/* Kick Confirmation Modal */}
      {activeServer && (
        <ConfirmActionModal
          isOpen={!!kickTarget}
          onClose={() => setKickTarget(null)}
          title={`Kick ${kickTarget?.display_name || kickTarget?.username || 'User'}`}
          message="This will remove them from the server. They can rejoin with a new invite."
          confirmLabel="Kick"
          loadingLabel="Kicking..."
          onConfirm={async () => {
            if (!kickTarget) return;
            const res = await apiFetch(
              `/api/v1/servers/${activeServer.id}/members/${kickTarget.user_id}`,
              { method: 'DELETE' }
            );
            if (!res.ok) {
              const data = await safeJson<{ error?: string }>(res);
              throw new Error(data?.error || 'Kick failed');
            }
            useMemberStore.getState().removeMember(kickTarget.user_id);
          }}
        />
      )}
    </div>
  );
};

export default MemberList;
