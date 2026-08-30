import { render, screen, fireEvent, act } from '../../../test-utils';
import MemberContextMenu from '@/renderer/components/Members/MemberContextMenu';
import { mockMember, mockMember2 } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useFriendStore } from '@/renderer/stores/chat/friendStore';
import { ADMIN_PERMISSIONS } from '@/renderer/utils/permissions';
import { resetAllStores } from '../../../helpers/store-helpers';
import type { Role } from '@/renderer/types/server';

vi.mock('@/renderer/services/friendEligibility', () => ({
  // Defaults matter: a bare vi.fn() returns undefined and the hook does
  // fetchEligibility(id).then(...), which would throw for every test in this
  // file rather than only the ones that care about eligibility.
  fetchEligibility: vi.fn().mockResolvedValue('eligible'),
  peekEligibility: vi.fn().mockReturnValue('eligible'),
}));
import { fetchEligibility, peekEligibility } from '@/renderer/services/friendEligibility';
const mockFetchEligibility = fetchEligibility as ReturnType<typeof vi.fn>;
const mockPeekEligibility = peekEligibility as ReturnType<typeof vi.fn>;

const SERVER_ID = 'server-1';
const OWNER_USER_ID = 'user-1';

const mockRoles: Role[] = [
  {
    id: 'role-admin',
    server_id: SERVER_ID,
    name: 'Admin',
    color: '#FF0000',
    position: 2,
    permissions: '0',
    is_default: false,
    is_managed: false,
    display_separately: true,
    mentionable: false,
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
    is_managed: false,
    display_separately: false,
    mentionable: false,
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
    is_managed: true,
    display_separately: false,
    mentionable: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  },
];

describe('MemberContextMenu — extended coverage', () => {
  const mockOnClose = vi.fn();
  const mockOnViewProfile = vi.fn();
  const mockOnBan = vi.fn();
  const mockOnKick = vi.fn();

  const defaultProps = {
    member: mockMember2,
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
    mockFetchEligibility.mockResolvedValue('eligible');
    mockPeekEligibility.mockReturnValue('eligible');
    vi.useFakeTimers();
    resetAllStores();
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    useUserStore.setState({ user: { id: OWNER_USER_ID, username: 'testuser' } as never });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  describe('role assignment via RBAC', () => {
    it('renders the context menu for a member', () => {
      render(<MemberContextMenu {...defaultProps} />);
      expect(
        screen.getByText(mockMember2.display_name || mockMember2.username)
      ).toBeInTheDocument();
    });

    it('calls assignRole when clicking an unassigned role', async () => {
      const mockAssign = vi.fn().mockResolvedValue(true);
      usePermissionStore.setState({
        serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
        serverRoles: { [SERVER_ID]: mockRoles },
        assignRole: mockAssign,
      });

      render(<MemberContextMenu {...defaultProps} />);
      fireEvent.click(screen.getByText('Assign Role'));

      await act(async () => {
        fireEvent.click(screen.getByText('Admin'));
      });

      expect(mockAssign).toHaveBeenCalledWith(SERVER_ID, mockMember2.user_id, 'role-admin');
    });

    it('calls unassignRole when clicking an already-assigned role', async () => {
      const mockUnassign = vi.fn().mockResolvedValue(true);
      usePermissionStore.setState({
        serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
        serverRoles: { [SERVER_ID]: mockRoles },
        unassignRole: mockUnassign,
      });

      const memberWithRole = {
        ...mockMember2,
        roles: [{ role_id: 'role-admin', role_name: 'Admin', position: 2 }],
      };

      render(<MemberContextMenu {...defaultProps} member={memberWithRole} />);
      fireEvent.click(screen.getByText('Assign Role'));

      await act(async () => {
        fireEvent.click(screen.getByText('Admin'));
      });

      expect(mockUnassign).toHaveBeenCalledWith(SERVER_ID, mockMember2.user_id, 'role-admin');
    });

    it('refetches members after successful role assignment', async () => {
      const mockAssign = vi.fn().mockResolvedValue(true);
      const mockFetchMembers = vi.fn().mockResolvedValue(undefined);
      usePermissionStore.setState({
        serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
        serverRoles: { [SERVER_ID]: mockRoles },
        assignRole: mockAssign,
      });
      useMemberStore.setState({ fetchMembers: mockFetchMembers });

      render(<MemberContextMenu {...defaultProps} />);
      fireEvent.click(screen.getByText('Assign Role'));

      await act(async () => {
        fireEvent.click(screen.getByText('Admin'));
      });

      expect(mockAssign).toHaveBeenCalledWith(SERVER_ID, mockMember2.user_id, 'role-admin');
      expect(mockFetchMembers).toHaveBeenCalledWith(SERVER_ID);
    });

    it('logs error when role assignment returns false', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const mockAssign = vi.fn().mockResolvedValue(false);
      usePermissionStore.setState({
        serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
        serverRoles: { [SERVER_ID]: mockRoles },
        assignRole: mockAssign,
      });

      render(<MemberContextMenu {...defaultProps} />);
      fireEvent.click(screen.getByText('Assign Role'));

      await act(async () => {
        fireEvent.click(screen.getByText('Admin'));
      });

      expect(consoleSpy).toHaveBeenCalledWith('Failed to toggle role: server returned non-OK');
      consoleSpy.mockRestore();
    });

    it('shows "No roles available" when server has no assignable roles', () => {
      usePermissionStore.setState({
        serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
        serverRoles: {
          [SERVER_ID]: [mockRoles[2]], // Only @everyone (is_default)
        },
      });
      render(<MemberContextMenu {...defaultProps} />);
      fireEvent.click(screen.getByText('Assign Role'));
      expect(screen.getByText('No roles available')).toBeInTheDocument();
    });

    it('resets togglingRoleId after failure', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const mockAssign = vi.fn().mockRejectedValue(new Error('Network error'));
      usePermissionStore.setState({
        serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
        serverRoles: { [SERVER_ID]: mockRoles },
        assignRole: mockAssign,
      });

      render(<MemberContextMenu {...defaultProps} />);
      fireEvent.click(screen.getByText('Assign Role'));

      await act(async () => {
        fireEvent.click(screen.getByText('Admin'));
      });

      // Should not be stuck in "Updating..." state
      expect(screen.queryByText('Updating...')).not.toBeInTheDocument();
      consoleSpy.mockRestore();
    });
  });

  describe('display name fallback', () => {
    it('shows username when display_name is empty', () => {
      const memberNoDisplay = { ...mockMember2, display_name: '' };
      render(<MemberContextMenu {...defaultProps} member={memberNoDisplay} />);
      expect(screen.getByText('testuser2')).toBeInTheDocument();
    });
  });

  describe('Ban and Kick', () => {
    it('hides Ban when targeting the owner', () => {
      render(<MemberContextMenu {...defaultProps} member={mockMember} />);
      expect(screen.queryByText('Ban')).not.toBeInTheDocument();
    });

    it('hides Kick when targeting the owner', () => {
      render(<MemberContextMenu {...defaultProps} member={mockMember} />);
      expect(screen.queryByText('Kick')).not.toBeInTheDocument();
    });

    it('Ban button is enabled and calls onBan', () => {
      render(<MemberContextMenu {...defaultProps} />);
      const banBtn = screen.getByText('Ban').closest('button')!;
      expect(banBtn).not.toBeDisabled();
      fireEvent.click(banBtn);
      expect(mockOnBan).toHaveBeenCalledWith(mockMember2);
    });

    it('Kick button is enabled and calls onKick', () => {
      render(<MemberContextMenu {...defaultProps} />);
      const kickBtn = screen.getByText('Kick').closest('button')!;
      expect(kickBtn).not.toBeDisabled();
      fireEvent.click(kickBtn);
      expect(mockOnKick).toHaveBeenCalledWith(mockMember2);
    });
  });

  describe('Send DM', () => {
    it('calls openDM on click', async () => {
      const mockOpenDM = vi.fn().mockResolvedValue({ id: 'dm-1' });
      useDMStore.setState({ openDM: mockOpenDM } as never);

      render(<MemberContextMenu {...defaultProps} />);

      await act(async () => {
        fireEvent.click(screen.getByText('Send DM'));
      });

      expect(mockOpenDM).toHaveBeenCalledWith(mockMember2.user_id);
      expect(mockOnClose).toHaveBeenCalled();
    });

    it('handles openDM error gracefully', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const mockOpenDM = vi.fn().mockRejectedValue(new Error('DM blocked'));
      useDMStore.setState({ openDM: mockOpenDM } as never);

      render(<MemberContextMenu {...defaultProps} />);

      await act(async () => {
        fireEvent.click(screen.getByText('Send DM'));
      });

      expect(consoleSpy).toHaveBeenCalledWith('Failed to open DM:', expect.any(String));
      expect(mockOnClose).toHaveBeenCalled();
      consoleSpy.mockRestore();
    });
  });

  describe('Friend Request', () => {
    it('calls sendRequest on click', async () => {
      const mockSendRequest = vi.fn().mockResolvedValue(undefined);
      useFriendStore.setState({ sendRequest: mockSendRequest } as never);

      render(<MemberContextMenu {...defaultProps} />);

      await act(async () => {
        fireEvent.click(screen.getByText('Send Friend Request'));
      });

      expect(mockSendRequest).toHaveBeenCalledWith(mockMember2.user_id);
      expect(mockOnClose).toHaveBeenCalled();
    });

    it('handles sendRequest error gracefully', async () => {
      // After #226 the menu delegates to the shared useFriendRequestState hook,
      // which captures the rejection in its own state rather than logging to
      // console. The menu's contract here is narrower: it fires the send and
      // closes without throwing. The error surfaces on the profile-card
      // surface (which keeps the hook mounted), not the transient menu.
      const mockSendRequest = vi.fn().mockRejectedValue(new Error('Already sent'));
      useFriendStore.setState({ sendRequest: mockSendRequest } as never);

      render(<MemberContextMenu {...defaultProps} />);

      await act(async () => {
        fireEvent.click(screen.getByText('Send Friend Request'));
      });

      expect(mockSendRequest).toHaveBeenCalled();
      expect(mockOnClose).toHaveBeenCalled();
    });
  });
});

// ── #1241: freeze-at-open ────────────────────────────────────────────────────

describe('MemberContextMenu — eligibility freeze-at-open (#1241)', () => {
  const props = {
    member: mockMember2,
    position: { x: 0, y: 0 },
    serverId: SERVER_ID,
    ownerUserId: OWNER_USER_ID,
    onClose: vi.fn(),
    onViewProfile: vi.fn(),
    onBan: vi.fn(),
    onKick: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS },
      serverRoles: { [SERVER_ID]: mockRoles },
    });
    useUserStore.setState({ user: { id: OWNER_USER_ID, username: 'testuser' } as never });
  });

  // A row that appears in an already-painted menu is a layout shift, a
  // focus-order mutation (WCAG 2.4.3), and invisible to a screen reader that
  // already announced the item count (4.1.3). The verdict is frozen at open.
  it('does not change its item set after the verdict resolves', async () => {
    mockPeekEligibility.mockReturnValue('pending');
    let resolveVerdict!: (v: string) => void;
    mockFetchEligibility.mockReturnValue(
      new Promise((r) => {
        resolveVerdict = r as (v: string) => void;
      })
    );

    const { rerender } = render(<MemberContextMenu {...props} />);
    const before = screen.getAllByRole('button').length;
    expect(screen.getByText('Send Friend Request')).toBeInTheDocument();

    await act(async () => {
      resolveVerdict('ineligible');
    });
    rerender(<MemberContextMenu {...props} />);

    expect(screen.getAllByRole('button')).toHaveLength(before);
    expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
  });

  // Degrade open is only for an UNRESOLVED verdict. A menu opened with the
  // answer already cached must honour it at paint.
  it('honours a warm ineligible verdict at open', () => {
    mockPeekEligibility.mockReturnValue('ineligible');
    mockFetchEligibility.mockResolvedValue('ineligible');
    render(<MemberContextMenu {...props} />);
    expect(screen.queryByText('Send Friend Request')).not.toBeInTheDocument();
  });

  it('shows the item on a warm eligible verdict', () => {
    mockPeekEligibility.mockReturnValue('eligible');
    mockFetchEligibility.mockResolvedValue('eligible');
    render(<MemberContextMenu {...props} />);
    expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
  });
});
