import { useInviteStore } from '@/renderer/stores/inviteStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import { useAuthStore } from '@/renderer/stores/authStore';

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
