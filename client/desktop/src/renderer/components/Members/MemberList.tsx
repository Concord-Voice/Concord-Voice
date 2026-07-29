import React, { useEffect, useState, useCallback, useId, useRef, useMemo } from 'react';
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
import { Search, Users } from 'lucide-react';
import { AttributedPopover } from '../Layout/AttributedPopover';
import './MemberList.css';

interface MemberListProps {
  compact?: boolean;
}

interface MemberGroup {
  key: string;
  label: string;
  emoji?: string;
  color?: string;
  members: ServerMember[];
}

interface OpenMemberGroup {
  key: string;
  anchor: HTMLElement;
}

interface CompactMemberGroupsProps {
  groups: MemberGroup[];
  openGroup: OpenMemberGroup | null;
  selectedGroup?: MemberGroup;
  triggerId: string;
  getMemberStatus: (userId: string) => PresenceStatus;
  onMemberClick: (event: React.MouseEvent, member: ServerMember) => void;
  onMemberContextMenu: (event: React.MouseEvent, member: ServerMember) => void;
  setOpenGroup: React.Dispatch<React.SetStateAction<OpenMemberGroup | null>>;
}

const CompactMemberGroups: React.FC<CompactMemberGroupsProps> = ({
  groups,
  openGroup,
  selectedGroup,
  triggerId,
  getMemberStatus,
  onMemberClick,
  onMemberContextMenu,
  setOpenGroup,
}) => (
  <>
    <nav className="member-compact-rail" aria-label="Member groups">
      {groups.map((group) => {
        const isOpen = openGroup?.key === group.key;
        return (
          <button
            key={group.key}
            id={`${triggerId}-members-${group.key}`}
            type="button"
            className="member-compact-trigger"
            aria-expanded={isOpen}
            aria-controls={`member-group-${group.key}`}
            aria-label={`${group.label} — ${group.members.length}`}
            title={group.label}
            onPointerDown={(event) => event.stopPropagation()}
            onClick={(event) => {
              const anchor = event.currentTarget;
              setOpenGroup((current) =>
                current?.key === group.key ? null : { key: group.key, anchor }
              );
            }}
            style={group.color ? { color: group.color } : undefined}
          >
            {group.emoji ? (
              <span className="member-compact-emoji">{group.emoji}</span>
            ) : (
              <Users size={20} aria-hidden="true" />
            )}
            <span className="member-compact-trigger-count" aria-hidden="true">
              {group.members.length}
            </span>
          </button>
        );
      })}
    </nav>
    {selectedGroup && openGroup && (
      <AttributedPopover
        id={`member-group-${selectedGroup.key}`}
        anchor={openGroup.anchor}
        label={`${selectedGroup.label} — ${selectedGroup.members.length}`}
        open
        onClose={() => setOpenGroup(null)}
      >
        <div className="member-compact-grid">
          {selectedGroup.members.map((member) => (
            <MemberItem
              key={member.user_id}
              member={member}
              status={getMemberStatus(member.user_id)}
              onClick={onMemberClick}
              onContextMenu={onMemberContextMenu}
              compact
            />
          ))}
        </div>
      </AttributedPopover>
    )}
  </>
);

const MemberList: React.FC<MemberListProps> = ({ compact = false }) => {
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
  const [openGroup, setOpenGroup] = useState<OpenMemberGroup | null>(null);
  const [searchAnchor, setSearchAnchor] = useState<HTMLElement | null>(null);
  const compactTriggerId = useId();

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

  useEffect(() => {
    if (!compact) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- compact triggers unmount in standard mode, so its detached popover state must be discarded
      setOpenGroup(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- compact search uses a detached trigger that must not survive standard mode
      setSearchAnchor(null);
    }
  }, [compact]);

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
    const groups: MemberGroup[] = [];

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

  const renderGroup = (group: MemberGroup) => {
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

  const selectedGroup = openGroup
    ? roleGroups.find((group) => group.key === openGroup.key)
    : undefined;
  const activeSearchAnchor = compact && searchAnchor?.isConnected ? searchAnchor : null;

  useEffect(() => {
    if (openGroup && (!selectedGroup || !openGroup.anchor.isConnected)) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- live grouping can remove a compact trigger while retaining this component; discard its detached anchor before the key can reappear
      setOpenGroup(null);
    }
  }, [openGroup, selectedGroup]);

  return (
    <div className={`member-list${compact ? ' member-list--compact' : ''}`}>
      {!compact && (
        <div className="member-list-header">
          <h3>Members</h3>
        </div>
      )}

      {/* Search */}
      {compact && members.length > 0 && (
        <>
          <button
            id={`${compactTriggerId}-member-search`}
            type="button"
            className="member-compact-trigger member-search-trigger"
            aria-label="Search members"
            title="Search members"
            aria-expanded={activeSearchAnchor !== null}
            aria-controls={`${compactTriggerId}-member-search-popover`}
            onClick={(event) => {
              const anchor = event.currentTarget;
              setSearchAnchor((current) => (current === anchor ? null : anchor));
            }}
          >
            <Search size={20} aria-hidden="true" />
          </button>
          <AttributedPopover
            id={`${compactTriggerId}-member-search-popover`}
            anchor={activeSearchAnchor}
            label="Search members"
            open={activeSearchAnchor !== null}
            placement="left"
            onClose={() => setSearchAnchor(null)}
          >
            <div className="member-list-search member-list-search--popover">
              <input
                type="text"
                className="member-list-search-input"
                placeholder="Search members..."
                value={searchQuery}
                onChange={(event) => setSearchQuery(event.target.value)}
                autoFocus
              />
            </div>
          </AttributedPopover>
        </>
      )}
      {!compact && members.length > 0 && (
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
      {filteredMembers.length > 0 &&
        (compact ? (
          <CompactMemberGroups
            groups={roleGroups}
            openGroup={openGroup}
            selectedGroup={selectedGroup}
            triggerId={compactTriggerId}
            getMemberStatus={getMemberStatus}
            onMemberClick={handleMemberClick}
            onMemberContextMenu={handleMemberContextMenu}
            setOpenGroup={setOpenGroup}
          />
        ) : (
          <>{roleGroups.map((group) => renderGroup(group))}</>
        ))}

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
