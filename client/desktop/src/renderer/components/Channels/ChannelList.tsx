import React, { useEffect, useRef, useMemo, useState, useCallback } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { ChevronDown, ChevronRight } from 'lucide-react';
import { useChannelStore } from '../../stores/channelStore';
import { useServerStore } from '../../stores/serverStore';
import { useUnreadStore } from '../../stores/unreadStore';
import {
  useVoiceStore,
  type ChannelVoiceMember,
  channelVoiceMemberFromApi,
} from '../../stores/voiceStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useDraftMessageStore } from '../../stores/draftMessageStore';
import { Permissions, hasPermission, MOVE_MEMBERS } from '../../utils/permissions';
import { apiFetch } from '../../services/apiClient';
import { moveVoiceParticipant } from '../../services/voiceParticipantApi';
import { Channel, ChannelGroup } from '../../types/chat';
import { useUserStore } from '../../stores/userStore';
import ChannelItem, { type VoiceMemberInfo } from './ChannelItem';
import VoiceParticipantContextMenu, {
  type VoiceMenuParticipant,
} from '../Voice/VoiceParticipantContextMenu';
import MemberProfileCard, { type ProfileCardMember } from '../Members/MemberProfileCard';
import './ChannelList.css';

/** MIME type carrying a dragged sidebar voice participant (#487 Scope B DnD). */
const PARTICIPANT_DND_MIME = 'application/concord-voice-participant';

interface DraggedParticipant {
  participantUserId: string;
  sourceChannelId: string;
  sourceServerId: string;
}

// ─── Drag-and-drop helper functions (extracted to reduce cognitive complexity) ──

type ChannelUpdate = { channel_id: string; group_id: string | null; position: number };

interface InsertPos {
  targetId: string;
  side: 'before' | 'after';
  targetType: 'channel' | 'category' | 'gap';
}

/** Parse the gap ID to determine a slot index within the sorted category list. */
export function parseGapSlot(gapId: string, sortedGroups: ChannelGroup[]): number {
  if (gapId === '__gap-bottom' || gapId === '__gap-uncategorized') {
    return sortedGroups.length;
  }
  if (gapId.startsWith('__gap-before-')) {
    const catId = gapId.replace('__gap-before-', '');
    const catIdx = sortedGroups.findIndex((g) => g.id === catId);
    return Math.max(0, catIdx);
  }
  return 0;
}

/** Build position updates when a channel is dropped onto a gap zone (uncategorized). */
export function buildGapDropUpdates(
  sourceChannel: Channel,
  gapSlot: number,
  nonLinked: Channel[],
  sourceId: string
): ChannelUpdate[] {
  const updates: ChannelUpdate[] = [];
  const slotBase = (gapSlot + 1) * 1000;
  const slotEnd = slotBase + 1000;
  const sameSlot = nonLinked
    .filter(
      (c) => !c.group_id && c.id !== sourceId && c.position >= slotBase && c.position < slotEnd
    )
    .sort((a, b) => a.position - b.position);
  sameSlot.push(sourceChannel);
  sameSlot.forEach((c, i) => {
    updates.push({ channel_id: c.id, group_id: null, position: slotBase + i });
  });
  return updates;
}

/** Build position updates when a channel is dropped into a category or reordered within a group. */
export function buildNormalDropUpdates(
  sourceChannel: Channel,
  targetGroupId: string | null,
  targetGroup: ChannelGroup | null | undefined,
  insertPos: InsertPos,
  nonLinked: Channel[],
  sortedGroups: ChannelGroup[],
  sourceId: string
): ChannelUpdate[] {
  const updates: ChannelUpdate[] = [];
  const groupChans = nonLinked
    .filter((c) => (targetGroupId ? c.group_id === targetGroupId : !c.group_id))
    .filter((c) => c.id !== sourceId)
    .sort((a, b) => a.position - b.position);

  let insertIdx: number;
  if (targetGroup) {
    insertIdx = groupChans.length;
  } else {
    const targetIdx = groupChans.findIndex((c) => c.id === insertPos.targetId);
    insertIdx = insertPos.side === 'after' ? targetIdx + 1 : targetIdx;
    if (insertIdx < 0) insertIdx = groupChans.length;
  }
  groupChans.splice(insertIdx, 0, sourceChannel);

  if (targetGroupId) {
    groupChans.forEach((c, i) => {
      updates.push({ channel_id: c.id, group_id: targetGroupId, position: i });
    });
  } else {
    const targetChannel = nonLinked.find((c) => c.id === insertPos.targetId);
    let slot = sortedGroups.length;
    const targetPos = targetChannel?.position ?? 0;
    if (targetPos >= 1000) {
      slot = Math.floor(targetPos / 1000) - 1;
      slot = Math.max(0, Math.min(slot, sortedGroups.length));
    }
    const slotBase = (slot + 1) * 1000;
    groupChans.forEach((c, i) => {
      updates.push({ channel_id: c.id, group_id: null, position: slotBase + i });
    });
  }
  return updates;
}

/** Resolve the target group ID from the drop position. Returns `undefined` if unresolvable. */
export function resolveTargetGroupId(
  insertPos: InsertPos,
  nonLinked: Channel[],
  channelGroups: ChannelGroup[]
): string | null | undefined {
  if (insertPos.targetType === 'gap') return null;
  if (insertPos.targetType === 'category') {
    const g = channelGroups.find((g) => g.id === insertPos.targetId);
    return g ? g.id : undefined;
  }
  const targetChannel = nonLinked.find((c) => c.id === insertPos.targetId);
  return targetChannel ? targetChannel.group_id || null : undefined;
}

/** Build position updates for the old group after a channel moves out of it. */
export function buildOldGroupUpdates(
  oldGroupId: string | null,
  nonLinked: Channel[],
  sourceId: string,
  sortedGroups: ChannelGroup[]
): ChannelUpdate[] {
  const updates: ChannelUpdate[] = [];
  const oldGroupChans = nonLinked
    .filter((c) => (oldGroupId ? c.group_id === oldGroupId : !c.group_id))
    .filter((c) => c.id !== sourceId)
    .sort((a, b) => a.position - b.position);

  if (oldGroupId) {
    oldGroupChans.forEach((c, i) => {
      updates.push({ channel_id: c.id, group_id: oldGroupId, position: i });
    });
  } else {
    const slotMap = new Map<number, Channel[]>();
    for (const c of oldGroupChans) {
      const s = c.position >= 1000 ? Math.floor(c.position / 1000) - 1 : sortedGroups.length;
      if (!slotMap.has(s)) slotMap.set(s, []);
      slotMap.get(s)?.push(c);
    }
    for (const [s, chans] of slotMap) {
      const base = (s + 1) * 1000;
      [...chans]
        .sort((a, b) => a.position - b.position)
        .forEach((c, i) => {
          updates.push({ channel_id: c.id, group_id: null, position: base + i });
        });
    }
  }
  return updates;
}

// ─── Category Group Header sub-component ────────────────────────────────

interface CategoryGroupHeaderProps {
  group: ChannelGroup;
  isCollapsed: boolean;
  groupUnreadCount: number;
  groupVoiceUsers: ChannelVoiceMember[];
  isCategoryDragging: boolean;
  isCategoryDragOver: boolean;
  canReorder: boolean;
  draggingType: 'channel' | 'category' | null;
  onToggleCollapsed: (id: string) => void;
  onContextMenu: (group: ChannelGroup, position: { x: number; y: number }) => void;
  onDragStart: (e: React.DragEvent, id: string, type: 'channel' | 'category') => void;
  onDragOver: (e: React.DragEvent, targetId: string, targetType: 'channel' | 'category') => void;
  onDrop: (e: React.DragEvent) => void;
  onDragEnd: () => void;
  onCategoryDragOver: (e: React.DragEvent, groupId: string) => void;
  itemRef: (id: string, el: HTMLElement | null) => void;
}

const CategoryGroupHeader: React.FC<CategoryGroupHeaderProps> = React.memo(
  ({
    group,
    isCollapsed,
    groupUnreadCount,
    groupVoiceUsers,
    isCategoryDragging,
    isCategoryDragOver,
    canReorder,
    draggingType,
    onToggleCollapsed,
    onContextMenu,
    onDragStart,
    onDragOver,
    onDrop,
    onDragEnd,
    onCategoryDragOver,
    itemRef,
  }) => (
    <section
      ref={(el) => itemRef(group.id, el)}
      className={`channel-group-header channel-group-header--collapsible ${isCategoryDragging ? 'dragging' : ''} ${isCategoryDragOver ? 'drag-over' : ''}`}
      aria-label={`${group.name} channel group`}
      draggable={canReorder}
      onDragStart={(e) => onDragStart(e, group.id, 'category')}
      onDragOver={(e) => {
        if (draggingType === 'channel') {
          onCategoryDragOver(e, group.id);
        } else {
          onDragOver(e, group.id, 'category');
        }
      }}
      onDrop={onDrop}
      onDragEnd={onDragEnd}
      onContextMenu={(e) => {
        e.preventDefault();
        e.stopPropagation();
        onContextMenu(group, { x: e.clientX, y: e.clientY });
      }}
    >
      <button
        type="button"
        className="channel-group-header__toggle"
        onClick={() => onToggleCollapsed(group.id)}
        aria-expanded={!isCollapsed}
        {...(group.name ? { 'aria-label': group.name } : {})}
      >
        <span className="channel-group-header__chevron">
          {isCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
        </span>
        <span>{(group.name ?? '').toUpperCase()}</span>
        {isCollapsed && groupVoiceUsers.length > 0 && (
          <CollapsedVoiceAvatars voiceUsers={groupVoiceUsers} />
        )}
        {isCollapsed && groupUnreadCount > 0 && (
          <span className="channel-group-header__count">
            {groupUnreadCount > 99 ? '99+' : groupUnreadCount}
          </span>
        )}
      </button>
    </section>
  )
);

CategoryGroupHeader.displayName = 'CategoryGroupHeader';

/** Collapsed voice user avatars shown on the category header. */
const CollapsedVoiceAvatars: React.FC<{ voiceUsers: ChannelVoiceMember[] }> = React.memo(
  ({ voiceUsers }) => (
    <div className="channel-group-header__voice-users">
      {voiceUsers.slice(0, 3).map((user) => (
        <div
          key={user.userId}
          className="channel-group-header__avatar"
          title={user.displayName || user.username}
        >
          {resolveMediaUrl(user.avatarUrl) ? (
            <img src={resolveMediaUrl(user.avatarUrl)} alt="" />
          ) : (
            <span>{(user.displayName || user.username).charAt(0).toUpperCase()}</span>
          )}
        </div>
      ))}
      {voiceUsers.length > 3 && (
        <span className="channel-group-header__avatar-overflow">+{voiceUsers.length - 3}</span>
      )}
    </div>
  )
);

CollapsedVoiceAvatars.displayName = 'CollapsedVoiceAvatars';

interface ChannelListProps {
  onContextMenu: (channel: Channel, position: { x: number; y: number }) => void;
  onEmptyContextMenu: (position: { x: number; y: number }) => void;
  onCategoryContextMenu: (group: ChannelGroup, position: { x: number; y: number }) => void;
}

const ChannelList: React.FC<ChannelListProps> = ({
  onContextMenu,
  onEmptyContextMenu,
  onCategoryContextMenu,
}) => {
  const activeServerId = useServerStore((state) => state.activeServerId);
  const channels = useChannelStore((state) => state.channels);
  const channelGroups = useChannelStore((state) => state.channelGroups);
  const collapsedGroups = useChannelStore((state) => state.collapsedGroups);
  const toggleGroupCollapsed = useChannelStore((state) => state.toggleGroupCollapsed);
  const activeChannelId = useChannelStore((state) => state.activeChannelId);
  const isLoading = useChannelStore((state) => state.isLoading);
  const error = useChannelStore((state) => state.error);
  const fetchChannels = useChannelStore((state) => state.fetchChannels);
  const setActiveChannel = useChannelStore((state) => state.setActiveChannel);
  const clearChannels = useChannelStore((state) => state.clearChannels);
  const reorderChannels = useChannelStore((state) => state.reorderChannels);
  const updateChannelGroup = useChannelStore((state) => state.updateChannelGroup);
  const unreadCounts = useUnreadStore((state) => state.unreadCounts);
  const clearUnread = useUnreadStore((state) => state.clearUnread);
  const drafts = useDraftMessageStore((s) => s.drafts);
  const voiceChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceParticipants = useVoiceStore((s) => s.participants);
  const voiceConnectionState = useVoiceStore((s) => s.connectionState);
  const channelVoiceMembers = useVoiceStore((s) => s.channelVoiceMembers);
  const setChannelVoiceMembers = useVoiceStore((s) => s.setChannelVoiceMembers);
  const clearAllChannelVoiceMembers = useVoiceStore((s) => s.clearAllChannelVoiceMembers);
  const showVoiceTextChat = useVoiceStore((s) => s.showVoiceTextChat);
  const setShowVoiceTextChat = useVoiceStore((s) => s.setShowVoiceTextChat);
  const fetchedServerRef = useRef<string | null>(null);

  // DnD state
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [draggingType, setDraggingType] = useState<'channel' | 'category' | null>(null);
  const [dragInsertPos, setDragInsertPos] = useState<{
    targetId: string;
    side: 'before' | 'after';
    targetType: 'channel' | 'category' | 'gap';
  } | null>(null);
  const lastInsertRef = useRef<typeof dragInsertPos>(null);
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const itemRefs = useRef<Map<string, HTMLElement>>(new Map());

  const activeServerPerms = usePermissionStore(
    (s) => (activeServerId ? s.serverPermissions[activeServerId] : undefined) ?? 0n
  );
  const canReorder = hasPermission(activeServerPerms, Permissions.MANAGE_CHANNELS);
  const currentUserId = useUserStore((s) => s.user?.id);

  // ── Voice-participant interactivity (#487 Scope A/B) ──
  const [participantMenu, setParticipantMenu] = useState<{
    participant: VoiceMenuParticipant;
    channelId: string;
    position: { x: number; y: number };
  } | null>(null);
  const [participantCard, setParticipantCard] = useState<{
    member: ProfileCardMember;
    position: { x: number; y: number };
  } | null>(null);
  const [draggedParticipant, setDraggedParticipant] = useState<DraggedParticipant | null>(null);
  const [participantDropTargetId, setParticipantDropTargetId] = useState<string | null>(null);

  const toMenuParticipant = useCallback(
    (p: VoiceMemberInfo): VoiceMenuParticipant => ({
      userId: p.userId,
      username: p.username,
      displayName: p.displayName,
      isMuted: p.isMuted,
      serverMuted: p.serverMuted ?? false,
      serverDeafened: p.serverDeafened ?? false,
    }),
    []
  );

  const handleParticipantClick = useCallback(
    (e: React.MouseEvent, _channelId: string, p: VoiceMemberInfo) => {
      e.preventDefault();
      e.stopPropagation();
      setParticipantMenu(null);
      setParticipantCard({
        member: {
          user_id: p.userId,
          username: p.username,
          display_name: p.displayName,
        },
        position: { x: e.clientX, y: e.clientY },
      });
    },
    []
  );

  const handleParticipantContextMenu = useCallback(
    (e: React.MouseEvent, channelId: string, p: VoiceMemberInfo) => {
      e.preventDefault();
      e.stopPropagation();
      setParticipantCard(null);
      setParticipantMenu({
        participant: toMenuParticipant(p),
        channelId,
        position: { x: e.clientX, y: e.clientY },
      });
    },
    [toMenuParticipant]
  );

  // Whether the local user may drag-move OTHERS (UX gate only — server is
  // authoritative). Self-drag is always permitted.
  const canMoveOthers = hasPermission(activeServerPerms, MOVE_MEMBERS);

  const handleParticipantDragStart = useCallback(
    (e: React.DragEvent, channelId: string, p: VoiceMemberInfo) => {
      const isSelf = p.userId === currentUserId;
      if (!isSelf && !canMoveOthers) {
        e.preventDefault();
        return;
      }
      if (!activeServerId) {
        e.preventDefault();
        return;
      }
      const payload: DraggedParticipant = {
        participantUserId: p.userId,
        sourceChannelId: channelId,
        sourceServerId: activeServerId,
      };
      e.dataTransfer.setData(PARTICIPANT_DND_MIME, JSON.stringify(payload));
      e.dataTransfer.effectAllowed = 'move';
      e.stopPropagation();
      setDraggedParticipant(payload);
    },
    [activeServerId, canMoveOthers, currentUserId]
  );

  const handleParticipantDragEnd = useCallback(() => {
    setDraggedParticipant(null);
    setParticipantDropTargetId(null);
  }, []);

  /** A drop is valid only onto a different voice channel in the same server. */
  const isValidParticipantDrop = useCallback(
    (target: Channel, dragged: DraggedParticipant | null): boolean =>
      !!dragged &&
      target.type === 'voice' &&
      target.server_id === dragged.sourceServerId &&
      target.server_id === activeServerId &&
      target.id !== dragged.sourceChannelId,
    [activeServerId]
  );

  const handleParticipantDragOver = useCallback(
    (e: React.DragEvent, channel: Channel) => {
      if (!isValidParticipantDrop(channel, draggedParticipant)) return;
      e.preventDefault();
      e.stopPropagation();
      e.dataTransfer.dropEffect = 'move';
      if (participantDropTargetId !== channel.id) setParticipantDropTargetId(channel.id);
    },
    [draggedParticipant, isValidParticipantDrop, participantDropTargetId]
  );

  const handleParticipantDrop = useCallback(
    (e: React.DragEvent, channel: Channel) => {
      e.preventDefault();
      e.stopPropagation();
      let dragged: DraggedParticipant | null = null;
      try {
        dragged = JSON.parse(e.dataTransfer.getData(PARTICIPANT_DND_MIME)) as DraggedParticipant;
      } catch {
        dragged = null;
      }
      setDraggedParticipant(null);
      setParticipantDropTargetId(null);
      if (!isValidParticipantDrop(channel, dragged) || !dragged) return;
      void moveVoiceParticipant(
        dragged.sourceServerId,
        dragged.participantUserId,
        channel.id
      ).catch(() => {
        // Server is authoritative; failures surface via voice_state_update / errors.
      });
    },
    [isValidParticipantDrop]
  );

  // Fetch channels when active server changes.
  // Guard prevents StrictMode double-mount from firing duplicate HTTP requests.
  const channelFetchRef = useRef<string | null>(null);

  useEffect(() => {
    if (activeServerId) {
      if (channelFetchRef.current !== activeServerId) {
        channelFetchRef.current = activeServerId;
        fetchChannels(activeServerId);
      }
    } else {
      channelFetchRef.current = null;
      clearChannels();
    }
    return () => {
      // Allow refetch if effect re-runs with a different serverId
      // (but NOT on StrictMode unmount — ref persists across remounts)
    };
  }, [activeServerId, fetchChannels, clearChannels]);

  // Fetch voice participants for all voice channels when channel list loads
  useEffect(() => {
    const voiceChans = channels.filter((c) => c.type === 'voice');
    if (voiceChans.length === 0 || !activeServerId) return;
    // Only fetch once per server switch
    if (fetchedServerRef.current === activeServerId) return;
    fetchedServerRef.current = activeServerId;

    // Fetch per-channel voice members for sidebar display.
    // Server-wide voice counts are now provided by the backend via
    // the server_voice_counts WebSocket broadcast.
    voiceChans.forEach(async (channel) => {
      try {
        const res = await apiFetch(`/api/v1/channels/${channel.id}/voice/participants`);
        if (!res.ok) return;
        const data = await res.json();
        const members: ChannelVoiceMember[] = (data.participants || []).map(
          channelVoiceMemberFromApi
        );
        if (members.length > 0) {
          setChannelVoiceMembers(channel.id, members);
        }
      } catch {
        // Non-critical — voice participants just won't show
      }
    });
  }, [channels, activeServerId, setChannelVoiceMembers]);

  // Clear voice members when server changes
  useEffect(() => {
    return () => {
      clearAllChannelVoiceMembers();
      fetchedServerRef.current = null;
    };
  }, [activeServerId, clearAllChannelVoiceMembers]);

  // Clear unread + mark as read whenever the active channel changes
  // (handles both manual clicks AND auto-focus from server switch)
  useEffect(() => {
    if (!activeChannelId) return;
    clearUnread(activeChannelId);
    apiFetch(`/api/v1/channels/${activeChannelId}/read`, { method: 'POST' }).catch(() => {});

    // If all channel unreads are now cleared, clear the server dot too
    const { unreadCounts: currentCounts } = useUnreadStore.getState();
    if (currentCounts.size === 0 && activeServerId) {
      useUnreadStore.getState().clearServerUnread(activeServerId);
    }
  }, [activeChannelId, activeServerId, clearUnread]);

  const handleContextMenu = useCallback(
    (e: React.MouseEvent, channel: Channel) => {
      e.preventDefault();
      e.stopPropagation();
      onContextMenu(channel, { x: e.clientX, y: e.clientY });
    },
    [onContextMenu]
  );

  const handleEmptyContextMenu = (e: React.MouseEvent) => {
    // Only fire if clicking empty space (not a channel item or group header)
    if ((e.target as HTMLElement).closest('.channel-item, .channel-group-header')) return;
    e.preventDefault();
    e.stopPropagation();
    onEmptyContextMenu({ x: e.clientX, y: e.clientY });
  };

  const handleChannelClick = useCallback(
    (channel: Channel) => {
      setActiveChannel(channel.id);
      // clearUnread + read mark handled by useEffect([activeChannelId]) above
    },
    [setActiveChannel]
  );

  const handleLinkedTextClick = useCallback(
    (voiceChannel: Channel, linkedText: Channel) => {
      setActiveChannel(voiceChannel.id);
      const opening = !showVoiceTextChat;
      setShowVoiceTextChat(opening);
      if (opening && linkedText) {
        clearUnread(linkedText.id);
        apiFetch(`/api/v1/channels/${linkedText.id}/read`, { method: 'POST' }).catch(() => {});
      }
    },
    [setActiveChannel, showVoiceTextChat, setShowVoiceTextChat, clearUnread]
  );

  const handleItemRef = useCallback((id: string, el: HTMLElement | null) => {
    if (el) itemRefs.current.set(id, el);
    else itemRefs.current.delete(id);
  }, []);

  // ── Drag-and-drop handlers ──

  const handleDragStart = useCallback(
    (e: React.DragEvent, id: string, type: 'channel' | 'category') => {
      if (!canReorder) return;
      e.dataTransfer.setData('application/concord-channel', JSON.stringify({ id, type }));
      e.dataTransfer.effectAllowed = 'move';
      setDraggingId(id);
      setDraggingType(type);

      // Create semi-transparent drag image
      const el = itemRefs.current.get(id);
      if (el) {
        const clone = el.cloneNode(true) as HTMLElement;
        const rect = el.getBoundingClientRect();
        Object.assign(clone.style, {
          position: 'fixed',
          top: '-9999px',
          left: '-9999px',
          width: `${rect.width}px`,
          opacity: '0.7',
          pointerEvents: 'none',
        });
        document.body.appendChild(clone);
        e.dataTransfer.setDragImage(clone, rect.width / 2, rect.height / 2);
        requestAnimationFrame(() => clone.remove());
      }
    },
    [canReorder]
  );

  const handleDragOver = useCallback(
    (e: React.DragEvent, targetId: string, targetType: 'channel' | 'category') => {
      e.preventDefault();
      e.stopPropagation();
      e.dataTransfer.dropEffect = 'move';

      // Categories can only reorder among other categories, not drop onto channels
      if (draggingType === 'category' && targetType === 'channel') return;

      const el = itemRefs.current.get(targetId);
      if (!el) return;

      const rect = el.getBoundingClientRect();
      const midY = rect.top + rect.height / 2;
      const deadZone = rect.height * 0.25;

      let side: 'before' | 'after';
      if (e.clientY < midY - deadZone) {
        side = 'before';
      } else if (e.clientY > midY + deadZone) {
        side = 'after';
      } else {
        // In dead zone — keep last position or default to 'after'
        if (lastInsertRef.current?.targetId === targetId) return;
        side = 'after';
      }

      const newPos = { targetId, side, targetType };
      if (lastInsertRef.current?.targetId === targetId && lastInsertRef.current?.side === side)
        return;

      lastInsertRef.current = newPos;
      setDragInsertPos(newPos);
    },
    [draggingType]
  );

  const handleDragEnd = useCallback(() => {
    setDraggingId(null);
    setDraggingType(null);
    setDragInsertPos(null);
    lastInsertRef.current = null;
  }, []);

  // Gap zones between categories — dropping here makes a channel uncategorized
  const handleGapDragOver = useCallback((e: React.DragEvent, gapId: string) => {
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = 'move';
    const newPos = { targetId: gapId, side: 'after' as const, targetType: 'gap' as const };
    lastInsertRef.current = newPos;
    setDragInsertPos(newPos);
  }, []);

  // Dragging a channel over a category header — treat as "move into category"
  const handleCategoryChannelDragOver = useCallback((e: React.DragEvent, groupId: string) => {
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = 'move';
    const newPos = {
      targetId: groupId,
      side: 'after' as const,
      targetType: 'category' as const,
    };
    lastInsertRef.current = newPos;
    setDragInsertPos(newPos);
  }, []);

  /** Handle a channel being dropped — resolves target group and builds position updates. */
  const handleChannelDrop = useCallback(
    (sourceId: string, insertPos: InsertPos) => {
      const nonLinked = channels.filter((c) => !c.linked_voice_channel_id);
      const sortedGroups = [...channelGroups].sort((a, b) => a.position - b.position);
      const sourceChannel = nonLinked.find((c) => c.id === sourceId);
      if (!sourceChannel || !activeServerId) return;

      const targetGroupId = resolveTargetGroupId(insertPos, nonLinked, channelGroups);
      if (targetGroupId === undefined) return;

      const gapSlot =
        insertPos.targetType === 'gap' ? parseGapSlot(insertPos.targetId, sortedGroups) : null;

      let updates: ChannelUpdate[];
      if (insertPos.targetType === 'gap' && gapSlot !== null) {
        updates = buildGapDropUpdates(sourceChannel, gapSlot, nonLinked, sourceId);
      } else {
        const targetGroup =
          insertPos.targetType === 'category'
            ? channelGroups.find((g) => g.id === insertPos.targetId)
            : null;
        updates = buildNormalDropUpdates(
          sourceChannel,
          targetGroupId,
          targetGroup,
          insertPos,
          nonLinked,
          sortedGroups,
          sourceId
        );
      }

      const oldGroupId = sourceChannel.group_id || null;
      if (oldGroupId !== targetGroupId) {
        updates = updates.concat(
          buildOldGroupUpdates(oldGroupId, nonLinked, sourceId, sortedGroups)
        );
      }

      reorderChannels(updates);
      apiFetch(`/api/v1/servers/${activeServerId}/channels/reorder`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ channels: updates }),
      }).catch(() => {
        fetchChannels(activeServerId);
      });
    },
    [activeServerId, channels, channelGroups, reorderChannels, fetchChannels]
  );

  /** Handle a category being reordered via drop. */
  const handleCategoryDrop = useCallback(
    (sourceId: string, insertPos: InsertPos) => {
      if (!activeServerId) return;
      const sorted = [...channelGroups].sort((a, b) => a.position - b.position);
      const filtered = sorted.filter((g) => g.id !== sourceId);
      const targetIdx = filtered.findIndex((g) => g.id === insertPos.targetId);
      if (targetIdx === -1) return;

      const insertIdx = insertPos.side === 'after' ? targetIdx + 1 : targetIdx;
      const sourceGroup = channelGroups.find((g) => g.id === sourceId);
      if (!sourceGroup) return;
      filtered.splice(insertIdx, 0, sourceGroup);

      filtered.forEach((g, i) => {
        updateChannelGroup(g.id, { position: i });
      });

      Promise.all(
        filtered.map((g, i) =>
          apiFetch(`/api/v1/servers/${activeServerId}/channel-groups/${g.id}`, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ position: i }),
          })
        )
      ).catch(() => {
        fetchChannels(activeServerId);
      });
    },
    [activeServerId, channelGroups, updateChannelGroup, fetchChannels]
  );

  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      const insertPos = lastInsertRef.current;
      lastInsertRef.current = null;
      requestAnimationFrame(() => handleDragEnd());

      if (!insertPos || !activeServerId) return;

      let parsed: { id: string; type: string };
      try {
        parsed = JSON.parse(e.dataTransfer.getData('application/concord-channel'));
      } catch {
        return;
      }

      const { id: sourceId, type: sourceType } = parsed;
      if (sourceId === insertPos.targetId) return;

      if (sourceType === 'channel') {
        handleChannelDrop(sourceId, insertPos);
      } else if (sourceType === 'category') {
        handleCategoryDrop(sourceId, insertPos);
      }
    },
    [activeServerId, handleChannelDrop, handleCategoryDrop, handleDragEnd]
  );

  // Build interleaved channel structure: categories sorted by position, with
  // uncategorized channels able to appear between/above/below categories.
  //
  // Uncategorized channels use a slot-encoded position: (slot+1)*1000 + orderInSlot
  //   slot 0 → before first category, slot 1 → after first category, etc.
  // Legacy channels (position < 1000) render at the bottom for backward compat.
  // Filter out linked text channels — they render inline below their parent voice channel.
  const groupedChannels = useMemo(() => {
    const nonLinked = channels.filter((c) => !c.linked_voice_channel_id);
    const sortedGroups = [...channelGroups].sort((a, b) => a.position - b.position);
    const numSlots = sortedGroups.length + 1; // slots 0..N (before first, between each, after last)

    // Separate uncategorized channels into slots
    const uncategorized = nonLinked.filter((c) => !c.group_id);
    const slotBuckets: Channel[][] = Array.from({ length: numSlots }, () => []);
    const legacyBucket: Channel[] = [];

    for (const ch of uncategorized) {
      if (ch.position >= 1000) {
        const slot = Math.floor(ch.position / 1000) - 1;
        const clampedSlot = Math.max(0, Math.min(slot, numSlots - 1));
        slotBuckets[clampedSlot].push(ch);
      } else {
        legacyBucket.push(ch);
      }
    }
    // Sort within each bucket by position
    for (const bucket of slotBuckets) bucket.sort((a, b) => a.position - b.position);
    legacyBucket.sort((a, b) => a.position - b.position);

    // Interleave: slot0, category0, slot1, category1, ..., slotN, legacy
    const result: { group: ChannelGroup | null; channels: Channel[]; slotIndex?: number }[] = [];

    for (let i = 0; i <= sortedGroups.length; i++) {
      if (slotBuckets[i].length > 0) {
        result.push({ group: null, channels: slotBuckets[i], slotIndex: i });
      }
      if (i < sortedGroups.length) {
        const g = sortedGroups[i];
        result.push({
          group: g,
          channels: nonLinked
            .filter((c) => c.group_id === g.id)
            .sort((a, b) => a.position - b.position),
        });
      }
    }

    // Legacy uncategorized at the bottom
    if (legacyBucket.length > 0) {
      result.push({ group: null, channels: legacyBucket });
    }

    return result;
  }, [channels, channelGroups]);

  // Map voice channel ID → linked text channel (for inline rendering)
  const linkedTextByVoice = useMemo(() => {
    const map = new Map<string, Channel>();
    for (const c of channels) {
      if (c.linked_voice_channel_id) {
        map.set(c.linked_voice_channel_id, c);
      }
    }
    return map;
  }, [channels]);

  // Clear unread for linked text channel while voice text chat is open
  // (handles new messages arriving while the panel is visible)
  useEffect(() => {
    if (!showVoiceTextChat || !voiceChannelId) return;
    const linked = linkedTextByVoice.get(voiceChannelId);
    if (!linked) return;
    const count = unreadCounts.get(linked.id);
    if (count && count > 0) {
      clearUnread(linked.id);
      apiFetch(`/api/v1/channels/${linked.id}/read`, { method: 'POST' }).catch(() => {});
    }
  }, [showVoiceTextChat, voiceChannelId, linkedTextByVoice, unreadCounts, clearUnread]);

  /** Compute voice member list for a channel */
  const getVoiceMembers = useCallback(
    (channel: Channel): VoiceMemberInfo[] => {
      if (channel.type !== 'voice') return [];
      const isConnected = channel.id === voiceChannelId && voiceConnectionState === 'connected';
      if (isConnected) {
        return Object.values(voiceParticipants).map((p) => ({
          userId: p.userId,
          displayName: p.displayName,
          username: p.username,
          isMuted: p.isMuted,
          isSpeaking: p.isSpeaking,
          serverMuted: p.serverMuted || false,
          serverDeafened: p.serverDeafened || false,
          isDeafened: p.isDeafened || false,
        }));
      }
      return (channelVoiceMembers[channel.id] || []).map((m) => ({
        userId: m.userId,
        displayName: m.displayName,
        username: m.username,
        isMuted: m.isMuted,
        isSpeaking: false,
        serverMuted: m.serverMuted || false,
        serverDeafened: m.serverDeafened || false,
        isDeafened: m.isDeafened,
      }));
    },
    [voiceChannelId, voiceConnectionState, voiceParticipants, channelVoiceMembers]
  );

  const renderChannelItem = useCallback(
    (channel: Channel, index: number, arr: Channel[], isGrouped: boolean) => {
      const isActive = channel.id === activeChannelId;
      const unread = unreadCounts.get(channel.id) || 0;
      const isLastInGroup = isGrouped && index === arr.length - 1;
      const voiceMembers = getVoiceMembers(channel);
      const linkedText =
        channel.type === 'voice' ? (linkedTextByVoice.get(channel.id) ?? null) : null;
      const showLinkedText = !!(linkedText && voiceChannelId === channel.id);
      const linkedTextUnread = linkedText ? unreadCounts.get(linkedText.id) || 0 : 0;
      const isLinkedTextActive = !!(showLinkedText && showVoiceTextChat);

      return (
        <ChannelItem
          key={channel.id}
          channel={channel}
          isActive={isActive}
          unread={unread}
          isGrouped={isGrouped}
          isLastInGroup={isLastInGroup}
          voiceMembers={voiceMembers}
          linkedText={linkedText}
          showLinkedText={showLinkedText}
          isLinkedTextActive={isLinkedTextActive}
          linkedTextUnread={linkedTextUnread}
          hasDraft={!!(drafts[channel.id]?.text.trim() || drafts[channel.id]?.replyToId)}
          canReorder={canReorder}
          isDragging={draggingId === channel.id}
          showGhostBefore={
            dragInsertPos?.targetId === channel.id && dragInsertPos?.side === 'before'
          }
          showGhostAfter={dragInsertPos?.targetId === channel.id && dragInsertPos?.side === 'after'}
          onChannelClick={handleChannelClick}
          onContextMenu={handleContextMenu}
          onDragStart={handleDragStart}
          onDragOver={handleDragOver}
          onDrop={handleDrop}
          onDragEnd={handleDragEnd}
          onLinkedTextClick={handleLinkedTextClick}
          itemRef={handleItemRef}
          onParticipantClick={handleParticipantClick}
          onParticipantContextMenu={handleParticipantContextMenu}
          onParticipantDragStart={handleParticipantDragStart}
          onParticipantDragEnd={handleParticipantDragEnd}
          onParticipantDragOver={handleParticipantDragOver}
          onParticipantDrop={handleParticipantDrop}
          isParticipantDropTarget={participantDropTargetId === channel.id}
          draggingParticipantUserId={draggedParticipant?.participantUserId ?? null}
        />
      );
    },
    [
      activeChannelId,
      unreadCounts,
      drafts,
      voiceChannelId,
      showVoiceTextChat,
      linkedTextByVoice,
      getVoiceMembers,
      canReorder,
      draggingId,
      dragInsertPos,
      handleChannelClick,
      handleContextMenu,
      handleDragStart,
      handleDragOver,
      handleDrop,
      handleDragEnd,
      handleLinkedTextClick,
      handleItemRef,
      handleParticipantClick,
      handleParticipantContextMenu,
      handleParticipantDragStart,
      handleParticipantDragEnd,
      handleParticipantDragOver,
      handleParticipantDrop,
      participantDropTargetId,
      draggedParticipant,
    ]
  );

  // No server selected
  if (!activeServerId) {
    return (
      <div className="channel-list">
        <div className="channel-list-empty">
          <p>Select a server to view channels</p>
        </div>
      </div>
    );
  }

  return (
    <div
      className="channel-list"
      role="tree"
      aria-label="Server channels"
      tabIndex={0}
      onContextMenu={handleEmptyContextMenu}
      onDragOver={canReorder ? (e) => e.preventDefault() : undefined}
      onDrop={canReorder ? handleDrop : undefined}
    >
      {/* Loading State */}
      {isLoading && channels.length === 0 && (
        <div className="channel-list-skeletons">
          <div className="channel-skeleton" />
          <div className="channel-skeleton" />
          <div className="channel-skeleton" />
        </div>
      )}

      {/* Error State */}
      {error && channels.length === 0 && !isLoading && (
        <div className="channel-list-error">
          <p>{error}</p>
          <button
            onClick={() => activeServerId && fetchChannels(activeServerId)}
            className="retry-btn"
          >
            Retry
          </button>
        </div>
      )}

      {/* Empty State — only when no channels AND no categories */}
      {!isLoading && !error && channels.length === 0 && channelGroups.length === 0 && (
        <div className="channel-list-empty">
          <p>No channels yet</p>
          <p className="channel-list-empty-hint">Use &quot;+ Add&quot; above to create one</p>
        </div>
      )}

      {/* Dynamic Channel Groups */}
      {(channels.length > 0 || channelGroups.length > 0) && (
        <>
          {groupedChannels.map(({ group, channels: groupChannels, slotIndex }, _groupIdx) => {
            const groupId = group?.id ?? `__uncategorized-${slotIndex ?? 'legacy'}`;
            const isCollapsed = group ? collapsedGroups.includes(group.id) : false;

            // Uncategorized channels render at root level with no header
            if (!group) {
              if (groupChannels.length === 0) return null;
              return (
                <div key={groupId} className="channel-group channel-group--uncategorized">
                  {groupChannels.map((ch, i, arr) => renderChannelItem(ch, i, arr, false))}
                </div>
              );
            }

            // Count unreads in collapsed group (text channels only, exclude voice-linked text)
            const groupUnreadCount = isCollapsed
              ? groupChannels
                  .filter((c) => c.type !== 'voice' && !c.linked_voice_channel_id)
                  .reduce((sum, c) => sum + (unreadCounts.get(c.id) || 0), 0)
              : 0;

            // Stacked voice user avatars when collapsed
            const groupVoiceUsers = isCollapsed
              ? groupChannels
                  .filter((c) => c.type === 'voice')
                  .flatMap((c) => channelVoiceMembers[c.id] || [])
              : [];

            const isCategoryDragging = draggingId === group.id;
            const showCategoryGhostBefore =
              dragInsertPos?.targetId === group.id && dragInsertPos?.side === 'before';
            const showCategoryGhostAfter =
              dragInsertPos?.targetId === group.id &&
              dragInsertPos?.side === 'after' &&
              isCollapsed;
            const isCategoryDragOver =
              draggingType === 'channel' &&
              dragInsertPos?.targetId === group.id &&
              dragInsertPos?.targetType === 'category';

            // Gap zone IDs for before this category
            const gapId = `__gap-before-${group.id}`;
            const isGapActive = dragInsertPos?.targetId === gapId;

            return (
              <React.Fragment key={groupId}>
                {/* Gap zone before category — drop here to ungroup a channel */}
                <div
                  className={`channel-drag-gap ${draggingType === 'channel' ? 'channel-drag-gap--visible' : ''} ${isGapActive ? 'channel-drag-gap--active' : ''}`}
                  aria-hidden="true"
                  onDragOver={(e) => handleGapDragOver(e, gapId)}
                  onDrop={handleDrop}
                />
                <div className="channel-group">
                  {showCategoryGhostBefore && <div className="channel-drag-ghost" />}
                  <CategoryGroupHeader
                    group={group}
                    isCollapsed={isCollapsed}
                    groupUnreadCount={groupUnreadCount}
                    groupVoiceUsers={groupVoiceUsers}
                    isCategoryDragging={isCategoryDragging}
                    isCategoryDragOver={isCategoryDragOver}
                    canReorder={canReorder}
                    draggingType={draggingType}
                    onToggleCollapsed={toggleGroupCollapsed}
                    onContextMenu={onCategoryContextMenu}
                    onDragStart={handleDragStart}
                    onDragOver={handleDragOver}
                    onDrop={handleDrop}
                    onDragEnd={handleDragEnd}
                    onCategoryDragOver={handleCategoryChannelDragOver}
                    itemRef={handleItemRef}
                  />
                  {!isCollapsed &&
                    groupChannels.map((ch, i, arr) => renderChannelItem(ch, i, arr, true))}
                  {showCategoryGhostAfter && <div className="channel-drag-ghost" />}
                </div>
              </React.Fragment>
            );
          })}
          {/* Gap zone after all groups — drop here to ungroup at the bottom */}
          {channelGroups.length > 0 && (
            <div
              className={`channel-drag-gap ${draggingType === 'channel' ? 'channel-drag-gap--visible' : ''} ${dragInsertPos?.targetId === '__gap-bottom' ? 'channel-drag-gap--active' : ''}`}
              aria-hidden="true"
              onDragOver={(e) => handleGapDragOver(e, '__gap-bottom')}
              onDrop={handleDrop}
            />
          )}
        </>
      )}

      {/* Voice-participant profile card (name click) */}
      {participantCard && (
        <MemberProfileCard
          member={participantCard.member}
          position={participantCard.position}
          onClose={() => setParticipantCard(null)}
        />
      )}

      {/* Voice-participant context menu (right-click) */}
      {participantMenu && activeServerId && (
        <VoiceParticipantContextMenu
          participant={participantMenu.participant}
          serverId={activeServerId}
          channelId={participantMenu.channelId}
          position={participantMenu.position}
          onClose={() => setParticipantMenu(null)}
          onViewProfile={() => {
            setParticipantCard({
              member: {
                user_id: participantMenu.participant.userId,
                username: participantMenu.participant.username,
                display_name: participantMenu.participant.displayName,
              },
              position: participantMenu.position,
            });
            setParticipantMenu(null);
          }}
        />
      )}
    </div>
  );
};

export default ChannelList;
