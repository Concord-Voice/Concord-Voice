import React, { useState, useMemo, useCallback, useRef, useEffect } from 'react';
import { AlertCircle } from 'lucide-react';
import PermissionGrid from './PermissionGrid';
import {
  ChannelOverride,
  UpsertOverrideRequest,
  NO_PERMISSION_WRITES,
} from '../../stores/chat/permissionStore';
import { Role } from '../../types/server';
import { ServerMember } from '../../stores/chat/memberStore';
import { parsePermissions, parseExactPermissions, countBits } from '../../utils/policy/permissions';
import './OverridePanel.css';

interface OverridePanelProps {
  overrides: ChannelOverride[];
  roles: Role[];
  members: ServerMember[];
  onUpsert: (data: UpsertOverrideRequest) => Promise<boolean>;
  onDelete: (overrideId: string) => Promise<boolean>;
  disabled?: boolean;
  emptyMessage?: string;
  /** A write the parent owns is in flight (the channel modal's category
   *  sync): every override write stays locked until it settles (#3406). */
  locked?: boolean;
  /** Told whenever this panel starts or settles its last write, so a parent
   *  control that writes too (or hides this panel) can lock itself (#3406). */
  onWritePendingChange?: (pending: boolean) => void;
  /** Ids of this scope's permission writes whose request has not settled, from
   *  the permission store. Those already in flight when the panel mounts were
   *  started by an earlier instance whose modal was closed (#3406 review,
   *  round 6). */
  writesInFlight?: readonly number[];
}

/** Which write attempt a rejected promise belongs to — decides which of the
 * three alert slots renders (D5). Edit and add share copy but not placement. */
type WriteErrorSite = 'edit' | 'add' | 'delete';

const SAVE_ERROR_MESSAGE =
  'Failed to save permission override. Your changes are still here — try again.';
const DELETE_ERROR_MESSAGE = 'Failed to remove permission override. Try again.';
// A refused Save whose row has left the list: there is no editor holding the
// changes and no row to retry, so the edit slot's "still here" would be false.
const VANISHED_SAVE_ERROR_MESSAGE =
  'Failed to save permission override. It is no longer in this list.';

// Shown in place of the grid for a mask the desktop cannot read exactly: an
// older control plane sends masks as JSON numbers, and one above 2^53 may have
// lost its low bits (#3406 review, round seven).
const UNREADABLE_MASK_MESSAGE =
  "This override's permissions can't be shown exactly until the server is updated, so it can't be edited here. You can still delete it.";

/** Whether both of an override's masks decode exactly, so it is safe to edit. */
function isReadableOverride(override: ChannelOverride): boolean {
  return (
    parseExactPermissions(override.allow) !== null && parseExactPermissions(override.deny) !== null
  );
}

/** Inline `role="alert"` for one of the three write-failure slots (D5). */
const WriteErrorAlert: React.FC<{ message: string }> = ({ message }) => (
  <div className="override-error" role="alert">
    <AlertCircle size={16} aria-hidden="true" />
    <span>{message}</span>
  </div>
);

const OverridePanel: React.FC<OverridePanelProps> = ({
  overrides,
  roles,
  members,
  onUpsert,
  onDelete,
  disabled = false,
  emptyMessage = 'No permission overrides configured.',
  locked = false,
  onWritePendingChange,
  writesInFlight = NO_PERMISSION_WRITES,
}) => {
  const [selectedOverrideId, setSelectedOverrideId] = useState<string | null>(null);
  const [editAllow, setEditAllow] = useState<bigint>(0n);
  const [editDeny, setEditDeny] = useState<bigint>(0n);
  // The override the draft was loaded from. A mask that could not be read loads
  // as 0n, so a draft from an unreadable override must never be saved, even
  // after a refresh makes that override readable (#3406 review, round 8).
  const [draftSource, setDraftSource] = useState<ChannelOverride | null>(null);

  // Add override form state
  const [addTargetType, setAddTargetType] = useState<'role' | 'user'>('role');
  const [addTargetId, setAddTargetId] = useState('');
  const [addAllow, setAddAllow] = useState<bigint>(0n);
  const [addDeny, setAddDeny] = useState<bigint>(0n);

  // Which write last failed, if any (D5). Cleared at the start of every write
  // attempt, on Cancel, on selecting an override, on any grid edit, and on any
  // add-target change — never left to go stale across an unrelated action.
  const [writeError, setWriteError] = useState<{ site: WriteErrorSite } | null>(null);

  // Busy state per write button (#3406 finding 2): disables the button that
  // triggered a write while that write is in flight, so a double-click can't
  // send two. Delete is tracked per-row since more than one row's delete can
  // be in flight at once; Save and Add each have exactly one live instance.
  const [isSavingOverride, setIsSavingOverride] = useState(false);
  // Saves whose request has not settled, including one a Cancel abandoned
  // (#3406 review, round 5). Cancel closes the editor and clears
  // isSavingOverride, but it cannot recall the PUT, which may still commit;
  // only settlement lowers this count, so the write lock holds until then.
  const [savesInFlight, setSavesInFlight] = useState(0);
  const [isAddingOverride, setIsAddingOverride] = useState(false);
  const [deletingOverrideIds, setDeletingOverrideIds] = useState<Set<string>>(() => new Set());
  // One write at a time (#3406 review): while a Save or Add is in flight, the
  // surfaces it would read or hide are read-only, so an edit cannot be made
  // and then discarded by that write's success, and a failure cannot land in
  // a slot the user has navigated away from. Cancel stays live and closes the
  // editor, so a hung request never traps it, but the lock itself holds until
  // every Save settles (savesInFlight): a cancelled PUT can still commit, and
  // a Delete, re-save or sync started meanwhile would race it. Any two writes that can
  // touch the same override also exclude each other: a pending Save or Add
  // disables every Delete, and a pending Delete disables Save, Add and
  // selection. Deletes of different rows may still overlap. `locked` extends
  // the same exclusion to a write the parent owns.
  //
  // Closing the modal unmounts this panel and loses all of the above, while the
  // request can still commit (#3406 review, round 6). So the writes already in
  // flight when this instance mounted are inherited from the one that closed.
  // Their kind is not known here, so each counts as an upsert in flight until
  // it settles, which is the widest lock: every Delete, selection and the add
  // form. Save needs a selection, so it cannot start either.
  const [inheritedWrites] = useState(() => new Set(writesInFlight));
  const isInheritedWritePending = writesInFlight.some((id) => inheritedWrites.has(id));
  const isUpsertPending = savesInFlight > 0 || isAddingOverride || isInheritedWritePending;
  const isDeletePending = deletingOverrideIds.size > 0;
  const isWritePending = isUpsertPending || isDeletePending;

  useEffect(() => {
    onWritePendingChange?.(isWritePending);
  }, [isWritePending, onWritePendingChange]);

  // Currency tokens for the stale-write race (#3406 finding 1): a write's
  // result is applied only if nothing newer has superseded it by the time
  // its promise settles. `operationRef` covers Save/Delete/Cancel/Select —
  // any of which can supersede a Save or Delete in flight. Add gets its own
  // counter plus a live snapshot of its own fields, since only a *new* add
  // attempt or a change to the fields it was about to write should drop it.
  const operationRef = useRef(0);
  const addOperationRef = useRef(0);
  const mountedRef = useRef(true);
  const selectedOverrideIdRef = useRef(selectedOverrideId);
  selectedOverrideIdRef.current = selectedOverrideId;
  const addFieldsRef = useRef({ addTargetType, addTargetId, addAllow, addDeny });
  addFieldsRef.current = { addTargetType, addTargetId, addAllow, addDeny };

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      operationRef.current += 1;
      addOperationRef.current += 1;
    };
  }, []);

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
    // A newer operation (#3406): supersedes any Save/Delete already in flight.
    operationRef.current += 1;
    setSelectedOverrideId(override.id);
    setDraftSource(override);
    setEditAllow(parsePermissions(override.allow));
    setEditDeny(parsePermissions(override.deny));
    setWriteError(null);
  }, []);

  const handleCancelEdit = useCallback(() => {
    operationRef.current += 1;
    setIsSavingOverride(false);
    setSelectedOverrideId(null);
    setWriteError(null);
  }, []);

  const handleEditAllowChange = useCallback((v: bigint) => {
    setEditAllow(v);
    setWriteError(null);
  }, []);

  const handleEditDenyChange = useCallback((v: bigint) => {
    setEditDeny(v);
    setWriteError(null);
  }, []);

  const handleAddAllowChange = useCallback((v: bigint) => {
    setAddAllow(v);
    setWriteError(null);
  }, []);

  const handleAddDenyChange = useCallback((v: bigint) => {
    setAddDeny(v);
    setWriteError(null);
  }, []);

  const handleSaveOverride = useCallback(
    async (override: ChannelOverride) => {
      const operation = ++operationRef.current;
      setWriteError(null);
      setIsSavingOverride(true);
      setSavesInFlight((n) => n + 1);
      // Fail closed (D4): the editor closes only on a write the caller confirmed.
      // A refusal, or a caller that rejects instead of settling false, keeps it
      // open with the alert.
      const ok = await onUpsert({
        target_type: override.target_type,
        target_id: override.target_id,
        allow: editAllow.toString(),
        deny: editDeny.toString(),
      }).catch(() => false);
      // Settled, current or not: this request can no longer commit, so it
      // stops holding the write lock.
      if (mountedRef.current) setSavesInFlight((n) => n - 1);
      // Stale write (#3406): a Cancel, a re-select, or another write started
      // while this one was in flight — drop the result silently. Only the
      // current save owns the busy flag: a Cancel already released it, and a
      // newer save may hold it now.
      if (!mountedRef.current || operation !== operationRef.current) return;
      setIsSavingOverride(false);
      if (selectedOverrideIdRef.current !== override.id) return;
      if (!ok) {
        setWriteError({ site: 'edit' });
        return;
      }
      setSelectedOverrideId(null);
    },
    [editAllow, editDeny, onUpsert]
  );

  const handleDeleteOverride = useCallback(
    async (overrideId: string) => {
      setWriteError(null);
      setDeletingOverrideIds((prev) => new Set(prev).add(overrideId));
      const ok = await onDelete(overrideId).catch(() => false);
      if (mountedRef.current) {
        setDeletingOverrideIds((prev) => {
          if (!prev.has(overrideId)) return prev;
          const next = new Set(prev);
          next.delete(overrideId);
          return next;
        });
      }
      // A delete answers for its own row, which the list-level slot still shows,
      // so no other write supersedes it: deletes on different rows may overlap
      // (each button blocks only its own row), and a failure must still surface
      // after another row's delete started (#3406 review).
      if (!mountedRef.current) return;
      if (!ok) {
        setWriteError({ site: 'delete' });
        return;
      }
      if (selectedOverrideIdRef.current === overrideId) {
        setSelectedOverrideId(null);
      }
    },
    [onDelete]
  );

  const handleAddOverride = useCallback(async () => {
    if (!addTargetId) return;
    const operation = ++addOperationRef.current;
    const snapshot = { addTargetType, addTargetId, addAllow, addDeny };
    setWriteError(null);
    setIsAddingOverride(true);
    const ok = await onUpsert({
      target_type: addTargetType,
      target_id: addTargetId,
      allow: addAllow.toString(),
      deny: addDeny.toString(),
    }).catch(() => false);
    if (mountedRef.current) setIsAddingOverride(false);
    // Stale write (#3406): a newer add attempt, or a target/type/bits change
    // since this one started — drop the result silently.
    const stillCurrent =
      mountedRef.current &&
      operation === addOperationRef.current &&
      addFieldsRef.current.addTargetType === snapshot.addTargetType &&
      addFieldsRef.current.addTargetId === snapshot.addTargetId &&
      addFieldsRef.current.addAllow === snapshot.addAllow &&
      addFieldsRef.current.addDeny === snapshot.addDeny;
    if (!stillCurrent) return;
    if (!ok) {
      setWriteError({ site: 'add' });
      return;
    }
    setAddTargetId('');
    setAddAllow(0n);
    setAddDeny(0n);
  }, [addTargetType, addTargetId, addAllow, addDeny, onUpsert]);

  const selectedOverride = useMemo(
    () => overrides.find((o) => o.id === selectedOverrideId) ?? null,
    [overrides, selectedOverrideId]
  );

  // Editable only when both the override and the draft's source read exactly.
  // Until the effect below reloads a stale draft, the notice stays up and Save
  // stays disabled, so no render can offer the 0n draft for saving.
  const isSelectedEditable =
    selectedOverride !== null &&
    isReadableOverride(selectedOverride) &&
    draftSource !== null &&
    isReadableOverride(draftSource);

  // A refresh that makes the selected override readable reloads the draft from
  // it. A draft from a readable source is left alone, so the moderator's edits
  // survive an ordinary refresh.
  useEffect(() => {
    if (!selectedOverride || !draftSource || isReadableOverride(draftSource)) return;
    if (!isReadableOverride(selectedOverride)) return;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: reloads the draft once when a refresh makes the selected override readable; the reload makes draftSource readable, so it cannot loop
    setDraftSource(selectedOverride);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- same reload as above
    setEditAllow(parsePermissions(selectedOverride.allow));
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- same reload as above
    setEditDeny(parsePermissions(selectedOverride.deny));
  }, [selectedOverride, draftSource]);

  const renderOverrideItem = (override: ChannelOverride) => {
    const isSelected = selectedOverrideId === override.id;
    const readable = isReadableOverride(override);
    const allowCount = countBits(override.allow);
    const denyCount = countBits(override.deny);

    return (
      <div key={override.id} className={`override-item${isSelected ? ' selected' : ''}`}>
        <button
          type="button"
          className="override-item-select"
          onClick={() => handleSelectOverride(override)}
          disabled={locked || isWritePending}
        >
          <div className="override-target">
            <div>
              <div className="override-target-name">{getTargetName(override)}</div>
              <div className="override-target-type">{override.target_type}</div>
            </div>
          </div>
          <div className="override-summary">
            {/* Counting only the mask that did decode would present an unknown
                deny as none, so an unreadable override gets a marker instead. */}
            {readable ? (
              <>
                {allowCount > 0 && (
                  <span className="override-allow-count">{allowCount} allowed</span>
                )}
                {denyCount > 0 && <span className="override-deny-count">{denyCount} denied</span>}
              </>
            ) : (
              <span className="override-target-type">Can&apos;t be shown</span>
            )}
          </div>
        </button>
        {/* D6a: a disabled panel (e.g. a synced channel) renders no delete button at
            all — the editor and add form already hide under sync, but deletion used
            to stay live, so a synced channel's override could be deleted and drift
            silently. */}
        {!disabled && (
          <button
            type="button"
            className="override-delete-btn"
            onClick={() => handleDeleteOverride(override.id)}
            aria-label="Delete override"
            disabled={locked || isUpsertPending || deletingOverrideIds.has(override.id)}
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
        )}
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

      {/* Delete alert: after the override lists, so it stays visible whether or
          not the editor is open (D5 placement). */}
      {writeError?.site === 'delete' && <WriteErrorAlert message={DELETE_ERROR_MESSAGE} />}

      {/* A refused Save whose row has since left the list (another moderator
          deleted it, and the store refreshed) has no edit slot to land in, so
          its alert shows here rather than nowhere (#3406 review, round 5). */}
      {writeError?.site === 'edit' && !selectedOverride && !disabled && (
        <WriteErrorAlert message={VANISHED_SAVE_ERROR_MESSAGE} />
      )}

      {/* Edit Selected Override */}
      {selectedOverride && !disabled && (
        <>
          <div className="section-header">Editing: {getTargetName(selectedOverride)}</div>
          {/* A mask that cannot be read exactly is not editable: the grid would
              show it as empty, and a Save would erase every bit it held. */}
          {isSelectedEditable ? (
            <PermissionGrid
              value={editAllow}
              onChange={handleEditAllowChange}
              deny={editDeny}
              onDenyChange={handleEditDenyChange}
              mode="override"
              disabled={disabled || isSavingOverride}
            />
          ) : (
            <output className="override-error">
              <AlertCircle size={16} aria-hidden="true" />
              <span>{UNREADABLE_MASK_MESSAGE}</span>
            </output>
          )}
          {writeError?.site === 'edit' && <WriteErrorAlert message={SAVE_ERROR_MESSAGE} />}
          <div style={{ marginTop: 12, display: 'flex', gap: 8 }}>
            <button
              className="add-override-btn"
              onClick={() => handleSaveOverride(selectedOverride)}
              disabled={locked || isSavingOverride || isDeletePending || !isSelectedEditable}
            >
              Save Override
            </button>
            <button
              className="add-override-btn"
              style={{ opacity: 0.7 }}
              onClick={handleCancelEdit}
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
              disabled={isUpsertPending}
              onChange={(e) => {
                setAddTargetType(e.target.value as 'role' | 'user');
                setAddTargetId('');
                setWriteError(null);
              }}
            >
              <option value="role">Role</option>
              <option value="user">User</option>
            </select>
            <select
              className="add-override-select"
              aria-label="Override target"
              value={addTargetId}
              disabled={isUpsertPending}
              onChange={(e) => {
                setAddTargetId(e.target.value);
                setWriteError(null);
              }}
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
            onChange={handleAddAllowChange}
            deny={addDeny}
            onDenyChange={handleAddDenyChange}
            mode="override"
            disabled={isUpsertPending}
          />
          {writeError?.site === 'add' && <WriteErrorAlert message={SAVE_ERROR_MESSAGE} />}
          <button
            className="add-override-btn"
            disabled={!addTargetId || locked || isUpsertPending || isDeletePending}
            onClick={handleAddOverride}
          >
            Add Override
          </button>
        </div>
      )}
    </>
  );
};

export default OverridePanel;
