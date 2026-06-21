import React, { useState, useMemo, useCallback } from 'react';
import PermissionGrid from './PermissionGrid';
import { ChannelOverride, UpsertOverrideRequest } from '../../stores/permissionStore';
import { Role } from '../../types/server';
import { ServerMember } from '../../stores/memberStore';
import { parsePermissions, countBits } from '../../utils/permissions';
import './OverridePanel.css';

interface OverridePanelProps {
  overrides: ChannelOverride[];
  roles: Role[];
  members: ServerMember[];
  onUpsert: (data: UpsertOverrideRequest) => Promise<void | boolean>;
  onDelete: (overrideId: string) => Promise<void | boolean>;
  disabled?: boolean;
  emptyMessage?: string;
}

const OverridePanel: React.FC<OverridePanelProps> = ({
  overrides,
  roles,
  members,
  onUpsert,
  onDelete,
  disabled = false,
  emptyMessage = 'No permission overrides configured.',
}) => {
  const [selectedOverrideId, setSelectedOverrideId] = useState<string | null>(null);
  const [editAllow, setEditAllow] = useState<bigint>(0n);
  const [editDeny, setEditDeny] = useState<bigint>(0n);

  // Add override form state
  const [addTargetType, setAddTargetType] = useState<'role' | 'user'>('role');
  const [addTargetId, setAddTargetId] = useState('');
  const [addAllow, setAddAllow] = useState<bigint>(0n);
  const [addDeny, setAddDeny] = useState<bigint>(0n);

  const roleOverrides = useMemo(
    () => overrides.filter((o) => o.target_type === 'role'),
    [overrides]
  );
  const userOverrides = useMemo(
    () => overrides.filter((o) => o.target_type === 'user'),
    [overrides]
  );

  const getTargetName = useCallback(
    (override: ChannelOverride): string => {
      if (override.target_type === 'role') {
        const role = roles.find((r) => r.id === override.target_id);
        return role?.name ?? 'Unknown Role';
      }
      const member = members.find((m) => m.user_id === override.target_id);
      return member?.display_name ?? member?.username ?? 'Unknown User';
    },
    [roles, members]
  );

  const handleSelectOverride = useCallback((override: ChannelOverride) => {
    setSelectedOverrideId(override.id);
    setEditAllow(parsePermissions(override.allow));
    setEditDeny(parsePermissions(override.deny));
  }, []);

  const handleSaveOverride = useCallback(
    async (override: ChannelOverride) => {
      const result = await onUpsert({
        target_type: override.target_type,
        target_id: override.target_id,
        allow: editAllow.toString(),
        deny: editDeny.toString(),
      });
      if (result !== false) {
        setSelectedOverrideId(null);
      }
    },
    [editAllow, editDeny, onUpsert]
  );

  const handleDeleteOverride = useCallback(
    async (overrideId: string) => {
      const result = await onDelete(overrideId);
      if (result !== false && selectedOverrideId === overrideId) {
        setSelectedOverrideId(null);
      }
    },
    [onDelete, selectedOverrideId]
  );

  const handleAddOverride = useCallback(async () => {
    if (!addTargetId) return;
    const result = await onUpsert({
      target_type: addTargetType,
      target_id: addTargetId,
      allow: addAllow.toString(),
      deny: addDeny.toString(),
    });
    if (result !== false) {
      setAddTargetId('');
      setAddAllow(0n);
      setAddDeny(0n);
    }
  }, [addTargetType, addTargetId, addAllow, addDeny, onUpsert]);

  const selectedOverride = useMemo(
    () => overrides.find((o) => o.id === selectedOverrideId) ?? null,
    [overrides, selectedOverrideId]
  );

  const renderOverrideItem = (override: ChannelOverride) => {
    const isSelected = selectedOverrideId === override.id;
    const allowCount = countBits(override.allow);
    const denyCount = countBits(override.deny);

    return (
      <div key={override.id} className={`override-item${isSelected ? ' selected' : ''}`}>
        <button
          type="button"
          className="override-item-select"
          onClick={() => handleSelectOverride(override)}
        >
          <div className="override-target">
            <div>
              <div className="override-target-name">{getTargetName(override)}</div>
              <div className="override-target-type">{override.target_type}</div>
            </div>
          </div>
          <div className="override-summary">
            {allowCount > 0 && <span className="override-allow-count">{allowCount} allowed</span>}
            {denyCount > 0 && <span className="override-deny-count">{denyCount} denied</span>}
          </div>
        </button>
        <button
          type="button"
          className="override-delete-btn"
          onClick={() => handleDeleteOverride(override.id)}
          aria-label="Delete override"
        >
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M2 4h12M5.33 4V2.67a1.33 1.33 0 011.34-1.34h2.66a1.33 1.33 0 011.34 1.34V4M12.67 4v9.33a1.33 1.33 0 01-1.34 1.34H4.67a1.33 1.33 0 01-1.34-1.34V4"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>
      </div>
    );
  };

  return (
    <>
      {/* Override List */}
      {roleOverrides.length > 0 && (
        <>
          <div className="section-header">Role Overrides</div>
          <div className="override-list">{roleOverrides.map(renderOverrideItem)}</div>
        </>
      )}

      {userOverrides.length > 0 && (
        <>
          <div className="section-header">User Overrides</div>
          <div className="override-list">{userOverrides.map(renderOverrideItem)}</div>
        </>
      )}

      {overrides.length === 0 && <div className="no-overrides">{emptyMessage}</div>}

      {/* Edit Selected Override */}
      {selectedOverride && !disabled && (
        <>
          <div className="section-header">Editing: {getTargetName(selectedOverride)}</div>
          <PermissionGrid
            value={editAllow}
            onChange={setEditAllow}
            deny={editDeny}
            onDenyChange={setEditDeny}
            mode="override"
            disabled={disabled}
          />
          <div style={{ marginTop: 12, display: 'flex', gap: 8 }}>
            <button
              className="add-override-btn"
              onClick={() => handleSaveOverride(selectedOverride)}
            >
              Save Override
            </button>
            <button
              className="add-override-btn"
              style={{ opacity: 0.7 }}
              onClick={() => setSelectedOverrideId(null)}
            >
              Cancel
            </button>
          </div>
        </>
      )}

      {/* Add Override Section */}
      {!disabled && !selectedOverride && (
        <div className="add-override-section">
          <div className="section-header">Add Override</div>
          <div className="add-override-row">
            <select
              className="add-override-select"
              aria-label="Override target type"
              value={addTargetType}
              onChange={(e) => {
                setAddTargetType(e.target.value as 'role' | 'user');
                setAddTargetId('');
              }}
            >
              <option value="role">Role</option>
              <option value="user">User</option>
            </select>
            <select
              className="add-override-select"
              aria-label="Override target"
              value={addTargetId}
              onChange={(e) => setAddTargetId(e.target.value)}
            >
              <option value="">Select {addTargetType === 'role' ? 'a role' : 'a user'}...</option>
              {addTargetType === 'role'
                ? roles.map((role) => (
                    <option key={role.id} value={role.id}>
                      {role.name}
                    </option>
                  ))
                : members.map((member) => (
                    <option key={member.user_id} value={member.user_id}>
                      {member.display_name ?? member.username}
                    </option>
                  ))}
            </select>
          </div>
          <PermissionGrid
            value={addAllow}
            onChange={setAddAllow}
            deny={addDeny}
            onDenyChange={setAddDeny}
            mode="override"
          />
          <button className="add-override-btn" disabled={!addTargetId} onClick={handleAddOverride}>
            Add Override
          </button>
        </div>
      )}
    </>
  );
};

export default OverridePanel;
