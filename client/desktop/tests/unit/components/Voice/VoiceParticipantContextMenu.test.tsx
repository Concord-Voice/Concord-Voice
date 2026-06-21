import { render, screen, fireEvent } from '../../../test-utils';
import { VoiceParticipantContextMenu } from '@/renderer/components/Voice/VoiceParticipantContextMenu';
import type { VoiceMenuParticipant } from '@/renderer/components/Voice/VoiceParticipantContextMenu';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import {
  ADMIN_PERMISSIONS,
  BASE_PERMISSIONS,
  MOVE_MEMBERS,
  MUTE_MEMBERS,
} from '@/renderer/utils/permissions';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockChannel } from '../../../mocks/fixtures';

// Mock the voice-participant API client to assert calls without network.
const mockMove = vi.fn().mockResolvedValue({ ok: true });
const mockDisconnect = vi.fn().mockResolvedValue({ ok: true });
vi.mock('@/renderer/services/voiceParticipantApi', () => ({
  moveVoiceParticipant: (...args: unknown[]) => mockMove(...args),
  disconnectVoiceParticipant: (...args: unknown[]) => mockDisconnect(...args),
}));

const SERVER_ID = 'server-1';
const SELF_ID = 'user-self';
const TARGET_ID = 'user-2';
const CURRENT_CHANNEL = 'voice-current';
const OTHER_VOICE = 'voice-other';

const participant: VoiceMenuParticipant = {
  userId: TARGET_ID,
  username: 'testuser2',
  displayName: 'Test User 2',
  isMuted: false,
  serverMuted: false,
  serverDeafened: false,
};

const baseProps = {
  participant,
  serverId: SERVER_ID,
  channelId: CURRENT_CHANNEL,
  position: { x: 100, y: 100 },
  onClose: vi.fn(),
  onViewProfile: vi.fn(),
};

function seedVoiceChannels() {
  useChannelStore.setState({
    channels: [
      { ...mockChannel, id: CURRENT_CHANNEL, type: 'voice', server_id: SERVER_ID, name: 'Current' },
      { ...mockChannel, id: OTHER_VOICE, type: 'voice', server_id: SERVER_ID, name: 'Other Voice' },
      { ...mockChannel, id: 'text-1', type: 'text', server_id: SERVER_ID, name: 'general' },
      {
        ...mockChannel,
        id: 'voice-other-server',
        type: 'voice',
        server_id: 'server-2',
        name: 'Elsewhere',
      },
    ],
  });
}

describe('VoiceParticipantContextMenu', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    useUserStore.setState({ user: { id: SELF_ID, username: 'me' } as never });
    seedVoiceChannels();
  });

  it('renders the participant display name in the header', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.getByText('Test User 2')).toBeInTheDocument();
  });

  it('always shows View Profile, Send DM, and Friend Request for a non-self target', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.getByText('View Profile')).toBeInTheDocument();
    expect(screen.getByText('Send DM')).toBeInTheDocument();
    expect(screen.getByText('Send Friend Request')).toBeInTheDocument();
  });

  it('shows Mute when actor holds MUTE_MEMBERS', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS | MUTE_MEMBERS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.getByText('Mute')).toBeInTheDocument();
  });

  it('hides Mute when actor lacks MUTE_MEMBERS', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.queryByText('Mute')).not.toBeInTheDocument();
  });

  it('shows Move to + Disconnect when actor holds MOVE_MEMBERS', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS | MOVE_MEMBERS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.getByText('Move to')).toBeInTheDocument();
    expect(screen.getByText('Disconnect')).toBeInTheDocument();
  });

  it('hides Move to + Disconnect when actor lacks MOVE_MEMBERS', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.queryByText('Move to')).not.toBeInTheDocument();
    expect(screen.queryByText('Disconnect')).not.toBeInTheDocument();
  });

  it('hides ALL voice actions when targeting self', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS } });
    render(
      <VoiceParticipantContextMenu
        {...baseProps}
        participant={{ ...participant, userId: SELF_ID }}
      />
    );
    expect(screen.queryByText('Mute')).not.toBeInTheDocument();
    expect(screen.queryByText('Move to')).not.toBeInTheDocument();
    expect(screen.queryByText('Disconnect')).not.toBeInTheDocument();
    // Self-targeted: Send DM / Friend Request also hidden.
    expect(screen.queryByText('Send DM')).not.toBeInTheDocument();
  });

  it('Move-to submenu lists only same-server voice channels except the current one', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS | MOVE_MEMBERS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    fireEvent.click(screen.getByText('Move to'));
    // Other same-server voice channel appears.
    expect(screen.getByText('Other Voice')).toBeInTheDocument();
    // Current channel, text channel, and other-server channel do NOT.
    expect(screen.queryByText('Current')).not.toBeInTheDocument();
    expect(screen.queryByText('general')).not.toBeInTheDocument();
    expect(screen.queryByText('Elsewhere')).not.toBeInTheDocument();
  });

  it('clicking a Move-to target calls moveVoiceParticipant with the target channel', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS | MOVE_MEMBERS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    fireEvent.click(screen.getByText('Move to'));
    fireEvent.click(screen.getByText('Other Voice'));
    expect(mockMove).toHaveBeenCalledWith(SERVER_ID, TARGET_ID, OTHER_VOICE);
  });

  it('clicking Disconnect calls disconnectVoiceParticipant', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS | MOVE_MEMBERS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    fireEvent.click(screen.getByText('Disconnect'));
    expect(mockDisconnect).toHaveBeenCalledWith(SERVER_ID, TARGET_ID);
  });

  it('shows Kick/Ban only when handlers AND permissions are present', () => {
    const onKick = vi.fn();
    const onBan = vi.fn();
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS } });
    render(
      <VoiceParticipantContextMenu
        {...baseProps}
        onKick={onKick}
        onBan={onBan}
        ownerUserId={SELF_ID}
      />
    );
    expect(screen.getByText('Kick')).toBeInTheDocument();
    expect(screen.getByText('Ban')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Kick'));
    expect(onKick).toHaveBeenCalled();
  });

  it('hides Kick/Ban when no handlers supplied even with permission', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS } });
    render(<VoiceParticipantContextMenu {...baseProps} />);
    expect(screen.queryByText('Kick')).not.toBeInTheDocument();
    expect(screen.queryByText('Ban')).not.toBeInTheDocument();
  });
});
