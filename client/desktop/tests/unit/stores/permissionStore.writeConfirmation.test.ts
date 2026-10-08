// #3456 §3.3/§3.4: the role and override writes answer with a result union and
// take an optional step-up confirmation. The first send is the host's request
// as it always was; the dialog's re-send adds `mfa_code` to the SAME body and is
// admitted against the account and server the dialog opened for.
//
// Each test names, in its first comment line, the production mutation that
// turns it red.
import { usePermissionStore, type WriteConfirmation } from '@/renderer/stores/chat/permissionStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import type { Role } from '@/renderer/types/server';

const API_BASE = 'http://localhost:8080';

const role: Role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  color: '#ff0000',
  position: 1,
  permissions: '1024',
  is_default: false,
  is_managed: false,
  display_separately: false,
  mentionable: false,
  emoji: '',
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const MFA_REQUIRED = { error: 'MFA required', mfa_required: true, methods: ['totp'] };

interface Seen {
  method: string;
  contentType: string | null;
  raw: string;
  json: Record<string, unknown> | undefined;
}

/** Answers `method path` and records what arrived. */
function record(
  method: 'post' | 'patch' | 'put' | 'delete',
  path: string,
  reply: () => Response
): Seen[] {
  const seen: Seen[] = [];
  server.use(
    http[method](`${API_BASE}${path}`, async ({ request }) => {
      const raw = await request.text();
      seen.push({
        method: request.method,
        contentType: request.headers.get('content-type'),
        raw,
        json: raw === '' ? undefined : (JSON.parse(raw) as Record<string, unknown>),
      });
      return reply();
    })
  );
  return seen;
}

function confirmation(mfaCode: string | undefined): WriteConfirmation {
  return { mfaCode, context: captureApiRequestContext() };
}

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
  usePermissionStore.getState().reset();
  usePermissionStore.setState({ serverRoles: { 'server-1': [role] } });
});

const OVERRIDE = {
  target_type: 'role' as const,
  target_id: 'role-9',
  // Above 2^53: a JSON number would lose its low bits (#3406).
  allow: '4611686018427387905',
  deny: '2',
};

describe('permissionStore role and override writes (#3456)', () => {
  describe('the first send is the host request, unchanged', () => {
    it('createRole sends no mfa_code', async () => {
      // Mutation: sendWrite adds mfa_code (even undefined-as-null) when no confirmation is given.
      const seen = record('post', '/api/v1/servers/server-1/roles', () =>
        HttpResponse.json({ role }, { status: 201 })
      );
      await usePermissionStore.getState().createRole('server-1', { name: 'Moderator' });
      expect(seen).toHaveLength(1);
      expect(seen[0].json).toEqual({ name: 'Moderator' });
      expect(seen[0].raw).not.toContain('mfa_code');
    });

    it('updateRole sends no mfa_code', async () => {
      // Mutation: sendWrite adds mfa_code when no confirmation is given.
      const seen = record('patch', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ role })
      );
      await usePermissionStore.getState().updateRole('server-1', 'role-1', { name: 'Mod' });
      expect(seen[0].json).toEqual({ name: 'Mod' });
    });

    it('deleteRole sends no body at all', async () => {
      // Mutation: the delete sends `{}` (or a Content-Type) when it has nothing to say.
      const seen = record('delete', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ message: 'deleted' })
      );
      await usePermissionStore.getState().deleteRole('server-1', 'role-1');
      expect(seen[0].raw).toBe('');
      expect(seen[0].contentType).toBeNull();
    });

    it.each([
      ['channel', 'put', '/api/v1/channels/ch-1/overrides'],
      ['category', 'put', '/api/v1/categories/cat-1/overrides'],
    ] as const)('the %s override upsert sends no mfa_code', async (kind, method, path) => {
      // Mutation: sendWrite adds mfa_code when no confirmation is given.
      const seen = record(method, path, () => HttpResponse.json({}));
      server.use(
        http.get(`${API_BASE}${path}`, () => HttpResponse.json({ overrides: [] })),
        http.get(`${API_BASE}/api/v1/channels/ch-1/overrides`, () =>
          HttpResponse.json({ overrides: [] })
        )
      );
      const state = usePermissionStore.getState();
      await (kind === 'channel'
        ? state.upsertChannelOverride('ch-1', OVERRIDE)
        : state.upsertCategoryOverride('cat-1', OVERRIDE));
      expect(seen[0].json).toEqual(OVERRIDE);
    });
  });

  describe('a refusal is returned as it came and changes nothing', () => {
    it('createRole: status and body, and no role is added', async () => {
      // Mutation: the store words the refusal (or swallows the body) instead of returning it.
      record('post', '/api/v1/servers/server-1/roles', () =>
        HttpResponse.json(MFA_REQUIRED, { status: 403 })
      );
      const result = await usePermissionStore.getState().createRole('server-1', { name: 'X' });
      expect(result).toEqual({
        ok: false,
        kind: 'refused',
        status: 403,
        body: MFA_REQUIRED,
        context: captureApiRequestContext(),
      });
      expect(usePermissionStore.getState().serverRoles['server-1']).toEqual([role]);
    });

    it('updateRole: status and body, and the role keeps its permissions', async () => {
      // Mutation: a refused update still merges the sent data into the role.
      record('patch', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json(MFA_REQUIRED, { status: 403 })
      );
      const result = await usePermissionStore
        .getState()
        .updateRole('server-1', 'role-1', { permissions: '9223372036854775807' });
      expect(result).toEqual({
        ok: false,
        kind: 'refused',
        status: 403,
        body: MFA_REQUIRED,
        context: captureApiRequestContext(),
      });
      expect(usePermissionStore.getState().serverRoles['server-1'][0].permissions).toBe('1024');
    });

    it('deleteRole: status and body, and the role stays in the list', async () => {
      // Mutation: a refused delete still filters the role out of serverRoles.
      record('delete', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json(MFA_REQUIRED, { status: 403 })
      );
      const result = await usePermissionStore.getState().deleteRole('server-1', 'role-1');
      expect(result).toEqual({
        ok: false,
        kind: 'refused',
        status: 403,
        body: MFA_REQUIRED,
        context: captureApiRequestContext(),
      });
      expect(usePermissionStore.getState().serverRoles['server-1']).toEqual([role]);
    });

    it('a refusal whose body is not JSON carries body null', async () => {
      // Mutation: the body parse is not caught, so the refusal becomes a network failure.
      server.use(
        http.delete(
          `${API_BASE}/api/v1/servers/server-1/roles/role-1`,
          () => new HttpResponse('upstream down', { status: 502 })
        )
      );
      const result = await usePermissionStore.getState().deleteRole('server-1', 'role-1');
      expect(result).toEqual({
        ok: false,
        kind: 'refused',
        status: 502,
        body: null,
        context: captureApiRequestContext(),
      });
    });

    it('an override upsert refusal does not re-read the list', async () => {
      // Mutation: the refetch runs before the refusal is checked.
      record('put', '/api/v1/channels/ch-1/overrides', () =>
        HttpResponse.json(MFA_REQUIRED, { status: 403 })
      );
      let reads = 0;
      server.use(
        http.get(`${API_BASE}/api/v1/channels/ch-1/overrides`, () => {
          reads += 1;
          return HttpResponse.json({ overrides: [] });
        })
      );
      const result = await usePermissionStore.getState().upsertChannelOverride('ch-1', OVERRIDE);
      expect(result).toMatchObject({ ok: false, kind: 'refused', status: 403 });
      expect(reads).toBe(0);
    });

    it('a 2xx createRole whose body cannot be read is an unknown outcome', async () => {
      // Mutation: createRole returns { ok: true } before reading the role out of the body.
      server.use(
        http.post(`${API_BASE}/api/v1/servers/server-1/roles`, () => new HttpResponse('not json'))
      );
      const result = await usePermissionStore.getState().createRole('server-1', { name: 'X' });
      expect(result).toEqual({ ok: false, kind: 'network' });
      expect(usePermissionStore.getState().serverRoles['server-1']).toEqual([role]);
    });
  });

  describe('a refusal carries the context its request went out as (C82)', () => {
    it('the first send captures before it sends, not when the answer lands', async () => {
      // Mutation: sendWrite captures in the refused branch (after the response), so the refusal belongs to the successor account.
      const sent = captureApiRequestContext();
      server.use(
        http.post(`${API_BASE}/api/v1/servers/server-1/roles`, () => {
          useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
          return HttpResponse.json(MFA_REQUIRED, { status: 403 });
        })
      );
      const result = await usePermissionStore.getState().createRole('server-1', { name: 'X' });
      expect(result).toMatchObject({ ok: false, kind: 'refused', status: 403 });
      if (result.ok || result.kind !== 'refused') throw new Error('expected a refusal');
      expect(result.context).toEqual(sent);
      expect(result.context.authLifecycle.authGeneration).not.toBe(
        useAuthStore.getState().authGeneration
      );
    });

    it('a re-send carries the dialog capture it was admitted against', async () => {
      // Mutation: sendWrite captures afresh even when the confirmation brings a context.
      record('delete', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json(MFA_REQUIRED, { status: 403 })
      );
      const c = confirmation('123456');
      const result = await usePermissionStore.getState().deleteRole('server-1', 'role-1', c);
      if (result.ok || result.kind !== 'refused') throw new Error('expected a refusal');
      expect(result.context).toBe(c.context);
    });
  });

  describe('the dialog re-send adds mfa_code to the same body', () => {
    it('createRole', async () => {
      // Mutation: the re-send drops the frozen fields or puts the code under another key.
      const seen = record('post', '/api/v1/servers/server-1/roles', () =>
        HttpResponse.json({ role }, { status: 201 })
      );
      const result = await usePermissionStore
        .getState()
        .createRole(
          'server-1',
          { name: 'Moderator', color: '#ff0000', permissions: '1024' },
          confirmation('123456')
        );
      expect(result.ok).toBe(true);
      expect(seen[0].json).toEqual({
        name: 'Moderator',
        color: '#ff0000',
        permissions: '1024',
        mfa_code: '123456',
      });
      expect(seen[0].contentType).toContain('application/json');
    });

    it('updateRole keeps the permissions bitfield a decimal string', async () => {
      // Mutation: the re-send converts permissions to a number (precision loss above 2^53).
      const seen = record('patch', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ role })
      );
      const permissions = '9223372036854775807';
      await usePermissionStore
        .getState()
        .updateRole('server-1', 'role-1', { name: 'Mod', permissions }, confirmation('654321'));
      expect(seen[0].json).toEqual({ name: 'Mod', permissions, mfa_code: '654321' });
      expect(seen[0].raw).toContain(`"permissions":"${permissions}"`);
    });

    it('deleteRole sends a JSON body carrying only mfa_code', async () => {
      // Mutation: the delete re-send drops its body (the code never reaches the server).
      const seen = record('delete', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ message: 'deleted' })
      );
      const result = await usePermissionStore
        .getState()
        .deleteRole('server-1', 'role-1', confirmation('123456'));
      expect(result).toEqual({ ok: true });
      expect(seen[0].json).toEqual({ mfa_code: '123456' });
      expect(seen[0].contentType).toContain('application/json');
      expect(usePermissionStore.getState().serverRoles['server-1']).toEqual([]);
    });

    it.each([
      ['channel', '/api/v1/channels/ch-1/overrides'],
      ['category', '/api/v1/categories/cat-1/overrides'],
    ] as const)(
      'the %s override upsert keeps allow and deny decimal strings',
      async (kind, path) => {
        // Mutation: the re-send re-serialises allow/deny as numbers, or omits the code.
        const seen = record('put', path, () => HttpResponse.json({}));
        server.use(http.get(`${API_BASE}${path}`, () => HttpResponse.json({ overrides: [] })));
        const state = usePermissionStore.getState();
        const result = await (kind === 'channel'
          ? state.upsertChannelOverride('ch-1', OVERRIDE, confirmation('123456'))
          : state.upsertCategoryOverride('cat-1', OVERRIDE, confirmation('123456')));
        expect(result).toEqual({ ok: true });
        expect(seen[0].json).toEqual({ ...OVERRIDE, mfa_code: '123456' });
        expect(seen[0].raw).toContain('"allow":"4611686018427387905"');
      }
    );

    it('a confirmation with no code sends the body unchanged (the server decides)', async () => {
      // Mutation: an absent code is sent as "" or null instead of being left out.
      const seen = record('patch', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ role })
      );
      await usePermissionStore
        .getState()
        .updateRole('server-1', 'role-1', { name: 'Mod' }, confirmation(undefined));
      expect(seen[0].json).toEqual({ name: 'Mod' });
    });

    it('a delete confirmation with no code sends no body', async () => {
      // Mutation: spreading an undefined body turns a code-less delete into `{}`.
      const seen = record('delete', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ message: 'deleted' })
      );
      await usePermissionStore.getState().deleteRole('server-1', 'role-1', confirmation(undefined));
      expect(seen[0].raw).toBe('');
    });
  });

  describe('the re-send is admitted against the dialog account', () => {
    it.each([
      ['createRole', 'post', '/api/v1/servers/server-1/roles'],
      ['updateRole', 'patch', '/api/v1/servers/server-1/roles/role-1'],
      ['deleteRole', 'delete', '/api/v1/servers/server-1/roles/role-1'],
      ['upsertChannelOverride', 'put', '/api/v1/channels/ch-1/overrides'],
      ['upsertCategoryOverride', 'put', '/api/v1/categories/cat-1/overrides'],
    ] as const)(
      '%s sends nothing once another account has signed in',
      async (name, method, path) => {
        // Mutation: sendWrite calls apiFetch(path, init) and drops the captured context.
        const seen = record(method, path, () => HttpResponse.json({ role }));
        const confirm = confirmation('123456');
        useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
        const state = usePermissionStore.getState();
        const runs = {
          createRole: () => state.createRole('server-1', { name: 'X' }, confirm),
          updateRole: () => state.updateRole('server-1', 'role-1', { name: 'X' }, confirm),
          deleteRole: () => state.deleteRole('server-1', 'role-1', confirm),
          upsertChannelOverride: () => state.upsertChannelOverride('ch-1', OVERRIDE, confirm),
          upsertCategoryOverride: () => state.upsertCategoryOverride('cat-1', OVERRIDE, confirm),
        };
        const result = await runs[name]();
        expect(result).toEqual({ ok: false, kind: 'aborted' });
        expect(seen).toHaveLength(0);
      }
    );

    it('a first send is not fenced by a context it never captured', async () => {
      // Mutation: sendWrite requires a context and aborts a code-less first send.
      record('delete', '/api/v1/servers/server-1/roles/role-1', () =>
        HttpResponse.json({ message: 'deleted' })
      );
      expect(await usePermissionStore.getState().deleteRole('server-1', 'role-1')).toEqual({
        ok: true,
      });
    });
  });
});
