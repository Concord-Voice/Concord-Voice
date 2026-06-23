import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';
import type { DMParticipant } from '@/renderer/stores/dmStore';

import GroupMemberItem, {
  GROUP_MEMBER_MENU_Z_INDEX,
} from '@/renderer/components/DirectMessages/GroupMemberItem';

describe('GroupMemberItem', () => {
  const mockOnRoleChange = vi.fn();
  const mockOnRemove = vi.fn();

  const adminParticipant: DMParticipant = {
    userId: 'user-1',
    username: 'alice',
    displayName: 'Alice Admin',
    role: 'admin',
  };

  const memberParticipant: DMParticipant = {
    userId: 'user-2',
    username: 'bob',
    displayName: 'Bob Member',
    role: 'member',
  };

  const defaultProps = {
    conversationId: 'group-1',
    createdBy: 'user-1',
    currentUserId: 'user-1',
    isCurrentUserAdmin: true,
    onRoleChange: mockOnRoleChange,
    onRemove: mockOnRemove,
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('keeps the member action menu on the shared context-menu z-index tier', () => {
    const { container } = render(
      <GroupMemberItem {...defaultProps} participant={memberParticipant} />
    );

    fireEvent.click(screen.getByLabelText('Member actions'));

    const menu = container.querySelector('.group-member-menu') as HTMLElement | null;
    expect(menu).not.toBeNull();
    expect(menu).toHaveStyle({ zIndex: String(GROUP_MEMBER_MENU_Z_INDEX) });
  });

  it('renders participant name and username', () => {
    render(<GroupMemberItem {...defaultProps} participant={memberParticipant} />);
    expect(screen.getByText('Bob Member')).toBeInTheDocument();
    expect(screen.getByText('@bob')).toBeInTheDocument();
  });

  it('shows Admin badge for admin role', () => {
    render(<GroupMemberItem {...defaultProps} participant={adminParticipant} />);
    expect(screen.getByText('Admin')).toBeInTheDocument();
  });

  it('does not show Admin badge for member role', () => {
    render(<GroupMemberItem {...defaultProps} participant={memberParticipant} />);
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
  });

  it('shows action menu for admin viewers on non-self non-creator members', () => {
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={memberParticipant}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    expect(screen.getByLabelText('Member actions')).toBeInTheDocument();
  });

  it('hides action menu for non-admin viewers', () => {
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={memberParticipant}
        currentUserId="user-3"
        isCurrentUserAdmin={false}
      />
    );
    expect(screen.queryByLabelText('Member actions')).not.toBeInTheDocument();
  });

  it('hides action menu on self', () => {
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={adminParticipant}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
      />
    );
    expect(screen.queryByLabelText('Member actions')).not.toBeInTheDocument();
  });

  it('hides action menu on creator', () => {
    // Current user is admin (user-3), target is creator (user-1)
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={adminParticipant}
        currentUserId="user-3"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    expect(screen.queryByLabelText('Member actions')).not.toBeInTheDocument();
  });

  it('opens action menu on button click', () => {
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={memberParticipant}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    fireEvent.click(screen.getByLabelText('Member actions'));
    expect(screen.getByText('Promote to Admin')).toBeInTheDocument();
    expect(screen.getByText('Remove from Group')).toBeInTheDocument();
  });

  it('shows Demote to Member for admin targets', () => {
    const nonCreatorAdmin: DMParticipant = {
      userId: 'user-5',
      username: 'eve',
      displayName: 'Eve Admin',
      role: 'admin',
    };
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={nonCreatorAdmin}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    fireEvent.click(screen.getByLabelText('Member actions'));
    expect(screen.getByText('Demote to Member')).toBeInTheDocument();
  });

  it('calls onRoleChange when promote/demote is clicked', () => {
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={memberParticipant}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    fireEvent.click(screen.getByLabelText('Member actions'));
    fireEvent.click(screen.getByText('Promote to Admin'));
    expect(mockOnRoleChange).toHaveBeenCalledWith('user-2', 'admin');
  });

  it('calls onRemove when remove is clicked', () => {
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={memberParticipant}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    fireEvent.click(screen.getByLabelText('Member actions'));
    fireEvent.click(screen.getByText('Remove from Group'));
    expect(mockOnRemove).toHaveBeenCalledWith('user-2');
  });

  it('shows (you) indicator for self', () => {
    render(
      <GroupMemberItem {...defaultProps} participant={adminParticipant} currentUserId="user-1" />
    );
    expect(screen.getByText('(you)')).toBeInTheDocument();
  });

  it('falls back to username when no displayName', () => {
    const noDisplayParticipant: DMParticipant = {
      userId: 'user-5',
      username: 'eve',
      role: 'member',
    };
    render(
      <GroupMemberItem
        {...defaultProps}
        participant={noDisplayParticipant}
        currentUserId="user-1"
        isCurrentUserAdmin={true}
        createdBy="user-1"
      />
    );
    expect(screen.getByText('eve')).toBeInTheDocument();
    // No @username shown since displayName is absent
    expect(screen.queryByText('@eve')).not.toBeInTheDocument();
  });
});
