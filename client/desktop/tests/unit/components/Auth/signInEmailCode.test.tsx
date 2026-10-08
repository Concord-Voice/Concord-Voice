import { renderHook, waitFor, act } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import {
  captureRuntimeServerSelection,
  resetRuntimeServerBase,
  type RuntimeServerSelection,
} from '@/renderer/services/system/runtimeServerBase';
import { CHALLENGE_TTL_MS } from '@/renderer/services/system/challengeIssuer';
import {
  __resetSignInEmailCodeForTests,
  __signInEmailCodeSendCountForTests,
  useSignInEmailCode,
} from '@/renderer/components/Auth/signInEmailCode';

const SEND_PATH = '/api/v1/auth/mfa/email/send';

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
  vi.useRealTimers();
});
afterAll(() => server.close());

beforeEach(() => {
  // Only Date: MSW and waitFor keep their real timers.
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-10-07T12:00:00Z'));
  __resetSignInEmailCodeForTests();
});

function recordSends(): string[] {
  const tokens: string[] = [];
  server.use(
    http.post(`*${SEND_PATH}`, async ({ request }) => {
      const body = (await request.json()) as { mfa_challenge_token: string };
      tokens.push(body.mfa_challenge_token);
      return HttpResponse.json({ message: 'Verification code sent to your email' });
    })
  );
  return tokens;
}

// One mounted surface on the email panel for `token`, as the login page or
// the modal mounts it.
function mountEmailPanel(token: string, selection: RuntimeServerSelection) {
  return renderHook(() =>
    useSignInEmailCode({
      challengeToken: token,
      serverSelection: selection,
      mode: 'email-sms',
      methods: ['email'],
      originChangedError: 'moved',
    })
  );
}

// Let any request a mount queued reach the handler before counting.
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 20));
  });
}

function advance(ms: number) {
  vi.setSystemTime(Date.now() + ms);
}

// The process-wide send record lasts as long as the server keeps the
// challenge, and no longer.
describe('the sign-in email code send record', () => {
  it('a remount just inside the TTL does not ask again (control)', async () => {
    const sends = recordSends();
    const selection = captureRuntimeServerSelection();
    mountEmailPanel('send-live', selection).unmount();
    await waitFor(() => expect(sends).toHaveLength(1));

    advance(CHALLENGE_TTL_MS - 1);
    mountEmailPanel('send-live', selection);
    await settle();
    expect(sends).toEqual(['send-live']);
  });

  it('a remount after the TTL finds no record and asks again', async () => {
    const sends = recordSends();
    const selection = captureRuntimeServerSelection();
    mountEmailPanel('send-expired', selection).unmount();
    await waitFor(() => expect(sends).toHaveLength(1));

    advance(CHALLENGE_TTL_MS);
    mountEmailPanel('send-expired', selection);
    await waitFor(() => expect(sends).toHaveLength(2));
    expect(sends).toEqual(['send-expired', 'send-expired']);
  });

  it('prunes expired records when the next send is made', async () => {
    const sends = recordSends();
    const selection = captureRuntimeServerSelection();
    mountEmailPanel('send-old-1', selection).unmount();
    mountEmailPanel('send-old-2', selection).unmount();
    await waitFor(() => expect(sends).toHaveLength(2));

    advance(CHALLENGE_TTL_MS);
    mountEmailPanel('send-new', selection);
    await waitFor(() => expect(sends).toHaveLength(3));
    expect(__signInEmailCodeSendCountForTests()).toBe(1);
  });

  it('keeps live records when the next send is made (control)', async () => {
    const sends = recordSends();
    const selection = captureRuntimeServerSelection();
    mountEmailPanel('send-kept-1', selection).unmount();
    mountEmailPanel('send-kept-2', selection).unmount();
    await waitFor(() => expect(sends).toHaveLength(2));

    advance(CHALLENGE_TTL_MS - 1);
    mountEmailPanel('send-kept-new', selection);
    await waitFor(() => expect(sends).toHaveLength(3));
    expect(__signInEmailCodeSendCountForTests()).toBe(3);
  });
});
