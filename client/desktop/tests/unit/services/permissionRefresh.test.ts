// #3456 (spec 3.8): the schedule behind `server_permissions_changed` and
// `permissions_changed`. The events carry no permission data, so everything that
// keeps a server toggle from becoming a synchronized refetch burst lives here:
// per-server jitter, drop-while-pending, a trailing debounce for the account
// event, no retry, and teardown on dispose and on an account change.
//
// Oracle: the store actions that perform the reads are replaced by spies, so each
// case asserts WHICH reads the schedule asked for and WHEN, never what the
// network answered. Every guard has a control case where the guarded branch is
// taken, so a guard cannot pass by the schedule doing nothing at all.
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  createJitteredRefetch,
  createPermissionRefresh,
  OWN_REFRESH_DEBOUNCE_MS,
  SERVER_REFRESH_JITTER_MS,
  type PermissionRefresh,
} from '@/renderer/services/system/permissionRefresh';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';

const SERVER_A = '11111111-1111-4111-8111-111111111111';
const SERVER_B = '22222222-2222-4222-8222-222222222222';
const UNKNOWN_SERVER = '99999999-9999-4999-8999-999999999999';
const CH_A1 = 'a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1';
const CH_A2 = 'a2a2a2a2-a2a2-4a2a-8a2a-a2a2a2a2a2a2';
const CH_A3 = 'a3a3a3a3-a3a3-4a3a-8a3a-a3a3a3a3a3a3';
const CH_B1 = 'b1b1b1b1-b1b1-4b1b-8b1b-b1b1b1b1b1b1';

const fetchServerPermissions = vi.fn();
const fetchChannelPermissions = vi.fn();
const evictChannelPermissions = vi.fn();
const evictServerPermissions = vi.fn();
const fetchServers = vi.fn();

function seedServers(ids: string[]): void {
  useServerStore.setState({
    servers: ids.map((id) => ({ id, name: id })) as never,
    fetchServers,
  });
}

function seedChannels(activeChannelId: string | null): void {
  useChannelStore.setState({
    activeChannelId,
    channelIdsByServer: { [SERVER_A]: [CH_A1, CH_A2, CH_A3], [SERVER_B]: [CH_B1] },
  });
}

let refresh: PermissionRefresh;
// Jitter returns whatever the case needs; the default keeps timers short.
const jitterMs = vi.fn((_maxMs: number) => 1000);

function newRefresh(): PermissionRefresh {
  refresh = createPermissionRefresh({ jitterMs });
  return refresh;
}

beforeEach(() => {
  vi.useFakeTimers();
  resetAllStores();
  fetchServerPermissions.mockReset().mockResolvedValue(undefined);
  fetchChannelPermissions.mockReset().mockResolvedValue(undefined);
  evictChannelPermissions.mockReset();
  evictServerPermissions.mockReset();
  fetchServers.mockReset().mockResolvedValue(undefined);
  jitterMs.mockClear();
  useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
  usePermissionStore.setState({
    fetchServerPermissions,
    fetchChannelPermissions,
    evictChannelPermissions,
    evictServerPermissions,
  });
  seedServers([SERVER_A, SERVER_B]);
  seedChannels(null);
  newRefresh();
});

afterEach(() => {
  refresh.dispose();
  vi.useRealTimers();
});

describe('serverPermissionsChanged', () => {
  it('ignores a server the account is not a member of', () => {
    // Mutation: delete the isKnownServer check in serverPermissionsChanged.
    refresh.serverPermissionsChanged(UNKNOWN_SERVER);

    expect(vi.getTimerCount()).toBe(0);
    expect(jitterMs).not.toHaveBeenCalled();
  });

  it('arms exactly one timer for a known server and reads nothing until it fires', () => {
    // Control for the unknown-server case: the same call on a known id arms a timer.
    // Mutation: swap the isKnownServer polarity.
    refresh.serverPermissionsChanged(SERVER_A);

    expect(vi.getTimerCount()).toBe(1);
    expect(evictChannelPermissions).not.toHaveBeenCalled();
    expect(fetchServerPermissions).not.toHaveBeenCalled();
  });

  it('draws its delay from the 0-5 s window through the injected source', () => {
    // Mutation: change SERVER_REFRESH_JITTER_MS or pass a different bound to jitterMs.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });

    refresh.serverPermissionsChanged(SERVER_A);

    expect(SERVER_REFRESH_JITTER_MS).toBe(5000);
    expect(jitterMs).toHaveBeenCalledExactlyOnceWith(5000);
  });

  it('fires at the drawn delay and not a millisecond before (upper edge 4999)', () => {
    // Mutation: add a constant to the setTimeout delay, or ignore the jitter value.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    jitterMs.mockReturnValueOnce(4999);

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(4998);
    expect(fetchServerPermissions).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('fires immediately when the draw is the lower edge 0', () => {
    // Mutation: clamp the delay to a minimum above zero.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    jitterMs.mockReturnValueOnce(0);

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(0);

    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('uses the real jitter when none is injected, always inside [0, 5000)', () => {
    // Mutation: pass a larger bound to fullJitter, or add a base offset to its result.
    refresh.dispose();
    const random = vi.spyOn(Math, 'random');
    for (const [draw, expected] of [
      [0, 0],
      [0.999999, 4999],
    ] as const) {
      random.mockReturnValueOnce(draw);
      const real = createPermissionRefresh();
      const timeout = vi.spyOn(globalThis, 'setTimeout');
      real.serverPermissionsChanged(SERVER_A);
      expect(timeout).toHaveBeenLastCalledWith(expect.any(Function), expected);
      timeout.mockRestore();
      real.dispose();
    }
    random.mockRestore();
  });

  it('drops a second event for the same server while the first is pending', () => {
    // Mutation: clear and re-arm the pending timer instead of returning early.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    jitterMs.mockReturnValueOnce(1000);

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(500);
    refresh.serverPermissionsChanged(SERVER_A);

    expect(jitterMs).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(1);
    // The FIRST timer still governs: it fires at 1000 ms, not 1000 ms after the second event.
    vi.advanceTimersByTime(500);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
    vi.advanceTimersByTime(10_000);
    expect(fetchServerPermissions).toHaveBeenCalledTimes(1);
  });

  it('coalesces per server: a second server is not blocked by the first', () => {
    // Mutation: replace the per-server Map with one shared pending flag.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n, [SERVER_B]: 1n } });

    refresh.serverPermissionsChanged(SERVER_A);
    refresh.serverPermissionsChanged(SERVER_B);
    expect(vi.getTimerCount()).toBe(2);

    vi.advanceTimersByTime(1000);
    expect(fetchServerPermissions).toHaveBeenCalledTimes(2);
    expect(fetchServerPermissions).toHaveBeenCalledWith(SERVER_A);
    expect(fetchServerPermissions).toHaveBeenCalledWith(SERVER_B);
  });

  it('accepts a new event for the same server once the pending one has fired', () => {
    // Mutation: forget to serverTimers.delete(serverId) when the timer fires.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);
    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);

    expect(fetchServerPermissions).toHaveBeenCalledTimes(2);
  });

  it('refetches the active channel when it belongs to the server, and evicts the rest', () => {
    // Mutation: evict the active channel too, or skip the channel refetch.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    seedChannels(CH_A2);

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);

    expect(evictChannelPermissions).toHaveBeenCalledExactlyOnceWith([CH_A1, CH_A3]);
    expect(fetchChannelPermissions).toHaveBeenCalledExactlyOnceWith(CH_A2);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it("evicts every channel and fetches none when the active channel is another server's", () => {
    // Mutation: drop the channelIdsByServer membership test in activeChannelOf.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    seedChannels(CH_B1);

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);

    expect(evictChannelPermissions).toHaveBeenCalledExactlyOnceWith([CH_A1, CH_A2, CH_A3]);
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('evicts every channel and fetches none when no channel is active', () => {
    // Mutation: treat a null activeChannelId as a match.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);

    expect(evictChannelPermissions).toHaveBeenCalledExactlyOnceWith([CH_A1, CH_A2, CH_A3]);
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('does not read server permissions that were never read (control: it does once cached)', () => {
    // Mutation: drop the serverPermissions[serverId] !== undefined condition.
    seedChannels(CH_A1);

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    // The channel side still ran, so the schedule itself fired.
    expect(fetchChannelPermissions).toHaveBeenCalledExactlyOnceWith(CH_A1);

    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('re-checks membership when the timer fires and reads nothing for a server that left', () => {
    // Mutation: delete the isKnownServer check at the top of refreshServer.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    seedChannels(CH_A1);

    refresh.serverPermissionsChanged(SERVER_A);
    useServerStore.setState({ servers: [{ id: SERVER_B, name: 'b' }] as never });
    vi.advanceTimersByTime(1000);

    expect(evictChannelPermissions).not.toHaveBeenCalled();
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('never retries: no further reads however long it waits', () => {
    // Mutation: re-arm the timer from the fired callback.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(10 * 60_000);

    expect(fetchServerPermissions).toHaveBeenCalledTimes(1);
  });
});

describe('createJitteredRefetch', () => {
  // The same timer the MFA-enforcement switch uses for its own re-GET, so its
  // contract is pinned here directly and not only through the per-server path.
  it('runs once at the drawn delay and drops a request made while it is pending', () => {
    // Mutation: re-arm on every request, or ignore the drawn delay.
    const run = vi.fn();
    const refetch = createJitteredRefetch(run, jitterMs);

    refetch.request();
    refetch.request();
    expect(jitterMs).toHaveBeenCalledExactlyOnceWith(SERVER_REFRESH_JITTER_MS);
    vi.advanceTimersByTime(999);
    expect(run).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    expect(run).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(60_000);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('can be armed again once it has fired', () => {
    // Mutation: forget to reset the pending state when the timer fires.
    const run = vi.fn();
    const refetch = createJitteredRefetch(run, jitterMs);

    refetch.request();
    vi.advanceTimersByTime(1000);
    refetch.request();
    vi.advanceTimersByTime(1000);

    expect(run).toHaveBeenCalledTimes(2);
  });

  it('cancel() disarms a pending run, and the next request arms a fresh one', () => {
    // Mutation: leave the pending state set in cancel(), so every later request is dropped.
    const run = vi.fn();
    const refetch = createJitteredRefetch(run, jitterMs);

    refetch.request();
    refetch.cancel();
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(60_000);
    expect(run).not.toHaveBeenCalled();

    refetch.request();
    vi.advanceTimersByTime(1000);
    expect(run).toHaveBeenCalledTimes(1);
  });
});

describe('ownPermissionsChanged', () => {
  function seedActive(serverId: string | null, channelId: string | null): void {
    useServerStore.setState({ activeServerId: serverId });
    seedChannels(channelId);
  }

  /** Cached answers for both servers and three channels, as a browsing session leaves them. */
  function seedCaches(): void {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_A]: 1n, [SERVER_B]: 2n },
      channelPermissions: { [CH_A1]: 1n, [CH_A2]: 2n, [CH_B1]: 3n },
    });
  }

  it('waits the 300 ms debounce, then re-reads memberships once', () => {
    // Mutation: change OWN_REFRESH_DEBOUNCE_MS or call fetchServers synchronously.
    refresh.ownPermissionsChanged();
    vi.advanceTimersByTime(OWN_REFRESH_DEBOUNCE_MS - 1);
    expect(fetchServers).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    expect(OWN_REFRESH_DEBOUNCE_MS).toBe(300);
    expect(fetchServers).toHaveBeenCalledTimes(1);
  });

  it('is a trailing debounce: a burst restarts the window and yields one fetchServers', () => {
    // Mutation: return early when a timer is pending instead of clearing it.
    refresh.ownPermissionsChanged();
    vi.advanceTimersByTime(200);
    refresh.ownPermissionsChanged();
    vi.advanceTimersByTime(200);
    refresh.ownPermissionsChanged();
    vi.advanceTimersByTime(299);
    expect(fetchServers).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    expect(fetchServers).toHaveBeenCalledTimes(1);
  });

  it('reads at most three things: memberships, the active server and the active channel', async () => {
    // Mutation: fan the permissions read out over every server in the list.
    seedActive(SERVER_A, CH_A2);
    seedCaches();

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(fetchServers).toHaveBeenCalledTimes(1);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
    expect(fetchChannelPermissions).toHaveBeenCalledExactlyOnceWith(CH_A2);
  });

  it('evicts every cached server and channel except the active ones', async () => {
    // Without this the "Set up verification" item stays on a server the account is not
    // looking at after it enrols, and unmasked bits survive a removed factor.
    // Mutation: evict nothing, evict the active ones too, or evict only channels.
    seedActive(SERVER_A, CH_A2);
    seedCaches();

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(evictServerPermissions).toHaveBeenCalledExactlyOnceWith([SERVER_B]);
    expect(evictChannelPermissions).toHaveBeenCalledExactlyOnceWith([CH_A1, CH_B1]);
  });

  it('evicts every cached answer, and reads none, when no server is active', async () => {
    // Mutation: return before the evictions when activeServerId is null.
    seedActive(null, null);
    seedCaches();

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(evictServerPermissions).toHaveBeenCalledExactlyOnceWith([SERVER_A, SERVER_B]);
    expect(evictChannelPermissions).toHaveBeenCalledExactlyOnceWith([CH_A1, CH_A2, CH_B1]);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it("evicts an active channel that is not the active server's rather than keeping it", async () => {
    // Mutation: exclude the raw activeChannelId from the eviction instead of activeChannelOf's.
    seedActive(SERVER_A, CH_B1);
    seedCaches();

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(evictChannelPermissions).toHaveBeenCalledExactlyOnceWith([CH_A1, CH_A2, CH_B1]);
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('waits out a server-list fetch already in flight, then fetches again', async () => {
    // fetchServers() dedups into a fetch in flight and returns at once, so a read that began
    // BEFORE the change would be the last word on servers[].permissions.
    // Mutation: drop the `await serverFetchSettled()` line in refreshOwn.
    seedActive(SERVER_A, null);
    useServerStore.setState({ isLoading: true });

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);
    expect(fetchServers).not.toHaveBeenCalled();
    expect(fetchServerPermissions).not.toHaveBeenCalled();

    useServerStore.setState({ isLoading: false });
    await vi.advanceTimersByTimeAsync(0);

    expect(fetchServers).toHaveBeenCalledTimes(1);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('reads nothing when the account changes while it waits on a fetch in flight', async () => {
    // Mutation: delete the isSameAuthLifecycle check in refreshOwn.
    seedActive(SERVER_A, CH_A1);
    seedCaches();
    useServerStore.setState({ isLoading: true });

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
    useServerStore.setState({ isLoading: false });
    await vi.advanceTimersByTimeAsync(0);

    expect(evictServerPermissions).not.toHaveBeenCalled();
    expect(evictChannelPermissions).not.toHaveBeenCalled();
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('reads memberships only when no server is active', async () => {
    // Mutation: drop the activeServerId === null return.
    seedActive(null, null);

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(fetchServers).toHaveBeenCalledTimes(1);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('skips the channel read when the active channel is not in the active server', async () => {
    // Mutation: pass activeChannelId through without activeChannelOf.
    seedActive(SERVER_A, CH_B1);

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
  });

  it('reads the active server as it is AFTER fetchServers settles', async () => {
    // Mutation: capture activeServerId before awaiting fetchServers.
    seedActive(SERVER_A, null);
    fetchServers.mockImplementation(async () => {
      useServerStore.setState({ activeServerId: SERVER_B });
    });

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);

    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_B);
  });

  it('drops the permission reads when the account changes while fetchServers is in flight', async () => {
    // Mutation: delete the isSameAuthLifecycle check after the await in refreshOwn.
    seedActive(SERVER_A, CH_A1);
    const inFlight = deferred();
    fetchServers.mockReturnValueOnce(inFlight.promise);

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);
    expect(fetchServers).toHaveBeenCalledTimes(1);

    // Account A signs out and B signs in; the old request then resolves.
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
    inFlight.resolve();
    await vi.advanceTimersByTimeAsync(0);

    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchChannelPermissions).not.toHaveBeenCalled();
    expect(evictServerPermissions).not.toHaveBeenCalled();
    expect(evictChannelPermissions).not.toHaveBeenCalled();
  });

  it('keeps reading across a routine token rotation (same account)', async () => {
    // Control for the lifecycle re-check: rotation preserves authGeneration.
    // Mutation: compare accessToken instead of authGeneration.
    seedActive(SERVER_A, null);
    const inFlight = deferred();
    fetchServers.mockReturnValueOnce(inFlight.promise);

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);
    const { authGeneration } = useAuthStore.getState();
    expect(useAuthStore.getState().rotateAuthCredentials(authGeneration, 'token-a2', 's2')).toBe(
      true
    );
    inFlight.resolve();
    await vi.advanceTimersByTimeAsync(0);

    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('never retries: one fetchServers however long it waits', async () => {
    // Mutation: re-arm ownTimer from refreshOwn.
    seedActive(SERVER_A, null);

    refresh.ownPermissionsChanged();
    await vi.advanceTimersByTimeAsync(OWN_REFRESH_DEBOUNCE_MS);
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(10 * 60_000);

    expect(fetchServers).toHaveBeenCalledTimes(1);
    expect(fetchServerPermissions).toHaveBeenCalledTimes(1);
  });
});

describe('teardown', () => {
  it('dispose clears every pending timer, server and account alike', () => {
    // Mutation: drop cancelAll() from dispose.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    refresh.serverPermissionsChanged(SERVER_A);
    refresh.serverPermissionsChanged(SERVER_B);
    refresh.ownPermissionsChanged();
    expect(vi.getTimerCount()).toBe(3);

    refresh.dispose();

    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(60_000);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchServers).not.toHaveBeenCalled();
  });

  it('dispose stops watching the account', () => {
    // Mutation: drop unsubscribeAuth() from dispose.
    const unsubscribe = vi.fn();
    const subscribe = vi.spyOn(useAuthStore, 'subscribe').mockReturnValueOnce(unsubscribe as never);
    const watched = createPermissionRefresh({ jitterMs });
    subscribe.mockRestore();

    watched.dispose();

    expect(unsubscribe).toHaveBeenCalledTimes(1);
  });

  it('an account change clears pending timers', () => {
    // Mutation: delete the subscribe callback, or compare accessToken for equality only.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    refresh.serverPermissionsChanged(SERVER_A);
    refresh.ownPermissionsChanged();

    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');

    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(60_000);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchServers).not.toHaveBeenCalled();
  });

  it('after an account change the same server is scheduled again, not dropped as pending', () => {
    // The reason cancelAll exists: a stale timer would swallow the next account's event.
    // Mutation: clear the timers but not the serverTimers Map in cancelAll.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    refresh.serverPermissionsChanged(SERVER_A);
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');

    refresh.serverPermissionsChanged(SERVER_A);
    vi.advanceTimersByTime(1000);

    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('a token rotation within the same account leaves pending timers alone', () => {
    // Mutation: subscribe on accessToken instead of authGeneration.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n } });
    refresh.serverPermissionsChanged(SERVER_A);
    const { authGeneration } = useAuthStore.getState();

    useAuthStore.getState().rotateAuthCredentials(authGeneration, 'token-a2', 'session-a2');

    expect(vi.getTimerCount()).toBe(1);
    vi.advanceTimersByTime(1000);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });
});
