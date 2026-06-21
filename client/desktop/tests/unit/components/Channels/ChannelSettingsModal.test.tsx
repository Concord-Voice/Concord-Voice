import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePermissionStore, type ChannelOverride } from '@/renderer/stores/permissionStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import type { Channel } from '@/renderer/types/chat';
import type { Role } from '@/renderer/types/server';

// Mock PermissionGrid since it has complex internal logic not relevant to this test
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

import ChannelSettingsModal from '@/renderer/components/Channels/ChannelSettingsModal';

const mockChannel: Channel = {
  id: 'channel-1',
  server_id: 'server-1',
  name: 'general',
  type: 'text',
  position: 0,
  group_id: 'group-1',
  sync_permissions: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockChannelNoGroup: Channel = {
  ...mockChannel,
  id: 'channel-nogp',
  group_id: undefined,
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
  channel_id: 'channel-1',
  target_type: 'role',
  target_id: 'role-1',
  allow: '1',
  deny: '2',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const mockUserOverride: ChannelOverride = {
  id: 'override-2',
  channel_id: 'channel-1',
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

describe('ChannelSettingsModal', () => {
  const mockOnClose = vi.fn();
  const mockFetchChannelOverrides = vi.fn();
  const mockFetchRoles = vi.fn();
  const mockUpsertChannelOverride = vi.fn().mockResolvedValue(true);
  const mockDeleteChannelOverride = vi.fn().mockResolvedValue(true);
  const mockSetCategorySync = vi.fn().mockResolvedValue(true);

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();

    usePermissionStore.setState({
      fetchChannelOverrides: mockFetchChannelOverrides,
      fetchRoles: mockFetchRoles,
      upsertChannelOverride: mockUpsertChannelOverride,
      deleteChannelOverride: mockDeleteChannelOverride,
      setCategorySync: mockSetCategorySync,
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
      <ChannelSettingsModal
        isOpen={false}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders modal title with channel name', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(screen.getByText(/Channel Permissions.*#general/)).toBeInTheDocument();
  });

  it('fetches channel overrides and roles when opened', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(mockFetchChannelOverrides).toHaveBeenCalledWith('channel-1');
    expect(mockFetchRoles).toHaveBeenCalledWith('server-1');
  });

  it('shows no overrides message when empty', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );
    expect(
      screen.getByText(/No permission overrides configured for this channel/)
    ).toBeInTheDocument();
  });

  // --- Override display ---

  it('displays role overrides when present', () => {
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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
      channelOverrides: { 'channel-1': [mockUserOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('User Overrides')).toBeInTheDocument();
    const overrideName = document.querySelector('.override-target-name');
    expect(overrideName).toHaveTextContent('Test User');
  });

  it('shows allow/deny counts in override summary', () => {
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('1 allowed')).toBeInTheDocument();
    expect(screen.getByText('1 denied')).toBeInTheDocument();
  });

  it('shows Unknown Role for missing role target', () => {
    const unknownRoleOverride: ChannelOverride = {
      ...mockRoleOverride,
      target_id: 'non-existent-role',
    };
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [unknownRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Unknown Role')).toBeInTheDocument();
  });

  it('shows Unknown User for missing user target', () => {
    const unknownUserOverride: ChannelOverride = {
      ...mockUserOverride,
      target_id: 'non-existent-user',
    };
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [unknownUserOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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
      channelOverrides: { 'channel-1': [mockUserOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const overrideName = document.querySelector('.override-target-name');
    expect(overrideName).toHaveTextContent('rawusername');
  });

  // --- Override editing ---

  it('selects an override and shows editor', () => {
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');
    expect(screen.getByText(/Editing: Moderator/)).toBeInTheDocument();
    expect(screen.getByText('Save Override')).toBeInTheDocument();
    expect(screen.getByTestId('permission-grid')).toBeInTheDocument();
  });

  it('saves override when Save Override is clicked', async () => {
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    clickOverrideItem('Moderator');

    await act(async () => {
      fireEvent.click(screen.getByText('Save Override'));
    });

    // handleSelectOverride populates editAllow/editDeny from the override's existing values
    expect(mockUpsertChannelOverride).toHaveBeenCalledWith('channel-1', {
      target_type: 'role',
      target_id: 'role-1',
      allow: '1',
      deny: '2',
    });
  });

  it('deselects override when Cancel is clicked in editor', () => {
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    await act(async () => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });

    expect(mockDeleteChannelOverride).toHaveBeenCalledWith('channel-1', 'override-1');
  });

  // --- Add Override Section ---

  it('shows Add Override section when no override is selected and not synced', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const addSection = document.querySelector('.add-override-section');
    expect(addSection).toBeInTheDocument();
    const addBtn = addSection!.querySelector('button.add-override-btn');
    expect(addBtn).toBeDisabled();
  });

  it('shows role options in target dropdown', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const typeSelect = screen.getAllByRole('combobox')[0];
    fireEvent.change(typeSelect, { target: { value: 'user' } });

    expect(screen.getByText('Select a user...')).toBeInTheDocument();
    expect(screen.getByText('Test User', { selector: 'option' })).toBeInTheDocument();
  });

  it('enables Add Override button when target selected', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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

  it('adds an override when Add Override is clicked', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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

    expect(mockUpsertChannelOverride).toHaveBeenCalledWith('channel-1', {
      target_type: 'role',
      target_id: 'role-1',
      allow: '0',
      deny: '0',
    });
  });

  it('resets add form after adding', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
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

  it('does not call upsert when no target is selected', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const addBtn = document.querySelector('.add-override-section button.add-override-btn')!;
    await act(async () => {
      fireEvent.click(addBtn);
    });

    expect(mockUpsertChannelOverride).not.toHaveBeenCalled();
  });

  // --- Category Sync Toggle ---

  it('shows sync toggle when channel has a group_id', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.getByText('Sync with category permissions')).toBeInTheDocument();
    expect(screen.getByRole('switch')).toBeInTheDocument();
  });

  it('does not show sync toggle when channel has no group_id', () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannelNoGroup}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    expect(screen.queryByText('Sync with category permissions')).not.toBeInTheDocument();
  });

  it('toggles sync on when switch is clicked', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const toggle = screen.getByRole('switch');
    expect(toggle).toHaveAttribute('aria-checked', 'false');

    await act(async () => {
      fireEvent.click(toggle);
    });

    expect(mockSetCategorySync).toHaveBeenCalledWith('channel-1', true);
  });

  it('toggles sync via keyboard Enter', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const toggle = screen.getByRole('switch');

    await act(async () => {
      fireEvent.keyDown(toggle, { key: 'Enter' });
    });

    expect(mockSetCategorySync).toHaveBeenCalledWith('channel-1', true);
  });

  it('toggles sync via keyboard Space', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const toggle = screen.getByRole('switch');

    await act(async () => {
      fireEvent.keyDown(toggle, { key: ' ' });
    });

    expect(mockSetCategorySync).toHaveBeenCalledWith('channel-1', true);
  });

  it('shows synced notice and hides add section when synced', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    await act(async () => {
      fireEvent.click(screen.getByRole('switch'));
    });

    expect(screen.getByText(/Permissions are synced with the parent category/)).toBeInTheDocument();
    expect(document.querySelector('.add-override-section')).not.toBeInTheDocument();
  });

  it('shows sync hint text when synced', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    await act(async () => {
      fireEvent.click(screen.getByRole('switch'));
    });

    expect(screen.getByText(/will be replaced with category permissions/)).toBeInTheDocument();
  });

  it('refetches overrides when sync is enabled', async () => {
    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    mockFetchChannelOverrides.mockClear();

    await act(async () => {
      fireEvent.click(screen.getByRole('switch'));
    });

    expect(mockFetchChannelOverrides).toHaveBeenCalledWith('channel-1');
  });

  it('does not update synced state if setCategorySync fails', async () => {
    mockSetCategorySync.mockResolvedValue(false);

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={mockChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const toggle = screen.getByRole('switch');
    expect(toggle).toHaveAttribute('aria-checked', 'false');

    await act(async () => {
      fireEvent.click(toggle);
    });

    expect(toggle).toHaveAttribute('aria-checked', 'false');
  });

  it('hides editor when synced even if override was selected', () => {
    // Instead of testing toggle interaction (covered by other sync tests),
    // verify that when sync_permissions is true from the start, the editor
    // is never shown even when an override is selected via store state.
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [mockRoleOverride] },
    });

    const syncedChannel = { ...mockChannel, sync_permissions: true };

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={syncedChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    // Override is shown in the list
    const overrideName = document.querySelector('.override-target-name');
    expect(overrideName).toHaveTextContent('Moderator');

    // Click override to try to select it
    clickOverrideItem('Moderator');

    // Editor should NOT appear because synced is true: {selectedOverride && !synced && (...)}
    expect(screen.queryByText('Save Override')).not.toBeInTheDocument();
    // Add Override section should also be hidden
    expect(document.querySelector('.add-override-section')).not.toBeInTheDocument();
  });

  it('initializes synced state from channel.sync_permissions', () => {
    const syncedChannel = { ...mockChannel, sync_permissions: true };

    render(
      <ChannelSettingsModal
        isOpen={true}
        channel={syncedChannel}
        serverId="server-1"
        onClose={mockOnClose}
      />
    );

    const toggle = screen.getByRole('switch');
    expect(toggle).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByText(/Permissions are synced with the parent category/)).toBeInTheDocument();
  });
});
