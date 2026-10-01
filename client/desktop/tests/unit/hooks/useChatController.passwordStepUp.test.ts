import { renderHook, act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import type { ChatContext } from '@/renderer/types/chat';
import {
  FIXTURE_PW,
  MINTED_TOKEN,
  MINT_PATH,
  parseBody,
  routeVerdict,
  type Wire,
} from '../../helpers/stepUpTokenWire';

// Reproduction for #3509 (Codex security P1), desktop half. Design spec
// "Developer decisions, 2026-10-01", T-2, T-4, T-5: the account password goes
// only to POST /api/v1/auth/step-up/password, which mints a single-use token,
// and the delete route is retried with { step_up_token }. A delete route never
// sees current_password.
//
// Oracle: after a 403 password_required refusal, no request to a message-delete
// route ever carries current_password; the password goes only to the mint
// endpoint, and the route is retried with step_up_token.
//
// The repro is expected to FAIL on the current tree: confirmDelete resends the
// entered password to the delete route. It drives the hook's existing
// confirmDelete({ currentPassword }) and asserts on the mocked apiFetch calls
// only, so it names no client function that does not exist yet. The mint call
// is assumed to go through apiFetch like every other authenticated request.

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({ getState: () => 'connected' }),
  ConnectionState: { CONNECTED: 'connected', DISCONNECTED: 'disconnected' },
}));

vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: () => ({ sendMessage: vi.fn(), sendDMMessage: vi.fn() }),
}));

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: { isInitialized: true },
}));

vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: vi.fn(),
  unpinMessage: vi.fn(),
}));

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/apiClient')>();
  return { ...actual, apiFetch: (...args: unknown[]) => mockApiFetch(...args) };
});

const FIXTURE_OTP = '123456';

const PASSWORD_CHALLENGE = {
  error: 'Password required',
  delete_rate_limited: true,
  password_required: true,
};

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'Retry-After': '30' },
  });
}

/**
 * Stands in for the post-fix server, recording every request:
 *  - the mint endpoint answers a token;
 *  - a delete carrying step_up_token succeeds;
 *  - a delete carrying current_password is a 400 (T-4, a stale client fails loudly);
 *  - any other delete is the password_required refusal.
 */
function installServer(): Wire[] {
  const wire: Wire[] = [];
  mockApiFetch.mockImplementation((path: string, init?: RequestInit) => {
    const body = parseBody(init?.body as string | undefined);
    wire.push({ path, method: init?.method ?? 'GET', body });
    if (path === MINT_PATH) {
      return Promise.resolve(json(200, { step_up_token: MINTED_TOKEN, expires_in: 60 }));
    }
    const verdict = routeVerdict(body);
    if (verdict === 'token') return Promise.resolve(json(200, {}));
    if (verdict === 'stale-client') {
      return Promise.resolve(json(400, { error: 'Invalid request body' }));
    }
    return Promise.resolve(json(403, PASSWORD_CHALLENGE));
  });
  return wire;
}

const routes: Array<[string, ChatContext, string, string, string]> = [
  [
    'channel message delete',
    { type: 'channel', id: 'channel-1', serverId: 'server-1' },
    'm1',
    '/api/v1/messages/m1',
    'messages.delete',
  ],
  [
    'DM message delete',
    { type: 'dm', id: 'conv-1' },
    'dm-m1',
    '/api/v1/dm/conversations/conv-1/messages/dm-m1',
    'dm.message_delete',
  ],
];

describe('useChatController password step-up (#3509)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    resetAllStores();
    useUserStore.setState({ user: mockUser });
  });

  async function refuseThenConfirm(
    ctx: ChatContext,
    id: string,
    step: { currentPassword?: string; mfaCode?: string }
  ) {
    useChatStore.getState().addMessage(ctx.id, { ...mockMessage, id, channel_id: ctx.id });
    const wire = installServer();
    const hook = renderHook(() => useChatController(ctx));
    await act(async () => {
      await hook.result.current.deleteMessage(id);
    });
    await waitFor(() => expect(hook.result.current.deleteRefusal?.view.view).toBe('password'));
    await act(async () => {
      hook.result.current.confirmDelete(step);
    });
    return wire;
  }

  // regression for #3509 (Codex P1)
  it.each(routes)(
    '%s: the password goes only to the mint endpoint and the route is retried with step_up_token',
    async (_name, ctx, id, deletePath, purpose) => {
      const wire = await refuseThenConfirm(ctx, id, { currentPassword: FIXTURE_PW });

      const deletes = () => wire.filter((r) => r.method === 'DELETE');
      // Precondition, and the arm being reached: the first delete was refused
      // with password_required (the view above), and the user's confirmation
      // produced a second delete to the same route.
      await waitFor(() => expect(deletes().length).toBeGreaterThanOrEqual(2));
      expect(deletes().every((r) => r.path === deletePath)).toBe(true);

      expect
        .soft(
          deletes().filter((r) => 'current_password' in r.body),
          'no request to a delete route may carry current_password'
        )
        .toEqual([]);
      const mints = wire.filter((r) => r.path === MINT_PATH);
      expect.soft(mints, 'the password must be sent once, to the mint endpoint').toHaveLength(1);
      expect.soft(mints[0]?.method).toBe('POST');
      expect.soft(mints[0]?.body).toEqual({ current_password: FIXTURE_PW, purpose });
      expect.soft(deletes().at(-1)?.body, 'the route is retried with the minted token').toEqual({
        step_up_token: MINTED_TOKEN,
      });
    }
  );

  // Control, valid before and after: an MFA code is not a password. The retry
  // carries mfa_code only, with no mint call and no current_password.
  it('an MFA-code retry sends mfa_code to the route and never calls the mint endpoint', async () => {
    const [, ctx, id, deletePath] = routes[0];
    const wire = await refuseThenConfirm(ctx, id, { mfaCode: FIXTURE_OTP });

    await waitFor(() =>
      expect(wire.filter((r) => r.method === 'DELETE').length).toBeGreaterThanOrEqual(2)
    );
    expect(wire.filter((r) => r.path === MINT_PATH)).toEqual([]);
    expect(wire.at(-1)).toEqual({
      path: deletePath,
      method: 'DELETE',
      body: { mfa_code: FIXTURE_OTP },
    });
  });
});
