import { renderHook, act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import type { ChatContext } from '@/renderer/types/chat';
import { FIXTURE_PW, MINT_PATH } from '../../helpers/stepUpTokenWire';

// The delete flow's handling of the password step-up exchange (#3509): what a
// mint refusal and a refused token do to the refusal slot, and the
// stale-context guard across the mint round trip.

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({ getState: () => 'connected' }),
  ConnectionState: { CONNECTED: 'connected', DISCONNECTED: 'disconnected' },
}));
vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: () => ({ sendMessage: vi.fn(), sendDMMessage: vi.fn() }),
}));
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({ e2eeService: { isInitialized: true } }));
vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: vi.fn(),
  unpinMessage: vi.fn(),
}));

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/apiClient')>();
  return { ...actual, apiFetch: (...args: unknown[]) => mockApiFetch(...args) };
});

const channelCtx: ChatContext = { type: 'channel', id: 'channel-1', serverId: 'server-1' };
const otherCtx: ChatContext = { type: 'channel', id: 'channel-2', serverId: 'server-1' };

const PASSWORD_CHALLENGE = {
  error: 'Current password required to keep deleting messages',
  delete_rate_limited: true,
  password_required: true,
};

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'Retry-After': '30' },
  });
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

async function refused(ctx: ChatContext = channelCtx) {
  useChatStore.getState().addMessage(ctx.id, { ...mockMessage, id: 'm1', channel_id: ctx.id });
  mockApiFetch.mockResolvedValueOnce(json(403, PASSWORD_CHALLENGE));
  const hook = renderHook(({ c }) => useChatController(c), { initialProps: { c: ctx } });
  await act(async () => {
    await hook.result.current.deleteMessage('m1');
  });
  await waitFor(() => expect(hook.result.current.deleteRefusal?.view.view).toBe('password'));
  return hook;
}

describe('useChatController password step-up token (#3509)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    resetAllStores();
    useUserStore.setState({ user: mockUser });
  });

  it('a mint refusal lands on the password field and the route is not retried', async () => {
    const hook = await refused();
    mockApiFetch.mockResolvedValueOnce(json(403, { error: 'Invalid password' }));

    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });

    await waitFor(() =>
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'password',
        error: 'That password is not correct.',
      })
    );
    expect(hook.result.current.deleteRefusal?.submitting).toBe(false);
    expect(mockApiFetch.mock.calls.map((c) => c[0])).toEqual(['/api/v1/messages/m1', MINT_PATH]);
  });

  it('the account lockout lands on the password field', async () => {
    const hook = await refused();
    mockApiFetch.mockResolvedValueOnce(json(423, { error_code: 'account_locked' }));

    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });

    await waitFor(() =>
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'password',
        error: 'Too many attempts. Try again in 30 seconds.',
      })
    );
  });

  it('a refused token re-prompts with the expiry copy and remounts the field', async () => {
    const hook = await refused();
    const before = hook.result.current.deleteRefusal?.promptKey ?? 0;
    mockApiFetch.mockResolvedValueOnce(json(200, { step_up_token: 'stale', expires_in: 60 }));
    mockApiFetch.mockResolvedValueOnce(
      json(403, {
        ...PASSWORD_CHALLENGE,
        error: 'Your confirmation expired. Enter your password again.',
        step_up_token_invalid: true,
      })
    );

    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });

    await waitFor(() =>
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'password',
        error: 'Your confirmation expired. Enter your password again.',
      })
    );
    expect(hook.result.current.deleteRefusal?.promptKey).toBe(before + 1);
  });

  it('a mint refusal that lands after the chat changed is discarded', async () => {
    const hook = await refused();
    const mint = deferred<Response>();
    mockApiFetch.mockReturnValueOnce(mint.promise);

    act(() => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });
    hook.rerender({ c: otherCtx });
    await act(async () => {
      mint.resolve(json(403, { error: 'Invalid password' }));
      await mint.promise;
    });

    expect(hook.result.current.deleteRefusal).toBeNull();
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});
