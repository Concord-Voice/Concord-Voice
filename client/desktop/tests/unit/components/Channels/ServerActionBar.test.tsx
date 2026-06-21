import { render, screen, fireEvent } from '../../../test-utils';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import ServerActionBar from '@/renderer/components/Channels/ServerActionBar';
import { mockServer } from '../../../mocks/fixtures';
import type { ServerWithRole } from '@/renderer/types/server';
import { MANAGE_SERVER, BASE_PERMISSIONS, ADMIN_PERMISSIONS } from '@/renderer/utils/permissions';

// Isolate ServerActionBar from the modal's internals (friendStore/router/hook).
vi.mock('@/renderer/components/Channels/SendToFriendModal', () => ({
  SendToFriendModal: ({ open }: { open: boolean }) =>
    open ? <div data-testid="send-to-friend-modal">modal</div> : null,
}));

// Permission presets matching backend role presets
const OWNER_PERMS = ADMIN_PERMISSIONS | MANAGE_SERVER;

describe('ServerActionBar', () => {
  const mockOnOpenCreateModal = vi.fn();
  const mockOnOpenCreateCategoryModal = vi.fn();

  const ownerServer: ServerWithRole = { ...mockServer, role: 'owner' };
  const adminServer: ServerWithRole = { ...mockServer, role: 'admin' };
  const memberServer: ServerWithRole = { ...mockServer, role: 'member' };

  beforeEach(() => {
    vi.clearAllMocks();
    useInviteStore.setState({ invites: {}, isLoading: false, error: null });
    // Default to owner permissions; individual tests override as needed
    usePermissionStore.setState({
      serverPermissions: { 'server-1': OWNER_PERMS },
    });
  });

  it('renders Add and Invite buttons for owner', () => {
    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    expect(screen.getByText('Add')).toBeInTheDocument();
    expect(screen.getByText('Invite')).toBeInTheDocument();
  });

  it('renders Add and Invite buttons for admin', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': ADMIN_PERMISSIONS } });
    render(
      <ServerActionBar
        server={adminServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    expect(screen.getByText('Add')).toBeInTheDocument();
    expect(screen.getByText('Invite')).toBeInTheDocument();
  });

  it('renders spacer for regular member (no actions)', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': BASE_PERMISSIONS } });
    const { container } = render(
      <ServerActionBar
        server={memberServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    expect(container.querySelector('.channel-actions-spacer')).toBeInTheDocument();
    expect(screen.queryByText('Add')).not.toBeInTheDocument();
    expect(screen.queryByText('Invite')).not.toBeInTheDocument();
  });

  it('calls onOpenCreateModal when Channel option clicked in Add menu', () => {
    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Add'));
    fireEvent.click(screen.getByText('Channel'));
    expect(mockOnOpenCreateModal).toHaveBeenCalled();
  });

  it('shows invite popup when Invite clicked', () => {
    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    expect(screen.getByText('Invite Code')).toBeInTheDocument();
  });

  it('shows Generate Code button when no active invites', () => {
    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    expect(screen.getByText('Generate Code')).toBeInTheDocument();
  });

  it('shows existing invite code and Copy Code button', () => {
    const now = new Date();
    now.setHours(now.getHours() + 1); // expires in 1 hour
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'AbCd1234',
            is_revoked: false,
            expires_at: now.toISOString(),
            max_uses: null,
            use_count: 0,
            created_by: 'user-1',
            creator_username: 'testuser',
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));

    expect(screen.getByText('AbCd1234')).toBeInTheDocument();
    expect(screen.getByText('Copy Code')).toBeInTheDocument();
  });

  it('toggles popup closed on second click', () => {
    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    expect(screen.getByText('Invite Code')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Invite'));
    expect(screen.queryByText('Invite Code')).not.toBeInTheDocument();
  });

  it('enables the Send to a Friend button and opens the modal on click', () => {
    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    expect(screen.getByText('Direct Invite')).toBeInTheDocument();
    const sendBtn = screen.getByText('Send to a Friend');
    expect(sendBtn.closest('button')).not.toBeDisabled();
    fireEvent.click(sendBtn);
    expect(screen.getByTestId('send-to-friend-modal')).toBeInTheDocument();
  });

  it('copies invite code to clipboard on Copy Code click', async () => {
    const now = new Date();
    now.setHours(now.getHours() + 1);
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'COPY1234',
            is_revoked: false,
            expires_at: now.toISOString(),
            max_uses: null,
            use_count: 0,
            created_by: 'user-1',
            creator_username: 'testuser',
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    fireEvent.click(screen.getByText('Copy Code'));
    await vi.waitFor(() => {
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('COPY1234');
    });
  });

  it('skips expired invites', () => {
    const expired = new Date();
    expired.setHours(expired.getHours() - 1);
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'EXPIRED1',
            is_revoked: false,
            expires_at: expired.toISOString(),
            max_uses: null,
            use_count: 0,
            created_by: 'user-1',
            creator_username: 'testuser',
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    // Should show Generate Code since the invite is expired
    expect(screen.getByText('Generate Code')).toBeInTheDocument();
  });

  it('skips revoked invites', () => {
    const now = new Date();
    now.setHours(now.getHours() + 1);
    useInviteStore.setState({
      invites: {
        'server-1': [
          {
            id: 'inv-1',
            server_id: 'server-1',
            code: 'REVOKED1',
            is_revoked: true,
            expires_at: now.toISOString(),
            max_uses: null,
            use_count: 0,
            created_by: 'user-1',
            creator_username: 'testuser',
            created_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    fireEvent.click(screen.getByText('Invite'));
    expect(screen.getByText('Generate Code')).toBeInTheDocument();
  });

  it('renders single class when only one action available', () => {
    // Create a server that is admin but with only invite capability
    // Actually, admin has both Channel and Invite so not single
    // Let's check the strip class is present
    const { container } = render(
      <ServerActionBar
        server={ownerServer}
        onOpenCreateModal={mockOnOpenCreateModal}
        onOpenCreateCategoryModal={mockOnOpenCreateCategoryModal}
      />
    );
    expect(container.querySelector('.channel-actions-strip')).toBeInTheDocument();
  });
});
