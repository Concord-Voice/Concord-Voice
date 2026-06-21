import { useState, useEffect, useCallback, useMemo } from 'react';
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
} from 'lucide-react';
import { useFriendStore, type Friend } from '../../stores/friendStore';
import { useFriendOrgStore, type FriendCategory } from '../../stores/friendOrgStore';
import ContextMenu from '../ui/ContextMenu';
import MemberProfileCard, { type ProfileCardMember } from '../Members/MemberProfileCard';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import AddFriendModal from './AddFriendModal';
import CategoryManagerPanel from './CategoryManagerPanel';
import { errorMessage } from '../../utils/redactError';
import './DirectMessages.css';

interface FriendsListProps {
  onFriendClick?: (userId: string) => void;
}

// §5.1 DnD contract: two typed sources on one surface, disambiguated by dataTransfer type.
const DT_SECTION = 'application/concord-section'; // section-header handle → reorder sectionOrder
const DT_FRIEND = 'application/concord-friend'; // friend row → assign friend to a category

const FriendsList: React.FC<FriendsListProps> = ({ onFriendClick }) => {
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

  useEffect(() => {
    fetchFriends();
    fetchRequests();
  }, [fetchFriends, fetchRequests]);

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

  const incomingRequests = pendingRequests.filter((r) => r.direction === 'received');
  const outgoingRequests = pendingRequests.filter((r) => r.direction === 'sent');
  const incomingCount = incomingRequests.length;

  // userId -> category (one-per-friend); categorized friends never fall to Online/Offline.
  const catByMember = useMemo(() => {
    const m = new Map<string, FriendCategory>();
    for (const c of categoryList) for (const id of c.memberIds) m.set(id, c);
    return m;
  }, [categoryList]);

  const uncategorized = friends.filter((f) => !catByMember.has(f.userId));
  const onlineUncat = uncategorized.filter((f) => f.status !== 'offline');
  const offlineUncat = uncategorized.filter((f) => f.status === 'offline');

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

  type RenderSection =
    | { kind: 'pending'; key: string }
    | { kind: 'builtin'; key: string; label: string; friends: Friend[] }
    | { kind: 'category'; key: string; cat: FriendCategory; friends: Friend[] };

  const sections = order
    .map((key): RenderSection | null => {
      if (key === 'pending') {
        return pendingRequests.length ? { kind: 'pending', key } : null;
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
            friends: friends.filter((f) => cat.memberIds.includes(f.userId)),
          }
        : null;
    })
    .filter((s): s is RenderSection => s !== null);

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
    return (
      <button
        type="button"
        key={friend.id}
        className={`friend-item ${friend.status === 'offline' ? 'offline' : ''}`}
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
              {(friend.displayName || friend.username).charAt(0).toUpperCase()}
            </span>
          )}
          <span className={`member-status-dot ${friend.status}`} />
        </div>
        <span className="member-username" style={tintColor ? { color: tintColor } : undefined}>
          {friend.displayName || friend.username}
        </span>
      </button>
    );
  };

  return (
    <div className="friends-list">
      <div className="friends-list-header">
        <h3>
          Friends
          {incomingCount > 0 && (
            <span className="conversation-unread-badge friends-header-badge">{incomingCount}</span>
          )}
        </h3>
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
            className="friends-add-btn"
            onClick={() => setShowAddFriendModal(true)}
            title="Add Friend"
          >
            <Plus size={16} />
          </button>
        </div>
      </div>

      {/* Keyboard-reorder announcements (WCAG 4.1.3 status messages) */}
      <div className="sr-only" aria-live="polite" role="status">
        {reorderAnnouncement}
      </div>

      {friends.length === 0 && categoryList.length === 0 && pendingRequests.length === 0 ? (
        <div className="friends-list-empty">
          <UserPlus size={28} style={{ marginBottom: 8, opacity: 0.4 }} />
          <p>Add friends to see them here</p>
        </div>
      ) : (
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
                  <span className="friend-category-count">{pendingRequests.length}</span>
                </button>

                {!isCollapsed && (
                  <>
                    {/* Incoming requests */}
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
                              className="friend-request-btn friend-request-accept"
                              onClick={(e) => {
                                e.stopPropagation();
                                handleAccept(req.id);
                              }}
                              disabled={!!isLoading}
                              title="Accept"
                            >
                              {isLoading === 'accept' ? (
                                <span className="friend-request-spinner" />
                              ) : (
                                <Check size={14} />
                              )}
                            </button>
                            <button
                              className="friend-request-btn friend-request-decline"
                              onClick={(e) => {
                                e.stopPropagation();
                                handleDecline(req.id);
                              }}
                              disabled={!!isLoading}
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

                    {/* Outgoing requests */}
                    {outgoingRequests.map((req) => {
                      const displayName = req.toDisplayName || req.toUsername;
                      const initial = displayName.charAt(0).toUpperCase();
                      return (
                        <div
                          key={req.id}
                          className="friend-item friend-request-item friend-request-outgoing"
                        >
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
                )}
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
        })
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
