import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import SendFriendRequestButton from '@/renderer/components/Members/SendFriendRequestButton';
import { useUserStore } from '@/renderer/stores/userStore';
import { useFriendStore, type Friend, type FriendRequest } from '@/renderer/stores/friendStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  // friendStore.sendRequest reads error bodies via safeJson — provide a
  // light real-ish impl so the error-path test surfaces the API's message.
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
}));
import { apiFetch } from '@/renderer/services/apiClient';
const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;

const SELF_ID = 'self-1';
const OTHER_ID = 'other-2';

function setSelf() {
  useUserStore.setState({
    user: {
      id: SELF_ID,
      username: 'me',
      email: 'me@test.com',
      email_verified: true,
    },
  });
}

const friend = (userId: string): Friend => ({
  id: `f-${userId}`,
  userId,
  username: userId,
  status: 'online',
});

const pending = (toUserId: string): FriendRequest => ({
  id: `r-${toUserId}`,
  fromUserId: SELF_ID,
  fromUsername: 'me',
  toUserId,
  toUsername: toUserId,
  direction: 'sent',
  createdAt: '2026-01-01T00:00:00Z',
});

describe('SendFriendRequestButton', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    setSelf();
  });

  it('renders nothing for the user themselves', () => {
    const { container } = render(<SendFriendRequestButton userId={SELF_ID} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing when userId is undefined', () => {
    const { container } = render(<SendFriendRequestButton userId={undefined} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders an inert "Friends" label when already friends', () => {
    useFriendStore.setState({ friends: [friend(OTHER_ID)] });
    render(<SendFriendRequestButton userId={OTHER_ID} />);
    const btn = screen.getByText('Friends');
    expect(btn).toBeDisabled();
  });

  it('renders an inert "Request Pending" label when a request is pending', () => {
    useFriendStore.setState({ pendingRequests: [pending(OTHER_ID)] });
    render(<SendFriendRequestButton userId={OTHER_ID} />);
    expect(screen.getByText('Request Pending')).toBeDisabled();
  });

  it('renders an actionable "Send Friend Request" for a stranger', () => {
    render(<SendFriendRequestButton userId={OTHER_ID} />);
    const btn = screen.getByRole('button', { name: 'Send friend request' });
    expect(btn).not.toBeDisabled();
    expect(btn).toHaveTextContent('Send Friend Request');
  });

  it('sends on click and shows the transient "Request Sent!" state', async () => {
    mockApiFetch.mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({}) });
    render(<SendFriendRequestButton userId={OTHER_ID} />);

    fireEvent.click(screen.getByRole('button', { name: 'Send friend request' }));

    await waitFor(() => expect(screen.getByText('Request Sent!')).toBeInTheDocument());
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/friends/request',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('fires onSent after a successful send', async () => {
    vi.useFakeTimers();
    mockApiFetch.mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({}) });
    const onSent = vi.fn();
    render(<SendFriendRequestButton userId={OTHER_ID} onSent={onSent} />);

    fireEvent.click(screen.getByRole('button', { name: 'Send friend request' }));
    // Flush the send() promise, then the 600ms onSent defer.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    expect(onSent).toHaveBeenCalled();
    vi.useRealTimers();
  });

  it('shows the error message inline when the send fails', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'Privacy: not allowed' }),
    });
    render(<SendFriendRequestButton userId={OTHER_ID} />);

    fireEvent.click(screen.getByRole('button', { name: 'Send friend request' }));

    await waitFor(() => expect(screen.getByText('Privacy: not allowed')).toBeInTheDocument());
  });
});
