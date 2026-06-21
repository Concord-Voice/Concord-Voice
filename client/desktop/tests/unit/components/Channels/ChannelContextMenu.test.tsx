import { render, screen, fireEvent } from '../../../test-utils';
import { mockChannel } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { MANAGE_CHANNELS, BASE_PERMISSIONS, ADMIN_PERMISSIONS } from '@/renderer/utils/permissions';
import ChannelContextMenu from '@/renderer/components/Channels/ChannelContextMenu';

const SERVER_ID = 'server-1';
const OWNER_PERMS = ADMIN_PERMISSIONS | MANAGE_CHANNELS;

describe('ChannelContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnEditChannel = vi.fn();
  const mockOnDeleteChannel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    // Default: owner-level permissions (can manage channels)
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: OWNER_PERMS },
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = (serverId = SERVER_ID, channel = mockChannel) => {
    return render(
      <ChannelContextMenu
        channel={channel}
        position={{ x: 100, y: 100 }}
        serverId={serverId}
        onClose={mockOnClose}
        onEditChannel={mockOnEditChannel}
        onDeleteChannel={mockOnDeleteChannel}
      />
    );
  };

  it('renders channel name', () => {
    renderMenu();
    expect(screen.getByText('general')).toBeInTheDocument();
  });

  it('shows Mark as Read item', () => {
    renderMenu();
    expect(screen.getByText('Mark as Read')).toBeInTheDocument();
  });

  it('shows Mute Channel item that opens the duration submenu', () => {
    // The mute action is now live (issue #84) — the old "disabled
    // placeholder" assertion has been replaced. The trigger shows
    // "Mute Channel" when the channel is currently unmuted; clicking it
    // opens a duration picker rather than firing a mute outright. Assert
    // both halves so a regression on either path fails this test.
    renderMenu();
    const muteBtn = screen.getByText('Mute Channel');
    expect(muteBtn).toBeInTheDocument();
    expect(muteBtn.closest('button')).not.toBeDisabled();
    fireEvent.click(muteBtn);
    expect(screen.getByText('For 15 minutes')).toBeInTheDocument();
    expect(screen.getByText('Until I turn it back on')).toBeInTheDocument();
  });

  it('shows Edit/Delete for owner', () => {
    renderMenu();
    expect(screen.getByText('Edit Channel')).toBeInTheDocument();
    expect(screen.getByText('Delete Channel')).toBeInTheDocument();
  });

  it('shows Edit/Delete for admin', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS } });
    renderMenu();
    expect(screen.getByText('Edit Channel')).toBeInTheDocument();
    expect(screen.getByText('Delete Channel')).toBeInTheDocument();
  });

  it('hides Edit/Delete for member', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu();
    expect(screen.queryByText('Edit Channel')).not.toBeInTheDocument();
    expect(screen.queryByText('Delete Channel')).not.toBeInTheDocument();
  });

  it('calls onEditChannel when Edit clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Edit Channel'));
    expect(mockOnEditChannel).toHaveBeenCalledWith(mockChannel);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onDeleteChannel when Delete clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Delete Channel'));
    expect(mockOnDeleteChannel).toHaveBeenCalledWith(mockChannel);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows Copy Link item', () => {
    renderMenu();
    expect(screen.getByText('Copy Link')).toBeInTheDocument();
  });
});
