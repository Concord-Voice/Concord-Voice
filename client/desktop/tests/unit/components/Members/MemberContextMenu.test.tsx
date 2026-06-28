import { render, screen, fireEvent, act } from '../../../test-utils';
import MemberContextMenu from '@/renderer/components/Members/MemberContextMenu';
import { mockMember, mockMember2 } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { apiFetch, safeJson } from '@/renderer/services/apiClient';
import { ADMIN_PERMISSIONS, BASE_PERMISSIONS, TIMEOUT_MEMBERS } from '@/renderer/utils/permissions';
import { resetAllStores } from '../../../helpers/store-helpers';
import type { Role } from '@/renderer/types/server';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

const SERVER_ID = 'server-1';
const OWNER_USER_ID = 'user-1'; // mockMember.user_id / mockServer.owner_id

const mockRoles: Role[] = [
  {
    id: 'role-admin',
    server_id: SERVER_ID,
    name: 'Admin',
    color: '#FF0000',
    position: 2,
    permissions: '0',
    is_default: false,
    display_separately: true,
    mentionable: false,
    require_mfa: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  },
  {
    id: 'role-mod',
    server_id: SERVER_ID,
    name: 'Moderator',
    color: '#00FF00',
    position: 1,
    permissions: '0',
    is_default: false,
    display_separately: false,
    mentionable: false,
    require_mfa: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  },
  {
    id: 'role-everyone',
    server_id: SERVER_ID,
    name: '@everyone',
    position: 0,
    permissions: '0',
    is_default: true,
    display_separately: false,
    mentionable: false,
    require_mfa: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  },
];

describe('MemberContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnViewProfile = vi.fn();
  const mockOnBan = vi.fn();
  const mockOnKick = vi.fn();

  const defaultProps = {
    member: mockMember2, // non-owner member
    position: { x: 100, y: 100 },
    serverId: SERVER_ID,
    ownerUserId: OWNER_USER_ID,
    onClose: mockOnClose,
    onViewProfile: mockOnViewProfile,
    onBan: mockOnBan,
    onKick: mockOnKick,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    resetAllStores();
    // Default: admin-level permissions
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    // Set current user to the owner (user-1) so self-guards don't hide items
    useUserStore.setState({ user: { id: OWNER_USER_ID, username: 'testuser' } as never });
    vi.mocked(apiFetch).mockResolvedValue({ ok: true } as Response);
    vi.mocked(safeJson).mockResolvedValue({});
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders member name in header', () => {
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.getByText('Test User 2')).toBeInTheDocument();
  });

  it('shows View Profile option', () => {
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.getByText('View Profile')).toBeInTheDocument();
  });

  it('shows Send DM option (enabled) for non-self member', () => {
    render(<MemberContextMenu {...defaultProps} />);
    const dmBtn = screen.getByText('Send DM');
    expect(dmBtn).toBeInTheDocument();
    expect(dmBtn.closest('button')).not.toBeDisabled();
  });

  it('hides Send DM when targeting self', () => {
    useUserStore.setState({ user: { id: mockMember2.user_id, username: 'testuser2' } as never });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Send DM')).not.toBeInTheDocument();
  });

  it('shows Assign Role for admin targeting non-owner', () => {
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.getByText('Assign Role')).toBeInTheDocument();
  });

  it('hides Assign Role for non-permissioned user', () => {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Assign Role')).not.toBeInTheDocument();
  });

  it('hides Assign Role when targeting owner', () => {
    render(<MemberContextMenu {...defaultProps} member={mockMember} />);
    expect(screen.queryByText('Assign Role')).not.toBeInTheDocument();
  });

  it('shows RBAC roles (excluding default) in role picker submenu', () => {
    render(<MemberContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Assign Role'));
    // Should show Admin and Moderator but not @everyone
    expect(screen.getByText('Admin')).toBeInTheDocument();
    expect(screen.getByText('Moderator')).toBeInTheDocument();
    expect(screen.queryByText('@everyone')).not.toBeInTheDocument();
  });

  it('shows checkmark for roles the member already has', () => {
    const memberWithRole = {
      ...mockMember2,
      roles: [{ role_id: 'role-admin', role_name: 'Admin', position: 2 }],
    };
    render(<MemberContextMenu {...defaultProps} member={memberWithRole} />);
    fireEvent.click(screen.getByText('Assign Role'));
    // The Admin role should have a checkmark
    const adminOption = screen.getByText('Admin').closest('button');
    expect(adminOption?.textContent).toContain('\u2713');
  });

  it('toggles role picker closed on second click', () => {
    render(<MemberContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Assign Role'));
    expect(screen.getByText('Admin')).toBeInTheDocument();
    // Click again to close — animation timeout then React state update
    fireEvent.click(screen.getByText('Assign Role'));
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
    expect(screen.queryByText('Moderator')).not.toBeInTheDocument();
  });

  it('shows Ban option for admin targeting non-owner', () => {
    render(<MemberContextMenu {...defaultProps} />);
    const banBtn = screen.getByText('Ban');
    expect(banBtn).toBeInTheDocument();
    expect(banBtn.closest('button')).not.toBeDisabled();
  });

  it('hides Ban for regular member', () => {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Ban')).not.toBeInTheDocument();
  });

  it('hides Ban when targeting owner', () => {
    render(<MemberContextMenu {...defaultProps} member={mockMember} />);
    expect(screen.queryByText('Ban')).not.toBeInTheDocument();
  });

  it('shows Kick option for admin targeting non-owner', () => {
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.getByText('Kick')).toBeInTheDocument();
  });

  it('shows Timeout for users with the timeout permission', () => {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: TIMEOUT_MEMBERS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.getByText('Timeout')).toBeInTheDocument();
  });

  it('hides Timeout for regular members', () => {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Timeout')).not.toBeInTheDocument();
  });

  it('hides Kick for regular member', () => {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Kick')).not.toBeInTheDocument();
  });

  it('calls onBan and onClose when Ban is clicked', () => {
    render(<MemberContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Ban'));
    expect(mockOnBan).toHaveBeenCalledWith(mockMember2);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onKick and onClose when Kick is clicked', () => {
    render(<MemberContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Kick'));
    expect(mockOnKick).toHaveBeenCalledWith(mockMember2);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('posts selected timeout duration and updates the member timeout', async () => {
    const timedOutUntil = '2026-06-28T18:00:00.000Z';
    vi.mocked(safeJson).mockResolvedValue({ timed_out_until: timedOutUntil });
    useMemberStore.getState().addMember(mockMember2);

    render(<MemberContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Timeout'));
    fireEvent.click(screen.getByText('5 minutes'));

    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/servers/server-1/members/user-2/timeout',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ duration_seconds: 300 }),
      })
    );
    expect(useMemberStore.getState().members[0].timed_out_until).toBe(timedOutUntil);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('removes an active timeout', async () => {
    useMemberStore.getState().addMember({
      ...mockMember2,
      timed_out_until: '2999-01-01T00:00:00.000Z',
    });

    render(
      <MemberContextMenu
        {...defaultProps}
        member={{ ...mockMember2, timed_out_until: '2999-01-01T00:00:00.000Z' }}
      />
    );
    fireEvent.click(screen.getByText('Remove Timeout'));

    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/servers/server-1/members/user-2/timeout',
      expect.objectContaining({ method: 'DELETE' })
    );
    expect(useMemberStore.getState().members[0].timed_out_until).toBeNull();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onViewProfile and onClose when View Profile clicked', () => {
    render(<MemberContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('View Profile'));
    expect(mockOnViewProfile).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows Send Friend Request for non-friend non-self member', () => {
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
  });

  it('shows Friends (disabled) when already friends', () => {
    useFriendStore.setState({
      friends: [
        {
          id: 'f-1',
          userId: mockMember2.user_id,
          username: 'testuser2',
          status: 'online' as const,
        },
      ],
    });
    render(<MemberContextMenu {...defaultProps} />);
    const friendsBtn = screen.getByText('Friends');
    expect(friendsBtn.closest('button')).toBeDisabled();
  });

  it('shows Request Pending (disabled) when request is pending', () => {
    useFriendStore.setState({
      pendingRequests: [
        {
          id: 'r-1',
          fromUserId: OWNER_USER_ID,
          fromUsername: 'testuser',
          toUserId: mockMember2.user_id,
          toUsername: 'testuser2',
          direction: 'sent' as const,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    });
    render(<MemberContextMenu {...defaultProps} />);
    const pendingBtn = screen.getByText('Request Pending');
    expect(pendingBtn.closest('button')).toBeDisabled();
  });

  it('hides Friend Request when targeting self', () => {
    useUserStore.setState({ user: { id: mockMember2.user_id, username: 'testuser2' } as never });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Send Friend Request')).not.toBeInTheDocument();
  });

  it('hides Ban, Kick, and Assign Role when targeting self', () => {
    useUserStore.setState({ user: { id: mockMember2.user_id, username: 'testuser2' } as never });
    render(<MemberContextMenu {...defaultProps} />);
    expect(screen.queryByText('Ban')).not.toBeInTheDocument();
    expect(screen.queryByText('Kick')).not.toBeInTheDocument();
    expect(screen.queryByText('Assign Role')).not.toBeInTheDocument();
  });
});
