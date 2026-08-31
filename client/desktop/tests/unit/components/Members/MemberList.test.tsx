import { render, screen, fireEvent, act, userEvent, within } from '../../../test-utils';

// #1241: opening a member context menu prefetches the friend-request
// eligibility verdict. Left unmocked it issues a real apiFetch and consumes the
// mockResolvedValueOnce queued for the ban/kick call under test, so the failure
// path silently gets the generic success response instead. Mock the service so
// these tests exercise ban/kick, not the privacy gate.
vi.mock('@/renderer/services/system/friendEligibility', () => ({
  fetchEligibility: vi.fn().mockResolvedValue('eligible'),
  peekEligibility: vi.fn().mockReturnValue('eligible'),
  prefetchEligibility: vi.fn(),
}));
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useMemberStore, ServerMember } from '@/renderer/stores/chat/memberStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import type { Role } from '@/renderer/types/server';
import { ADMIN_PERMISSIONS } from '@/renderer/utils/policy/permissions';
import { mockUser, mockServer, mockMember, mockMember2 } from '../../../mocks/fixtures';

import MemberList from '@/renderer/components/Members/MemberList';

// Mock apiFetch to control member loading
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
  API_BASE: 'http://localhost:8080',
}));

// Mock MemberProfileCard to avoid complex rendering
vi.mock('@/renderer/components/Members/MemberProfileCard', () => ({
  default: ({
    member,
    onClose,
    onViewFullProfile,
  }: {
    member: { username: string };
    onClose: () => void;
    onViewFullProfile?: () => void;
  }) => (
    <div data-testid="profile-card">
      {member.username}
      <button onClick={onClose}>Close</button>
      {onViewFullProfile && <button onClick={onViewFullProfile}>View Full Profile</button>}
    </div>
  ),
}));

vi.mock('@/renderer/components/Members/UserProfileModal', () => ({
  default: ({ isOpen, member }: { isOpen: boolean; member: ServerMember }) =>
    isOpen ? <div data-testid="full-profile">{member.username}</div> : null,
}));

// Mock MemberContextMenu — expose onBan/onKick so we can trigger the modals
vi.mock('@/renderer/components/Members/MemberContextMenu', () => ({
  default: ({
    member,
    onClose,
    onBan,
    onKick,
  }: {
    member: ServerMember;
    onClose: () => void;
    onBan: (m: ServerMember) => void;
    onKick: (m: ServerMember) => void;
  }) => (
    <div data-testid="context-menu">
      {member.username}
      <button onClick={onClose}>Close</button>
      <button data-testid="ctx-ban" onClick={() => onBan(member)}>
        Ban
      </button>
      <button data-testid="ctx-kick" onClick={() => onKick(member)}>
        Kick
      </button>
    </div>
  ),
}));

const makeMember = (overrides: Partial<ServerMember> = {}): ServerMember => ({
  user_id: 'member-1',
  username: 'alice',
  display_name: 'Alice',
  role: 'member',
  joined_at: '2025-01-01T00:00:00Z',
  roles: [],
  ...overrides,
});

const makeRole = (overrides: Partial<Role> = {}): Role => ({
  id: 'role-1',
  server_id: mockServer.id,
  name: 'Moderators',
  position: 10,
  permissions: '0',
  is_default: false,
  is_managed: false,
  display_separately: true,
  mentionable: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
  ...overrides,
});

const seedCompactMembers = (
  members: ServerMember[],
  statuses: Array<[string, 'online' | 'offline' | 'dnd' | 'invisible']>,
  roles: Role[] = []
) => {
  mockApiFetch.mockReturnValue(new Promise(() => {}));
  useMemberStore.setState({
    members,
    onlineUserIds: new Set(
      statuses.filter(([, status]) => status === 'online' || status === 'dnd').map(([id]) => id)
    ),
    userStatuses: new Map(statuses),
    isLoading: false,
    error: null,
  });
  usePermissionStore.setState({
    serverPermissions: { [mockServer.id]: ADMIN_PERMISSIONS },
    serverRoles: { [mockServer.id]: roles },
  });
};

describe('MemberList', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser });
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    // No RBAC roles configured — members fall into Online/Offline groups
    usePermissionStore.setState({ serverPermissions: {}, serverRoles: {} });
    // Default: return members successfully
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember, mockMember2] }),
    });
  });

  it('renders Members header', () => {
    render(<MemberList />);
    expect(screen.getByText('Members')).toBeInTheDocument();
  });

  it('shows empty state when no members', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [] }),
    });
    render(<MemberList />);
    expect(await screen.findByText('No members')).toBeInTheDocument();
  });

  it('shows loading skeletons when loading', () => {
    // Make fetch never resolve so isLoading stays true
    mockApiFetch.mockReturnValue(new Promise(() => {}));
    render(<MemberList />);
    const skeletons = document.querySelectorAll('.member-skeleton');
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it('shows error state with retry button', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Network error' }),
    });
    render(<MemberList />);
    expect(await screen.findByText('Network error')).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
  });

  it('renders members grouped by role', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember, mockMember2] }),
    });
    render(<MemberList />);
    // Without display_separately RBAC roles, members fall into Online/Offline groups
    expect(await screen.findByText(/Offline/)).toBeInTheDocument();
  });

  it('shows member display name', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    expect(await screen.findByText('Test User')).toBeInTheDocument();
  });

  it('renders avatar initial when no avatar URL', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    await screen.findByText('Test User');
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('opens profile card on member click', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    await screen.findByText('Test User');
    fireEvent.click(screen.getByText('Test User'));
    expect(screen.getByTestId('profile-card')).toBeInTheDocument();
  });

  it('opens context menu on right click', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    await screen.findByText('Test User');
    fireEvent.contextMenu(screen.getByText('Test User'));
    expect(screen.getByTestId('context-menu')).toBeInTheDocument();
  });

  describe('ban confirmation modal', () => {
    it('opens ban modal when onBan is called from context menu', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      // Open context menu
      fireEvent.contextMenu(screen.getByText('Test User'));
      expect(screen.getByTestId('context-menu')).toBeInTheDocument();
      // Click ban in context menu mock
      fireEvent.click(screen.getByTestId('ctx-ban'));
      // Context menu should close, ban modal should open
      expect(screen.queryByTestId('context-menu')).not.toBeInTheDocument();
      expect(screen.getByText(/permanently remove them/)).toBeInTheDocument();
    });

    it('calls ban API and removes member on confirm', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');

      // Set up ban API response after mount fetches have consumed their mocks
      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-ban'));

      const confirmBtn = screen.getByRole('button', { name: 'Ban' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      // Verify ban API was called. The body carries the #1354 purge opt-in,
      // which defaults to false when the checkbox is left alone.
      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/servers/${mockServer.id}/bans/${mockMember.user_id}`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ purge_messages: false }),
        }
      );
      // Member should be removed from store
      const members = useMemberStore.getState().members;
      expect(members.find((m) => m.user_id === mockMember.user_id)).toBeUndefined();
    });

    it('shows error when ban API fails', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: async () => ({ members: [mockMember, mockMember2] }),
      });

      render(<MemberList />);
      await screen.findByText('Test User');

      // Now set up the ban to fail
      mockApiFetch.mockResolvedValueOnce({ ok: false });
      mockSafeJson.mockResolvedValueOnce({ error: 'Hierarchy violation' });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-ban'));

      const confirmBtn = screen.getByRole('button', { name: 'Ban' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      // Error should be displayed in the modal
      expect(screen.getByText('Hierarchy violation')).toBeInTheDocument();
    });

    it('closes ban modal on cancel', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-ban'));
      expect(screen.getByText(/permanently remove them/)).toBeInTheDocument();

      // Click cancel
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByText(/permanently remove them/)).not.toBeInTheDocument();
    });
  });

  describe('kick confirmation modal', () => {
    it('opens kick modal when onKick is called from context menu', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));
      expect(screen.queryByTestId('context-menu')).not.toBeInTheDocument();
      expect(screen.getByText(/can rejoin with a new invite/)).toBeInTheDocument();
    });

    it('calls kick API and removes member on confirm', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');

      // Set up kick API response after mount fetches have consumed their mocks
      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));

      const confirmBtn = screen.getByRole('button', { name: 'Kick' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/servers/${mockServer.id}/members/${mockMember.user_id}`,
        {
          method: 'DELETE',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ purge_messages: false }),
        }
      );
      const members = useMemberStore.getState().members;
      expect(members.find((m) => m.user_id === mockMember.user_id)).toBeUndefined();
    });

    it('shows error when kick API fails', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: async () => ({ members: [mockMember, mockMember2] }),
      });

      render(<MemberList />);
      await screen.findByText('Test User');

      // Now set up the kick to fail
      mockApiFetch.mockResolvedValueOnce({ ok: false });
      mockSafeJson.mockResolvedValueOnce({ error: 'Cannot kick owner' });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));

      const confirmBtn = screen.getByRole('button', { name: 'Kick' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      expect(screen.getByText('Cannot kick owner')).toBeInTheDocument();
    });

    it('closes kick modal on cancel', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));
      expect(screen.getByText(/can rejoin with a new invite/)).toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByText(/can rejoin with a new invite/)).not.toBeInTheDocument();
    });
  });

  describe('compact role bubbles', () => {
    it('opens member search to the left and filters the compact groups', async () => {
      const user = userEvent.setup();
      const alice = makeMember();
      const bob = makeMember({
        user_id: 'bob',
        username: 'bob',
        display_name: 'Bob',
      });
      seedCompactMembers(
        [alice, bob],
        [
          [alice.user_id, 'online'],
          [bob.user_id, 'online'],
        ]
      );
      render(<MemberList compact />);

      const trigger = screen.getByRole('button', { name: 'Search members' });
      expect(trigger).toHaveAttribute('title', 'Search members');
      expect(screen.getByRole('button', { name: 'Online — 2' })).toHaveAttribute('title', 'Online');
      await user.click(trigger);
      const search = screen.getByRole('region', { name: 'Search members' });
      expect(search).toHaveAttribute('data-placement', 'left');
      const input = screen.getByPlaceholderText('Search members...');
      expect(input).toHaveFocus();

      await user.type(input, 'Bob');
      await user.click(screen.getByRole('button', { name: 'Online — 1' }));
      expect(screen.getByRole('button', { name: 'Bob — Online' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Alice — Online' })).not.toBeInTheDocument();
    });

    it('uses highest-role assignment, role emoji/fallback, and presence fallback groups', () => {
      const moderators = makeRole({ id: 'moderators', emoji: '🛡️', position: 20 });
      const helpers = makeRole({ id: 'helpers', name: 'Helpers', position: 10 });
      const alice = makeMember({
        user_id: 'alice',
        roles: [
          {
            role_id: helpers.id,
            role_name: helpers.name,
            position: helpers.position,
            display_separately: true,
          },
          {
            role_id: moderators.id,
            role_name: moderators.name,
            position: moderators.position,
            display_separately: true,
          },
        ],
      });
      const eve = makeMember({
        user_id: 'eve',
        username: 'eve',
        display_name: 'Eve',
        roles: [
          {
            role_id: moderators.id,
            role_name: moderators.name,
            position: moderators.position,
            display_separately: true,
          },
        ],
      });
      const bob = makeMember({
        user_id: 'bob',
        username: 'bob',
        display_name: 'Bob',
        roles: [
          {
            role_id: helpers.id,
            role_name: helpers.name,
            position: helpers.position,
            display_separately: true,
          },
        ],
      });
      const online = makeMember({
        user_id: 'online',
        username: 'online',
        display_name: 'Online Member',
      });
      const offline = makeMember({
        user_id: 'offline',
        username: 'offline',
        display_name: 'Offline Member',
      });
      const invisible = makeMember({
        user_id: 'invisible',
        username: 'invisible',
        display_name: 'Invisible Member',
      });
      seedCompactMembers(
        [alice, eve, bob, online, offline, invisible],
        [
          ['alice', 'online'],
          ['eve', 'dnd'],
          ['bob', 'online'],
          ['online', 'online'],
          ['offline', 'offline'],
          ['invisible', 'invisible'],
        ],
        [helpers, moderators]
      );

      render(<MemberList compact />);

      const rail = screen.getByRole('navigation', { name: 'Member groups' });
      expect(
        within(rail)
          .getAllByRole('button')
          .map((button) => button.getAttribute('aria-label'))
      ).toEqual(['Moderators — 2', 'Helpers — 1', 'Online — 1', 'Offline — 2']);
      expect(
        within(screen.getByRole('button', { name: 'Moderators — 2' })).getByText('🛡️')
      ).toBeVisible();
      expect(
        screen.getByRole('button', { name: 'Helpers — 1' }).querySelector('.lucide-users')
      ).toBeInTheDocument();

      const moderatorsTrigger = screen.getByRole('button', { name: 'Moderators — 2' });
      expect(moderatorsTrigger.id).not.toBe('');
      fireEvent.click(moderatorsTrigger);
      expect(screen.getByRole('region', { name: 'Moderators — 2' })).toHaveAttribute(
        'data-dock-focus-owner',
        moderatorsTrigger.id
      );
      expect(screen.getByRole('button', { name: 'Alice — Online' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Eve — Do Not Disturb' })).toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: 'Helpers — 1' }));
      expect(screen.getByRole('button', { name: 'Bob — Online' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Alice — Online' })).not.toBeInTheDocument();
    });

    it('reuses profile and full-profile handlers from compact member avatars', () => {
      const alice = makeMember();
      seedCompactMembers([alice], [[alice.user_id, 'online']]);
      render(<MemberList compact />);

      fireEvent.click(screen.getByRole('button', { name: 'Online — 1' }));
      fireEvent.click(screen.getByRole('button', { name: 'Alice — Online' }));
      expect(screen.getByTestId('profile-card')).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'View Full Profile' }));
      expect(screen.getByTestId('full-profile')).toHaveTextContent('alice');
    });

    it('reuses right-click context and permitted moderation actions', () => {
      const alice = makeMember();
      seedCompactMembers([alice], [[alice.user_id, 'online']]);
      render(<MemberList compact />);

      fireEvent.click(screen.getByRole('button', { name: 'Online — 1' }));
      fireEvent.contextMenu(screen.getByRole('button', { name: 'Alice — Online' }));
      expect(screen.getByTestId('context-menu')).toBeInTheDocument();
      expect(screen.getByTestId('ctx-ban')).toBeInTheDocument();
      expect(screen.getByTestId('ctx-kick')).toBeInTheDocument();

      fireEvent.click(screen.getByTestId('ctx-ban'));
      expect(screen.getByText(/permanently remove them/)).toBeInTheDocument();
    });

    it('switches one bubble at a time and restores focus after outside or Escape dismissal', async () => {
      const user = userEvent.setup();
      const alice = makeMember();
      const bob = makeMember({
        user_id: 'bob',
        username: 'bob',
        display_name: 'Bob',
      });
      seedCompactMembers(
        [alice, bob],
        [
          [alice.user_id, 'online'],
          [bob.user_id, 'offline'],
        ]
      );
      render(<MemberList compact />);

      const onlineTrigger = screen.getByRole('button', { name: 'Online — 1' });
      await user.click(onlineTrigger);
      expect(screen.getByRole('region', { name: 'Online — 1' })).toBeVisible();
      const onlineFocus = vi.spyOn(onlineTrigger, 'focus');

      const offlineTrigger = screen.getByRole('button', { name: 'Offline — 1' });
      await user.click(offlineTrigger);
      expect(onlineFocus).not.toHaveBeenCalled();
      expect(offlineTrigger).toHaveFocus();
      expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();
      expect(screen.getByRole('region', { name: 'Offline — 1' })).toBeVisible();

      fireEvent.pointerDown(document.body);
      expect(screen.queryByRole('region', { name: 'Offline — 1' })).not.toBeInTheDocument();
      expect(offlineTrigger).toHaveFocus();

      await user.click(onlineTrigger);
      fireEvent.keyDown(document, { key: 'Escape' });
      expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();
      expect(onlineTrigger).toHaveFocus();
    });

    it('discards a detached compact anchor when switching through standard mode', () => {
      const alice = makeMember();
      seedCompactMembers([alice], [[alice.user_id, 'online']]);
      const { rerender } = render(<MemberList compact />);

      fireEvent.click(screen.getByRole('button', { name: 'Online — 1' }));
      expect(screen.getByRole('region', { name: 'Online — 1' })).toBeInTheDocument();

      rerender(<MemberList />);
      expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();

      rerender(<MemberList compact />);
      expect(screen.getByRole('button', { name: 'Online — 1' })).toHaveAttribute(
        'aria-expanded',
        'false'
      );
      expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();
    });

    it('clears a detached group anchor when live presence removes and later restores its key', () => {
      const alice = makeMember();
      seedCompactMembers([alice], [[alice.user_id, 'online']]);
      render(<MemberList compact />);

      const originalTrigger = screen.getByRole('button', { name: 'Online — 1' });
      fireEvent.click(originalTrigger);
      expect(screen.getByRole('region', { name: 'Online — 1' })).toBeInTheDocument();

      act(() => useMemberStore.getState().setUserOffline(alice.user_id));
      expect(originalTrigger).not.toBeInTheDocument();
      expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();

      act(() => useMemberStore.getState().setUserOnline(alice.user_id));
      const replacementTrigger = screen.getByRole('button', { name: 'Online — 1' });
      expect(replacementTrigger).not.toBe(originalTrigger);
      expect(replacementTrigger).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();

      fireEvent.click(replacementTrigger);
      expect(screen.getByRole('region', { name: 'Online — 1' })).toBeVisible();
    });

    it('keeps every member in a long internally scrollable bubble', () => {
      const members = Array.from({ length: 24 }, (_, index) =>
        makeMember({
          user_id: `member-${index}`,
          username: `member-${index + 1}`,
          display_name: `Member ${index + 1}`,
        })
      );
      seedCompactMembers(
        members,
        members.map((member) => [member.user_id, 'online'] as const)
      );
      render(<MemberList compact />);

      fireEvent.click(screen.getByRole('button', { name: 'Online — 24' }));
      const region = screen.getByRole('region', { name: 'Online — 24' });
      expect(screen.getAllByRole('button', { name: /^Member \d+ — Online$/ })).toHaveLength(24);
      // Inline value, not computed — jsdom 30 resolves calc() against the
      // viewport, so toHaveStyle would see "752px" rather than the declaration
      // AttributedPopover actually sets. See the matching note in
      // AttributedPopover.test.tsx.
      expect(region.style.maxHeight).toBe('calc(100vh - 16px)');
      expect(region.querySelector('.attributed-popover__body')).toContainElement(
        screen.getByRole('button', { name: 'Member 24 — Online' })
      );
    });
  });

  // --- #2653 item 3: the offline group reads as inactive, not as an alert ---

  describe('offline group presentation (#2653 item 3)', () => {
    const seedOnlineAndOffline = (roles: Role[] = []) => {
      const alice = makeMember({ user_id: 'alice', username: 'alice', display_name: 'Alice' });
      const bob = makeMember({ user_id: 'bob', username: 'bob', display_name: 'Bob' });
      seedCompactMembers(
        [alice, bob],
        [
          ['alice', 'online'],
          ['bob', 'offline'],
        ],
        roles
      );
    };

    it('renders the offline group with a moon and leaves Online on the Users glyph', () => {
      seedOnlineAndOffline();
      render(<MemberList compact />);

      const offline = screen.getByRole('button', { name: 'Offline — 1' });
      expect(offline.querySelector('.lucide-moon')).not.toBeNull();
      expect(
        screen.getByRole('button', { name: 'Online — 1' }).querySelector('.lucide-users')
      ).not.toBeNull();
    });

    it('keeps the Users fallback on an emoji-less ROLE group, which is not offline', () => {
      // The `Users` glyph is the no-emoji fallback for EVERY group. Swapping it wholesale
      // would put a moon on every role group; only key === 'offline' may take the Moon.
      const helpers = makeRole({ id: 'helpers', name: 'Helpers', position: 10 });
      const alice = makeMember({
        user_id: 'alice',
        username: 'alice',
        display_name: 'Alice',
        roles: [
          {
            role_id: helpers.id,
            role_name: helpers.name,
            position: helpers.position,
            display_separately: true,
          },
        ],
      });
      seedCompactMembers([alice], [['alice', 'offline']], [helpers]);
      render(<MemberList compact />);

      const helpersTrigger = screen.getByRole('button', { name: 'Helpers — 1' });
      expect(helpersTrigger.querySelector('.lucide-users')).not.toBeNull();
      expect(helpersTrigger.querySelector('.lucide-moon')).toBeNull();
      expect(helpersTrigger.querySelector('.member-compact-trigger-count')).not.toHaveClass(
        'member-compact-trigger-count--offline'
      );
    });

    it('mutes only the offline count badge', () => {
      seedOnlineAndOffline();
      render(<MemberList compact />);

      expect(
        screen
          .getByRole('button', { name: 'Offline — 1' })
          .querySelector('.member-compact-trigger-count')
      ).toHaveClass('member-compact-trigger-count--offline');
      expect(
        screen
          .getByRole('button', { name: 'Online — 1' })
          .querySelector('.member-compact-trigger-count')
      ).not.toHaveClass('member-compact-trigger-count--offline');
    });

    it('still prefers a role emoji over the group glyph in the compact rail (#2653 regression lock)', () => {
      const guards = makeRole({ id: 'guards', name: 'Guards', emoji: '⚔️', position: 20 });
      const alice = makeMember({
        user_id: 'alice',
        username: 'alice',
        display_name: 'Alice',
        roles: [
          {
            role_id: guards.id,
            role_name: guards.name,
            position: guards.position,
            display_separately: true,
          },
        ],
      });
      seedCompactMembers([alice], [['alice', 'online']], [guards]);
      const { container } = render(<MemberList compact />);

      expect(container.querySelector('.member-compact-emoji')?.textContent).toBe('⚔️');
      expect(
        screen.getByRole('button', { name: 'Guards — 1' }).querySelector('.lucide')
      ).toBeNull();
    });
  });
});
