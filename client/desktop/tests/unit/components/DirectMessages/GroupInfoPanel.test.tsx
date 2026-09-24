import { render, screen, fireEvent } from '../../../test-utils';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { vi } from 'vitest';

// Mock EditGroupModal to avoid nested complexity
vi.mock('@/renderer/components/DirectMessages/EditGroupModal', () => ({
  default: ({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) =>
    isOpen ? (
      <div data-testid="edit-group-modal">
        <button onClick={onClose}>Close Edit</button>
      </div>
    ) : null,
}));

// Mock GroupMemberItem to isolate unit — exposes onRoleChange and onRemove via buttons
vi.mock('@/renderer/components/DirectMessages/GroupMemberItem', () => ({
  default: ({
    participant,
    onRoleChange,
    onRemove,
  }: {
    participant: { userId: string; username: string; role?: string };
    onRoleChange: (userId: string, role: 'admin' | 'member') => void;
    onRemove: (userId: string) => void;
  }) => (
    <div data-testid={`member-${participant.userId}`}>
      {participant.username}
      {participant.role === 'admin' && <span>Admin</span>}
      <button onClick={() => onRoleChange(participant.userId, 'admin')}>
        Promote {participant.username}
      </button>
      <button onClick={() => onRemove(participant.userId)}>Remove {participant.username}</button>
    </div>
  ),
}));

import GroupInfoPanel from '@/renderer/components/DirectMessages/GroupInfoPanel';

describe('GroupInfoPanel', () => {
  const mockOnClose = vi.fn();
  const mockOnRequestRemoval = vi.fn();

  const mockConversation: DMConversation = {
    id: 'group-1',
    isGroup: true,
    isPersonal: false,
    name: 'Test Group',
    createdBy: 'user-1',
    participants: [
      { userId: 'user-1', username: 'alice', role: 'admin' },
      { userId: 'user-2', username: 'bob', role: 'member' },
      { userId: 'user-3', username: 'charlie', role: 'member' },
    ],
    lastMessage: null,
    unreadCount: 0,
    createdAt: '2025-01-01T00:00:00Z',
  };

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });
    useDMStore.setState({
      leaveGroup: vi.fn().mockResolvedValue(undefined),
      deleteGroup: vi.fn().mockResolvedValue(undefined),
      updateMemberRole: vi.fn().mockResolvedValue(undefined),
      removeGroupMember: vi.fn().mockResolvedValue(undefined),
    });
    // Mock window.confirm
    vi.spyOn(window, 'confirm').mockReturnValue(true);
  });

  const renderPanel = (conversation: DMConversation = mockConversation) =>
    render(
      <GroupInfoPanel
        conversation={conversation}
        onClose={mockOnClose}
        onRequestRemoval={mockOnRequestRemoval}
      />
    );

  it('renders group name and member count', () => {
    renderPanel();
    expect(screen.getByText('Test Group')).toBeInTheDocument();
    expect(screen.getByText('3 members')).toBeInTheDocument();
  });

  it('renders all member items', () => {
    renderPanel();
    expect(screen.getByTestId('member-user-1')).toBeInTheDocument();
    expect(screen.getByTestId('member-user-2')).toBeInTheDocument();
    expect(screen.getByTestId('member-user-3')).toBeInTheDocument();
  });

  it('shows Leave Group button for all members', () => {
    renderPanel();
    expect(screen.getByText('Leave Group')).toBeInTheDocument();
  });

  it('shows Hide, Clear, Purge, and a separate Leave action without using native confirm', () => {
    const confirmSpy = vi.spyOn(window, 'confirm');
    renderPanel();

    expect(screen.getByRole('button', { name: 'Hide thread' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Clear history for me' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Hide thread' }));
    fireEvent.click(screen.getByRole('button', { name: 'Clear history for me' }));
    fireEvent.click(screen.getByRole('button', { name: 'Leave Group' }));
    expect(mockOnRequestRemoval).toHaveBeenNthCalledWith(1, {
      conversation: mockConversation,
      action: 'hide',
    });
    expect(mockOnRequestRemoval).toHaveBeenNthCalledWith(2, {
      conversation: mockConversation,
      action: 'clear',
    });
    expect(mockOnRequestRemoval).toHaveBeenNthCalledWith(3, {
      conversation: mockConversation,
      action: 'leave',
    });
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('shows Delete Group button for admin/creator', () => {
    renderPanel();
    expect(screen.getByText('Delete Group')).toBeInTheDocument();
  });

  it('hides Delete Group button for non-admin non-creator', () => {
    useUserStore.setState({ user: { id: 'user-2', username: 'bob' } as any });
    renderPanel();
    expect(screen.queryByText('Delete Group')).not.toBeInTheDocument();
  });

  it('calls onClose when close button clicked', () => {
    renderPanel();
    const closeBtn = document.querySelector('.group-info-close-btn');
    fireEvent.click(closeBtn!);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows edit button for admin users', () => {
    renderPanel();
    expect(screen.getByLabelText('Edit group name')).toBeInTheDocument();
  });

  it('hides edit button for non-admin users', () => {
    useUserStore.setState({ user: { id: 'user-2', username: 'bob' } as any });
    renderPanel();
    expect(screen.queryByLabelText('Edit group name')).not.toBeInTheDocument();
  });

  it('requests Leave confirmation on Leave Group click', () => {
    renderPanel();
    fireEvent.click(screen.getByRole('button', { name: 'Leave Group' }));
    expect(mockOnRequestRemoval).toHaveBeenCalledWith({
      conversation: mockConversation,
      action: 'leave',
    });
  });

  it('calls deleteGroup on Delete Group click', async () => {
    const mockDelete = vi.fn().mockResolvedValue(undefined);
    useDMStore.setState({ deleteGroup: mockDelete });

    renderPanel();
    fireEvent.click(screen.getByText('Delete Group'));

    await vi.waitFor(() => {
      expect(mockDelete).toHaveBeenCalledWith('group-1');
    });
  });

  it('shows participant names as comma-separated fallback when no group name', () => {
    const noNameConv = { ...mockConversation, name: null };
    renderPanel(noNameConv);
    expect(screen.getByText('alice, bob, charlie')).toBeInTheDocument();
  });

  it('shows singular "member" for single participant', () => {
    const singleConv = {
      ...mockConversation,
      participants: [{ userId: 'user-1', username: 'alice', role: 'admin' as const }],
    };
    renderPanel(singleConv);
    expect(screen.getByText('1 member')).toBeInTheDocument();
  });

  // --- Edit modal ---

  it('opens EditGroupModal when edit button is clicked', () => {
    renderPanel();
    expect(screen.queryByTestId('edit-group-modal')).not.toBeInTheDocument();

    fireEvent.click(screen.getByLabelText('Edit group name'));
    expect(screen.getByTestId('edit-group-modal')).toBeInTheDocument();
  });

  it('closes EditGroupModal when its onClose is called', () => {
    renderPanel();
    fireEvent.click(screen.getByLabelText('Edit group name'));
    expect(screen.getByTestId('edit-group-modal')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Close Edit'));
    expect(screen.queryByTestId('edit-group-modal')).not.toBeInTheDocument();
  });

  // --- Delete group confirmation flow ---

  it('does not call deleteGroup when confirm is cancelled', () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false);
    const mockDelete = vi.fn().mockResolvedValue(undefined);
    useDMStore.setState({ deleteGroup: mockDelete });

    renderPanel();
    fireEvent.click(screen.getByText('Delete Group'));
    expect(mockDelete).not.toHaveBeenCalled();
  });

  it('shows error when deleteGroup fails', async () => {
    const mockDelete = vi.fn().mockRejectedValue(new Error('Permission denied'));
    useDMStore.setState({ deleteGroup: mockDelete });

    renderPanel();
    fireEvent.click(screen.getByText('Delete Group'));

    await vi.waitFor(() => {
      expect(screen.getByText('Permission denied')).toBeInTheDocument();
    });
  });

  it('shows fallback error for non-Error deleteGroup rejection', async () => {
    const mockDelete = vi.fn().mockRejectedValue(42);
    useDMStore.setState({ deleteGroup: mockDelete });

    renderPanel();
    fireEvent.click(screen.getByText('Delete Group'));

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to delete group')).toBeInTheDocument();
    });
  });

  // --- Role change and remove member error handling ---

  it('shows error when handleRoleChange fails', async () => {
    const mockUpdateRole = vi.fn().mockRejectedValue(new Error('Role update failed'));
    useDMStore.setState({ updateMemberRole: mockUpdateRole });

    renderPanel();
    fireEvent.click(screen.getByText('Promote bob'));

    await vi.waitFor(() => {
      expect(screen.getByText('Role update failed')).toBeInTheDocument();
    });
  });

  it('calls updateMemberRole on role change', async () => {
    const mockUpdateRole = vi.fn().mockResolvedValue(undefined);
    useDMStore.setState({ updateMemberRole: mockUpdateRole });

    renderPanel();
    fireEvent.click(screen.getByText('Promote bob'));

    await vi.waitFor(() => {
      expect(mockUpdateRole).toHaveBeenCalledWith('group-1', 'user-2', 'admin');
    });
  });

  it('shows error when handleRemoveMember fails', async () => {
    const mockRemove = vi.fn().mockRejectedValue(new Error('Remove failed'));
    useDMStore.setState({ removeGroupMember: mockRemove });

    renderPanel();
    fireEvent.click(screen.getByText('Remove bob'));

    await vi.waitFor(() => {
      expect(screen.getByText('Remove failed')).toBeInTheDocument();
    });
  });

  it('shows fallback error for non-Error handleRemoveMember rejection', async () => {
    const mockRemove = vi.fn().mockRejectedValue('oops');
    useDMStore.setState({ removeGroupMember: mockRemove });

    renderPanel();
    fireEvent.click(screen.getByText('Remove bob'));

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to remove member')).toBeInTheDocument();
    });
  });

  it('shows fallback error for non-Error handleRoleChange rejection', async () => {
    const mockUpdateRole = vi.fn().mockRejectedValue(null);
    useDMStore.setState({ updateMemberRole: mockUpdateRole });

    renderPanel();
    fireEvent.click(screen.getByText('Promote bob'));

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to update role')).toBeInTheDocument();
    });
  });

  it('shows Delete Group button for admin who is not creator', () => {
    // user-4 is admin but not the creator (user-1)
    useUserStore.setState({ user: { id: 'user-4', username: 'dave' } as any });
    const convWithAdminDave = {
      ...mockConversation,
      participants: [
        { userId: 'user-1', username: 'alice', role: 'admin' as const },
        { userId: 'user-4', username: 'dave', role: 'admin' as const },
      ],
    };
    renderPanel(convWithAdminDave);
    expect(screen.getByText('Delete Group')).toBeInTheDocument();
  });

  it('renders group icon initial from group name', () => {
    renderPanel();
    const icon = document.querySelector('.group-info-icon span');
    expect(icon?.textContent).toBe('T'); // "Test Group" -> "T"
  });

  it('renders group icon initial "G" when no name', () => {
    const noNameConv = { ...mockConversation, name: null };
    renderPanel(noNameConv);
    const icon = document.querySelector('.group-info-icon span');
    expect(icon?.textContent).toBe('G');
  });

  it('shows Delete Group for creator who is not admin', () => {
    // user-1 is creator but role is member
    useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });
    const creatorNotAdmin = {
      ...mockConversation,
      createdBy: 'user-1',
      participants: [
        { userId: 'user-1', username: 'alice', role: 'member' as const },
        { userId: 'user-2', username: 'bob', role: 'admin' as const },
      ],
    };
    renderPanel(creatorNotAdmin);
    expect(screen.getByText('Delete Group')).toBeInTheDocument();
  });
});
