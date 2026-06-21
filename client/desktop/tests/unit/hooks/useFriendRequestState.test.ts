import { renderHook, act, waitFor } from '@testing-library/react';
import { useFriendRequestState } from '@/renderer/hooks/useFriendRequestState';
import { useUserStore } from '@/renderer/stores/userStore';
import { useFriendStore, type Friend, type FriendRequest } from '@/renderer/stores/friendStore';
import { resetAllStores } from '../../helpers/store-helpers';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
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

function friend(userId: string): Friend {
  return { id: `f-${userId}`, userId, username: userId, status: 'online' };
}

function pending(fromUserId: string, toUserId: string): FriendRequest {
  return {
    id: `r-${fromUserId}-${toUserId}`,
    fromUserId,
    fromUsername: fromUserId,
    toUserId,
    toUsername: toUserId,
    direction: fromUserId === SELF_ID ? 'sent' : 'received',
    createdAt: '2026-01-01T00:00:00Z',
  };
}

describe('useFriendRequestState', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    setSelf();
  });

  it('is not visible for the signed-in user themselves', () => {
    const { result } = renderHook(() => useFriendRequestState(SELF_ID));
    expect(result.current.visible).toBe(false);
    expect(result.current.canSend).toBe(false);
  });

  it('is not visible when userId is undefined', () => {
    const { result } = renderHook(() => useFriendRequestState(undefined));
    expect(result.current.visible).toBe(false);
  });

  it('is visible and sendable for a stranger', () => {
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.visible).toBe(true);
    expect(result.current.canSend).toBe(true);
    expect(result.current.label).toBe('Send Friend Request');
  });

  it('reports Friends and is not sendable when already friends', () => {
    useFriendStore.setState({ friends: [friend(OTHER_ID)] });
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.isFriend).toBe(true);
    expect(result.current.canSend).toBe(false);
    expect(result.current.label).toBe('Friends');
  });

  it('reports Request Pending for an outgoing pending request', () => {
    useFriendStore.setState({ pendingRequests: [pending(SELF_ID, OTHER_ID)] });
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.hasPendingRequest).toBe(true);
    expect(result.current.canSend).toBe(false);
    expect(result.current.label).toBe('Request Pending');
  });

  it('reports Request Pending for an incoming pending request', () => {
    useFriendStore.setState({ pendingRequests: [pending(OTHER_ID, SELF_ID)] });
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.hasPendingRequest).toBe(true);
    expect(result.current.canSend).toBe(false);
  });

  it('send() transitions idle → sending → sent and POSTs the request', async () => {
    mockApiFetch.mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({}) });
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.status).toBe('idle');

    await act(async () => {
      await result.current.send();
    });

    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/friends/request',
      expect.objectContaining({ method: 'POST' })
    );
    await waitFor(() => expect(result.current.status).toBe('sent'));
  });

  it('send() transitions to error and captures the message on failure', async () => {
    mockApiFetch.mockRejectedValueOnce(new Error('Privacy: not allowed'));
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));

    await act(async () => {
      await result.current.send();
    });

    await waitFor(() => expect(result.current.status).toBe('error'));
    expect(result.current.errorMessage).toBe('Privacy: not allowed');
  });
});
