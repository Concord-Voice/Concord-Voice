import { renderHook, act, waitFor } from '@testing-library/react';
import { beforeAll, afterAll, afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import type { ChatContext } from '@/renderer/types/chat';
import { FIXTURE_PW, MINT_PATH, MINTED_TOKEN } from '../../helpers/stepUpTokenWire';

// Codex on #3509 (P1), the delete flow: the confirmed delete must not go out
// as an account that signed in while the password was being exchanged. The
// real apiFetch runs here, against msw, because the guard is in it.

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

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const ctx: ChatContext = { type: 'channel', id: 'channel-1', serverId: 'server-1' };

describe('useChatController password delete across an account switch', () => {
  beforeEach(() => {
    resetAllStores();
    useUserStore.setState({ user: mockUser });
  });

  it.each([
    ['another account signed in', true, 1],
    ['control: the same account', false, 2],
  ])('%s', async (_name, switchAccount, deletes) => {
    let calls = 0;
    let minted = false;
    server.use(
      http.delete('*/api/v1/messages/:id', () => {
        calls += 1;
        if (calls === 1) {
          return HttpResponse.json(
            {
              error: 'Current password required to keep deleting messages',
              password_required: true,
              delete_rate_limited: true,
            },
            { status: 403, headers: { 'Retry-After': '30' } }
          );
        }
        return new HttpResponse(null, { status: 204 });
      }),
      http.post(`*${MINT_PATH}`, () => {
        if (switchAccount) {
          useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
        }
        minted = true;
        return HttpResponse.json({ step_up_token: MINTED_TOKEN, expires_in: 60 });
      })
    );
    useChatStore.getState().addMessage(ctx.id, { ...mockMessage, id: 'm1', channel_id: ctx.id });
    const hook = renderHook(() => useChatController(ctx));
    await act(async () => {
      await hook.result.current.deleteMessage('m1');
    });
    await waitFor(() => expect(hook.result.current.deleteRefusal?.view.view).toBe('password'));

    await act(async () => {
      hook.result.current.confirmDelete({ currentPassword: FIXTURE_PW });
    });
    // An aborted delete reports nothing (nothing was sent, and the account
    // switch resets the stores), so wait for the mint and let the flow settle.
    await waitFor(() => expect(minted).toBe(true));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 100));
    });

    expect(calls).toBe(deletes);
  });
});
