import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    encryptPreferences: vi.fn().mockResolvedValue('encrypted-blob'),
    decryptPreferences: vi.fn(),
  },
}));

import { e2eeService } from '@/renderer/services/e2eeService';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => {
  server.resetHandlers();
  friendOrgSyncService.stopWatching();
});

describe('friendOrgSyncService', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    vi.mocked(e2eeService.encryptPreferences).mockClear().mockResolvedValue('encrypted-blob');
    vi.mocked(e2eeService.decryptPreferences).mockReset();
    (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
    // fetchAndApply sets isApplyingRemote=true and only clears it via
    // setTimeout(0); if a prior test exits before that microtask drains,
    // the flag leaks here and silently disables schedulePush.
    (friendOrgSyncService as unknown as { isApplyingRemote: boolean }).isApplyingRemote = false;
    // friendOrgStore has no persist; reset to an empty blob explicitly.
    useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });
  });

  describe('pushFriendOrg', () => {
    it('encrypts and pushes the friend-org blob to the server', async () => {
      useFriendOrgStore.getState().createCategory('Close Friends', '💜', '#fa709a');

      let pushedBody: { encrypted_data: string } | null = null;
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/friend-organization`, async ({ request }) => {
          pushedBody = (await request.json()) as { encrypted_data: string };
          return HttpResponse.json({ version: 1 });
        })
      );

      await friendOrgSyncService.pushFriendOrg();

      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
      expect(pushedBody).toEqual({ encrypted_data: 'encrypted-blob' });
    });

    it('does nothing when e2ee is not initialized', async () => {
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = false;
      vi.mocked(e2eeService.encryptPreferences).mockClear();

      await friendOrgSyncService.pushFriendOrg();

      expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
    });
  });

  describe('fetchAndApply', () => {
    it('decrypts and applies a remote blob (round-trip)', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        categories: [{ id: 'cat_1', name: 'A', emoji: '', color: null, memberIds: ['u1'] }],
        sectionOrder: ['cat_1', 'online'],
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
          HttpResponse.json({
            friend_organization: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await friendOrgSyncService.fetchAndApply();

      const s = useFriendOrgStore.getState();
      expect(s.categories).toHaveLength(1);
      expect(s.categories[0]).toMatchObject({ id: 'cat_1', name: 'A', memberIds: ['u1'] });
      expect(s.sectionOrder).toEqual(['cat_1', 'online']);
    });

    it('pushes local state when the server has no friend organization', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
          HttpResponse.json({ friend_organization: null })
        ),
        http.put(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
          HttpResponse.json({ version: 1 })
        )
      );

      await friendOrgSyncService.fetchAndApply();

      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
    });

    it('rejects a malformed (overlapping-memberIds) blob and leaves the store EMPTY', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        categories: [
          { id: 'cat_1', name: 'A', emoji: '', color: null, memberIds: ['u1'] },
          { id: 'cat_2', name: 'B', emoji: '', color: null, memberIds: ['u1'] }, // u1 in two cats
        ],
        sectionOrder: ['cat_1', 'cat_2'],
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
          HttpResponse.json({
            friend_organization: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await friendOrgSyncService.fetchAndApply();

      const s = useFriendOrgStore.getState();
      expect(s.categories).toEqual([]);
      expect(s.sectionOrder).toEqual([]);
    });

    it('handles fetch failure gracefully', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
          HttpResponse.json({ error: 'Failed' }, { status: 500 })
        )
      );

      await expect(friendOrgSyncService.fetchAndApply()).resolves.toBeUndefined();
    });
  });

  describe('startWatching / debounced push', () => {
    it('schedules an encrypted push when the store changes after startWatching', async () => {
      vi.useFakeTimers();
      try {
        friendOrgSyncService.startWatching();
        useFriendOrgStore.getState().createCategory('Triggers Push', '', null);
        // Before the debounce fires, no push yet.
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
        // Advance past the 3s debounce — the push should now have run once.
        await vi.advanceTimersByTimeAsync(3500);
        expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);
      } finally {
        friendOrgSyncService.stopWatching();
        vi.useRealTimers();
      }
    });

    it('clears the pending debounce timer on stopWatching (no late push)', async () => {
      vi.useFakeTimers();
      try {
        friendOrgSyncService.startWatching();
        useFriendOrgStore.getState().createCategory('Pending', '', null);
        friendOrgSyncService.stopWatching();
        await vi.advanceTimersByTimeAsync(5000);
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      } finally {
        vi.useRealTimers();
      }
    });

    it('does NOT schedule a push while applying a remote blob (echo guard, no apply→push loop)', async () => {
      vi.useFakeTimers();
      try {
        vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
          v: 1,
          categories: [{ id: 'cat_1', name: 'A', emoji: '', color: null, memberIds: [] }],
          sectionOrder: ['cat_1'],
        });
        server.use(
          http.get(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
            HttpResponse.json({
              friend_organization: { encrypted_data: 'encrypted', version: 1 },
            })
          )
        );

        // Watch BEFORE the remote apply so the store-change from _hydrate would
        // normally schedule a push — the echo guard must suppress it.
        friendOrgSyncService.startWatching();
        await friendOrgSyncService.fetchAndApply();
        // The apply mutated the store; advance past the debounce window.
        await vi.advanceTimersByTimeAsync(3500);
        // No push must have been scheduled from the apply (echo guard held).
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      } finally {
        friendOrgSyncService.stopWatching();
        vi.useRealTimers();
      }
    });
  });
});
