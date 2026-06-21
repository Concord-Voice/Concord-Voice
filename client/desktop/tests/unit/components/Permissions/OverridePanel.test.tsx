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
    <div data-testid="permission-grid" data-mode={mode} data-disabled={isDisabled}>
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
import { ChannelOverride } from '@/renderer/stores/permissionStore';
import { Role } from '@/renderer/types/server';
import { ServerMember } from '@/renderer/stores/memberStore';

// --- Mock data ---

const mockRole: Role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  color: '#ff0000',
  position: 1,
  permissions: '0',
  is_default: false,
  display_separately: false,
  mentionable: false,
  require_mfa: false,
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

const defaultProps = {
  overrides: [] as ChannelOverride[],
  roles: [mockRole, mockRole2],
  members: [mockMember],
  onUpsert: vi.fn().mockResolvedValue(undefined),
  onDelete: vi.fn().mockResolvedValue(undefined),
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
    const onUpsert = vi.fn().mockResolvedValue(undefined);
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

  // 12. Delete calls onDelete
  it('delete calls onDelete', async () => {
    const onDelete = vi.fn().mockResolvedValue(undefined);
    render(<OverridePanel {...defaultProps} onDelete={onDelete} overrides={[mockRoleOverride]} />);
    const deleteBtn = screen.getByLabelText('Delete override');
    await act(async () => {
      fireEvent.click(deleteBtn);
    });
    expect(onDelete).toHaveBeenCalledWith('override-1');
  });

  // 13. Clears selection when selected override is deleted
  it('clears selection when selected override is deleted', async () => {
    const onDelete = vi.fn().mockResolvedValue(undefined);
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
    const onUpsert = vi.fn().mockResolvedValue(undefined);
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
    const onUpsert = vi.fn().mockResolvedValue(undefined);
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
    const onUpsert = vi.fn().mockResolvedValue(undefined);
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
