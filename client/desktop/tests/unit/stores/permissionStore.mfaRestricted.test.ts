// #3456: three properties of the permission store's effective-permission reads.
//
// 1. `mfa_restricted` is parsed from GET /servers/:id/permissions: stored only
//    when strictly `true`, cleared by an absent or false answer, per server,
//    cleared by reset(), and never persisted.
// 2. Both reads are fenced by read order (F10): an older response that lands
//    after a newer one writes nothing. The permission-change events make
//    overlapping reads routine.
// 3. evictChannelPermissions and evictServerPermissions drop the cached answer
//    (and a server's mfa_restricted flag) AND discard any read of it already in
//    flight.
// 4. reset() drops the `permissions:*` read tickets, and only those.
//
// Oracle: each fence case asserts the freshest answer is in the store BEFORE the
// stale response is released, then that releasing it changes nothing; the
// control cases show the same read does write when nothing superseded it.
import { describe, it, expect, beforeAll, afterAll, afterEach, beforeEach } from 'vitest';
import { http, HttpResponse } from 'msw';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';
import { server } from '../../mocks/server';

const API_BASE = 'http://localhost:8080';
const SERVER_URL = `${API_BASE}/api/v1/servers/server-1/permissions`;
const CHANNEL_URL = `${API_BASE}/api/v1/channels/channel-1/permissions`;

/**
 * Answers the FIRST request to `url` only after `release()`, with `stale`, and
 * every later request at once with `fresh`. `received` settles once the held
 * request has reached the server.
 */
function holdFirst(url: string, stale: () => Response, fresh: () => Response) {
  const held = deferred();
  const arrived = deferred();
  let calls = 0;
  server.use(
    http.get(url, async () => {
      calls += 1;
      if (calls > 1) return fresh();
      arrived.resolve();
      await held.promise;
      return stale();
    })
  );
  return { release: held.resolve, received: arrived.promise };
}

const serverBody = (permissions: string, restricted?: boolean) =>
  HttpResponse.json(
    restricted === undefined ? { permissions } : { permissions, mfa_restricted: restricted }
  );

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
  usePermissionStore.getState().reset();
});

describe('fetchServerPermissions: mfa_restricted', () => {
  it('stores the flag when the response says true', async () => {
    // Mutation: change `=== true` to `=== false` in fetchServerPermissions.
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));

    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });
    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(1024n);
  });

  it('leaves the flag unset when the field is absent', async () => {
    // Mutation: write `data.mfa_restricted` itself instead of the strict boolean.
    server.use(http.get(SERVER_URL, () => serverBody('1024')));

    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(1024n);
  });

  it('does not store a false flag: the record holds only true', async () => {
    // Mutation: return `{ ...rest, [serverId]: restricted }` in withMfaRestricted.
    server.use(http.get(SERVER_URL, () => serverBody('1024', false)));

    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
    expect('server-1' in usePermissionStore.getState().mfaRestrictedByServer).toBe(false);
  });

  it.each([['"true"'], ['1'], ['null']])(
    'treats a non-boolean-true value (%s) as unrestricted',
    async (raw) => {
      // Mutation: use a truthiness test (`!!data.mfa_restricted`).
      server.use(
        http.get(SERVER_URL, () =>
          HttpResponse.json({ permissions: '1024', mfa_restricted: JSON.parse(raw) })
        )
      );

      await usePermissionStore.getState().fetchServerPermissions('server-1');

      expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
    }
  );

  it('clears the flag when a later read says the server stopped restricting', async () => {
    // Mutation: only ever add to the record, never remove (drop `...rest` handling).
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));
    await usePermissionStore.getState().fetchServerPermissions('server-1');
    expect(usePermissionStore.getState().mfaRestrictedByServer['server-1']).toBe(true);

    server.use(http.get(SERVER_URL, () => serverBody('2048')));
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(2048n);
  });

  it('keeps servers independent', async () => {
    // Mutation: replace the per-server record with a single flag.
    server.use(
      http.get(SERVER_URL, () => serverBody('1024', true)),
      http.get(`${API_BASE}/api/v1/servers/server-2/permissions`, () => serverBody('1024'))
    );

    await usePermissionStore.getState().fetchServerPermissions('server-1');
    await usePermissionStore.getState().fetchServerPermissions('server-2');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });
  });

  it('leaves the flag untouched when the read fails (non-ok response)', async () => {
    // Mutation: clear the flag before checking res.ok.
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    server.use(http.get(SERVER_URL, () => new HttpResponse(null, { status: 500 })));
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });
  });

  it('leaves the flag untouched on a network error', async () => {
    // Mutation: reset the flag in the catch block.
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    server.use(http.get(SERVER_URL, () => HttpResponse.error()));
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });
  });

  it('reset() clears every flag (the account-switch path)', async () => {
    // Mutation: drop mfaRestrictedByServer from reset().
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));
    await usePermissionStore.getState().fetchServerPermissions('server-1');
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });

    usePermissionStore.getState().reset();

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
  });

  it('is never persisted', async () => {
    // Mutation: wrap the store in persist() without partializing the flag out.
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    const stored = [localStorage, sessionStorage].flatMap((storage) =>
      Object.keys(storage).map((key) => `${key}=${storage.getItem(key) ?? ''}`)
    );
    expect(stored.filter((entry) => /mfa.?restricted|server-1/i.test(entry))).toEqual([]);
  });

  it('writes nothing once another account has signed in', async () => {
    // Mutation: delete the isSameAuthLifecycle check in fetchServerPermissions.
    const g = holdFirst(
      SERVER_URL,
      () => serverBody('1024', true),
      () => serverBody('1')
    );
    const pending = usePermissionStore.getState().fetchServerPermissions('server-1');
    await g.received;

    usePermissionStore.getState().reset();
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
    g.release();
    await pending;

    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
    expect(usePermissionStore.getState().serverPermissions['server-1']).toBeUndefined();
  });
});

describe('effective-permission reads are sequenced per key (F10)', () => {
  it('a server read that began earlier and lands later does not overwrite the newer answer', async () => {
    // Mutation: delete the settleRead check in fetchServerPermissions.
    const g = holdFirst(
      SERVER_URL,
      () => serverBody('1024', true),
      () => serverBody('2048')
    );
    const older = usePermissionStore.getState().fetchServerPermissions('server-1');
    await g.received;

    await usePermissionStore.getState().fetchServerPermissions('server-1');
    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(2048n);
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});

    g.release();
    await older;

    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(2048n);
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
  });

  it('control: a lone server read commits both permissions and the flag', async () => {
    // Mutation: make settleRead return a non-commit for a first read.
    server.use(http.get(SERVER_URL, () => serverBody('1024', true)));

    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(1024n);
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });
  });

  it('a channel read that began earlier and lands later does not overwrite the newer answer', async () => {
    // Mutation: delete the settleRead check in fetchChannelPermissions.
    const g = holdFirst(
      CHANNEL_URL,
      () => HttpResponse.json({ permissions: '1024' }),
      () => HttpResponse.json({ permissions: '2048' })
    );
    const older = usePermissionStore.getState().fetchChannelPermissions('channel-1');
    await g.received;

    await usePermissionStore.getState().fetchChannelPermissions('channel-1');
    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBe(2048n);

    g.release();
    await older;

    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBe(2048n);
  });

  it("a confirmed override write on a channel does not discard that channel's permission read", async () => {
    // channelOverrides sequences on the BARE channel id, so the permission read's key must differ.
    // Mutation: return the bare channelId from channelPermissionsScope.
    const g = holdFirst(
      CHANNEL_URL,
      () => HttpResponse.json({ permissions: '1024' }),
      () => HttpResponse.json({ permissions: '2048' })
    );
    const inFlight = usePermissionStore.getState().fetchChannelPermissions('channel-1');
    await g.received;
    server.use(
      http.delete(
        `${API_BASE}/api/v1/channels/channel-1/overrides/override-1`,
        () => new HttpResponse(null, { status: 204 })
      )
    );

    expect(
      await usePermissionStore.getState().deleteChannelOverride('channel-1', 'override-1')
    ).toBe(true);
    g.release();
    await inFlight;

    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBe(1024n);
  });

  it('a server id and a channel id that are equal do not share a ticket counter', async () => {
    // Mutation: return the bare id from BOTH serverPermissionsScope and channelPermissionsScope.
    const g = holdFirst(
      `${API_BASE}/api/v1/servers/shared-1/permissions`,
      () => serverBody('1024'),
      () => serverBody('2048')
    );
    const heldServer = usePermissionStore.getState().fetchServerPermissions('shared-1');
    await g.received;
    server.use(
      http.get(`${API_BASE}/api/v1/channels/shared-1/permissions`, () =>
        HttpResponse.json({ permissions: '4096' })
      )
    );

    await usePermissionStore.getState().fetchChannelPermissions('shared-1');
    g.release();
    await heldServer;

    expect(usePermissionStore.getState().serverPermissions['shared-1']).toBe(1024n);
    expect(usePermissionStore.getState().channelPermissions['shared-1']).toBe(4096n);
  });

  it('writes nothing for a channel read once another account has signed in', async () => {
    // Mutation: delete the isSameAuthLifecycle check in fetchChannelPermissions.
    const g = holdFirst(
      CHANNEL_URL,
      () => HttpResponse.json({ permissions: '1024' }),
      () => HttpResponse.json({ permissions: '1' })
    );
    const pending = usePermissionStore.getState().fetchChannelPermissions('channel-1');
    await g.received;

    usePermissionStore.getState().reset();
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
    g.release();
    await pending;

    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBeUndefined();
  });
});

describe('evictChannelPermissions', () => {
  it('drops cached answers for the named channels only', () => {
    // Mutation: clear the whole channelPermissions map.
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': 1n, 'channel-2': 2n, 'channel-3': 3n },
    });

    usePermissionStore.getState().evictChannelPermissions(['channel-1', 'channel-3']);

    expect(usePermissionStore.getState().channelPermissions).toEqual({ 'channel-2': 2n });
  });

  it('is a no-op for channels that were never cached (state object unchanged)', () => {
    // Mutation: always return a new channelPermissions object.
    usePermissionStore.setState({ channelPermissions: { 'channel-2': 2n } });
    const before = usePermissionStore.getState().channelPermissions;

    usePermissionStore.getState().evictChannelPermissions(['channel-1']);
    usePermissionStore.getState().evictChannelPermissions([]);

    expect(usePermissionStore.getState().channelPermissions).toBe(before);
  });

  it('discards a read already in flight, so a pre-change answer cannot return', async () => {
    // Mutation: drop the markWriteConfirmed call in evictChannelPermissions.
    const g = holdFirst(
      CHANNEL_URL,
      () => HttpResponse.json({ permissions: '1024' }),
      () => HttpResponse.json({ permissions: '2048' })
    );
    const inFlight = usePermissionStore.getState().fetchChannelPermissions('channel-1');
    await g.received;

    usePermissionStore.getState().evictChannelPermissions(['channel-1']);
    g.release();
    await inFlight;

    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBeUndefined();
  });

  it('a read begun after the eviction still lands', async () => {
    // Control for the in-flight case. Mutation: raise the floor past future tickets.
    usePermissionStore.setState({ channelPermissions: { 'channel-1': 1n } });
    server.use(http.get(CHANNEL_URL, () => HttpResponse.json({ permissions: '2048' })));

    usePermissionStore.getState().evictChannelPermissions(['channel-1']);
    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBeUndefined();
    await usePermissionStore.getState().fetchChannelPermissions('channel-1');

    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBe(2048n);
  });

  it("evicting one channel does not discard another channel's in-flight read", async () => {
    // Mutation: mark every channel scope confirmed regardless of the ids passed.
    const g = holdFirst(
      CHANNEL_URL,
      () => HttpResponse.json({ permissions: '1024' }),
      () => HttpResponse.json({ permissions: '2048' })
    );
    const inFlight = usePermissionStore.getState().fetchChannelPermissions('channel-1');
    await g.received;

    usePermissionStore.getState().evictChannelPermissions(['channel-2']);
    g.release();
    await inFlight;

    expect(usePermissionStore.getState().channelPermissions['channel-1']).toBe(1024n);
  });
});

describe('evictServerPermissions', () => {
  it('drops cached answers and mfa_restricted flags for the named servers only', () => {
    // Mutation: leave mfaRestrictedByServer alone, or clear the whole map.
    usePermissionStore.setState({
      serverPermissions: { 'server-1': 1n, 'server-2': 2n, 'server-3': 3n },
      mfaRestrictedByServer: { 'server-1': true, 'server-3': true },
    });

    usePermissionStore.getState().evictServerPermissions(['server-1', 'server-3']);

    expect(usePermissionStore.getState().serverPermissions).toEqual({ 'server-2': 2n });
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
  });

  it('is a no-op for servers that were never cached (state objects unchanged)', () => {
    // Mutation: always return new maps.
    usePermissionStore.setState({
      serverPermissions: { 'server-2': 2n },
      mfaRestrictedByServer: { 'server-2': true },
    });
    const before = usePermissionStore.getState();

    usePermissionStore.getState().evictServerPermissions(['server-1']);
    usePermissionStore.getState().evictServerPermissions([]);

    expect(usePermissionStore.getState().serverPermissions).toBe(before.serverPermissions);
    expect(usePermissionStore.getState().mfaRestrictedByServer).toBe(before.mfaRestrictedByServer);
  });

  it('discards a read already in flight, so a pre-change answer cannot return', async () => {
    // Mutation: drop the markWriteConfirmed call in evictServerPermissions.
    const g = holdFirst(
      SERVER_URL,
      () => serverBody('1024', true),
      () => serverBody('2048')
    );
    const inFlight = usePermissionStore.getState().fetchServerPermissions('server-1');
    await g.received;

    usePermissionStore.getState().evictServerPermissions(['server-1']);
    g.release();
    await inFlight;

    expect(usePermissionStore.getState().serverPermissions['server-1']).toBeUndefined();
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
  });

  it('a read begun after the eviction still lands', async () => {
    // Control for the in-flight case. Mutation: raise the floor past future tickets.
    usePermissionStore.setState({
      serverPermissions: { 'server-1': 1n },
      mfaRestrictedByServer: { 'server-1': true },
    });
    server.use(http.get(SERVER_URL, () => serverBody('2048')));

    usePermissionStore.getState().evictServerPermissions(['server-1']);
    expect(usePermissionStore.getState().serverPermissions['server-1']).toBeUndefined();
    await usePermissionStore.getState().fetchServerPermissions('server-1');

    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(2048n);
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({});
  });

  it("evicting one server does not discard another server's in-flight read", async () => {
    // Mutation: mark every server scope confirmed regardless of the ids passed.
    const g = holdFirst(
      SERVER_URL,
      () => serverBody('1024', true),
      () => serverBody('2048')
    );
    const inFlight = usePermissionStore.getState().fetchServerPermissions('server-1');
    await g.received;

    usePermissionStore.getState().evictServerPermissions(['server-2']);
    g.release();
    await inFlight;

    expect(usePermissionStore.getState().serverPermissions['server-1']).toBe(1024n);
    expect(usePermissionStore.getState().mfaRestrictedByServer).toEqual({ 'server-1': true });
  });
});

describe('reset() and the read tickets', () => {
  // The ticket map is module-private, so these observe it through its one visible
  // effect: an eviction raises a scope's floor above a held read's ticket, and
  // that read lands as `stale` for as long as the floor survives. Landing in the
  // SAME auth lifecycle is not a production path (reset() runs with an account
  // change, which fences the read first); it is only how a test sees the floor.
  it.each([
    {
      name: 'server',
      url: SERVER_URL,
      read: () => usePermissionStore.getState().fetchServerPermissions('server-1'),
      evict: () => usePermissionStore.getState().evictServerPermissions(['server-1']),
      landed: () => usePermissionStore.getState().serverPermissions['server-1'],
      stale: () => serverBody('1024'),
      fresh: () => serverBody('2048'),
    },
    {
      name: 'channel',
      url: CHANNEL_URL,
      read: () => usePermissionStore.getState().fetchChannelPermissions('channel-1'),
      evict: () => usePermissionStore.getState().evictChannelPermissions(['channel-1']),
      landed: () => usePermissionStore.getState().channelPermissions['channel-1'],
      stale: () => HttpResponse.json({ permissions: '1024' }),
      fresh: () => HttpResponse.json({ permissions: '2048' }),
    },
  ])('forgets the $name floor, so the map does not outlive the account', async (scope) => {
    // Mutation: drop clearPermissionReadSequences() from reset(), or its prefix test.
    const g = holdFirst(scope.url, scope.stale, scope.fresh);
    const held = scope.read();
    await g.received;
    scope.evict();

    usePermissionStore.getState().reset();
    g.release();
    await held;

    expect(scope.landed()).toBe(1024n);
  });

  it('control: without a reset the same eviction discards the held read', async () => {
    // Shows the case above can only pass because reset() dropped the floor.
    // Mutation: make evictServerPermissions stop raising the floor.
    const g = holdFirst(
      SERVER_URL,
      () => serverBody('1024'),
      () => serverBody('2048')
    );
    const held = usePermissionStore.getState().fetchServerPermissions('server-1');
    await g.received;
    usePermissionStore.getState().evictServerPermissions(['server-1']);

    g.release();
    await held;

    expect(usePermissionStore.getState().serverPermissions['server-1']).toBeUndefined();
  });

  it('keeps the override tickets: a confirmed write still makes an older read re-read', async () => {
    // channelOverrides sequences on the BARE channel id, which reset() must not touch.
    // Mutation: clear every read sequence in reset(), not only `permissions:*`.
    const overridesUrl = `${API_BASE}/api/v1/channels/channel-1/overrides`;
    const g = holdFirst(
      overridesUrl,
      () => HttpResponse.json({ overrides: [{ id: 'stale' }] }),
      () => HttpResponse.json({ overrides: [{ id: 'fresh' }] })
    );
    const held = usePermissionStore.getState().fetchChannelOverrides('channel-1');
    await g.received;
    server.use(
      http.delete(`${overridesUrl}/override-1`, () => new HttpResponse(null, { status: 204 }))
    );
    expect(
      await usePermissionStore.getState().deleteChannelOverride('channel-1', 'override-1')
    ).toBe(true);

    usePermissionStore.getState().reset();
    g.release();
    await held;

    expect(usePermissionStore.getState().channelOverrides['channel-1']).toEqual([{ id: 'fresh' }]);
  });
});
