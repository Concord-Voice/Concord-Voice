import { act, render, screen, fireEvent } from '../../../test-utils';
import MemberProfileCard from '@/renderer/components/Members/MemberProfileCard';
import { mockMember } from '../../../mocks/fixtures';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { fetchEligibility, peekEligibility } from '@/renderer/services/system/friendEligibility';
import { resetAllStores } from '../../../helpers/store-helpers';

// #1241: the affordance is now gated on server eligibility. Most of these tests
// are about rendering and sending, not about the gate (which has its own suite
// in tests/unit/hooks/useFriendRequestState.test.ts), so the default verdict is
// eligible. The § "eligibility gate" block below overrides it per test.
//
// The defaults are load-bearing: a bare vi.fn() returns undefined, and the hook
// does fetchEligibility(id).then(...), which would throw in every test here.
vi.mock('@/renderer/services/system/friendEligibility', () => ({
  fetchEligibility: vi.fn().mockResolvedValue('eligible'),
  peekEligibility: vi.fn().mockReturnValue('eligible'),
}));

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
    // Re-arm explicitly rather than relying on the factory defaults surviving
    // whatever vi.clearAllMocks() does to them.
    vi.mocked(fetchEligibility).mockResolvedValue('eligible');
    vi.mocked(peekEligibility).mockReturnValue('eligible');
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

  it('renders the ordered Now activity list without duplicating Custom Status', () => {
    useRichPresenceStore.getState().setOtherPresence(mockMember.user_id, {
      category: 'server_voice',
      minimized: false,
      payload: {
        channel_id: '11111111-1111-4111-8111-111111111111',
        channel_name: 'Lobby',
        server_id: '22222222-2222-4222-8222-222222222222',
        server_name: 'Concord',
      },
      updated_at: 1,
    });
    useRichPresenceStore.getState().setOtherPresence(mockMember.user_id, {
      category: 'private_call',
      minimized: false,
      payload: { call_type: 'group', participant_count: 3 },
      updated_at: 1,
    });
    useRichPresenceStore.getState().setCustomText(mockMember.user_id, {
      emoji: '🎧',
      text: 'Listening to music',
    });

    render(<MemberProfileCard {...defaultProps} />);

    const section = screen.getByRole('region', { name: 'Now' });
    expect(section).toBeInTheDocument();
    expect(screen.getAllByText('Custom Status')).toHaveLength(1);
    expect(Array.from(section.querySelectorAll('li')).map((item) => item.textContent)).toEqual([
      expect.stringContaining('In voice'),
      expect.stringContaining('In a group call'),
      expect.stringContaining('Listening to music'),
    ]);
  });

  it('gives each mounted Now section its own labelled heading', () => {
    const secondMember = { ...defaultProps.member, user_id: 'other-user', username: 'otheruser' };
    useRichPresenceStore.getState().setOtherPresence(mockMember.user_id, {
      category: 'server_voice',
      minimized: false,
      payload: {
        channel_id: 'channel-1',
        channel_name: 'Lobby',
        server_id: 'server-1',
        server_name: 'Concord',
      },
      updated_at: 1,
    });
    useRichPresenceStore.getState().setOtherPresence(secondMember.user_id, {
      category: 'private_call',
      minimized: false,
      payload: { call_type: 'dm' },
      updated_at: 1,
    });

    render(
      <>
        <MemberProfileCard {...defaultProps} />
        <MemberProfileCard {...defaultProps} member={secondMember} />
      </>
    );

    const sections = screen.getAllByRole('region', { name: 'Now' });
    expect(sections).toHaveLength(2);
    const headingIds = sections.map((section) => section.getAttribute('aria-labelledby'));
    expect(new Set(headingIds).size).toBe(2);
    for (const [index, section] of sections.entries()) {
      const headingId = headingIds[index];
      expect(headingId).toBeTruthy();
      const heading = document.getElementById(headingId as string);
      expect(heading).toHaveTextContent('Now');
      expect(section.contains(heading)).toBe(true);
    }
  });

  it('reclamps a subscribed card when a live snapshot increases measured height', () => {
    const measuredHeight = vi
      .spyOn(HTMLElement.prototype, 'offsetHeight', 'get')
      .mockImplementation(function () {
        if (!this.classList.contains('member-profile-card')) return 0;
        return this.querySelector('.member-profile-now-list') ? 500 : 100;
      });
    const measuredWidth = vi
      .spyOn(HTMLElement.prototype, 'offsetWidth', 'get')
      .mockImplementation(function () {
        return this.classList.contains('member-profile-card') ? 300 : 0;
      });
    try {
      const { container } = render(
        <MemberProfileCard {...defaultProps} position={{ x: 300, y: 600 }} />
      );
      const card = container.querySelector('.member-profile-card') as HTMLElement;
      const initialTop = Number.parseFloat(card.style.top);

      act(() => {
        useRichPresenceStore.getState().replaceOtherPresence({
          [mockMember.user_id]: {
            server_voice: {
              category: 'server_voice',
              minimized: false,
              payload: {
                channel_id: 'channel-1',
                channel_name: 'Lobby',
                server_id: 'server-1',
                server_name: 'Concord',
              },
              updated_at: 1,
            },
            private_call: {
              category: 'private_call',
              minimized: false,
              payload: { call_type: 'group', participant_count: 3 },
              updated_at: 1,
            },
          },
        });
      });

      const updatedTop = Number.parseFloat(card.style.top);
      expect(updatedTop).toBeLessThan(initialTop);
      expect(updatedTop + 500).toBeLessThanOrEqual(globalThis.innerHeight - 8);
    } finally {
      measuredHeight.mockRestore();
      measuredWidth.mockRestore();
    }
  });

  it('uses a fixed minimized marker and omits source detail values', () => {
    useRichPresenceStore.getState().setOtherPresence(mockMember.user_id, {
      category: 'server_voice',
      minimized: true,
      payload: {
        channel_id: '11111111-1111-4111-8111-111111111111',
        channel_name: 'Hidden Lobby',
        server_id: '22222222-2222-4222-8222-222222222222',
        server_name: 'Hidden Server',
      },
      updated_at: 1,
    });

    const { container } = render(<MemberProfileCard {...defaultProps} />);

    expect(screen.getByText('Details hidden')).toBeInTheDocument();
    expect(container.textContent).not.toContain('Hidden Lobby');
    expect(container.textContent).not.toContain('Hidden Server');
    expect(screen.queryByText('11111111-1111-4111-8111-111111111111')).not.toBeInTheDocument();
    expect(screen.queryByText('22222222-2222-4222-8222-222222222222')).not.toBeInTheDocument();
    for (const element of document.querySelectorAll('*')) {
      for (const attribute of Array.from(element.attributes)) {
        expect(attribute.value).not.toContain('11111111-1111-4111-8111-111111111111');
        expect(attribute.value).not.toContain('22222222-2222-4222-8222-222222222222');
        expect(attribute.value).not.toContain('Hidden Lobby');
        expect(attribute.value).not.toContain('Hidden Server');
      }
    }
    expect(screen.queryByText('Hidden Lobby')).not.toBeInTheDocument();
    expect(screen.queryByText('Hidden Server')).not.toBeInTheDocument();
  });

  it('does not expose private-call identities or minimized participant counts', () => {
    useRichPresenceStore.getState().setOtherPresence(mockMember.user_id, {
      category: 'private_call',
      minimized: true,
      payload: Object.assign(
        { call_type: 'group' as const, participant_count: 7 },
        { participant_names: ['Alice Example', 'Bob Example'], participant_ids: ['alice-id'] }
      ),
      updated_at: 1,
    });

    const { container } = render(<MemberProfileCard {...defaultProps} />);

    expect(container.textContent).not.toContain('Alice Example');
    expect(container.textContent).not.toContain('Bob Example');
    expect(container.textContent).not.toContain('alice-id');
    expect(container.textContent).not.toContain('With 7 people');
    for (const element of container.querySelectorAll('*')) {
      for (const attribute of Array.from(element.attributes)) {
        expect(attribute.value).not.toContain('Alice Example');
        expect(attribute.value).not.toContain('Bob Example');
        expect(attribute.value).not.toContain('alice-id');
        expect(attribute.value).not.toContain('With 7 people');
      }
    }
  });

  it('removes the Now section when the replacement snapshot has no activities', () => {
    useRichPresenceStore.getState().setCustomText(mockMember.user_id, { text: 'Available' });
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('Custom Status')).toBeInTheDocument();

    act(() => {
      useRichPresenceStore.getState().replaceOtherPresence({});
    });

    expect(screen.queryByRole('region', { name: 'Now' })).not.toBeInTheDocument();
  });

  it('tracks category replacement, clear, and store reset without stale activity', () => {
    const userId = mockMember.user_id;
    useRichPresenceStore.getState().setOtherPresence(userId, {
      category: 'server_voice',
      minimized: false,
      payload: {
        channel_id: '11111111-1111-4111-8111-111111111111',
        channel_name: 'Old Lobby',
        server_id: '22222222-2222-4222-8222-222222222222',
        server_name: 'Old Server',
      },
      updated_at: 1,
    });
    render(<MemberProfileCard {...defaultProps} />);
    expect(screen.getByText('Old Lobby · Old Server')).toBeInTheDocument();

    act(() => {
      useRichPresenceStore.getState().setOtherPresence(userId, {
        category: 'server_voice',
        minimized: false,
        payload: {
          channel_id: '33333333-3333-4333-8333-333333333333',
          channel_name: 'New Lobby',
          server_id: '44444444-4444-4444-8444-444444444444',
          server_name: 'New Server',
        },
        updated_at: 2,
      });
    });
    expect(screen.getByText('New Lobby · New Server')).toBeInTheDocument();
    expect(screen.queryByText('Old Lobby · Old Server')).not.toBeInTheDocument();

    act(() => {
      useRichPresenceStore.getState().clearOtherPresence(userId, 'server_voice');
    });
    expect(screen.queryByRole('region', { name: 'Now' })).not.toBeInTheDocument();

    act(() => {
      useRichPresenceStore.getState().setCustomText(userId, { text: 'Reset me' });
    });
    expect(screen.getByRole('region', { name: 'Now' })).toBeInTheDocument();
    act(() => {
      useRichPresenceStore.getState().reset();
    });
    expect(screen.queryByRole('region', { name: 'Now' })).not.toBeInTheDocument();
  });

  it('keeps markup-looking Custom Status text as literal React text', () => {
    const literal = '<img src=x onerror=alert(1)>';
    useRichPresenceStore.getState().setCustomText(mockMember.user_id, { text: literal });

    const { container } = render(<MemberProfileCard {...defaultProps} />);

    expect(screen.getByText(literal)).toBeInTheDocument();
    expect(container.querySelector('img[src="x"]')).not.toBeInTheDocument();
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

  // ── Server eligibility gate (#1241) ──
  //
  // The card's `showActions` is `friendActionVisible || !!onViewFullProfile`, so
  // an ineligible verdict does NOT necessarily suppress the action row — that is
  // intended. The narrower guarantee these tests pin is that
  // SendFriendRequestButton self-gates on the same verdict (`if (!visible)
  // return null`), so the friend-request BUTTON is absent whenever the verdict
  // is ineligible, whatever else the row is carrying.

  it('renders the Send Friend Request button for an eligible verdict', () => {
    // Positive control for the two negatives below: without it, an ineligible
    // assertion could pass merely because the card failed to render at all.
    render(<MemberProfileCard {...defaultProps} />);

    expect(screen.getByRole('button', { name: 'Send friend request' })).toBeInTheDocument();
  });

  it('hides the friend-request button for an ineligible verdict', () => {
    vi.mocked(peekEligibility).mockReturnValue('ineligible');
    vi.mocked(fetchEligibility).mockResolvedValue('ineligible');

    render(<MemberProfileCard {...defaultProps} />);

    // The card itself rendered — the absence below is the gate, not a crash.
    expect(screen.getByText('@testuser')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send friend request' })).not.toBeInTheDocument();
    // Nothing else wanted the row, so it is suppressed entirely.
    expect(document.querySelector('.member-profile-actions')).not.toBeInTheDocument();
  });

  it('keeps View Full Profile while hiding the friend-request button when ineligible', () => {
    vi.mocked(peekEligibility).mockReturnValue('ineligible');
    vi.mocked(fetchEligibility).mockResolvedValue('ineligible');

    render(<MemberProfileCard {...defaultProps} onViewFullProfile={vi.fn()} />);

    expect(screen.queryByRole('button', { name: 'Send friend request' })).not.toBeInTheDocument();
    // The row survives for the other affordance — only the gated button goes.
    expect(document.querySelector('.member-profile-actions')).toBeInTheDocument();
    expect(screen.getByText('View Full Profile')).toBeInTheDocument();
  });
});
