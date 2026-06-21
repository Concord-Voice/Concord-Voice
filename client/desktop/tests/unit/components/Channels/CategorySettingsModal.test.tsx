import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePermissionStore, type ChannelOverride } from '@/renderer/stores/permissionStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import type { ChannelGroup } from '@/renderer/types/chat';
import type { Role } from '@/renderer/types/server';

// Mock PermissionGrid since it has complex internal logic not relevant to this test
vi.mock('@/renderer/components/Permissions/PermissionGrid', () => ({
  default: ({
    value,
    onChange,
    deny,
    onDenyChange,
    mode,
  }: {
    value: bigint;
    onChange: (v: bigint) => void;
    deny: bigint;
    onDenyChange?: (v: bigint) => void;
    mode: string;
  }) => (
    <div data-testid="permission-grid" data-mode={mode}>
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

import CategorySettingsModal from '@/renderer/components/Channels/CategorySettingsModal';

const mockCategory: ChannelGroup = {
  id: 'cat-1',
  server_id: 'server-1',
  name: 'General',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

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

const mockRole2: Role = {
  ...mockRole,
  id: 'role-2',
  name: 'Admin',
  position: 2,
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

/** Click the override-item-select button that contains the given target name */
function clickOverrideItem(name: string) {
  const el = document.querySelector('.override-target-name');
  expect(el).toHaveTextContent(name);
  const btn = el!.closest('.override-item')!.querySelector('.override-item-select')!;
  fireEvent.click(btn);
}

describe('CategorySettingsModal', () => {
  const mockOnClose = vi.fn();
  const mockFetchCategoryOverrides = vi.fn();
  const mockFetchRoles = vi.fn();
  const mockUpsertCategoryOverride = vi.fn().mockResolvedValue(true);
  const mockDeleteCategoryOverride = vi.fn().mockResolvedValue(true);

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();

    usePermissionStore.setState({
      fetchCategoryOverrides: mockFetchCategoryOverrides,
      fetchRoles: mockFetchRoles,
      upsertCategoryOverride: mockUpsertCategoryOverride,
      deleteCategoryOverride: mockDeleteCategoryOverride,
      serverRoles: { 'server-1': [mockRole, mockRole2] },
      channelOverrides: {},
    });

    useMemberStore.setState({
      members: [
        {
          user_id: 'user-1',
          username: 'testuser',
          display_name: 'Test User',
          role: 'member' as const,
          joined_at: '2025-01-01T00:00:00Z',
          roles: [],
        },
        {
          user_id: 'user-2',
          username: 'anotheruser',
          display_name: 'Another User',
          role: 'member' as const,
          joined_at: '2025-01-01T00:00:00Z',
          roles: [],
        },
      ],
    });
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <CategorySettingsModal
        isOpen={false}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders modal title with category name', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(screen.getByText(/Category Permissions.*General/)).toBeInTheDocument();
  });

  it('fetches category overrides and roles when opened', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(mockFetchCategoryOverrides).toHaveBeenCalledWith('cat-1');
    expect(mockFetchRoles).toHaveBeenCalledWith('server-1');
  });

  it('shows no overrides message when empty', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(
      screen.getByText(/No permission overrides configured for this category/)
    ).toBeInTheDocument();
  });

  it('displays role overrides when present', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Role Overrides')).toBeInTheDocument();
    const overrideName = document.querySelector('.override-target-name');
    expect(overrideName).toHaveTextContent('Moderator');
  });

  it('displays user overrides when present', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockUserOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('User Overrides')).toBeInTheDocument();
    const overrideName = document.querySelector('.override-target-name');
    expect(overrideName).toHaveTextContent('Test User');
  });

  it('displays both role and user overrides', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride, mockUserOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Role Overrides')).toBeInTheDocument();
    expect(screen.getByText('User Overrides')).toBeInTheDocument();
  });

  it('shows allow/deny counts in override summary', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('1 allowed')).toBeInTheDocument();
    expect(screen.getByText('1 denied')).toBeInTheDocument();
  });

  it('shows Unknown Role for role override with missing role', () => {
    const unknownRoleOverride: ChannelOverride = {
      ...mockRoleOverride,
      target_id: 'non-existent-role',
    };
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [unknownRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Unknown Role')).toBeInTheDocument();
  });

  it('shows Unknown User for user override with missing member', () => {
    const unknownUserOverride: ChannelOverride = {
      ...mockUserOverride,
      target_id: 'non-existent-user',
    };
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [unknownUserOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Unknown User')).toBeInTheDocument();
  });

  it('shows username when display_name is not set', () => {
    useMemberStore.setState({
      members: [
        {
          user_id: 'user-1',
          username: 'rawusername',
          role: 'member' as const,
          joined_at: '2025-01-01T00:00:00Z',
          roles: [],
        },
      ],
    });
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockUserOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const overrideName = document.querySelector('.override-target-name');
    expect(overrideName).toHaveTextContent('rawusername');
  });

  it('selects an override and shows the PermissionGrid editor', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');

    expect(screen.getByText(/Editing: Moderator/)).toBeInTheDocument();
    expect(screen.getByText('Save Override')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
    expect(screen.getByTestId('permission-grid')).toBeInTheDocument();
  });

  it('saves override when Save Override is clicked', async () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');

    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    // handleSelectOverride populates editAllow/editDeny from the override's existing values
    expect(mockUpsertCategoryOverride).toHaveBeenCalledWith('cat-1', {
      target_type: 'role',
      target_id: 'role-1',
      allow: '1',
      deny: '2',
    });
  });

  it('deselects override when Cancel is clicked in editor', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');
    expect(screen.getByText(/Editing: Moderator/)).toBeInTheDocument();

    fireEvent.click(screen.getByText('Cancel'));
    expect(screen.queryByText(/Editing: Moderator/)).not.toBeInTheDocument();
  });

  it('deletes an override when delete button is clicked', async () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const deleteBtn = screen.getByLabelText('Delete override');

    await act(async () => {
      fireEvent.click(deleteBtn);
    });

    expect(mockDeleteCategoryOverride).toHaveBeenCalledWith('cat-1', 'override-1');
  });

  it('clears selection when the selected override is deleted', async () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');
    expect(screen.getByText(/Editing: Moderator/)).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });

    expect(mockDeleteCategoryOverride).toHaveBeenCalledWith('cat-1', 'override-1');
  });

  // --- Add Override Section ---

  it('shows Add Override section when no override is selected', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const addSection = document.querySelector('.add-override-section');
    expect(addSection).toBeInTheDocument();
    const addBtn = addSection!.querySelector('button.add-override-btn');
    expect(addBtn).toBeDisabled();
  });

  it('hides Add Override section when an override is selected', () => {
    usePermissionStore.setState({
      channelOverrides: { 'category:cat-1': [mockRoleOverride] },
    });

    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');
    expect(document.querySelector('.add-override-section')).not.toBeInTheDocument();
  });

  it('shows role options in target dropdown', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Select a role...')).toBeInTheDocument();
    expect(screen.getByText('Moderator', { selector: 'option' })).toBeInTheDocument();
    expect(screen.getByText('Admin', { selector: 'option' })).toBeInTheDocument();
  });

  it('switches to user options when target type changed', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const typeSelect = screen.getAllByRole('combobox')[0];
    fireEvent.change(typeSelect, { target: { value: 'user' } });

    expect(screen.getByText('Select a user...')).toBeInTheDocument();
    expect(screen.getByText('Test User', { selector: 'option' })).toBeInTheDocument();
    expect(screen.getByText('Another User', { selector: 'option' })).toBeInTheDocument();
  });

  it('enables Add Override button when a target is selected', () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const addBtn = document.querySelector('.add-override-section button.add-override-btn');
    expect(addBtn).toBeDisabled();

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });

    expect(addBtn).not.toBeDisabled();
  });

  it('adds an override when Add Override button is clicked', async () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });

    const addBtn = document.querySelector('.add-override-section button.add-override-btn')!;
    await act(async () => {
      fireEvent.click(addBtn);
    });

    expect(mockUpsertCategoryOverride).toHaveBeenCalledWith('cat-1', {
      target_type: 'role',
      target_id: 'role-1',
      allow: '0',
      deny: '0',
    });
  });

  it('does not call upsert when no target is selected', async () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const addBtn = document.querySelector('.add-override-section button.add-override-btn')!;
    await act(async () => {
      fireEvent.click(addBtn);
    });

    expect(mockUpsertCategoryOverride).not.toHaveBeenCalled();
  });

  it('resets add form after successful add', async () => {
    render(
      <CategorySettingsModal
        isOpen={true}
        category={mockCategory}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const targetSelect = screen.getAllByRole('combobox')[1];
    fireEvent.change(targetSelect, { target: { value: 'role-1' } });

    const addBtn = document.querySelector('.add-override-section button.add-override-btn')!;
    await act(async () => {
      fireEvent.click(addBtn);
    });

    expect(targetSelect).toHaveValue('');
  });
});
