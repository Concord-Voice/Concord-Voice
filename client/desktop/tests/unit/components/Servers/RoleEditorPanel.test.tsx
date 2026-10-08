import { render, screen, fireEvent, act, waitFor, within, userEvent } from '../../../test-utils';
import RoleEditorPanel from '@/renderer/components/Servers/RoleEditorPanel';
import { vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { FIRST_SEND_SESSION_CHANGED } from '@/renderer/services/system/dangerousActionRequest';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import type {
  PermissionWriteFailure,
  PermissionWriteOutcome,
  RoleCreateOutcome,
} from '@/renderer/stores/chat/permissionStore';
import type { Role } from '@/renderer/types/server';
import { server as mswServer } from '../../../mocks/server';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  MFA_REQUIRED,
  SETUP_LINK,
  stubStepUpRead,
  type GatedReply,
} from '../../../helpers/gatedRoute';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

vi.mock('@/renderer/components/Permissions/PermissionGrid', () => ({
  default: ({
    value,
    onChange,
    mode,
  }: {
    value: bigint;
    onChange: (v: bigint) => void;
    mode: string;
  }) => (
    <div data-testid="permission-grid" data-mode={mode}>
      <button data-testid="set-perm" onClick={() => onChange(value | 1n)}>
        Set Perm
      </button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: ({ size, inline }: { size?: string; inline?: boolean }) => (
    <div data-testid="loading-spinner" data-size={size} data-inline={String(inline)} />
  ),
}));

vi.mock('emoji-picker-react', () => ({
  default: () => <div data-testid="emoji-picker" />,
}));

vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: ({ onSelect, onClose }: { onSelect: (emoji: string) => void; onClose: () => void }) => (
    <div data-testid="lazy-emoji-picker">
      <button data-testid="pick-emoji" onClick={() => onSelect('\u{1F389}')}>
        Pick
      </button>
      <button data-testid="close-emoji" onClick={onClose}>
        Close
      </button>
    </div>
  ),
}));

const mockRole: Role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  color: '#ff0000',
  position: 1,
  permissions: '3',
  is_default: false,
  is_managed: false,
  display_separately: false,
  mentionable: false,
  emoji: '',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const defaultRole: Role = {
  ...mockRole,
  id: 'role-default',
  name: '@everyone',
  position: 0,
  is_default: true,
  is_managed: true,
  color: '#99aab5',
};

const mockRole2: Role = { ...mockRole, id: 'role-2', name: 'Admin', position: 2 };

const OK: PermissionWriteOutcome = { ok: true };
// Captured when called, as the store captures when it sends: inside a test,
// after `beforeEach` has signed in, so the refusal belongs to that session.
type RefusedWrite = Extract<PermissionWriteFailure, { kind: 'refused' }>;

const refusedWith = ({ status, body }: GatedReply): RefusedWrite => ({
  ok: false,
  kind: 'refused',
  status,
  body,
  context: captureApiRequestContext(),
});
const mfaRefused = () => refusedWith(MFA_REQUIRED);

const createdRole: Role = { ...mockRole, id: 'role-new', name: 'New Role', position: 3 };

// Fresh per test: every callback resolves the success the store would return.
// `vi.fn<T>()` types the mock from the prop's own signature, so a drifted
// contract fails to compile instead of resolving the wrong shape.
const makeProps = () => ({
  serverId: 'server-1',
  roles: [mockRole, defaultRole, mockRole2],
  onCreateRole: vi.fn<(...args: unknown[]) => Promise<RoleCreateOutcome>>().mockResolvedValue({
    ok: true,
    role: createdRole,
  }),
  onSaveRole: vi
    .fn<(...args: unknown[]) => Promise<PermissionWriteOutcome>>()
    .mockResolvedValue(OK),
  onDeleteRole: vi
    .fn<(...args: unknown[]) => Promise<PermissionWriteOutcome>>()
    .mockResolvedValue(OK),
});

let defaultProps: ReturnType<typeof makeProps>;

const EMPTY_STATE = 'Select a role to edit, or create a new one.';
const stepUpDialog = () => screen.getByRole('dialog', { name: DIALOG_TITLE });

/** Types a TOTP code into the open step-up dialog and presses its primary button. */
async function verifyInDialog(primaryLabel: string) {
  await userEvent.type(await screen.findByLabelText(CODE_LABEL), FIXTURE_OTP);
  await userEvent.click(within(stepUpDialog()).getByRole('button', { name: primaryLabel }));
}

/** The confirmation's body, which no other dialog in the flow carries. */
const CONFIRMATION_TEXT = 'Are you sure you want to delete the role';

const selectModerator = () => fireEvent.click(screen.getByText('Moderator').closest('button')!);

/** The role-list rail holds focus, or a control in it. */
const focusIsInRoleList = () =>
  document.querySelector('.role-hierarchy')!.contains(document.activeElement);

describe('RoleEditorPanel', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    stubStepUpRead();
    defaultProps = makeProps();
  });

  it('renders empty state message when no role is selected', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    expect(screen.getByText(EMPTY_STATE)).toBeInTheDocument();
  });

  it('shows role list sorted by position (higher first)', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    const roleButtons = screen.getAllByRole('button').filter((btn) => {
      const text = btn.textContent || '';
      return text === 'Admin' || text === 'Moderator' || text === '@everyone';
    });
    expect(roleButtons[0]).toHaveTextContent('Admin');
    expect(roleButtons[1]).toHaveTextContent('Moderator');
    expect(roleButtons[2]).toHaveTextContent('@everyone');
  });

  it('shows "Create Role" button that calls onCreateRole', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    const createBtn = screen.getByText('+ Create Role');
    await act(async () => {
      fireEvent.click(createBtn);
    });
    expect(defaultProps.onCreateRole).toHaveBeenCalledTimes(1);
  });

  it('selecting a role shows editor with name, color, permissions', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    const roleBtn = screen.getByText('Moderator').closest('button')!;
    fireEvent.click(roleBtn);

    expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');
    expect(screen.getByTestId('permission-grid')).toBeInTheDocument();
  });

  it('editing role name updates the input', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    const nameInput = screen.getByLabelText('Role Name');
    fireEvent.change(nameInput, { target: { value: 'Super Mod' } });
    expect(nameInput).toHaveValue('Super Mod');
  });

  it('shows PermissionGrid with mode="role"', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    const grid = screen.getByTestId('permission-grid');
    expect(grid).toHaveAttribute('data-mode', 'role');
  });

  it('save button calls onSaveRole with correct data', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    expect(defaultProps.onSaveRole).toHaveBeenCalledWith('role-1', {
      name: 'Moderator',
      color: '#ff0000',
      emoji: '',
      permissions: '3',
      display_separately: false,
      mentionable: false,
    });
  });

  it('shows loading spinner while saving', async () => {
    let resolveOnSave: () => void;
    const slowSave = vi.fn(
      () =>
        new Promise<PermissionWriteOutcome>((resolve) => {
          resolveOnSave = () => resolve(OK);
        })
    );
    render(<RoleEditorPanel {...defaultProps} onSaveRole={slowSave} />);
    selectModerator();

    // Start save but don't await
    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    // While saving, spinner should show
    expect(screen.getByTestId('loading-spinner')).toBeInTheDocument();
    expect(screen.getByText('Saving...')).toBeInTheDocument();

    // Resolve the save
    await act(async () => {
      resolveOnSave!();
    });

    expect(screen.queryByTestId('loading-spinner')).not.toBeInTheDocument();
    expect(screen.getByText('Save Role')).toBeInTheDocument();
  });

  it('default role shows note and no Delete button', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('@everyone').closest('button')!);

    expect(
      screen.getByText('This is the default role assigned to all members.')
    ).toBeInTheDocument();
    expect(screen.queryByText('Delete')).not.toBeInTheDocument();
  });

  it('shows ToggleSwitch for "Display Separately" and "Mentionable"', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    expect(screen.getByText('Display Separately')).toBeInTheDocument();
    expect(screen.getByText('Mentionable')).toBeInTheDocument();
    // Both are checkboxes rendered by ToggleSwitch
    const toggles = screen.getAllByRole('checkbox');
    expect(toggles.length).toBeGreaterThanOrEqual(2);
  });

  it('shows emoji picker section', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    expect(screen.getByText('Role Emoji (Optional)')).toBeInTheDocument();
    expect(screen.getByTitle('Pick an emoji')).toBeInTheDocument();
  });

  it('opens and uses emoji picker to select emoji', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    // Open the emoji picker
    fireEvent.click(screen.getByTitle('Pick an emoji'));
    expect(screen.getByTestId('lazy-emoji-picker')).toBeInTheDocument();

    // Pick an emoji
    fireEvent.click(screen.getByTestId('pick-emoji'));
    // The picker should close and emoji should appear
    expect(screen.queryByTestId('lazy-emoji-picker')).not.toBeInTheDocument();
  });

  it('closes emoji picker via close button', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    fireEvent.click(screen.getByTitle('Pick an emoji'));
    expect(screen.getByTestId('lazy-emoji-picker')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('close-emoji'));
    expect(screen.queryByTestId('lazy-emoji-picker')).not.toBeInTheDocument();
  });

  it('clears emoji when remove button is clicked', async () => {
    const roleWithEmoji: Role = { ...mockRole, emoji: '\u{1F525}' };
    render(<RoleEditorPanel {...defaultProps} roles={[roleWithEmoji, defaultRole]} />);
    selectModerator();

    // The remove emoji button should be visible
    const removeBtn = screen.getByTitle('Remove emoji');
    fireEvent.click(removeBtn);

    // After clearing, the "Pick an emoji" placeholder should show
    expect(screen.getByText('Pick an emoji')).toBeInTheDocument();
  });

  it('role color dot and colored name in list', () => {
    render(<RoleEditorPanel {...defaultProps} />);

    // Check that the role color dot exists with correct background color
    const roleDots = document.querySelectorAll('.role-color-dot');
    expect(roleDots.length).toBe(3);

    // Check colored name in the list
    const modName = screen.getByText('Moderator');
    expect(modName).toHaveStyle({ color: '#ff0000' });
  });

  it('changing permission via PermissionGrid updates save payload', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    // Click the set-perm button in the mocked PermissionGrid
    fireEvent.click(screen.getByTestId('set-perm'));

    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    // permissions '3' parsed as 3n, OR'd with 1n = still 3n
    expect(defaultProps.onSaveRole).toHaveBeenCalledWith(
      'role-1',
      expect.objectContaining({
        permissions: '3',
      })
    );
  });

  it('toggling Display Separately updates save payload', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    // Find the Display Separately toggle (first checkbox after the role editor loads)
    const toggles = screen.getAllByRole('checkbox');
    // Display Separately is the first toggle
    const displayToggle = toggles[0];
    fireEvent.click(displayToggle);

    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    expect(defaultProps.onSaveRole).toHaveBeenCalledWith(
      'role-1',
      expect.objectContaining({ display_separately: true })
    );
  });

  it('toggling Mentionable updates save payload', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    const toggles = screen.getAllByRole('checkbox');
    // Mentionable is the second toggle
    const mentionableToggle = toggles[1];
    fireEvent.click(mentionableToggle);

    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    expect(defaultProps.onSaveRole).toHaveBeenCalledWith(
      'role-1',
      expect.objectContaining({ mentionable: true })
    );
  });

  it('disables inputs while saving', async () => {
    let resolveOnSave: () => void;
    const slowSave = vi.fn(
      () =>
        new Promise<PermissionWriteOutcome>((resolve) => {
          resolveOnSave = () => resolve(OK);
        })
    );
    render(<RoleEditorPanel {...defaultProps} onSaveRole={slowSave} />);
    selectModerator();

    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    expect(screen.getByLabelText('Role Name')).toBeDisabled();

    await act(async () => {
      resolveOnSave!();
    });
  });

  it('re-enables save button after onSaveRole completes (try/finally)', async () => {
    // The component's handleSaveRole wraps onSaveRole in try/finally,
    // ensuring isRoleSaving resets to false regardless of outcome.
    let resolveSave!: () => void;
    const slowSave = vi.fn(
      () =>
        new Promise<PermissionWriteOutcome>((resolve) => {
          resolveSave = () => resolve(OK);
        })
    );
    render(<RoleEditorPanel {...defaultProps} onSaveRole={slowSave} />);
    selectModerator();

    await act(async () => {
      fireEvent.click(screen.getByText('Save Role'));
    });

    // While saving, spinner should show and button disabled
    expect(screen.getByTestId('loading-spinner')).toBeInTheDocument();
    expect(screen.getByText('Saving...').closest('button')).toBeDisabled();

    // Resolve the save — try/finally resets isRoleSaving regardless
    await act(async () => {
      resolveSave();
    });

    // After completion, save button should be re-enabled
    expect(screen.getByText('Save Role')).toBeInTheDocument();
    expect(screen.queryByTestId('loading-spinner')).not.toBeInTheDocument();
    expect(screen.getByText('Save Role').closest('button')).not.toBeDisabled();
  });

  it('editing role color updates the color input', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    selectModerator();

    // There are two inputs with the color value (type="color" and type="text")
    // Target the text input specifically
    const colorTextInput = screen.getByLabelText('Role Color') as HTMLInputElement;
    const textColorInput = colorTextInput
      .closest('.form-group')!
      .querySelector('input[type="text"]') as HTMLInputElement;
    fireEvent.change(textColorInput, { target: { value: '#00ff00' } });
    expect(textColorInput).toHaveValue('#00ff00');
  });

  const clickDelete = () => userEvent.click(screen.getByRole('button', { name: 'Delete' }));
  const confirmDelete = () =>
    userEvent.click(
      within(screen.getByRole('dialog', { name: 'Delete Role' })).getByRole('button', {
        name: 'Delete Role',
      })
    );

  describe('create', () => {
    // Mutation: sending `onCreateRole(request, confirmation)` unconditionally (an `undefined`
    // second argument on the first send) turns this red.
    it('first send carries the new role and no confirmation', async () => {
      render(<RoleEditorPanel {...defaultProps} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      expect(defaultProps.onCreateRole).toHaveBeenCalledTimes(1);
      expect(defaultProps.onCreateRole.mock.calls[0]).toEqual([
        { name: 'New Role', color: '#99aab5', permissions: '0' },
      ]);
    });

    // Mutation: the request built inside \`send\` (re-read from the live \`roles\` on the re-send):
    // a role another administrator created meanwhile renames what was verified (#3456 review).
    it('re-sends the role the first send carried, even after the role list changes', async () => {
      defaultProps.onCreateRole
        .mockResolvedValueOnce(mfaRefused())
        .mockResolvedValueOnce({ ok: true, role: createdRole });
      const view = render(<RoleEditorPanel {...defaultProps} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      await screen.findByRole('dialog', { name: DIALOG_TITLE });

      view.rerender(
        <RoleEditorPanel {...defaultProps} roles={[...defaultProps.roles, createdRole]} />
      );
      await verifyInDialog('Create Role');

      await waitFor(() => expect(defaultProps.onCreateRole).toHaveBeenCalledTimes(2));
      const [first, second] = defaultProps.onCreateRole.mock.calls;
      expect(second[0]).toBe(first[0]);
      expect(second[0]).toEqual({ name: 'New Role', color: '#99aab5', permissions: '0' });
    });

    // Mutation: dropping `setSelectedRoleId(outcome.role.id)` from handleCreateRole in RoleEditorPanel.tsx leaves the empty state (red).
    it('selects the created role', async () => {
      render(<RoleEditorPanel {...defaultProps} roles={[...defaultProps.roles, createdRole]} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      expect(await screen.findByLabelText('Role Name')).toHaveValue('New Role');
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    });

    // Mutation: selecting on `outcome` rather than `outcome.ok` in handleCreateRole selects a role from a failure (red).
    it('a plain failure shows the server sentence and selects nothing', async () => {
      defaultProps.onCreateRole.mockResolvedValue(
        refusedWith({ status: 409, body: { error: 'Role limit reached' } })
      );
      render(<RoleEditorPanel {...defaultProps} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      expect(await screen.findByText('Role limit reached')).toBeInTheDocument();
      expect(screen.getByText(EMPTY_STATE)).toBeInTheDocument();
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    });

    // Mutation: changing WRITE_COPY['roles.create'].failure in RoleEditorPanel.tsx changes this sentence (red).
    it('a failure with no server sentence falls back to the create copy', async () => {
      defaultProps.onCreateRole.mockResolvedValue({ ok: false, kind: 'network' });
      render(<RoleEditorPanel {...defaultProps} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      expect(await screen.findByText('Failed to create role')).toBeInTheDocument();
    });

    // Mutation: dropping `.catch(() => WRITE_UNKNOWN)` in startWrite lets a rejected callback escape as an unhandled rejection (red).
    it('a rejected callback reads as an unknown failure, not an unhandled error', async () => {
      defaultProps.onCreateRole.mockRejectedValue(new Error('boom'));
      render(<RoleEditorPanel {...defaultProps} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      expect(await screen.findByText('Failed to create role')).toBeInTheDocument();
    });

    // Mutation: ignoring the refusal in startWrite (never calling setPending) leaves no dialog (red).
    it('a refusal opens the dialog; the code re-sends the create and selects the new role', async () => {
      defaultProps.onCreateRole
        .mockResolvedValueOnce(mfaRefused())
        .mockResolvedValueOnce({ ok: true, role: createdRole });
      render(<RoleEditorPanel {...defaultProps} roles={[...defaultProps.roles, createdRole]} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      await screen.findByRole('dialog', { name: DIALOG_TITLE });
      expect(screen.queryByText('Failed to create role')).toBeNull();
      await verifyInDialog('Create Role');
      await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
      expect(defaultProps.onCreateRole).toHaveBeenCalledTimes(2);
      expect(defaultProps.onCreateRole.mock.calls[1]).toEqual([
        defaultProps.onCreateRole.mock.calls[0][0],
        { mfaCode: FIXTURE_OTP, context: expect.anything() },
      ]);
      expect(screen.getByLabelText('Role Name')).toHaveValue('New Role');
    });

    // Mutation: making the isDirty callback passed to useDiscardPrompt in RoleEditorPanel.tsx `() => true` asks to discard a create that has no edits (red).
    it('enrolment refusal: the setup link opens settings with no discard question', async () => {
      defaultProps.onCreateRole.mockResolvedValue(refusedWith(ENROLMENT_REQUIRED));
      render(<RoleEditorPanel {...defaultProps} />);
      await userEvent.click(screen.getByText('+ Create Role'));
      expect(await screen.findByText(ENROLMENT_TEXT)).toBeInTheDocument();
      await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
          kind: 'serverSettings',
          serverId: 'server-1',
          section: 'roles',
        })
      );
      expect(screen.queryByRole('dialog', { name: 'Discard unsaved changes?' })).toBeNull();
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    });
  });

  describe('save', () => {
    const body = {
      name: 'Super Mod',
      color: '#ff0000',
      emoji: '',
      permissions: '3',
      display_separately: false,
      mentionable: false,
    };
    const editAndSave = async () => {
      selectModerator();
      fireEvent.change(screen.getByLabelText('Role Name'), { target: { value: 'Super Mod' } });
      await userEvent.click(screen.getByRole('button', { name: 'Save Role' }));
    };

    // Mutation: dropping `confirmation` from the re-send (`onSaveRole(roleId, data)`) in handleSaveRole sends the code nowhere (red).
    // Mutation: dropping `capture={pending?.context}` from the dialog in RoleEditorPanel.tsx re-sends against the dialog's own capture, not the refused save's (red).
    it('a refusal opens the dialog; the code re-sends the same body with the confirmation', async () => {
      const refused = mfaRefused();
      defaultProps.onSaveRole.mockResolvedValueOnce(refused).mockResolvedValueOnce(OK);
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      await screen.findByRole('dialog', { name: DIALOG_TITLE });
      expect(screen.queryByRole('alert')).toBeNull();
      expect(defaultProps.onSaveRole).toHaveBeenCalledTimes(1);
      expect(defaultProps.onSaveRole.mock.calls[0]).toEqual(['role-1', body]);

      await verifyInDialog('Save Role');
      await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
      expect(defaultProps.onSaveRole).toHaveBeenCalledTimes(2);
      expect(defaultProps.onSaveRole.mock.calls[1]).toEqual([
        'role-1',
        body,
        { mfaCode: FIXTURE_OTP, context: expect.anything() },
      ]);
      // The same capture, not an equal one: the dialog never took its own.
      expect(defaultProps.onSaveRole.mock.calls[1][2]?.context).toBe(refused.context);
      expect(screen.getByLabelText('Role Name')).toHaveValue('Super Mod');
      expect(screen.getByRole('button', { name: 'Save Role' })).not.toBeDisabled();
    });

    // Mutation: dropping the apiRequestContextIsCurrent check from permissionWriteStepUp.stepUpSeedOf hands the refused save to whoever is signed in now (red).
    // Mutation: dropping the stale arm from permissionWriteStepUp.failureTextOf words it as the server's "MFA required" (red).
    it('a refusal that lands after the account changed opens no dialog and says the session ended', async () => {
      defaultProps.onSaveRole.mockImplementationOnce(async () => {
        const refused = mfaRefused();
        useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
        return refused;
      });
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      expect(await screen.findByText(FIRST_SEND_SESSION_CHANGED)).toBeInTheDocument();
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
      expect(defaultProps.onSaveRole).toHaveBeenCalledTimes(1);
      expect(screen.getByRole('button', { name: 'Save Role' })).not.toBeDisabled();
    });

    // Mutation: making startWrite open the dialog for any refusal (`refusal === null` always false) shows no server sentence (red).
    it('a plain failure shows the server sentence and opens no dialog', async () => {
      defaultProps.onSaveRole.mockResolvedValue(
        refusedWith({ status: 403, body: { error: 'Role name is reserved' } })
      );
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      expect(await screen.findByText('Role name is reserved')).toBeInTheDocument();
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
      expect(screen.getByRole('button', { name: 'Save Role' })).not.toBeDisabled();
    });

    // Mutation: changing WRITE_COPY['roles.update'].failure in RoleEditorPanel.tsx changes this sentence (red).
    it('a failure with no server sentence falls back to the save copy', async () => {
      defaultProps.onSaveRole.mockRejectedValue(new Error('offline'));
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      expect(await screen.findByText('Failed to save role')).toBeInTheDocument();
    });

    // Mutation: widening permissionWriteStepUp.stepUpSeedOf to return any adapter refusal opens the dialog for a 429 (red).
    it('a step-up budget refusal is worded as a failure, not offered a code', async () => {
      defaultProps.onSaveRole.mockResolvedValue(
        refusedWith({ status: 429, body: { step_up_budget_exhausted: true } })
      );
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      expect(await screen.findByText('Failed to save role')).toBeInTheDocument();
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    });

    // Mutation: dropping `setWriteError(null)` from handleSelectRole keeps the last role's error on the next role (red).
    it('selecting another role clears the error', async () => {
      defaultProps.onSaveRole.mockResolvedValue(
        refusedWith({ status: 400, body: { error: 'Role name is reserved' } })
      );
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      await screen.findByText('Role name is reserved');
      fireEvent.click(screen.getByText('Admin').closest('button')!);
      expect(screen.queryByText('Role name is reserved')).toBeNull();
    });

    // Mutation: replacing `onClose={endWrite}` with a no-op leaves the dialog over the editor (red).
    it('cancelling the dialog keeps the editor and edits, with no alert', async () => {
      defaultProps.onSaveRole.mockResolvedValue(mfaRefused());
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      await screen.findByLabelText(CODE_LABEL);
      await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull());
      expect(screen.getByLabelText('Role Name')).toHaveValue('Super Mod');
      expect(screen.queryByRole('alert')).toBeNull();
      expect(defaultProps.onSaveRole).toHaveBeenCalledTimes(1);
    });

    // Mutation: dropping `confirmDiscard` from the openVerificationSetup call lets Settings open over unsaved edits (red).
    it('enrolment refusal: the setup link asks to discard edits before leaving', async () => {
      defaultProps.onSaveRole.mockResolvedValue(refusedWith(ENROLMENT_REQUIRED));
      render(<RoleEditorPanel {...defaultProps} />);
      await editAndSave();
      await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));
      const discard = await screen.findByRole('dialog', { name: 'Discard unsaved changes?' });
      expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();

      await userEvent.click(within(discard).getByRole('button', { name: 'Cancel' }));
      await waitFor(() =>
        expect(screen.queryByRole('dialog', { name: 'Discard unsaved changes?' })).toBeNull()
      );
      expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
      expect(screen.getByText(ENROLMENT_TEXT)).toBeInTheDocument();

      await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
      await userEvent.click(
        within(await screen.findByRole('dialog', { name: 'Discard unsaved changes?' })).getByRole(
          'button',
          { name: 'Discard Changes' }
        )
      );
      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
          kind: 'serverSettings',
          serverId: 'server-1',
          section: 'roles',
        })
      );
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    });
  });

  // The General form's edits outlive a switch to Roles, and leaving for App
  // Settings drops them with the rest of the page: the panel's one question
  // covers them even when the role form itself is untouched.
  describe("the page's own edits", () => {
    const DISCARD = 'Discard unsaved changes?';

    async function refuseCreateForEnrolment() {
      defaultProps.onCreateRole.mockResolvedValue(refusedWith(ENROLMENT_REQUIRED));
      await userEvent.click(screen.getByText('+ Create Role'));
      await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));
    }

    // Mutation: dropping `pageIsDirty` from the panel's discard predicate leaves for App Settings unasked (red).
    it('a dirty page is asked about before setup, with the role form untouched', async () => {
      render(<RoleEditorPanel {...defaultProps} pageIsDirty={() => true} />);
      await refuseCreateForEnrolment();

      const discard = await screen.findByRole('dialog', { name: DISCARD });
      expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
      await userEvent.click(within(discard).getByRole('button', { name: 'Discard Changes' }));
      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
          kind: 'serverSettings',
          serverId: 'server-1',
          section: 'roles',
        })
      );
    });

    // Control: a clean page and a clean form leave at once, with no question.
    it('a clean page is not asked about', async () => {
      render(<RoleEditorPanel {...defaultProps} pageIsDirty={() => false} />);
      await refuseCreateForEnrolment();

      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).not.toBeNull()
      );
      expect(screen.queryByRole('dialog', { name: DISCARD })).toBeNull();
    });
  });

  describe('delete', () => {
    // Mutation: calling onDeleteRole from the Delete button directly (no confirmation, R11) turns this red.
    it('opens a confirmation first and sends nothing until it is confirmed', async () => {
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      const confirm = screen.getByRole('dialog', { name: 'Delete Role' });
      expect(within(confirm).getByText('Moderator')).toBeInTheDocument();
      expect(defaultProps.onDeleteRole).not.toHaveBeenCalled();

      await userEvent.click(within(confirm).getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete Role' })).toBeNull());
      expect(defaultProps.onDeleteRole).not.toHaveBeenCalled();
      expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');
    });

    // Mutation: dropping `onDeleted={clearSelection}` in RoleEditorPanel.tsx keeps the deleted role selected (red).
    it('confirming clears the selection on success and lands focus in the role list', async () => {
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      await waitFor(() => expect(screen.getByText(EMPTY_STATE)).toBeInTheDocument());
      expect(defaultProps.onDeleteRole.mock.calls).toEqual([['role-1']]);
      await waitFor(() => expect(focusIsInRoleList()).toBe(true));
    });

    // Mutation: clearing the selection before the outcome is read (ignoring `outcome.ok`) empties the editor on a failure (red).
    it('a failure keeps the selection and words the server sentence in the confirmation', async () => {
      defaultProps.onDeleteRole.mockResolvedValue(
        refusedWith({ status: 409, body: { error: 'Role is managed by an integration' } })
      );
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      expect(await screen.findByText('Role is managed by an integration')).toBeInTheDocument();
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
      expect(screen.queryByText(EMPTY_STATE)).toBeNull();

      await userEvent.click(
        within(screen.getByRole('dialog', { name: 'Delete Role' })).getByRole('button', {
          name: 'Cancel',
        })
      );
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Delete Role' })).toBeNull());
      expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');
    });

    // Mutation: dropping `.catch(() => WRITE_UNKNOWN)` in RoleDeleteFlow.handleConfirm leaves a rejected delete unworded (red).
    it('a rejected callback reads as an unknown failure and keeps the selection', async () => {
      defaultProps.onDeleteRole.mockRejectedValue(new Error('offline'));
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      expect(await screen.findByText('Failed to delete role')).toBeInTheDocument();
      expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');
    });

    // Mutation: dropping `onDeleted()` from the dialog's onSuccess in RoleDeleteFlow.tsx keeps the role selected after a verified delete; passing `() => null` to useFocusFallbackOnEnd leaves focus on <body> (red).
    it('refusal swaps the confirmation for the dialog; the code re-sends and clears the selection', async () => {
      defaultProps.onDeleteRole.mockResolvedValueOnce(mfaRefused()).mockResolvedValueOnce(OK);
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();

      await screen.findByRole('dialog', { name: DIALOG_TITLE });
      expect(screen.queryByRole('dialog', { name: 'Delete Role' })).toBeNull();
      expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');

      await verifyInDialog('Delete Role');
      await waitFor(() => expect(screen.getByText(EMPTY_STATE)).toBeInTheDocument());
      expect(screen.queryByRole('dialog')).toBeNull();
      expect(defaultProps.onDeleteRole.mock.calls).toEqual([
        ['role-1'],
        ['role-1', { mfaCode: FIXTURE_OTP, context: expect.anything() }],
      ]);
      await waitFor(() => expect(document.activeElement).not.toBe(document.body));
      expect(focusIsInRoleList()).toBe(true);
    });

    // Mutation: calling `onDeleted()` from the dialog's `onClose` in RoleDeleteFlow.tsx clears the selection on a cancel (red).
    it('cancelling the dialog keeps the role selected and focus off <body>', async () => {
      defaultProps.onDeleteRole.mockResolvedValue(mfaRefused());
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      await screen.findByLabelText(CODE_LABEL);
      await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');
      expect(defaultProps.onDeleteRole).toHaveBeenCalledTimes(1);
      expect(document.activeElement).not.toBe(document.body);
    });

    // Mutation: opening the confirmation on `pending === null` without `&& !ending` re-mounts "Delete Role" for the commit between the dialog closing and the flow ending; it takes focus and closes, and focus never reaches the Delete button (red).
    it('cancelling the dialog returns focus to the Delete button and never re-shows the confirmation', async () => {
      defaultProps.onDeleteRole.mockResolvedValue(mfaRefused());
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      await screen.findByLabelText(CODE_LABEL);

      // Records, not a re-query: the re-shown confirmation is added and removed within one flush.
      const reshown: string[] = [];
      const watch = new MutationObserver((records) => {
        for (const added of records.flatMap((record) => [...record.addedNodes])) {
          if (added.textContent?.includes(CONFIRMATION_TEXT)) reshown.push(added.nodeName);
        }
      });
      watch.observe(document.body, { childList: true, subtree: true });
      await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      watch.disconnect();

      expect(reshown).toEqual([]);
      expect(screen.getByRole('button', { name: 'Delete' })).toHaveFocus();
    });

    // Mutation: dropping `closeHost: endStepUp` from the delete flow's setup link leaves the dialog over Settings (red).
    it('enrolment refusal: the setup link opens settings and abandons the delete', async () => {
      defaultProps.onDeleteRole.mockResolvedValue(refusedWith(ENROLMENT_REQUIRED));
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));
      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
          kind: 'serverSettings',
          serverId: 'server-1',
          section: 'roles',
        })
      );
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(defaultProps.onDeleteRole).toHaveBeenCalledTimes(1);
      expect(screen.getByLabelText('Role Name')).toHaveValue('Moderator');
    });

    // Mutation: dropping `confirmDiscard` from the RoleDeleteFlow props lets Settings open over unsaved edits in the form behind the delete (red).
    it('enrolment refusal: the setup link asks to discard form edits, and a declined discard changes nothing', async () => {
      defaultProps.onDeleteRole.mockResolvedValue(refusedWith(ENROLMENT_REQUIRED));
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      fireEvent.change(screen.getByLabelText('Role Name'), { target: { value: 'Super Mod' } });
      await clickDelete();
      await confirmDelete();
      await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));
      const discard = await screen.findByRole('dialog', { name: 'Discard unsaved changes?' });
      expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();

      await userEvent.click(within(discard).getByRole('button', { name: 'Cancel' }));
      await waitFor(() =>
        expect(screen.queryByRole('dialog', { name: 'Discard unsaved changes?' })).toBeNull()
      );
      expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
      expect(screen.getByText(ENROLMENT_TEXT)).toBeInTheDocument();
      expect(screen.getByLabelText('Role Name')).toHaveValue('Super Mod');

      await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
      await userEvent.click(
        within(await screen.findByRole('dialog', { name: 'Discard unsaved changes?' })).getByRole(
          'button',
          { name: 'Discard Changes' }
        )
      );
      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
          kind: 'serverSettings',
          serverId: 'server-1',
          section: 'roles',
        })
      );
      expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
      expect(defaultProps.onDeleteRole).toHaveBeenCalledTimes(1);
    });

    // Mutation: making `formIsDirty` in RoleEditorPanel.tsx `() => true` asks to discard a form the user never changed (red).
    it('enrolment refusal: with no form edits the setup link opens settings without asking', async () => {
      defaultProps.onDeleteRole.mockResolvedValue(refusedWith(ENROLMENT_REQUIRED));
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await clickDelete();
      await confirmDelete();
      await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));
      await waitFor(() =>
        expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({
          kind: 'serverSettings',
          serverId: 'server-1',
          section: 'roles',
        })
      );
      expect(screen.queryByRole('dialog', { name: 'Discard unsaved changes?' })).toBeNull();
    });
  });

  // One role write at a time: a second would replace the first's refusal, and the
  // dialog under the user would swap its purpose and intro mid-verification.
  describe('one write at a time', () => {
    const SAVE_INTRO = 'This server asks you to verify before you save changes to this role.';
    const createButton = () => screen.getByRole('button', { name: '+ Create Role' });
    /** The editor's own Save, which the step-up dialog's primary of the same name hides. */
    const editorSaveButton = () => {
      const editor = document.querySelector<HTMLElement>('.role-editor');
      if (editor === null) throw new Error('the role editor is not rendered');
      return within(editor).getByRole('button', { name: 'Save Role', hidden: true });
    };

    /** A save whose answer the test holds back, so the create lands while it is in flight. */
    function holdSave() {
      let answer: (outcome: PermissionWriteOutcome) => void = () => undefined;
      defaultProps.onSaveRole.mockImplementation(
        () =>
          new Promise<PermissionWriteOutcome>((resolve) => {
            answer = resolve;
          })
      );
      return (outcome: PermissionWriteOutcome) => act(async () => answer(outcome));
    }

    // Mutation: dropping `createDisabled={writeBusy}` from the rail in RoleEditorPanel.tsx leaves the create control live while the save is in flight (red).
    it('the create control is unavailable while a save is in flight, and still takes focus', async () => {
      const answer = holdSave();
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await userEvent.click(screen.getByRole('button', { name: 'Save Role' }));

      expect(createButton()).toHaveAttribute('aria-disabled', 'true');
      expect(createButton()).not.toBeDisabled();
      await userEvent.click(createButton());
      expect(defaultProps.onCreateRole).not.toHaveBeenCalled();

      await answer(OK);
      await waitFor(() => expect(createButton()).not.toHaveAttribute('aria-disabled'));
      await userEvent.click(createButton());
      expect(defaultProps.onCreateRole).toHaveBeenCalledTimes(1);
    });

    // Mutation: dropping the `writeBusy` guard from startWrite AND the rail's availability lets the create's refusal replace the save's (red).
    it("a create clicked before the save is refused leaves the save's dialog as it was", async () => {
      defaultProps.onCreateRole.mockResolvedValue(refusedWith(MFA_REQUIRED));
      const answer = holdSave();
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await userEvent.click(screen.getByRole('button', { name: 'Save Role' }));
      await userEvent.click(createButton());

      await answer(mfaRefused());

      const dialog = await screen.findByRole('dialog', { name: DIALOG_TITLE });
      expect(within(dialog).getByText(SAVE_INTRO)).toBeInTheDocument();
      expect(within(dialog).getByRole('button', { name: 'Save Role' })).toBeInTheDocument();
      expect(defaultProps.onCreateRole).not.toHaveBeenCalled();
    });

    // Mutation: dropping `pending !== null` from the `writeBusy` guard in startWrite sends a second save while the first one's dialog is up and replaces its refusal (red).
    it('a save activated while the dialog is up sends nothing', async () => {
      defaultProps.onSaveRole.mockResolvedValue(mfaRefused());
      render(<RoleEditorPanel {...defaultProps} />);
      selectModerator();
      await userEvent.click(screen.getByRole('button', { name: 'Save Role' }));
      await screen.findByRole('dialog', { name: DIALOG_TITLE });
      expect(defaultProps.onSaveRole).toHaveBeenCalledTimes(1);

      // The dialog is modal; a stray activation behind it is still ignored.
      await act(async () => {
        editorSaveButton().click();
      });
      expect(defaultProps.onSaveRole).toHaveBeenCalledTimes(1);
    });
  });
});
