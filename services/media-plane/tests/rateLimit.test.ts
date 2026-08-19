import { describe, it, expect, vi } from 'vitest';

import {
  withRateLimit,
  EVENT_BUDGETS,
  FALLBACK_BUDGET,
  type RateLimitedSocket,
  type RoomEventName,
} from '../src/lib/rateLimit.js';

/** Fake socket capturing registered listeners so tests can invoke them directly. */
function makeSocket(userId = 'user-1') {
  const listeners = new Map<string, (...args: never[]) => unknown>();
  const emit = vi.fn();
  const socket = {
    data: { userId },
    on: (event: string, listener: (...args: never[]) => unknown) => {
      listeners.set(event, listener);
      return socket;
    },
    emit,
  } as unknown as RateLimitedSocket;
  return { socket, listeners, emit };
}

/** Controllable clock — no real time passes in any test. */
function makeClock(startMs = 1_000_000) {
  let now = startMs;
  return {
    now: () => now,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

describe('withRateLimit', () => {
  it('forwards args and the ack to the handler while under budget', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    withRateLimit(socket, 'produce', handler, { now: clock.now });

    const ack = vi.fn();
    await listeners.get('produce')!({ transportId: 't1' } as never, ack as never);

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler).toHaveBeenCalledWith({ transportId: 't1' }, ack);
  });

  it('rejects with a typed ack and does NOT invoke the handler on overflow', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    withRateLimit(socket, 'produce', handler, { now: clock.now });
    const listener = listeners.get('produce')!;

    const budget = EVENT_BUDGETS['produce'];
    for (let i = 0; i < budget.capacity; i += 1) {
      await listener({} as never, vi.fn() as never);
    }
    expect(handler).toHaveBeenCalledTimes(budget.capacity);

    const ack = vi.fn();
    await listener({} as never, ack as never);

    expect(ack).toHaveBeenCalledWith({ error: 'rate_limited' });
    expect(handler).toHaveBeenCalledTimes(budget.capacity); // unchanged
  });

  it('emits a typed error when the overflowing call supplies no ack', async () => {
    const { socket, listeners, emit } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    withRateLimit(socket, 'set-deafen', handler, { now: clock.now });
    const listener = listeners.get('set-deafen')!;

    for (let i = 0; i < EVENT_BUDGETS['set-deafen'].capacity; i += 1) {
      await listener({} as never);
    }
    emit.mockClear();
    await listener({} as never);

    expect(emit).toHaveBeenCalledWith('error', { error: 'rate_limited', event: 'set-deafen' });
    expect(handler).toHaveBeenCalledTimes(EVENT_BUDGETS['set-deafen'].capacity);
  });

  it('self-heals as the clock advances (AC #3 — no permanent lockout)', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    withRateLimit(socket, 'join-room', handler, { now: clock.now });
    const listener = listeners.get('join-room')!;

    const budget = EVENT_BUDGETS['join-room'];
    for (let i = 0; i < budget.capacity; i += 1) await listener({} as never, vi.fn() as never);
    const rejected = vi.fn();
    await listener({} as never, rejected as never);
    expect(rejected).toHaveBeenCalledWith({ error: 'rate_limited' });

    // The handler is a vi.fn() that never invokes its ack, so asserting on the
    // ack here would pass whether or not the bucket refilled — it is never
    // called at all. Assert the HANDLER count, which is what refill actually
    // changes. (CodeRabbit #2793: the previous form was vacuous and still
    // passed with `clock.advance` replaced by a no-op.)
    expect(handler).toHaveBeenCalledTimes(budget.capacity);

    clock.advance(Math.ceil(1000 / budget.refillPerSec));
    const accepted = vi.fn();
    await listener({} as never, accepted as never);
    expect(accepted).not.toHaveBeenCalled();
    expect(handler).toHaveBeenCalledTimes(budget.capacity + 1);
  });

  it('never accumulates beyond capacity no matter how long the socket idles', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    withRateLimit(socket, 'produce', handler, { now: clock.now });
    const listener = listeners.get('produce')!;

    clock.advance(60 * 60 * 1000); // one idle hour

    const budget = EVENT_BUDGETS['produce'];
    for (let i = 0; i < budget.capacity; i += 1) await listener({} as never, vi.fn() as never);
    const ack = vi.fn();
    await listener({} as never, ack as never);

    expect(ack).toHaveBeenCalledWith({ error: 'rate_limited' });
  });

  it('credits nothing when the system clock steps backwards', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    withRateLimit(socket, 'produce', handler, { now: clock.now });
    const listener = listeners.get('produce')!;

    for (let i = 0; i < EVENT_BUDGETS['produce'].capacity; i += 1) {
      await listener({} as never, vi.fn() as never);
    }
    clock.advance(-60_000); // backwards
    const ack = vi.fn();
    await listener({} as never, ack as never);

    expect(ack).toHaveBeenCalledWith({ error: 'rate_limited' });
  });

  it('keeps per-event budgets independent', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const produce = vi.fn();
    const consume = vi.fn();
    withRateLimit(socket, 'produce', produce, { now: clock.now });
    withRateLimit(socket, 'consume', consume, { now: clock.now });

    for (let i = 0; i < EVENT_BUDGETS['produce'].capacity + 5; i += 1) {
      await listeners.get('produce')!({} as never, vi.fn() as never);
    }
    const ack = vi.fn();
    await listeners.get('consume')!({} as never, ack as never);

    expect(consume).toHaveBeenCalledTimes(1);
    expect(ack).not.toHaveBeenCalledWith({ error: 'rate_limited' });
  });

  it('does not share buckets between two sockets', async () => {
    const clock = makeClock();
    const a = makeSocket('user-a');
    const b = makeSocket('user-b');
    const handlerA = vi.fn();
    const handlerB = vi.fn();
    withRateLimit(a.socket, 'produce', handlerA, { now: clock.now });
    withRateLimit(b.socket, 'produce', handlerB, { now: clock.now });

    for (let i = 0; i < EVENT_BUDGETS['produce'].capacity + 5; i += 1) {
      await a.listeners.get('produce')!({} as never, vi.fn() as never);
    }
    const ack = vi.fn();
    await b.listeners.get('produce')!({} as never, ack as never);

    expect(handlerB).toHaveBeenCalledTimes(1);
    expect(ack).not.toHaveBeenCalledWith({ error: 'rate_limited' });
  });

  it('reuses one bucket Map across calls rather than reallocating', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    withRateLimit(socket, 'produce', vi.fn(), { now: clock.now });
    const listener = listeners.get('produce')!;

    await listener({} as never, vi.fn() as never);
    const first = (socket.data as { rateBuckets?: unknown }).rateBuckets;
    await listener({} as never, vi.fn() as never);
    const second = (socket.data as { rateBuckets?: unknown }).rateBuckets;

    expect(first).toBeDefined();
    expect(second).toBe(first);
  });

  it('falls back to the strict budget for an unknown event rather than allowing unlimited', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn();
    // Simulate a type-erased JS caller passing a name absent from EVENT_BUDGETS.
    withRateLimit(socket, 'not-a-real-event' as RoomEventName, handler, { now: clock.now });
    const listener = listeners.get('not-a-real-event')!;

    for (let i = 0; i < FALLBACK_BUDGET.capacity; i += 1) {
      await listener({} as never, vi.fn() as never);
    }
    const ack = vi.fn();
    await listener({} as never, ack as never);

    expect(ack).toHaveBeenCalledWith({ error: 'rate_limited' });
    expect(handler).toHaveBeenCalledTimes(FALLBACK_BUDGET.capacity);
  });

  it('reports rejections through onReject with no payload data', async () => {
    const { socket, listeners } = makeSocket('user-9');
    const clock = makeClock();
    const onReject = vi.fn();
    withRateLimit(socket, 'set-deafen', vi.fn(), { now: clock.now, onReject });
    const listener = listeners.get('set-deafen')!;

    for (let i = 0; i < EVENT_BUDGETS['set-deafen'].capacity; i += 1) {
      await listener({} as never, vi.fn() as never);
    }
    // Field name deliberately not "secret"/"token" — detect-secrets flags those
    // keywords even in test fixtures, and the point here is only that the
    // payload never reaches onReject.
    await listener({ sensitivePayload: 'do-not-log' } as never, vi.fn() as never);

    expect(onReject).toHaveBeenCalledWith('set-deafen', 'user-9');
  });
});

/**
 * #2032 red-team FIX 1. socket.io dispatches listeners inside
 * `process.nextTick`, so an unguarded throw from a handler is an
 * `uncaughtException` and an unguarded rejection is an unhandled rejection —
 * both fatal under Node's default `--unhandled-rejections=throw`, and the
 * media-plane installs no process-level handler. Each test below throws out of
 * the listener (and so fails) against the unguarded `return handler(...args)`.
 */
describe('withRateLimit handler-failure guard (#2032)', () => {
  it('contains a SYNCHRONOUS handler throw and answers the ack', () => {
    const { socket, listeners } = makeSocket('user-9');
    const clock = makeClock();
    const onHandlerError = vi.fn();
    // Shape of the real trigger: `update-rtp-capabilities` destructures in its
    // PARAMETER LIST, outside its own try, so a no-argument emit throws before
    // any handler code runs.
    const handler = vi.fn(({ rtpCapabilities }: { rtpCapabilities: unknown }) => rtpCapabilities);
    withRateLimit(socket, 'update-rtp-capabilities', handler, { now: clock.now, onHandlerError });

    const ack = vi.fn();
    expect(() =>
      listeners.get('update-rtp-capabilities')!(undefined as never, ack as never)
    ).not.toThrow();

    expect(ack).toHaveBeenCalledWith({ error: 'internal_error' });
    expect(onHandlerError).toHaveBeenCalledWith('update-rtp-capabilities', 'user-9');
  });

  it('contains an ASYNC handler rejection and answers the ack', async () => {
    const { socket, listeners } = makeSocket('user-9');
    const clock = makeClock();
    const onHandlerError = vi.fn();
    // Shape of the real trigger: the handler's own catch calls the ack, which
    // is undefined when the client emitted without one — so the catch throws.
    const handler = vi.fn(async () => {
      await Promise.resolve();
      throw new TypeError('callback is not a function');
    });
    withRateLimit(socket, 'consume', handler, { now: clock.now, onHandlerError });

    const ack = vi.fn();
    await listeners.get('consume')!({} as never, ack as never);

    expect(ack).toHaveBeenCalledWith({ error: 'internal_error' });
    expect(onHandlerError).toHaveBeenCalledWith('consume', 'user-9');
  });

  it('emits a typed error when the failing call supplied no ack', async () => {
    const { socket, listeners, emit } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn(() => {
      throw new Error('boom');
    });
    withRateLimit(socket, 'set-deafen', handler, { now: clock.now });

    emit.mockClear();
    await listeners.get('set-deafen')!({} as never);

    expect(emit).toHaveBeenCalledWith('error', { error: 'internal_error', event: 'set-deafen' });
  });

  it('hands the reporter NO error value, so no Error.cause can reach a log sink', async () => {
    const { socket, listeners } = makeSocket('user-9');
    const clock = makeClock();
    const onHandlerError = vi.fn();
    const handler = vi.fn(() => {
      throw new Error('outer', { cause: new Error('inner-secret-material') });
    });
    withRateLimit(socket, 'produce', handler, { now: clock.now, onHandlerError });

    await listeners.get('produce')!({} as never, vi.fn() as never);

    expect(onHandlerError).toHaveBeenCalledTimes(1);
    expect(onHandlerError.mock.calls[0]).toEqual(['produce', 'user-9']);
  });

  it('survives an ack that itself throws', () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn(() => {
      throw new Error('boom');
    });
    withRateLimit(socket, 'produce', handler, { now: clock.now });

    const hostileAck = vi.fn(() => {
      throw new Error('hostile ack');
    });
    expect(() => listeners.get('produce')!({} as never, hostileAck as never)).not.toThrow();
    expect(hostileAck).toHaveBeenCalledTimes(1);
  });

  it('still returns a resolving handler result untouched', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const handler = vi.fn(async () => 'ok');
    withRateLimit(socket, 'produce', handler, { now: clock.now });

    await expect(listeners.get('produce')!({} as never, vi.fn() as never)).resolves.toBe('ok');
  });

  // The rejection path fires under a flood and every call on it is
  // throw-capable: the reporter is injected plumbing, the ack is
  // client-supplied, and emit can hit a torn-down socket. socket.io dispatches
  // listeners inside process.nextTick and this service registers no
  // process-level handler, so an escape here would make the abuse control
  // itself the crash vector. (CodeRabbit #2793.)
  it('survives a rate-limited ack that itself throws', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    withRateLimit(socket, 'set-deafen', vi.fn(), { now: clock.now });
    const listener = listeners.get('set-deafen')!;

    for (let i = 0; i < EVENT_BUDGETS['set-deafen'].capacity; i += 1) {
      await listener({} as never, vi.fn() as never);
    }
    const hostileAck = vi.fn(() => {
      throw new Error('client ack exploded');
    });

    expect(() => listener({} as never, hostileAck as never)).not.toThrow();
    expect(hostileAck).toHaveBeenCalledTimes(1);
  });

  it('survives a throwing onReject and still answers the client', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const onReject = vi.fn(() => {
      throw new Error('reporter exploded');
    });
    withRateLimit(socket, 'set-deafen', vi.fn(), { now: clock.now, onReject });
    const listener = listeners.get('set-deafen')!;

    for (let i = 0; i < EVENT_BUDGETS['set-deafen'].capacity; i += 1) {
      await listener({} as never, vi.fn() as never);
    }
    const ack = vi.fn();
    expect(() => listener({} as never, ack as never)).not.toThrow();

    // A broken reporter must not suppress the client's rejection.
    expect(ack).toHaveBeenCalledWith({ error: 'rate_limited' });
  });

  it('survives a throwing onHandlerError', async () => {
    const { socket, listeners } = makeSocket();
    const clock = makeClock();
    const onHandlerError = vi.fn(() => {
      throw new Error('reporter exploded');
    });
    const handler = vi.fn(() => {
      throw new Error('handler exploded');
    });
    withRateLimit(socket, 'set-deafen', handler, { now: clock.now, onHandlerError });
    const listener = listeners.get('set-deafen')!;

    const ack = vi.fn();
    expect(() => listener({} as never, ack as never)).not.toThrow();
    expect(ack).toHaveBeenCalledWith({ error: 'internal_error' });
  });
});
