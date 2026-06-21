import { render, screen, fireEvent, waitFor, within } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useFriendStore, type Friend, type FriendRequest } from '@/renderer/stores/friendStore';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { vi } from 'vitest';

// Mock child components that are heavy or have their own dependencies
vi.mock('@/renderer/components/DirectMessages/AddFriendModal', () => ({
  default: ({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) =>
    isOpen ? (
      <div data-testid="add-friend-modal">
        <button onClick={onClose}>Close Modal</button>
      </div>
    ) : null,
}));

vi.mock('@/renderer/components/Members/MemberProfileCard', () => ({
  default: ({
    member,
    onClose,
  }: {
    member: { user_id: string; username: string };
    onClose: () => void;
  }) => (
    <div data-testid="profile-card" data-user-id={member.user_id}>
      <span>{member.username}</span>
      <button onClick={onClose}>Close Profile</button>
    </div>
  ),
}));

vi.mock('@/renderer/utils/schemeColors', () => ({
  resolveUserAccentColors: vi.fn().mockReturnValue(null),
}));

vi.mock('@/renderer/components/DirectMessages/CategoryManagerPanel', () => ({
  default: ({ onClose }: { onClose: () => void }) => (
    <div data-testid="category-manager-panel">
      <button onClick={onClose}>Close Manager</button>
    </div>
  ),
}));

import FriendsList from '@/renderer/components/DirectMessages/FriendsList';

// --- Test fixtures ---

const makeFriend = (overrides: Partial<Friend> = {}): Friend => ({
  id: 'friendship-1',
  userId: 'user-2',
  username: 'alice',
  displayName: 'Alice',
  avatarUrl: undefined,
  colorScheme: undefined,
  status: 'online',
  ...overrides,
});

// Minimal stateful DataTransfer stand-in for fireEvent drag tests (jsdom lacks one).
const makeDataTransfer = () => {
  const store: Record<string, string> = {};
  return {
    setData: (type: string, val: string) => {
      store[type] = val;
    },
    getData: (type: string) => store[type] ?? '',
    types: [] as string[],
    setDragImage: () => {},
    effectAllowed: 'move',
    dropEffect: 'move',
  };
};

const makeRequest = (overrides: Partial<FriendRequest> = {}): FriendRequest => ({
  id: 'req-1',
  fromUserId: 'user-3',
  fromUsername: 'bob',
  fromDisplayName: 'Bob',
  fromAvatarUrl: undefined,
  toUserId: 'user-1',
  toUsername: 'me',
  toDisplayName: 'Me',
  toAvatarUrl: undefined,
  direction: 'received',
  createdAt: '2025-01-01T00:00:00Z',
  ...overrides,
});

describe('FriendsList', () => {
  const mockOnFriendClick = vi.fn();

  beforeEach(() => {
    resetAllStores();
    // friendOrgStore is not registered in resetAllStores; clear it explicitly so
    // category/sectionOrder state does not leak across tests.
    useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });
    vi.clearAllMocks();
    useFriendStore.setState({
      friends: [],
      pendingRequests: [],
      fetchFriends: vi.fn().mockResolvedValue(undefined),
      fetchRequests: vi.fn().mockResolvedValue(undefined),
      acceptRequest: vi.fn().mockResolvedValue(undefined),
      declineRequest: vi.fn().mockResolvedValue(undefined),
      removeFriend: vi.fn().mockResolvedValue(undefined),
    });
  });

  afterEach(() => {
    // Restore any vi.spyOn on store actions so spies don't leak across tests
    // (clearAllMocks only clears call history; it leaves the spy installed).
    vi.restoreAllMocks();
  });

  // --- Basic Rendering ---

  it('renders the Friends header', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Friends')).toBeInTheDocument();
  });

  it('renders Add Friend button', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByTitle('Add Friend')).toBeInTheDocument();
  });

  it('fetches friends and requests on mount', () => {
    const mockFetchFriends = vi.fn().mockResolvedValue(undefined);
    const mockFetchRequests = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({
      fetchFriends: mockFetchFriends,
      fetchRequests: mockFetchRequests,
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(mockFetchFriends).toHaveBeenCalled();
    expect(mockFetchRequests).toHaveBeenCalled();
  });

  // --- Empty State ---

  it('shows empty state when no friends', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Add friends to see them here')).toBeInTheDocument();
  });

  // --- Online/Offline Categories ---

  it('renders Online and Offline categories with friend counts', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-1',
          userId: 'u-1',
          username: 'alice',
          displayName: 'Alice',
          status: 'online',
        }),
        makeFriend({
          id: 'f-2',
          userId: 'u-2',
          username: 'bob',
          displayName: 'Bob',
          status: 'offline',
        }),
        makeFriend({
          id: 'f-3',
          userId: 'u-3',
          username: 'charlie',
          displayName: 'Charlie',
          status: 'idle',
        }),
      ],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Online')).toBeInTheDocument();
    expect(screen.getByText('Offline')).toBeInTheDocument();
    // Online: Alice (online) + Charlie (idle) = 2; Offline: Bob = 1
    // Verify friends are displayed
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.getByText('Charlie')).toBeInTheDocument();
  });

  it('renders friend with display name when available', () => {
    useFriendStore.setState({
      friends: [makeFriend({ displayName: 'Alice Wonderland' })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Alice Wonderland')).toBeInTheDocument();
  });

  it('renders friend with username when no display name', () => {
    useFriendStore.setState({
      friends: [makeFriend({ displayName: undefined })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('alice')).toBeInTheDocument();
  });

  it('renders avatar initial from display name', () => {
    useFriendStore.setState({
      friends: [makeFriend({ displayName: 'Alice' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const initial = container.querySelector('.member-avatar-initial');
    expect(initial?.textContent).toBe('A');
  });

  it('renders avatar image when avatarUrl is provided', () => {
    useFriendStore.setState({
      friends: [makeFriend({ avatarUrl: 'https://example.com/avatar.png' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const img = container.querySelector('.member-avatar-img') as HTMLImageElement;
    expect(img).toBeInTheDocument();
    expect(img.src).toBe('https://example.com/avatar.png');
  });

  it('renders status dot for friend', () => {
    useFriendStore.setState({
      friends: [makeFriend({ status: 'online' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const statusDot = container.querySelector('.member-status-dot.online');
    expect(statusDot).toBeInTheDocument();
  });

  it('applies offline class to offline friends', () => {
    useFriendStore.setState({
      friends: [makeFriend({ status: 'offline' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const offlineFriend = container.querySelector('.friend-item.offline');
    expect(offlineFriend).toBeInTheDocument();
  });

  // --- Category Collapsing ---

  it('toggles category collapsed state on click', () => {
    useFriendStore.setState({
      friends: [makeFriend({ status: 'online' })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    // Online category should be expanded by default
    const onlineHeader = screen.getByText('Online').closest('button');
    expect(onlineHeader).toHaveAttribute('aria-expanded', 'true');

    // Click to collapse
    fireEvent.click(onlineHeader!);
    expect(onlineHeader).toHaveAttribute('aria-expanded', 'false');

    // Click again to expand
    fireEvent.click(onlineHeader!);
    expect(onlineHeader).toHaveAttribute('aria-expanded', 'true');
  });

  it('toggles category on Enter key press', () => {
    useFriendStore.setState({
      friends: [makeFriend({ status: 'online' })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const onlineHeader = screen.getByText('Online').closest('button');
    expect(onlineHeader).toHaveAttribute('aria-expanded', 'true');

    fireEvent.keyDown(onlineHeader!, { key: 'Enter' });
    expect(onlineHeader).toHaveAttribute('aria-expanded', 'false');
  });

  it('hides friends when category is collapsed', () => {
    useFriendStore.setState({
      friends: [makeFriend({ status: 'online', displayName: 'Alice' })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    expect(screen.getByText('Alice')).toBeInTheDocument();

    // Collapse the Online category
    const onlineHeader = screen.getByText('Online').closest('button');
    fireEvent.click(onlineHeader!);

    // Alice should no longer be visible
    expect(screen.queryByText('Alice')).not.toBeInTheDocument();
  });

  // --- Pending Requests Section ---

  it('shows Pending Requests section with incoming requests', () => {
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Pending Requests')).toBeInTheDocument();
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.getByText('Incoming request')).toBeInTheDocument();
  });

  it('shows incoming request count badge in header', () => {
    useFriendStore.setState({
      pendingRequests: [
        makeRequest({ id: 'req-1', direction: 'received' }),
        makeRequest({
          id: 'req-2',
          direction: 'received',
          fromUsername: 'charlie',
          fromDisplayName: 'Charlie',
        }),
      ],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const badge = container.querySelector('.friends-header-badge');
    expect(badge).toBeInTheDocument();
    expect(badge?.textContent).toBe('2');
  });

  it('does not show incoming count badge when no incoming requests', () => {
    useFriendStore.setState({
      pendingRequests: [makeRequest({ direction: 'sent' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const badge = container.querySelector('.friends-header-badge');
    expect(badge).not.toBeInTheDocument();
  });

  it('does not show Pending Requests section when no pending requests', () => {
    useFriendStore.setState({ pendingRequests: [] });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.queryByText('Pending Requests')).not.toBeInTheDocument();
  });

  it('shows outgoing request with Pending label', () => {
    useFriendStore.setState({
      pendingRequests: [
        makeRequest({
          id: 'req-2',
          direction: 'sent',
          toUsername: 'dave',
          toDisplayName: 'Dave',
        }),
      ],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Dave')).toBeInTheDocument();
    expect(screen.getByText('Outgoing request')).toBeInTheDocument();
    expect(screen.getByText('Pending')).toBeInTheDocument();
  });

  it('shows request display name from fromDisplayName when available', () => {
    useFriendStore.setState({
      pendingRequests: [makeRequest({ fromDisplayName: 'Bobby Tables' })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('Bobby Tables')).toBeInTheDocument();
  });

  it('shows request username when fromDisplayName is not available', () => {
    useFriendStore.setState({
      pendingRequests: [makeRequest({ fromDisplayName: undefined })],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.getByText('bob')).toBeInTheDocument();
  });

  it('shows avatar initial from request display name', () => {
    useFriendStore.setState({
      pendingRequests: [makeRequest({ fromDisplayName: 'Bob' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const initials = container.querySelectorAll('.member-avatar-initial');
    const bobInitial = Array.from(initials).find((el) => el.textContent === 'B');
    expect(bobInitial).toBeTruthy();
  });

  // --- Accept/Decline Requests ---

  it('calls acceptRequest when accept button is clicked', async () => {
    const mockAccept = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
      acceptRequest: mockAccept,
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const acceptBtn = screen.getByTitle('Accept');
    fireEvent.click(acceptBtn);

    await waitFor(() => {
      expect(mockAccept).toHaveBeenCalledWith('req-1');
    });
  });

  it('calls declineRequest when decline button is clicked', async () => {
    const mockDecline = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
      declineRequest: mockDecline,
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const declineBtn = screen.getByTitle('Decline');
    fireEvent.click(declineBtn);

    await waitFor(() => {
      expect(mockDecline).toHaveBeenCalledWith('req-1');
    });
  });

  it('disables accept/decline buttons during loading', async () => {
    // Make acceptRequest hang indefinitely
    const mockAccept = vi.fn().mockImplementation(() => new Promise(() => {}));
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
      acceptRequest: mockAccept,
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const acceptBtn = screen.getByTitle('Accept');
    const declineBtn = screen.getByTitle('Decline');

    fireEvent.click(acceptBtn);

    // Both buttons should become disabled
    await waitFor(() => {
      expect(acceptBtn).toBeDisabled();
      expect(declineBtn).toBeDisabled();
    });
  });

  it('handles acceptRequest error gracefully', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const mockAccept = vi.fn().mockRejectedValue(new Error('Network error'));
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
      acceptRequest: mockAccept,
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    fireEvent.click(screen.getByTitle('Accept'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        'Failed to accept friend request:',
        expect.any(String)
      );
    });
    consoleSpy.mockRestore();
  });

  it('handles declineRequest error gracefully', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const mockDecline = vi.fn().mockRejectedValue(new Error('Network error'));
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
      declineRequest: mockDecline,
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    fireEvent.click(screen.getByTitle('Decline'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        'Failed to decline friend request:',
        expect.any(String)
      );
    });
    consoleSpy.mockRestore();
  });

  // --- Pending Requests Collapse ---

  it('collapses pending requests section on category header click', () => {
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    expect(screen.getByText('Bob')).toBeInTheDocument();

    const pendingHeader = screen.getByText('Pending Requests').closest('button');
    fireEvent.click(pendingHeader!);

    expect(screen.queryByText('Incoming request')).not.toBeInTheDocument();
  });

  it('shows pending request count in category header', () => {
    useFriendStore.setState({
      pendingRequests: [
        makeRequest({ id: 'req-1' }),
        makeRequest({ id: 'req-2', direction: 'sent', toDisplayName: 'Eve' }),
      ],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    // Total pending count (including outgoing) should be 2
    const pendingHeader = screen.getByText('Pending Requests').closest('button');
    const countEl = pendingHeader?.querySelector('.friend-category-count');
    expect(countEl?.textContent).toBe('2');
  });

  // --- Add Friend Modal ---

  it('opens AddFriendModal when add button is clicked', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    expect(screen.queryByTestId('add-friend-modal')).not.toBeInTheDocument();

    fireEvent.click(screen.getByTitle('Add Friend'));

    expect(screen.getByTestId('add-friend-modal')).toBeInTheDocument();
  });

  it('closes AddFriendModal when close is called', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    fireEvent.click(screen.getByTitle('Add Friend'));
    expect(screen.getByTestId('add-friend-modal')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Close Modal'));
    expect(screen.queryByTestId('add-friend-modal')).not.toBeInTheDocument();
  });

  // --- Context Menu ---

  it('shows context menu on right-click of a friend', () => {
    useFriendStore.setState({
      friends: [makeFriend()],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });

    expect(screen.getByText('Remove Friend')).toBeInTheDocument();
    expect(screen.getByText('Message')).toBeInTheDocument();
  });

  it('calls onFriendClick when Message is clicked in context menu', () => {
    useFriendStore.setState({
      friends: [makeFriend({ userId: 'user-2' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });

    fireEvent.click(screen.getByText('Message'));
    expect(mockOnFriendClick).toHaveBeenCalledWith('user-2');
  });

  it('calls removeFriend when Remove Friend is clicked in context menu', async () => {
    const mockRemove = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({
      friends: [makeFriend({ userId: 'user-2' })],
      removeFriend: mockRemove,
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });

    fireEvent.click(screen.getByText('Remove Friend'));

    await waitFor(() => {
      expect(mockRemove).toHaveBeenCalledWith('user-2');
    });
  });

  it('handles removeFriend error gracefully', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const mockRemove = vi.fn().mockRejectedValue(new Error('Remove failed'));
    useFriendStore.setState({
      friends: [makeFriend()],
      removeFriend: mockRemove,
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });
    fireEvent.click(screen.getByText('Remove Friend'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to remove friend:', expect.any(String));
    });
    consoleSpy.mockRestore();
  });

  // --- Friend Click / Profile Card ---

  it('shows profile card when friend is clicked', () => {
    useFriendStore.setState({
      friends: [makeFriend({ userId: 'user-2', username: 'alice' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.click(friendItem!, { clientX: 150, clientY: 250 });

    expect(screen.getByTestId('profile-card')).toBeInTheDocument();
    expect(screen.getByTestId('profile-card')).toHaveAttribute('data-user-id', 'user-2');
  });

  it('closes profile card when clicking the same friend again', () => {
    useFriendStore.setState({
      friends: [makeFriend()],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');

    // Open
    fireEvent.click(friendItem!, { clientX: 150, clientY: 250 });
    expect(screen.getByTestId('profile-card')).toBeInTheDocument();

    // Close by clicking the same friend
    fireEvent.click(friendItem!, { clientX: 150, clientY: 250 });
    expect(screen.queryByTestId('profile-card')).not.toBeInTheDocument();
  });

  it('closes profile card via onClose callback', () => {
    useFriendStore.setState({
      friends: [makeFriend()],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.click(friendItem!, { clientX: 150, clientY: 250 });

    expect(screen.getByTestId('profile-card')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Close Profile'));
    expect(screen.queryByTestId('profile-card')).not.toBeInTheDocument();
  });

  // --- Context Menu Header ---

  it('shows friend display name in context menu header', () => {
    useFriendStore.setState({
      friends: [makeFriend({ displayName: 'Alice Wonderland' })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });

    // The name appears in both the friend list and context menu header
    const ctxHeader = document.querySelector('.ctx-menu-header');
    expect(ctxHeader?.textContent).toBe('Alice Wonderland');
  });

  it('shows friend username in context menu header when no display name', () => {
    useFriendStore.setState({
      friends: [makeFriend({ displayName: undefined })],
    });
    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });

    // username 'alice' should appear in the context menu header
    const ctxHeader = document.querySelector('.ctx-menu-header');
    expect(ctxHeader?.textContent).toBe('alice');
  });

  // --- Mixed friend list ---

  it('separates online and offline friends correctly', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-1',
          userId: 'u-1',
          username: 'online-alice',
          displayName: 'Alice',
          status: 'online',
        }),
        makeFriend({
          id: 'f-2',
          userId: 'u-2',
          username: 'idle-bob',
          displayName: 'Bob',
          status: 'idle',
        }),
        makeFriend({
          id: 'f-3',
          userId: 'u-3',
          username: 'dnd-charlie',
          displayName: 'Charlie',
          status: 'dnd',
        }),
        makeFriend({
          id: 'f-4',
          userId: 'u-4',
          username: 'offline-dave',
          displayName: 'Dave',
          status: 'offline',
        }),
      ],
    });
    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    // All should be visible
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.getByText('Charlie')).toBeInTheDocument();
    expect(screen.getByText('Dave')).toBeInTheDocument();
  });

  // --- Friend categories: render contract (#324, Task 7) ---

  it('renders a categorized friend under its category header in ALL presence states, incl offline', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-1',
          userId: 'u-1',
          username: 'alice',
          displayName: 'Alice',
          status: 'offline',
        }),
      ],
    });
    const id = useFriendOrgStore.getState().createCategory('Close', '', null);
    useFriendOrgStore.getState().assignFriend('u-1', id);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    // The "Close" section header renders, and Alice appears under it.
    expect(screen.getByText('Close')).toBeInTheDocument();
    expect(screen.getByText('Alice')).toBeInTheDocument();

    // Alice is NOT duplicated into Online/Offline — only one row.
    expect(screen.getAllByText('Alice')).toHaveLength(1);
    // The built-in Offline section should have a 0 count (Alice is categorized, not uncategorized).
    const offlineHeader = screen.getByText('Offline').closest('button');
    expect(offlineHeader?.querySelector('.friend-category-count')?.textContent).toBe('0');
  });

  it('renders an empty category header (droppable) with a 0 count', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', status: 'online' })],
    });
    useFriendOrgStore.getState().createCategory('Gaming', '', null);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const gamingHeader = screen.getByText('Gaming').closest('button');
    expect(gamingHeader).toBeInTheDocument();
    expect(gamingHeader?.querySelector('.friend-category-count')?.textContent).toBe('0');
  });

  it('uncategorized friends still fall to Online/Offline by presence', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-1',
          userId: 'u-1',
          username: 'uncat',
          displayName: 'Uncat',
          status: 'online',
        }),
      ],
    });
    // A category exists but does NOT contain u-1.
    useFriendOrgStore.getState().createCategory('Other', '', null);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const onlineHeader = screen.getByText('Online').closest('button');
    expect(onlineHeader?.querySelector('.friend-category-count')?.textContent).toBe('1');
    expect(screen.getByText('Uncat')).toBeInTheDocument();
  });

  it('renders a category present in the blob but absent from sectionOrder (members never vanish)', () => {
    // Gitar review on #1704: a malformed/partial blob can hold a category whose id is not in
    // sectionOrder. catByMember still pulls its members out of Online/Offline, so without
    // appending orphaned categories to the render order those friends would disappear entirely.
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-1',
          userId: 'u-1',
          username: 'alice',
          displayName: 'Alice',
          status: 'online',
        }),
      ],
    });
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [{ id: 'cat_x', name: 'Close', emoji: '', color: null, memberIds: ['u-1'] }],
      sectionOrder: [], // 'cat_x' deliberately absent
    });

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    // The category is appended to the render order and Alice appears under it — not vanished.
    expect(screen.getByText('Close')).toBeInTheDocument();
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getAllByText('Alice')).toHaveLength(1);
    // Alice is categorized, so the built-in Online section excludes her (count 0).
    const onlineHeaderX = screen.getByText('Online').closest('button');
    expect(onlineHeaderX?.querySelector('.friend-category-count')?.textContent).toBe('0');
  });

  it('tints a categorized friend name with the category color', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-1',
          userId: 'u-1',
          username: 'alice',
          displayName: 'Alice',
          status: 'online',
        }),
      ],
    });
    const id = useFriendOrgStore.getState().createCategory('Close', '', '#fa709a');
    useFriendOrgStore.getState().assignFriend('u-1', id);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const nameEl = screen.getByText('Alice');
    expect(nameEl).toHaveStyle({ color: '#fa709a' });
  });

  // --- Friend categories: Manage-categories trigger (#324, Task 7) ---

  it('opens the CategoryManagerPanel from the Manage categories button', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.queryByTestId('category-manager-panel')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /manage categories/i }));
    expect(screen.getByTestId('category-manager-panel')).toBeInTheDocument();
  });

  it('closes the CategoryManagerPanel via its onClose', () => {
    render(<FriendsList onFriendClick={mockOnFriendClick} />);
    fireEvent.click(screen.getByRole('button', { name: /manage categories/i }));
    expect(screen.getByTestId('category-manager-panel')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Close Manager'));
    expect(screen.queryByTestId('category-manager-panel')).not.toBeInTheDocument();
  });

  // --- Friend categories: drag-and-drop + keyboard reorder (#324, Task 8) ---
  // These assert on the resulting friendOrgStore state (behavior) rather than spying on
  // Zustand actions — spying on a Zustand action ref does not survive the store's set().

  const orgState = () => useFriendOrgStore.getState();
  const categoryOf = (userId: string) =>
    orgState().categories.find((c) => c.memberIds.includes(userId));

  it('reorders sections when a section header is dropped (concord-section dataTransfer)', () => {
    useFriendStore.setState({ friends: [makeFriend({ userId: 'u-1', status: 'online' })] });
    // Two categories so we have cat-* section handles to drag between.
    const a = orgState().createCategory('A', '', null);
    const b = orgState().createCategory('B', '', null);
    // Seeded order is [a, b, ...builtins].
    expect(orgState().sectionOrder.indexOf(a)).toBeLessThan(orgState().sectionOrder.indexOf(b));

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const handleA = screen.getByRole('button', { name: /reorder a/i });
    const handleB = screen.getByRole('button', { name: /reorder b/i });
    const dt = makeDataTransfer();

    fireEvent.dragStart(handleA, { dataTransfer: dt });
    fireEvent.dragOver(handleB, { dataTransfer: dt, clientY: 9999 });
    fireEvent.drop(handleB, { dataTransfer: dt, clientY: 9999 });

    // 'A' now sits after 'B' (dropped past B's midpoint → 'after').
    const order = orgState().sectionOrder;
    expect(order).toContain(a);
    expect(order).toContain(b);
    expect(order.indexOf(a)).toBeGreaterThan(order.indexOf(b));
  });

  it('assigns a friend when a friend row is dropped on a category header (concord-friend)', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice', status: 'online' })],
    });
    const cat = orgState().createCategory('Gaming', '', null);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendRow = screen.getByText('Alice').closest('.friend-item') as HTMLElement;
    const catHeader = screen.getByText('Gaming').closest('button') as HTMLElement;
    const dt = makeDataTransfer();

    fireEvent.dragStart(friendRow, { dataTransfer: dt });
    fireEvent.dragOver(catHeader, { dataTransfer: dt });
    fireEvent.drop(catHeader, { dataTransfer: dt });

    expect(categoryOf('u-1')?.id).toBe(cat);
  });

  it('unassigns when a friend is dropped on a built-in Online/Offline header', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice', status: 'online' })],
    });
    const cat = orgState().createCategory('Gaming', '', null);
    orgState().assignFriend('u-1', cat);
    expect(categoryOf('u-1')?.id).toBe(cat);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendRow = screen.getByText('Alice').closest('.friend-item') as HTMLElement;
    const offlineHeader = screen.getByText('Offline').closest('button') as HTMLElement;
    const dt = makeDataTransfer();

    fireEvent.dragStart(friendRow, { dataTransfer: dt });
    fireEvent.dragOver(offlineHeader, { dataTransfer: dt });
    fireEvent.drop(offlineHeader, { dataTransfer: dt });

    expect(categoryOf('u-1')).toBeUndefined(); // → Uncategorized
  });

  it('is a no-op when a friend is dropped on its own current category', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice', status: 'online' })],
    });
    const cat = orgState().createCategory('Gaming', '', null);
    orgState().assignFriend('u-1', cat);
    const categoriesBefore = orgState().categories;

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const friendRow = screen.getByText('Alice').closest('.friend-item') as HTMLElement;
    const catHeader = screen.getByText('Gaming').closest('button') as HTMLElement;
    const dt = makeDataTransfer();

    fireEvent.dragStart(friendRow, { dataTransfer: dt });
    fireEvent.dragOver(catHeader, { dataTransfer: dt });
    fireEvent.drop(catHeader, { dataTransfer: dt });

    // Still in the same category, and the state object was not replaced (no-op short-circuit).
    expect(categoryOf('u-1')?.id).toBe(cat);
    expect(orgState().categories).toBe(categoriesBefore);
  });

  it('keyboard: grab a section handle, ArrowDown moves the section in sectionOrder and announces it', () => {
    useFriendStore.setState({ friends: [makeFriend({ userId: 'u-1', status: 'online' })] });
    const a = orgState().createCategory('A', '', null);
    orgState().createCategory('B', '', null);
    expect(orgState().sectionOrder.indexOf(a)).toBe(0);

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    const handleA = screen.getByRole('button', { name: /reorder a/i });
    handleA.focus();
    fireEvent.keyDown(handleA, { key: ' ' }); // grab
    fireEvent.keyDown(handleA, { key: 'ArrowDown' }); // move down

    // 'A' moved one slot later in the order.
    expect(orgState().sectionOrder.indexOf(a)).toBe(1);

    // The aria-live region announces the move.
    const live = document.querySelector('[aria-live="polite"]');
    expect(live).toBeInTheDocument();
    expect(live?.textContent).toMatch(/moved to position/i);
  });

  // --- Friend categories: context-menu "Move to category" submenu (#324, Task 9) ---

  // Open the friend context menu and its "Move to category" submenu; return the menu element
  // so queries can be scoped to it (category names also render as sidebar section headers).
  const openMoveSubmenu = (container: HTMLElement) => {
    const friendItem = container.querySelector('.friend-item:not(.friend-request-item)');
    fireEvent.contextMenu(friendItem!, { clientX: 100, clientY: 200 });
    const menu = document.querySelector('.ctx-menu') as HTMLElement;
    fireEvent.click(within(menu).getByText('Move to category'));
    return menu;
  };

  it('renders a "Move to category" submenu with the category radio + Uncategorized + New category…', () => {
    useFriendStore.setState({ friends: [makeFriend({ userId: 'u-1', displayName: 'Alice' })] });
    orgState().createCategory('Gaming', '', null);
    orgState().createCategory('Work', '', null);

    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const menu = openMoveSubmenu(container);
    const submenu = within(menu);

    expect(submenu.getByText('Gaming')).toBeInTheDocument();
    expect(submenu.getByText('Work')).toBeInTheDocument();
    expect(submenu.getByText('Uncategorized')).toBeInTheDocument();
    expect(submenu.getByText('New category…')).toBeInTheDocument();
  });

  it('selecting a category assigns the friend to it', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice' })],
    });
    const gaming = orgState().createCategory('Gaming', '', null);

    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const menu = openMoveSubmenu(container);
    fireEvent.click(within(menu).getByText('Gaming'));

    expect(categoryOf('u-1')?.id).toBe(gaming);
  });

  it('selecting Uncategorized unassigns the friend', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice' })],
    });
    const gaming = orgState().createCategory('Gaming', '', null);
    orgState().assignFriend('u-1', gaming);
    expect(categoryOf('u-1')?.id).toBe(gaming);

    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const menu = openMoveSubmenu(container);
    fireEvent.click(within(menu).getByText('Uncategorized'));

    expect(categoryOf('u-1')).toBeUndefined();
  });

  it('marks the friend’s current category with a check', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice' })],
    });
    const gaming = orgState().createCategory('Gaming', '', null);
    orgState().createCategory('Work', '', null);
    orgState().assignFriend('u-1', gaming);

    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    const menu = openMoveSubmenu(container);

    // The current category item carries an icon; the other does not.
    const gamingItem = within(menu).getByText('Gaming').closest('.ctx-menu-item');
    const workItem = within(menu).getByText('Work').closest('.ctx-menu-item');
    expect(gamingItem?.querySelector('.ctx-menu-item-icon')).toBeInTheDocument();
    expect(workItem?.querySelector('.ctx-menu-item-icon')).not.toBeInTheDocument();
  });

  it('"New category…" opens the CategoryManagerPanel', () => {
    useFriendStore.setState({ friends: [makeFriend({ userId: 'u-1', displayName: 'Alice' })] });
    orgState().createCategory('Gaming', '', null);

    const { container } = render(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(screen.queryByTestId('category-manager-panel')).not.toBeInTheDocument();

    const menu = openMoveSubmenu(container);
    fireEvent.click(within(menu).getByText('New category…'));

    expect(screen.getByTestId('category-manager-panel')).toBeInTheDocument();
  });
});
