import { render, screen, fireEvent } from '../../../test-utils';
import MemberProfileCard from '@/renderer/components/Members/MemberProfileCard';
import { mockMember } from '../../../mocks/fixtures';
import { useUserStore } from '@/renderer/stores/userStore';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';
import { resetAllStores } from '../../../helpers/store-helpers';

describe('MemberProfileCard', () => {
  const mockOnClose = vi.fn();
  const defaultProps = {
    member: { ...mockMember, bio: undefined },
    status: 'online' as const,
    position: { x: 300, y: 200 },
    onClose: mockOnClose,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
  });

  it('renders member username and display name', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('Test User')).toBeInTheDocument();
    expect(screen.getByText('@testuser')).toBeInTheDocument();
  });

  it('scopes another user card to their profile theme', () => {
    const member = {
      ...defaultProps.member,
      color_scheme: JSON.stringify({ scheme: 'hacker', themeMode: 'light' }),
    };

    render(<MemberProfileCard {...defaultProps} member={member} />);

    const card = document.querySelector('.member-profile-card');
    expect(card).toHaveAttribute('data-scheme', 'hacker');
    expect(card).toHaveAttribute('data-theme', 'light');
  });

  it('lets the viewer self-card inherit the app theme when profile theme is unset', () => {
    useUserStore.setState({
      user: {
        id: mockMember.user_id,
        username: mockMember.username,
        email: 'me@test.com',
        email_verified: true,
      },
    });

    render(<MemberProfileCard {...defaultProps} />);

    const card = document.querySelector('.member-profile-card');
    expect(card).toHaveAttribute('data-scheme', '');
    expect(card).toHaveAttribute('data-theme', '');
  });

  it('does not apply profile accent overrides to the viewer self-card', () => {
    useUserStore.setState({
      user: {
        id: mockMember.user_id,
        username: mockMember.username,
        email: 'me@test.com',
        email_verified: true,
      },
    });
    const member = {
      ...defaultProps.member,
      color_scheme: JSON.stringify({ scheme: 'hacker', themeMode: 'light' }),
    };

    render(<MemberProfileCard {...defaultProps} member={member} />);

    const card = document.querySelector('.member-profile-card');
    expect(card).toHaveAttribute('data-scheme', '');
    expect(card).toHaveAttribute('data-theme', '');
    expect(screen.getByText('T')).not.toHaveStyle({ color: '#fff' });
    expect((screen.getByText('T') as HTMLElement).style.background).toBe('');
  });

  it('renders role badge', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('Owner')).toBeInTheDocument();
  });

  it('renders status text for online', () => {
    render(<MemberProfileCard {...defaultProps} status="online" />);
    expect(screen.getByText('Online')).toBeInTheDocument();
  });

  it('renders status text for dnd', () => {
    render(<MemberProfileCard {...defaultProps} status="dnd" />);
    expect(screen.getByText('Do Not Disturb')).toBeInTheDocument();
  });

  it('renders "Offline" for invisible status', () => {
    render(<MemberProfileCard {...defaultProps} status="invisible" />);
    expect(screen.getByText('Offline')).toBeInTheDocument();
  });

  it('renders avatar initial when no avatar URL', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('renders avatar image when URL provided', () => {
    const memberWithAvatar = { ...mockMember, avatar_url: 'https://example.com/avatar.png' };
    render(<MemberProfileCard {...defaultProps} member={memberWithAvatar} />);
    const img = screen.getByAltText('testuser');
    expect(img).toBeInTheDocument();
    expect(img).toHaveAttribute('src', 'https://example.com/avatar.png');
  });

  it('renders bio when provided', () => {
    const memberWithBio = { ...mockMember, bio: 'Hello, I am a test user' };
    render(<MemberProfileCard {...defaultProps} member={memberWithBio} />);
    expect(screen.getByText('Hello, I am a test user')).toBeInTheDocument();
    expect(screen.getByText('About')).toBeInTheDocument();
  });

  it('does not render bio section when bio is absent', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.queryByText('About')).not.toBeInTheDocument();
  });

  it('renders joined date', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('Joined')).toBeInTheDocument();
    // The exact date depends on timezone (Jan 1, 2025 or Dec 31, 2024)
    const detailRows = document.querySelectorAll('.member-profile-detail-value');
    const joinedValue = detailRows[detailRows.length - 1];
    expect(joinedValue?.textContent).toMatch(/\w+ \d{1,2}, \d{4}/);
  });

  it('closes on Escape key', () => {
    render(<MemberProfileCard {...defaultProps} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('renders "Offline" for offline status with no lastSeen', () => {
    render(<MemberProfileCard {...defaultProps} status="offline" />);
    expect(screen.getByText('Offline')).toBeInTheDocument();
  });

  it('shows lastSeen for offline user with timestamp', () => {
    // 30 minutes ago
    const thirtyMinsAgo = Math.floor(Date.now() / 1000) - 30 * 60;
    render(<MemberProfileCard {...defaultProps} status="offline" lastSeen={thirtyMinsAgo} />);
    expect(screen.getByText('Last seen 30m ago')).toBeInTheDocument();
  });

  it('shows "Just now" for very recent lastSeen', () => {
    const justNow = Math.floor(Date.now() / 1000);
    render(<MemberProfileCard {...defaultProps} status="offline" lastSeen={justNow} />);
    expect(screen.getByText('Last seen Just now')).toBeInTheDocument();
  });

  it('shows hours for lastSeen within 24h', () => {
    const twoHoursAgo = Math.floor(Date.now() / 1000) - 2 * 60 * 60;
    render(<MemberProfileCard {...defaultProps} status="offline" lastSeen={twoHoursAgo} />);
    expect(screen.getByText('Last seen 2h ago')).toBeInTheDocument();
  });

  it('shows days for lastSeen beyond 24h', () => {
    const threeDaysAgo = Math.floor(Date.now() / 1000) - 3 * 24 * 60 * 60;
    render(<MemberProfileCard {...defaultProps} status="offline" lastSeen={threeDaysAgo} />);
    expect(screen.getByText('Last seen 3d ago')).toBeInTheDocument();
  });

  it('renders member role badge', () => {
    render(<MemberProfileCard {...defaultProps} />);
    const badge = document.querySelector('.member-profile-role-badge');
    expect(badge).toBeInTheDocument();
  });

  it('renders member username with @ prefix', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('@testuser')).toBeInTheDocument();
  });

  // ── Custom Status detail row (#1233) ──

  it('renders a Custom Status row with emoji + text when present in the store', () => {
    useRichPresenceStore
      .getState()
      .setCustomText(mockMember.user_id, { emoji: '🎧', text: 'Listening to music' });
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('Custom Status')).toBeInTheDocument();
    expect(screen.getByText('Listening to music')).toBeInTheDocument();
    expect(screen.getByText('🎧')).toBeInTheDocument();
  });

  it('does not render a Custom Status row when the store has no entry', () => {
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.queryByText('Custom Status')).not.toBeInTheDocument();
  });

  // ── Send Friend Request action (#226) ──

  it('renders the Send Friend Request action for another user', () => {
    // No current user set as this member → not self → affordance visible.
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByRole('button', { name: 'Send friend request' })).toBeInTheDocument();
  });

  it('hides the friend-request action on the viewer’s own card', () => {
    // Current user IS this member → self → affordance hidden. With no
    // onViewFullProfile callback either, the whole action row is suppressed.
    useUserStore.setState({
      user: {
        id: mockMember.user_id,
        username: mockMember.username,
        email: 'me@test.com',
        email_verified: true,
      },
    });
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.queryByRole('button', { name: 'Send friend request' })).not.toBeInTheDocument();
    expect(document.querySelector('.member-profile-actions')).not.toBeInTheDocument();
  });

  it('still renders the action row for self when View Full Profile is available', () => {
    useUserStore.setState({
      user: {
        id: mockMember.user_id,
        username: mockMember.username,
        email: 'me@test.com',
        email_verified: true,
      },
    });
    render(<MemberProfileCard {...defaultProps} onViewFullProfile={vi.fn()} />);
    // Friend button hidden (self) but the row persists for View Full Profile.
    expect(screen.queryByRole('button', { name: 'Send friend request' })).not.toBeInTheDocument();
    expect(screen.getByText('View Full Profile')).toBeInTheDocument();
  });
});
