import { renderHook, act, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useChatController } from '@/renderer/hooks/messaging/useChatController';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockUser, mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { deferred } from '../../helpers/deferred';
import { confirmAndSettle, confirmInFlight } from '../../helpers/confirmDelete';
import type { ChatContext } from '@/renderer/types/chat';

// #3455 T7: the delete-rate soft-lock refusal slot in useChatController. The
// hook owns one refusal slot, an in-flight dedupe, and the retry request; the
// wire reading itself is pinned in deleteRefusal.test.ts.

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

// The real safeJson, so a non-JSON body takes the path a proxy page would.
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/apiClient')>();
  return { ...actual, apiFetch: (...args: unknown[]) => mockApiFetch(...args) };
});

const channelCtx: ChatContext = { type: 'channel', id: 'channel-1', serverId: 'server-1' };
const otherChannelCtx: ChatContext = { type: 'channel', id: 'channel-2', serverId: 'server-1' };
const dmCtx: ChatContext = { type: 'dm', id: 'conv-1' };

const CHALLENGE = {
  error: 'Confirm it is you',
  delete_rate_limited: true,
  mfa_required: true,
  methods: ['totp'],
};
const PASSWORD_CHALLENGE = {
  error: 'Password required',
  delete_rate_limited: true,
  password_required: true,
};

// Bound to constants: detect-secrets flags keyword/literal adjacency, not the
// value (same convention as purgeApi.test.ts).
const FIXTURE_OTP = '123456';
const FIXTURE_PW = 'hunter2-fixture';

function res(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

function htmlRes(status: number): Response {
  return new Response('<html>Bad gateway</html>', {
    status,
    headers: { 'Content-Type': 'text/html' },
  });
}

function seed(channelId: string, ...ids: string[]) {
  for (const id of ids) {
    useChatStore.getState().addMessage(channelId, { ...mockMessage, id, channel_id: channelId });
  }
}

function storedIds(channelId: string): string[] {
  return (useChatStore.getState().messagesByChannel.get(channelId) ?? []).map((m) => m.id);
}

function lastInit(): RequestInit {
  return mockApiFetch.mock.calls.at(-1)?.[1] as RequestInit;
}

describe('useChatController delete refusal (#3455)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    resetAllStores();
    useUserStore.setState({ user: mockUser });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  async function refuseFirst(ctx: ChatContext = channelCtx, id = 'm1', body: object = CHALLENGE) {
    seed(ctx.id, id);
    mockApiFetch.mockResolvedValueOnce(res(403, body));
    const hook = renderHook(({ c }) => useChatController(c), { initialProps: { c: ctx } });
    await act(async () => {
      await hook.result.current.deleteMessage(id);
    });
    return hook;
  }

  describe('the request', () => {
    it('sends no body and no content type on the first attempt', async () => {
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(mockApiFetch).toHaveBeenCalledTimes(1);
      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/messages/m1', { method: 'DELETE' });
    });

    it('a retry with a code sends only mfa_code', async () => {
      const { result } = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));

      expect(lastInit().method).toBe('DELETE');
      expect(lastInit().headers).toEqual({ 'Content-Type': 'application/json' });
      expect(JSON.parse(lastInit().body as string)).toEqual({ mfa_code: FIXTURE_OTP });
    });

    // Rewritten for #3509: the password goes only to the mint endpoint, and
    // the route is retried with the token it returned, never the password.
    it('a retry with a password mints a token and sends only step_up_token', async () => {
      const { result } = await refuseFirst(channelCtx, 'm1', PASSWORD_CHALLENGE);
      mockApiFetch.mockResolvedValueOnce(
        res(200, { step_up_token: 'minted-token', expires_in: 60 })
      );
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await confirmAndSettle({ result }, { currentPassword: FIXTURE_PW });
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(3));

      const [mintPath, mintInit] = mockApiFetch.mock.calls[1] as [string, RequestInit];
      expect(mintPath).toBe('/api/v1/auth/step-up/password');
      expect(JSON.parse(mintInit.body as string)).toEqual({
        current_password: FIXTURE_PW,
        purpose: 'messages.delete',
      });
      expect(mockApiFetch.mock.calls[2][0]).toBe('/api/v1/messages/m1');
      expect(JSON.parse(lastInit().body as string)).toEqual({ step_up_token: 'minted-token' });
    });

    it('a retry with an empty factor still sends no body', async () => {
      const { result } = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await confirmAndSettle({ result }, { mfaCode: '', currentPassword: '' });
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));

      expect(lastInit()).toEqual({ method: 'DELETE' });
    });

    it('targets the DM route in a DM, with and without a factor', async () => {
      const { result } = await refuseFirst(dmCtx, 'dm-m1');
      expect(mockApiFetch.mock.calls[0]).toEqual([
        '/api/v1/dm/conversations/conv-1/messages/dm-m1',
        { method: 'DELETE' },
      ]);

      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
      expect(mockApiFetch.mock.calls[1][0]).toBe('/api/v1/dm/conversations/conv-1/messages/dm-m1');
    });

    it('confirmDelete with nothing on screen sends nothing', async () => {
      const { result } = renderHook(() => useChatController(channelCtx));
      const outcome = await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });
      expect(outcome).toEqual({ kind: 'aborted' });
      expect(mockApiFetch).not.toHaveBeenCalled();
      expect(result.current.deleteRefusal).toBeNull();
    });
  });

  describe('the refusal slot', () => {
    it('a soft-lock 403 fills the slot with the confirm view and removes nothing', async () => {
      const { result } = await refuseFirst();

      expect(result.current.deleteRefusal).toMatchObject({
        messageId: 'm1',
        view: { view: 'confirm', methods: ['totp'] },
      });
      expect(result.current.deleteRefusal).not.toHaveProperty('submitting');
      expect(result.current.deleteRefusal).not.toHaveProperty('promptKey');
      expect(storedIds('channel-1')).toEqual(['m1']);
    });

    it('a 429 fills the slot with a wait view carrying Retry-After', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(res(429, { error: 'Rate limit' }, { 'Retry-After': '9' }));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(result.current.deleteRefusal?.view).toEqual({
        view: 'wait',
        reason: 'requests',
        retryAfterSeconds: 9,
      });
    });

    it('a delete below the threshold succeeds silently: row gone, no slot', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(result.current.deleteRefusal).toBeNull();
      expect(storedIds('channel-1')).toEqual([]);
    });

    it('a refusal for another message is discarded while the slot is occupied', async () => {
      seed('channel-1', 'm1', 'm2');
      const first = deferred<Response>();
      const second = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
      const { result } = renderHook(() => useChatController(channelCtx));

      let pending: Promise<void>[] = [];
      act(() => {
        pending = [result.current.deleteMessage('m1'), result.current.deleteMessage('m2')];
      });
      await act(async () => {
        first.resolve(res(403, CHALLENGE));
        await pending[0];
      });
      expect(result.current.deleteRefusal?.messageId).toBe('m1');

      await act(async () => {
        second.resolve(res(403, { ...CHALLENGE, methods: ['webauthn'] }));
        await pending[1];
      });

      expect(result.current.deleteRefusal).toMatchObject({
        messageId: 'm1',
        view: { view: 'confirm', methods: ['totp'] },
      });
    });

    it('a transport failure for another message is discarded the same way', async () => {
      seed('channel-1', 'm1', 'm2');
      const first = deferred<Response>();
      const second = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
      const { result } = renderHook(() => useChatController(channelCtx));

      let pending: Promise<void>[] = [];
      act(() => {
        pending = [result.current.deleteMessage('m1'), result.current.deleteMessage('m2')];
      });
      await act(async () => {
        first.resolve(res(403, CHALLENGE));
        await pending[0];
      });
      await act(async () => {
        second.reject(new TypeError('Failed to fetch'));
        await pending[1];
      });

      expect(result.current.deleteRefusal?.messageId).toBe('m1');
      expect(result.current.deleteRefusal?.view.view).toBe('confirm');
    });

    it('dismissDeleteRefusal empties the slot', async () => {
      const { result } = await refuseFirst();
      act(() => result.current.dismissDeleteRefusal());
      expect(result.current.deleteRefusal).toBeNull();
    });

    it('a retry in flight keeps the slot, then resolves to success and clears it', async () => {
      const { result } = await refuseFirst();
      const retry = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(retry.promise);

      const retryPending = confirmInFlight({ result }, { mfaCode: FIXTURE_OTP });
      expect(result.current.deleteRefusal?.view).toEqual({ view: 'confirm', methods: ['totp'] });

      await act(async () => {
        retry.resolve(res(200, {}));
        await retryPending;
      });
      await expect(retryPending).resolves.toEqual({ kind: 'success' });
      await waitFor(() => expect(result.current.deleteRefusal).toBeNull());
      expect(storedIds('channel-1')).toEqual([]);
    });

    it('a refused retry shows the new refusal and resolves it as answered', async () => {
      const { result } = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(503, { error: 'Soft-lock unavailable' }));
      const outcome = await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });
      await waitFor(() => expect(result.current.deleteRefusal?.view.view).toBe('unavailable'));

      expect(outcome).toEqual({ kind: 'answered' });
      expect(storedIds('channel-1')).toEqual(['m1']);
    });
  });

  describe('in-flight dedupe', () => {
    it('a second delete for an id already in flight is a no-op', async () => {
      seed('channel-1', 'm1');
      const pending = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(pending.promise);
      const { result } = renderHook(() => useChatController(channelCtx));

      let both: Promise<void>[] = [];
      act(() => {
        both = [result.current.deleteMessage('m1'), result.current.deleteMessage('m1')];
      });
      expect(mockApiFetch).toHaveBeenCalledTimes(1);

      await act(async () => {
        pending.resolve(res(200, {}));
        await Promise.all(both);
      });
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    it('the id is released once the request settles, so a later delete goes out', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(res(500, { error: 'boom' }));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });

    it('the id is released after a transport failure too', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });
      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });

    it('dedupes per id: two different messages are both sent', async () => {
      seed('channel-1', 'm1', 'm2');
      mockApiFetch.mockResolvedValue(res(200, {}));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await Promise.all([result.current.deleteMessage('m1'), result.current.deleteMessage('m2')]);
      });
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });
  });

  describe('a 404', () => {
    it('on a RETRY counts as gone: the row is removed and the slot cleared', async () => {
      const { result } = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(404, { error: 'Message not found' }));
      await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });

      await waitFor(() => expect(result.current.deleteRefusal).toBeNull());
      expect(storedIds('channel-1')).toEqual([]);
    });

    it('on the ORIGINAL attempt is a failure: the row stays and the slot reports it', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(res(404, { error: 'Message not found' }));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(storedIds('channel-1')).toEqual(['m1']);
      expect(result.current.deleteRefusal?.view).toMatchObject({
        view: 'failed',
        message: 'Message not found',
      });
    });
  });

  describe('slot lifetime', () => {
    it('clears when the chat context changes', async () => {
      const { result, rerender } = await refuseFirst();
      expect(result.current.deleteRefusal).not.toBeNull();
      // The same id exists in the next channel, so only the context-change
      // reset (not the vanished-message effect) can clear the slot.
      seed('channel-2', 'm1');

      rerender({ c: otherChannelCtx });
      expect(result.current.deleteRefusal).toBeNull();
    });

    it('clears when the refused message is deleted elsewhere', async () => {
      const { result } = await refuseFirst();
      expect(result.current.deleteRefusal).not.toBeNull();

      act(() => useChatStore.getState().deleteMessage('channel-1', 'm1'));
      await waitFor(() => expect(result.current.deleteRefusal).toBeNull());
    });

    it('survives an unrelated message arriving in the same channel', async () => {
      const { result } = await refuseFirst();
      act(() => seed('channel-1', 'm-unrelated'));
      expect(result.current.deleteRefusal?.messageId).toBe('m1');
    });

    it('does not warn or throw when a response lands after unmount', async () => {
      seed('channel-1', 'm1');
      const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
      const pending = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(pending.promise);
      const { result, unmount } = renderHook(() => useChatController(channelCtx));

      let call: Promise<void> = Promise.resolve();
      act(() => {
        call = result.current.deleteMessage('m1');
      });
      unmount();
      pending.resolve(res(403, CHALLENGE));
      await expect(call).resolves.toBeUndefined();

      expect(errors).not.toHaveBeenCalled();
      errors.mockRestore();
    });

    // Codex on #3509: a response that lands after the chat context changed
    // must not fill the slot. Unguarded, it showed the old message's refusal
    // in the new chat, and confirming it retried that message against the new
    // chat. In the ordinary case the vanished-message effect clears it one
    // commit later, because it looks the message up in the CURRENT chat, so
    // the realistic case is checked render by render, and the end-state cases
    // seed the same id in the new chat so that only a context check can drop it.
    describe('a response that lands after the chat context changed', () => {
      const otherDmCtx: ChatContext = { type: 'dm', id: 'conv-2' };

      function deleteThenSwitch(from: ChatContext, to: ChatContext, id = 'm1') {
        seed(from.id, id);
        const pending = deferred<Response>();
        mockApiFetch.mockReturnValueOnce(pending.promise);
        const seen: Array<string | null> = [];
        const hook = renderHook(
          ({ c }) => {
            const r = useChatController(c);
            seen.push(r.deleteRefusal ? `${c.id}:${r.deleteRefusal.messageId}` : null);
            return r;
          },
          { initialProps: { c: from } }
        );
        let call: Promise<void> = Promise.resolve();
        act(() => {
          call = hook.result.current.deleteMessage(id);
        });
        const switchedAt = seen.length;
        hook.rerender({ c: to });
        return {
          ...hook,
          pending,
          call,
          afterSwitch: () => seen.slice(switchedAt).filter(Boolean),
        };
      }

      it('a refusal is discarded and never renders in the new chat', async () => {
        const { result, pending, call, afterSwitch } = deleteThenSwitch(
          channelCtx,
          otherChannelCtx
        );

        await act(async () => {
          pending.resolve(res(403, CHALLENGE));
          await call;
        });

        expect(afterSwitch()).toEqual([]);
        expect(result.current.deleteRefusal).toBeNull();
        expect(storedIds('channel-1')).toEqual(['m1']);
      });

      it('a transport failure is discarded the same way', async () => {
        seed('conv-2', 'm1');
        const { result, pending, call, afterSwitch } = deleteThenSwitch(dmCtx, otherDmCtx);

        await act(async () => {
          pending.reject(new TypeError('Failed to fetch'));
          await call;
        });

        expect(afterSwitch()).toEqual([]);
        expect(result.current.deleteRefusal).toBeNull();
      });

      it('a refused retry is discarded too, so nothing is left to confirm against the new chat', async () => {
        const { result, rerender } = await refuseFirst();
        const retry = deferred<Response>();
        mockApiFetch.mockReturnValueOnce(retry.promise);
        const retryPending = confirmInFlight({ result }, { mfaCode: FIXTURE_OTP });
        seed('channel-2', 'm1');
        rerender({ c: otherChannelCtx });

        await act(async () => {
          retry.resolve(res(403, { ...CHALLENGE, error: 'Invalid code' }));
          await retryPending;
        });
        await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));

        expect(result.current.deleteRefusal).toBeNull();
        await expect(confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP })).resolves.toEqual({
          kind: 'aborted',
        });
        expect(mockApiFetch).toHaveBeenCalledTimes(2);
      });

      it('a success still removes the row from the chat it was sent from', async () => {
        const { result, pending, call } = deleteThenSwitch(channelCtx, otherChannelCtx);

        await act(async () => {
          pending.resolve(res(200, {}));
          await call;
        });

        expect(storedIds('channel-1')).toEqual([]);
        expect(result.current.deleteRefusal).toBeNull();
      });

      it('a refusal that lands after switching back to its own chat is shown there', async () => {
        const { result, rerender, pending, call } = deleteThenSwitch(channelCtx, otherChannelCtx);
        rerender({ c: channelCtx });

        await act(async () => {
          pending.resolve(res(403, CHALLENGE));
          await call;
        });

        expect(result.current.deleteRefusal?.messageId).toBe('m1');
        expect(result.current.deleteRefusal?.view.view).toBe('confirm');
      });
    });

    it('does not warn or throw when a transport failure lands after unmount', async () => {
      seed('channel-1', 'm1');
      const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
      const pending = deferred<Response>();
      mockApiFetch.mockReturnValueOnce(pending.promise);
      const { result, unmount } = renderHook(() => useChatController(channelCtx));

      let call: Promise<void> = Promise.resolve();
      act(() => {
        call = result.current.deleteMessage('m1');
      });
      unmount();
      pending.reject(new TypeError('Failed to fetch'));
      await expect(call).resolves.toBeUndefined();

      expect(errors).not.toHaveBeenCalled();
      errors.mockRestore();
    });
  });

  describe('unreadable and failed responses', () => {
    it('a non-JSON error body is treated as empty: no proxy text reaches the dialog', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(htmlRes(502));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(result.current.deleteRefusal?.view).toEqual({
        view: 'failed',
        message: undefined,
        retryAfterSeconds: undefined,
      });
      expect(JSON.stringify(result.current.deleteRefusal)).not.toContain('Bad gateway');
    });

    it('a non-JSON 503 still maps by status', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(htmlRes(503));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });
      expect(result.current.deleteRefusal?.view).toEqual({ view: 'unavailable' });
    });

    it('a transport failure is reported, not swallowed', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(result.current.deleteRefusal).toMatchObject({
        messageId: 'm1',
        view: { view: 'failed' },
      });
      // Transport detail is never user copy.
      expect(result.current.deleteRefusal?.view).not.toHaveProperty('message');
    });

    it('an AbortError on a RETRY is aborted, never a network error, and keeps the slot', async () => {
      const { result } = await refuseFirst();
      mockApiFetch.mockRejectedValueOnce(new DOMException('aborted', 'AbortError'));

      const outcome = await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });

      expect(outcome).toEqual({ kind: 'aborted' });
      expect(result.current.deleteRefusal?.view).toEqual({ view: 'confirm', methods: ['totp'] });
      expect(storedIds('channel-1')).toEqual(['m1']);
    });

    it('an AbortError (account or server changed under the request) reports nothing', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockRejectedValueOnce(new DOMException('aborted', 'AbortError'));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(result.current.deleteRefusal).toBeNull();
      expect(storedIds('channel-1')).toEqual(['m1']);
    });

    it('a transport failure on a retry keeps the message and resolves as transport', async () => {
      const { result } = await refuseFirst();
      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
      const outcome = await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });
      await waitFor(() => expect(result.current.deleteRefusal?.view.view).toBe('failed'));

      expect(outcome).toEqual({ kind: 'transport' });
      expect(storedIds('channel-1')).toEqual(['m1']);
    });
  });

  describe('openedAt', () => {
    it('is reset by every response, so each Retry-After counts from its own arrival', async () => {
      vi.useFakeTimers({ toFake: ['Date'] });
      vi.setSystemTime(new Date('2026-09-30T10:00:00Z'));
      const { result } = await refuseFirst();
      expect(result.current.deleteRefusal?.openedAt).toBe(Date.parse('2026-09-30T10:00:00Z'));

      vi.setSystemTime(new Date('2026-09-30T10:05:00Z'));
      mockApiFetch.mockResolvedValueOnce(res(429, { error: 'slow' }, { 'Retry-After': '30' }));
      await confirmAndSettle({ result }, { mfaCode: FIXTURE_OTP });
      await waitFor(() => expect(result.current.deleteRefusal?.view.view).toBe('wait'));

      expect(result.current.deleteRefusal?.openedAt).toBe(Date.parse('2026-09-30T10:05:00Z'));
    });
  });

  // Replaces the promptKey (#3466) block, removed on purpose: the slot no longer
  // counts attempts or carries a per-attempt error. The factor hook empties and
  // refocuses its own input from the outcome, so what the controller owes it is
  // the right outcome for each answer, and a view that stays bare.
  describe('the outcome a retry resolves to (D6)', () => {
    it.each([
      [
        'an invalid code',
        res(403, { error: 'Invalid MFA code' }),
        { kind: 'refusal', refusal: { kind: 'invalidMfaCode' } },
      ],
      [
        'a re-issued soft-lock challenge',
        res(403, CHALLENGE),
        { kind: 'refusal', refusal: { kind: 'mfaRequired', methods: ['totp'] } },
      ],
      [
        'a wrong password at the route',
        res(403, { error: 'Invalid password' }),
        { kind: 'refusal', refusal: { kind: 'invalidPassword' } },
      ],
      ['a 500', res(500, { error: 'boom' }), { kind: 'answered' }],
      [
        'a spent budget',
        res(429, { step_up_budget_exhausted: true }),
        { kind: 'refusal', refusal: { kind: 'rateLimited' } },
      ],
    ] as const)('%s', async (_label, response, expected) => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(response);
      expect(await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP })).toEqual(expected);
    });

    it('an invalid code keeps the confirm view with its methods and no per-attempt error', async () => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(403, { error: 'Invalid MFA code' }));
      await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP });

      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'confirm',
        methods: ['totp'],
      });
    });

    it('a wrong password at the route moves to the bare password view', async () => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(403, { error: 'Invalid password' }));
      await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP });

      expect(hook.result.current.deleteRefusal?.view).toEqual({ view: 'password' });
    });

    // The delete soft-lock's own kind is the hook's mfaRequired.
    it('the soft-lock challenge on a retry is the hook’s mfaRequired with the methods', async () => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(403, { ...CHALLENGE, methods: ['webauthn', 'totp'] }));
      const outcome = await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP });

      expect(outcome).toEqual({
        kind: 'refusal',
        refusal: { kind: 'mfaRequired', methods: ['webauthn', 'totp'] },
      });
      expect(hook.result.current.deleteRefusal?.view).toEqual({
        view: 'confirm',
        methods: ['webauthn', 'totp'],
      });
    });

    it('a 404 on a retry is success: the row is gone', async () => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(404, { error: 'Message not found' }));
      expect(await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP })).toEqual({ kind: 'success' });
    });
  });

  // E8 and the #17 loop: both wire shapes of the enrolment refusal must reach
  // the slot as the terminal enroll view and the hook as the terminal kind.
  describe.each([
    ['without delete_rate_limited', { mfa_enrollment_required: true }, {}],
    [
      'with delete_rate_limited and a Retry-After',
      { delete_rate_limited: true, mfa_enrollment_required: true },
      { 'Retry-After': '60' },
    ],
  ])('enrolment %s', (_label, wire, headers) => {
    const body = { error: 'Set up an authenticator app or security key to do this.', ...wire };

    it('on the FIRST delete, fills the slot with the enroll view: no failed view, no countdown', async () => {
      seed('channel-1', 'm1');
      mockApiFetch.mockResolvedValueOnce(res(403, body, headers));
      const { result } = renderHook(() => useChatController(channelCtx));
      await act(async () => {
        await result.current.deleteMessage('m1');
      });

      expect(result.current.deleteRefusal?.view).toEqual({ view: 'enroll' });
      expect(storedIds('channel-1')).toEqual(['m1']);
    });

    it('on a RETRY, ends the stage: the enroll view, and the hook’s terminal refusal', async () => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(403, body, headers));

      const outcome = await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP });

      expect(outcome).toEqual({ kind: 'refusal', refusal: { kind: 'enrollmentRequired' } });
      expect(hook.result.current.deleteRefusal?.view).toEqual({ view: 'enroll' });
    });

    it('through the password exchange too: a minted token the route then refuses', async () => {
      const hook = await refuseFirst(channelCtx, 'm1', PASSWORD_CHALLENGE);
      mockApiFetch.mockResolvedValueOnce(res(200, { step_up_token: 'tok', expires_in: 60 }));
      mockApiFetch.mockResolvedValueOnce(res(403, body, headers));

      const outcome = await confirmAndSettle(hook, { currentPassword: FIXTURE_PW });

      expect(outcome).toEqual({ kind: 'refusal', refusal: { kind: 'enrollmentRequired' } });
      expect(hook.result.current.deleteRefusal?.view).toEqual({ view: 'enroll' });
    });
  });

  // The factor the hook proved reaches the delete body (a WebAuthn token and a
  // backup code are both just the code).
  describe('the retry body', () => {
    it.each([
      ['a WebAuthn assertion token', 'webauthn-assertion-token-0123456789'],
      ['a backup code', 'abcd1234'],
      ['a TOTP code', FIXTURE_OTP],
    ])('%s is sent as mfa_code, on the channel route', async (_label, code) => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(200, {}));

      expect(await confirmAndSettle(hook, { mfaCode: code })).toEqual({ kind: 'success' });

      expect(mockApiFetch.mock.calls.at(-1)?.[0]).toBe('/api/v1/messages/m1');
      expect(JSON.parse(lastInit().body as string)).toEqual({ mfa_code: code });
    });

    it('a WebAuthn token is sent as mfa_code on the DM route too', async () => {
      const hook = await refuseFirst(dmCtx, 'dm-m1');
      mockApiFetch.mockResolvedValueOnce(res(200, {}));

      await confirmAndSettle(hook, { mfaCode: 'webauthn-assertion-token-0123456789' });

      expect(mockApiFetch.mock.calls.at(-1)?.[0]).toBe(
        '/api/v1/dm/conversations/conv-1/messages/dm-m1'
      );
      expect(JSON.parse(lastInit().body as string)).toEqual({
        mfa_code: 'webauthn-assertion-token-0123456789',
      });
    });

    it('a code wins over a password: nothing is minted', async () => {
      const hook = await refuseFirst();
      mockApiFetch.mockResolvedValueOnce(res(200, {}));

      await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP, currentPassword: FIXTURE_PW });

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
      expect(JSON.parse(lastInit().body as string)).toEqual({ mfa_code: FIXTURE_OTP });
    });

    // apiFetch owns the pre-dispatch fence (mocked here), so what this layer
    // owes it is the run's capture on the retry, and on the exchange before it.
    it('the run’s capture is what admits the retry', async () => {
      const hook = await refuseFirst();
      const capture = captureApiRequestContext();
      mockApiFetch.mockResolvedValueOnce(res(200, {}));

      await confirmAndSettle(hook, { mfaCode: FIXTURE_OTP }, capture);

      expect(mockApiFetch.mock.calls.at(-1)?.[2]).toEqual({ context: capture });
    });

    it('the same capture admits the password exchange and the delete after it', async () => {
      const hook = await refuseFirst(channelCtx, 'm1', PASSWORD_CHALLENGE);
      const capture = captureApiRequestContext();
      mockApiFetch.mockResolvedValueOnce(res(200, { step_up_token: 'tok', expires_in: 60 }));
      mockApiFetch.mockResolvedValueOnce(res(200, {}));

      await confirmAndSettle(hook, { currentPassword: FIXTURE_PW }, capture);

      expect(mockApiFetch.mock.calls[1][2]).toEqual({ context: capture });
      expect(mockApiFetch.mock.calls[2][2]).toEqual({ context: capture });
    });

    it('a retry without a capture still binds the exchange and the delete to one context', async () => {
      const hook = await refuseFirst(channelCtx, 'm1', PASSWORD_CHALLENGE);
      mockApiFetch.mockResolvedValueOnce(res(200, { step_up_token: 'tok', expires_in: 60 }));
      mockApiFetch.mockResolvedValueOnce(res(200, {}));

      await confirmAndSettle(hook, { currentPassword: FIXTURE_PW });

      const mintContext = (mockApiFetch.mock.calls[1][2] as { context: unknown }).context;
      expect(mintContext).toBeDefined();
      expect((mockApiFetch.mock.calls[2][2] as { context: unknown }).context).toBe(mintContext);
    });
  });

  // D7: nothing was sent, so the factor hook is told "aborted" and nothing is
  // shown. A password error here would blame a password nobody tested.
  describe('an unsent password exchange (D7)', () => {
    it('is aborted: no password error, no failed view, and no delete follows', async () => {
      const hook = await refuseFirst(channelCtx, 'm1', PASSWORD_CHALLENGE);
      mockApiFetch.mockRejectedValueOnce(new DOMException('aborted', 'AbortError'));

      const outcome = await confirmAndSettle(hook, { currentPassword: FIXTURE_PW });

      expect(outcome).toEqual({ kind: 'aborted' });
      expect(hook.result.current.deleteRefusal?.view).toEqual({ view: 'password' });
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
      expect(mockApiFetch.mock.calls[1][0]).toBe('/api/v1/auth/step-up/password');
    });

    it('releases the id: a later delete of the same message goes out', async () => {
      const hook = await refuseFirst(channelCtx, 'm1', PASSWORD_CHALLENGE);
      mockApiFetch.mockRejectedValueOnce(new DOMException('aborted', 'AbortError'));
      await confirmAndSettle(hook, { currentPassword: FIXTURE_PW });

      mockApiFetch.mockResolvedValueOnce(res(200, {}));
      await act(async () => {
        await hook.result.current.deleteMessage('m1');
      });
      expect(mockApiFetch).toHaveBeenCalledTimes(3);
    });
  });
});
