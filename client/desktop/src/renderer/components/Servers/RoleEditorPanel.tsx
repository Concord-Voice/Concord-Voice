import React, { useState, useEffect, useRef, useCallback } from 'react';
import PermissionGrid from '../Permissions/PermissionGrid';
import ToggleSwitch from '../Settings/ToggleSwitch';
import EmojiPicker from '../EmojiPicker/LazyEmojiPicker';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { parsePermissions } from '../../utils/permissions';
import type { Role } from '../../types/server';

interface RoleEditorPanelProps {
  roles: Role[];
  onCreateRole: () => Promise<Role | null | void>;
  onSaveRole: (
    roleId: string,
    data: {
      name: string;
      color: string;
      emoji: string;
      permissions: string;
      display_separately: boolean;
      mentionable: boolean;
    }
  ) => Promise<void>;
  onDeleteRole: (roleId: string) => Promise<void>;
}

const RoleEditorPanel: React.FC<RoleEditorPanelProps> = ({
  roles,
  onCreateRole,
  onSaveRole,
  onDeleteRole,
}) => {
  const [selectedRoleId, setSelectedRoleId] = useState<string | null>(null);
  const [editRoleName, setEditRoleName] = useState('');
  const [editRoleColor, setEditRoleColor] = useState('#99aab5');
  const [editRoleEmoji, setEditRoleEmoji] = useState('');
  const [showRoleEmojiPicker, setShowRoleEmojiPicker] = useState(false);
  const [editRoleDisplaySeparately, setEditRoleDisplaySeparately] = useState(false);
  const [editRoleMentionable, setEditRoleMentionable] = useState(false);
  const [editRolePermissions, setEditRolePermissions] = useState<bigint>(0n);
  const [isRoleSaving, setIsRoleSaving] = useState(false);
  const roleEmojiPickerRef = useRef<HTMLDivElement>(null);

  const selectedRole = roles.find((r) => r.id === selectedRoleId) || null;

  useEffect(() => {
    if (selectedRole) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleName from selectedRole when the selected role changes; not a render loop
      setEditRoleName(selectedRole.name);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleColor from selectedRole when the selected role changes; not a render loop
      setEditRoleColor(selectedRole.color || '#99aab5');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleEmoji from selectedRole when the selected role changes; not a render loop
      setEditRoleEmoji(selectedRole.emoji || '');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: closes emoji picker when the selected role changes; not a render loop
      setShowRoleEmojiPicker(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleDisplaySeparately from selectedRole when the selected role changes; not a render loop
      setEditRoleDisplaySeparately(selectedRole.display_separately);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleMentionable from selectedRole when the selected role changes; not a render loop
      setEditRoleMentionable(selectedRole.mentionable);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRolePermissions from selectedRole when the selected role changes; not a render loop
      setEditRolePermissions(parsePermissions(selectedRole.permissions));
    }
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- keyed on selectedRoleId (stable), NOT selectedRole (a fresh roles.find() reference each render); re-running on a `roles` refresh would clobber the user's unsaved in-progress edits
  }, [selectedRoleId]);

  const handlePermChange = useCallback((newValue: bigint) => {
    setEditRolePermissions(newValue);
  }, []);

  const handleSaveRole = async () => {
    if (!selectedRoleId) return;
    setIsRoleSaving(true);
    try {
      await onSaveRole(selectedRoleId, {
        name: editRoleName,
        color: editRoleColor,
        emoji: editRoleEmoji || '',
        permissions: editRolePermissions.toString(),
        display_separately: editRoleDisplaySeparately,
        mentionable: editRoleMentionable,
      });
    } finally {
      setIsRoleSaving(false);
    }
  };

  const handleDeleteRole = async () => {
    if (!selectedRoleId) return;
    await onDeleteRole(selectedRoleId);
    setSelectedRoleId(null);
  };

  return (
    <div className="roles-layout">
      <div className="roles-list">
        {[...roles]
          .sort((a, b) => b.position - a.position)
          .map((role) => (
            <button
              key={role.id}
              className={`role-item ${selectedRoleId === role.id ? 'selected' : ''}`}
              onClick={() => setSelectedRoleId(role.id)}
            >
              <span
                className="role-color-dot"
                style={{ backgroundColor: role.color || '#99aab5' }}
              />
              <span style={role.color ? { color: role.color } : undefined}>{role.name}</span>
            </button>
          ))}
        <button
          className="create-role-btn"
          onClick={async () => {
            const created = await onCreateRole();
            if (created && 'id' in created) {
              setSelectedRoleId(created.id);
            }
          }}
        >
          + Create Role
        </button>
      </div>

      <div className="role-editor">
        {selectedRole ? (
          <>
            {selectedRole.is_default && (
              <div className="role-default-note">
                This is the default role assigned to all members.
              </div>
            )}

            <div className="form-group">
              <label htmlFor="role-editor-name" className="form-label">
                Role Name
              </label>
              <input
                id="role-editor-name"
                type="text"
                className="form-input"
                value={editRoleName}
                onChange={(e) => setEditRoleName(e.target.value)}
                disabled={isRoleSaving}
              />
            </div>

            <div className="form-group">
              <label htmlFor="role-editor-color" className="form-label">
                Role Color
              </label>
              <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                <input
                  id="role-editor-color"
                  type="color"
                  value={editRoleColor}
                  onChange={(e) => setEditRoleColor(e.target.value)}
                  disabled={isRoleSaving}
                  style={{
                    width: '40px',
                    height: '32px',
                    border: 'none',
                    cursor: 'pointer',
                    background: 'none',
                  }}
                />
                <input
                  type="text"
                  className="form-input"
                  value={editRoleColor}
                  onChange={(e) => setEditRoleColor(e.target.value)}
                  disabled={isRoleSaving}
                  style={{ width: '120px' }}
                />
              </div>
            </div>

            <div className="form-group">
              <span className="form-label">Role Emoji (Optional)</span>
              <div className="emoji-input-wrapper" ref={roleEmojiPickerRef}>
                <div className="emoji-input-container">
                  <button
                    type="button"
                    className={`emoji-picker-button ${editRoleEmoji ? 'has-emoji' : ''}`}
                    onClick={() => setShowRoleEmojiPicker(!showRoleEmojiPicker)}
                    disabled={isRoleSaving}
                    title={editRoleEmoji ? 'Change emoji' : 'Pick an emoji'}
                  >
                    {editRoleEmoji ? (
                      <span className="emoji-picker-button-emoji">{editRoleEmoji}</span>
                    ) : (
                      <span className="emoji-picker-button-placeholder">Pick an emoji</span>
                    )}
                  </button>
                  {editRoleEmoji && (
                    <button
                      type="button"
                      className="emoji-clear-btn"
                      onClick={() => setEditRoleEmoji('')}
                      disabled={isRoleSaving}
                      title="Remove emoji"
                    >
                      ✕
                    </button>
                  )}
                </div>
                {showRoleEmojiPicker && (
                  <div className="emoji-picker-container">
                    <EmojiPicker
                      mode="inline"
                      onSelect={(emoji: string) => {
                        setEditRoleEmoji(emoji);
                        setShowRoleEmojiPicker(false);
                      }}
                      onClose={() => setShowRoleEmojiPicker(false)}
                    />
                  </div>
                )}
              </div>
              <span className="channel-form-hint">
                Shown next to the role name in the member list and next to member names in chat.
              </span>
            </div>

            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-label">Display Separately</span>
                <span className="settings-row-hint">
                  Members with this role appear in their own group in the member list.
                </span>
              </div>
              <ToggleSwitch
                checked={editRoleDisplaySeparately}
                onChange={setEditRoleDisplaySeparately}
                disabled={isRoleSaving}
              />
            </div>

            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-label">Mentionable</span>
                <span className="settings-row-hint">
                  Members with the Mention Roles permission can @mention this role to notify all who
                  hold it.
                </span>
              </div>
              <ToggleSwitch
                checked={editRoleMentionable}
                onChange={setEditRoleMentionable}
                disabled={isRoleSaving}
              />
            </div>

            <div className="form-group">
              <span className="form-label">Permissions</span>
              <PermissionGrid value={editRolePermissions} onChange={handlePermChange} mode="role" />
            </div>

            <div className="role-editor-actions">
              {!selectedRole.is_default && (
                <button
                  type="button"
                  className="server-settings-cancel-btn"
                  onClick={handleDeleteRole}
                  disabled={isRoleSaving}
                >
                  Delete
                </button>
              )}
              <button
                type="button"
                className="server-settings-submit-btn"
                onClick={handleSaveRole}
                disabled={isRoleSaving}
              >
                {isRoleSaving ? (
                  <>
                    Saving...
                    <LoadingSpinner size="small" inline />
                  </>
                ) : (
                  'Save Role'
                )}
              </button>
            </div>
          </>
        ) : (
          <div style={{ color: 'var(--text-muted)', padding: '40px', textAlign: 'center' }}>
            Select a role to edit, or create a new one.
          </div>
        )}
      </div>
    </div>
  );
};

export default RoleEditorPanel;
