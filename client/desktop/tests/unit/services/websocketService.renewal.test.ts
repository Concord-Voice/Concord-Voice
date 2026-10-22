/**
 * websocketService.renewal.test.ts — in-band control-socket token renewal.
 *
 * The control plane closes a socket at the exp of the token that opened it.
 * `renewToken` carries each rotated token over the open socket as an
 * `auth_renew` frame so the server moves that deadline instead. These tests
 * drive the real connect flow (ticket fetch -> WebSocket -> `connected`
 * envelope) against a mock socket and assert on the frames the socket was
 * actually asked to send, which is the seam this code does not own.
 *
 * Timing contract under test: 5 s ack timeout, one retry 11 s after a
 * failure, then give up and leave the old deadline in force.
 */

import { describe, it, expect, beforeEach, afterEach, vi, type Mock } from 'vitest';
import { WebSocketService, ConnectionState } from '@/renderer/services/messaging/websocketService';
import { resetRuntimeServerBase } from '@/renderer/services/system/runtimeServerBase';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { _resetClientVersionCache } from '@/renderer/utils/runtime/clientVersion';
import { resetAllStores } from '../../helpers/store-helpers';

const UUID_CLIENT = '11111111-1111-4111-8111-111111111111';
const UUID_USER = '22222222-2222-4222-8222-222222222222';

// Distinctive values so the console-leak scan cannot match anything by accident.
const T0 = 'tok-origin-SENTINEL-0f3a9c';
const T1 = 'tok-rotated-SENTINEL-7b21de';
const T2 = 'tok-rotated-again-SENTINEL-c4e858';

const ACK_TIMEOUT_MS = 5_000;
const RETRY_DELAY_MS = 11_000;

class MockWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static constructed = 0;

  readyState = MockWebSocket.CONNECTING;
  readonly url: string;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  readonly sentMessages: string[] = [];
  /** When set, the next `auth_renew` send throws instead of recording. */
  throwOnNextRenew = false;

  constructor(url: string) {
    MockWebSocket.constructed += 1;
    this.url = url;
    setTimeout(() => {
      this.readyState = MockWebSocket.OPEN;
      this.onopen?.(new Event('open'));
    }, 0);
  }

  send(data: string): void {
    if (this.throwOnNextRenew && data.includes('"auth_renew"')) {
      this.throwOnNextRenew = false;
      throw new Error('send failed');
    }
    this.sentMessages.push(data);
  }

  close(code = 1000, reason = 'test'): void {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(new CloseEvent('close', { code, reason }));
  }

  simulateMessage(data: object): void {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data) }));
  }

  simulateError(): void {
    this.onerror?.(new Event('error'));
  }
}

/** The private surface these tests read. Typed so no `any` is needed. */
interface RenewalInternals {
  ws: MockWebSocket | null;
  socketToken: string | null;
  sendRenewal: () => void;
}

function internals(svc: WebSocketService): RenewalInternals {
  return svc as unknown as RenewalInternals;
}

interface AuthRenewFrame {
  type: 'auth_renew';
  data: { token: string };
}

function isAuthRenewFrame(v: unknown): v is AuthRenewFrame {
  if (typeof v !== 'object' || v === null) return false;
  const rec = v as Record<string, unknown>;
  if (rec.type !== 'auth_renew') return false;
  const data = rec.data;
  return (
    typeof data === 'object' &&
    data !== null &&
    typeof (data as Record<string, unknown>).token === 'string'
  );
}

/** Raw `auth_renew` frames the socket was asked to send, in order. */
function renewFrames(ws: MockWebSocket): string[] {
  return ws.sentMessages.filter((raw) => {
    const parsed: unknown = JSON.parse(raw);
    return isAuthRenewFrame(parsed);
  });
}

/** Tokens carried by the `auth_renew` frames, in order. */
function renewedTokens(ws: MockWebSocket): string[] {
  return renewFrames(ws).map((raw) => {
    const parsed: unknown = JSON.parse(raw);
    return isAuthRenewFrame(parsed) ? parsed.data.token : '';
  });
}

function ack(ws: MockWebSocket, ok: boolean): void {
  ws.simulateMessage({ type: 'auth_renew_result', data: { ok } });
}

function bumpAuthGeneration(): void {
  useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
}

describe('WebSocketService — in-band token renewal', () => {
  let service: WebSocketService;
  let mockFetch: Mock<typeof fetch>;

  /** Flush the ticket fetch, the WebSocket construction and the mock onopen. */
  async function flushConnect(): Promise<void> {
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(0);
  }

  function currentSocket(): MockWebSocket {
    const ws = internals(service).ws;
    if (!ws) throw new Error('expected an open socket');
    return ws;
  }

  function deliverConnected(ws: MockWebSocket): void {
    ws.simulateMessage({
      type: 'connected',
      data: { client_id: UUID_CLIENT, user_id: UUID_USER },
    });
  }

  /** Connect with `token` and complete the handshake; returns the live socket. */
  async function openConnected(token: string): Promise<MockWebSocket> {
    service.connect(token);
    await flushConnect();
    const ws = currentSocket();
    deliverConnected(ws);
    expect(service.getState()).toBe(ConnectionState.CONNECTED);
    return ws;
  }

  beforeEach(() => {
    vi.useFakeTimers();
    resetAllStores();
    _resetClientVersionCache();
    resetRuntimeServerBase();
    MockWebSocket.constructed = 0;
    mockFetch = vi.fn<typeof fetch>(() =>
      Promise.resolve(
        new Response(JSON.stringify({ ticket: 'mock-ticket' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );
    vi.stubGlobal('fetch', mockFetch);
    vi.stubGlobal('WebSocket', MockWebSocket);
    service = new WebSocketService('ws://localhost:8080');
  });

  afterEach(() => {
    service.disconnect();
    resetRuntimeServerBase();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  // ── 1. The frame ───────────────────────────────────────────────────────
  describe('sending', () => {
    it('sends exactly {"type":"auth_renew","data":{"token":T}} on the open socket', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);

      expect(renewFrames(ws)).toEqual([
        JSON.stringify({ type: 'auth_renew', data: { token: T1 } }),
      ]);
    });

    it('sends the frame on the socket that is currently open, not a superseded one', async () => {
      const first = await openConnected(T0);
      first.close(1000);
      const second = await openConnected(T0);

      service.renewToken(T1);

      expect(renewedTokens(first)).toEqual([]);
      expect(renewedTokens(second)).toEqual([T1]);
    });
  });

  // ── 2. Guards that send nothing ────────────────────────────────────────
  describe('no send', () => {
    it('sends nothing before the connected envelope (state is not CONNECTED)', async () => {
      service.connect(T0);
      await flushConnect();
      const ws = currentSocket();
      expect(service.getState()).toBe(ConnectionState.CONNECTING);

      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([]);
    });

    it('sends nothing when disconnected and no socket exists', () => {
      expect(() => service.renewToken(T1)).not.toThrow();
      expect(internals(service).ws).toBeNull();
    });

    it('sends nothing when T equals the token the socket was opened with', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T0);

      expect(renewedTokens(ws)).toEqual([]);
    });

    it('sends nothing when authGeneration changed since the socket opened', async () => {
      const ws = await openConnected(T0);
      const sendRenewal = vi.spyOn(internals(service), 'sendRenewal');

      bumpAuthGeneration();
      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([]);
      // The per-send currentness check would also drop this frame, so the
      // outcome above cannot tell the two guards apart. A token from another
      // generation must never reach the send path at all.
      expect(sendRenewal).not.toHaveBeenCalled();
    });

    it('does not send a duplicate frame while a renewal for the same T is in flight', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(1_000);
      service.renewToken(T1);

      expect(renewedTokens(ws)).toEqual([T1]);
    });
  });

  // ── 3. ok:true ──────────────────────────────────────────────────────────
  describe('accepted', () => {
    it('ok:true ends the renewal and advances the socket token', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, true);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([T1]);

      // socketToken advanced: the same token is now "already current".
      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS);
      expect(renewedTokens(ws)).toEqual([T1]);
    });

    it('a later rotation after an accepted renewal is sent', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, true);
      service.renewToken(T2);

      expect(renewedTokens(ws)).toEqual([T1, T2]);
    });
  });

  // ── 4. ok:false ─────────────────────────────────────────────────────────
  describe('refused', () => {
    it('ok:false retries exactly once 11 s later, then gives up after a second refusal', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);

      await vi.advanceTimersByTimeAsync(10_900);
      expect(renewedTokens(ws)).toEqual([T1]);

      await vi.advanceTimersByTimeAsync(100);
      expect(renewedTokens(ws)).toEqual([T1, T1]);

      ack(ws, false);
      await vi.advanceTimersByTimeAsync(60_000);
      expect(renewedTokens(ws)).toEqual([T1, T1]);
    });

    it('a retry that is accepted advances the socket token', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS);
      ack(ws, true);

      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(60_000);
      expect(renewedTokens(ws)).toEqual([T1, T1]);
    });

    it('a send that throws counts as a failed attempt and is retried once', async () => {
      const ws = await openConnected(T0);
      ws.throwOnNextRenew = true;

      service.renewToken(T1);
      expect(renewedTokens(ws)).toEqual([]);

      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS - 1);
      expect(renewedTokens(ws)).toEqual([]);
      await vi.advanceTimersByTimeAsync(1);
      expect(renewedTokens(ws)).toEqual([T1]);

      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);
      expect(renewedTokens(ws)).toEqual([T1]);
    });
  });

  // ── 5. Ack timeout ──────────────────────────────────────────────────────
  describe('no ack', () => {
    it('no ack within 5 s is a failure: retry at +11 s, second timeout gives up', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);

      // 5 s timeout + 11 s retry delay = first retry at 16 s.
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS - 100);
      expect(renewedTokens(ws)).toEqual([T1]);
      await vi.advanceTimersByTimeAsync(100);
      expect(renewedTokens(ws)).toEqual([T1, T1]);

      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + 60_000);
      expect(renewedTokens(ws)).toEqual([T1, T1]);
    });
  });

  // ── 6. Retry currentness ────────────────────────────────────────────────
  describe('retry is skipped when no longer current', () => {
    it('skips the retry when the socket closed before it fired', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);
      // Closed at the transport, close event not yet delivered.
      ws.readyState = MockWebSocket.CLOSED;
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([T1]);
    });

    it('skips the retry when the state left CONNECTED before it fired', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);
      ws.simulateError();
      expect(service.getState()).toBe(ConnectionState.ERROR);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([T1]);
    });

    it('skips the retry when authGeneration changed before it fired', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);
      bumpAuthGeneration();
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([T1]);
    });

    it('control: the same retry does fire when nothing changed', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS);

      expect(renewedTokens(ws)).toEqual([T1, T1]);
    });
  });

  // ── 7. Late ack ─────────────────────────────────────────────────────────
  describe('late ack', () => {
    it('an ack after the final attempt timed out changes nothing', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);
      expect(renewedTokens(ws)).toEqual([T1, T1]);

      ack(ws, true);
      expect(internals(service).socketToken).toBe(T0);

      // Not suppressed as "already current": a fresh renewal goes out.
      service.renewToken(T1);
      expect(renewedTokens(ws)).toEqual([T1, T1, T1]);
    });

    // After an attempt times out the renewal waits 11 s to retry. An ack that
    // lands in that wait answers no live attempt and must change nothing.
    it('an ack that arrives during the retry wait (attempt timed out) changes nothing', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + 1_000);
      expect(renewedTokens(ws)).toEqual([T1]);

      ack(ws, true);

      expect(internals(service).socketToken).toBe(T0);
      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS);
      expect(renewedTokens(ws).length).toBeGreaterThan(1);
    });
  });

  // ── 8. Teardown cancels ─────────────────────────────────────────────────
  describe('teardown cancels a pending renewal', () => {
    it('handleClose cancels it: no frame afterwards, and the slot is free for the next socket', async () => {
      const first = await openConnected(T0);

      service.renewToken(T1);
      first.close(1000);
      expect(service.getState()).toBe(ConnectionState.DISCONNECTED);

      // A fresh socket opened with the same token, inside the old ack window.
      const second = await openConnected(T0);
      service.renewToken(T1);
      expect(renewedTokens(second)).toEqual([T1]);

      ack(second, true);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);
      expect(renewedTokens(first)).toEqual([T1]);
      expect(renewedTokens(second)).toEqual([T1]);
    });

    it('handleClose on an abnormal close cancels it and no timer fires a frame', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      ack(ws, false);
      ws.close(1006);
      expect(service.getState()).toBe(ConnectionState.RECONNECTING);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS + ACK_TIMEOUT_MS);

      expect(renewedTokens(ws)).toEqual([T1]);
    });

    it('disconnect cancels it: no frame afterwards, and the slot is free for the next socket', async () => {
      const first = await openConnected(T0);

      service.renewToken(T1);
      ack(first, false);
      service.disconnect();

      const second = await openConnected(T0);
      service.renewToken(T1);
      expect(renewedTokens(second)).toEqual([T1]);

      ack(second, true);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS + ACK_TIMEOUT_MS + RETRY_DELAY_MS);
      expect(renewedTokens(first)).toEqual([T1]);
      expect(renewedTokens(second)).toEqual([T1]);
    });
  });

  // ── 9. Rotation during the ticket fetch ─────────────────────────────────
  describe('rotation while the ticket was in flight', () => {
    it('on connected, renews with the token that arrived after the ticket was fetched', async () => {
      service.connect(T0);
      // The ticket request already captured T0; the rotation lands now.
      service.updateToken(T1);
      await flushConnect();
      const ws = currentSocket();
      expect(mockFetch).toHaveBeenCalledTimes(1);
      const headers = mockFetch.mock.calls[0][1]?.headers as Record<string, string>;
      expect(headers.Authorization).toBe(`Bearer ${T0}`);
      expect(renewedTokens(ws)).toEqual([]);

      deliverConnected(ws);

      expect(service.getState()).toBe(ConnectionState.CONNECTED);
      expect(renewedTokens(ws)).toEqual([T1]);
    });

    it('control: no renewal on connected when the token did not rotate', async () => {
      const ws = await openConnected(T0);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS);

      expect(renewedTokens(ws)).toEqual([]);
    });
  });

  // ── 10. Superseding renewal ─────────────────────────────────────────────
  describe('a newer token while one is in flight', () => {
    it('replaces the in-flight renewal: T2 is sent at once and only T2 is retried', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(1_000);
      service.renewToken(T2);
      expect(renewedTokens(ws)).toEqual([T1, T2]);

      // T1's ack timer was cancelled; T2 times out and is retried once.
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS + 60_000);
      expect(renewedTokens(ws)).toEqual([T1, T2, T2]);
    });

    it('an ack arriving after the replacement is attributed to T2 (frames carry no correlation id)', async () => {
      const ws = await openConnected(T0);

      service.renewToken(T1);
      service.renewToken(T2);
      // This may be the server's answer to T1; the client cannot tell.
      ack(ws, true);

      expect(internals(service).socketToken).toBe(T2);
      service.renewToken(T2);
      expect(renewedTokens(ws)).toEqual([T1, T2]);
    });
  });

  // ── 11. Token never logged ──────────────────────────────────────────────
  describe('logging', () => {
    it('never writes a token to any console method on any renewal path', async () => {
      const methods = ['log', 'info', 'warn', 'error', 'debug', 'trace'] as const;
      const spies = methods.map((m) => vi.spyOn(console, m).mockImplementation(() => {}));

      // Rotation during the ticket fetch, then renewal on connected.
      service.connect(T0);
      service.updateToken(T1);
      await flushConnect();
      const ws = currentSocket();
      deliverConnected(ws);
      // Refusal, retry, accepted.
      ack(ws, false);
      await vi.advanceTimersByTimeAsync(RETRY_DELAY_MS);
      ack(ws, true);
      // Timeout path to give-up.
      service.renewToken(T2);
      await vi.advanceTimersByTimeAsync(ACK_TIMEOUT_MS + RETRY_DELAY_MS + ACK_TIMEOUT_MS);
      // Late and malformed results.
      ack(ws, true);
      ws.simulateMessage({ type: 'auth_renew_result', data: { ok: 'yes' } });
      ws.simulateMessage({ type: 'auth_renew_result', data: { token: T2 } });
      // Send failure, then abnormal close with a renewal pending.
      ws.throwOnNextRenew = true;
      service.renewToken(T0 + '-x');
      service.renewToken(T2 + '-y');
      ws.close(1006);
      await vi.advanceTimersByTimeAsync(60_000);
      service.disconnect();

      const logged = spies
        .flatMap((s) => s.mock.calls.flat())
        .map((arg) => {
          if (typeof arg === 'string') return arg;
          if (arg instanceof Error) return `${arg.name} ${arg.message} ${arg.stack ?? ''}`;
          try {
            return JSON.stringify(arg);
          } catch {
            return String(arg);
          }
        })
        .join('\n');

      expect(logged).not.toContain('SENTINEL');
      for (const t of [T0, T1, T2]) expect(logged).not.toContain(t);
      // Positive control: the scan saw real output on these paths.
      expect(logged.length).toBeGreaterThan(0);
    });
  });

  // ── 13. Old server ──────────────────────────────────────────────────────
  describe('old server that ignores auth_renew', () => {
    it('two attempts, then nothing; the socket stays CONNECTED and no reconnect starts', async () => {
      const ws = await openConnected(T0);
      const constructedBefore = MockWebSocket.constructed;
      const fetchesBefore = mockFetch.mock.calls.length;

      service.renewToken(T1);
      await vi.advanceTimersByTimeAsync(5 * 60_000);

      expect(renewedTokens(ws)).toEqual([T1, T1]);
      expect(service.getState()).toBe(ConnectionState.CONNECTED);
      expect(internals(service).ws).toBe(ws);
      expect(MockWebSocket.constructed).toBe(constructedBefore);
      expect(mockFetch.mock.calls.length).toBe(fetchesBefore);
    });
  });
});
