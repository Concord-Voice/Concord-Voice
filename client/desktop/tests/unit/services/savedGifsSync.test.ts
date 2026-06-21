import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { useSavedGifsStore } from '@/renderer/stores/savedGifsStore';
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
  savedGifsSyncService.stopWatching();
});

describe('savedGifsSyncService', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    // Clear call history AND re-arm the resolved value — without the clear,
    // call counts from prior tests in this file leak into per-test assertions
    // (notably the new watch/debounce tests that assert toHaveBeenCalledTimes).
    vi.mocked(e2eeService.encryptPreferences).mockClear().mockResolvedValue('encrypted-blob');
    vi.mocked(e2eeService.decryptPreferences).mockReset();
    (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
    // fetchAndApply sets isApplyingRemote=true and only clears it via
    // setTimeout(0); if a prior test exits before that microtask drains,
    // the flag leaks here and silently disables schedulePush.
    (savedGifsSyncService as unknown as { isApplyingRemote: boolean }).isApplyingRemote = false;
  });

  describe('pushSavedGifs', () => {
    it('encrypts and pushes saved GIFs to server', async () => {
      useSavedGifsStore.getState().saveGif('abc123');

      let pushedBody: { encrypted_data: string } | null = null;
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/saved-gifs`, async ({ request }) => {
          pushedBody = (await request.json()) as { encrypted_data: string };
          return HttpResponse.json({ version: 1 });
        })
      );

      await savedGifsSyncService.pushSavedGifs();

      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
      expect(pushedBody).toEqual({ encrypted_data: 'encrypted-blob' });
    });

    it('does nothing when e2ee is not initialized', async () => {
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = false;
      vi.mocked(e2eeService.encryptPreferences).mockClear();

      await savedGifsSyncService.pushSavedGifs();

      expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
    });

    it('handles push failure gracefully', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/users/me/saved-gifs`, () =>
          HttpResponse.json({ error: 'Failed' }, { status: 500 })
        )
      );

      // Should not throw on server error
      await expect(savedGifsSyncService.pushSavedGifs()).resolves.toBeUndefined();
    });

    it('handles encryption failure gracefully', async () => {
      vi.mocked(e2eeService.encryptPreferences).mockRejectedValueOnce(new Error('encrypt fail'));

      // Should not throw when encryption rejects
      await expect(savedGifsSyncService.pushSavedGifs()).resolves.toBeUndefined();
    });
  });

  describe('fetchAndApply', () => {
    it('does nothing when e2ee is not initialized', async () => {
      (e2eeService as unknown as { isInitialized: boolean }).isInitialized = false;
      vi.mocked(e2eeService.decryptPreferences).mockClear();

      await savedGifsSyncService.fetchAndApply();

      expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
    });

    it('pushes local state when server has no saved GIFs', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/saved-gifs`, () =>
          HttpResponse.json({ saved_gifs: null })
        ),
        http.put(`${API_BASE}/api/v1/users/me/saved-gifs`, () => HttpResponse.json({ version: 1 }))
      );

      await savedGifsSyncService.fetchAndApply();

      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
    });

    it('decrypts and applies remote saved GIFs', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 1,
        gifs: [
          { slug: 'abc123', savedAt: 1000 },
          { slug: 'def456', savedAt: 2000 },
        ],
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/saved-gifs`, () =>
          HttpResponse.json({
            saved_gifs: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await savedGifsSyncService.fetchAndApply();

      const gifs = useSavedGifsStore.getState().gifs;
      expect(gifs).toHaveLength(2);
      expect(gifs[0].slug).toBe('abc123');
      expect(gifs[1].slug).toBe('def456');
    });

    it('ignores unknown blob versions', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
        v: 99,
        gifs: [{ slug: 'should-not-apply', savedAt: 1 }],
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/saved-gifs`, () =>
          HttpResponse.json({
            saved_gifs: { encrypted_data: 'encrypted', version: 1 },
          })
        )
      );

      await savedGifsSyncService.fetchAndApply();

      expect(useSavedGifsStore.getState().gifs).toEqual([]);
    });

    it('re-pushes local state when server data fails to decrypt', async () => {
      vi.mocked(e2eeService.decryptPreferences).mockRejectedValueOnce(new Error('decrypt fail'));
      useSavedGifsStore.getState().saveGif('local-only');

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/saved-gifs`, () =>
          HttpResponse.json({
            saved_gifs: { encrypted_data: 'bad-cipher', version: 1 },
          })
        ),
        http.put(`${API_BASE}/api/v1/users/me/saved-gifs`, () => HttpResponse.json({ version: 2 }))
      );

      await savedGifsSyncService.fetchAndApply();

      // Should have pushed local state to overwrite stale server data
      expect(e2eeService.encryptPreferences).toHaveBeenCalled();
    });

    it('handles fetch failure gracefully', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/saved-gifs`, () =>
          HttpResponse.json({ error: 'Failed' }, { status: 500 })
        )
      );

      // Should not throw on server error
      await expect(savedGifsSyncService.fetchAndApply()).resolves.toBeUndefined();
    });
  });

  describe('startWatching / stopWatching', () => {
    it('stopWatching as a no-op leaves no scheduled work behind', () => {
      vi.useFakeTimers();
      try {
        savedGifsSyncService.stopWatching();
        // No watch was started, so advancing through the full debounce window
        // must not produce any encrypt/push side-effect.
        vi.advanceTimersByTime(10_000);
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      } finally {
        vi.useRealTimers();
      }
    });

    it('startWatching subscribes to store changes', () => {
      const subscribeSpy = vi.spyOn(useSavedGifsStore, 'subscribe');
      try {
        savedGifsSyncService.startWatching();
        expect(subscribeSpy).toHaveBeenCalledTimes(1);
      } finally {
        savedGifsSyncService.stopWatching();
        subscribeSpy.mockRestore();
      }
    });

    it('startWatching cleans up previous subscriptions when called again', () => {
      // Replace subscribe with a pure mock that hands back captured unsub fns;
      // proves the first unsub gets invoked when the second startWatching
      // calls stopWatching internally to tear down stale state.
      const unsubMocks: ReturnType<typeof vi.fn>[] = [];
      const subscribeSpy = vi.spyOn(useSavedGifsStore, 'subscribe').mockImplementation(() => {
        const unsub = vi.fn();
        unsubMocks.push(unsub);
        return unsub;
      });
      try {
        savedGifsSyncService.startWatching();
        savedGifsSyncService.startWatching();
        expect(subscribeSpy).toHaveBeenCalledTimes(2);
        // The first subscribe's unsub must have been called by the second
        // startWatching's internal stopWatching cleanup — proves no leak.
        expect(unsubMocks[0]).toHaveBeenCalled();
        // The second subscription is the new active one and must NOT have
        // been unsubscribed yet.
        expect(unsubMocks[1]).not.toHaveBeenCalled();
      } finally {
        savedGifsSyncService.stopWatching();
        subscribeSpy.mockRestore();
      }
    });

    it('schedules an encrypted push when store changes after startWatching', async () => {
      vi.useFakeTimers();
      try {
        savedGifsSyncService.startWatching();
        useSavedGifsStore.getState().saveGif('triggers-push');
        // Before the debounce fires, no push yet
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
        // Advance past the 3s debounce — the push should now have run once
        await vi.advanceTimersByTimeAsync(3500);
        expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);
      } finally {
        savedGifsSyncService.stopWatching();
        vi.useRealTimers();
      }
    });

    it('clears the pending debounce timer on stopWatching (no late push)', async () => {
      vi.useFakeTimers();
      try {
        savedGifsSyncService.startWatching();
        useSavedGifsStore.getState().saveGif('pending');
        // Stop BEFORE the debounce fires — the clearTimeout path must prevent
        // the push from being executed when time advances past the debounce.
        savedGifsSyncService.stopWatching();
        await vi.advanceTimersByTimeAsync(5000);
        expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      } finally {
        vi.useRealTimers();
      }
    });

    it('coalesces rapid changes into a single debounced push', async () => {
      vi.useFakeTimers();
      try {
        savedGifsSyncService.startWatching();
        useSavedGifsStore.getState().saveGif('one');
        useSavedGifsStore.getState().saveGif('two');
        useSavedGifsStore.getState().saveGif('three');
        await vi.advanceTimersByTimeAsync(3500);
        // Three rapid saveGif() calls reset the debounce timer each time, so
        // only ONE push lands once the window finally elapses.
        expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);
      } finally {
        savedGifsSyncService.stopWatching();
        vi.useRealTimers();
      }
    });
  });
});
