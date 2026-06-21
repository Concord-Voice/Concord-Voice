import { useState, useCallback, useEffect, useRef } from 'react';
import { X, Search, UserPlus } from 'lucide-react';
import { useDMStore } from '../../stores/dmStore';
import { useFriendStore, type Friend } from '../../stores/friendStore';
import { useUserStore } from '../../stores/userStore';
import './DirectMessages.css';

interface CreateGroupModalProps {
  isOpen: boolean;
  onClose: () => void;
}

interface SelectedUser {
  userId: string;
  username: string;
  displayName?: string;
}

const MAX_GROUP_MEMBERS = 9; // 10 total including creator

const CreateGroupModal: React.FC<CreateGroupModalProps> = ({ isOpen, onClose }) => {
  const [groupName, setGroupName] = useState('');
  const [searchQuery, setSearchQuery] = useState('');
  const [selectedUsers, setSelectedUsers] = useState<SelectedUser[]>([]);
  const [isCreating, setIsCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const searchInputRef = useRef<HTMLInputElement>(null);

  const friends = useFriendStore((s) => s.friends);
  const currentUserId = useUserStore((s) => s.user?.id) || '';

  // Focus search input when modal opens
  useEffect(() => {
    if (!isOpen) return;
    const focusTimer = setTimeout(() => searchInputRef.current?.focus(), 100);
    return () => clearTimeout(focusTimer);
  }, [isOpen]);

  // Reset state when modal closes
  useEffect(() => {
    if (!isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets groupName when modal closes; not a render loop
      setGroupName('');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets searchQuery when modal closes; not a render loop
      setSearchQuery('');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets selectedUsers when modal closes; not a render loop
      setSelectedUsers([]);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when modal closes; not a render loop
      setError(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets isCreating when modal closes; not a render loop
      setIsCreating(false);
    }
  }, [isOpen]);

  const filteredFriends = friends.filter((f) => {
    // Don't show already-selected users
    if (selectedUsers.some((s) => s.userId === f.userId)) return false;
    // Don't show self
    if (f.userId === currentUserId) return false;
    // Filter by search query
    if (!searchQuery) return true;
    const q = searchQuery.toLowerCase();
    return (
      f.username.toLowerCase().includes(q) || (f.displayName?.toLowerCase().includes(q) ?? false)
    );
  });

  const handleAddUser = useCallback(
    (friend: Friend) => {
      if (selectedUsers.length >= MAX_GROUP_MEMBERS) return;
      setSelectedUsers((prev) => [
        ...prev,
        { userId: friend.userId, username: friend.username, displayName: friend.displayName },
      ]);
      setSearchQuery('');
      searchInputRef.current?.focus();
    },
    [selectedUsers.length]
  );

  const handleRemoveUser = useCallback((userId: string) => {
    setSelectedUsers((prev) => prev.filter((u) => u.userId !== userId));
  }, []);

  const handleCreate = useCallback(async () => {
    if (selectedUsers.length === 0 || isCreating) return;
    setIsCreating(true);
    setError(null);

    try {
      const userIds = selectedUsers.map((u) => u.userId);
      const name = groupName.trim() || undefined;
      await useDMStore.getState().createGroupDM(userIds, name);
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create group');
    } finally {
      setIsCreating(false);
    }
  }, [selectedUsers, groupName, isCreating, onClose]);

  if (!isOpen) return null;

  return (
    <dialog className="create-group-modal-overlay" open aria-label="Create Group DM">
      <div className="create-group-modal">
        <div className="create-group-modal-header">
          <h3>Create Group DM</h3>
          <button type="button" className="create-group-close-btn" onClick={onClose}>
            <X size={18} />
          </button>
        </div>

        <div className="create-group-modal-body">
          <input
            type="text"
            className="create-group-name-input"
            placeholder="Group Name (optional)"
            value={groupName}
            onChange={(e) => setGroupName(e.target.value)}
            maxLength={100}
          />

          {selectedUsers.length > 0 && (
            <div className="create-group-chips">
              {selectedUsers.map((u) => (
                <span key={u.userId} className="create-group-chip">
                  {u.displayName || u.username}
                  <button
                    type="button"
                    onClick={() => handleRemoveUser(u.userId)}
                    aria-label={`Remove ${u.username}`}
                  >
                    <X size={12} />
                  </button>
                </span>
              ))}
            </div>
          )}

          <div className="create-group-search">
            <Search size={14} className="create-group-search-icon" />
            <input
              ref={searchInputRef}
              type="text"
              placeholder="Search friends..."
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
            />
          </div>

          {selectedUsers.length >= MAX_GROUP_MEMBERS && (
            <div className="create-group-limit">
              Maximum {MAX_GROUP_MEMBERS + 1} members reached
            </div>
          )}

          <div className="create-group-results">
            {filteredFriends.length === 0 ? (
              <div className="create-group-no-results">
                {searchQuery ? 'No friends match your search' : 'No friends available to add'}
              </div>
            ) : (
              filteredFriends.map((friend) => (
                <div key={friend.userId} className="create-group-user-row">
                  <div className="conversation-avatar" style={{ width: 32, height: 32 }}>
                    <span className="conversation-avatar-initial" style={{ fontSize: 12 }}>
                      {(friend.displayName || friend.username).charAt(0).toUpperCase()}
                    </span>
                  </div>
                  <div className="create-group-user-info">
                    <span className="create-group-user-display">
                      {friend.displayName || friend.username}
                    </span>
                    {friend.displayName && (
                      <span className="create-group-user-username">@{friend.username}</span>
                    )}
                  </div>
                  <button
                    type="button"
                    className="create-group-add-btn"
                    onClick={() => handleAddUser(friend)}
                    disabled={selectedUsers.length >= MAX_GROUP_MEMBERS}
                  >
                    <UserPlus size={14} />
                  </button>
                </div>
              ))
            )}
          </div>

          {error && <div className="create-group-error">{error}</div>}
        </div>

        <div className="create-group-modal-footer">
          <button
            type="button"
            className="create-group-create-btn"
            onClick={handleCreate}
            disabled={selectedUsers.length === 0 || isCreating}
          >
            {isCreating ? 'Creating...' : `Create Group (${selectedUsers.length} selected)`}
          </button>
        </div>
      </div>
    </dialog>
  );
};

export default CreateGroupModal;
