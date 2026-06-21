import { render, screen, fireEvent } from '../../../test-utils';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { vi } from 'vitest';

import CreateGroupModal from '@/renderer/components/DirectMessages/CreateGroupModal';

describe('CreateGroupModal', () => {
  const mockOnClose = vi.fn();

  const mockFriends = [
    {
      id: 'f-1',
      userId: 'user-2',
      username: 'bob',
      displayName: 'Bob Smith',
      status: 'online' as const,
    },
    {
      id: 'f-2',
      userId: 'user-3',
      username: 'charlie',
      displayName: 'Charlie Brown',
      status: 'offline' as const,
    },
    {
      id: 'f-3',
      userId: 'user-4',
      username: 'diana',
      status: 'online' as const,
    },
  ];

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });
    useFriendStore.setState({ friends: mockFriends as any });
    useDMStore.setState({
      createGroupDM: vi.fn().mockResolvedValue({
        id: 'group-new',
        isGroup: true,
        name: 'Test Group',
      }),
    });
  });

  it('renders when isOpen is true', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Create Group DM')).toBeInTheDocument();
  });

  it('does not render when isOpen is false', () => {
    const { container } = render(<CreateGroupModal isOpen={false} onClose={mockOnClose} />);
    expect(container.innerHTML).toBe('');
  });

  it('shows group name input', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByPlaceholderText('Group Name (optional)')).toBeInTheDocument();
  });

  it('shows search input', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByPlaceholderText('Search friends...')).toBeInTheDocument();
  });

  it('calls onClose when close button clicked', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    const closeBtn = document.querySelector('.create-group-close-btn');
    expect(closeBtn).toBeInTheDocument();
    fireEvent.click(closeBtn!);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows friend list', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Bob Smith')).toBeInTheDocument();
    expect(screen.getByText('Charlie Brown')).toBeInTheDocument();
    expect(screen.getByText('diana')).toBeInTheDocument();
  });

  it('adds user chip when friend add button is clicked', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    // Click the add button for Bob
    const addBtns = document.querySelectorAll('.create-group-add-btn');
    fireEvent.click(addBtns[0]);
    // Should show chip
    expect(screen.getByText('Bob Smith')).toBeInTheDocument();
    // Bob should have a remove button (chip with X)
    expect(screen.getByLabelText('Remove bob')).toBeInTheDocument();
  });

  it('removes user chip when remove button is clicked', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    // Add Bob
    const addBtns = document.querySelectorAll('.create-group-add-btn');
    fireEvent.click(addBtns[0]);
    // Now remove
    fireEvent.click(screen.getByLabelText('Remove bob'));
    // Bob should be back in the friend list (no chip), still visible as a friend row
    expect(screen.queryByLabelText('Remove bob')).not.toBeInTheDocument();
  });

  it('create button disabled with no users selected', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    const createBtn = screen.getByText('Create Group (0 selected)');
    expect(createBtn).toBeDisabled();
  });

  it('create button enabled with users selected', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    const addBtns = document.querySelectorAll('.create-group-add-btn');
    fireEvent.click(addBtns[0]);
    const createBtn = screen.getByText('Create Group (1 selected)');
    expect(createBtn).not.toBeDisabled();
  });

  it('filters friends by search query', () => {
    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);
    const searchInput = screen.getByPlaceholderText('Search friends...');
    fireEvent.change(searchInput, { target: { value: 'bob' } });
    expect(screen.getByText('Bob Smith')).toBeInTheDocument();
    expect(screen.queryByText('Charlie Brown')).not.toBeInTheDocument();
  });

  it('calls createGroupDM on create button click', async () => {
    const mockCreate = vi.fn().mockResolvedValue({ id: 'group-new' });
    useDMStore.setState({ createGroupDM: mockCreate });

    render(<CreateGroupModal isOpen={true} onClose={mockOnClose} />);

    // Add a user
    const addBtns = document.querySelectorAll('.create-group-add-btn');
    fireEvent.click(addBtns[0]);

    // Set group name
    const nameInput = screen.getByPlaceholderText('Group Name (optional)');
    fireEvent.change(nameInput, { target: { value: 'My Group' } });

    // Click create
    fireEvent.click(screen.getByText('Create Group (1 selected)'));

    await vi.waitFor(() => {
      expect(mockCreate).toHaveBeenCalledWith(['user-2'], 'My Group');
      expect(mockOnClose).toHaveBeenCalled();
    });
  });
});
