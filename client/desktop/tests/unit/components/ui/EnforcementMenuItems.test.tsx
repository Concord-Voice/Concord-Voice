import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { EnforcementMenuItems } from '@/renderer/components/ui/EnforcementMenuItems';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/ui/EnforcementMenuItems.css', () => ({}));

const mockApiFetch = vi.fn().mockResolvedValue({});
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

describe('EnforcementMenuItems', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('should show user-level mute when actor has permission and target is in voice', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={true}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: true,
          canDeafen: false,
          canModerate: true,
        }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Mute')).toBeInTheDocument();
  });

  it('should not show items when target is not in voice and no hard enforcement available', () => {
    const { container } = render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={false}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: false,
          canDeafen: false,
          canModerate: true,
        }}
        onClose={() => {}}
      />
    );
    // Component returns null when nothing to show — container only has the wrapper div
    expect(container.innerHTML).toBe('');
  });

  it('should show Server Mute toggle for moderators', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={false}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: true,
          canDeafen: true,
          canModerate: true,
        }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Server Mute')).toBeInTheDocument();
    expect(screen.getByText('Server Deafen')).toBeInTheDocument();
  });

  it('should show Remove Server Mute when target is already server-muted', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={true}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={false}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: true,
          canDeafen: false,
          canModerate: true,
        }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Remove Server Mute')).toBeInTheDocument();
  });

  it('should show Remove Server Deafen when target is already server-deafened', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={true}
        targetIsMuted={false}
        targetIsInVoice={false}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: false,
          canDeafen: true,
          canModerate: true,
        }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Remove Server Deafen')).toBeInTheDocument();
  });

  it('should only show mute in 1:1 DM context', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={true}
        context={{ type: 'dm_1on1', conversationId: 'conv-1' }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Mute')).toBeInTheDocument();
    expect(screen.queryByText('Server Mute')).not.toBeInTheDocument();
    expect(screen.queryByText('Deafen')).not.toBeInTheDocument();
  });

  it('should show hard enforcement for group DM admin', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={true}
        context={{ type: 'dm_group', conversationId: 'conv-1', isAdmin: true }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Mute')).toBeInTheDocument();
    expect(screen.getByText('Server Mute')).toBeInTheDocument();
    expect(screen.getByText('Server Deafen')).toBeInTheDocument();
  });

  it('should only show soft mute for group DM non-admin', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={true}
        context={{ type: 'dm_group', conversationId: 'conv-1', isAdmin: false }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Mute')).toBeInTheDocument();
    expect(screen.queryByText('Server Mute')).not.toBeInTheDocument();
  });

  it('should show Deafen for server context with canDeafen and target in voice', () => {
    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={true}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: false,
          canDeafen: true,
          canModerate: false,
        }}
        onClose={() => {}}
      />
    );
    expect(screen.getByText('Deafen')).toBeInTheDocument();
    expect(screen.queryByText('Mute')).not.toBeInTheDocument();
  });

  it('should not show DM soft mute when target is not in voice', () => {
    const { container } = render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={false}
        context={{ type: 'dm_1on1', conversationId: 'conv-1' }}
        onClose={() => {}}
      />
    );
    expect(container.innerHTML).toBe('');
  });

  it('logs error when apiFetch throws during enforcement action', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const onClose = vi.fn();
    mockApiFetch.mockRejectedValueOnce(new Error('network error'));

    render(
      <EnforcementMenuItems
        targetUserId="user-1"
        targetServerMuted={false}
        targetServerDeafened={false}
        targetIsMuted={false}
        targetIsInVoice={true}
        context={{
          type: 'server',
          serverId: 's1',
          canMute: true,
          canDeafen: false,
          canModerate: true,
        }}
        onClose={onClose}
      />
    );

    fireEvent.click(screen.getByText('Mute'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Enforcement action failed', 'network error');
    });
    // onClose is still called even after error
    expect(onClose).toHaveBeenCalled();
    consoleSpy.mockRestore();
  });
});
