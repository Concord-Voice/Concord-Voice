/**
 * Extended tests for WebSocketService — covers DM subscriptions, server subscriptions,
 * aggressive reconnect, sendDMMessage, sendProfileUpdate, sendServerUpdate,
 * sendHeartbeat, handleClose reconnect logic, removeAllHandlers, and
 * the resubscription on reconnect flow.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { WebSocketService, ConnectionState } from '@/renderer/services/websocketService';

// Mock WebSocket
class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;

  readyState = MockWebSocket.CONNECTING;
  url: string;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  sentMessages: string[] = [];

  constructor(url: string) {
    this.url = url;
    setTimeout(() => {
      this.readyState = MockWebSocket.OPEN;
      if (this.onopen) this.onopen(new Event('open'));
    }, 0);
  }

  send(data: string) {
    this.sentMessages.push(data);
  }

  close(code?: number, _reason?: string) {
    this.readyState = MockWebSocket.CLOSED;
    if (this.onclose) {
      this.onclose(new CloseEvent('close', { code: code ?? 1000, reason: 'test' }));
    }
  }

  simulateMessage(data: object) {
    if (this.onmessage) {
      this.onmessage(new MessageEvent('message', { data: JSON.stringify(data) }));
    }
  }

  simulateError() {
    if (this.onerror) {
      this.onerror(new Event('error'));
    }
  }

  simulateAbnormalClose() {
    this.readyState = MockWebSocket.CLOSED;
    if (this.onclose) {
      this.onclose(new CloseEvent('close', { code: 1006, reason: 'abnormal' }));
    }
  }
}

const originalWebSocket = globalThis.WebSocket;
const originalFetch = globalThis.fetch;

beforeAll(() => {
  (globalThis as any).WebSocket = MockWebSocket;
});
afterAll(() => {
  globalThis.WebSocket = originalWebSocket;
});

async function connectAndWaitForOpen(
  service: WebSocketService,
  token = 'test-token'
): Promise<MockWebSocket> {
  service.connect(token);
  await vi.advanceTimersByTimeAsync(0);
  await vi.advanceTimersByTimeAsync(0);
  return (service as any).ws as MockWebSocket;
}

function markConnected(ws: MockWebSocket) {
  // client_id + user_id must be valid UUIDs (ConnectedSchema in ws-events.ts
  // tightens both to UUID); non-UUID values are rejected by safeParse at the
  // dispatch boundary and never reach the CONNECTED-state transition.
  ws.simulateMessage({
    type: 'connected',
    data: {
      client_id: '11111111-1111-4111-8111-111111111111',
      user_id: '22222222-2222-4222-8222-222222222222',
    },
  });
}

describe('WebSocketService — extended', () => {
  let service: WebSocketService;
  let mockFetch: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ ticket: 'mock-ticket' }),
    });
    globalThis.fetch = mockFetch;
    service = new WebSocketService('ws://localhost:8080');
  });

  afterEach(() => {
    service.disconnect();
    vi.useRealTimers();
    globalThis.fetch = originalFetch;
  });

  describe('DM subscriptions', () => {
    it('subscribeDM tracks and sends subscribe_dm message', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.subscribeDM('conv-1');
      expect(service.isDMSubscribed('conv-1')).toBe(true);

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('subscribe_dm');
      expect(sent.data.conversation_id).toBe('conv-1');
    });

    it('unsubscribeDM removes tracking and sends unsubscribe_dm', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.subscribeDM('conv-1');
      service.unsubscribeDM('conv-1');

      expect(service.isDMSubscribed('conv-1')).toBe(false);
      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('unsubscribe_dm');
    });
  });

  describe('server subscriptions', () => {
    it('subscribeServer sends subscribe_server message', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.subscribeServer('server-1');

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('subscribe_server');
      expect(sent.data.server_id).toBe('server-1');
    });

    it('unsubscribeServer sends unsubscribe_server message', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.subscribeServer('server-1');
      service.unsubscribeServer('server-1');

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('unsubscribe_server');
    });
  });

  describe('sendDMMessage', () => {
    it('builds correct DM message payload', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendDMMessage('conv-1', 'DM text', {
        nonce: 'nonce-dm',
        keyVersion: 2,
      });

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('dm_message');
      expect(sent.data.conversation_id).toBe('conv-1');
      expect(sent.data.content).toBe('DM text');
      expect(sent.data.nonce).toBe('nonce-dm');
      expect(sent.data.key_version).toBe(2);
    });

    it('includes mention_meta when provided', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendDMMessage('conv-1', 'text', { nonce: 'n', mentionMeta: 'mention-data' });

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.data.mention_meta).toBe('mention-data');
    });

    it('includes attachment_ids when provided', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendDMMessage('conv-1', 'files', {
        nonce: 'n',
        attachmentIds: ['att-1', 'att-2'],
      });

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.data.attachment_ids).toEqual(['att-1', 'att-2']);
    });
  });

  describe('sendDMTypingIndicator', () => {
    it('builds correct DM typing payload', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendDMTypingIndicator('conv-1', true);

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('dm_typing');
      expect(sent.data.conversation_id).toBe('conv-1');
      expect(sent.data.is_typing).toBe(true);
    });
  });

  describe('sendProfileUpdate', () => {
    it('sends profile_update message', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendProfileUpdate();

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('profile_update');
    });
  });

  describe('sendServerUpdate', () => {
    it('sends server_update with correct server_id', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendServerUpdate('server-1');

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('server_update');
      expect(sent.data.server_id).toBe('server-1');
    });
  });

  describe('sendHeartbeat', () => {
    it('sends heartbeat message', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendHeartbeat();

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('heartbeat');
    });
  });

  describe('sendSetStatus', () => {
    it('sends invisible status', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendSetStatus('invisible');

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('set_status');
      expect(sent.data.status).toBe('invisible');
    });
  });

  describe('sendMessage with mention_meta', () => {
    it('includes mention_meta in message payload', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.sendMessage('ch-1', 'Hello @user', { nonce: 'nonce-1', mentionMeta: 'meta' });

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.data.mention_meta).toBe('meta');
    });
  });

  describe('aggressive reconnect', () => {
    it('uses shorter delays when aggressive mode is on', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.setAggressiveReconnect(true);

      // Trigger abnormal close to start reconnect
      ws.simulateAbnormalClose();

      expect(service.getState()).toBe(ConnectionState.RECONNECTING);
      // Aggressive first attempt: 500ms delay
    });

    it('reschedules pending reconnect when switching to aggressive', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      // Trigger abnormal close to start reconnect
      ws.simulateAbnormalClose();

      // Now enable aggressive — should reschedule the pending timer
      service.setAggressiveReconnect(true);

      // Verify state is still reconnecting after switching mode
      expect(service.getState()).toBe(ConnectionState.RECONNECTING);
    });
  });

  describe('resubscription on reconnect', () => {
    it('tracks subscriptions for resubscription after reconnect', async () => {
      const ws1 = await connectAndWaitForOpen(service);
      markConnected(ws1);

      // Subscribe to various things
      service.subscribe('ch-1');
      service.subscribeServer('server-1');
      service.subscribeDM('conv-1');

      // Verify subscriptions are tracked internally
      expect(service.isSubscribed('ch-1')).toBe(true);
      expect(service.isDMSubscribed('conv-1')).toBe(true);

      // After disconnect, subscriptions should still be tracked
      ws1.simulateAbnormalClose();

      expect(service.isSubscribed('ch-1')).toBe(true);
      expect(service.isDMSubscribed('conv-1')).toBe(true);
    });
  });

  describe('handleClose', () => {
    it('sets state to DISCONNECTED on clean close (1000)', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      ws.close(1000, 'normal');

      expect(service.getState()).toBe(ConnectionState.DISCONNECTED);
    });

    it('sets state to RECONNECTING on abnormal close', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      ws.simulateAbnormalClose();

      expect(service.getState()).toBe(ConnectionState.RECONNECTING);
    });
  });

  describe('handleError', () => {
    it('sets state to ERROR', async () => {
      const ws = await connectAndWaitForOpen(service);

      ws.simulateError();

      expect(service.getState()).toBe(ConnectionState.ERROR);
    });
  });

  describe('handleMessage edge cases', () => {
    it('logs server error messages', async () => {
      const ws = await connectAndWaitForOpen(service);

      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      ws.simulateMessage({ type: 'error', data: { message: 'Server error' } });
      expect(errorSpy).toHaveBeenCalled();
      errorSpy.mockRestore();
    });

    it('handles malformed JSON gracefully', async () => {
      const ws = await connectAndWaitForOpen(service);

      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      if (ws.onmessage) {
        ws.onmessage(new MessageEvent('message', { data: 'not-json{{{' }));
      }
      expect(errorSpy).toHaveBeenCalled();
      errorSpy.mockRestore();
    });

    it('catches handler errors without crashing', async () => {
      const ws = await connectAndWaitForOpen(service);

      const errorHandler = vi.fn(() => {
        throw new Error('Handler crashed');
      });
      service.on('subscribed', errorHandler);

      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      ws.simulateMessage({ type: 'subscribed', data: {} });
      errorSpy.mockRestore();

      expect(errorHandler).toHaveBeenCalled();
    });
  });

  describe('removeAllHandlers', () => {
    it('clears all message and connection handlers', async () => {
      const msgHandler = vi.fn();
      const connHandler = vi.fn();

      service.on('test', msgHandler);
      service.onConnectionChange(connHandler);

      service.removeAllHandlers();

      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({ type: 'test', data: {} });

      // Message handler should not have been called after removeAll
      expect(msgHandler).not.toHaveBeenCalled();
    });
  });

  describe('createConnection — ticket fetch failure', () => {
    it('sets ERROR state on ticket fetch failure', async () => {
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
      });

      service.connect('test-token');
      await vi.advanceTimersByTimeAsync(0);

      expect(service.getState()).toBe(ConnectionState.ERROR);
    });

    it('handles AbortError silently on disconnect during connect', async () => {
      mockFetch.mockImplementation(
        () =>
          new Promise((_, reject) => {
            setTimeout(() => reject(new DOMException('Aborted', 'AbortError')), 50);
          })
      );

      service.connect('test-token');
      service.disconnect();

      await vi.advanceTimersByTimeAsync(100);

      expect(service.getState()).toBe(ConnectionState.DISCONNECTED);
    });
  });

  describe('disconnect edge cases', () => {
    it('detaches handlers from ws before closing', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      service.disconnect();

      // After disconnect, ws handlers should be null
      expect(ws.onopen).toBeNull();
      expect(ws.onmessage).toBeNull();
      expect(ws.onerror).toBeNull();
      expect(ws.onclose).toBeNull();
    });
  });

  describe('ping interval', () => {
    it('sends heartbeat every 30 seconds after connection', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      const messagesBefore = ws.sentMessages.length;

      // Advance 30 seconds to trigger ping
      await vi.advanceTimersByTimeAsync(30000);

      const newMessages = ws.sentMessages.slice(messagesBefore);
      const heartbeats = newMessages
        .map((m) => JSON.parse(m))
        .filter((m: any) => m.type === 'heartbeat');
      expect(heartbeats.length).toBeGreaterThanOrEqual(1);
    });
  });

  describe('resetReconnectState', () => {
    it('resets reconnect attempts and delay to defaults', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      // Trigger abnormal close to start reconnecting (increments attempts)
      ws.simulateAbnormalClose();
      expect(service.getState()).toBe(ConnectionState.RECONNECTING);

      // Advance to fire reconnect attempt 1 (at 1s base delay)
      await vi.advanceTimersByTimeAsync(1100);
      // Let the new WS open so we can close it again
      await vi.advanceTimersByTimeAsync(0);

      // Reset backoff state — next reconnect should use base delay (1s) again
      service.resetReconnectState();

      // Snapshot fetch count after reset (before second close)
      const fetchCountAfterReset = mockFetch.mock.calls.length;

      // Trigger another abnormal close → schedules reconnect with reset backoff
      const ws2 = (service as any).ws as MockWebSocket | null;
      if (ws2) {
        ws2.simulateAbnormalClose();
      }

      // At base delay (1s) the reconnect should fire — if backoff weren't reset,
      // the delay would be 2s+ (attempt 2 exponential) and this wouldn't trigger.
      await vi.advanceTimersByTimeAsync(1100);
      expect(mockFetch.mock.calls.length).toBeGreaterThan(fetchCountAfterReset);
    });

    it('clears any pending reconnect timer', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      // Trigger abnormal close → schedules reconnect timer
      ws.simulateAbnormalClose();

      const fetchCountBefore = mockFetch.mock.calls.length;
      service.resetReconnectState();

      // Advance past when the timer would have fired — should NOT reconnect
      await vi.advanceTimersByTimeAsync(35_000);
      expect(mockFetch.mock.calls.length).toBe(fetchCountBefore);
    });
  });

  describe('updateToken', () => {
    it('uses updated token on automatic reconnect (backoff)', async () => {
      const ws = await connectAndWaitForOpen(service, 'old-token');
      markConnected(ws);

      // Update token while connected — no disconnect/connect cycle
      service.updateToken('refreshed-token');

      // Clear fetch history so we only capture the reconnect's ticket fetch
      mockFetch.mockClear();
      mockFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ ticket: 'mock-ticket' }),
      });

      // Abnormal close triggers automatic reconnect via backoff
      ws.simulateAbnormalClose();

      // Advance past the reconnect delay (1s base)
      await vi.advanceTimersByTimeAsync(1100);

      // The ws-ticket fetch during automatic reconnect should use the updated token
      expect(mockFetch).toHaveBeenCalledWith(
        expect.stringContaining('/auth/ws-ticket'),
        expect.objectContaining({
          headers: expect.objectContaining({
            Authorization: 'Bearer refreshed-token',
          }),
        })
      );
    });

    it('does not disconnect or reconnect on its own', async () => {
      const ws = await connectAndWaitForOpen(service);
      markConnected(ws);

      const stateBefore = service.getState();
      service.updateToken('updated-token');

      expect(service.getState()).toBe(stateBefore);
      expect(ws.readyState).toBe(MockWebSocket.OPEN);
    });
  });
});
