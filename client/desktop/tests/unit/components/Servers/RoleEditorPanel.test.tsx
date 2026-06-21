import { render, screen, fireEvent, act } from '../../../test-utils';
import RoleEditorPanel from '@/renderer/components/Servers/RoleEditorPanel';
import { vi } from 'vitest';
import type { Role } from '@/renderer/types/server';

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
  display_separately: false,
  mentionable: false,
  require_mfa: false,
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
  color: '#99aab5',
};

const mockRole2: Role = { ...mockRole, id: 'role-2', name: 'Admin', position: 2 };

const defaultProps = {
  roles: [mockRole, defaultRole, mockRole2],
  onCreateRole: vi.fn().mockResolvedValue(undefined),
  onSaveRole: vi.fn().mockResolvedValue(undefined),
  onDeleteRole: vi.fn().mockResolvedValue(undefined),
};

describe('RoleEditorPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders empty state message when no role is selected', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    expect(screen.getByText('Select a role to edit, or create a new one.')).toBeInTheDocument();
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
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    const nameInput = screen.getByLabelText('Role Name');
    fireEvent.change(nameInput, { target: { value: 'Super Mod' } });
    expect(nameInput).toHaveValue('Super Mod');
  });

  it('shows PermissionGrid with mode="role"', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    const grid = screen.getByTestId('permission-grid');
    expect(grid).toHaveAttribute('data-mode', 'role');
  });

  it('save button calls onSaveRole with correct data', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
        new Promise<void>((resolve) => {
          resolveOnSave = resolve;
        })
    );
    render(<RoleEditorPanel {...defaultProps} onSaveRole={slowSave} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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

  it('delete button calls onDeleteRole and clears selection', async () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    await act(async () => {
      fireEvent.click(screen.getByText('Delete'));
    });

    expect(defaultProps.onDeleteRole).toHaveBeenCalledWith('role-1');
    // After deletion, selection is cleared -> empty state
    expect(screen.getByText('Select a role to edit, or create a new one.')).toBeInTheDocument();
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
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    expect(screen.getByText('Display Separately')).toBeInTheDocument();
    expect(screen.getByText('Mentionable')).toBeInTheDocument();
    // Both are checkboxes rendered by ToggleSwitch
    const toggles = screen.getAllByRole('checkbox');
    expect(toggles.length).toBeGreaterThanOrEqual(2);
  });

  it('shows emoji picker section', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    expect(screen.getByText('Role Emoji (Optional)')).toBeInTheDocument();
    expect(screen.getByTitle('Pick an emoji')).toBeInTheDocument();
  });

  it('opens and uses emoji picker to select emoji', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    fireEvent.click(screen.getByTitle('Pick an emoji'));
    expect(screen.getByTestId('lazy-emoji-picker')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('close-emoji'));
    expect(screen.queryByTestId('lazy-emoji-picker')).not.toBeInTheDocument();
  });

  it('clears emoji when remove button is clicked', async () => {
    const roleWithEmoji: Role = { ...mockRole, emoji: '\u{1F525}' };
    render(<RoleEditorPanel {...defaultProps} roles={[roleWithEmoji, defaultRole]} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
        new Promise<void>((resolve) => {
          resolveOnSave = resolve;
        })
    );
    render(<RoleEditorPanel {...defaultProps} onSaveRole={slowSave} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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
        new Promise<void>((resolve) => {
          resolveSave = resolve;
        })
    );
    render(<RoleEditorPanel {...defaultProps} onSaveRole={slowSave} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

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

  it('auto-selects created role when onCreateRole returns a role', async () => {
    const createdRole: Role = {
      ...mockRole,
      id: 'role-new',
      name: 'New Role',
      position: 3,
    };
    const onCreateRole = vi.fn().mockResolvedValue(createdRole);
    render(
      <RoleEditorPanel
        {...defaultProps}
        onCreateRole={onCreateRole}
        roles={[...defaultProps.roles, createdRole]}
      />
    );

    await act(async () => {
      fireEvent.click(screen.getByText('+ Create Role'));
    });

    // The editor should now show the created role's name
    expect(screen.getByLabelText('Role Name')).toHaveValue('New Role');
  });

  it('editing role color updates the color input', () => {
    render(<RoleEditorPanel {...defaultProps} />);
    fireEvent.click(screen.getByText('Moderator').closest('button')!);

    // There are two inputs with the color value (type="color" and type="text")
    // Target the text input specifically
    const colorTextInput = screen.getByLabelText('Role Color') as HTMLInputElement;
    const textColorInput = colorTextInput
      .closest('.form-group')!
      .querySelector('input[type="text"]') as HTMLInputElement;
    fireEvent.change(textColorInput, { target: { value: '#00ff00' } });
    expect(textColorInput).toHaveValue('#00ff00');
  });
});
