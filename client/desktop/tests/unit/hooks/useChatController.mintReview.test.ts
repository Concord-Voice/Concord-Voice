import { renderHook, act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import type { ChatContext } from '@/renderer/types/chat';
import { FIXTURE_PW } from '../../helpers/stepUpTokenWire';

// Reproductions from the #3509 frontend review for the delete flow:
// M1 — a mint refusal with mfa_required (the account enrolled MFA after the
//      prompt opened) must move to the confirm view with the mint's methods,
//      not leave a password prompt that can never pass;
// M2 — a 404 from the mint (a server older than the step-up endpoint) gets
//      copy saying the server does not support this yet, not "try again";
// L3 — the previous error is cleared when a retry starts, so a repeated
//      identical refusal is announced again.

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

const ctx: ChatContext = { type: 'channel', id: 'channel-1', serverId: 'server-1' };

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'Retry-After': '30' },
  });
}

async function refusedWithPassword() {
  useChatStore.getState().addMessage(ctx.id, { ...mockMessage, id: 'm1', channel_id: ctx.id });
  mockApiFetch.mockResolvedValueOnce(
    json(403, {
      error: 'Current password required to keep deleting messages',
      password_required: true,
      delete_rate_limited: true,
    })
  );
  const hook = renderHook(() => useChatController(ctx));
  await act(async () => {
    await hook.result.current.deleteMessage('m1');
  });
  await waitFor(() => expect(hook.result.current.deleteRefusal?.view.view).toBe('password'));
  return hook;
}

describe('useChatController mint refusals (#3509 frontend review)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    resetAllStores();
    useUserStore.setState({ user: mockUser });
  });

  it('M1: an mfa_required mint refusal moves to the confirm view with its methods', async () => {
    const hook = await refusedWithPassword();
    mockApiFetch.mockResolvedValueOnce(
      json(403, {
        error: 'MFA verification required',
        mfa_required: true,
        mfa_methods: ['webauthn'],
      })
    );

    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });

    await waitFor(() =>
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'confirm',
        methods: ['webauthn'],
      })
    );
  });

  it('M2: a mint the server does not have is named as unsupported, not retryable', async () => {
    const hook = await refusedWithPassword();
    mockApiFetch.mockResolvedValueOnce(json(404, { error: 'Not Found' }));

    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });

    await waitFor(() =>
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'password',
        error: "This server doesn't support this confirmation yet.",
      })
    );
  });

  it('L3: a retry clears the previous error while it is in flight', async () => {
    const hook = await refusedWithPassword();
    mockApiFetch.mockResolvedValueOnce(json(403, { error: 'Invalid password' }));
    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });
    await waitFor(() => expect(hook.result.current.deleteRefusal?.view).toHaveProperty('error'));

    let release!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(new Promise<Response>((r) => (release = r)));
    act(() => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });

    expect(hook.result.current.deleteRefusal?.view).toEqual({ view: 'password' });
    await act(async () => {
      release(json(403, { error: 'Invalid password' }));
    });
    await waitFor(() =>
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'password',
        error: 'That password is not correct.',
      })
    );
  });
});
