// Regression for #3406 (Codex review, round 8): an account change clears the
// permission store (gracefulReset → reset), but it cannot cancel a request
// already in flight. A read or write started by account A that resolves after
// account B signed in wrote A's data into B's store: A's effective permissions
// for a server they share, A's roles, A's override lists.
//
// Oracle: a permission-store continuation whose account is no longer signed in
// writes nothing. The same continuation still writes when the account has not
// changed (the control), so the fence cannot pass by writing nothing ever.
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { buildReorderPayload } from '@/renderer/utils/policy/roleHierarchy';
import type { RoleHierarchy } from '@/renderer/types/server';

const API_BASE = 'http://localhost:8080';

const role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  position: 1,
  permissions: '1024',
  is_default: false,
  is_managed: false,
  display_separately: false,
  mentionable: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

/**
 * A network response held until `open()` is called. `received` settles once the
 * server has the request: apiFetch refuses a request whose account changes
 * BEFORE dispatch, so the account must change after this for the continuation
 * to be the one under test.
 */
function gate(): {
  held: Promise<void>;
  open: () => void;
  received: Promise<void>;
  arrive: () => void;
} {
  let open!: () => void;
  let arrive!: () => void;
  const held = new Promise<void>((resolve) => {
    open = resolve;
  });
  const received = new Promise<void>((resolve) => {
    arrive = resolve;
  });
  return { held, open, received, arrive };
}

type State = ReturnType<typeof usePermissionStore.getState>;

interface FenceCase {
  name: string;
  method: 'get' | 'post' | 'patch' | 'put' | 'delete';
  path: string;
  reply: () => Response;
  run: () => Promise<unknown>;
  /** What the continuation writes when the account has not changed. */
  written: (s: State) => unknown;
}

const cases: FenceCase[] = [
  {
    name: 'fetchServerPermissions',
    method: 'get',
    path: '/api/v1/servers/server-1/permissions',
    reply: () => HttpResponse.json({ permissions: '1024' }),
    run: () => usePermissionStore.getState().fetchServerPermissions('server-1'),
    written: (s) => s.serverPermissions['server-1'],
  },
  {
    name: 'fetchChannelPermissions',
    method: 'get',
    path: '/api/v1/channels/channel-1/permissions',
    reply: () => HttpResponse.json({ permissions: '1024' }),
    run: () => usePermissionStore.getState().fetchChannelPermissions('channel-1'),
    written: (s) => s.channelPermissions['channel-1'],
  },
  {
    name: 'fetchRoles',
    method: 'get',
    path: '/api/v1/servers/server-1/roles',
    reply: () => HttpResponse.json({ roles: [role] }),
    run: () => usePermissionStore.getState().fetchRoles('server-1'),
    written: (s) => s.serverRoles['server-1'],
  },
  {
    name: 'createRole',
    method: 'post',
    path: '/api/v1/servers/server-1/roles',
    reply: () => HttpResponse.json({ role }),
    run: () => usePermissionStore.getState().createRole('server-1', { name: 'Moderator' }),
    written: (s) => s.serverRoles['server-1'],
  },
  {
    name: 'updateRole',
    method: 'patch',
    path: '/api/v1/servers/server-1/roles/role-1',
    reply: () => HttpResponse.json({ role }),
    run: () => usePermissionStore.getState().updateRole('server-1', 'role-1', { name: 'Mod' }),
    written: (s) => s.serverRoles['server-1'],
  },
  {
    name: 'deleteRole',
    method: 'delete',
    path: '/api/v1/servers/server-1/roles/role-1',
    reply: () => new HttpResponse(null, { status: 204 }),
    run: () => usePermissionStore.getState().deleteRole('server-1', 'role-1'),
    written: (s) => s.serverRoles['server-1'],
  },
  {
    name: 'fetchChannelOverrides',
    method: 'get',
    path: '/api/v1/channels/channel-1/overrides',
    reply: () => HttpResponse.json({ overrides: [] }),
    run: () => usePermissionStore.getState().fetchChannelOverrides('channel-1'),
    written: (s) => s.channelOverrides['channel-1'],
  },
  {
    name: 'deleteChannelOverride',
    method: 'delete',
    path: '/api/v1/channels/channel-1/overrides/override-1',
    reply: () => new HttpResponse(null, { status: 204 }),
    run: () => usePermissionStore.getState().deleteChannelOverride('channel-1', 'override-1'),
    written: (s) => s.channelOverrides['channel-1'],
  },
  {
    name: 'fetchCategoryOverrides',
    method: 'get',
    path: '/api/v1/categories/cat-1/overrides',
    reply: () => HttpResponse.json({ overrides: [] }),
    run: () => usePermissionStore.getState().fetchCategoryOverrides('cat-1'),
    written: (s) => s.channelOverrides['category:cat-1'],
  },
  {
    name: 'deleteCategoryOverride',
    method: 'delete',
    path: '/api/v1/categories/cat-1/overrides/override-1',
    reply: () => new HttpResponse(null, { status: 204 }),
    run: () => usePermissionStore.getState().deleteCategoryOverride('cat-1', 'override-1'),
    written: (s) => s.channelOverrides['category:cat-1'],
  },
];

function hold(c: FenceCase) {
  const g = gate();
  const resolver: HttpResponseResolver = async () => {
    g.arrive();
    await g.held;
    return c.reply();
  };
  server.use(http[c.method](`${API_BASE}${c.path}`, resolver));
  return g;
}

describe('permission store continuations are fenced to the account that started them (#3406)', () => {
  beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
  afterAll(() => server.close());
  afterEach(() => server.resetHandlers());

  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    usePermissionStore.getState().reset();
  });

  it.each(cases)('$name writes nothing once another account has signed in', async (c) => {
    const g = hold(c);
    const pending = c.run();
    await g.received;

    // Account A signs out and account B signs in while A's request is open.
    usePermissionStore.getState().reset();
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');

    g.open();
    await pending;

    expect(c.written(usePermissionStore.getState())).toBeUndefined();
  });

  it.each(cases)('$name still writes when the account has not changed', async (c) => {
    const g = hold(c);
    const pending = c.run();
    await g.received;
    g.open();
    await pending;

    expect(c.written(usePermissionStore.getState())).toBeDefined();
  });
  // setCategorySync writes to the channel store as well (round 8), behind the
  // same fence.
  it('setCategorySync records nothing once another account has signed in', async () => {
    useChannelStore.setState({
      channels: [
        {
          id: 'channel-1',
          server_id: 'server-1',
          name: 'general',
          type: 'text',
          position: 0,
          group_id: 'group-1',
          sync_permissions: false,
          created_at: '2025-01-01T00:00:00Z',
          updated_at: '2025-01-01T00:00:00Z',
        },
      ],
    });
    const g = hold({
      name: 'setCategorySync',
      method: 'put',
      path: '/api/v1/channels/channel-1/permission-sync',
      reply: () => HttpResponse.json({ sync_permissions: true }),
      run: () => Promise.resolve(),
      written: () => undefined,
    });
    const pending = usePermissionStore.getState().setCategorySync('channel-1', true);
    await g.received;
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
    g.open();

    expect(await pending).toBe(false);
    expect(useChannelStore.getState().channels[0].sync_permissions).toBe(false);
    expect(usePermissionStore.getState().channelOverrides['channel-1']).toBeUndefined();
  });
});

// Regression for #3406 (Gitar review of round 9): the two override saves and
// the role reorder never captured the account. A write whose response settled
// after another account signed in still marked the scope confirmed, reported
// success to the modal that started it, and fired its refetch on the NEW
// account's behalf. That refetch is fenced to the account it starts under, so
// it proceeds.
//
// Oracle: once the account has changed, a save reports failure and neither
// write issues a further request. The reorder still reports ok, because its
// write committed, but reconciled: false, because the view was not re-read.
describe('writes that re-read are fenced to the account that started them (#3406)', () => {
  beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
  afterAll(() => server.close());
  afterEach(() => server.resetHandlers());

  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    usePermissionStore.getState().reset();
  });

  const reorder = () => {
    const hierarchy: RoleHierarchy = {
      aboveCeiling: [],
      band: [
        { ...role, id: 'role-2', position: 2 },
        { ...role, id: 'role-1', position: 1 },
      ],
      managed: [],
      pinned: [],
    };
    return usePermissionStore
      .getState()
      .reorderRoles('server-1', buildReorderPayload(hierarchy, ['role-1', 'role-2']));
  };

  const writes = [
    {
      name: 'a channel override save',
      method: 'put' as const,
      writePath: '/api/v1/channels/channel-1/overrides',
      readPath: '/api/v1/channels/channel-1/overrides',
      readBody: { overrides: [] },
      run: () =>
        usePermissionStore.getState().upsertChannelOverride('channel-1', {
          target_type: 'role',
          target_id: 'role-1',
          allow: '1024',
          deny: '0',
        }),
      fenced: { ok: false, kind: 'network' } as unknown,
      control: { ok: true } as unknown,
    },
    {
      name: 'a category override save',
      method: 'put' as const,
      writePath: '/api/v1/categories/cat-1/overrides',
      readPath: '/api/v1/categories/cat-1/overrides',
      readBody: { overrides: [] },
      run: () =>
        usePermissionStore.getState().upsertCategoryOverride('cat-1', {
          target_type: 'role',
          target_id: 'role-1',
          allow: '1024',
          deny: '0',
        }),
      fenced: { ok: false, kind: 'network' } as unknown,
      control: { ok: true } as unknown,
    },
    {
      name: 'a role reorder',
      method: 'patch' as const,
      writePath: '/api/v1/servers/server-1/roles/reorder',
      readPath: '/api/v1/servers/server-1/roles',
      readBody: { roles: [role] },
      run: reorder,
      fenced: { ok: true, reconciled: false } as unknown,
      control: { ok: true, reconciled: true } as unknown,
    },
  ];

  function arrange(w: (typeof writes)[number]) {
    const g = gate();
    let reads = 0;
    server.use(
      http[w.method](`${API_BASE}${w.writePath}`, async () => {
        g.arrive();
        await g.held;
        return HttpResponse.json({});
      }),
      http.get(`${API_BASE}${w.readPath}`, () => {
        reads += 1;
        return HttpResponse.json(w.readBody);
      })
    );
    return { g, reads: () => reads };
  }

  it.each(writes)('$name issues no request once another account has signed in', async (w) => {
    const { g, reads } = arrange(w);
    const pending = w.run();
    await g.received;

    usePermissionStore.getState().reset();
    useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');

    g.open();
    expect(await pending).toEqual(w.fenced);
    expect(reads()).toBe(0);
  });

  it.each(writes)('$name still re-reads when the account has not changed', async (w) => {
    const { g, reads } = arrange(w);
    const pending = w.run();
    await g.received;

    g.open();
    expect(await pending).toEqual(w.control);
    expect(reads()).toBe(1);
  });
});
