import { http, HttpResponse } from 'msw';
import { presenceOverrideSyncService } from '@/renderer/services/system/presenceOverrideSync';
import { usePresenceOverrideStore } from '@/renderer/stores/ui/presenceOverrideStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';

const API_BASE = 'http://localhost:8080';
const ENDPOINT = `${API_BASE}/api/v1/users/me/presence-overrides/custom_text`;
const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';
const UUID_C = '33333333-3333-4333-8333-333333333333';

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    encryptPreferences: vi.fn(),
    decryptPreferences: vi.fn(),
  },
}));

import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { deferred } from '../../helpers/deferred';

function remotePreference(version: number, encryptedData = 'remote-ciphertext') {
  return {
    preference: {
      encrypted_data: encryptedData,
      version,
      updated_at: '2026-07-11T12:00:00.000Z',
    },
  };
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  presenceOverrideSyncService.reset();
  usePresenceOverrideStore.getState().reset();
  useAuthStore.getState().setAccessToken('mock-token');
  (e2eeService as unknown as { isInitialized: boolean }).isInitialized = true;
  vi.mocked(e2eeService.encryptPreferences).mockReset().mockResolvedValue('encrypted-document');
  vi.mocked(e2eeService.decryptPreferences).mockReset();
});

afterEach(() => {
  presenceOverrideSyncService.reset();
  server.resetHandlers();
});

describe('presenceOverrideSyncService.fetchAndApply', () => {
  it('applies null as empty version zero without bootstrapping a PUT', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 8);
    let putCount = 0;
    server.use(
      http.get(ENDPOINT, () => HttpResponse.json({ preference: null })),
      http.put(ENDPOINT, () => {
        putCount += 1;
        return HttpResponse.json({ version: 1 });
      })
    );

    await expect(presenceOverrideSyncService.fetchAndApply()).resolves.toBe(true);

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [],
      appliedVersion: 0,
      loading: false,
      error: null,
    });
    expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
    expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
    expect(putCount).toBe(0);
  });

  it('decrypts, strictly validates, canonicalizes, and applies a preference', async () => {
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_B, UUID_A, UUID_B],
    });
    server.use(http.get(ENDPOINT, () => HttpResponse.json(remotePreference(3))));

    await presenceOverrideSyncService.fetchAndApply();

    expect(e2eeService.decryptPreferences).toHaveBeenCalledWith('remote-ciphertext');
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_A, UUID_B],
      appliedVersion: 3,
      loading: false,
      error: null,
    });
  });

  it('preserves applied state when E2EE is unavailable', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 2);
    (e2eeService as unknown as { isInitialized: boolean }).isInitialized = false;
    let getCount = 0;
    server.use(
      http.get(ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json({ preference: null });
      })
    );

    await presenceOverrideSyncService.fetchAndApply();

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_A],
      appliedVersion: 2,
      loading: false,
      error: 'Presence exception encryption is unavailable',
    });
    expect(getCount).toBe(0);
  });

  it.each([
    {
      name: 'decryption failure',
      configure: () =>
        vi
          .mocked(e2eeService.decryptPreferences)
          .mockRejectedValue(new Error('sentinel-private-ciphertext')),
    },
    {
      name: 'malformed decrypted document',
      configure: () =>
        vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
          v: 1,
          excludedUserIds: [UUID_B, 'sentinel-private-identifier'],
        }),
    },
  ])('preserves applied state on $name without leaking details', async ({ configure }) => {
    usePresenceOverrideStore.getState().apply([UUID_A], 2);
    configure();
    server.use(http.get(ENDPOINT, () => HttpResponse.json(remotePreference(3))));

    await presenceOverrideSyncService.fetchAndApply();

    const state = usePresenceOverrideStore.getState();
    expect(state.excludedUserIds).toEqual([UUID_A]);
    expect(state.appliedVersion).toBe(2);
    expect(state.error).toBe('Failed to load presence exceptions');
    expect(state.error).not.toContain('sentinel');
  });

  it('rejects a non-exact GET envelope before decryption', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 2);
    server.use(
      http.get(ENDPOINT, () =>
        HttpResponse.json({
          ...remotePreference(3),
          excluded_user_ids: [UUID_B],
        })
      )
    );

    await presenceOverrideSyncService.fetchAndApply();

    expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_A],
      appliedVersion: 2,
      error: 'Failed to load presence exceptions',
    });
  });
});

describe('presenceOverrideSyncService.save', () => {
  it('encrypts the canonical document and sends the exact CAS body', async () => {
    usePresenceOverrideStore.getState().apply([UUID_C], 3);
    let pushedBody: unknown;
    server.use(
      http.put(ENDPOINT, async ({ request }) => {
        pushedBody = await request.json();
        return HttpResponse.json({ version: 4 });
      })
    );

    await presenceOverrideSyncService.save([UUID_B, UUID_A, UUID_B]);

    expect(e2eeService.encryptPreferences).toHaveBeenCalledWith({
      v: 1,
      excludedUserIds: [UUID_A, UUID_B],
    });
    expect(pushedBody).toEqual({
      encrypted_data: 'encrypted-document',
      expected_version: 3,
      excluded_user_ids: [UUID_A, UUID_B],
    });
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_A, UUID_B],
      appliedVersion: 4,
      saving: false,
      conflict: false,
      error: null,
    });
  });

  it('preserves applied state when a success response is malformed', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    server.use(http.put(ENDPOINT, () => HttpResponse.json({ version: 0, extra: true })));

    await presenceOverrideSyncService.save([UUID_B]);

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_A],
      appliedVersion: 3,
      saving: false,
      error: 'Failed to save presence exceptions',
    });
  });

  it('refetches authoritative state on 409 and requires explicit retry', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_C],
    });
    let putCount = 0;
    server.use(
      http.put(ENDPOINT, () => {
        putCount += 1;
        return HttpResponse.json(
          { code: 'presence_override_version_conflict', current_version: 4 },
          { status: 409 }
        );
      }),
      http.get(ENDPOINT, () => HttpResponse.json(remotePreference(4)))
    );

    await presenceOverrideSyncService.save([UUID_B]);

    expect(putCount).toBe(1);
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_C],
      appliedVersion: 4,
      saving: false,
      conflict: true,
      error: null,
    });
  });
});

describe('presenceOverrideSyncService event ordering', () => {
  it('invalidates an older fetch so its late response cannot overwrite a successful save', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_C],
    });
    const fetchStarted = deferred<void>();
    const releaseFetch = deferred<void>();
    const putCompleted = deferred<void>();
    let fetchSignal: AbortSignal | undefined;
    server.use(
      http.get(ENDPOINT, async ({ request }) => {
        fetchSignal = request.signal;
        fetchStarted.resolve();
        await releaseFetch.promise;
        return HttpResponse.json(remotePreference(3));
      }),
      http.put(ENDPOINT, () => {
        putCompleted.resolve();
        return HttpResponse.json({ version: 4 });
      })
    );

    const fetch = presenceOverrideSyncService.fetchAndApply();
    await fetchStarted.promise;
    const save = presenceOverrideSyncService.save([UUID_B]);
    await putCompleted.promise;
    await save;
    const fetchWasAborted = fetchSignal?.aborted;

    releaseFetch.resolve();
    await fetch;

    expect(fetchWasAborted).toBe(true);
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_B],
      appliedVersion: 4,
      loading: false,
      saving: false,
      error: null,
    });
  });

  it('skips an ordinary fetch while a save is pending', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_C],
    });
    const putStarted = deferred<void>();
    const releasePut = deferred<void>();
    let getCount = 0;
    server.use(
      http.put(ENDPOINT, async () => {
        putStarted.resolve();
        await releasePut.promise;
        return HttpResponse.json({ version: 4 });
      }),
      http.get(ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(remotePreference(5));
      })
    );

    const save = presenceOverrideSyncService.save([UUID_B]);
    await putStarted.promise;
    await presenceOverrideSyncService.fetchAndApply();
    const getCountDuringSave = getCount;

    releasePut.resolve();
    await save;

    expect(getCountDuringSave).toBe(0);
    expect(getCount).toBe(0);
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_B],
      appliedVersion: 4,
      loading: false,
      saving: false,
      error: null,
    });
  });

  it('defers a pending save echo and installs the response version before ignoring it', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    const requestStarted = deferred<void>();
    const releaseResponse = deferred<void>();
    let getCount = 0;
    server.use(
      http.put(ENDPOINT, async () => {
        requestStarted.resolve();
        await releaseResponse.promise;
        return HttpResponse.json({ version: 4 });
      }),
      http.get(ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(remotePreference(4));
      })
    );

    const save = presenceOverrideSyncService.save([UUID_B]);
    await requestStarted.promise;
    await presenceOverrideSyncService.handleRemoteUpdate(4);
    expect(getCount).toBe(0);

    releaseResponse.resolve();
    await save;

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_B],
      appliedVersion: 4,
      saving: false,
    });
    expect(getCount).toBe(0);
  });

  it('refetches a deferred version that differs from the installed save response', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_C],
    });
    const requestStarted = deferred<void>();
    const releaseResponse = deferred<void>();
    let getCount = 0;
    server.use(
      http.put(ENDPOINT, async () => {
        requestStarted.resolve();
        await releaseResponse.promise;
        return HttpResponse.json({ version: 4 });
      }),
      http.get(ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(remotePreference(5));
      })
    );

    const save = presenceOverrideSyncService.save([UUID_B]);
    await requestStarted.promise;
    await presenceOverrideSyncService.handleRemoteUpdate(5);
    releaseResponse.resolve();
    await save;

    expect(getCount).toBe(1);
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_C],
      appliedVersion: 5,
      saving: false,
    });
  });

  it('ignores an equal applied version and refetches a different version', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 4);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_B],
    });
    let getCount = 0;
    server.use(
      http.get(ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(remotePreference(5));
      })
    );

    await presenceOverrideSyncService.handleRemoteUpdate(4);
    expect(getCount).toBe(0);

    await presenceOverrideSyncService.handleRemoteUpdate(5);
    expect(getCount).toBe(1);
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_B],
      appliedVersion: 5,
    });
  });
});

describe('presenceOverrideSyncService.reset', () => {
  it('aborts and invalidates an in-flight fetch so late data cannot apply', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 2);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({
      v: 1,
      excludedUserIds: [UUID_B],
    });
    const requestStarted = deferred<void>();
    const releaseResponse = deferred<void>();
    let requestSignal: AbortSignal | undefined;
    server.use(
      http.get(ENDPOINT, async ({ request }) => {
        requestSignal = request.signal;
        requestStarted.resolve();
        await releaseResponse.promise;
        return HttpResponse.json(remotePreference(3));
      })
    );

    const fetch = presenceOverrideSyncService.fetchAndApply();
    await requestStarted.promise;
    presenceOverrideSyncService.reset();
    expect(requestSignal?.aborted).toBe(true);
    releaseResponse.resolve();
    await expect(fetch).resolves.toBe(false);

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [],
      appliedVersion: 0,
      loading: false,
      error: null,
    });
  });

  it('invalidates a save that resolves encryption after reset', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 2);
    const encrypted = deferred<string>();
    vi.mocked(e2eeService.encryptPreferences).mockReturnValueOnce(encrypted.promise);
    let putCount = 0;
    server.use(
      http.put(ENDPOINT, () => {
        putCount += 1;
        return HttpResponse.json({ version: 3 });
      })
    );

    const save = presenceOverrideSyncService.save([UUID_B]);
    expect(e2eeService.encryptPreferences).toHaveBeenCalledTimes(1);
    presenceOverrideSyncService.reset();
    encrypted.resolve('late-encrypted-document');
    await expect(save).resolves.toBe(false);

    expect(putCount).toBe(0);
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [],
      appliedVersion: 0,
      saving: false,
      error: null,
    });
  });
});
