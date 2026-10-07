import { beforeEach, describe, expect, it, vi } from 'vitest';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { resetAllStores } from '../../helpers/store-helpers';

// The step-up run's request context reaching the privacy PATCH, and the
// pre-dispatch refusal that comes back. `apiFetch` is mocked at the module
// boundary because the observables are its arguments and its rejection.
//
// "Mutant:" comments name the production change each test exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

const PRIVACY_PATH = '/api/v1/users/me/privacy';
const CREDENTIALS = { currentPassword: 'fixture-password', mfaCode: '123456' }; // pragma: allowlist secret

const stored = {
  messages_friends_only: true,
  messages_server_members: true,
  dm_privacy_level: 2,
  dm_friends_of_friends: false,
  auto_accept_friend_codes: false,
  searchable_by_username: false,
  searchable_by_email: false,
  searchable_by_phone: false,
  allow_embedded_content: false,
  load_gifs_automatically: true,
  share_personalization_with_gif_provider: true,
  require_auth_before_purge: false,
};

const accepted = () =>
  new Response(JSON.stringify({ privacy: stored }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });

const abortError = () => new DOMException('The request was aborted.', 'AbortError');

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  mockApiFetch.mockReset();
});

describe('privacyStore.disablePurgeFence — the run context', () => {
  // Mutant: `undefined` passed instead of the context, so the PATCH is admitted
  // for whatever account and server are current when it is sent.
  it('forwards the very context it was given to apiFetch', async () => {
    mockApiFetch.mockResolvedValueOnce(accepted());
    const context = captureApiRequestContext();

    const result = await usePrivacyStore.getState().disablePurgeFence(CREDENTIALS, context);

    expect(result).toEqual({ kind: 'accepted' });
    const [path, init, options] = mockApiFetch.mock.calls[0];
    expect(path).toBe(PRIVACY_PATH);
    expect(init.method).toBe('PATCH');
    expect(options.context).toBe(context);
  });

  it('sends as its own operation, with no options, when given no context', async () => {
    mockApiFetch.mockResolvedValueOnce(accepted());

    await usePrivacyStore.getState().disablePurgeFence(CREDENTIALS);

    expect(mockApiFetch.mock.calls[0]).toHaveLength(2);
  });

  it('forwards the context from updatePrivacy too', async () => {
    mockApiFetch.mockResolvedValueOnce(accepted());
    const context = captureApiRequestContext();

    await usePrivacyStore
      .getState()
      .updatePrivacy({ requireAuthBeforePurge: false }, CREDENTIALS, context);

    expect(mockApiFetch.mock.calls[0][2].context).toBe(context);
  });
});

describe('privacyStore.disablePurgeFence — the pre-dispatch fence', () => {
  // Mutant: the `isAbortError` arm dropped, so a request that was never sent
  // is reported as a transport refusal and the user is told the change failed.
  it('answers aborted when apiFetch refuses before dispatch', async () => {
    mockApiFetch.mockRejectedValueOnce(abortError());

    const result = await usePrivacyStore
      .getState()
      .disablePurgeFence(CREDENTIALS, captureApiRequestContext());

    expect(result).toEqual({ kind: 'aborted' });
  });

  it('still reports any other transport failure as a refusal', async () => {
    mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));

    const result = await usePrivacyStore.getState().disablePurgeFence(CREDENTIALS);

    expect(result.kind).toBe('refused');
  });

  it('leaves the stored setting untouched when the request was never sent', async () => {
    mockApiFetch.mockRejectedValueOnce(abortError());
    const before = usePrivacyStore.getState().settings.requireAuthBeforePurge;

    await usePrivacyStore.getState().disablePurgeFence(CREDENTIALS, captureApiRequestContext());

    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(before);
  });
});
