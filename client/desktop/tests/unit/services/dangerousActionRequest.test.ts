import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { jsonResponse } from '../../helpers/stepUpApi';

// The first send of a dangerous-action host and its re-send (#3456 §3.3, §3.4).
// Only `apiFetch` is replaced; the adapter and the context fence are real.
//
// "Mutant:" comments name the production change each case exists to turn red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import {
  FIRST_SEND_SESSION_CHANGED,
  resendWithCode,
  resender,
  sendFirst,
  type FrozenRequest,
} from '@/renderer/services/system/dangerousActionRequest';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { updateServer } from '@/renderer/services/system/serverUpdateApi';
import { useAuthStore } from '@/renderer/stores/auth/authStore';

const REQUEST: FrozenRequest = { path: '/api/v1/channels/channel-1', method: 'DELETE' };
const MFA_REQUIRED_BODY = {
  error: 'MFA verification required',
  mfa_required: true,
  methods: ['totp'],
};
const FAILED = 'Failed to delete channel';
const SERVER_ID = '11111111-1111-4111-8111-111111111111';
const SAVE_BODY = { name: 'Renamed' };

function switchAccount(): void {
  useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
}

/** A route answer that lands after an account switch, as a slow response would. */
function answerAfterSwitch(status: number, body: unknown) {
  return async () => {
    switchAccount();
    return jsonResponse(status, body);
  };
}

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset();
});

describe('sendFirst', () => {
  // Mutant: the first send through bare `apiFetch`, admitted against nothing captured.
  it('sends under a capture of the account it started as, and hands that capture on', async () => {
    const generation = useAuthStore.getState().authGeneration;
    mockApiFetch.mockResolvedValueOnce(jsonResponse(403, MFA_REQUIRED_BODY));

    const first = await sendFirst(REQUEST, FAILED);

    expect(first.kind).toBe('stepUp');
    const [, , options] = mockApiFetch.mock.calls[0];
    expect(options.context.authLifecycle.authGeneration).toBe(generation);
    if (first.kind === 'stepUp') expect(first.context).toBe(options.context);
  });

  // Mutant: the stale check dropped, so account A's refusal opens a dialog under account B.
  it('opens nothing for a verification refusal that lands after an account switch', async () => {
    mockApiFetch.mockImplementationOnce(answerAfterSwitch(403, MFA_REQUIRED_BODY));

    await expect(sendFirst(REQUEST, FAILED)).rejects.toThrow(FIRST_SEND_SESSION_CHANGED);
  });

  // Mutant: the stale check moved above the adapter, so an ordinary refusal reads as a dead session.
  it("words any other refusal with the server's sentence, whenever it lands", async () => {
    mockApiFetch.mockImplementationOnce(answerAfterSwitch(403, { error: 'Missing permission' }));

    await expect(sendFirst(REQUEST, FAILED)).rejects.toThrow('Missing permission');
  });

  it('returns the response on success', async () => {
    mockApiFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));

    const first = await sendFirst(REQUEST, FAILED);

    expect(first.kind).toBe('ok');
  });
});

// A host's own last-moment check (the moderation pin claim, #3458) may refuse a send.
describe('a dispatch that refuses', () => {
  const UNSENT = 'Nothing was sent.';
  const refuse = vi.fn(async () => null);

  // Mutant: a null dispatch read as a response, or worded with the generic failure.
  it('makes the first send throw its sentence, and nothing reaches apiFetch', async () => {
    await expect(
      sendFirst(REQUEST, FAILED, { dispatch: refuse, unsentMessage: UNSENT })
    ).rejects.toThrow(UNSENT);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  // Mutant: a refused re-send reported as aborted (the dialog would say "try again"
  // for a reason it cannot fix) or as ok.
  it('makes the re-send a status-less refusal carrying its sentence', async () => {
    const result = await resendWithCode(
      REQUEST,
      '314159',
      captureApiRequestContext(),
      refuse,
      UNSENT
    );

    expect(result).toEqual({ kind: 'refused', status: 0, body: { error: UNSENT } });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });
});

describe('resender', () => {
  it('sends nothing before a request is frozen', async () => {
    const result = await resender(null)('314159', undefined as never);

    expect(result).toEqual({ kind: 'aborted' });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });
});

describe('updateServer (Server Settings save, first send)', () => {
  // Mutant: the save sent with bare `apiFetch`.
  it('sends under a capture and hands that capture on with the refusal', async () => {
    mockApiFetch.mockResolvedValueOnce(jsonResponse(403, MFA_REQUIRED_BODY));

    const result = await updateServer(SERVER_ID, SAVE_BODY);

    expect(result.kind).toBe('stepUp');
    const [, , options] = mockApiFetch.mock.calls[0];
    if (result.kind === 'stepUp') expect(result.context).toBe(options.context);
  });

  // Mutant: the stale check dropped from `updateServer`.
  it('opens nothing for a refusal that lands after an account switch', async () => {
    mockApiFetch.mockImplementationOnce(answerAfterSwitch(403, MFA_REQUIRED_BODY));

    const result = await updateServer(SERVER_ID, SAVE_BODY);

    expect(result).toEqual({ kind: 'failed', message: FIRST_SEND_SESSION_CHANGED });
  });
});
