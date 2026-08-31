import { act, render, screen, fireEvent, waitFor, within, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  useFriendStore,
  type Friend,
  type FriendRequest,
} from '@/renderer/stores/chat/friendStore';
import { useFriendOrgStore } from '@/renderer/stores/chat/friendOrgStore';
import { useLayoutStore } from '@/renderer/stores/ui/layoutStore';
import { DockOverlayProvider, DockShell } from '@/renderer/components/Layout/DockShell';
import { vi } from 'vitest';

// Mock child components that are heavy or have their own dependencies
vi.mock('@/renderer/components/DirectMessages/AddFriendModal', async () => {
  const { default: Modal } = await import('@/renderer/components/ui/Modal');
  return {
    default: ({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) => (
      <Modal isOpen={isOpen} onClose={onClose} title="Add Friend">
        <div data-testid="add-friend-modal">
          <button onClick={onClose}>Close Modal</button>
        </div>
      </Modal>
    ),
  };
});

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

vi.mock('@/renderer/utils/ui/schemeColors', () => ({
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
import FriendsFlexSpace from '@/renderer/components/DirectMessages/FriendsFlexSpace';

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

  it('centers and scales the empty-state icon for the compact rail', () => {
    const { container } = render(<FriendsList compact onFriendClick={mockOnFriendClick} />);
    const emptyState = container.querySelector('.friends-list-empty');
    const icon = emptyState?.querySelector('svg');

    expect(emptyState).toHaveClass('friends-list-empty--compact');
    expect(icon).toHaveAttribute('width', '20');
    expect(icon).toHaveAttribute('height', '20');
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
      expect(acceptBtn).toHaveAccessibleName('Accepting friend request from Bob');
      expect(declineBtn).toHaveAccessibleName('Decline friend request from Bob');
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

  it('keeps an Add Friend modal owned by its trigger while a transient dock is open', () => {
    vi.useFakeTimers();
    useLayoutStore.getState().setSidebarPinned('dm', 'right', false);

    const { container } = render(
      <DockOverlayProvider>
        <DockShell
          context="dm"
          side="right"
          label="Friends"
          header={null}
          renderBody={(compact) => (
            <FriendsList compact={compact} onFriendClick={mockOnFriendClick} />
          )}
        />
      </DockOverlayProvider>
    );
    const lip = screen.getByRole('button', { name: 'Open Friends sidebar' });
    fireEvent.focus(lip);
    const trigger = screen.getByRole('button', { name: 'Add Friend' });
    const surface = container.querySelector('.dock-shell__surface') as HTMLElement;

    expect(trigger.id).not.toBe('');
    trigger.focus();
    fireEvent.click(trigger);

    const dialog = screen.getByRole('dialog', { name: 'Add Friend' });
    expect(dialog.closest('.modal-overlay')).toHaveAttribute('data-dock-focus-owner', trigger.id);

    fireEvent.mouseLeave(surface);
    act(() => vi.runAllTimers());
    expect(surface).toHaveAttribute('data-state', 'open');
    vi.useRealTimers();
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

  // --- Compact category rail ---

  it('preserves standard invisible friends in Online without offline dimming', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-invisible',
          userId: 'u-invisible',
          displayName: 'Invisible',
          status: 'invisible',
        }),
      ],
    });

    render(<FriendsList onFriendClick={mockOnFriendClick} />);

    expect(
      screen.getByText('Online').closest('button')?.querySelector('.friend-category-count')
        ?.textContent
    ).toBe('1');
    expect(
      screen.getByText('Offline').closest('button')?.querySelector('.friend-category-count')
        ?.textContent
    ).toBe('0');
    expect(screen.getByRole('button', { name: 'Invisible' })).not.toHaveClass('offline');
  });

  it('renders compact category triggers in the persisted section order', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({ id: 'f-online', userId: 'u-online', displayName: 'Alice', status: 'online' }),
        makeFriend({ id: 'f-offline', userId: 'u-offline', displayName: 'Bob', status: 'offline' }),
        makeFriend({ id: 'f-work', userId: 'u-work', displayName: 'Cara', status: 'dnd' }),
      ],
      pendingRequests: [makeRequest()],
    });
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [
        { id: 'cat-work', name: 'Work', emoji: '🛠️', color: null, memberIds: ['u-work'] },
      ],
      sectionOrder: ['offline', 'cat-work', 'pending', 'online'],
    });

    const { container } = render(<FriendsList compact onFriendClick={mockOnFriendClick} />);

    const rail = screen.getByRole('navigation', { name: 'Friends categories' });
    expect(
      within(rail)
        .getAllByRole('button')
        .map((button) => button.getAttribute('aria-label'))
    ).toEqual(['Offline — 1', 'Work — 1', 'Pending Requests — 1', 'Online — 1']);
    expect(
      Array.from(container.querySelectorAll('.friends-list--compact button')).map((button) =>
        button.getAttribute('title')
      )
      // #2653 item 2b: the search trigger sits between the header actions and the rail,
      // matching where MemberList puts its own compact search button.
    ).toEqual([
      'Manage categories',
      'Add Friend',
      'Search friends',
      'Offline',
      'Work',
      'Pending Requests',
      'Online',
    ]);
  });

  it('switches compact bubbles with a real pointer sequence and restores the active trigger', async () => {
    const user = userEvent.setup();
    useFriendStore.setState({
      friends: [
        makeFriend({ id: 'f-online', userId: 'u-online', displayName: 'Alice', status: 'online' }),
        makeFriend({ id: 'f-offline', userId: 'u-offline', displayName: 'Bob', status: 'offline' }),
      ],
    });
    render(<FriendsList compact onFriendClick={mockOnFriendClick} />);

    const onlineTrigger = screen.getByRole('button', { name: 'Online — 1' });
    expect(onlineTrigger.id).not.toBe('');
    await user.click(onlineTrigger);
    expect(screen.getByRole('region', { name: 'Online — 1' })).toHaveAttribute(
      'data-dock-focus-owner',
      onlineTrigger.id
    );
    const onlineFocus = vi.spyOn(onlineTrigger, 'focus');

    const offlineTrigger = screen.getByRole('button', { name: 'Offline — 1' });
    await user.click(offlineTrigger);
    expect(onlineFocus).not.toHaveBeenCalled();
    expect(offlineTrigger).toHaveFocus();
    expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Offline — 1' })).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('region', { name: 'Offline — 1' })).not.toBeInTheDocument();
    expect(offlineTrigger).toHaveFocus();
  });

  it('clears the open compact bubble when switching through standard mode', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({ id: 'f-online', userId: 'u-online', displayName: 'Alice', status: 'online' }),
      ],
    });
    const { rerender } = render(<FriendsList compact onFriendClick={mockOnFriendClick} />);

    const originalTrigger = screen.getByRole('button', { name: 'Online — 1' });
    fireEvent.click(originalTrigger);
    expect(screen.getByRole('region', { name: 'Online — 1' })).toBeInTheDocument();

    rerender(<FriendsList onFriendClick={mockOnFriendClick} />);
    expect(
      screen.queryByRole('navigation', { name: 'Friends categories' })
    ).not.toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();

    rerender(<FriendsList compact onFriendClick={mockOnFriendClick} />);
    const replacementTrigger = screen.getByRole('button', { name: 'Online — 1' });
    expect(replacementTrigger).not.toBe(originalTrigger);
    expect(replacementTrigger).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('region', { name: 'Online — 1' })).not.toBeInTheDocument();

    fireEvent.click(replacementTrigger);
    expect(screen.getByRole('region', { name: 'Online — 1' })).toBeInTheDocument();
  });

  it('requires a fresh click after an open pending section is removed and re-added', async () => {
    useFriendStore.setState({ pendingRequests: [makeRequest()] });
    render(<FriendsList compact onFriendClick={mockOnFriendClick} />);

    const originalTrigger = screen.getByRole('button', { name: 'Pending Requests — 1' });
    fireEvent.click(originalTrigger);
    expect(screen.getByRole('region', { name: 'Pending Requests — 1' })).toBeInTheDocument();

    act(() => useFriendStore.setState({ pendingRequests: [] }));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: /^Pending Requests/ })).not.toBeInTheDocument();
    });

    act(() => useFriendStore.setState({ pendingRequests: [makeRequest({ id: 'req-2' })] }));
    const replacementTrigger = screen.getByRole('button', { name: 'Pending Requests — 1' });
    expect(replacementTrigger).not.toBe(originalTrigger);
    expect(replacementTrigger).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('region', { name: 'Pending Requests — 1' })).not.toBeInTheDocument();

    fireEvent.click(replacementTrigger);
    expect(screen.getByRole('region', { name: 'Pending Requests — 1' })).toBeInTheDocument();
  });

  it('reuses pending request actions inside the compact bubble', async () => {
    const acceptRequest = vi.fn().mockResolvedValue(undefined);
    const declineRequest = vi.fn().mockResolvedValue(undefined);
    useFriendStore.setState({
      pendingRequests: [makeRequest()],
      acceptRequest,
      declineRequest,
    });
    render(<FriendsList compact onFriendClick={mockOnFriendClick} />);
    fireEvent.click(screen.getByRole('button', { name: 'Pending Requests — 1' }));

    fireEvent.click(screen.getByTitle('Accept'));
    await waitFor(() => expect(acceptRequest).toHaveBeenCalledWith('req-1'));

    fireEvent.click(screen.getByTitle('Decline'));
    await waitFor(() => expect(declineRequest).toHaveBeenCalledWith('req-1'));
  });

  it('reuses friend profile, context, messaging, and category actions in compact mode', () => {
    useFriendStore.setState({
      friends: [makeFriend({ id: 'f-1', userId: 'u-1', displayName: 'Alice', status: 'online' })],
    });
    const work = useFriendOrgStore.getState().createCategory('Work', '', null);
    const { container } = render(<FriendsList compact onFriendClick={mockOnFriendClick} />);
    fireEvent.click(screen.getByRole('button', { name: 'Online — 1' }));

    const friendRow = screen.getByRole('button', { name: 'Alice' });
    fireEvent.click(friendRow, { clientX: 150, clientY: 250 });
    expect(screen.getByTestId('profile-card')).toHaveAttribute('data-user-id', 'u-1');

    fireEvent.contextMenu(friendRow, { clientX: 100, clientY: 200 });
    fireEvent.click(screen.getByText('Message'));
    expect(mockOnFriendClick).toHaveBeenCalledWith('u-1');

    fireEvent.contextMenu(friendRow, { clientX: 100, clientY: 200 });
    const menu = document.querySelector('.ctx-menu') as HTMLElement;
    fireEvent.click(within(menu).getByText('Move to category'));
    fireEvent.click(within(menu).getByText('Work'));
    expect(categoryOf('u-1')?.id).toBe(work);

    fireEvent.click(screen.getByRole('button', { name: /manage categories/i }));
    expect(screen.getByTestId('category-manager-panel')).toBeInTheDocument();
    expect(container.querySelector('.friends-list--compact')).toBeInTheDocument();
  });

  it('treats offline and invisible compact friends as dimmed but keyboard reachable', () => {
    useFriendStore.setState({
      friends: [
        makeFriend({
          id: 'f-offline',
          userId: 'u-offline',
          displayName: 'Offline',
          status: 'offline',
        }),
        makeFriend({
          id: 'f-invisible',
          userId: 'u-invisible',
          displayName: 'Invisible',
          status: 'invisible',
        }),
      ],
    });
    render(<FriendsList compact onFriendClick={mockOnFriendClick} />);
    fireEvent.click(screen.getByRole('button', { name: 'Offline — 2' }));

    for (const name of ['Offline', 'Invisible']) {
      const row = screen.getByRole('button', { name });
      expect(row).toHaveClass('offline');
      expect(row).toHaveAccessibleDescription('Offline');
      expect(row).not.toHaveAttribute('tabindex', '-1');
    }
  });

  it('keeps existing empty-section rules and does not cap long compact sections', () => {
    const friends = Array.from({ length: 24 }, (_, index) =>
      makeFriend({
        id: `f-${index}`,
        userId: `u-${index}`,
        displayName: `Friend ${index + 1}`,
        status: 'online',
      })
    );
    useFriendStore.setState({ friends });
    useFriendOrgStore.getState().createCategory('Gaming', '', null);
    render(<FriendsList compact onFriendClick={mockOnFriendClick} />);

    expect(screen.queryByRole('button', { name: /^Pending Requests/ })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Offline — 0' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Gaming — 0' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Online — 24' }));
    expect(screen.getAllByRole('button', { name: /^Friend \d+$/ })).toHaveLength(24);
  });
});

// --- #2653 item 2a: the Friends header stops colliding with the dock pin ---

describe('FriendsList header (#2653 item 2a)', () => {
  const seedFriends = () => {
    resetAllStores();
    useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });
    vi.clearAllMocks();
    useFriendStore.setState({
      friends: [makeFriend()],
      pendingRequests: [],
      fetchFriends: vi.fn().mockResolvedValue(undefined),
      fetchRequests: vi.fn().mockResolvedValue(undefined),
    });
  };

  const renderDockedFriends = () =>
    render(
      <DockOverlayProvider>
        <FriendsFlexSpace />
      </DockOverlayProvider>
    );

  beforeEach(() => {
    seedFriends();
    useLayoutStore.setState({
      interfaceLocked: false,
      sidebarProfiles: {
        dm: {
          left: { width: 212, pinned: true },
          right: { width: 294, pinned: true },
        },
        server: {
          left: { width: 318, pinned: true },
          right: { width: 306, pinned: true },
        },
      },
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('gives the truncatable title a title attribute so a clipped label is recoverable', () => {
    render(<FriendsList />);

    expect(screen.getByRole('heading', { name: /Friends/ })).toHaveAttribute('title', 'Friends');
  });

  it('puts the title, the actions and the pin in the dock header — one row, not two', () => {
    const { container } = renderDockedFriends();

    const dockHeader = container.querySelector('.dock-shell__header');
    const headers = container.querySelectorAll('.friends-list-header');

    expect(headers).toHaveLength(1);
    expect(dockHeader?.contains(headers[0])).toBe(true);
    expect(dockHeader?.contains(screen.getByRole('heading', { name: /Friends/ }))).toBe(true);
    expect(dockHeader?.contains(screen.getByRole('button', { name: 'Add Friend' }))).toBe(true);
    expect(dockHeader?.contains(screen.getByRole('button', { name: 'Manage categories' }))).toBe(
      true
    );
    expect(
      dockHeader?.contains(screen.getByRole('button', { name: 'Unpin Friends sidebar' }))
    ).toBe(true);
    // The body must not draw a second header strip below the dock's.
    expect(container.querySelector('.friends-list .friends-list-header')).toBeNull();
  });

  it('keeps the dock header in flow rather than floating it over the list header', () => {
    const { container } = renderDockedFriends();

    const dockHeader = container.querySelector('.dock-shell__header');

    expect(dockHeader).not.toBeNull();
    expect(dockHeader).not.toHaveClass('dock-shell__header--actions-only');
    expect(
      dockHeader?.contains(screen.getByRole('button', { name: 'Unpin Friends sidebar' }))
    ).toBe(true);
  });

  it('still carries the title and actions when the pin is hidden by the interface lock', () => {
    useLayoutStore.setState({ interfaceLocked: true });

    const { container } = renderDockedFriends();

    const dockHeader = container.querySelector('.dock-shell__header');
    expect(dockHeader?.contains(screen.getByRole('heading', { name: /Friends/ }))).toBe(true);
    expect(screen.queryByRole('button', { name: /Friends sidebar$/ })).toBeNull();
  });

  it('omits the compact dock header entirely when the pin is hidden, leaving no empty strip', () => {
    useLayoutStore.setState({ interfaceLocked: true });
    useLayoutStore.setState((state) => ({
      sidebarProfiles: {
        ...state.sidebarProfiles,
        dm: { ...state.sidebarProfiles.dm, right: { width: 120, pinned: true } },
      },
    }));

    const { container } = renderDockedFriends();

    // Compact keeps its own action row in the rail, so the dock header would be an empty
    // padded, bordered strip — the reason the compact header resolver returns null.
    expect(container.querySelector('.dock-shell__header')).toBeNull();
    expect(container.querySelector('.friends-list--compact .friends-list-header')).not.toBeNull();
  });
});

// --- #2653 item 2b: friend search in both presentations, mirroring MemberList ---

describe('FriendsList search (#2653 item 2b)', () => {
  const alice = makeFriend({ id: 'f-1', userId: 'u-1', username: 'alice', displayName: 'Alice' });
  const bob = makeFriend({ id: 'f-2', userId: 'u-2', username: 'bob', displayName: 'Bob' });

  const seed = (friends: Friend[], pendingRequests: FriendRequest[] = []) => {
    useFriendStore.setState({ friends, pendingRequests });
  };

  beforeEach(() => {
    resetAllStores();
    useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });
    vi.clearAllMocks();
    useFriendStore.setState({
      friends: [alice, bob],
      pendingRequests: [],
      fetchFriends: vi.fn().mockResolvedValue(undefined),
      fetchRequests: vi.fn().mockResolvedValue(undefined),
      acceptRequest: vi.fn().mockResolvedValue(undefined),
      declineRequest: vi.fn().mockResolvedValue(undefined),
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('exposes a named search control in the standard presentation', () => {
    render(<FriendsList />);

    expect(screen.getByRole('textbox', { name: 'Search friends' })).toBeVisible();
  });

  it('filters the rendered friends by the term', async () => {
    const user = userEvent.setup();
    render(<FriendsList />);

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'ali');

    expect(screen.getByText('Alice')).toBeVisible();
    expect(screen.queryByText('Bob')).toBeNull();
  });

  it('matches on username as well as display name', async () => {
    const user = userEvent.setup();
    seed([alice, makeFriend({ id: 'f-3', userId: 'u-3', username: 'zephyr', displayName: 'Zed' })]);
    render(<FriendsList />);

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'zephyr');

    expect(screen.getByText('Zed')).toBeVisible();
    expect(screen.queryByText('Alice')).toBeNull();
  });

  it('restores the full list when the term is cleared', async () => {
    const user = userEvent.setup();
    render(<FriendsList />);
    const input = screen.getByRole('textbox', { name: 'Search friends' });

    await user.type(input, 'ali');
    await user.clear(input);

    expect(screen.getByText('Alice')).toBeVisible();
    expect(screen.getByText('Bob')).toBeVisible();
  });

  it('reports no matches rather than leaving the list silently empty', async () => {
    const user = userEvent.setup();
    render(<FriendsList />);

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'zzz');

    expect(screen.getByText('No friends match "zzz"')).toBeVisible();
  });

  it('does not render the search control when there are no friends at all', () => {
    seed([]);
    render(<FriendsList />);

    expect(screen.queryByRole('textbox', { name: 'Search friends' })).toBeNull();
  });

  // The sections the search filters are gated on friends OR categories OR pending requests.
  // Gating the CONTROL on friends alone left a user with only pending requests a filterable
  // panel and nothing to filter it with.
  it('offers search in both presentations when only pending requests exist', () => {
    seed([], [makeRequest({ id: 'req-1', fromUsername: 'bob', fromDisplayName: 'Bob' })]);
    const { rerender } = render(<FriendsList />);

    expect(screen.getByRole('textbox', { name: 'Search friends' })).toBeVisible();

    rerender(<FriendsList compact />);
    expect(screen.getByRole('button', { name: 'Search friends' })).toBeVisible();
  });

  it('filters pending requests for a user with no friends at all', async () => {
    const user = userEvent.setup();
    seed(
      [],
      [
        makeRequest({ id: 'req-1', fromUsername: 'bob', fromDisplayName: 'Bob' }),
        makeRequest({ id: 'req-2', fromUsername: 'cara', fromDisplayName: 'Cara' }),
      ]
    );
    render(<FriendsList />);

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'bob');

    expect(screen.getByText('Bob')).toBeVisible();
    expect(screen.queryByText('Cara')).toBeNull();
  });

  it('explains a zero-match term for a user with no friends at all', async () => {
    const user = userEvent.setup();
    seed([], [makeRequest({ id: 'req-1', fromUsername: 'bob', fromDisplayName: 'Bob' })]);
    render(<FriendsList />);

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'zzz');

    expect(screen.getByText('No friends match "zzz"')).toBeVisible();
  });

  it('offers search when only a category exists', () => {
    seed([]);
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [{ id: 'cat-work', name: 'Work', emoji: '', color: null, memberIds: [] }],
      sectionOrder: ['cat-work'],
    });
    render(<FriendsList />);

    expect(screen.getByRole('textbox', { name: 'Search friends' })).toBeVisible();
  });

  it('reports a zero-match term in the compact popover and on its trigger', async () => {
    const user = userEvent.setup();
    render(<FriendsList compact />);

    await user.click(screen.getByRole('button', { name: 'Search friends' }));
    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'zzz');

    const popover = screen.getByRole('region', { name: 'Search friends' });
    expect(within(popover).getByText('No friends match "zzz"')).toBeVisible();
    // The popover is dismissable, so the trigger has to carry the state once it closes.
    expect(screen.getByRole('button', { name: 'Search friends' })).toHaveAttribute(
      'title',
      'No friends match "zzz"'
    );
  });

  it('opens at most one compact popover at a time', async () => {
    const user = userEvent.setup();
    render(<FriendsList compact />);

    await user.click(screen.getByRole('button', { name: 'Online — 2' }));
    expect(screen.getByRole('region', { name: /^Online/ })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Search friends' }));
    expect(screen.queryByRole('region', { name: /^Online/ })).toBeNull();
    expect(screen.getByRole('region', { name: 'Search friends' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Online — 2' }));
    expect(screen.queryByRole('region', { name: 'Search friends' })).toBeNull();
    expect(screen.getByRole('region', { name: /^Online/ })).toBeInTheDocument();
  });

  it('filters the category sections as well as the built-in ones', async () => {
    const user = userEvent.setup();
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [{ id: 'cat-work', name: 'Work', emoji: '', color: null, memberIds: ['u-1'] }],
      sectionOrder: ['cat-work', 'online', 'offline'],
    });
    render(<FriendsList />);

    const countFor = (label: string) =>
      screen.getByText(label).closest('button')?.querySelector('.friend-category-count')
        ?.textContent;

    expect(countFor('Work')).toBe('1');
    expect(countFor('Online')).toBe('1');

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'bob');

    expect(countFor('Work')).toBe('0');
    expect(countFor('Online')).toBe('1');
    expect(screen.getByText('Bob')).toBeVisible();
    expect(screen.queryByText('Alice')).toBeNull();
  });

  it('filters pending requests but keeps the header badge term-independent (C9)', async () => {
    const user = userEvent.setup();
    seed(
      [alice],
      [
        makeRequest({ id: 'req-1', fromUsername: 'bob', fromDisplayName: 'Bob' }),
        makeRequest({ id: 'req-2', fromUsername: 'cara', fromDisplayName: 'Cara' }),
      ]
    );
    const { container } = render(<FriendsList />);

    const badge = () => container.querySelector('.friends-header-badge');
    expect(badge()?.textContent).toBe('2');
    expect(badge()).not.toHaveAttribute('title');

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'bob');

    // The badge still counts every incoming request — it is not a search result count.
    expect(badge()?.textContent).toBe('2');
    expect(badge()).toHaveAttribute('title', '2 pending — hidden by search');
    // ...but the rows and the section count follow the term.
    expect(screen.getByText('Bob')).toBeVisible();
    expect(screen.queryByText('Cara')).toBeNull();
    expect(
      screen
        .getByText('Pending Requests')
        .closest('button')
        ?.querySelector('.friend-category-count')?.textContent
    ).toBe('1');
  });

  it('drops the pending section entirely when no request matches', async () => {
    const user = userEvent.setup();
    seed([alice], [makeRequest({ id: 'req-1', fromUsername: 'bob', fromDisplayName: 'Bob' })]);
    render(<FriendsList />);

    expect(screen.getByText('Pending Requests')).toBeVisible();

    await user.type(screen.getByRole('textbox', { name: 'Search friends' }), 'ali');

    expect(screen.queryByText('Pending Requests')).toBeNull();
    expect(screen.getByText('Alice')).toBeVisible();
  });

  it('opens friend search to the left and filters the compact rail', async () => {
    const user = userEvent.setup();
    render(<FriendsList compact />);

    const trigger = screen.getByRole('button', { name: 'Search friends' });
    expect(trigger).toHaveAttribute('title', 'Search friends');
    expect(screen.getByRole('button', { name: 'Online — 2' })).toBeInTheDocument();

    await user.click(trigger);

    const popover = screen.getByRole('region', { name: 'Search friends' });
    expect(popover).toHaveAttribute('data-placement', 'left');
    const input = screen.getByRole('textbox', { name: 'Search friends' });
    expect(input).toHaveFocus();

    await user.type(input, 'ali');

    await user.click(screen.getByRole('button', { name: 'Online — 1' }));
    expect(screen.getByRole('button', { name: 'Alice' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Bob' })).toBeNull();
  });

  it('does not render the compact search trigger when there are no friends at all', () => {
    seed([]);
    render(<FriendsList compact />);

    expect(screen.queryByRole('button', { name: 'Search friends' })).toBeNull();
  });

  it('discards the compact search anchor when switching through standard mode', () => {
    const { rerender } = render(<FriendsList compact />);

    fireEvent.click(screen.getByRole('button', { name: 'Search friends' }));
    expect(screen.getByRole('region', { name: 'Search friends' })).toBeInTheDocument();

    rerender(<FriendsList />);
    expect(screen.queryByRole('region', { name: 'Search friends' })).toBeNull();

    rerender(<FriendsList compact />);
    const replacement = screen.getByRole('button', { name: 'Search friends' });
    expect(replacement).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('region', { name: 'Search friends' })).toBeNull();
  });
});

// --- #2653 item 3: the offline group reads as inactive, not as an alert ---

describe('FriendsList offline group (#2653 item 3)', () => {
  beforeEach(() => {
    resetAllStores();
    useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });
    vi.clearAllMocks();
    useFriendStore.setState({
      friends: [
        makeFriend({ id: 'f-1', userId: 'u-1', username: 'alice', displayName: 'Alice' }),
        makeFriend({
          id: 'f-2',
          userId: 'u-2',
          username: 'bob',
          displayName: 'Bob',
          status: 'offline',
        }),
      ],
      pendingRequests: [],
      fetchFriends: vi.fn().mockResolvedValue(undefined),
      fetchRequests: vi.fn().mockResolvedValue(undefined),
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  const trigger = (name: string) => screen.getByRole('button', { name });

  it('renders the offline rail trigger with a moon, not a network-fault glyph', () => {
    render(<FriendsList compact />);

    const offline = trigger('Offline — 1');
    expect(offline.querySelector('.lucide-moon')).not.toBeNull();
    expect(offline.querySelector('.lucide-wifi-off')).toBeNull();
  });

  it('leaves the online group on the Users glyph', () => {
    render(<FriendsList compact />);

    expect(trigger('Online — 1').querySelector('.lucide-users')).not.toBeNull();
  });

  it('mutes only the offline count badge', () => {
    render(<FriendsList compact />);

    expect(trigger('Offline — 1').querySelector('.friends-compact-trigger-count')).toHaveClass(
      'friends-compact-trigger-count--offline'
    );
    expect(trigger('Online — 1').querySelector('.friends-compact-trigger-count')).not.toHaveClass(
      'friends-compact-trigger-count--offline'
    );
  });

  it('still prefers a category emoji over any glyph (regression lock)', () => {
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [{ id: 'cat-work', name: 'Work', emoji: '🛠️', color: null, memberIds: ['u-1'] }],
      sectionOrder: ['cat-work', 'online', 'offline'],
    });
    render(<FriendsList compact />);

    const work = trigger('Work — 1');
    expect(work.querySelector('.friends-compact-emoji')?.textContent).toBe('🛠️');
    expect(work.querySelector('.lucide')).toBeNull();
    // A category is never the offline builtin, so its badge keeps the accent fill.
    expect(work.querySelector('.friends-compact-trigger-count')).not.toHaveClass(
      'friends-compact-trigger-count--offline'
    );
  });
});
