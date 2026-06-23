import React from 'react';
import { render, screen, fireEvent } from '../../../test-utils';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/ParticipantTile.css', () => ({}));
vi.mock('@/renderer/components/Voice/ParticipantVolumeRow.css', () => ({}));
vi.mock('@/renderer/components/ui/ContextMenu.css', () => ({}));
vi.mock('@/renderer/components/ui/EnforcementMenuItems.css', () => ({}));
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({}),
}));
vi.mock('@/renderer/hooks/useUserThemeScope', () => ({
  useUserThemeScope: () => ({ scopeProps: { style: {} } }),
}));

import ParticipantTile from '@/renderer/components/Voice/ParticipantTile';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voiceStore';

const makeParticipant = (overrides: Partial<VoiceParticipant> = {}): VoiceParticipant => ({
  userId: 'user-1',
  username: 'testuser',
  displayName: 'Test User',
  isMuted: false,
  isDeafened: false,
  isVideoOn: false,
  isScreenSharing: false,
  isSpeaking: false,
  serverMuted: false,
  serverDeafened: false,
  ...overrides,
});

describe('ParticipantTile', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // The participant menu (VoiceParticipantContextMenu) only renders when the
    // active call's server + channel are known — seed them so the menu opens.
    useVoiceStore.setState({ activeServerId: 'server-1', activeChannelId: 'voice-1' });
  });

  it('renders the display name', () => {
    render(<ParticipantTile participant={makeParticipant()} />);
    expect(screen.getByText('Test User')).toBeInTheDocument();
  });

  it('falls back to username when displayName is undefined', () => {
    render(<ParticipantTile participant={makeParticipant({ displayName: undefined })} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('shows "(You)" suffix when isLocal is true', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal />);
    expect(screen.getByText(/Test User/)).toHaveTextContent('Test User (You)');
  });

  it('does not show "(You)" when isLocal is false', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    expect(screen.getByText('Test User')).toBeInTheDocument();
    expect(screen.queryByText(/\(You\)/)).not.toBeInTheDocument();
  });

  it('shows muted icon when participant is muted', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isMuted: true })} />
    );
    expect(container.querySelector('.participant-tile__status--muted')).toBeInTheDocument();
  });

  it('does not show muted icon when not muted', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isMuted: false })} />
    );
    expect(container.querySelector('.participant-tile__status--muted')).not.toBeInTheDocument();
  });

  it('shows screen share icon when participant is screen sharing', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isScreenSharing: true })} />
    );
    expect(container.querySelector('.participant-tile__status--screen')).toBeInTheDocument();
  });

  it('shows testing indicator when participant is testing audio devices', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isTesting: true })} />
    );
    expect(container.querySelector('.participant-tile__status--testing')).toBeInTheDocument();
    expect(screen.getByTitle('Testing audio devices')).toBeInTheDocument();
  });

  it('renders compact class when compact prop is true', () => {
    const { container } = render(<ParticipantTile participant={makeParticipant()} compact />);
    expect(container.querySelector('.participant-tile--compact')).toBeInTheDocument();
  });

  it('renders avatar fallback with first character of display name', () => {
    render(<ParticipantTile participant={makeParticipant({ avatarUrl: undefined })} />);
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('renders avatar image when avatarUrl is provided', () => {
    render(
      <ParticipantTile
        participant={makeParticipant({ avatarUrl: 'https://example.com/avatar.png' })}
      />
    );
    const img = screen.getByAltText('Test User');
    expect(img).toBeInTheDocument();
    expect(img).toHaveAttribute('src', 'https://example.com/avatar.png');
  });

  it('applies magnification scale when not 1', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant()} magnificationScale={1.12} />
    );
    const tile = container.querySelector('.participant-tile');
    expect(tile?.getAttribute('style')).toContain('scale(1.12)');
  });

  it('does not apply transform when magnificationScale is 1', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant()} magnificationScale={1} />
    );
    const tile = container.querySelector('.participant-tile');
    const style = tile?.getAttribute('style') || '';
    expect(style).not.toContain('scale(');
  });

  it('renders video element when video is on and stream exists', () => {
    const mockStream = {
      id: 'stream-1',
      active: true,
    } as unknown as MediaStream;

    vi.spyOn(HTMLVideoElement.prototype, 'play').mockResolvedValue(undefined);

    const { container } = render(
      <ParticipantTile
        participant={makeParticipant({ isVideoOn: true, videoStream: mockStream })}
      />
    );
    expect(container.querySelector('.participant-tile__video')).toBeInTheDocument();
    expect(container.querySelector('.participant-tile--video')).toBeInTheDocument();
  });

  it('does not render video element when video is off', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isVideoOn: false })} />
    );
    expect(container.querySelector('.participant-tile__video')).not.toBeInTheDocument();
  });

  // ===== Server enforcement indicators =====

  it('shows server-muted indicator with title', () => {
    render(<ParticipantTile participant={makeParticipant({ serverMuted: true })} />);
    expect(screen.getByTitle('Server Muted')).toBeInTheDocument();
  });

  it('shows server-deafened indicator with title', () => {
    render(<ParticipantTile participant={makeParticipant({ serverDeafened: true })} />);
    expect(screen.getByTitle('Server Deafened')).toBeInTheDocument();
  });

  it('prioritizes server-mute over self-mute indicator', () => {
    render(<ParticipantTile participant={makeParticipant({ isMuted: true, serverMuted: true })} />);
    // Server mute indicator should be visible, not self-mute
    expect(screen.getByTitle('Server Muted')).toBeInTheDocument();
  });

  it('shows self-muted indicator when not server-muted', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isMuted: true })} />
    );
    expect(screen.queryByTitle('Server Muted')).not.toBeInTheDocument();
    expect(container.querySelector('.participant-tile__status--muted')).toBeInTheDocument();
  });

  it('prioritizes server-deafened over self-deafened indicator', () => {
    render(
      <ParticipantTile participant={makeParticipant({ isDeafened: true, serverDeafened: true })} />
    );
    expect(screen.getByTitle('Server Deafened')).toBeInTheDocument();
  });

  it('shows lock badge on server-muted indicator', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ serverMuted: true })} />
    );
    expect(container.querySelector('.participant-tile__lock-badge')).toBeInTheDocument();
  });

  it('shows self-deafened indicator without lock badge when not server-deafened', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ isDeafened: true, serverDeafened: false })} />
    );
    expect(container.querySelector('.participant-tile__status--deafened')).toBeInTheDocument();
    expect(container.querySelector('.participant-tile__lock-badge')).not.toBeInTheDocument();
  });

  it('uses smaller icon sizes in compact mode for server-muted', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ serverMuted: true })} compact />
    );
    const statusEl = container.querySelector('.participant-tile__status--server-muted');
    expect(statusEl).toBeInTheDocument();
    // Compact mode renders MicOff at size 10 and Lock at size 6
    const svgs = statusEl?.querySelectorAll('svg');
    expect(svgs?.length).toBe(2);
  });

  it('uses smaller icon sizes in compact mode for server-deafened', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant({ serverDeafened: true })} compact />
    );
    const statusEl = container.querySelector('.participant-tile__status--server-deafened');
    expect(statusEl).toBeInTheDocument();
    const svgs = statusEl?.querySelectorAll('svg');
    expect(svgs?.length).toBe(2);
  });

  it('shows no status overlay when not muted, deafened, or server-enforced', () => {
    const { container } = render(
      <ParticipantTile
        participant={makeParticipant({
          isMuted: false,
          isDeafened: false,
          serverMuted: false,
          serverDeafened: false,
          isScreenSharing: false,
        })}
      />
    );
    expect(container.querySelector('.participant-tile__status--muted')).not.toBeInTheDocument();
    expect(container.querySelector('.participant-tile__status--deafened')).not.toBeInTheDocument();
    expect(
      container.querySelector('.participant-tile__status--server-muted')
    ).not.toBeInTheDocument();
    expect(
      container.querySelector('.participant-tile__status--server-deafened')
    ).not.toBeInTheDocument();
    expect(container.querySelector('.participant-tile__status--screen')).not.toBeInTheDocument();
  });

  it('resolves user accent colors from member store', () => {
    useMemberStore.setState({
      members: [
        {
          user_id: 'user-1',
          server_id: 'server-1',
          username: 'testuser',
          display_name: 'Test User',
          avatar_url: null,
          role: 'member' as const,
          joined_at: '2025-01-01T00:00:00Z',
          color_scheme: JSON.stringify({ scheme: 'hacker' }),
        },
      ],
    });

    const { container } = render(<ParticipantTile participant={makeParticipant()} />);
    // Should render without crashing with custom colors
    expect(container.querySelector('.participant-tile')).toBeInTheDocument();
  });

  // ── Context menu — right-click behavior ────────────────────────────────

  it('right-click on a remote tile prevents default and stops propagation', () => {
    const parentHandler = vi.fn();
    const { container } = render(
      <div onContextMenu={parentHandler}>
        <ParticipantTile participant={makeParticipant()} isLocal={false} />
      </div>
    );
    const tile = container.querySelector('.participant-tile') as HTMLElement;
    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    const preventDefaultSpy = vi.spyOn(event, 'preventDefault');
    const stopPropagationSpy = vi.spyOn(event, 'stopPropagation');
    tile.dispatchEvent(event);
    expect(preventDefaultSpy).toHaveBeenCalled();
    expect(stopPropagationSpy).toHaveBeenCalled();
    // Parent's handler should NOT fire because propagation was stopped
    expect(parentHandler).not.toHaveBeenCalled();
  });

  it('right-click on a local tile stops propagation too (no channel menu leak)', () => {
    const parentHandler = vi.fn();
    const { container } = render(
      <div onContextMenu={parentHandler}>
        <ParticipantTile participant={makeParticipant()} isLocal />
      </div>
    );
    const tile = container.querySelector('.participant-tile') as HTMLElement;
    fireEvent.contextMenu(tile);
    // Parent's handler should NOT fire — we swallow right-clicks even for local
    expect(parentHandler).not.toHaveBeenCalled();
  });

  it('right-click on a remote tile opens the VoiceParticipantContextMenu', () => {
    const { container } = render(
      <ParticipantTile participant={makeParticipant()} isLocal={false} />
    );
    const tile = container.querySelector('.participant-tile') as HTMLElement;
    fireEvent.contextMenu(tile);
    // The menu renders the participant's display name and the volume slider
    expect(screen.getAllByText('Test User').length).toBeGreaterThanOrEqual(1);
    expect(screen.getByLabelText('Participant volume')).toBeInTheDocument();
  });

  it('right-click on a local tile does NOT open the VoiceParticipantContextMenu', () => {
    const { container } = render(<ParticipantTile participant={makeParticipant()} isLocal />);
    const tile = container.querySelector('.participant-tile') as HTMLElement;
    fireEvent.contextMenu(tile);
    // No participant-volume control should exist for local user
    expect(screen.queryByLabelText('Participant volume')).not.toBeInTheDocument();
  });

  it('clicking a remote tile name opens the MemberProfileCard', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    fireEvent.click(screen.getByText('Test User'));
    // The profile card renders the @username row, distinct from the tile label.
    expect(screen.getByText('@testuser')).toBeInTheDocument();
  });

  it('clicking a local tile name does NOT open the MemberProfileCard', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal />);
    fireEvent.click(screen.getByText(/Test User/));
    expect(screen.queryByText('@testuser')).not.toBeInTheDocument();
  });

  // ── Keyboard activation of the interactive name (S6848/S1082) ──

  it('exposes button semantics (role + tabIndex) on a remote tile name', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    const name = screen.getByText('Test User');
    expect(name.getAttribute('role')).toBe('button');
    expect(name.getAttribute('tabindex')).toBe('0');
  });

  it('does NOT expose button semantics on a local tile name', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal />);
    const name = screen.getByText(/Test User/);
    expect(name.getAttribute('role')).toBeNull();
    expect(name.getAttribute('tabindex')).toBeNull();
  });

  it('opens the MemberProfileCard when Enter is pressed on a remote tile name', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    fireEvent.keyDown(screen.getByText('Test User'), { key: 'Enter' });
    expect(screen.getByText('@testuser')).toBeInTheDocument();
  });

  it('opens the MemberProfileCard when Space is pressed on a remote tile name', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    fireEvent.keyDown(screen.getByText('Test User'), { key: ' ' });
    expect(screen.getByText('@testuser')).toBeInTheDocument();
  });

  it('ignores non-activation keys on a remote tile name', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    fireEvent.keyDown(screen.getByText('Test User'), { key: 'a' });
    expect(screen.queryByText('@testuser')).not.toBeInTheDocument();
  });

  it('opens the MemberProfileCard via keyboard on a video-mode name overlay', () => {
    const mockStream = { id: 'stream-1', active: true } as unknown as MediaStream;
    vi.spyOn(HTMLVideoElement.prototype, 'play').mockResolvedValue(undefined);
    render(
      <ParticipantTile
        participant={makeParticipant({ isVideoOn: true, videoStream: mockStream })}
        isLocal={false}
      />
    );
    fireEvent.keyDown(screen.getByText('Test User'), { key: 'Enter' });
    expect(screen.getByText('@testuser')).toBeInTheDocument();
  });

  // ── Keyboard / menu-trigger button — keyboard-accessible alternative to right-click ──

  it('renders a menu-trigger button on remote tiles', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    expect(screen.getByLabelText('Open menu for Test User')).toBeInTheDocument();
  });

  it('does NOT render a menu-trigger button on local tiles', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal />);
    expect(screen.queryByLabelText(/Open menu for/)).not.toBeInTheDocument();
  });

  it('clicking the menu-trigger button opens the ParticipantContextMenu', () => {
    render(<ParticipantTile participant={makeParticipant()} isLocal={false} />);
    const trigger = screen.getByLabelText('Open menu for Test User');
    fireEvent.click(trigger);
    // Same menu content as right-click — verifies onClick → setCtxMenu wiring
    expect(screen.getByLabelText('Participant volume')).toBeInTheDocument();
  });
});
