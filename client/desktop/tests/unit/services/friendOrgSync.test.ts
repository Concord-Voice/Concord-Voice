import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { useFriendOrgStore } from '@/renderer/stores/chat/friendOrgStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
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
import { deferred } from '../../helpers/deferred';

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
    // friendOrgStore has no persist; reset decrypted state explicitly.
    useFriendOrgStore.getState().reset();
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

    it('does not send prior-account ciphertext after stop while encryption is pending', async () => {
      const encryptionStarted = deferred<void>();
      const releaseEncryption = deferred<void>();
      vi.mocked(e2eeService.encryptPreferences).mockImplementationOnce(async () => {
        encryptionStarted.resolve();
        await releaseEncryption.promise;
        return 'prior-account-ciphertext';
      });

      let putCount = 0;
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/friend-organization`, () => {
          putCount += 1;
          return HttpResponse.json({ version: 1 });
        })
      );

      useFriendOrgStore.getState().createCategory('Prior account', '', null);
      const push = friendOrgSyncService.pushFriendOrg();
      await encryptionStarted.promise;

      friendOrgSyncService.stopWatching();
      useAuthStore.getState().setAccessToken('next-account-token');
      releaseEncryption.resolve();
      await push;

      expect(putCount).toBe(0);
    });
  });

  describe('fetchAndApply', () => {
    it('ignores a prior-account response released after stop and store reset', async () => {
      const requestStarted = deferred<void>();
      const releaseResponse = deferred<void>();
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        categories: [
          {
            id: 'cat_prior',
            name: 'Prior account',
            emoji: '',
            color: null,
            memberIds: ['prior-user'],
          },
        ],
        sectionOrder: ['cat_prior'],
      });
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/friend-organization`, async () => {
          requestStarted.resolve();
          await releaseResponse.promise;
          return HttpResponse.json({
            friend_organization: { encrypted_data: 'prior-ciphertext', version: 1 },
          });
        })
      );

      const fetch = friendOrgSyncService.fetchAndApply();
      await requestStarted.promise;
      friendOrgSyncService.stopWatching();
      useFriendOrgStore.getState().reset();
      releaseResponse.resolve();
      await fetch;

      expect(useFriendOrgStore.getState().categories).toEqual([]);
      expect(useFriendOrgStore.getState().sectionOrder).toEqual([]);
    });

    it('does not bootstrap prior-account state after stop and store reset', async () => {
      const requestStarted = deferred<void>();
      const releaseResponse = deferred<void>();
      let putCount = 0;
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/friend-organization`, async () => {
          requestStarted.resolve();
          await releaseResponse.promise;
          return HttpResponse.json({ friend_organization: null });
        }),
        http.put(`${API_BASE}/api/v1/users/me/friend-organization`, () => {
          putCount += 1;
          return HttpResponse.json({ version: 1 });
        })
      );

      const fetch = friendOrgSyncService.fetchAndApply();
      await requestStarted.promise;
      friendOrgSyncService.stopWatching();
      useFriendOrgStore.getState().reset();
      releaseResponse.resolve();
      await fetch;

      expect(putCount).toBe(0);
    });

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

      await expect(friendOrgSyncService.fetchAndApply()).resolves.toBe(true);
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

    it('cancels the pending remote-apply timer and restarts from a non-applying state', async () => {
      vi.useFakeTimers();
      try {
        vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
          v: 1,
          categories: [{ id: 'cat_remote', name: 'Remote', emoji: '', color: null, memberIds: [] }],
          sectionOrder: ['cat_remote'],
        });
        server.use(
          http.get(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
            HttpResponse.json({
              friend_organization: { encrypted_data: 'encrypted', version: 1 },
            })
          ),
          http.put(`${API_BASE}/api/v1/users/me/friend-organization`, () =>
            HttpResponse.json({ version: 2 })
          )
        );

        friendOrgSyncService.startWatching();
        await friendOrgSyncService.fetchAndApply();
        expect(vi.getTimerCount()).toBe(1);

        // A second remote apply replaces the owned reset timer instead of
        // leaving an untracked prior-account callback behind.
        await friendOrgSyncService.fetchAndApply();
        expect(vi.getTimerCount()).toBe(1);

        friendOrgSyncService.stopWatching();
        expect(vi.getTimerCount()).toBe(0);

        friendOrgSyncService.startWatching();
        useFriendOrgStore.getState().createCategory('New account', '', null);
        await vi.advanceTimersByTimeAsync(3500);
        expect(e2eeService.encryptPreferences).toHaveBeenCalledOnce();
      } finally {
        friendOrgSyncService.stopWatching();
        vi.useRealTimers();
      }
    });
  });
});
