// Regression for #3406 (Codex review, round 9): a permission-store read that
// started before a confirmed write, and resolves after it, overwrote the write.
// A settings modal reads its overrides when it opens; a save made before that
// read answers refetches the list, and the older response then replaced the
// fresh one, so an added override vanished or a saved mask reverted. Saving the
// stale row again could erase the bits just written. Deletes and category sync
// have the same race, and so do the role writes, which patch the list locally.
//
// Oracle: a read that started before a confirmed write to the same scope
// commits nothing once that write has confirmed; the write's own result stands.
// Every case asserts the write's result BEFORE releasing the stale read, so it
// cannot pass by the write never landing.
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import { buildReorderPayload } from '@/renderer/utils/policy/roleHierarchy';
import type { RoleHierarchy } from '@/renderer/types/server';

const API_BASE = 'http://localhost:8080';

function override(id: string, allow: string) {
  return {
    id,
    channel_id: 'channel-1',
    target_type: 'role' as const,
    target_id: 'role-1',
    allow,
    deny: '0',
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  };
}

function role(id: string, name: string) {
  return {
    id,
    server_id: 'server-1',
    name,
    position: 1,
    permissions: '1024',
    is_default: false,
    is_managed: false,
    display_separately: false,
    mentionable: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  };
}

/**
 * A GET whose FIRST request is held until `release()`, answering `stale`, and
 * whose later requests answer `fresh()` at once. `received` settles once the
 * held request has reached the server.
 */
function holdFirstRead(path: string, stale: () => Response, fresh: () => Response) {
  let release!: () => void;
  let arrive!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const received = new Promise<void>((resolve) => {
    arrive = resolve;
  });
  let calls = 0;
  server.use(
    http.get(`${API_BASE}${path}`, async () => {
      calls += 1;
      if (calls > 1) return fresh();
      arrive();
      await held;
      return stale();
    })
  );
  return { release, received };
}

type State = ReturnType<typeof usePermissionStore.getState>;

interface StaleReadCase {
  name: string;
  /** Seeds the store and registers every handler; returns the held read. */
  arrange: () => ReturnType<typeof holdFirstRead>;
  read: () => Promise<unknown>;
  write: () => Promise<unknown>;
  /** The write's own result, which must survive the stale read. */
  expectWritten: (s: State) => void;
}

const cases: StaleReadCase[] = [
  {
    name: 'a channel override save',
    arrange: () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/channel-1/overrides`, () =>
          HttpResponse.json({ override: override('o-new', '1024') })
        )
      );
      return holdFirstRead(
        '/api/v1/channels/channel-1/overrides',
        () => HttpResponse.json({ overrides: [] }),
        () => HttpResponse.json({ overrides: [override('o-new', '1024')] })
      );
    },
    read: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
    write: () =>
      usePermissionStore.getState().upsertChannelOverride('channel-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '1024',
        deny: '0',
      }),
    expectWritten: (s) =>
      expect(s.channelOverrides['channel-1']?.map((o) => o.id)).toEqual(['o-new']),
  },
  {
    name: 'a category override save',
    arrange: () => {
      server.use(
        http.put(`${API_BASE}/api/v1/categories/cat-1/overrides`, () =>
          HttpResponse.json({ override: override('o-new', '1024') })
        )
      );
      return holdFirstRead(
        '/api/v1/categories/cat-1/overrides',
        () => HttpResponse.json({ overrides: [] }),
        () => HttpResponse.json({ overrides: [override('o-new', '1024')] })
      );
    },
    read: () => usePermissionStore.getState().fetchCategoryOverrides('cat-1'),
    write: () =>
      usePermissionStore.getState().upsertCategoryOverride('cat-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '1024',
        deny: '0',
      }),
    expectWritten: (s) =>
      expect(s.channelOverrides['category:cat-1']?.map((o) => o.id)).toEqual(['o-new']),
  },
  {
    name: 'a channel override delete',
    arrange: () => {
      usePermissionStore.setState({
        channelOverrides: { 'channel-1': [override('o-1', '1024')] },
      });
      server.use(
        http.delete(
          `${API_BASE}/api/v1/channels/channel-1/overrides/o-1`,
          () => new HttpResponse(null, { status: 204 })
        )
      );
      return holdFirstRead(
        '/api/v1/channels/channel-1/overrides',
        () => HttpResponse.json({ overrides: [override('o-1', '1024')] }),
        () => HttpResponse.json({ overrides: [] })
      );
    },
    read: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
    write: () => usePermissionStore.getState().deleteChannelOverride('channel-1', 'o-1'),
    expectWritten: (s) => expect(s.channelOverrides['channel-1']).toEqual([]),
  },
  {
    name: 'a category override delete',
    arrange: () => {
      usePermissionStore.setState({
        channelOverrides: { 'category:cat-1': [override('o-1', '1024')] },
      });
      server.use(
        http.delete(
          `${API_BASE}/api/v1/categories/cat-1/overrides/o-1`,
          () => new HttpResponse(null, { status: 204 })
        )
      );
      return holdFirstRead(
        '/api/v1/categories/cat-1/overrides',
        () => HttpResponse.json({ overrides: [override('o-1', '1024')] }),
        () => HttpResponse.json({ overrides: [] })
      );
    },
    read: () => usePermissionStore.getState().fetchCategoryOverrides('cat-1'),
    write: () => usePermissionStore.getState().deleteCategoryOverride('cat-1', 'o-1'),
    expectWritten: (s) => expect(s.channelOverrides['category:cat-1']).toEqual([]),
  },
  {
    name: 'turning category sync on',
    arrange: () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/channel-1/permission-sync`, () =>
          HttpResponse.json({ sync_permissions: true })
        )
      );
      return holdFirstRead(
        '/api/v1/channels/channel-1/overrides',
        () => HttpResponse.json({ overrides: [override('o-own', '2048')] }),
        () => HttpResponse.json({ overrides: [override('o-synced', '1024')] })
      );
    },
    read: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
    write: () => usePermissionStore.getState().setCategorySync('channel-1', true),
    expectWritten: (s) =>
      expect(s.channelOverrides['channel-1']?.map((o) => o.id)).toEqual(['o-synced']),
  },
  {
    name: 'a role create',
    arrange: () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/server-1/roles`, () =>
          HttpResponse.json({ role: role('role-new', 'Moderator') })
        )
      );
      return holdFirstRead(
        '/api/v1/servers/server-1/roles',
        () => HttpResponse.json({ roles: [] }),
        () => HttpResponse.json({ roles: [role('role-new', 'Moderator')] })
      );
    },
    read: () => usePermissionStore.getState().fetchRoles('server-1'),
    write: () => usePermissionStore.getState().createRole('server-1', { name: 'Moderator' }),
    expectWritten: (s) => expect(s.serverRoles['server-1']?.map((r) => r.id)).toEqual(['role-new']),
  },
  {
    name: 'a role update',
    arrange: () => {
      usePermissionStore.setState({ serverRoles: { 'server-1': [role('role-1', 'Old')] } });
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/server-1/roles/role-1`, () =>
          HttpResponse.json({ role: role('role-1', 'New') })
        )
      );
      return holdFirstRead(
        '/api/v1/servers/server-1/roles',
        () => HttpResponse.json({ roles: [role('role-1', 'Old')] }),
        () => HttpResponse.json({ roles: [role('role-1', 'New')] })
      );
    },
    read: () => usePermissionStore.getState().fetchRoles('server-1'),
    write: () => usePermissionStore.getState().updateRole('server-1', 'role-1', { name: 'New' }),
    expectWritten: (s) => expect(s.serverRoles['server-1']?.map((r) => r.name)).toEqual(['New']),
  },
  {
    name: 'a role delete',
    arrange: () => {
      usePermissionStore.setState({ serverRoles: { 'server-1': [role('role-1', 'Old')] } });
      server.use(
        http.delete(
          `${API_BASE}/api/v1/servers/server-1/roles/role-1`,
          () => new HttpResponse(null, { status: 204 })
        )
      );
      return holdFirstRead(
        '/api/v1/servers/server-1/roles',
        () => HttpResponse.json({ roles: [role('role-1', 'Old')] }),
        () => HttpResponse.json({ roles: [] })
      );
    },
    read: () => usePermissionStore.getState().fetchRoles('server-1'),
    write: () => usePermissionStore.getState().deleteRole('server-1', 'role-1'),
    expectWritten: (s) => expect(s.serverRoles['server-1']).toEqual([]),
  },
  {
    name: 'a role reorder',
    arrange: () => {
      usePermissionStore.setState({
        serverRoles: {
          'server-1': [{ ...role('role-2', 'Admin'), position: 2 }, role('role-1', 'Mod')],
        },
      });
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/server-1/roles/reorder`, () => HttpResponse.json({}))
      );
      return holdFirstRead(
        '/api/v1/servers/server-1/roles',
        () =>
          HttpResponse.json({
            roles: [{ ...role('role-2', 'Admin'), position: 2 }, role('role-1', 'Mod')],
          }),
        () =>
          HttpResponse.json({
            roles: [{ ...role('role-1', 'Mod'), position: 2 }, role('role-2', 'Admin')],
          })
      );
    },
    read: () => usePermissionStore.getState().fetchRoles('server-1'),
    write: () => {
      const hierarchy: RoleHierarchy = {
        aboveCeiling: [],
        band: usePermissionStore.getState().serverRoles['server-1'] ?? [],
        managed: [],
        pinned: [],
      };
      return usePermissionStore
        .getState()
        .reorderRoles('server-1', buildReorderPayload(hierarchy, ['role-1', 'role-2']));
    },
    expectWritten: (s) =>
      expect(s.serverRoles['server-1']?.map((r) => [r.id, r.position])).toEqual([
        ['role-1', 2],
        ['role-2', 1],
      ]),
  },
];

describe('a read that started before a confirmed write cannot overwrite it (#3406)', () => {
  beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
  afterAll(() => server.close());
  afterEach(() => server.resetHandlers());

  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    usePermissionStore.getState().reset();
    useChannelStore.setState({
      channels: [
        {
          id: 'channel-1',
          server_id: 'server-1',
          name: 'general',
          type: 'text',
          position: 0,
          group_id: 'cat-1',
          sync_permissions: false,
          created_at: '2025-01-01T00:00:00Z',
          updated_at: '2025-01-01T00:00:00Z',
        },
      ],
    });
  });

  it.each(cases)('after $name', async (c) => {
    const stale = c.arrange();
    const pendingRead = c.read();
    await stale.received;

    await expect(c.write()).resolves.toBeTruthy();
    // The write's result is in place before the stale read is released.
    c.expectWritten(usePermissionStore.getState());

    stale.release();
    await pendingRead;

    c.expectWritten(usePermissionStore.getState());
  });

  // Control: the fence is about ORDER, not about reads in general. A read that
  // starts after the write has confirmed still commits.
  it('still commits a read that started after the write confirmed', async () => {
    server.use(
      http.put(`${API_BASE}/api/v1/channels/channel-1/overrides`, () =>
        HttpResponse.json({ override: override('o-new', '1024') })
      ),
      http.get(`${API_BASE}/api/v1/channels/channel-1/overrides`, () =>
        HttpResponse.json({ overrides: [override('o-later', '4096')] })
      )
    );
    await usePermissionStore.getState().upsertChannelOverride('channel-1', {
      target_type: 'role',
      target_id: 'role-1',
      allow: '1024',
      deny: '0',
    });
    await usePermissionStore.getState().fetchChannelOverrides('channel-1');

    expect(usePermissionStore.getState().channelOverrides['channel-1']?.map((o) => o.id)).toEqual([
      'o-later',
    ]);
  });
});

/**
 * Like holdFirstRead, but the requests after the held one answer from `later`
 * in order, the last answer repeating. Lets a write's own refetch fail.
 */
function scriptedRead(path: string, stale: () => Response, later: Array<() => Response>) {
  let release!: () => void;
  let arrive!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const received = new Promise<void>((resolve) => {
    arrive = resolve;
  });
  let calls = 0;
  server.use(
    http.get(`${API_BASE}${path}`, async () => {
      calls += 1;
      if (calls > 1) return later[Math.min(calls - 2, later.length - 1)]();
      arrive();
      await held;
      return stale();
    })
  );
  return { release, received };
}

const serverError = () => new HttpResponse(null, { status: 500 });

// Guards for the parts of the fix the cases above cannot reach. Each was
// found as a surviving mutant of the fix (round 9).
describe('stale permission reads: the paths behind the ordering (#3406)', () => {
  beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
  afterAll(() => server.close());
  afterEach(() => server.resetHandlers());

  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    usePermissionStore.getState().reset();
  });

  // A write that refetches is protected by its refetch landing later, until
  // that refetch fails. Then only the confirmed write's floor stops the stale
  // read, and the stale read re-reads instead of landing.
  it.each([
    {
      name: 'a channel override save',
      path: '/api/v1/channels/channel-1/overrides',
      stale: () => HttpResponse.json({ overrides: [] }),
      fresh: () => HttpResponse.json({ overrides: [override('o-new', '1024')] }),
      arrangeWrite: () =>
        server.use(
          http.put(`${API_BASE}/api/v1/channels/channel-1/overrides`, () =>
            HttpResponse.json({ override: override('o-new', '1024') })
          )
        ),
      read: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
      write: () =>
        usePermissionStore.getState().upsertChannelOverride('channel-1', {
          target_type: 'role',
          target_id: 'role-1',
          allow: '1024',
          deny: '0',
        }),
      view: (s: State) => s.channelOverrides['channel-1']?.map((o) => o.id),
      expected: ['o-new'],
    },
    {
      name: 'a category override save',
      path: '/api/v1/categories/cat-1/overrides',
      stale: () => HttpResponse.json({ overrides: [] }),
      fresh: () => HttpResponse.json({ overrides: [override('o-new', '1024')] }),
      arrangeWrite: () =>
        server.use(
          http.put(`${API_BASE}/api/v1/categories/cat-1/overrides`, () =>
            HttpResponse.json({ override: override('o-new', '1024') })
          )
        ),
      read: () => usePermissionStore.getState().fetchCategoryOverrides('cat-1'),
      write: () =>
        usePermissionStore.getState().upsertCategoryOverride('cat-1', {
          target_type: 'role',
          target_id: 'role-1',
          allow: '1024',
          deny: '0',
        }),
      view: (s: State) => s.channelOverrides['category:cat-1']?.map((o) => o.id),
      expected: ['o-new'],
    },
    {
      name: 'turning category sync on',
      path: '/api/v1/channels/channel-1/overrides',
      stale: () => HttpResponse.json({ overrides: [override('o-own', '2048')] }),
      fresh: () => HttpResponse.json({ overrides: [override('o-synced', '1024')] }),
      arrangeWrite: () =>
        server.use(
          http.put(`${API_BASE}/api/v1/channels/channel-1/permission-sync`, () =>
            HttpResponse.json({ sync_permissions: true })
          )
        ),
      read: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
      write: () => usePermissionStore.getState().setCategorySync('channel-1', true),
      view: (s: State) => s.channelOverrides['channel-1']?.map((o) => o.id),
      expected: ['o-synced'],
    },
    {
      name: 'a role reorder',
      path: '/api/v1/servers/server-1/roles',
      stale: () =>
        HttpResponse.json({
          roles: [{ ...role('role-2', 'Admin'), position: 2 }, role('role-1', 'Mod')],
        }),
      fresh: () =>
        HttpResponse.json({
          roles: [{ ...role('role-1', 'Mod'), position: 2 }, role('role-2', 'Admin')],
        }),
      arrangeWrite: () => {
        usePermissionStore.setState({
          serverRoles: {
            'server-1': [{ ...role('role-2', 'Admin'), position: 2 }, role('role-1', 'Mod')],
          },
        });
        server.use(
          http.patch(`${API_BASE}/api/v1/servers/server-1/roles/reorder`, () =>
            HttpResponse.json({})
          )
        );
      },
      read: () => usePermissionStore.getState().fetchRoles('server-1'),
      write: () => {
        const hierarchy: RoleHierarchy = {
          aboveCeiling: [],
          band: usePermissionStore.getState().serverRoles['server-1'] ?? [],
          managed: [],
          pinned: [],
        };
        return usePermissionStore
          .getState()
          .reorderRoles('server-1', buildReorderPayload(hierarchy, ['role-1', 'role-2']));
      },
      view: (s: State) => s.serverRoles['server-1']?.map((r) => [r.id, r.position]),
      expected: [
        ['role-1', 2],
        ['role-2', 1],
      ],
    },
  ])('after $name whose refetch fails, the stale read re-reads', async (c) => {
    c.arrangeWrite();
    const stale = scriptedRead(c.path, c.stale, [serverError, c.fresh]);
    const pendingRead = c.read();
    await stale.received;

    await c.write();

    stale.release();
    await pendingRead;

    expect(c.view(usePermissionStore.getState())).toEqual(c.expected);
  });

  // A write that patches the view locally patches whatever the view holds. If
  // that view was incomplete, dropping the stale read would leave it so.
  it.each([
    {
      name: 'a role create before the first read landed',
      path: '/api/v1/servers/server-1/roles',
      stale: () => HttpResponse.json({ roles: [role('role-old', 'Member')] }),
      fresh: () =>
        HttpResponse.json({ roles: [role('role-old', 'Member'), role('role-new', 'Moderator')] }),
      arrangeWrite: () =>
        server.use(
          http.post(`${API_BASE}/api/v1/servers/server-1/roles`, () =>
            HttpResponse.json({ role: role('role-new', 'Moderator') })
          )
        ),
      read: () => usePermissionStore.getState().fetchRoles('server-1'),
      write: () => usePermissionStore.getState().createRole('server-1', { name: 'Moderator' }),
      view: (s: State) => s.serverRoles['server-1']?.map((r) => r.id),
      expected: ['role-old', 'role-new'],
    },
    {
      name: 'a channel override delete from a cache missing a row',
      path: '/api/v1/channels/channel-1/overrides',
      stale: () =>
        HttpResponse.json({ overrides: [override('o-1', '1024'), override('o-2', '2048')] }),
      fresh: () => HttpResponse.json({ overrides: [override('o-2', '2048')] }),
      arrangeWrite: () => {
        usePermissionStore.setState({
          channelOverrides: { 'channel-1': [override('o-1', '1024')] },
        });
        server.use(
          http.delete(
            `${API_BASE}/api/v1/channels/channel-1/overrides/o-1`,
            () => new HttpResponse(null, { status: 204 })
          )
        );
      },
      read: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
      write: () => usePermissionStore.getState().deleteChannelOverride('channel-1', 'o-1'),
      view: (s: State) => s.channelOverrides['channel-1']?.map((o) => o.id),
      expected: ['o-2'],
    },
    {
      name: 'a category override delete from a cache missing a row',
      path: '/api/v1/categories/cat-1/overrides',
      stale: () =>
        HttpResponse.json({ overrides: [override('o-1', '1024'), override('o-2', '2048')] }),
      fresh: () => HttpResponse.json({ overrides: [override('o-2', '2048')] }),
      arrangeWrite: () => {
        usePermissionStore.setState({
          channelOverrides: { 'category:cat-1': [override('o-1', '1024')] },
        });
        server.use(
          http.delete(
            `${API_BASE}/api/v1/categories/cat-1/overrides/o-1`,
            () => new HttpResponse(null, { status: 204 })
          )
        );
      },
      read: () => usePermissionStore.getState().fetchCategoryOverrides('cat-1'),
      write: () => usePermissionStore.getState().deleteCategoryOverride('cat-1', 'o-1'),
      view: (s: State) => s.channelOverrides['category:cat-1']?.map((o) => o.id),
      expected: ['o-2'],
    },
  ])('after $name, the stale read re-reads', async (c) => {
    c.arrangeWrite();
    const stale = scriptedRead(c.path, c.stale, [c.fresh]);
    const pendingRead = c.read();
    await stale.received;

    await expect(c.write()).resolves.toBeTruthy();

    stale.release();
    await pendingRead;

    expect(c.view(usePermissionStore.getState())).toEqual(c.expected);
  });

  it('an older read never replaces a newer one', async () => {
    const stale = scriptedRead(
      '/api/v1/channels/channel-1/overrides',
      () => HttpResponse.json({ overrides: [override('o-old', '1024')] }),
      [() => HttpResponse.json({ overrides: [override('o-new', '2048')] })]
    );
    const older = usePermissionStore.getState().fetchChannelOverrides('channel-1');
    await stale.received;
    await usePermissionStore.getState().fetchChannelOverrides('channel-1');

    stale.release();
    await older;

    expect(usePermissionStore.getState().channelOverrides['channel-1']?.map((o) => o.id)).toEqual([
      'o-new',
    ]);
  });

  // reorderRoles reports `reconciled` from its own refetch's return value. A
  // refetch overtaken by a newer read still leaves the view at least that new.
  it('a role read overtaken by a newer one still reports the view refreshed', async () => {
    const stale = scriptedRead(
      '/api/v1/servers/server-1/roles',
      () => HttpResponse.json({ roles: [role('role-1', 'Old')] }),
      [() => HttpResponse.json({ roles: [role('role-1', 'New')] })]
    );
    const older = usePermissionStore.getState().fetchRoles('server-1');
    await stale.received;
    await expect(usePermissionStore.getState().fetchRoles('server-1')).resolves.toBe(true);

    stale.release();

    await expect(older).resolves.toBe(true);
    expect(usePermissionStore.getState().serverRoles['server-1']?.map((r) => r.name)).toEqual([
      'New',
    ]);
  });
});
