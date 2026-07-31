import { useState, useEffect, useCallback, useId, useMemo } from 'react';
import { createPortal } from 'react-dom';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import {
  ChevronDown,
  ChevronRight,
  UserPlus,
  Plus,
  Check,
  X,
  Clock,
  MessageSquare,
  UserMinus,
  FolderPlus,
  GripVertical,
  Users,
  Moon,
  Folder,
  Search,
} from 'lucide-react';
import { useFriendStore, type Friend } from '../../stores/friendStore';
import { useFriendOrgStore, type FriendCategory } from '../../stores/friendOrgStore';
import ContextMenu from '../ui/ContextMenu';
import MemberProfileCard, { type ProfileCardMember } from '../Members/MemberProfileCard';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import AddFriendModal from './AddFriendModal';
import CategoryManagerPanel from './CategoryManagerPanel';
import { errorMessage } from '../../utils/redactError';
import { AttributedPopover } from '../Layout/AttributedPopover';
import './DirectMessages.css';

interface FriendsListProps {
  onFriendClick?: (userId: string) => void;
  compact?: boolean;
  /**
   * #2653 item 2a: the standard-presentation `[title | actions]` row belongs in DockShell's
   * own header so the dock renders ONE row, `[title | actions | pin]` (spec §4 item 2a).
   * The markup stays here because every piece of state behind it lives here — the pending
   * badge, the search term that mutes it, and both modals — so the row is delivered by
   * portalling into a host the dock supplies rather than by lifting that state out.
   *
   * Three states, deliberately distinct:
   *   `undefined` — not docked (standalone/tests): render the row in flow, as before.
   *   `null`      — docked, host not attached yet: render nothing for this commit.
   *   element     — portal the row into the dock header.
   * Compact ignores it: the spec keeps compact behaviour exactly, so the rail actions stay
   * in the body and the dock header carries only the pin.
   */
  headerHost?: HTMLElement | null;
}

type RenderSection =
  | { kind: 'pending'; key: string }
  | { kind: 'builtin'; key: string; label: string; friends: Friend[] }
  | { kind: 'category'; key: string; cat: FriendCategory; friends: Friend[] };

interface OpenFriendSection {
  key: string;
  anchor: HTMLElement;
}

interface CompactSectionTriggerProps {
  section: RenderSection;
  pendingCount: number;
  openSection: OpenFriendSection | null;
  triggerId: string;
  onToggleSection: (key: string, anchor: HTMLElement) => void;
}

interface CompactFriendsSectionsProps {
  sections: RenderSection[];
  pendingCount: number;
  openSection: OpenFriendSection | null;
  selectedSection?: RenderSection;
  triggerId: string;
  renderPendingRows: () => React.ReactNode;
  renderFriendRow: (friend: Friend, tintColor: string | null) => React.ReactNode;
  onToggleSection: (key: string, anchor: HTMLElement) => void;
  onCloseSection: () => void;
}

function getSectionLabel(section: RenderSection): string {
  if (section.kind === 'pending') return 'Pending Requests';
  if (section.kind === 'category') return section.cat.name;
  return section.label;
}

function getSectionCount(section: RenderSection, pendingCount: number): number {
  return section.kind === 'pending' ? pendingCount : section.friends.length;
}

function getFriendStatusLabel(status: Friend['status'], appearsOffline: boolean): string {
  if (appearsOffline) return 'Offline';
  if (status === 'dnd') return 'Do Not Disturb';
  return 'Online';
}

const CompactSectionIcon: React.FC<{ section: RenderSection }> = ({ section }) => {
  if (section.kind === 'pending') return <Clock size={20} />;
  if (section.kind === 'builtin') {
    // #2653 item 3: Moon, not WifiOff — offline is "not here", not a network fault. It is
    // also the only candidate that is a single continuous path, so it survives the dense
    // rail intact and stays distinguishable from the Online `Users` silhouette (spec C7).
    return section.key === 'online' ? <Users size={20} /> : <Moon size={20} />;
  }
  if (section.cat.emoji) {
    return <span className="friends-compact-emoji">{section.cat.emoji}</span>;
  }
  return <Folder size={20} />;
};

const CompactSectionTrigger: React.FC<CompactSectionTriggerProps> = ({
  section,
  pendingCount,
  openSection,
  triggerId,
  onToggleSection,
}) => {
  const label = getSectionLabel(section);
  const count = getSectionCount(section, pendingCount);
  const isOpen = openSection?.key === section.key;
  // Only the Offline BUILT-IN mutes its badge — a category is never this key.
  const isOffline = section.kind === 'builtin' && section.key === 'offline';

  return (
    <div className="friends-compact-category">
      <button
        id={`${triggerId}-friends-${section.key}`}
        type="button"
        className="friends-compact-trigger"
        aria-expanded={isOpen}
        aria-controls={`friends-section-${section.key}`}
        aria-label={`${label} — ${count}`}
        title={label}
        onPointerDown={(event) => event.stopPropagation()}
        onClick={(event) => onToggleSection(section.key, event.currentTarget)}
      >
        <CompactSectionIcon section={section} />
        <span
          className={`friends-compact-trigger-count${
            isOffline ? ' friends-compact-trigger-count--offline' : ''
          }`}
          aria-hidden="true"
        >
          {count}
        </span>
      </button>
    </div>
  );
};

const CompactFriendsSections: React.FC<CompactFriendsSectionsProps> = ({
  sections,
  pendingCount,
  openSection,
  selectedSection,
  triggerId,
  renderPendingRows,
  renderFriendRow,
  onToggleSection,
  onCloseSection,
}) => {
  return (
    <>
      <nav className="friends-compact-rail" aria-label="Friends categories">
        {sections.map((section) => (
          <CompactSectionTrigger
            key={section.key}
            section={section}
            pendingCount={pendingCount}
            openSection={openSection}
            triggerId={triggerId}
            onToggleSection={onToggleSection}
          />
        ))}
      </nav>
      {selectedSection && openSection && (
        <AttributedPopover
          id={`friends-section-${selectedSection.key}`}
          anchor={openSection.anchor}
          label={`${getSectionLabel(selectedSection)} — ${getSectionCount(selectedSection, pendingCount)}`}
          open
          onClose={onCloseSection}
        >
          {selectedSection.kind === 'pending'
            ? renderPendingRows()
            : selectedSection.friends.map((friend) =>
                renderFriendRow(
                  friend,
                  selectedSection.kind === 'category' ? selectedSection.cat.color : null
                )
              )}
        </AttributedPopover>
      )}
    </>
  );
};

// §5.1 DnD contract: two typed sources on one surface, disambiguated by dataTransfer type.
const DT_SECTION = 'application/concord-section'; // section-header handle → reorder sectionOrder
const DT_FRIEND = 'application/concord-friend'; // friend row → assign friend to a category

const FriendsList: React.FC<FriendsListProps> = ({
  onFriendClick,
  compact = false,
  headerHost,
}) => {
  // eslint-disable-next-line @eslint-react/use-state -- Set() is cheap to construct; lazy initializer would add noise without benefit
  const [collapsedCategories, setCollapsedCategories] = useState<Set<string>>(new Set());
  const [showAddFriendModal, setShowAddFriendModal] = useState(false);
  const [manageOpen, setManageOpen] = useState(false);
  const openCategoryManager = useCallback(() => setManageOpen(true), []);
  const categoryList = useFriendOrgStore((s) => s.categories);
  const sectionOrder = useFriendOrgStore((s) => s.sectionOrder);
  const reorderSections = useFriendOrgStore((s) => s.reorderSections);
  const assignFriend = useFriendOrgStore((s) => s.assignFriend);
  // DnD: the section key currently being dragged (handle source).
  const [draggingSection, setDraggingSection] = useState<string | null>(null);
  // Keyboard reorder: the section key currently "grabbed" via Space/Enter.
  const [grabbedSection, setGrabbedSection] = useState<string | null>(null);
  // aria-live announcement for keyboard moves.
  const [reorderAnnouncement, setReorderAnnouncement] = useState('');
  const friends = useFriendStore((s) => s.friends);
  const fetchFriends = useFriendStore((s) => s.fetchFriends);
  const pendingRequests = useFriendStore((s) => s.pendingRequests);
  const fetchRequests = useFriendStore((s) => s.fetchRequests);
  const acceptRequest = useFriendStore((s) => s.acceptRequest);
  const declineRequest = useFriendStore((s) => s.declineRequest);
  const removeFriend = useFriendStore((s) => s.removeFriend);
  const [actionLoading, setActionLoading] = useState<Record<string, 'accept' | 'decline' | null>>(
    {}
  );
  const [contextMenu, setContextMenu] = useState<{
    friend: Friend;
    position: { x: number; y: number };
  } | null>(null);
  // "Move to category" submenu open-state within the friend context menu.
  const [showCatSub, setShowCatSub] = useState(false);
  const [selectedFriend, setSelectedFriend] = useState<{
    userId: string;
    position: { x: number; y: number };
  } | null>(null);
  const [openSection, setOpenSection] = useState<OpenFriendSection | null>(null);
  const [searchQuery, setSearchQuery] = useState('');
  const [searchAnchor, setSearchAnchor] = useState<HTMLElement | null>(null);
  const compactTriggerId = useId();
  const addFriendTriggerId = useId();

  useEffect(() => {
    fetchFriends();
    fetchRequests();
  }, [fetchFriends, fetchRequests]);

  useEffect(() => {
    if (!compact) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: compact triggers unmount in standard mode, so their detached popover state must be discarded
      setOpenSection(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- compact search uses a detached trigger that must not survive standard mode
      setSearchAnchor(null);
    }
  }, [compact]);

  // Exactly one compact popover may be open: the search bubble and a section bubble are
  // anchored to neighbouring rail cells, so leaving both open overlaps them.
  const toggleSection = useCallback((key: string, anchor: HTMLElement) => {
    setSearchAnchor(null);
    setOpenSection((current) => (current?.key === key ? null : { key, anchor }));
  }, []);

  const closeSection = useCallback(() => setOpenSection(null), []);

  const toggleSearch = useCallback((anchor: HTMLElement) => {
    setOpenSection(null);
    setSearchAnchor((current) => (current === anchor ? null : anchor));
  }, []);

  const toggleCategory = (category: string) => {
    setCollapsedCategories((prev) => {
      const next = new Set(prev);
      if (next.has(category)) {
        next.delete(category);
      } else {
        next.add(category);
      }
      return next;
    });
  };

  const handleAccept = useCallback(
    async (requestId: string) => {
      setActionLoading((prev) => ({ ...prev, [requestId]: 'accept' }));
      try {
        await acceptRequest(requestId);
      } catch (err) {
        console.error('Failed to accept friend request:', errorMessage(err));
      } finally {
        setActionLoading((prev) => {
          const next = { ...prev };
          delete next[requestId];
          return next;
        });
      }
    },
    [acceptRequest]
  );

  const handleDecline = useCallback(
    async (requestId: string) => {
      setActionLoading((prev) => ({ ...prev, [requestId]: 'decline' }));
      try {
        await declineRequest(requestId);
      } catch (err) {
        console.error('Failed to decline friend request:', errorMessage(err));
      } finally {
        setActionLoading((prev) => {
          const next = { ...prev };
          delete next[requestId];
          return next;
        });
      }
    },
    [declineRequest]
  );

  const handleFriendContextMenu = useCallback((e: React.MouseEvent, friend: Friend) => {
    e.preventDefault();
    e.stopPropagation();
    setShowCatSub(false); // collapse the submenu each time a fresh menu opens
    setContextMenu({ friend, position: { x: e.clientX, y: e.clientY } });
  }, []);

  const handleRemoveFriend = useCallback(
    async (userId: string) => {
      setContextMenu(null);
      try {
        await removeFriend(userId);
      } catch (err) {
        console.error('Failed to remove friend:', errorMessage(err));
      }
    },
    [removeFriend]
  );

  const handleFriendClick = useCallback((e: React.MouseEvent, friend: Friend) => {
    setSelectedFriend((prev) =>
      prev?.userId === friend.userId
        ? null
        : { userId: friend.userId, position: { x: e.clientX, y: e.clientY } }
    );
  }, []);

  // Derive live friend data from the store (avoid stale snapshots)
  const selectedFriendData = selectedFriend
    ? friends.find((f) => f.userId === selectedFriend.userId)
    : null;

  // The header badge counts every incoming request, not the search results — it is the
  // "you have mail" signal, so a term must not make it appear to drop (#2653 item 2b, C9).
  const incomingCount = pendingRequests.filter((r) => r.direction === 'received').length;

  // #2653 item 2b: filter BEFORE the section build so categories, the Online/Offline
  // built-ins, pending, and the compact rail all inherit the term. Filtering after the
  // build (or only at the rendered rows) leaves sections whose counts disagree with their
  // contents. Mirrors MemberList's `filteredMembers` seam.
  const trimmedQuery = searchQuery.trim();
  const searchTerm = trimmedQuery.toLowerCase();

  const filteredFriends = useMemo(() => {
    if (!searchTerm) return friends;
    return friends.filter(
      (f) =>
        f.username.toLowerCase().includes(searchTerm) ||
        f.displayName?.toLowerCase().includes(searchTerm)
    );
  }, [friends, searchTerm]);

  const filteredPending = useMemo(() => {
    if (!searchTerm) return pendingRequests;
    return pendingRequests.filter((r) => {
      // Match the counterparty — the name the row actually renders — not the local user.
      const username = r.direction === 'received' ? r.fromUsername : r.toUsername;
      const displayName = r.direction === 'received' ? r.fromDisplayName : r.toDisplayName;
      return (
        username.toLowerCase().includes(searchTerm) ||
        displayName?.toLowerCase().includes(searchTerm)
      );
    });
  }, [pendingRequests, searchTerm]);

  const incomingRequests = filteredPending.filter((r) => r.direction === 'received');
  const outgoingRequests = filteredPending.filter((r) => r.direction === 'sent');

  // One gate for "this panel has something a term could filter". The search controls used to
  // key on `friends.length` alone while the sections they filter keyed on this triple, so a
  // user with no friends but a pending request (or a category) got a filterable panel and no
  // control to filter it. The no-match message below shares the gate for the same reason:
  // widening the controls without widening it leaves that user staring at a blank panel.
  const hasFilterableContent =
    friends.length > 0 || categoryList.length > 0 || pendingRequests.length > 0;
  const noMatches =
    searchTerm !== '' &&
    hasFilterableContent &&
    filteredFriends.length === 0 &&
    filteredPending.length === 0;
  const noMatchMessage = `No friends match "${trimmedQuery}"`;

  // userId -> category (one-per-friend); categorized friends never fall to Online/Offline.
  const catByMember = useMemo(() => {
    const m = new Map<string, FriendCategory>();
    for (const c of categoryList) for (const id of c.memberIds) m.set(id, c);
    return m;
  }, [categoryList]);

  const uncategorized = filteredFriends.filter((f) => !catByMember.has(f.userId));
  const onlineUncat = uncategorized.filter(
    (f) => f.status !== 'offline' && (!compact || f.status !== 'invisible')
  );
  const offlineUncat = uncategorized.filter(
    (f) => f.status === 'offline' || (compact && f.status === 'invisible')
  );

  // Build the ordered render list: persisted sectionOrder, then any category present in
  // the blob but missing from sectionOrder, then any missing built-ins — both appended in
  // default order (resilience — an empty store renders Pending/Online/Offline). Appending
  // orphaned categories is load-bearing: catByMember pulls their members out of
  // Online/Offline, so without a rendered category section those friends would vanish
  // entirely (Gitar review on #1704).
  const order = useMemo(() => {
    const present = new Set(sectionOrder);
    const missingCats = categoryList.map((c) => c.id).filter((id) => !present.has(id));
    const tail = (['pending', 'online', 'offline'] as const).filter((k) => !present.has(k));
    return [...sectionOrder, ...missingCats, ...tail];
  }, [sectionOrder, categoryList]);

  const sections = order
    .map((key): RenderSection | null => {
      if (key === 'pending') {
        return filteredPending.length ? { kind: 'pending', key } : null;
      }
      if (key === 'online') {
        return { kind: 'builtin', key, label: 'Online', friends: onlineUncat };
      }
      if (key === 'offline') {
        return { kind: 'builtin', key, label: 'Offline', friends: offlineUncat };
      }
      const cat = categoryList.find((c) => c.id === key);
      // orphan cat id (in sectionOrder but no matching category) → skip
      return cat
        ? {
            kind: 'category',
            key,
            cat,
            friends: filteredFriends.filter((f) => cat.memberIds.includes(f.userId)),
          }
        : null;
    })
    .filter((s): s is RenderSection => s !== null);
  const selectedSection = openSection
    ? sections.find((section) => section.key === openSection.key)
    : undefined;
  const activeSearchAnchor = compact && searchAnchor?.isConnected ? searchAnchor : null;

  useEffect(() => {
    if (openSection && (!selectedSection || !openSection.anchor.isConnected)) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- live friend/category updates can remove a compact trigger while retaining this component; discard its detached anchor before the key can reappear
      setOpenSection(null);
    }
  }, [openSection, selectedSection]);

  // The current render order of section keys (what reorderSections operates on).
  const currentOrderKeys = sections.map((s) => s.key);

  // Human-readable label for a section key (for aria announcements).
  const sectionLabel = (key: string): string => {
    if (key === 'pending') return 'Pending Requests';
    if (key === 'online') return 'Online';
    if (key === 'offline') return 'Offline';
    return categoryList.find((c) => c.id === key)?.name ?? key;
  };

  // --- Section reorder (drag) ---
  const handleSectionDragStart = (e: React.DragEvent, key: string) => {
    e.dataTransfer.setData(DT_SECTION, key);
    e.dataTransfer.effectAllowed = 'move';
    setDraggingSection(key);
  };

  const handleSectionDragEnd = () => setDraggingSection(null);

  // Move `sourceKey` to be positioned relative to `targetKey` (before unless dropping past midpoint).
  const reorderRelativeTo = (sourceKey: string, targetKey: string, side: 'before' | 'after') => {
    if (sourceKey === targetKey) return;
    const filtered = currentOrderKeys.filter((k) => k !== sourceKey);
    const targetIdx = filtered.indexOf(targetKey);
    if (targetIdx === -1) return;
    const insertIdx = side === 'after' ? targetIdx + 1 : targetIdx;
    filtered.splice(insertIdx, 0, sourceKey);
    reorderSections(filtered);
  };

  // A section header is both a section-reorder drop target AND a friend-assign drop target.
  const handleSectionHeaderDragOver = (e: React.DragEvent) => {
    const types = new Set(e.dataTransfer.types);
    if (types.has(DT_SECTION) || types.has(DT_FRIEND)) {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
    }
  };

  const handleSectionHeaderDrop = (e: React.DragEvent, section: RenderSection) => {
    const sectionSource = e.dataTransfer.getData(DT_SECTION);
    const friendSource = e.dataTransfer.getData(DT_FRIEND);

    if (sectionSource) {
      // Section reorder. Drop side determined by pointer vs. header midpoint.
      e.preventDefault();
      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
      const side: 'before' | 'after' = e.clientY < rect.top + rect.height / 2 ? 'before' : 'after';
      reorderRelativeTo(sectionSource, section.key, side);
      setDraggingSection(null);
      return;
    }

    if (friendSource) {
      // Friend assignment. Category header → assign; built-in Online/Offline → unassign.
      e.preventDefault();
      if (section.kind === 'category') {
        if (section.cat.memberIds.includes(friendSource)) return; // own category → no-op
        assignFriend(friendSource, section.cat.id);
      } else if (section.kind === 'builtin') {
        assignFriend(friendSource, null); // → Uncategorized
      }
    }
  };

  // --- Friend row drag (assign source) ---
  const handleFriendDragStart = (e: React.DragEvent, friend: Friend) => {
    e.dataTransfer.setData(DT_FRIEND, friend.userId);
    e.dataTransfer.effectAllowed = 'move';
  };

  // --- Keyboard reorder (WCAG 2.1.1 / 2.5.7) ---
  const moveGrabbedSection = (key: string, dir: -1 | 1) => {
    const idx = currentOrderKeys.indexOf(key);
    const nextIdx = idx + dir;
    if (idx === -1 || nextIdx < 0 || nextIdx >= currentOrderKeys.length) return;
    const next = [...currentOrderKeys];
    next.splice(idx, 1);
    next.splice(nextIdx, 0, key);
    reorderSections(next);
    setReorderAnnouncement(`${sectionLabel(key)} moved to position ${nextIdx + 1}`);
  };

  const handleHandleKeyDown = (e: React.KeyboardEvent, key: string) => {
    if (e.key === ' ' || e.key === 'Enter') {
      e.preventDefault();
      setGrabbedSection((prev) => {
        if (prev === key) {
          setReorderAnnouncement(`${sectionLabel(key)} dropped`);
          return null;
        }
        setReorderAnnouncement(
          `${sectionLabel(key)} grabbed. Use arrow keys to move, Escape to cancel.`
        );
        return key;
      });
      return;
    }
    if (e.key === 'Escape') {
      if (grabbedSection) {
        e.preventDefault();
        setReorderAnnouncement(`${sectionLabel(key)} move cancelled`);
        setGrabbedSection(null);
      }
      return;
    }
    if ((e.key === 'ArrowUp' || e.key === 'ArrowDown') && grabbedSection === key) {
      e.preventDefault();
      moveGrabbedSection(key, e.key === 'ArrowDown' ? 1 : -1);
    }
  };

  // A single friend row. `tintColor` (category color) tints the username when set.
  const renderFriendRow = (friend: Friend, tintColor: string | null) => {
    const friendColors = resolveUserAccentColors(friend.colorScheme);
    const displayName = friend.displayName || friend.username;
    const appearsOffline =
      friend.status === 'offline' || (compact && friend.status === 'invisible');
    const statusLabel = getFriendStatusLabel(friend.status, appearsOffline);
    return (
      <button
        type="button"
        key={friend.id}
        className={`friend-item ${appearsOffline ? 'offline' : ''}`}
        aria-label={displayName}
        title={statusLabel}
        // Friend-assign drag source (§5.1). `draggable` does NOT block the click/keyboard
        // handlers, and the onMouseDown stopPropagation below does NOT suppress dragstart.
        draggable
        onDragStart={(e) => handleFriendDragStart(e, friend)}
        onClick={(e) => handleFriendClick(e, friend)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            const rect = e.currentTarget.getBoundingClientRect();
            handleFriendClick(
              {
                clientX: rect.left + rect.width / 2,
                clientY: rect.top + rect.height / 2,
                currentTarget: e.currentTarget,
              } as unknown as React.MouseEvent,
              friend
            );
          }
        }}
        onMouseDown={(e) => e.stopPropagation()}
        onContextMenu={(e) => handleFriendContextMenu(e, friend)}
      >
        <div className="member-avatar">
          {resolveMediaUrl(friend.avatarUrl) ? (
            <img
              src={resolveMediaUrl(friend.avatarUrl)}
              alt={friend.username}
              className="member-avatar-img"
            />
          ) : (
            <span
              className="member-avatar-initial"
              style={
                friendColors ? { background: friendColors.gradient, color: '#fff' } : undefined
              }
            >
              {displayName.charAt(0).toUpperCase()}
            </span>
          )}
          <span className={`member-status-dot ${friend.status}`} />
        </div>
        <span className="member-username" style={tintColor ? { color: tintColor } : undefined}>
          {displayName}
        </span>
      </button>
    );
  };

  const renderPendingRows = () => (
    <>
      {incomingRequests.map((req) => {
        const displayName = req.fromDisplayName || req.fromUsername;
        const initial = displayName.charAt(0).toUpperCase();
        const isLoading = actionLoading[req.id];
        return (
          <div key={req.id} className="friend-item friend-request-item">
            <div className="member-avatar">
              <span className="member-avatar-initial">{initial}</span>
            </div>
            <div className="friend-request-info">
              <span className="member-username">{displayName}</span>
              <span className="friend-request-meta">Incoming request</span>
            </div>
            <div className="friend-request-actions">
              <button
                type="button"
                className="friend-request-btn friend-request-accept"
                onClick={(e) => {
                  e.stopPropagation();
                  handleAccept(req.id);
                }}
                disabled={!!isLoading}
                aria-label={`${isLoading === 'accept' ? 'Accepting' : 'Accept'} friend request from ${displayName}`}
                title="Accept"
              >
                {isLoading === 'accept' ? (
                  <span className="friend-request-spinner" />
                ) : (
                  <Check size={14} />
                )}
              </button>
              <button
                type="button"
                className="friend-request-btn friend-request-decline"
                onClick={(e) => {
                  e.stopPropagation();
                  handleDecline(req.id);
                }}
                disabled={!!isLoading}
                aria-label={`${isLoading === 'decline' ? 'Declining' : 'Decline'} friend request from ${displayName}`}
                title="Decline"
              >
                {isLoading === 'decline' ? (
                  <span className="friend-request-spinner" />
                ) : (
                  <X size={14} />
                )}
              </button>
            </div>
          </div>
        );
      })}

      {outgoingRequests.map((req) => {
        const displayName = req.toDisplayName || req.toUsername;
        const initial = displayName.charAt(0).toUpperCase();
        return (
          <div key={req.id} className="friend-item friend-request-item friend-request-outgoing">
            <div className="member-avatar">
              <span className="member-avatar-initial">{initial}</span>
            </div>
            <div className="friend-request-info">
              <span className="member-username">{displayName}</span>
              <span className="friend-request-meta">Outgoing request</span>
            </div>
            <div className="friend-request-actions">
              <span className="friend-request-pending-label">
                <Clock size={12} />
                Pending
              </span>
            </div>
          </div>
        );
      })}
    </>
  );

  const headerRow = (
    <>
      {!compact && (
        <h3 title="Friends">
          Friends
          {incomingCount > 0 && (
            <span
              className="conversation-unread-badge friends-header-badge"
              // While a term is active the badge can outnumber the visible rows; say why.
              title={searchTerm ? `${incomingCount} pending — hidden by search` : undefined}
            >
              {incomingCount}
            </span>
          )}
        </h3>
      )}
      <div className="friends-list-header-actions">
        <button
          type="button"
          className="friends-add-btn"
          aria-label="Manage categories"
          title="Manage categories"
          onClick={openCategoryManager}
        >
          <FolderPlus size={16} />
        </button>
        <button
          id={`${addFriendTriggerId}-add-friend`}
          type="button"
          className="friends-add-btn"
          onClick={() => setShowAddFriendModal(true)}
          aria-label="Add Friend"
          title="Add Friend"
        >
          <Plus size={16} />
        </button>
      </div>
    </>
  );

  // Docked + standard: the dock owns the row (see `headerHost`). `null` means the host has
  // not attached yet — ref callbacks flush before paint, so nothing is visibly missing.
  const headerBelongsToDock = !compact && headerHost !== undefined;

  return (
    <div className={`friends-list${compact ? ' friends-list--compact' : ''}`}>
      {headerBelongsToDock ? (
        headerHost && createPortal(headerRow, headerHost)
      ) : (
        <div className="friends-list-header">{headerRow}</div>
      )}

      {!compact && hasFilterableContent && (
        <div className="friends-list-search">
          <input
            type="text"
            className="friends-list-search-input"
            aria-label="Search friends"
            placeholder="Search friends..."
            value={searchQuery}
            onChange={(event) => setSearchQuery(event.target.value)}
          />
        </div>
      )}

      {/* Keyboard-reorder announcements (WCAG 4.1.3 status messages) */}
      <div className="sr-only" aria-live="polite" role="status">
        {reorderAnnouncement}
      </div>

      {compact && hasFilterableContent && (
        <>
          <button
            id={`${compactTriggerId}-friends-search`}
            type="button"
            className="friends-compact-trigger friends-search-trigger"
            aria-label="Search friends"
            // The rail has no room for a sentence, so the trigger carries the no-match state
            // once the popover is dismissed — the popover body says it while it is open.
            title={noMatches ? noMatchMessage : 'Search friends'}
            aria-expanded={activeSearchAnchor !== null}
            aria-controls={`${compactTriggerId}-friends-search-popover`}
            onClick={(event) => toggleSearch(event.currentTarget)}
          >
            <Search size={20} aria-hidden="true" />
          </button>
          <AttributedPopover
            id={`${compactTriggerId}-friends-search-popover`}
            anchor={activeSearchAnchor}
            label="Search friends"
            open={activeSearchAnchor !== null}
            placement="left"
            onClose={() => setSearchAnchor(null)}
          >
            <div className="friends-list-search friends-list-search--popover">
              <input
                type="text"
                className="friends-list-search-input"
                aria-label="Search friends"
                placeholder="Search friends..."
                value={searchQuery}
                onChange={(event) => setSearchQuery(event.target.value)}
                autoFocus
              />
              {/* Parity with MemberList's ungated `No members found`: a compact user whose
                  term matches nothing must get the same signal a standard user does. */}
              {noMatches && <p className="friends-list-search-empty">{noMatchMessage}</p>}
            </div>
          </AttributedPopover>
        </>
      )}

      {compact && hasFilterableContent && (
        <CompactFriendsSections
          sections={sections}
          pendingCount={filteredPending.length}
          openSection={openSection}
          selectedSection={selectedSection}
          triggerId={compactTriggerId}
          renderPendingRows={renderPendingRows}
          renderFriendRow={renderFriendRow}
          onToggleSection={toggleSection}
          onCloseSection={closeSection}
        />
      )}
      {!hasFilterableContent && (
        <div className={`friends-list-empty${compact ? ' friends-list-empty--compact' : ''}`}>
          <UserPlus className="friends-list-empty-icon" size={compact ? 20 : 28} />
          {!compact && <p>Add friends to see them here</p>}
        </div>
      )}
      {!compact &&
        hasFilterableContent &&
        sections.map((section) => {
          if (section.kind === 'pending') {
            const isCollapsed = collapsedCategories.has('pending');
            return (
              <div key="pending" className="friend-category">
                <button
                  type="button"
                  className="friend-category-header"
                  onClick={() => toggleCategory('pending')}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      toggleCategory('pending');
                    }
                  }}
                  aria-expanded={!isCollapsed}
                >
                  {isCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                  <span>Pending Requests</span>
                  <span className="friend-category-count">{filteredPending.length}</span>
                </button>

                {!isCollapsed && renderPendingRows()}
              </div>
            );
          }

          const isCategory = section.kind === 'category';
          const label = isCategory ? section.cat.name : section.label;
          const emoji = isCategory ? section.cat.emoji : '';
          const tintColor = isCategory ? section.cat.color : null;
          const isCollapsed = collapsedCategories.has(section.key);
          const isGrabbed = grabbedSection === section.key;
          const isDragging = draggingSection === section.key;

          return (
            <section
              key={section.key}
              aria-label={label}
              className={`friend-category ${isDragging ? 'friend-category-dragging' : ''} ${
                isGrabbed ? 'friend-category-grabbed' : ''
              }`}
              onDragOver={handleSectionHeaderDragOver}
              onDrop={(e) => handleSectionHeaderDrop(e, section)}
            >
              <div className="friend-category-header-row">
                <button
                  type="button"
                  className="friend-category-drag-handle"
                  aria-label={`Reorder ${label}`}
                  aria-pressed={isGrabbed}
                  draggable
                  onDragStart={(e) => handleSectionDragStart(e, section.key)}
                  onDragEnd={handleSectionDragEnd}
                  onKeyDown={(e) => handleHandleKeyDown(e, section.key)}
                >
                  <GripVertical size={12} />
                </button>
                <button
                  type="button"
                  className="friend-category-header"
                  onClick={() => toggleCategory(section.key)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      toggleCategory(section.key);
                    }
                  }}
                  aria-expanded={!isCollapsed}
                >
                  {isCollapsed ? <ChevronRight size={12} /> : <ChevronDown size={12} />}
                  {emoji && <span className="category-item-emoji">{emoji}</span>}
                  <span>{label}</span>
                  <span className="friend-category-count">{section.friends.length}</span>
                </button>
              </div>

              {!isCollapsed && section.friends.map((friend) => renderFriendRow(friend, tintColor))}
            </section>
          );
        })}

      {/* The built-in Online/Offline sections always render (they are drop targets), so an
          unmatched term shows a row of zero counts rather than an empty panel. Say so. */}
      {!compact && noMatches && (
        <div className="friends-list-empty">
          <p>{noMatchMessage}</p>
        </div>
      )}

      {contextMenu && (
        <ContextMenu position={contextMenu.position} onClose={() => setContextMenu(null)}>
          <ContextMenu.Header>
            {contextMenu.friend.displayName || contextMenu.friend.username}
          </ContextMenu.Header>
          <ContextMenu.Separator />
          <ContextMenu.Item
            icon={<MessageSquare size={14} />}
            label="Message"
            onClick={() => {
              onFriendClick?.(contextMenu.friend.userId);
              setContextMenu(null);
            }}
          />
          <ContextMenu.Separator />
          <ContextMenu.Item
            label="Move to category"
            hasSubMenu
            onClick={() => setShowCatSub((v) => !v)}
          />
          {showCatSub && (
            <ContextMenu.SubMenu>
              {categoryList.map((c) => (
                <ContextMenu.Item
                  key={c.id}
                  label={`${c.emoji ? c.emoji + ' ' : ''}${c.name}`}
                  icon={
                    catByMember.get(contextMenu.friend.userId)?.id === c.id ? (
                      <Check size={14} />
                    ) : undefined
                  }
                  onClick={() => {
                    assignFriend(contextMenu.friend.userId, c.id);
                    setContextMenu(null);
                  }}
                />
              ))}
              <ContextMenu.Item
                label="Uncategorized"
                icon={
                  catByMember.get(contextMenu.friend.userId) === undefined ? (
                    <Check size={14} />
                  ) : undefined
                }
                onClick={() => {
                  assignFriend(contextMenu.friend.userId, null);
                  setContextMenu(null);
                }}
              />
              <ContextMenu.Separator />
              <ContextMenu.Item
                label="New category…"
                onClick={() => {
                  setContextMenu(null);
                  openCategoryManager();
                }}
              />
            </ContextMenu.SubMenu>
          )}
          <ContextMenu.Separator />
          <ContextMenu.Item
            icon={<UserMinus size={14} />}
            label="Remove Friend"
            danger
            onClick={() => handleRemoveFriend(contextMenu.friend.userId)}
          />
        </ContextMenu>
      )}

      {/* Profile Card */}
      {selectedFriend && selectedFriendData && (
        <MemberProfileCard
          member={
            {
              user_id: selectedFriendData.userId,
              username: selectedFriendData.username,
              display_name: selectedFriendData.displayName,
              avatar_url: selectedFriendData.avatarUrl,
              color_scheme: selectedFriendData.colorScheme,
            } satisfies ProfileCardMember
          }
          status={selectedFriendData.status}
          position={selectedFriend.position}
          onClose={() => setSelectedFriend(null)}
        />
      )}

      <AddFriendModal isOpen={showAddFriendModal} onClose={() => setShowAddFriendModal(false)} />

      {manageOpen && <CategoryManagerPanel onClose={() => setManageOpen(false)} />}
    </div>
  );
};

export default FriendsList;
