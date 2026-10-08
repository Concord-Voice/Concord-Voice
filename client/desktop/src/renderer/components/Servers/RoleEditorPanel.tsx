import React, { useState, useEffect, useRef, useCallback } from 'react';
import PermissionGrid from '../Permissions/PermissionGrid';
import ToggleSwitch from '../Settings/ToggleSwitch';
import ErrorBanner from '../Settings/ErrorBanner';
import EmojiPicker from '../EmojiPicker/LazyEmojiPicker';
import LoadingSpinner from '../Auth/LoadingSpinner';
import DangerousActionStepUpDialog from '../Auth/DangerousActionStepUpDialog';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import RoleHierarchyList from './RoleHierarchyList';
import RoleDeleteFlow, { roleListFocusTarget, type RoleDeleteTarget } from './RoleDeleteFlow';
import { useDiscardPrompt } from './useDiscardPrompt';
import { describeFailureWith } from '../../services/system/dangerousActionRequest';
import {
  failureTextOf,
  sendResultOf,
  stepUpSeedOf,
  type PermissionWriteStepUpSeed,
} from '../../services/system/permissionWriteStepUp';
import {
  WRITE_UNKNOWN,
  type PermissionWriteOutcome,
  type RoleCreateOutcome,
  type UpdateRoleRequest,
  type CreateRoleRequest,
  type WriteConfirmation,
} from '../../stores/chat/permissionStore';
import { parsePermissions } from '../../utils/policy/permissions';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
import type { Role } from '../../types/server';

interface RoleEditorPanelProps {
  serverId: string;
  roles: Role[];
  /**
   * Creates `request`, which the panel builds once when the create starts.
   * `confirmation` is set only on the step-up dialog's re-send, which carries
   * the same request; the first send has none and is the request as it always was.
   */
  onCreateRole: (
    request: CreateRoleRequest,
    confirmation?: WriteConfirmation
  ) => Promise<RoleCreateOutcome>;
  /** Saves every field the form holds. `confirmation` as for `onCreateRole`. */
  onSaveRole: (
    roleId: string,
    data: Required<UpdateRoleRequest>,
    confirmation?: WriteConfirmation
  ) => Promise<PermissionWriteOutcome>;
  /** `confirmation` as for `onCreateRole`. */
  onDeleteRole: (
    roleId: string,
    confirmation?: WriteConfirmation
  ) => Promise<PermissionWriteOutcome>;
  /**
   * Whether the page around the panel holds edits of its own (Server Settings'
   * General form, which outlives a switch to Roles). Setting up verification
   * leaves the page, so the panel's one discard question covers those too.
   */
  pageIsDirty?: () => boolean;
}

type RoleWritePurpose = Extract<StepUpPurpose, 'roles.create' | 'roles.update'>;

/**
 * A new role: the first default name no role holds yet, read when the create
 * starts. The verified re-send reuses this request, so a role another
 * administrator creates meanwhile cannot rename what was verified (#3456 review).
 */
function newRoleRequest(roles: readonly Role[]): CreateRoleRequest {
  const existingNames = new Set(roles.map((r) => r.name));
  let name = 'New Role';
  let counter = 2;
  while (existingNames.has(name)) {
    name = `New Role ${counter++}`;
  }
  return { name, color: '#99aab5', permissions: '0' };
}

/** One role write: sent once as it always was, and re-sent unchanged if the server asks to verify. */
interface RoleWrite {
  purpose: RoleWritePurpose;
  /** `confirmation` is set only on the dialog's re-send; the request is frozen by the caller. */
  send: (confirmation?: WriteConfirmation) => Promise<PermissionWriteOutcome>;
}

/**
 * The write that the server refused for verification, that refusal, and the
 * account and server the write went out as: the dialog's capture.
 */
interface PendingWrite extends PermissionWriteStepUpSeed {
  write: RoleWrite;
}

const DEFAULT_ROLE_COLOR = '#99aab5';

const WRITE_COPY: Record<
  RoleWritePurpose,
  {
    failure: string;
    intro: string;
    primaryLabel: string;
    busyLabel: string;
    describeFailure: (status: number, body: unknown) => string;
  }
> = {
  'roles.create': {
    failure: 'Failed to create role',
    intro: 'This server asks you to verify before you create a role.',
    primaryLabel: 'Create Role',
    busyLabel: 'Creating...',
    describeFailure: describeFailureWith('Failed to create role'),
  },
  'roles.update': {
    failure: 'Failed to save role',
    intro: 'This server asks you to verify before you save changes to this role.',
    primaryLabel: 'Save Role',
    busyLabel: 'Saving...',
    describeFailure: describeFailureWith('Failed to save role'),
  },
};

const RoleEditorPanel: React.FC<RoleEditorPanelProps> = ({
  serverId,
  roles,
  onCreateRole,
  onSaveRole,
  onDeleteRole,
  pageIsDirty,
}) => {
  const [selectedRoleId, setSelectedRoleId] = useState<string | null>(null);
  const [editRoleName, setEditRoleName] = useState('');
  const [editRoleColor, setEditRoleColor] = useState(DEFAULT_ROLE_COLOR);
  const [editRoleEmoji, setEditRoleEmoji] = useState('');
  const [showRoleEmojiPicker, setShowRoleEmojiPicker] = useState(false);
  const [editRoleDisplaySeparately, setEditRoleDisplaySeparately] = useState(false);
  const [editRoleMentionable, setEditRoleMentionable] = useState(false);
  const [editRolePermissions, setEditRolePermissions] = useState<bigint>(0n);
  const [inFlight, setInFlight] = useState<RoleWritePurpose | null>(null);
  const [writeError, setWriteError] = useState<string | null>(null);
  const [pending, setPending] = useState<PendingWrite | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<RoleDeleteTarget | null>(null);
  const roleEmojiPickerRef = useRef<HTMLDivElement>(null);

  const selectedRole = roles.find((r) => r.id === selectedRoleId) || null;
  const isRoleSaving = inFlight === 'roles.update';

  useEffect(() => {
    if (selectedRole) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleName from selectedRole when the selected role changes; not a render loop
      setEditRoleName(selectedRole.name);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets editRoleColor from selectedRole when the selected role changes; not a render loop
      setEditRoleColor(selectedRole.color || DEFAULT_ROLE_COLOR);
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

  const returnTo = { kind: 'serverSettings', serverId, section: 'roles' } as const;
  // Edits the form holds that the role does not. Leaving Server Settings to set
  // up verification, from a refused save or a refused delete, would lose them
  // (D-4), and the page's own edits with them; with nothing selected, or
  // nothing changed, there is nothing to lose.
  const formIsDirty = () =>
    selectedRole !== null &&
    (editRoleName !== selectedRole.name ||
      editRoleColor !== (selectedRole.color || DEFAULT_ROLE_COLOR) ||
      editRoleEmoji !== (selectedRole.emoji || '') ||
      editRoleDisplaySeparately !== selectedRole.display_separately ||
      editRoleMentionable !== selectedRole.mentionable ||
      editRolePermissions !== parsePermissions(selectedRole.permissions));
  const { confirmDiscard, prompt: discardPrompt } = useDiscardPrompt(
    () => formIsDirty() || pageIsDirty?.() === true
  );

  // One write at a time: from the first send until the dialog it may open has
  // closed. A second would replace the first's refusal, and the dialog under
  // the user would swap its purpose and intro mid-verification.
  const writeBusy = inFlight !== null || pending !== null;

  // The first send is the request as it always was. A refusal that asks for
  // verification hands the same write to the dialog; anything else is worded here.
  const startWrite = async (write: RoleWrite) => {
    if (writeBusy) return;
    setWriteError(null);
    setInFlight(write.purpose);
    try {
      const outcome = await write.send().catch(() => WRITE_UNKNOWN);
      if (outcome.ok) return;
      const seed = stepUpSeedOf(outcome);
      if (seed === null) {
        setWriteError(failureTextOf(outcome, WRITE_COPY[write.purpose].failure));
        return;
      }
      setPending({ write, ...seed });
    } finally {
      setInFlight(null);
    }
  };

  const resend = async (mfaCode: string | undefined, context: WriteConfirmation['context']) =>
    pending === null
      ? ({ kind: 'aborted' } as const)
      : sendResultOf(await pending.write.send({ mfaCode, context }));

  const handleSaveRole = async () => {
    if (!selectedRoleId) return;
    const roleId = selectedRoleId;
    // Frozen here: the dialog re-sends exactly this and never re-reads the form.
    const data: Required<UpdateRoleRequest> = {
      name: editRoleName,
      color: editRoleColor,
      emoji: editRoleEmoji || '',
      permissions: editRolePermissions.toString(),
      display_separately: editRoleDisplaySeparately,
      mentionable: editRoleMentionable,
    };
    await startWrite({
      purpose: 'roles.update',
      send: (confirmation) =>
        confirmation ? onSaveRole(roleId, data, confirmation) : onSaveRole(roleId, data),
    });
  };

  // Selecting the freshly created role is the only reason this is not simply
  // `onCreateRole`: the rail renders the button, the panel owns the selection.
  const handleCreateRole = () => {
    const request = newRoleRequest(roles);
    void startWrite({
      purpose: 'roles.create',
      send: async (confirmation) => {
        const outcome = await (confirmation
          ? onCreateRole(request, confirmation)
          : onCreateRole(request));
        if (outcome.ok) setSelectedRoleId(outcome.role.id);
        return outcome;
      },
    });
  };

  const handleSelectRole = (roleId: string) => {
    setWriteError(null);
    setSelectedRoleId(roleId);
  };

  const endWrite = () => setPending(null);
  const endDelete = useCallback(() => setDeleteTarget(null), []);
  const clearSelection = () => setSelectedRoleId(null);

  const errorBanner = writeError === null ? null : <ErrorBanner error={writeError} />;
  // Closed, the dialog reads none of this; the update copy only fills its required props.
  const stepUpPurpose = pending?.write.purpose ?? 'roles.update';
  const copy = WRITE_COPY[stepUpPurpose];

  return (
    <>
      <div className="roles-layout">
        <RoleHierarchyList
          serverId={serverId}
          roles={roles}
          selectedRoleId={selectedRoleId}
          onSelectRole={handleSelectRole}
          onCreateRole={handleCreateRole}
          createDisabled={writeBusy}
        />

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
                    Members with the Mention Roles permission can @mention this role to notify all
                    who hold it.
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
                <PermissionGrid
                  value={editRolePermissions}
                  onChange={handlePermChange}
                  mode="role"
                />
              </div>

              {errorBanner}

              <div className="role-editor-actions">
                {!selectedRole.is_default && (
                  <button
                    type="button"
                    className="server-settings-cancel-btn"
                    onClick={() =>
                      setDeleteTarget({ id: selectedRole.id, name: selectedRole.name })
                    }
                    disabled={isRoleSaving}
                  >
                    Delete
                  </button>
                )}
                <button
                  type="button"
                  className="server-settings-submit-btn"
                  onClick={handleSaveRole}
                  disabled={inFlight !== null}
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
            <>
              {errorBanner}
              <div style={{ color: 'var(--text-secondary)', padding: '40px', textAlign: 'center' }}>
                Select a role to edit, or create a new one.
              </div>
            </>
          )}
        </div>
      </div>
      <RoleDeleteFlow
        target={deleteTarget}
        onDelete={onDeleteRole}
        onDeleted={clearSelection}
        onEnd={endDelete}
        returnTo={returnTo}
        confirmDiscard={confirmDiscard}
      />
      <DangerousActionStepUpDialog
        isOpen={pending !== null}
        purpose={stepUpPurpose}
        seed={pending?.refusal}
        capture={pending?.context}
        intro={copy.intro}
        primaryLabel={copy.primaryLabel}
        busyLabel={copy.busyLabel}
        send={resend}
        describeFailure={copy.describeFailure}
        onSuccess={endWrite}
        onClose={endWrite}
        onSetUpVerification={() => {
          void openVerificationSetup({ returnTo, confirmDiscard, closeHost: endWrite });
        }}
        focusFallback={roleListFocusTarget}
      />
      {discardPrompt}
    </>
  );
};

export default RoleEditorPanel;
