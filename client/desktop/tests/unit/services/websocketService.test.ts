import {
  WebSocketService,
  ConnectionState,
  fullJitter,
} from '@/renderer/services/websocketService';

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
    // Simulate async open
    setTimeout(() => {
      this.readyState = MockWebSocket.OPEN;
      if (this.onopen) this.onopen(new Event('open'));
    }, 0);
  }

  send(data: string) {
    this.sentMessages.push(data);
  }

  close(_code?: number, _reason?: string) {
    this.readyState = MockWebSocket.CLOSED;
    if (this.onclose) {
      this.onclose(new CloseEvent('close', { code: 1000, reason: 'test' }));
    }
  }

  // Test helper: simulate incoming message
  simulateMessage(data: object) {
    if (this.onmessage) {
      this.onmessage(new MessageEvent('message', { data: JSON.stringify(data) }));
    }
  }

  // Test helper: simulate error
  simulateError() {
    if (this.onerror) {
      this.onerror(new Event('error'));
    }
  }
}

// Install mocks
const originalWebSocket = globalThis.WebSocket;
const originalFetch = globalThis.fetch;

beforeAll(() => {
  (globalThis as any).WebSocket = MockWebSocket;
});
afterAll(() => {
  globalThis.WebSocket = originalWebSocket;
});

// Helper: connect and wait for the async createConnection chain to resolve
// (fetch ticket → json parse → new WebSocket → setTimeout onopen)
async function connectAndWaitForOpen(
  service: WebSocketService,
  token = 'test-token'
): Promise<MockWebSocket> {
  service.connect(token);
  // Flush microtasks: resolve fetch() promise
  await vi.advanceTimersByTimeAsync(0);
  // Flush microtasks: resolve json() promise + new WebSocket setTimeout(0)
  await vi.advanceTimersByTimeAsync(0);
  const ws = (service as any).ws as MockWebSocket;
  return ws;
}

describe('WebSocketService', () => {
  let service: WebSocketService;
  let mockFetch: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    // Mock fetch for ws-ticket endpoint
    mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ ticket: 'mock-ticket' }),
    });
    globalThis.fetch = mockFetch;
    service = new WebSocketService('ws://localhost:8080');
  });

  afterEach(() => {
    service.disconnect();
    vi.restoreAllMocks();
    vi.useRealTimers();
    globalThis.fetch = originalFetch;
  });

  describe('initial state', () => {
    it('starts disconnected', () => {
      expect(service.getState()).toBe(ConnectionState.DISCONNECTED);
    });

    it('has no connection info', () => {
      expect(service.getConnectionInfo()).toBeNull();
    });
  });

  describe('connect', () => {
    it('sets state to CONNECTING', () => {
      service.connect('test-token');
      expect(service.getState()).toBe(ConnectionState.CONNECTING);
    });

    it('does not reconnect if already connected', async () => {
      const ws = await connectAndWaitForOpen(service);

      // Simulate 'connected' message to set CONNECTED state
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      expect(service.getState()).toBe(ConnectionState.CONNECTED);

      // Try connecting again — should be a no-op
      service.connect('test-token');
      expect(service.getState()).toBe(ConnectionState.CONNECTED);
    });

    it('restarts an in-flight connect when called with a newer token (#1977)', () => {
      mockFetch.mockResolvedValue({
        ok: true,
        json: () => new Promise(() => undefined),
      });

      service.connect('stale-token');
      expect(mockFetch).toHaveBeenCalledTimes(1);

      service.connect('fresh-token');

      expect(mockFetch).toHaveBeenCalledTimes(2);
      expect(mockFetch.mock.calls[1][1]?.headers).toMatchObject({
        Authorization: 'Bearer fresh-token',
      });
    });

    it('detaches a superseded connecting socket when the token rotates after ticket fetch (#1977)', async () => {
      service.connect('stale-token');
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(0);

      const staleWs = (service as any).ws as MockWebSocket;
      expect(staleWs).not.toBeNull();
      expect(staleWs.onopen).not.toBeNull();

      service.connect('fresh-token');

      expect(staleWs.onopen).toBeNull();
      expect(staleWs.onmessage).toBeNull();
      expect(staleWs.onerror).toBeNull();
      expect(staleWs.onclose).toBeNull();
      expect(staleWs.readyState).toBe(MockWebSocket.CLOSED);
      expect(mockFetch).toHaveBeenCalledTimes(2);
    });

    it('clears an armed reconnect timer when a fresh token supersedes reconnecting (#1977)', async () => {
      vi.spyOn(Math, 'random').mockReturnValue(0.5);
      const s = service as unknown as {
        token: string | null;
        state: ConnectionState;
        reconnectTimer: NodeJS.Timeout | null;
        scheduleReconnect: () => void;
      };
      s.token = 'stale-token';
      s.state = ConnectionState.RECONNECTING;
      s.scheduleReconnect();
      expect(s.reconnectTimer).not.toBeNull();

      service.connect('fresh-token');

      expect(s.reconnectTimer).toBeNull();
      await vi.advanceTimersByTimeAsync(1000);
      expect(mockFetch).toHaveBeenCalledTimes(1);
    });

    // ─── Connection-boundary validation (defense-in-depth) ──────────────
    // The validator fails-closed before `new WebSocket(wsUrl)` to defend
    // against future regressions that let attacker-influenced data reach
    // the connection URL.

    it('refuses to connect when the configured URL has a non-WebSocket scheme', async () => {
      const evilService = new WebSocketService('https://attacker.example');
      evilService.connect('test-token');
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(0);
      // No WebSocket constructed; service transitioned to ERROR
      expect((evilService as any).ws).toBeNull();
      expect(evilService.getState()).toBe(ConnectionState.ERROR);
      // CRITICAL: the validator must reject BEFORE the ticket fetch — otherwise
      // a tainted `this.url` would leak the Bearer token to the attacker host.
      // Per Copilot review on PR #943.
      expect(mockFetch).not.toHaveBeenCalled();
    });

    it('refuses to connect when the ticket contains URL-control characters', async () => {
      vi.spyOn(Math, 'random').mockReturnValue(0.5);
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ ticket: 'foo&secondary=hijack' }),
      });
      service.connect('test-token');
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(0);
      expect((service as any).ws).toBeNull();
      expect(service.getState()).toBe(ConnectionState.ERROR);
    });

    it('refuses to connect when the ticket is oversized', async () => {
      vi.spyOn(Math, 'random').mockReturnValue(0.5);
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ ticket: 'a'.repeat(5000) }),
      });
      service.connect('test-token');
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(0);
      expect((service as any).ws).toBeNull();
      expect(service.getState()).toBe(ConnectionState.ERROR);
    });

    it('accepts a hex-shaped ticket and a ws: URL in dev (happy path)', async () => {
      // The default mock returns 'mock-ticket' which matches the allowlist; this
      // test asserts the happy path without modification — the websocket is
      // created and reaches the ws state, and the ticket fetch happened.
      const ws = await connectAndWaitForOpen(service);
      expect(ws).not.toBeNull();
      // Positive control: the validator did NOT short-circuit the fetch.
      // Pairs with the "non-WebSocket scheme refuses" test which asserts the inverse.
      expect(mockFetch).toHaveBeenCalled();
    });

    it('refuses to connect with ws: URL when running in production (PROD-mode wss enforcement)', async () => {
      // Stub import.meta.env.PROD so validateWsBaseUrl applies the prod-only
      // wss-required rule. Without this test, that rule is dead-code in CI —
      // a regression that drops the guard would ship unnoticed.
      vi.stubEnv('PROD', 'true');
      try {
        const prodService = new WebSocketService('ws://localhost:8080');
        prodService.connect('test-token');
        await vi.advanceTimersByTimeAsync(0);
        await vi.advanceTimersByTimeAsync(0);
        expect((prodService as any).ws).toBeNull();
        expect(prodService.getState()).toBe(ConnectionState.ERROR);
        // The wss-enforcement check is at the URL-validation gate — fetch must
        // not have been called, otherwise the token would have leaked over plain ws.
        expect(mockFetch).not.toHaveBeenCalled();
      } finally {
        vi.unstubAllEnvs();
      }
    });
  });

  describe('disconnect', () => {
    it('sets state to DISCONNECTED', () => {
      service.connect('test-token');
      service.disconnect();
      expect(service.getState()).toBe(ConnectionState.DISCONNECTED);
    });

    it('clears connection info', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });
      expect(service.getConnectionInfo()).not.toBeNull();

      service.disconnect();
      expect(service.getConnectionInfo()).toBeNull();
    });
  });

  describe('message handling', () => {
    it('stores connection info from connected message', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      const info = service.getConnectionInfo();
      expect(info?.clientId).toBe('11111111-1111-4111-8111-111111111111');
      expect(info?.userId).toBe('22222222-2222-4222-8222-222222222222');
    });

    it('calls registered message handlers', async () => {
      const ws = await connectAndWaitForOpen(service);

      const handler = vi.fn();
      service.on('subscribed', handler);

      // Schema strips unknown keys at dispatch — `{ foo: 'bar' }` does not
      // survive zod.parse against SubscribedSchema (which has no `foo`).
      // Send a valid channel_id and expect the parsed shape to reach the handler.
      ws.simulateMessage({
        type: 'subscribed',
        data: { channel_id: '33333333-3333-4333-8333-333333333333' },
      });

      expect(handler).toHaveBeenCalledWith({
        type: 'subscribed',
        data: { channel_id: '33333333-3333-4333-8333-333333333333' },
      });
    });

    it('can remove message handlers', async () => {
      const ws = await connectAndWaitForOpen(service);

      const handler = vi.fn();
      const off = service.on('subscribed', handler);
      off();

      ws.simulateMessage({ type: 'subscribed', data: {} });

      expect(handler).not.toHaveBeenCalled();
    });
  });

  describe('connection state callbacks', () => {
    it('notifies immediately with current state', () => {
      const callback = vi.fn();
      service.onConnectionChange(callback);
      // Immediately called exactly once with current state (DISCONNECTED)
      expect(callback).toHaveBeenCalledTimes(1);
      expect(callback).toHaveBeenCalledWith(ConnectionState.DISCONNECTED);
    });

    it('notifies on state changes after connect', () => {
      const callback = vi.fn();
      service.onConnectionChange(callback);
      callback.mockClear();

      service.connect('test-token');
      expect(callback).toHaveBeenCalledWith(ConnectionState.CONNECTING);
    });

    it('can unsubscribe from state changes', () => {
      const callback = vi.fn();
      const unsub = service.onConnectionChange(callback);
      unsub();
      callback.mockClear();

      service.connect('test-token');
      expect(callback).not.toHaveBeenCalled();
    });
  });

  describe('channel subscriptions', () => {
    it('tracks subscribed channels', () => {
      expect(service.isSubscribed('ch1')).toBe(false);

      // Manually add (subscribe sends via WS which needs OPEN state)
      (service as any).subscribedChannels.add('ch1');
      expect(service.isSubscribed('ch1')).toBe(true);
    });
  });

  describe('send methods', () => {
    it('sendMessage builds correct payload', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      service.sendMessage('ch-1', 'Hello', { nonce: 'nonce-1' });

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('message');
      expect(sent.data.channel_id).toBe('ch-1');
      expect(sent.data.content).toBe('Hello');
      expect(sent.data.nonce).toBe('nonce-1');
    });

    it('sendMessage includes attachment_ids when provided', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      service.sendMessage('ch-1', 'Files attached', {
        nonce: 'nonce-2',
        attachmentIds: ['file-1', 'file-2'],
      });

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.data.attachment_ids).toEqual(['file-1', 'file-2']);
    });

    it('sendMessage omits attachment_ids when empty', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      service.sendMessage('ch-1', 'No files');

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.data.attachment_ids).toBeUndefined();
    });

    it('sendTypingIndicator builds correct payload', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      service.sendTypingIndicator('ch-1', true);

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('typing');
      expect(sent.data.channel_id).toBe('ch-1');
      expect(sent.data.is_typing).toBe(true);
    });

    it('sendSetStatus builds correct payload', async () => {
      const ws = await connectAndWaitForOpen(service);
      ws.simulateMessage({
        type: 'connected',
        data: {
          client_id: '11111111-1111-4111-8111-111111111111',
          user_id: '22222222-2222-4222-8222-222222222222',
        },
      });

      service.sendSetStatus('dnd');

      const sent = JSON.parse(ws.sentMessages[ws.sentMessages.length - 1]);
      expect(sent.type).toBe('set_status');
      expect(sent.data.status).toBe('dnd');
    });

    it('send warns once and does not transmit when not connected', () => {
      // Source: send() guards on readyState !== OPEN, console.warn's for non-
      // subscription message types, then returns without touching the WS.
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      try {
        service.send({ type: 'test', data: {} });
        expect(warnSpy).toHaveBeenCalledTimes(1);
        expect(warnSpy).toHaveBeenCalledWith(
          expect.stringContaining('Cannot send message'),
          'test'
        );
        // No WebSocket exists yet (connect was never called), so nothing got
        // transmitted — proves the early-return path ran instead of the send path.
        expect((service as any).ws).toBeNull();
      } finally {
        warnSpy.mockRestore();
      }
    });
  });
});

describe('fullJitter', () => {
  it('returns integer in [0, base) for 1000 iterations', () => {
    for (let i = 0; i < 1000; i++) {
      const d = fullJitter(1000);
      expect(Number.isInteger(d)).toBe(true);
      expect(d).toBeGreaterThanOrEqual(0);
      expect(d).toBeLessThan(1000);
    }
  });

  it('returns 0 when base is 0', () => {
    expect(fullJitter(0)).toBe(0);
  });

  it('handles base of 1 (returns 0)', () => {
    for (let i = 0; i < 100; i++) {
      expect(fullJitter(1)).toBe(0);
    }
  });
});

describe('scheduleReconnect attempt cap', () => {
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

  it('drops aggressive mode after MAX_AGGRESSIVE_ATTEMPTS (3) failures', () => {
    const s = service as unknown as {
      aggressiveReconnect: boolean;
      reconnectAttempts: number;
      scheduleReconnect: () => void;
      reconnectTimer: NodeJS.Timeout | null;
      setAggressiveReconnect: (v: boolean) => void;
    };
    s.setAggressiveReconnect(true);
    // Simulate three failed attempts incrementing the counter
    s.reconnectAttempts = 3;
    s.scheduleReconnect();
    // After hitting cap, aggressive flips off and attempts resets to 0
    expect(s.aggressiveReconnect).toBe(false);
    expect(s.reconnectAttempts).toBe(0);
  });

  it('stays aggressive below cap', () => {
    const s = service as unknown as {
      aggressiveReconnect: boolean;
      reconnectAttempts: number;
      scheduleReconnect: () => void;
      setAggressiveReconnect: (v: boolean) => void;
    };
    s.setAggressiveReconnect(true);
    s.reconnectAttempts = 2;
    s.scheduleReconnect();
    expect(s.aggressiveReconnect).toBe(true);
  });
});

describe('standard-mode backoff jitter', () => {
  // Math.random is pinned so fullJitter(base) === floor(rand * base) is a
  // deterministic expectation (per tests.md "no random data"). The assertion
  // is that the STANDARD (non-aggressive) branch now jitters the delay the
  // same way the aggressive branch always has — desyncing a fleet-wide
  // thundering herd against a recovering origin (#769 / origin-502-storm).
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('applies full jitter to the standard (non-aggressive) backoff delay', () => {
    vi.useFakeTimers();
    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout');
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      aggressiveReconnect: boolean;
      reconnectAttempts: number;
      scheduleReconnect: () => void;
    };
    s.aggressiveReconnect = false;
    s.reconnectAttempts = 0; // base = min(1000 * 2^0, 30000) = 1000

    s.scheduleReconnect();

    // Deterministic code armed 1000ms; full jitter at random=0.5 → floor(0.5*1000)=500.
    expect(setTimeoutSpy.mock.calls.at(-1)?.[1]).toBe(500);

    svc.disconnect();
  });

  it('jitters the capped 30s delay to strictly below the ceiling', () => {
    vi.useFakeTimers();
    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout');
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      aggressiveReconnect: boolean;
      reconnectAttempts: number;
      scheduleReconnect: () => void;
    };
    s.aggressiveReconnect = false;
    s.reconnectAttempts = 20; // >= maxReconnectAttempts → base capped at 30000

    s.scheduleReconnect();

    expect(setTimeoutSpy.mock.calls.at(-1)?.[1]).toBe(Math.floor(0.999 * 30000));
    expect(setTimeoutSpy.mock.calls.at(-1)?.[1] as number).toBeLessThan(30000);

    svc.disconnect();
  });
});

describe('offline short-circuit (navigator.onLine gate)', () => {
  // navigator.onLine === false is a reliable "definitely offline" signal; the
  // gate skips the guaranteed-to-fail ws-ticket POST and waits for the browser
  // 'online' event instead of hammering the network on the backoff timer. (It
  // does NOT help the origin-502 case — there the client is online.)
  const setOnLine = (value: boolean) =>
    Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => value });

  afterEach(() => {
    setOnLine(true); // restore jsdom default so sibling suites see "online"
    vi.useRealTimers();
    vi.restoreAllMocks();
    globalThis.fetch = originalFetch;
  });

  it('defers the ws-ticket fetch while offline and reconnects when online fires', async () => {
    vi.useFakeTimers();
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ ticket: 'mock-ticket' }),
    });
    globalThis.fetch = mockFetch;
    setOnLine(false);

    const svc = new WebSocketService('ws://localhost:8080');
    svc.connect('jwt-token');
    await vi.advanceTimersByTimeAsync(0);

    // Offline → no guaranteed-to-fail ticket POST, but armed for recovery.
    expect(mockFetch).not.toHaveBeenCalled();
    expect(svc.getState()).toBe(ConnectionState.RECONNECTING);

    // Connectivity returns → retry fires the ticket fetch.
    setOnLine(true);
    globalThis.dispatchEvent(new Event('online'));
    await vi.advanceTimersByTimeAsync(0);

    expect(mockFetch).toHaveBeenCalledWith(
      expect.stringContaining('/auth/ws-ticket'),
      expect.anything()
    );

    svc.disconnect();
  });

  it('removes the online listener on disconnect (no zombie reconnect)', async () => {
    vi.useFakeTimers();
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ ticket: 'mock-ticket' }),
    });
    globalThis.fetch = mockFetch;
    setOnLine(false);

    const svc = new WebSocketService('ws://localhost:8080');
    svc.connect('jwt-token');
    await vi.advanceTimersByTimeAsync(0);
    expect(mockFetch).not.toHaveBeenCalled();

    svc.disconnect();

    // 'online' fires AFTER disconnect — the listener must have been removed.
    setOnLine(true);
    globalThis.dispatchEvent(new Event('online'));
    await vi.advanceTimersByTimeAsync(0);
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it('still reconnects when navigator.onLine is stuck false and "online" never fires (regression #1768)', async () => {
    // Regression for #1768 (defect A): #1657 added the navigator.onLine
    // short-circuit, which arms a ONE-SHOT 'online' listener and nothing else.
    // In Electron, navigator.onLine can stick `false` across a network flap /
    // server deploy and the 'online' event may never (re)fire — stranding the
    // client in RECONNECTING forever until a manual app restart (the reported
    // "requires a restart" symptom). A bounded fallback MUST still attempt a
    // real reconnect even though 'online' never fires.
    vi.useFakeTimers();
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ ticket: 'mock-ticket' }),
    });
    globalThis.fetch = mockFetch;
    setOnLine(false);

    const svc = new WebSocketService('ws://localhost:8080');
    svc.connect('jwt-token');
    await vi.advanceTimersByTimeAsync(0);

    // Offline gate engaged: no immediate ticket POST, armed for recovery.
    expect(mockFetch).not.toHaveBeenCalled();
    expect(svc.getState()).toBe(ConnectionState.RECONNECTING);

    // 'online' NEVER fires; navigator.onLine stays false the whole time.
    await vi.advanceTimersByTimeAsync(60_000);

    // The client must NOT be stranded — a bounded fallback attempts a real
    // reconnect (the ws-ticket fetch fires) despite the stuck offline signal.
    expect(mockFetch).toHaveBeenCalledWith(
      expect.stringContaining('/auth/ws-ticket'),
      expect.anything()
    );

    svc.disconnect();
  });
});

describe('ticket cache', () => {
  type CacheHarness = {
    token: string | null;
    ticketCache: {
      ticket: string;
      issuedAt: number;
      token: string;
      sessionId: string | null;
    } | null;
    consumeCachedTicket: () => string | null;
  };

  it('consumes cached ticket within TTL for the same token/session', () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as CacheHarness;
    s.token = 'current-token';
    s.ticketCache = {
      ticket: 'cached-ticket-abc',
      issuedAt: Date.now(),
      token: 'current-token',
      sessionId: null,
    };
    const consumed = s.consumeCachedTicket();
    expect(consumed).toBe('cached-ticket-abc');
    // Cache is cleared after consumption (single-use invariant)
    expect(s.ticketCache).toBeNull();
  });

  it('rejects cached ticket from a different token (#1977)', () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as CacheHarness;
    s.token = 'fresh-token';
    s.ticketCache = {
      ticket: 'stale-token-ticket',
      issuedAt: Date.now(),
      token: 'stale-token',
      sessionId: null,
    };
    const consumed = s.consumeCachedTicket();
    expect(consumed).toBeNull();
    expect(s.ticketCache).toBeNull();
  });

  it('rejects cached ticket older than TTL', () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as CacheHarness;
    s.token = 'current-token';
    s.ticketCache = {
      ticket: 'stale-ticket',
      issuedAt: Date.now() - 10_000,
      token: 'current-token',
      sessionId: null,
    };
    const consumed = s.consumeCachedTicket();
    expect(consumed).toBeNull();
    expect(s.ticketCache).toBeNull(); // stale entries cleared too
  });

  it('returns null when cache is empty', () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as { consumeCachedTicket: () => string | null };
    const consumed = s.consumeCachedTicket();
    expect(consumed).toBeNull();
  });
});

describe('connection-ready promise', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('whenConnectionReady resolves when connection_ready frame arrives', async () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      createConnectionReadyPromise: () => void;
      resolveConnectionReady: () => void;
      whenConnectionReady: () => Promise<void>;
    };
    s.createConnectionReadyPromise();
    const p = s.whenConnectionReady();
    s.resolveConnectionReady();
    await expect(p).resolves.toBeUndefined();
  });

  it('whenConnectionReady rejects after CONNECTION_READY_TIMEOUT_MS', async () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      createConnectionReadyPromise: () => void;
      whenConnectionReady: () => Promise<void>;
    };
    s.createConnectionReadyPromise();
    const p = s.whenConnectionReady();
    vi.advanceTimersByTime(5_000);
    await expect(p).rejects.toThrow(/connection_ready/);
  });

  it('rejecting on disconnect allows new promise on next open', async () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      createConnectionReadyPromise: () => void;
      rejectConnectionReady: (err: Error) => void;
      whenConnectionReady: () => Promise<void>;
    };
    s.createConnectionReadyPromise();
    const p1 = s.whenConnectionReady();
    s.rejectConnectionReady(new Error('disconnected'));
    await expect(p1).rejects.toThrow(/disconnected/);

    s.createConnectionReadyPromise();
    const p2 = s.whenConnectionReady();
    expect(p2).not.toBe(p1);
  });

  it('does not produce unhandled rejection when no consumer awaits', async () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      createConnectionReadyPromise: () => void;
      rejectConnectionReady: (err: Error) => void;
    };
    // Track unhandled rejections during this test
    const unhandled: unknown[] = [];
    const handler = (event: PromiseRejectionEvent) => {
      unhandled.push(event.reason);
    };
    globalThis.addEventListener('unhandledrejection', handler);

    s.createConnectionReadyPromise();
    s.rejectConnectionReady(new Error('test rejection'));

    // Give the microtask queue time to fire any unhandled rejection events
    await vi.advanceTimersByTimeAsync(10);
    globalThis.removeEventListener('unhandledrejection', handler);

    expect(unhandled).toHaveLength(0);
  });

  it('clears resolve/reject refs after timeout fires', async () => {
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      createConnectionReadyPromise: () => void;
      whenConnectionReady: () => Promise<void>;
      connectionReadyResolve: (() => void) | null;
      connectionReadyReject: ((err: Error) => void) | null;
      connectionReadyTimer: NodeJS.Timeout | null;
    };
    s.createConnectionReadyPromise();
    const p = s.whenConnectionReady();
    // Attach a no-op handler so the rejection is consumed (test cleanliness)
    p.catch(() => {});
    vi.advanceTimersByTime(5_000);
    await Promise.resolve(); // flush microtask queue
    // After timeout, refs should be cleared
    expect(s.connectionReadyResolve).toBeNull();
    expect(s.connectionReadyReject).toBeNull();
    expect(s.connectionReadyTimer).toBeNull();
  });
});

describe('origin 502 storm recovery', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('recovers within 3 aggressive attempts when origin returns 502 twice then 200', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch');
    let call = 0;
    fetchMock.mockImplementation(async () => {
      call++;
      if (call <= 2) {
        return new Response(null, { status: 502, statusText: 'Bad Gateway' });
      }
      return new Response(JSON.stringify({ ticket: 'ok-ticket-123' }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      });
    });

    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      token: string | null;
      aggressiveReconnect: boolean;
      reconnectAttempts: number;
      createConnection: (signal?: AbortSignal) => Promise<void>;
      setAggressiveReconnect: (v: boolean) => void;
    };
    s.token = 'mock-jwt';
    s.setAggressiveReconnect(true);
    s.reconnectAttempts = 0;

    // Attempt 1 → 502 → catch → scheduleReconnect (aggressive, attempt 0 → base 500ms → jitter[0,500))
    await s.createConnection();
    // Flush any pending microtasks from the failed fetch, then fire the reconnect timer
    await Promise.resolve();
    vi.advanceTimersByTime(500);
    await vi.runOnlyPendingTimersAsync();

    // Attempt 2 → 502 → catch → scheduleReconnect (aggressive, attempt 1 → base 1000ms → jitter[0,1000))
    await Promise.resolve();
    vi.advanceTimersByTime(1_000);
    await vi.runOnlyPendingTimersAsync();

    // Attempt 3 → 200 → ticket obtained → new WebSocket (MockWebSocket; doesn't throw)
    await Promise.resolve();

    // After three attempts, fetch must have been called exactly 3 times.
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});

describe('resubscribeChannels emits probe', () => {
  it('sends connection_ready_probe after all resubscribes', () => {
    vi.useFakeTimers();
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      subscribedChannels: Set<string>;
      subscribedDMs: Set<string>;
      subscribedServers: Set<string>;
      resubscribeChannels: () => void;
      send: (msg: { type: string; data: Record<string, unknown> }) => void;
    };
    // Seed state
    s.subscribedChannels.clear();
    s.subscribedDMs.clear();
    s.subscribedServers.clear();
    s.subscribedChannels.add('ch1');
    s.subscribedChannels.add('ch2');
    s.subscribedDMs.add('dm1');
    s.subscribedServers.add('sv1');

    const sent: Array<{ type: string; data: Record<string, unknown> }> = [];
    const originalSend = s.send.bind(svc);
    s.send = (msg) => {
      sent.push(msg);
      originalSend(msg);
    };

    s.resubscribeChannels();

    const types = sent.map((m) => m.type);
    expect(types).toEqual([
      'subscribe',
      'subscribe',
      'subscribe_server',
      'subscribe_dm',
      'connection_ready_probe',
    ]);
    // Probe payload marks protocol v2
    const probe = sent[sent.length - 1];
    expect(probe.data).toEqual({ protocol_version: 2 });
    vi.useRealTimers();
  });

  it('sends probe even with empty subscribed sets (cold connect)', () => {
    vi.useFakeTimers();
    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      subscribedChannels: Set<string>;
      subscribedDMs: Set<string>;
      subscribedServers: Set<string>;
      resubscribeChannels: () => void;
      send: (msg: { type: string; data: Record<string, unknown> }) => void;
    };
    s.subscribedChannels.clear();
    s.subscribedDMs.clear();
    s.subscribedServers.clear();

    const sent: Array<{ type: string }> = [];
    s.send = (msg) => {
      sent.push({ type: msg.type });
    };

    s.resubscribeChannels();

    expect(sent.map((m) => m.type)).toEqual(['connection_ready_probe']);
    vi.useRealTimers();
  });
});

describe('handleError log level', () => {
  let svc: WebSocketService;
  let s: {
    aggressiveReconnect: boolean;
    reconnectAttempts: number;
    setAggressiveReconnect: (v: boolean) => void;
    handleError: (event: Event) => void;
  };

  beforeEach(() => {
    vi.useFakeTimers();
    svc = new WebSocketService('ws://localhost:8080');
    s = svc as unknown as typeof s;
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  // Mirrors the first-attempt 1006 close pattern observed at cold-start: the
  // aggressive-backoff infrastructure (#769) already absorbs a single CF-edge
  // upgrade drop, so the matching `onerror` event is expected — not a signal
  // worth surfacing as console.error. Downgrading to console.warn quiets the
  // expected bounce while preserving the loud signal when aggressive mode has
  // exhausted its budget. Also avoids logging the WebSocket `event` object
  // directly, which would leak `event.target.url` (containing the ?ticket=
  // query param) into the console.
  it('logs warn (not error) on first-attempt transport drop during aggressive reconnect', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    s.setAggressiveReconnect(true);
    s.reconnectAttempts = 0;

    s.handleError(new Event('error'));

    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('first-attempt transport drop'), {
      type: 'Event',
      event: 'error',
    });
    expect(errorSpy).not.toHaveBeenCalled();
    expect(svc.getState()).toBe(ConnectionState.ERROR);
  });

  it('logs error once aggressive budget exhausted (reconnectAttempts > 0)', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    s.setAggressiveReconnect(true);
    s.reconnectAttempts = 1;

    s.handleError(new Event('error'));

    expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('transport error'), {
      type: 'Event',
      event: 'error',
    });
    expect(warnSpy).not.toHaveBeenCalled();
    expect(svc.getState()).toBe(ConnectionState.ERROR);
  });

  it('logs structured Error details for transport failures', () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    s.setAggressiveReconnect(false);
    s.handleError(new Error('socket failed') as unknown as Event);

    expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('transport error'), {
      type: 'Error',
      message: 'socket failed',
    });
  });

  it('logs structured CloseEvent details for transport failures', () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    s.setAggressiveReconnect(false);
    s.handleError(new CloseEvent('close', { code: 1006, reason: 'abnormal' }));

    expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('transport error'), {
      type: 'CloseEvent',
      event: 'close',
      code: 1006,
      reason: 'abnormal',
    });
  });

  it('logs error when not in aggressive mode (normal exponential reconnect)', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    s.setAggressiveReconnect(false);
    s.reconnectAttempts = 0;

    s.handleError(new Event('error'));

    expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('transport error'), {
      type: 'Event',
      event: 'error',
    });
    expect(warnSpy).not.toHaveBeenCalled();
    expect(svc.getState()).toBe(ConnectionState.ERROR);
  });

  // Locks in the warn → error progression. If a future refactor breaks the
  // order between scheduleReconnect's `reconnectAttempts++` and the next
  // createConnection call (so attempts stays at 0 across multiple errors),
  // this test catches it. Manual increment proxies scheduleReconnect's
  // pre-createConnection bump (websocketService.ts ~line 790).
  it('progresses warn → error across an aggressive-attempt increment', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    s.setAggressiveReconnect(true);
    s.reconnectAttempts = 0;

    s.handleError(new Event('error'));
    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(errorSpy).not.toHaveBeenCalled();

    // Simulate scheduleReconnect's increment-before-next-attempt.
    s.reconnectAttempts = 1;

    s.handleError(new Event('error'));
    expect(warnSpy).toHaveBeenCalledTimes(1); // unchanged
    expect(errorSpy).toHaveBeenCalledTimes(1);
  });
});

describe('createConnection error logging', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  // The createConnection catch block previously logged the raw `error`
  // object. `new WebSocket(url)` synchronously throws a SyntaxError whose
  // message includes the full URL — including the `?ticket=<hex>` query
  // param. console.error('msg:', error) would expose the auth bearer in
  // any log sink that serializes Error objects (including DevTools
  // console output and any future telemetry pipeline). The fix logs
  // error.name (a fixed identifier like 'Error' or 'SyntaxError'),
  // preserving triage signal without leaking the ticket.
  //
  // Test strategy: use a fetch rejection as a proxy. It routes through
  // the same catch block (createConnection awaits the ticket fetch
  // first) and lets us assert the log shape without standing up an
  // exploding-WebSocket constructor harness.
  it('does not log raw Error object in createConnection catch (avoids ticket leak via Error.message)', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    const sensitive = 'sensitive-ticket-abc123';
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(
      new Error(`Network failed for https://x.com?ticket=${sensitive}`)
    );

    const svc = new WebSocketService('ws://localhost:8080');
    const s = svc as unknown as {
      token: string | null;
      createConnection: (signal?: AbortSignal) => Promise<void>;
    };
    s.token = 'mock-jwt';
    await s.createConnection();

    expect(errorSpy).toHaveBeenCalled();
    for (const call of errorSpy.mock.calls) {
      for (const arg of call) {
        // An Error instance would stringify to "Error: <message>", exposing the URL.
        expect(arg).not.toBeInstanceOf(Error);
        if (typeof arg === 'string') {
          expect(arg).not.toContain(sensitive);
          expect(arg).not.toContain('?ticket=');
        }
      }
    }
  });
});

describe('send / handleMessage / notifyConnectionHandlers error logging', () => {
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
    vi.restoreAllMocks();
  });

  it('logs redacted error when ws.send throws', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const ws = await connectAndWaitForOpen(service);
    ws.simulateMessage({
      type: 'connected',
      data: {
        client_id: '11111111-1111-4111-8111-111111111111',
        user_id: '22222222-2222-4222-8222-222222222222',
      },
    });

    // Force the underlying ws.send to throw
    ws.send = () => {
      throw new Error('boom');
    };

    service.send({ type: 'chat_message', data: { text: 'hi' } });

    expect(errorSpy).toHaveBeenCalledWith('Failed to send WebSocket message:', 'boom');
  });

  it('logs redacted error when a message handler throws', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const ws = await connectAndWaitForOpen(service);
    ws.simulateMessage({
      type: 'connected',
      data: {
        client_id: '11111111-1111-4111-8111-111111111111',
        user_id: '22222222-2222-4222-8222-222222222222',
      },
    });

    service.on('subscribed', () => {
      throw new Error('boom');
    });
    ws.simulateMessage({ type: 'subscribed', data: {} });

    expect(errorSpy).toHaveBeenCalledWith('Error in message handler:', 'boom');
  });

  it('logs redacted error when incoming message is invalid JSON', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const ws = await connectAndWaitForOpen(service);
    ws.simulateMessage({
      type: 'connected',
      data: {
        client_id: '11111111-1111-4111-8111-111111111111',
        user_id: '22222222-2222-4222-8222-222222222222',
      },
    });

    // Bypass the simulateMessage helper to deliver non-JSON data directly
    if (ws.onmessage) {
      ws.onmessage(new MessageEvent('message', { data: 'not-valid-json{' }));
    }

    expect(errorSpy).toHaveBeenCalledWith('Failed to parse WebSocket message:', expect.any(String));
  });

  it('logs redacted error when a connection-state handler throws', () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    // onConnectionChange immediately invokes the handler once at registration
    // (outside the notifyConnectionHandlers try/catch). Throw only on the
    // SECOND invocation so we exercise the catch at line 907 specifically.
    let callCount = 0;
    service.onConnectionChange(() => {
      if (callCount++ > 0) throw new Error('boom');
    });
    // Trigger setState → notifyConnectionHandlers (the catch we want)
    service.connect('test-token');

    expect(errorSpy).toHaveBeenCalledWith('Error in connection handler:', 'boom');
  });
});
