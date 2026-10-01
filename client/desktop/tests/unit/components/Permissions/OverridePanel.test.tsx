import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';

vi.mock('@/renderer/components/Permissions/OverridePanel.css', () => ({}));

vi.mock('@/renderer/components/Permissions/PermissionGrid', () => ({
  default: ({
    value,
    onChange,
    deny,
    onDenyChange,
    mode,
    disabled: isDisabled,
  }: {
    value: bigint;
    onChange: (v: bigint) => void;
    deny: bigint;
    onDenyChange?: (v: bigint) => void;
    mode: string;
    disabled?: boolean;
  }) => (
    <div
      data-testid="permission-grid"
      data-mode={mode}
      data-disabled={isDisabled}
      data-allow={value.toString()}
      data-deny={deny.toString()}
    >
      <button data-testid="set-allow" onClick={() => onChange(value | 1n)}>
        Set Allow
      </button>
      {onDenyChange && (
        <button data-testid="set-deny" onClick={() => onDenyChange(deny | 2n)}>
          Set Deny
        </button>
      )}
    </div>
  ),
}));

import OverridePanel from '@/renderer/components/Permissions/OverridePanel';
import { ChannelOverride } from '@/renderer/stores/chat/permissionStore';
import { Role } from '@/renderer/types/server';
import { ServerMember } from '@/renderer/stores/chat/memberStore';

// --- Mock data ---

const mockRole: Role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  color: '#ff0000',
  position: 1,
  permissions: '0',
  is_default: false,
  is_managed: false,
  display_separately: false,
  mentionable: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};
const mockRole2: Role = { ...mockRole, id: 'role-2', name: 'Admin', position: 2 };

const mockMember: ServerMember = {
  user_id: 'user-1',
  username: 'testuser',
  display_name: 'Test User',
  role: 'member',
  joined_at: '2025-01-01T00:00:00Z',
  roles: [],
};

const mockRoleOverride: ChannelOverride = {
  id: 'override-1',
  channel_id: 'cat-1',
  target_type: 'role',
  target_id: 'role-1',
  allow: '1',
  deny: '2',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockUserOverride: ChannelOverride = {
  id: 'override-2',
  channel_id: 'cat-1',
  target_type: 'user',
  target_id: 'user-1',
  allow: '4',
  deny: '0',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

// --- Helpers ---

function clickOverrideItem(name: string) {
  const el = document.querySelector('.override-target-name');
  expect(el).toHaveTextContent(name);
  const btn = el!.closest('.override-item')!.querySelector('.override-item-select')!;
  fireEvent.click(btn);
}

// Unlike clickOverrideItem, finds the row by name rather than assuming it's
// the first one in the DOM — needed once a test has more than one override.
function selectOverrideByName(name: string) {
  const el = screen.getByText(name, { selector: '.override-target-name' });
  const btn = el.closest('.override-item')!.querySelector('.override-item-select')!;
  fireEvent.click(btn);
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

const defaultProps = {
  overrides: [] as ChannelOverride[],
  roles: [mockRole, mockRole2],
  members: [mockMember],
  onUpsert: vi.fn().mockResolvedValue(true),
  onDelete: vi.fn().mockResolvedValue(true),
};

describe('OverridePanel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // 1. Renders empty message when no overrides
  it('renders empty message when no overrides', () => {
    render(<OverridePanel {...defaultProps} />);
    expect(screen.getByText('No permission overrides configured.')).toBeInTheDocument();
  });

  // 2. Renders custom empty message
  it('renders custom empty message', () => {
    render(<OverridePanel {...defaultProps} emptyMessage="Nothing here" />);
    expect(screen.getByText('Nothing here')).toBeInTheDocument();
  });

  // 3. Displays role overrides section when role overrides present
  it('displays role overrides section when role overrides present', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    expect(screen.getByText('Role Overrides')).toBeInTheDocument();
    expect(
      screen.getByText('Moderator', { selector: '.override-target-name' })
    ).toBeInTheDocument();
  });

  // 4. Displays user overrides section when user overrides present
  it('displays user overrides section when user overrides present', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockUserOverride]} />);
    expect(screen.getByText('User Overrides')).toBeInTheDocument();
    expect(screen.getByText('Test User')).toBeInTheDocument();
  });

  // 5. Shows allow/deny counts in summary
  it('shows allow/deny counts in summary', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    expect(screen.getByText('1 allowed')).toBeInTheDocument();
    expect(screen.getByText('1 denied')).toBeInTheDocument();
  });

  // 6. Shows "Unknown Role" for missing role
  it('shows "Unknown Role" for missing role', () => {
    const orphanOverride: ChannelOverride = {
      ...mockRoleOverride,
      id: 'override-orphan',
      target_id: 'role-nonexistent',
    };
    render(<OverridePanel {...defaultProps} overrides={[orphanOverride]} />);
    expect(screen.getByText('Unknown Role')).toBeInTheDocument();
  });

  // 7. Shows "Unknown User" for missing member
  it('shows "Unknown User" for missing member', () => {
    const orphanOverride: ChannelOverride = {
      ...mockUserOverride,
      id: 'override-orphan',
      target_id: 'user-nonexistent',
    };
    render(<OverridePanel {...defaultProps} overrides={[orphanOverride]} />);
    expect(screen.getByText('Unknown User')).toBeInTheDocument();
  });

  // 8. Shows username when display_name not set
  it('shows username when display_name is not set', () => {
    const memberNoDisplay: ServerMember = {
      ...mockMember,
      display_name: undefined,
    };
    render(
      <OverridePanel {...defaultProps} members={[memberNoDisplay]} overrides={[mockUserOverride]} />
    );
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  // 9. Selects override and shows editor with PermissionGrid
  it('selects override and shows editor with PermissionGrid', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
    expect(screen.getByTestId('permission-grid')).toBeInTheDocument();
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-mode', 'override');
  });

  // 10. Saves override calls onUpsert with correct data
  it('saves override and calls onUpsert with correct data', async () => {
    const onUpsert = vi.fn().mockResolvedValue(true);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');

    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    expect(onUpsert).toHaveBeenCalledWith({
      target_type: 'role',
      target_id: 'role-1',
      allow: '1',
      deny: '2',
    });
  });

  // 11. Cancel deselects override
  it('cancel deselects override', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Cancel'));
    expect(screen.queryByText('Editing: Moderator')).not.toBeInTheDocument();
  });

  // 11b. Cancel is a secondary action, not a dimmed brand button
  it('draws Cancel as an opaque secondary action', () => {
    // opacity: 0.7 on the brand pair blended white into the light panel: 2.8:1 in
    // Hacker light, and the contrast ratchet cannot see opacity.
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    const cancel = screen.getByText('Cancel');
    expect(cancel.style.opacity).toBe('');
    expect(cancel).toHaveClass('override-cancel-btn');
    expect(cancel).not.toHaveClass('add-override-btn');
  });

  // 12. Delete calls onDelete
  it('delete calls onDelete', async () => {
    const onDelete = vi.fn().mockResolvedValue(true);
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);
    const deleteBtn = screen.getByLabelText('Delete override');
    await act(async () => {
      fireEvent.click(deleteBtn);
    });
    expect(onDelete).toHaveBeenCalledWith('override-1');
  });

  // 13. Clears selection when selected override is deleted
  it('clears selection when selected override is deleted', async () => {
    const onDelete = vi.fn().mockResolvedValue(true);
    const { rerender } = render(
      <OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />
    );
    clickOverrideItem('Moderator');
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });

    // After delete, the parent would remove the override from the list
    rerender(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[]} />);
    expect(screen.queryByText('Editing: Moderator')).not.toBeInTheDocument();
  });

  // 14. Add Override section shows when no override selected and not disabled
  it('shows Add Override section when no override selected and not disabled', () => {
    render(<OverridePanel {...defaultProps} />);
    expect(screen.getByText('Add Override', { selector: '.section-header' })).toBeInTheDocument();
  });

  // 15. Hides Add Override when override is selected
  it('hides Add Override when override is selected', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    expect(screen.getByText('Add Override', { selector: '.section-header' })).toBeInTheDocument();
    clickOverrideItem('Moderator');
    expect(
      screen.queryByText('Add Override', { selector: '.section-header' })
    ).not.toBeInTheDocument();
  });

  // 16. Shows role options in target dropdown
  it('shows role options in target dropdown', () => {
    render(<OverridePanel {...defaultProps} />);
    const targetSelect = screen.getAllByRole('combobox')[1];
    const options = targetSelect.querySelectorAll('option');
    // placeholder + 2 roles
    expect(options).toHaveLength(3);
    expect(options[1]).toHaveTextContent('Moderator');
    expect(options[2]).toHaveTextContent('Admin');
  });

  // 17. Switches to user options
  it('switches to user options', () => {
    render(<OverridePanel {...defaultProps} />);
    const typeSelect = screen.getAllByRole('combobox')[0];
    fireEvent.change(typeSelect, { target: { value: 'user' } });

    const targetSelect = screen.getAllByRole('combobox')[1];
    const options = targetSelect.querySelectorAll('option');
    // placeholder + 1 member
    expect(options).toHaveLength(2);
    expect(options[1]).toHaveTextContent('Test User');
  });

  // 18. Enables Add button when target selected
  it('enables Add button when target selected', () => {
    render(<OverridePanel {...defaultProps} />);
    const addBtn = screen.getByRole('button', { name: 'Add Override' });
    expect(addBtn).toBeDisabled();

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    expect(addBtn).not.toBeDisabled();
  });

  // 19. Adds override calls onUpsert
  it('adds override and calls onUpsert', async () => {
    const onUpsert = vi.fn().mockResolvedValue(true);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    expect(onUpsert).toHaveBeenCalledWith({
      target_type: 'role',
      target_id: 'role-1',
      allow: '0',
      deny: '0',
    });
  });

  // 20. Does not call onUpsert when no target selected
  it('does not call onUpsert when no target selected', async () => {
    const onUpsert = vi.fn().mockResolvedValue(true);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const addBtn = screen.getByRole('button', { name: 'Add Override' });
    // Button is disabled, but let's also verify onUpsert isn't called
    await act(async () => {
      fireEvent.click(addBtn);
    });

    expect(onUpsert).not.toHaveBeenCalled();
  });

  // 21. Resets form after add
  it('resets form after add', async () => {
    const onUpsert = vi.fn().mockResolvedValue(true);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    // After adding, target select should reset to empty
    expect((targetSelect as HTMLSelectElement).value).toBe('');
  });

  // 22. Does not clear selection when onUpsert returns false (save)
  it('does not clear selection when onUpsert returns false (save)', async () => {
    const onUpsert = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    // Editor should still be visible because onUpsert returned false
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
  });

  // 23. Does not clear selection when onDelete returns false
  it('does not clear selection when onDelete returns false', async () => {
    const onDelete = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });

    // Editor should still be visible because onDelete returned false
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
  });

  // 24. Does not reset add form when onUpsert returns false (add)
  it('does not reset add form when onUpsert returns false (add)', async () => {
    const onUpsert = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const targetSelect = screen.getAllByRole('combobox')[1] as HTMLSelectElement;
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    expect(targetSelect.value).toBe('role-1');

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    // Target select should still have its value because onUpsert returned false
    expect(targetSelect.value).toBe('role-1');
  });

  // 25. Add override selects have aria-labels
  it('add override selects have aria-labels', () => {
    render(<OverridePanel {...defaultProps} />);
    expect(screen.getByLabelText('Override target type')).toBeInTheDocument();
    expect(screen.getByLabelText('Override target')).toBeInTheDocument();
  });

  // 26. When disabled=true, hides edit and add sections but shows override list
  it('hides edit and add sections when disabled but shows override list', () => {
    render(<OverridePanel {...defaultProps} disabled overrides={[mockRoleOverride]} />);
    // Override list still visible
    expect(screen.getByText('Moderator')).toBeInTheDocument();
    expect(screen.getByText('Role Overrides')).toBeInTheDocument();
    // Add section hidden
    expect(screen.queryByText('Add Override')).not.toBeInTheDocument();
    // Select an override — editor should NOT appear
    clickOverrideItem('Moderator');
    expect(screen.queryByText('Editing: Moderator')).not.toBeInTheDocument();
  });
});

/**
 * V1-V5 (#3406 regression): the editor surfaces a `role="alert"` for a
 * rejected or refused write — save, add, and delete each get their own
 * alert slot — and keeps the editor or add form open with the in-progress
 * edits intact rather than discarding them on any settled promise.
 */
describe('OverridePanel — V1-V5 regression (#3406)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // V1 (save): a rejected save keeps the editor open with the save-alert
  // copy and the edited bits still on the grid; a retried save that
  // succeeds closes the editor and clears the alert.
  it('V1: a rejected save keeps the editor open with the alert; a retry that succeeds closes it', async () => {
    const onUpsert = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    fireEvent.click(screen.getByTestId('set-allow'));

    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Failed to save permission override. Your changes are still here — try again.'
    );
    // The edit is still on the grid, not reset by the failed save.
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-allow', '1');

    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    expect(screen.queryByText('Editing: Moderator')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // A caller that rejects instead of settling false is a refusal too: the
  // editor closes only on a confirmed write (D4). Kills dropping `.catch`.
  it('a save whose promise rejects keeps the editor open with the alert', async () => {
    const onUpsert = vi.fn().mockRejectedValue(new Error('network'));
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to save permission override.');
  });

  it('an add whose promise rejects keeps the target with the alert', async () => {
    const onUpsert = vi.fn().mockRejectedValue(new Error('network'));
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);
    const targetSelect = screen.getAllByRole('combobox')[1] as HTMLSelectElement;
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });
    expect(targetSelect.value).toBe('role-1');
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to save permission override.');
  });

  it('a delete whose promise rejects keeps the row with the alert', async () => {
    const onDelete = vi.fn().mockRejectedValue(new Error('network'));
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Delete override' }));
    });
    expect(screen.getByRole('button', { name: 'Delete override' })).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to remove permission override.');
  });

  // V2 (add): a rejected add keeps the chosen target and the edited bits and
  // shows the save-alert copy; a retry that succeeds resets the form.
  it('V2: a rejected add keeps the target and bits with the alert; a retry that succeeds resets the form', async () => {
    const onUpsert = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const targetSelect = screen.getAllByRole('combobox')[1] as HTMLSelectElement;
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    fireEvent.click(screen.getByTestId('set-allow'));

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    expect(targetSelect.value).toBe('role-1');
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Failed to save permission override. Your changes are still here — try again.'
    );

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    expect(targetSelect.value).toBe('');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // V3 (delete): a rejected delete keeps the row and the open editor and
  // shows the delete-alert copy.
  it('V3: a rejected delete keeps the row, keeps the editor open, and shows the delete alert', async () => {
    const onDelete = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');

    await act(async () => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });

    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
    expect(
      screen.getByText('Moderator', { selector: '.override-target-name' })
    ).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Failed to remove permission override. Try again.'
    );
  });

  // V4 (clearing): the alert clears on Cancel, on a grid edit while the
  // editor stays open, and immediately on a retry (before it resolves).
  it('V4a: Cancel clears a stale save alert', async () => {
    const onUpsert = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(screen.getByRole('alert')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Cancel'));
    clickOverrideItem('Moderator');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // Selecting an override clears a stale alert on its own, with no Cancel in
  // between. Only the delete alert can show this: the add form (and its alert)
  // is hidden while an editor is open, so an add alert would vanish anyway.
  it('V4d: selecting an override clears a stale delete alert', async () => {
    const onDelete = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Delete override' }));
    });
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to remove permission override.');

    clickOverrideItem('Moderator');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('V4e: changing the add target clears a stale add alert', async () => {
    const onUpsert = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);
    const [typeSelect, targetSelect] = screen.getAllByRole('combobox');
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to save permission override.');

    fireEvent.change(targetSelect, { target: { value: '' } });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });
    expect(screen.getByRole('alert')).toBeInTheDocument();
    fireEvent.change(typeSelect, { target: { value: 'user' } });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('V4b: a grid edit clears a stale save alert without closing the editor', async () => {
    const onUpsert = vi.fn().mockResolvedValue(false);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(screen.getByRole('alert')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('set-deny'));

    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('V4c: retrying a failed save clears the stale alert immediately, before the retry resolves', async () => {
    let resolveRetry: (v: boolean) => void = () => {};
    const onUpsert = vi
      .fn()
      .mockResolvedValueOnce(false)
      .mockImplementationOnce(
        () =>
          new Promise<boolean>((resolve) => {
            resolveRetry = resolve;
          })
      );
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);
    clickOverrideItem('Moderator');
    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(screen.getByRole('alert')).toBeInTheDocument();

    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    await act(async () => {
      resolveRetry(true);
    });
  });

  // V5: a disabled panel renders no "Delete override" button at all.
  it('V5: disabled renders no Delete override button', () => {
    render(<OverridePanel {...defaultProps} disabled overrides={[mockRoleOverride]} />);
    expect(screen.queryByLabelText('Delete override')).not.toBeInTheDocument();
  });

  // Stale-write race (#3406 finding 1): a Save the user cancelled must not act
  // on the editor they open next. Since round 5 the next override cannot be
  // opened until the cancelled Save settles, because it may still commit.
  it('a cancelled Save success leaves the next override editor open with no alert', async () => {
    const save = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(save.promise);
    render(
      <OverridePanel
        {...defaultProps}
        onUpsert={onUpsert}
        overrides={[mockRoleOverride, mockUserOverride]}
      />
    );

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    fireEvent.click(screen.getByText('Cancel'));
    selectOverrideByName('Test User');
    expect(screen.queryByText('Editing: Test User')).not.toBeInTheDocument();

    await act(async () => {
      save.resolve(true);
    });

    selectOverrideByName('Test User');
    expect(screen.getByText('Editing: Test User')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // A cancelled Save's refusal is the user's own abandonment, not a failure to
  // report: no alert in the list-level slot a vanished row's refusal uses, and
  // none on the override opened next.
  it('a cancelled Save failure shows no alert, before or after the next override opens', async () => {
    const save = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(save.promise);
    render(
      <OverridePanel
        {...defaultProps}
        onUpsert={onUpsert}
        overrides={[mockRoleOverride, mockUserOverride]}
      />
    );

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    fireEvent.click(screen.getByText('Cancel'));

    await act(async () => {
      save.resolve(false);
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    selectOverrideByName('Test User');
    expect(screen.getByText('Editing: Test User')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  // Stale-write race, add form (#3406 finding 1): a target/type/bits change
  // made while an add is in flight must survive a stale success.
  it('a stale Add success after the target changed keeps the new target and bits', async () => {
    const add = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(add.promise);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const targetSelect = screen.getAllByRole('combobox')[1] as HTMLSelectElement;
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    fireEvent.click(screen.getByTestId('set-allow'));

    act(() => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    expect(onUpsert).toHaveBeenCalledWith({
      target_type: 'role',
      target_id: 'role-1',
      allow: '1',
      deny: '0',
    });

    // The user changes the target and bits while the add is still in flight.
    fireEvent.change(targetSelect, { target: { value: 'role-2' } });
    fireEvent.click(screen.getByTestId('set-deny'));

    await act(async () => {
      add.resolve(true);
    });

    expect(targetSelect.value).toBe('role-2');
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-allow', '1');
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-deny', '2');
  });

  // Busy state (#3406 finding 2): each write button disables only for its
  // own write, and re-enables once that write settles.
  // Deletes on different rows may overlap (each row's button only blocks its
  // own row), so one row's delete must not supersede another row's failure.
  it("a delete failure still shows when another row's delete started meanwhile", async () => {
    const first = deferred<boolean>();
    const second = deferred<boolean>();
    const onDelete = vi
      .fn()
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);
    render(
      <OverridePanel
        {...defaultProps}
        onDelete={onDelete}
        overrides={[mockRoleOverride, mockUserOverride]}
      />
    );
    const [deleteA, deleteB] = screen.getAllByRole('button', { name: 'Delete override' });
    fireEvent.click(deleteA);
    fireEvent.click(deleteB);

    await act(async () => {
      first.resolve(false);
    });
    await act(async () => {
      second.resolve(true);
    });

    expect(screen.getByRole('alert')).toHaveTextContent('Failed to remove permission override.');
  });

  it('disables the triggering button while its write is pending, then re-enables it', async () => {
    const save = deferred<boolean>();
    const add = deferred<boolean>();
    const del = deferred<boolean>();
    const onUpsert = vi
      .fn()
      .mockImplementationOnce(() => save.promise)
      .mockImplementationOnce(() => add.promise);
    const onDelete = vi.fn().mockReturnValue(del.promise);

    render(
      <OverridePanel
        {...defaultProps}
        onUpsert={onUpsert}
        onDelete={onDelete}
        overrides={[mockRoleOverride]}
      />
    );

    clickOverrideItem('Moderator');
    const saveButton = screen.getByText('Save Override');
    act(() => {
      fireEvent.click(saveButton);
    });
    expect(saveButton).toBeDisabled();
    await act(async () => {
      save.resolve(false);
    });
    expect(saveButton).not.toBeDisabled();

    fireEvent.click(screen.getByText('Cancel'));

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    const addButton = screen.getByRole('button', { name: 'Add Override' });
    act(() => {
      fireEvent.click(addButton);
    });
    expect(addButton).toBeDisabled();
    await act(async () => {
      add.resolve(false);
    });
    expect(addButton).not.toBeDisabled();

    const deleteButton = screen.getByLabelText('Delete override');
    act(() => {
      fireEvent.click(deleteButton);
    });
    expect(deleteButton).toBeDisabled();
    await act(async () => {
      del.resolve(false);
    });
    expect(deleteButton).not.toBeDisabled();
  });
});

// Codex's third round on #3406 found two more races, both caused by the editor
// staying interactive while a write was in flight. The editor now takes one
// write at a time: the surfaces that write would read are read-only until it
// settles, and Cancel stays live so a hung request cannot trap the user.
describe('OverridePanel — one write at a time (#3406)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('keeps the edit grid read-only while its save is in flight, so no edit can be lost', async () => {
    const save = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(save.promise);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);

    selectOverrideByName('Moderator');
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-disabled', 'false');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-disabled', 'true');

    await act(async () => {
      save.resolve(false);
    });

    // A refusal hands the grid back, with the draft the user was saving.
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-disabled', 'false');
    expect(screen.getByRole('alert')).toBeInTheDocument();
  });

  it('keeps the override list closed while an add is in flight, so its failure stays visible', async () => {
    const add = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(add.promise);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);

    fireEvent.change(screen.getAllByRole('combobox')[1], { target: { value: 'role-2' } });
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    selectOverrideByName('Moderator');
    expect(screen.queryByText('Editing: Moderator')).not.toBeInTheDocument();

    await act(async () => {
      add.resolve(false);
    });

    expect(screen.getByRole('alert')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add Override' })).toBeInTheDocument();
  });

  it('locks the add fields while an add is in flight and releases them when it settles', async () => {
    const add = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(add.promise);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} />);

    const [typeSelect, targetSelect] = screen.getAllByRole('combobox');
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    expect(typeSelect).toBeDisabled();
    expect(targetSelect).toBeDisabled();
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-disabled', 'true');

    await act(async () => {
      add.resolve(false);
    });

    expect(typeSelect).not.toBeDisabled();
    expect(targetSelect).not.toBeDisabled();
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-disabled', 'false');
  });

  // Round 5: Cancel closes the editor but cannot recall its PUT, so no second
  // save can start until the abandoned one settles. The next save then holds
  // its own lock for as long as it is in flight.
  it('starts no newer save until an abandoned save settles, then locks the newer one', async () => {
    const first = deferred<boolean>();
    const second = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    fireEvent.click(screen.getByText('Cancel'));
    selectOverrideByName('Moderator');
    expect(screen.queryByText('Save Override')).not.toBeInTheDocument();
    expect(onUpsert).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.resolve(true);
    });

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(onUpsert).toHaveBeenCalledTimes(2);
    expect(screen.getByText('Save Override')).toBeDisabled();
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-disabled', 'true');
    expect(screen.getByText('Editing: Moderator')).toBeInTheDocument();
  });
});

// Codex's fourth round on #3406: the one-write lock covered selection and the
// grids, but a Delete could still race a pending Save or Add on the same row,
// and a Save or Add could still race a pending Delete. Any two writes that can
// touch the same override now exclude each other; deletes of different rows
// may still overlap.
describe('OverridePanel — writes exclude each other (#3406)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('keeps every Delete button disabled while a save is in flight', async () => {
    const save = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(save.promise);
    render(
      <OverridePanel
        {...defaultProps}
        onUpsert={onUpsert}
        overrides={[mockRoleOverride, mockUserOverride]}
      />
    );

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    for (const btn of screen.getAllByLabelText('Delete override')) expect(btn).toBeDisabled();

    await act(async () => {
      save.resolve(false);
    });
    for (const btn of screen.getAllByLabelText('Delete override')) expect(btn).not.toBeDisabled();
  });

  it('keeps every Delete button disabled while an add is in flight', async () => {
    const add = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(add.promise);
    render(<OverridePanel {...defaultProps} onUpsert={onUpsert} overrides={[mockRoleOverride]} />);

    fireEvent.change(screen.getAllByRole('combobox')[1], { target: { value: 'role-2' } });
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: 'Add Override' }));
    });

    expect(screen.getByLabelText('Delete override')).toBeDisabled();

    await act(async () => {
      add.resolve(true);
    });
    expect(screen.getByLabelText('Delete override')).not.toBeDisabled();
  });

  it('keeps Save and the list closed while a delete is in flight', async () => {
    const del = deferred<boolean>();
    const onDelete = vi.fn().mockReturnValue(del.promise);
    render(
      <OverridePanel
        {...defaultProps}
        onDelete={onDelete}
        overrides={[mockRoleOverride, mockUserOverride]}
      />
    );

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getAllByLabelText('Delete override')[0]);
    });

    expect(screen.getByText('Save Override')).toBeDisabled();
    const rows = document.querySelectorAll<HTMLButtonElement>('.override-item-select');
    for (const row of rows) expect(row).toBeDisabled();

    await act(async () => {
      del.resolve(false);
    });
    expect(screen.getByText('Save Override')).not.toBeDisabled();
  });

  it('keeps Add disabled while a delete is in flight', async () => {
    const del = deferred<boolean>();
    const onDelete = vi.fn().mockReturnValue(del.promise);
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);

    fireEvent.change(screen.getAllByRole('combobox')[1], { target: { value: 'role-2' } });
    act(() => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });

    expect(screen.getByRole('button', { name: 'Add Override' })).toBeDisabled();

    await act(async () => {
      del.resolve(true);
    });
    expect(screen.getByRole('button', { name: 'Add Override' })).not.toBeDisabled();
  });
});

describe('OverridePanel — parent write coordination (#3406)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('tells the parent when a save starts and when it settles', async () => {
    const save = deferred<boolean>();
    const onWritePendingChange = vi.fn();
    render(
      <OverridePanel
        {...defaultProps}
        onUpsert={vi.fn().mockReturnValue(save.promise)}
        onWritePendingChange={onWritePendingChange}
        overrides={[mockRoleOverride]}
      />
    );
    expect(onWritePendingChange).toHaveBeenLastCalledWith(false);

    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(onWritePendingChange).toHaveBeenLastCalledWith(true);

    await act(async () => {
      save.resolve(false);
    });
    expect(onWritePendingChange).toHaveBeenLastCalledWith(false);
  });

  it('tells the parent while a delete is in flight', async () => {
    const del = deferred<boolean>();
    const onWritePendingChange = vi.fn();
    render(
      <OverridePanel
        {...defaultProps}
        onDelete={vi.fn().mockReturnValue(del.promise)}
        onWritePendingChange={onWritePendingChange}
        overrides={[mockRoleOverride]}
      />
    );

    act(() => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });
    expect(onWritePendingChange).toHaveBeenLastCalledWith(true);

    await act(async () => {
      del.resolve(true);
    });
    expect(onWritePendingChange).toHaveBeenLastCalledWith(false);
  });

  it('accepts no write while locked, and every write once unlocked', () => {
    const { rerender } = render(
      <OverridePanel {...defaultProps} locked overrides={[mockRoleOverride]} />
    );
    fireEvent.change(screen.getAllByRole('combobox')[1], { target: { value: 'role-2' } });

    expect(screen.getByLabelText('Delete override')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add Override' })).toBeDisabled();
    expect(document.querySelector('.override-item-select')).toBeDisabled();

    rerender(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    expect(screen.getByLabelText('Delete override')).not.toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add Override' })).not.toBeDisabled();
    expect(document.querySelector('.override-item-select')).not.toBeDisabled();
  });
});

// Codex's fifth round on #3406. Two races the earlier rounds left open:
// Cancel released the write lock while its Save was still in flight, and a
// Save whose row vanished from the `overrides` prop left Add unlocked with the
// failure rendered nowhere. Neighbouring blocks are top-level siblings that
// each own their `beforeEach`, so this one does too.
describe('OverridePanel — #3406 Codex round 5', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // The list-level slot a vanished row's refusal uses: no editor holds the
  // changes any more, so it must not claim they are still here.
  const VANISHED_SAVE_ERROR = 'Failed to save permission override. It is no longer in this list.';

  const deleteButtons = () => screen.getAllByLabelText('Delete override') as HTMLButtonElement[];
  const selectionButtons = () =>
    Array.from(document.querySelectorAll<HTMLButtonElement>('.override-item-select'));

  // R1 setup: Save clicked with onUpsert pending, then Cancel. A target is
  // chosen on the add form first so the Add button has a target once Cancel
  // brings the add section back.
  function renderWithAbandonedSave() {
    const save = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(save.promise);
    const onWritePendingChange = vi.fn();
    render(
      <OverridePanel
        {...defaultProps}
        onUpsert={onUpsert}
        onWritePendingChange={onWritePendingChange}
        overrides={[mockRoleOverride, mockUserOverride]}
      />
    );
    fireEvent.change(screen.getAllByRole('combobox')[1], { target: { value: 'role-2' } });
    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(onUpsert, 'harness: the save reached onUpsert').toHaveBeenCalledTimes(1);
    return { save, onWritePendingChange };
  }

  it('R1: Cancel keeps every Delete override button disabled until the save settles', async () => {
    const { save } = renderWithAbandonedSave();

    // Positive control: the lock is up before Cancel.
    for (const btn of deleteButtons()) {
      expect(btn.disabled, 'control: Delete disabled while Save pending, before Cancel').toBe(true);
    }

    fireEvent.click(screen.getByText('Cancel'));
    for (const btn of deleteButtons()) {
      expect(btn.disabled, 'Delete override disabled after Cancel, save still pending').toBe(true);
    }

    await act(async () => {
      save.resolve(true);
    });
    for (const btn of deleteButtons()) {
      expect(btn.disabled, 'Delete override re-enabled once the save settled').toBe(false);
    }
  });

  it('R1: Cancel keeps every override selection button disabled until the save settles', async () => {
    const { save } = renderWithAbandonedSave();

    // Positive control: the lock is up before Cancel.
    for (const btn of selectionButtons()) {
      expect(btn.disabled, 'control: selection disabled while Save pending, before Cancel').toBe(
        true
      );
    }

    fireEvent.click(screen.getByText('Cancel'));
    expect(selectionButtons()).toHaveLength(2);
    for (const btn of selectionButtons()) {
      expect(btn.disabled, 'selection button disabled after Cancel, save still pending').toBe(true);
    }

    await act(async () => {
      save.resolve(true);
    });
    for (const btn of selectionButtons()) {
      expect(btn.disabled, 'selection button re-enabled once the save settled').toBe(false);
    }
  });

  it('R1: Cancel keeps the Add Override button disabled until the save settles', async () => {
    const { save } = renderWithAbandonedSave();

    // Control: Add is not rendered while the editor is open, so its lock cannot
    // be observed before Cancel. The target chosen earlier must survive Cancel,
    // so the post-settle re-enable below proves the button is target-ready and
    // that the disabled state in between comes from the write lock alone.
    expect(screen.queryByRole('button', { name: 'Add Override' })).not.toBeInTheDocument();

    fireEvent.click(screen.getByText('Cancel'));
    expect(
      (screen.getAllByRole('combobox')[1] as HTMLSelectElement).value,
      'control: add target is still chosen after Cancel'
    ).toBe('role-2');
    expect(
      (screen.getByRole('button', { name: 'Add Override' }) as HTMLButtonElement).disabled,
      'Add Override disabled after Cancel, save still pending'
    ).toBe(true);

    await act(async () => {
      save.resolve(true);
    });
    expect(
      (screen.getByRole('button', { name: 'Add Override' }) as HTMLButtonElement).disabled,
      'Add Override re-enabled once the save settled'
    ).toBe(false);
  });

  it('R1: Cancel keeps onWritePendingChange true until the save settles', async () => {
    const { save, onWritePendingChange } = renderWithAbandonedSave();

    // Positive control: the parent was told about the pending save.
    expect(
      onWritePendingChange,
      'control: pending reported before Cancel'
    ).toHaveBeenLastCalledWith(true);

    fireEvent.click(screen.getByText('Cancel'));
    expect(
      onWritePendingChange.mock.calls.at(-1),
      'last onWritePendingChange after Cancel, save still pending'
    ).toEqual([true]);

    await act(async () => {
      save.resolve(true);
    });
    expect(
      onWritePendingChange.mock.calls.at(-1),
      'last onWritePendingChange once the save settled'
    ).toEqual([false]);
  });

  // R2 setup: a target is chosen on the add form, a Save is left pending, and
  // the `overrides` prop re-renders without the saved row (an external refresh).
  function renderWithSavedRowRemoved() {
    const save = deferred<boolean>();
    const onUpsert = vi.fn().mockReturnValue(save.promise);
    const props = { ...defaultProps, onUpsert };
    const { rerender } = render(<OverridePanel {...props} overrides={[mockRoleOverride]} />);
    fireEvent.change(screen.getAllByRole('combobox')[1], { target: { value: 'role-2' } });
    selectOverrideByName('Moderator');
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    expect(onUpsert, 'harness: the save reached onUpsert').toHaveBeenCalledTimes(1);

    rerender(<OverridePanel {...props} overrides={[mockUserOverride]} />);
    // Positive control: the Add section IS rendered, so a missing section
    // cannot pass for disabled.
    expect(screen.getByText('Add Override', { selector: '.section-header' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add Override' })).toBeInTheDocument();
    return { save };
  }

  it('R2a: a pending Save disables the Add button after its row leaves the overrides prop', () => {
    renderWithSavedRowRemoved();

    expect(
      (screen.getByRole('button', { name: 'Add Override' }) as HTMLButtonElement).disabled,
      'Add Override disabled while a Save is pending'
    ).toBe(true);
  });

  it('R2a: a pending Save disables both add selects after its row leaves the overrides prop', () => {
    renderWithSavedRowRemoved();

    expect(
      (screen.getByLabelText('Override target type') as HTMLSelectElement).disabled,
      'Override target type select disabled while a Save is pending'
    ).toBe(true);
    expect(
      (screen.getByLabelText('Override target') as HTMLSelectElement).disabled,
      'Override target select disabled while a Save is pending'
    ).toBe(true);
  });

  it('R2a: a pending Save disables the add PermissionGrid after its row leaves the overrides prop', () => {
    renderWithSavedRowRemoved();

    expect(
      screen.getByTestId('permission-grid').getAttribute('data-disabled'),
      'add PermissionGrid disabled while a Save is pending'
    ).toBe('true');
  });

  it('R2b: a Save refused after its row left the overrides prop still shows the save alert', async () => {
    const { save } = renderWithSavedRowRemoved();

    await act(async () => {
      save.resolve(false);
    });

    expect(
      screen.queryByRole('alert'),
      'role=alert with the save-failure message is in the document'
    ).not.toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent(VANISHED_SAVE_ERROR);
  });
});

// Codex's seventh round on #3406: the legacy numeric Administrator decode is
// sound only for effective permissions, where the Administrator bit subsumes
// every other. An override mask is a literal set of bits. A pre-#3406 server
// sends allow/deny as JSON numbers, and one above 2^53 cannot be trusted to
// be exact, so a mask such as 2^62 + 1024 must not load into the editor, or be
// counted, as the Administrator bit alone: the low deny bit would silently
// vanish from what the moderator sees and would save.
describe('OverridePanel — a numeric override mask above 2^53 is never read as Administrator (#3406)', () => {
  const legacyMask = 4611686018427388928; // 2^62 + 1024, exactly representable
  const legacyOverride = {
    ...mockRoleOverride,
    allow: legacyMask as unknown as string,
    deny: legacyMask as unknown as string,
  };

  // The editor no longer loads such a mask at all (see the describe below), so
  // it cannot load it as the Administrator bit either.
  it('does not load the mask into the editor as the Administrator bit', () => {
    render(<OverridePanel {...defaultProps} overrides={[legacyOverride]} />);
    selectOverrideByName('Moderator');

    expect(screen.queryByTestId('permission-grid')).not.toBeInTheDocument();
    expect(screen.getByText('Save Override')).toBeDisabled();
  });

  it('does not count the mask as one allowed and one denied bit', () => {
    render(<OverridePanel {...defaultProps} overrides={[legacyOverride]} />);

    expect(screen.queryByText('1 allowed')).not.toBeInTheDocument();
    expect(screen.queryByText('1 denied')).not.toBeInTheDocument();
  });

  it('still decodes a safe numeric mask exactly', () => {
    const safe = {
      ...mockRoleOverride,
      allow: 1024 as unknown as string,
      deny: 3 as unknown as string,
    };
    render(<OverridePanel {...defaultProps} overrides={[safe]} />);
    expect(screen.getByText('1 allowed')).toBeInTheDocument();
    expect(screen.getByText('2 denied')).toBeInTheDocument();
    selectOverrideByName('Moderator');

    const grid = screen.getByTestId('permission-grid');
    expect(grid).toHaveAttribute('data-allow', '1024');
    expect(grid).toHaveAttribute('data-deny', '3');
  });
});

// Codex's seventh round on #3406, second half: decoding an unreadable mask to
// no bits keeps the Administrator bit out of the editor, but the editor then
// shows an empty mask and a Save writes it back, erasing every bit the override
// held, including its denies. A mask the desktop cannot read exactly must not
// be editable at all: the grid gives way to a notice, Save stays disabled, and
// Delete, which needs no bits, stays available.
describe('OverridePanel — an override whose mask cannot be read exactly is not editable (#3406)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  const unreadable = {
    ...mockRoleOverride,
    allow: '1024',
    deny: 4611686018427388928 as unknown as string, // 2^62 + 1024 from a pre-#3406 server
  };

  it('shows a notice instead of the grid and keeps Save disabled', () => {
    render(<OverridePanel {...defaultProps} overrides={[unreadable]} />);
    selectOverrideByName('Moderator');

    expect(screen.queryByTestId('permission-grid')).not.toBeInTheDocument();
    expect(screen.getByText(/can't be shown exactly/i)).toBeInTheDocument();
    const save = screen.getByText('Save Override');
    expect(save).toBeDisabled();
    fireEvent.click(save);
    expect(defaultProps.onUpsert).not.toHaveBeenCalled();
  });

  it('treats an unreadable allow mask the same way', () => {
    const unreadableAllow = { ...unreadable, allow: unreadable.deny, deny: '2' };
    render(<OverridePanel {...defaultProps} overrides={[unreadableAllow]} />);
    selectOverrideByName('Moderator');

    expect(screen.queryByTestId('permission-grid')).not.toBeInTheDocument();
    expect(screen.getByText('Save Override')).toBeDisabled();
  });

  it('still lets the override be deleted', () => {
    render(<OverridePanel {...defaultProps} overrides={[unreadable]} />);
    selectOverrideByName('Moderator');

    const del = screen.getByLabelText('Delete override');
    expect(del).not.toBeDisabled();
    fireEvent.click(del);
    expect(defaultProps.onDelete).toHaveBeenCalledWith('override-1');
  });

  it('marks the row instead of counting only the bits it could read', () => {
    render(<OverridePanel {...defaultProps} overrides={[unreadable]} />);

    // allow reads as one bit, but the deny is unknown: a bare "1 allowed"
    // would present the override as denying nothing.
    expect(screen.queryByText('1 allowed')).not.toBeInTheDocument();
    expect(screen.getByText("Can't be shown")).toBeInTheDocument();
  });

  // Codex's eighth round: a modal opens on cached overrides and refreshes them.
  // Selected while unreadable, the draft held 0n for the unknown deny; when the
  // refresh then delivered the same override exactly, Save came back with that
  // 0n draft, and saving erased the deny the override held.
  it('reloads the draft when a refresh makes the selected override readable', () => {
    const view = render(<OverridePanel {...defaultProps} overrides={[unreadable]} />);
    selectOverrideByName('Moderator');
    expect(screen.getByText('Save Override')).toBeDisabled();

    const refreshed = { ...unreadable, allow: '1024', deny: '2' };
    view.rerender(<OverridePanel {...defaultProps} overrides={[refreshed]} />);

    const grid = screen.getByTestId('permission-grid');
    expect(grid).toHaveAttribute('data-allow', '1024');
    expect(grid).toHaveAttribute('data-deny', '2');
    expect(screen.getByText('Save Override')).not.toBeDisabled();
  });

  // The other direction: a draft from a readable override is the moderator's
  // work, so a refresh of that override must not reload it.
  it("keeps a readable override's edits across a refresh", () => {
    const readable = { ...mockRoleOverride, allow: '4', deny: '0' };
    const view = render(<OverridePanel {...defaultProps} overrides={[readable]} />);
    selectOverrideByName('Moderator');
    fireEvent.click(screen.getByTestId('set-allow'));
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-allow', '5');

    const refreshed = { ...readable, deny: '8', updated_at: '2025-01-02T00:00:00Z' };
    view.rerender(<OverridePanel {...defaultProps} overrides={[refreshed]} />);

    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-allow', '5');
    expect(screen.getByTestId('permission-grid')).toHaveAttribute('data-deny', '0');
  });

  it('keeps an exactly readable override editable', () => {
    render(<OverridePanel {...defaultProps} overrides={[mockRoleOverride]} />);
    selectOverrideByName('Moderator');

    expect(screen.getByTestId('permission-grid')).toBeInTheDocument();
    expect(screen.queryByText(/can't be shown exactly/i)).not.toBeInTheDocument();
    expect(screen.getByText('Save Override')).not.toBeDisabled();
    expect(screen.queryByText("Can't be shown")).not.toBeInTheDocument();
    expect(screen.getByText('1 allowed')).toBeInTheDocument();
  });
});
