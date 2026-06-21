import React from 'react';
import { render, screen } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/UserFrameBar.css', () => ({}));
vi.mock('@/renderer/components/Voice/ParticipantTile.css', () => ({}));

vi.mock('@/renderer/components/Voice/ParticipantTile', () => ({
  default: ({
    participant,
    isLocal,
    compact,
  }: {
    participant: { userId: string; username: string };
    isLocal?: boolean;
    compact?: boolean;
  }) => (
    <div data-testid={`tile-${participant.userId}`} data-local={isLocal} data-compact={compact}>
      {participant.username}
    </div>
  ),
}));

vi.mock('@/renderer/components/Voice/useVoiceMagnification', () => ({
  useVoiceMagnification: () => ({}),
}));

import UserFrameBar from '@/renderer/components/Voice/UserFrameBar';

describe('UserFrameBar', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useUserStore.setState({
      user: {
        id: 'local-user',
        username: 'me',
        email: '',
        display_name: 'Me',
        bio: null,
        avatar_url: null,
        header_image_url: null,
        links: [],
        email_verified: false,
        age_verified: true,
        created_at: '',
        updated_at: '',
      },
    });
  });

  it('renders the user frame bar container with correct height', () => {
    useVoiceStore.setState({ participants: {} });
    const { container } = render(<UserFrameBar height={120} />);
    const bar = container.querySelector('.user-frame-bar');
    expect(bar).toBeInTheDocument();
    expect(bar?.getAttribute('style')).toContain('height: 120px');
  });

  it('renders a compact ParticipantTile for each participant', () => {
    useVoiceStore.setState({
      participants: {
        'local-user': {
          userId: 'local-user',
          username: 'me',
          displayName: 'Me',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
        'remote-1': {
          userId: 'remote-1',
          username: 'alice',
          displayName: 'Alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    render(<UserFrameBar height={120} />);
    expect(screen.getByTestId('tile-local-user')).toBeInTheDocument();
    expect(screen.getByTestId('tile-remote-1')).toBeInTheDocument();
    // All tiles should be compact
    expect(screen.getByTestId('tile-local-user')).toHaveAttribute('data-compact', 'true');
  });

  it('marks local user tile as isLocal', () => {
    useVoiceStore.setState({
      participants: {
        'local-user': {
          userId: 'local-user',
          username: 'me',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    render(<UserFrameBar height={120} />);
    expect(screen.getByTestId('tile-local-user')).toHaveAttribute('data-local', 'true');
  });

  it('sorts video-on users first', () => {
    useVoiceStore.setState({
      participants: {
        'user-a': {
          userId: 'user-a',
          username: 'alpha',
          displayName: 'Alpha',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
        'user-b': {
          userId: 'user-b',
          username: 'beta',
          displayName: 'Beta',
          isMuted: false,
          isDeafened: false,
          isVideoOn: true,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    const { container } = render(<UserFrameBar height={120} />);
    const tiles = container.querySelectorAll('[data-testid^="tile-"]');
    // Video-on user (beta) should be first
    expect(tiles[0]).toHaveAttribute('data-testid', 'tile-user-b');
    expect(tiles[1]).toHaveAttribute('data-testid', 'tile-user-a');
  });

  it('renders no tiles when participants is empty', () => {
    useVoiceStore.setState({ participants: {} });
    const { container } = render(<UserFrameBar height={120} />);
    expect(container.querySelectorAll('[data-testid^="tile-"]').length).toBe(0);
  });
});
