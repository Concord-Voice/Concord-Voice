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

vi.mock('@/renderer/services/friendEligibility', () => ({
  fetchEligibility: vi.fn(),
  peekEligibility: vi.fn(),
}));
import {
  fetchEligibility,
  peekEligibility,
  type EligibilityVerdict,
} from '@/renderer/services/friendEligibility';
const mockFetchEligibility = fetchEligibility as ReturnType<typeof vi.fn>;
const mockPeekEligibility = peekEligibility as ReturnType<typeof vi.fn>;

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
    mockFetchEligibility.mockResolvedValue('eligible' as EligibilityVerdict);
    mockPeekEligibility.mockReturnValue('pending');
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

  // #1241: a stranger is now hidden until the server confirms eligibility, so
  // this waits where it used to assert synchronously. The synchronous assertion
  // WAS the always-visible behaviour this issue removes.
  it('is visible and sendable for an eligible stranger', async () => {
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    await waitFor(() => expect(result.current.visible).toBe(true));
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

// ── #1241: eligibility gating ────────────────────────────────────────────────

describe('useFriendRequestState — eligibility gating (#1241)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    setSelf();
    mockPeekEligibility.mockReturnValue('pending');
  });

  it('hides the affordance while the verdict is in flight', () => {
    mockFetchEligibility.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.visible).toBe(false);
    expect(result.current.canSend).toBe(false);
  });

  it('shows the affordance on eligible', async () => {
    mockFetchEligibility.mockResolvedValue('eligible');
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    await waitFor(() => expect(result.current.visible).toBe(true));
  });

  it('hides on ineligible — the only authoritative hide', async () => {
    mockFetchEligibility.mockResolvedValue('ineligible');
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    await waitFor(() => expect(mockFetchEligibility).toHaveBeenCalled());
    expect(result.current.visible).toBe(false);
    expect(result.current.canSend).toBe(false);
  });

  // Degrade OPEN: the server is the only authority, so a shown button that
  // 403s is recoverable while a wrongly hidden one is an undiagnosable dead end.
  it('degrades open on unknown', async () => {
    mockFetchEligibility.mockResolvedValue('unknown');
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    await waitFor(() => expect(result.current.visible).toBe(true));
  });

  it('never probes for self', () => {
    renderHook(() => useFriendRequestState(SELF_ID));
    expect(mockFetchEligibility).not.toHaveBeenCalled();
  });

  it('never probes when already friends, and stays visible', () => {
    useFriendStore.setState({ friends: [friend(OTHER_ID)] });
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(mockFetchEligibility).not.toHaveBeenCalled();
    expect(result.current.visible).toBe(true);
    expect(result.current.label).toBe('Friends');
  });

  it('never probes when a request is already pending, and stays visible', () => {
    useFriendStore.setState({ pendingRequests: [pending(OTHER_ID, SELF_ID)] });
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(mockFetchEligibility).not.toHaveBeenCalled();
    expect(result.current.visible).toBe(true);
  });

  it('never probes when userId is undefined', () => {
    renderHook(() => useFriendRequestState(undefined));
    expect(mockFetchEligibility).not.toHaveBeenCalled();
  });

  // Context menus must not mutate their item set after paint: a row appearing
  // is a layout shift, a focus-order change (WCAG 2.4.3), and invisible to a
  // screen reader that already announced the item count (4.1.3).
  it('freezeAtOpen shows on a cold cache and never flips afterwards', async () => {
    mockPeekEligibility.mockReturnValue('pending');
    mockFetchEligibility.mockResolvedValue('ineligible');
    const { result, rerender } = renderHook(() =>
      useFriendRequestState(OTHER_ID, { freezeAtOpen: true })
    );
    expect(result.current.visible).toBe(true);
    await waitFor(() => expect(mockFetchEligibility).toHaveBeenCalled());
    rerender();
    expect(result.current.visible).toBe(true);
  });

  it('freezeAtOpen honours a warm ineligible verdict at mount', () => {
    mockPeekEligibility.mockReturnValue('ineligible');
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID, { freezeAtOpen: true }));
    expect(result.current.visible).toBe(false);
  });

  it('freezeAtOpen honours a warm eligible verdict at mount', () => {
    mockPeekEligibility.mockReturnValue('eligible');
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID, { freezeAtOpen: true }));
    expect(result.current.visible).toBe(true);
  });

  // The common double-mount: MemberProfileCard renders SendFriendRequestButton
  // for the same user. Without a synchronous seed the second instance would
  // flash hidden for a frame on a verdict already known.
  it('renders immediately from a warm cache, with no hidden frame', () => {
    mockPeekEligibility.mockReturnValue('eligible');
    mockFetchEligibility.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.visible).toBe(true);
  });

  it('hides immediately from a warm ineligible cache', () => {
    mockPeekEligibility.mockReturnValue('ineligible');
    mockFetchEligibility.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => useFriendRequestState(OTHER_ID));
    expect(result.current.visible).toBe(false);
  });

  it('does not peek for a short-circuited user', () => {
    useFriendStore.setState({ friends: [friend(OTHER_ID)] });
    renderHook(() => useFriendRequestState(OTHER_ID, { freezeAtOpen: true }));
    expect(mockPeekEligibility).not.toHaveBeenCalled();
  });
});

// ── #1241: the subject-identity fence ────────────────────────────────────────
//
// `captured` is a useState SEED, which React runs once per INSTANCE, not once
// per subject. The context menu's close is deferred 150 ms, so right-clicking
// one member row then another moves the menu A -> B without passing through
// null: the component reconciles instead of unmounting. Nothing covered that —
// every pre-existing rerender() re-rendered with the SAME userId.
describe('useFriendRequestState — subject identity fence (#1241)', () => {
  const USER_A = 'user-a';
  const USER_B = 'user-b';

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    setSelf();
  });

  it('re-seeds for a swapped userId, with no frame showing the previous subject', () => {
    const verdictFor = (id: string): EligibilityVerdict =>
      id === USER_A ? 'ineligible' : 'eligible';
    mockPeekEligibility.mockImplementation((id: string) => verdictFor(id));
    mockFetchEligibility.mockImplementation((id: string) => Promise.resolve(verdictFor(id)));

    const { result, rerender } = renderHook(
      ({ userId }: { userId: string }) => useFriendRequestState(userId),
      { initialProps: { userId: USER_A } }
    );
    expect(result.current.visible).toBe(false);

    rerender({ userId: USER_B });

    // Synchronous, deliberately: the re-seed happens DURING render, so B's
    // verdict is in effect before paint. An `await waitFor` here would also
    // pass on an implementation that showed A's verdict for a frame first.
    expect(result.current.visible).toBe(true);
    expect(result.current.canSend).toBe(true);
  });

  it('re-seeds the FROZEN verdict too, so a menu moved A -> B is not stuck on A', () => {
    mockPeekEligibility.mockImplementation((id: string) =>
      id === USER_A ? 'ineligible' : 'eligible'
    );
    mockFetchEligibility.mockReturnValue(new Promise(() => {}));

    const { result, rerender } = renderHook(
      ({ userId }: { userId: string }) => useFriendRequestState(userId, { freezeAtOpen: true }),
      { initialProps: { userId: USER_A } }
    );
    expect(result.current.visible).toBe(false);

    rerender({ userId: USER_B });
    expect(result.current.visible).toBe(true);
  });

  it('a late verdict for the replaced subject cannot overwrite the current one', async () => {
    let resolveA!: (v: EligibilityVerdict) => void;
    mockPeekEligibility.mockReturnValue('pending');
    mockFetchEligibility.mockImplementation((id: string) =>
      id === USER_A
        ? new Promise<EligibilityVerdict>((res) => {
            resolveA = res;
          })
        : Promise.resolve<EligibilityVerdict>('eligible')
    );

    const { result, rerender } = renderHook(
      ({ userId }: { userId: string }) => useFriendRequestState(userId),
      { initialProps: { userId: USER_A } }
    );
    expect(result.current.visible).toBe(false); // A still in flight

    rerender({ userId: USER_B });
    await waitFor(() => expect(result.current.visible).toBe(true));

    // A's authoritative hide lands AFTER the subject moved on. It describes a
    // user this instance no longer renders, so it must be dropped.
    await act(async () => {
      resolveA('ineligible');
    });

    expect(result.current.visible).toBe(true);
  });
});
