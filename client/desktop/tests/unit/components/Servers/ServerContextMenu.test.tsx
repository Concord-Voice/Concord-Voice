import { render, screen, fireEvent } from '../../../test-utils';
import { mockServer, mockUser } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { MANAGE_SERVER, BASE_PERMISSIONS } from '@/renderer/utils/permissions';
import ServerContextMenu from '@/renderer/components/Servers/ServerContextMenu';

const SERVER_ID = 'server-1';
const OWNER_ID = 'user-1'; // matches mockServer.owner_id
const OTHER_ID = 'user-2';
// MANAGE_SERVER controls "Server Settings" visibility
const OWNER_PERMS = MANAGE_SERVER | BASE_PERMISSIONS;

describe('ServerContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnEditServer = vi.fn();
  const mockOnDeleteServer = vi.fn();
  const mockOnLeaveServer = vi.fn();
  const mockOnInvite = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    // Default: owner-level permissions and identity
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: OWNER_PERMS },
    });
    useUserStore.setState({ user: { ...mockUser, id: OWNER_ID } });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = (serverOverrides = {}) => {
    const server = { ...mockServer, ...serverOverrides };
    return render(
      <ServerContextMenu
        server={server}
        position={{ x: 100, y: 100 }}
        onClose={mockOnClose}
        onEditServer={mockOnEditServer}
        onDeleteServer={mockOnDeleteServer}
        onLeaveServer={mockOnLeaveServer}
        onInvite={mockOnInvite}
      />
    );
  };

  it('renders server name and role', () => {
    renderMenu();
    expect(screen.getByText('Test Server')).toBeInTheDocument();
    expect(screen.getByText('owner')).toBeInTheDocument();
  });

  it('shows Mark All as Read', () => {
    renderMenu();
    expect(screen.getByText('Mark All as Read')).toBeInTheDocument();
  });

  it('shows Server Settings for owner', () => {
    // beforeEach sets owner identity + MANAGE_SERVER perm
    renderMenu();
    expect(screen.getByText('Server Settings')).toBeInTheDocument();
  });

  it('shows Server Settings for admin', () => {
    // Admin: non-owner identity but has MANAGE_SERVER perm
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    renderMenu({ role: 'admin' });
    expect(screen.getByText('Server Settings')).toBeInTheDocument();
  });

  it('hides Server Settings for member', () => {
    // Member: non-owner identity, no MANAGE_SERVER perm
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu({ role: 'member' });
    expect(screen.queryByText('Server Settings')).not.toBeInTheDocument();
  });

  it('shows Delete Server for owner', () => {
    // beforeEach sets user-1 = owner
    renderMenu();
    expect(screen.getByText('Delete Server')).toBeInTheDocument();
    expect(screen.queryByText('Leave Server')).not.toBeInTheDocument();
  });

  it('shows Leave Server for member (not Delete)', () => {
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu({ role: 'member' });
    expect(screen.getByText('Leave Server')).toBeInTheDocument();
    expect(screen.queryByText('Delete Server')).not.toBeInTheDocument();
  });

  it('shows Invite to Server for all roles', () => {
    renderMenu();
    expect(screen.getByText('Invite to Server')).toBeInTheDocument();
  });

  it('calls onEditServer when Server Settings clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Server Settings'));
    expect(mockOnEditServer).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onDeleteServer when Delete Server clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Delete Server'));
    expect(mockOnDeleteServer).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onLeaveServer when Leave Server clicked', () => {
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu({ role: 'member' });
    fireEvent.click(screen.getByText('Leave Server'));
    expect(mockOnLeaveServer).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onInvite when Invite to Server clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Invite to Server'));
    expect(mockOnInvite).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
