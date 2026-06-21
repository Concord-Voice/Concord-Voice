import { useState, useCallback } from 'react';
import { LogOut, Trash2, Edit3, X } from 'lucide-react';
import { useDMStore, type DMConversation } from '../../stores/dmStore';
import { useUserStore } from '../../stores/userStore';
import GroupMemberItem from './GroupMemberItem';
import EditGroupModal from './EditGroupModal';
import './DirectMessages.css';

interface GroupInfoPanelProps {
  conversation: DMConversation;
  onClose: () => void;
}

const GroupInfoPanel: React.FC<GroupInfoPanelProps> = ({ conversation, onClose }) => {
  const [isEditModalOpen, setIsEditModalOpen] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const currentUserId = useUserStore((s) => s.user?.id) || '';

  const currentUserParticipant = conversation.participants.find((p) => p.userId === currentUserId);
  const isCurrentUserAdmin = currentUserParticipant?.role === 'admin';
  const isCreator = conversation.createdBy === currentUserId;

  const groupName =
    conversation.name ||
    conversation.participants.map((p) => p.displayName || p.username).join(', ');

  const handleRoleChange = useCallback(
    async (userId: string, role: 'admin' | 'member') => {
      setActionError(null);
      try {
        await useDMStore.getState().updateMemberRole(conversation.id, userId, role);
      } catch (err) {
        setActionError(err instanceof Error ? err.message : 'Failed to update role');
      }
    },
    [conversation.id]
  );

  const handleRemoveMember = useCallback(
    async (userId: string) => {
      setActionError(null);
      try {
        await useDMStore.getState().removeGroupMember(conversation.id, userId);
      } catch (err) {
        setActionError(err instanceof Error ? err.message : 'Failed to remove member');
      }
    },
    [conversation.id]
  );

  const handleLeaveGroup = useCallback(async () => {
    if (!confirm('Are you sure you want to leave this group?')) return;
    setActionError(null);
    try {
      await useDMStore.getState().leaveGroup(conversation.id);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : 'Failed to leave group');
    }
  }, [conversation.id]);

  const handleDeleteGroup = useCallback(async () => {
    if (!confirm('Are you sure you want to delete this group? This cannot be undone.')) return;
    setActionError(null);
    try {
      await useDMStore.getState().deleteGroup(conversation.id);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : 'Failed to delete group');
    }
  }, [conversation.id]);

  return (
    <div className="group-info-panel">
      <div className="group-info-header">
        <h3>Group Info</h3>
        <button type="button" className="group-info-close-btn" onClick={onClose}>
          <X size={18} />
        </button>
      </div>

      <div className="group-info-body">
        {/* Group icon / initial */}
        <div className="group-info-icon">
          <span>{(conversation.name || 'G').charAt(0).toUpperCase()}</span>
        </div>

        {/* Group name + edit */}
        <div className="group-info-name-row">
          <h4 className="group-info-name">{groupName}</h4>
          {isCurrentUserAdmin && (
            <button
              type="button"
              className="group-info-edit-btn"
              onClick={() => setIsEditModalOpen(true)}
              aria-label="Edit group name"
            >
              <Edit3 size={14} />
            </button>
          )}
        </div>

        <div className="group-info-member-count">
          {conversation.participants.length} member
          {conversation.participants.length === 1 ? '' : 's'}
        </div>

        {actionError && <div className="group-info-error">{actionError}</div>}

        {/* Member list */}
        <div className="group-info-members">
          <h5 className="group-info-members-title">Members</h5>
          {conversation.participants.map((p) => (
            <GroupMemberItem
              key={p.userId}
              participant={p}
              conversationId={conversation.id}
              createdBy={conversation.createdBy}
              currentUserId={currentUserId}
              isCurrentUserAdmin={isCurrentUserAdmin}
              onRoleChange={handleRoleChange}
              onRemove={handleRemoveMember}
            />
          ))}
        </div>

        {/* Action buttons */}
        <div className="group-info-actions">
          <button
            type="button"
            className="group-info-action-btn group-info-leave-btn"
            onClick={handleLeaveGroup}
          >
            <LogOut size={14} />
            Leave Group
          </button>

          {(isCreator || isCurrentUserAdmin) && (
            <button
              type="button"
              className="group-info-action-btn group-info-delete-btn"
              onClick={handleDeleteGroup}
            >
              <Trash2 size={14} />
              Delete Group
            </button>
          )}
        </div>
      </div>

      <EditGroupModal
        isOpen={isEditModalOpen}
        onClose={() => setIsEditModalOpen(false)}
        conversationId={conversation.id}
        currentName={conversation.name}
      />
    </div>
  );
};

export default GroupInfoPanel;
