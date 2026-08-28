import { useInviteStore } from '@/renderer/stores/inviteStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import { useAuthStore } from '@/renderer/stores/authStore';
import type { Server } from '@/renderer/types/server';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('inviteStore', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('starts with empty state', () => {
    const state = useInviteStore.getState();
    expect(state.invites).toEqual({});
    expect(state.isLoading).toBe(false);
    expect(state.error).toBeNull();
  });

  describe('clearInvites', () => {
    it('clears all invite data', () => {
      useInviteStore.setState({
        invites: {
          'server-1': [
            {
              id: 'invite-1',
              server_id: 'server-1',
              code: 'ABC12345',
              created_by: 'user-1',
              creator_username: 'testuser',
              max_uses: null,
              use_count: 0,
              expires_at: null,
              is_revoked: false,
              created_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });
      useInviteStore.getState().clearInvites();
      expect(useInviteStore.getState().invites).toEqual({});
    });
  });

  describe('fetchInvites', () => {
    it('fetches invites for a server', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/invites`, () => {
          return HttpResponse.json({
            invites: [
              {
                id: 'inv-1',
                server_id: 'server-1',
                code: 'ABC123',
                created_by: 'user-1',
                creator_username: 'admin',
                max_uses: 10,
                use_count: 2,
                expires_at: null,
                is_revoked: false,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useInviteStore.getState().fetchInvites('server-1');
      const state = useInviteStore.getState();
      expect(state.isLoading).toBe(false);
      expect(state.invites['server-1']).toHaveLength(1);
      expect(state.invites['server-1'][0].code).toBe('ABC123');
    });

    it('sets error on fetch failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/invites`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );
      await useInviteStore.getState().fetchInvites('server-1');
      expect(useInviteStore.getState().error).toBe('Forbidden');
    });
  });

  describe('createInvite', () => {
    it('creates and caches an invite', async () => {
      const result = await useInviteStore.getState().createInvite('server-1', { max_uses: 1 });
      expect(result).not.toBeNull();
      expect(result!.code).toBe('TESTCODE');
      expect(useInviteStore.getState().invites['server-1']).toHaveLength(1);
    });

    it('returns null on failure', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/server-1/invites`, () => {
          return HttpResponse.json({ error: 'Limit reached' }, { status: 400 });
        })
      );
      const result = await useInviteStore.getState().createInvite('server-1');
      expect(result).toBeNull();
      expect(useInviteStore.getState().error).toBe('Limit reached');
    });
  });

  describe('revokeInvite', () => {
    it('marks invite as revoked in cache', async () => {
      useInviteStore.setState({
        invites: {
          'server-1': [
            {
              id: 'inv-1',
              server_id: 'server-1',
              code: 'CODE1',
              created_by: 'user-1',
              creator_username: 'admin',
              max_uses: null,
              use_count: 0,
              expires_at: null,
              is_revoked: false,
              created_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/server-1/invites/inv-1`, () => {
          return HttpResponse.json({ message: 'Revoked' });
        })
      );
      const result = await useInviteStore.getState().revokeInvite('server-1', 'inv-1');
      expect(result).toBe(true);
      expect(useInviteStore.getState().invites['server-1'][0].is_revoked).toBe(true);
    });

    it('returns false on failure', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/server-1/invites/inv-1`, () => {
          return HttpResponse.json({ error: 'Not found' }, { status: 404 });
        })
      );
      expect(await useInviteStore.getState().revokeInvite('server-1', 'inv-1')).toBe(false);
    });
  });

  describe('joinServer', () => {
    it('joins a server via invite code', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () => {
          return HttpResponse.json({ server: { id: 'srv', name: 'S' }, role: 'member' });
        })
      );
      const result = await useInviteStore.getState().joinServer('CODE');
      expect(result).not.toBeNull();
      expect(useInviteStore.getState().isLoading).toBe(false);
    });

    it('returns null on invalid code', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () => {
          return HttpResponse.json({ error: 'Invalid or expired invite' }, { status: 404 });
        })
      );
      expect(await useInviteStore.getState().joinServer('BAD')).toBeNull();
      expect(useInviteStore.getState().error).toBe('Invalid or expired invite');
    });
  });

  // regression for #2363: inviteStore.joinServer must reconcile serverStore
  // membership itself so every caller (not just JoinServerModal) gets a
  // sidebar entry + WS subscription for the server it just joined.
  describe('joinServer membership reconciliation (#2363)', () => {
    function buildJoinedServer(id: string): Server {
      return {
        id,
        name: 'Reconciled Server',
        owner_id: 'user-2',
        allow_embedded_content: true,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      };
    }

    it('T3a: writes the joined server into serverStore.servers on success', async () => {
      useServerStore.setState({ servers: [] });
      const joinedServer = buildJoinedServer('server-2363-a');
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () =>
          HttpResponse.json({ server: joinedServer, role: 'member' })
        )
      );

      const result = await useInviteStore.getState().joinServer('CODE2363A');

      expect(result).not.toBeNull();
      const ids = useServerStore.getState().servers.map((s) => s.id);
      expect(
        ids,
        `serverStore.servers should contain id "${joinedServer.id}" after a successful join, got [${ids.join(', ')}]`
      ).toContain(joinedServer.id);
      // SHAPE, not just presence. JoinServerModal.test.tsx used to assert this
      // and lost it when the write moved here; without it, dropping `role` from
      // joinServer's addServer literal leaves every test in the PR green while
      // the sidebar row loses its role. regression for #2363.
      const written = useServerStore.getState().servers.find((s) => s.id === joinedServer.id);
      expect(written).toMatchObject({
        id: joinedServer.id,
        role: 'member',
        member_count: 0,
        online_count: 0,
      });
    });

    // Seeded from a REHYDRATED row, not from a second successful join. Two 200s
    // for the same server cannot happen — the control plane 409s an existing
    // member before the insert — and modelling the impossible case is what makes
    // a later reader keep the upsert for the wrong reason. `serverStore` uses
    // `persist`, so a stale `concord-servers` entry sharing an id with a fresh
    // join IS reachable, and it is the collision the upsert exists for.
    it('T3b: a join colliding with a stale persisted row upserts, and keeps its real counts', async () => {
      const joinedServer = buildJoinedServer('server-2363-b');
      useServerStore.setState({
        servers: [
          {
            ...joinedServer,
            role: 'member',
            name: 'stale name',
            member_count: 42,
            online_count: 7,
          },
        ],
      });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () =>
          HttpResponse.json({ server: joinedServer, role: 'member' })
        )
      );

      await useInviteStore.getState().joinServer('CODE2363B');

      const matches = useServerStore.getState().servers.filter((s) => s.id === joinedServer.id);
      expect(
        matches,
        `expected exactly one "${joinedServer.id}" entry after a join onto a stale persisted row, found ${matches.length}`
      ).toHaveLength(1);
      // The fresh response wins on ordinary fields...
      expect(matches[0].name).toBe(joinedServer.name);
      // ...but never on the counts, which joinServer fabricates as 0.
      expect(matches[0].member_count).toBe(42);
      expect(matches[0].online_count).toBe(7);
    });

    // CODEX P1 (round 2). gracefulReset() clears the stores; it cannot cancel a
    // POST already in flight. User A joins, logs out, user B signs in, A's join
    // resolves — and an unconditional write puts A's server in B's sidebar AND in
    // B's subscribe_server set, which is derived from the same array.
    it('T3d: a join resolving after an account switch does NOT write into the new session', async () => {
      useServerStore.setState({ servers: [] });
      const joinedServer = buildJoinedServer('server-2363-leak');
      let releaseJoin: () => void = () => {};
      const joinInFlight = new Promise<void>((resolve) => {
        releaseJoin = resolve;
      });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, async () => {
          await joinInFlight;
          return HttpResponse.json({ server: joinedServer, role: 'member' });
        })
      );

      const pending = useInviteStore.getState().joinServer('CODE2363LEAK');
      // A logs out, B signs in. setAccessToken advances authGeneration, which is
      // what captureAuthLifecycle snapshotted.
      useAuthStore.getState().setAccessToken('mock-token-user-b');
      releaseJoin();
      const result = await pending;

      expect(result, 'a superseded join must not report success to the new session').toBeNull();
      expect(
        useServerStore.getState().servers,
        "user A's join must not appear in user B's sidebar"
      ).toEqual([]);
    });

    // The other half of T3d, and the reason the fence compares authGeneration
    // rather than a credential. rotateAuthCredentials replaces BOTH the access
    // token and the session id while deliberately preserving the generation —
    // tokens rotate on a schedule, so a join in flight across a proactive refresh
    // is routine. Comparing either credential would discard it after the server
    // had already committed the membership.
    it('T3e: a join surviving an ordinary token rotation still reconciles', async () => {
      useServerStore.setState({ servers: [] });
      useAuthStore.getState().beginAuthLifecycle('token-v1', 'session-v1');
      const generation = useAuthStore.getState().authGeneration;
      const joinedServer = buildJoinedServer('server-2363-rotate');
      let releaseJoin: () => void = () => {};
      const joinInFlight = new Promise<void>((resolve) => {
        releaseJoin = resolve;
      });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, async () => {
          await joinInFlight;
          return HttpResponse.json({ server: joinedServer, role: 'member' });
        })
      );

      const pending = useInviteStore.getState().joinServer('CODE2363ROT');
      // A proactive refresh: new token, new session id, SAME generation.
      useAuthStore.getState().rotateAuthCredentials(generation, 'token-v2', 'session-v2');
      expect(
        useAuthStore.getState().sessionId,
        'the rotation must actually have replaced the session id, or this test proves nothing'
      ).toBe('session-v2');
      releaseJoin();
      const result = await pending;

      expect(result, 'an ordinary rotation must not fail the join').not.toBeNull();
      expect(useServerStore.getState().servers.map((s) => s.id)).toContain(joinedServer.id);
    });

    // res.ok says the request succeeded, not that the body is what we asked for.
    // An unvalidated cast writes `id: undefined` into the sidebar, and
    // `key={server.id}` then collides for every such row — the duplicate-key
    // defect this PR fixes, arriving by a different door.
    it('T3f: a 200 with no server in the body is a failed join, not an undefined row', async () => {
      useServerStore.setState({ servers: [] });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () =>
          // Well-formed HTTP, malformed payload.
          HttpResponse.json({ role: 'member' })
        )
      );

      const result = await useInviteStore.getState().joinServer('CODE2363SHAPE');

      expect(result, 'a malformed 200 must not read as a successful join').toBeNull();
      expect(
        useServerStore.getState().servers,
        'no row may be written for a server that was never described'
      ).toEqual([]);
      expect(useInviteStore.getState().error).toBeTruthy();
      expect(useInviteStore.getState().isLoading).toBe(false);
    });

    // The id-only guard let this through: ServerList and ServerBar call
    // server.name.charAt(0) unguarded at three sites, so a contract-drifted 200
    // crashes the authenticated chrome rather than merely rendering oddly.
    it('T3g: a server row with no name is rejected before it reaches the sidebar', async () => {
      useServerStore.setState({ servers: [] });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () =>
          HttpResponse.json({ server: { id: 'server-2363-noname' }, role: 'member' })
        )
      );

      const result = await useInviteStore.getState().joinServer('CODE2363NONAME');

      expect(result, 'a row the renderer cannot render is not a successful join').toBeNull();
      expect(useServerStore.getState().servers).toEqual([]);
    });

    it('T3h: an unrecognised role is rejected before it reaches the sidebar', async () => {
      useServerStore.setState({ servers: [] });
      const joinedServer = buildJoinedServer('server-2363-badrole');
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () =>
          HttpResponse.json({ server: joinedServer, role: 'superuser' })
        )
      );

      const result = await useInviteStore.getState().joinServer('CODE2363ROLE');

      expect(result).toBeNull();
      expect(useServerStore.getState().servers).toEqual([]);
    });

    // isLoading and error are SHARED. A's continuation resolving after B started
    // their own join must not clear B's spinner — the comment says A's
    // continuation is dropped, so it must drop ALL of it, not just the store write.
    it("T3i: a superseded join does not clear a successor join's loading state", async () => {
      useServerStore.setState({ servers: [] });
      useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
      const joinedServer = buildJoinedServer('server-2363-supersede');
      let releaseA: () => void = () => {};
      const aInFlight = new Promise<void>((resolve) => {
        releaseA = resolve;
      });
      let releaseB: () => void = () => {};
      const bInFlight = new Promise<void>((resolve) => {
        releaseB = resolve;
      });
      let call = 0;
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, async () => {
          call += 1;
          await (call === 1 ? aInFlight : bInFlight);
          return HttpResponse.json({ server: joinedServer, role: 'member' });
        })
      );

      const aPending = useInviteStore.getState().joinServer('CODE2363AAA');
      // A logs out, B signs in and starts their own join.
      useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
      const bPending = useInviteStore.getState().joinServer('CODE2363BBB');
      expect(useInviteStore.getState().isLoading).toBe(true);

      // A's request resolves into B's session.
      releaseA();
      await aPending;

      expect(
        useInviteStore.getState().isLoading,
        "B's join is still in flight, so its spinner must still be up"
      ).toBe(true);

      releaseB();
      await bPending;
      expect(useInviteStore.getState().isLoading).toBe(false);
    });

    // joinSequence alone does not fence a stale ACCOUNT: with no successor join,
    // A still owns the sequence, so its failure would be written into B's store.
    // Both questions are asked on every write (CodeRabbit, round 9).
    it('T3j: a stale join failure does not write an error into the new session', async () => {
      useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
      let releaseA: () => void = () => {};
      const aInFlight = new Promise<void>((resolve) => {
        releaseA = resolve;
      });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, async () => {
          await aInFlight;
          return HttpResponse.json({ error: 'Invalid or expired invite' }, { status: 404 });
        })
      );

      const pending = useInviteStore.getState().joinServer('CODE2363STALE');
      // B signs in and starts NOTHING, so A still owns joinSequence.
      useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
      releaseA();

      expect(await pending).toBeNull();
      expect(
        useInviteStore.getState().error,
        "A's failure must not surface to B, who never asked for anything"
      ).toBeNull();
    });

    it('T3c: a failed join leaves serverStore.servers untouched (fence)', async () => {
      useServerStore.setState({ servers: [] });
      server.use(
        http.post(`${API_BASE}/api/v1/invites/join`, () => {
          return HttpResponse.json({ error: 'Invalid or expired invite' }, { status: 404 });
        })
      );

      const result = await useInviteStore.getState().joinServer('BADCODE2363');

      expect(result).toBeNull();
      expect(
        useServerStore.getState().servers,
        'serverStore.servers must remain empty after a failed join'
      ).toEqual([]);
    });
  });

  describe('getInviteInfo', () => {
    it('fetches invite info by code', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/invites/MYCODE`, () => {
          return HttpResponse.json({ server_name: 'Cool', member_count: 42, is_valid: true });
        })
      );
      expect(await useInviteStore.getState().getInviteInfo('MYCODE')).not.toBeNull();
    });

    it('returns null for invalid code', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/invites/BAD`, () => {
          return HttpResponse.json({ error: 'Invalid invite code' }, { status: 404 });
        })
      );
      expect(await useInviteStore.getState().getInviteInfo('BAD')).toBeNull();
    });
  });
});
