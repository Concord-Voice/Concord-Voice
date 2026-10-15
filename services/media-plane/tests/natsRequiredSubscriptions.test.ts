import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';

vi.mock('@/config/index.js', () => ({ config: { natsUrl: 'nats://localhost:4222' } }));

const { mockNc, mockConnect } = vi.hoisted(() => {
  const mockNc = {
    subscribe: vi.fn(),
    status: vi.fn(),
    flush: vi.fn(),
    drain: vi.fn(),
    close: vi.fn(),
    isClosed: vi.fn(),
  };
  return { mockNc, mockConnect: vi.fn() };
});

vi.mock('nats', () => ({
  connect: (...args: unknown[]) => mockConnect(...args),
  JSONCodec: () => ({
    encode: (data: unknown) => JSON.stringify(data),
    decode: (data: string) => JSON.parse(data),
  }),
}));

import { NatsService } from '../src/lib/nats.js';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}

class AsyncQueue<T> implements AsyncIterable<T> {
  private readonly values: T[] = [];
  private nextValue: ((value: IteratorResult<T>) => void) | undefined;
  private ended = false;

  push(value: T): void {
    if (this.nextValue) {
      const next = this.nextValue;
      this.nextValue = undefined;
      next({ done: false, value });
    } else {
      this.values.push(value);
    }
  }

  end(): void {
    this.ended = true;
    this.nextValue?.({ done: true, value: undefined });
    this.nextValue = undefined;
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: () => {
        const value = this.values.shift();
        if (value !== undefined) return Promise.resolve({ done: false, value });
        if (this.ended) return Promise.resolve({ done: true, value: undefined });
        return new Promise((resolve) => {
          this.nextValue = resolve;
        });
      },
    };
  }
}

interface Message {
  data: string;
  reply?: string;
  respond?: ReturnType<typeof vi.fn>;
}

function subscription() {
  const messages = new AsyncQueue<Message>();
  const closing = deferred<void>();
  let closed = false;
  const end = () => {
    closed = true;
    closing.resolve();
    messages.end();
  };
  return {
    closed: closing.promise,
    isClosed: () => closed,
    unsubscribe: vi.fn(end),
    end,
    messages,
    [Symbol.asyncIterator]: () => messages[Symbol.asyncIterator](),
  };
}

type Subscription = ReturnType<typeof subscription>;
type SubscriptionKind = 'core' | 'request';
const coreSubject = 'voice.enforce.mute';
const requestSubject = 'voice.enforce.disconnect.ack';

describe('NatsService required subscriptions', () => {
  let service: NatsService;
  let statuses: AsyncQueue<{ type: string; data: string }>;
  let handles: Map<string, Subscription[]>;

  beforeEach(() => {
    vi.useFakeTimers();
    service = new NatsService();
    statuses = new AsyncQueue();
    handles = new Map();
    mockConnect.mockReset().mockResolvedValue(mockNc);
    mockNc.status.mockReset().mockReturnValue(statuses);
    mockNc.isClosed.mockReset().mockReturnValue(false);
    mockNc.flush.mockReset().mockResolvedValue(undefined);
    mockNc.drain.mockReset().mockImplementation(async () => statuses.end());
    mockNc.close.mockReset().mockResolvedValue(undefined);
    mockNc.subscribe.mockReset().mockImplementation((subject: string) => {
      const handle = subscription();
      handles.set(subject, [...(handles.get(subject) ?? []), handle]);
      return handle;
    });
  });

  afterEach(async () => {
    await service.close();
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  function latest(subject: string): Subscription {
    const handle = handles.get(subject)?.at(-1);
    expect(handle).toBeDefined();
    return handle!;
  }

  function register(kind: SubscriptionKind, handler = vi.fn()): Subscription {
    if (kind === 'core') {
      service.subscribe(coreSubject, handler, { required: true });
      return latest(coreSubject);
    }
    service.subscribeRequest(
      requestSubject,
      async (data) => {
        await handler(data);
        return { ok: true };
      },
      { required: true }
    );
    return latest(requestSubject);
  }

  async function settle(): Promise<void> {
    await vi.advanceTimersByTimeAsync(0);
  }

  async function status(type: string): Promise<void> {
    statuses.push({ type, data: 'nats://localhost:4222' });
    await settle();
  }

  async function confirmedPair(): Promise<void> {
    await service.connect();
    register('core');
    register('request');
    await service.confirmRequiredSubscriptions();
    expect(service.requiredSubscriptionsReady()).toBe(true);
  }

  it('never confirms an empty required set', async () => {
    expect(service.requiredSubscriptionsReady()).toBe(false);
    await service.connect();
    await expect(service.confirmRequiredSubscriptions()).rejects.toThrow();
    expect(service.requiredSubscriptionsReady()).toBe(false);
  });

  it('waits for the broker flush with every required core and request handle installed', async () => {
    await service.connect();
    register('core');
    register('request');
    const flush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(flush.promise);
    const observer = vi.fn();
    service.observeRequiredSubscriptions(observer);

    const confirmation = service.confirmRequiredSubscriptions();
    await settle();
    expect(mockNc.flush).toHaveBeenCalledTimes(1);
    expect(service.requiredSubscriptionsReady()).toBe(false);

    flush.resolve();
    await confirmation;
    expect(service.requiredSubscriptionsReady()).toBe(true);
    expect(observer).toHaveBeenLastCalledWith(true);
    expect(mockNc.subscribe.mock.calls.map(([subject]) => subject)).toEqual([
      coreSubject,
      requestSubject,
    ]);
  });

  it('requires a fresh broker barrier after another required registration', async () => {
    await service.connect();
    register('core');
    await service.confirmRequiredSubscriptions();
    expect(service.requiredSubscriptionsReady()).toBe(true);

    register('request');
    expect(service.requiredSubscriptionsReady()).toBe(false);
    const flush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(flush.promise);
    const confirmation = service.confirmRequiredSubscriptions();
    await settle();
    expect(service.requiredSubscriptionsReady()).toBe(false);
    flush.resolve();
    await confirmation;
    expect(service.requiredSubscriptionsReady()).toBe(true);
  });

  it('refuses a partial required set when the broker rejects a required request subscription', async () => {
    await service.connect();
    register('core');
    mockNc.subscribe.mockImplementation((subject: string) => {
      if (subject === requestSubject) throw new Error('subscription denied');
      return latest(coreSubject);
    });

    expect(() => register('request')).toThrow('subscription denied');
    await expect(service.confirmRequiredSubscriptions()).rejects.toThrow();
    expect(service.requiredSubscriptionsReady()).toBe(false);
  });

  it.each<SubscriptionKind>(['core', 'request'])(
    'invalidates an ended required %s handle while its handler is still awaiting',
    async (kind) => {
      await service.connect();
      const handling = deferred<void>();
      const handler = vi.fn(() => handling.promise);
      const handle = register(kind, handler);
      const observer = vi.fn();
      service.observeRequiredSubscriptions(observer);
      await service.confirmRequiredSubscriptions();
      const respond = vi.fn();
      handle.messages.push({
        data: JSON.stringify({ action: 'mute' }),
        reply: '_INBOX.test',
        respond,
      });
      await settle();
      expect(handler).toHaveBeenCalledTimes(1);

      handle.end();
      expect(service.requiredSubscriptionsReady()).toBe(false);
      await settle();
      expect(observer).toHaveBeenLastCalledWith(false);
      handling.resolve();
      await settle();
      expect(service.requiredSubscriptionsReady()).toBe(false);
      expect(respond).not.toHaveBeenCalled();
    }
  );

  it.each<SubscriptionKind>(['core', 'request'])(
    'keeps a required %s subscriber usable after a handler rejects',
    async (kind) => {
      await service.connect();
      const handler = vi.fn().mockRejectedValueOnce(new Error('effect unavailable'));
      const handle = register(kind, handler);
      await service.confirmRequiredSubscriptions();
      const firstReply = vi.fn();
      const secondReply = vi.fn();
      handle.messages.push({ data: '{}', reply: '_INBOX.first', respond: firstReply });
      handle.messages.push({ data: '{}', reply: '_INBOX.second', respond: secondReply });
      await settle();

      expect(handler).toHaveBeenCalledTimes(2);
      expect(service.requiredSubscriptionsReady()).toBe(true);
      expect(firstReply).not.toHaveBeenCalled();
      if (kind === 'request') expect(secondReply).toHaveBeenCalledTimes(1);
    }
  );

  it('bounds a hung confirmation to 2000 ms and ignores its late flush completion', async () => {
    await service.connect();
    register('core');
    const flush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(flush.promise);
    const outcome = vi.fn();
    const confirmation = service.confirmRequiredSubscriptions().then(
      () => outcome('resolved'),
      () => outcome('rejected')
    );

    await vi.advanceTimersByTimeAsync(1999);
    expect(outcome).not.toHaveBeenCalled();
    expect(service.requiredSubscriptionsReady()).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await confirmation;
    expect(outcome).toHaveBeenCalledWith('rejected');

    flush.resolve();
    await settle();
    expect(service.requiredSubscriptionsReady()).toBe(false);
  });

  it.each(['rejection', 'timeout', 'synchronous throw'])(
    'invalidates an already confirmed set when a fresh broker barrier fails by %s',
    async (failure) => {
      await confirmedPair();
      const observer = vi.fn();
      service.observeRequiredSubscriptions(observer);
      expect(observer).toHaveBeenLastCalledWith(true);
      const flush = deferred<void>();
      if (failure === 'synchronous throw') {
        mockNc.flush.mockImplementationOnce(() => {
          throw new Error('flush unavailable');
        });
      } else {
        mockNc.flush.mockReturnValueOnce(flush.promise);
      }
      const outcome = vi.fn();
      const confirmation = service.confirmRequiredSubscriptions().then(
        () => outcome('resolved'),
        () => outcome('rejected')
      );

      if (failure === 'rejection') flush.reject(new Error('flush unavailable'));
      if (failure === 'timeout') await vi.advanceTimersByTimeAsync(2000);
      else await settle();
      await confirmation;

      expect(outcome).toHaveBeenCalledExactlyOnceWith('rejected');
      expect(service.requiredSubscriptionsReady()).toBe(false);
      expect(observer).toHaveBeenLastCalledWith(false);
      expect(mockNc.flush).toHaveBeenCalledTimes(2);
      if (failure === 'timeout') {
        flush.resolve();
        await settle();
        expect(service.requiredSubscriptionsReady()).toBe(false);
      }
    }
  );

  it('keeps one raw flush across retry deadlines until it settles, then requires a fresh barrier', async () => {
    await confirmedPair();
    await status('disconnect');
    const flush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(flush.promise);
    await status('reconnect');

    for (let deadline = 0; deadline < 4; deadline++) {
      await vi.advanceTimersByTimeAsync(2000);
      expect(mockNc.flush).toHaveBeenCalledTimes(2);
      expect(service.requiredSubscriptionsReady()).toBe(false);
    }

    flush.resolve();
    await settle();
    expect(service.requiredSubscriptionsReady()).toBe(false);
    expect(mockNc.flush).toHaveBeenCalledTimes(2);
    await service.confirmRequiredSubscriptions();
    expect(mockNc.flush).toHaveBeenCalledTimes(3);
    expect(service.requiredSubscriptionsReady()).toBe(true);
  });

  it('reconfirms live subscriptions on reconnect without duplicate broker subscriptions', async () => {
    await confirmedPair();
    await status('disconnect');
    expect(service.requiredSubscriptionsReady()).toBe(false);
    const flush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(flush.promise);

    await status('reconnect');
    expect(service.requiredSubscriptionsReady()).toBe(false);
    expect(mockNc.flush).toHaveBeenCalledTimes(2);
    expect(mockNc.subscribe).toHaveBeenCalledTimes(2);
    flush.resolve();
    await settle();
    expect(service.requiredSubscriptionsReady()).toBe(true);
    expect(mockNc.subscribe).toHaveBeenCalledTimes(2);
  });

  it('reinstalls only the closed required handle when reconnecting', async () => {
    await confirmedPair();
    await status('disconnect');
    latest(coreSubject).end();
    await settle();

    await status('reconnect');
    expect(service.requiredSubscriptionsReady()).toBe(true);
    expect(handles.get(coreSubject)).toHaveLength(2);
    expect(handles.get(requestSubject)).toHaveLength(1);
  });

  it('never restores readiness from a flush started before a newer disconnect', async () => {
    await confirmedPair();
    const observer = vi.fn();
    service.observeRequiredSubscriptions(observer);
    await status('disconnect');
    const staleFlush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(staleFlush.promise);
    await status('reconnect');
    await status('disconnect');

    staleFlush.resolve();
    await settle();
    expect(service.isConnected()).toBe(false);
    expect(service.requiredSubscriptionsReady()).toBe(false);
    expect(observer).toHaveBeenLastCalledWith(false);

    await status('reconnect');
    expect(service.requiredSubscriptionsReady()).toBe(true);
  });

  it('serializes reconnect confirmation and retries a failed barrier once after 2000 ms', async () => {
    await confirmedPair();
    await status('disconnect');
    const failedFlush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(failedFlush.promise);
    await status('reconnect');
    await status('reconnect');
    await status('reconnect');
    expect(mockNc.flush).toHaveBeenCalledTimes(2);

    failedFlush.reject(new Error('flush unavailable'));
    await settle();
    expect(service.requiredSubscriptionsReady()).toBe(false);
    await vi.advanceTimersByTimeAsync(1999);
    expect(mockNc.flush).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(mockNc.flush).toHaveBeenCalledTimes(3);
    expect(service.requiredSubscriptionsReady()).toBe(true);
    await vi.advanceTimersByTimeAsync(6000);
    expect(mockNc.flush).toHaveBeenCalledTimes(3);
  });

  it('keeps the confirmed required set ready when an optional subscriber ends', async () => {
    await confirmedPair();
    service.subscribe('ops.optional.test', vi.fn());
    latest('ops.optional.test').end();
    await settle();
    expect(service.requiredSubscriptionsReady()).toBe(true);
  });

  it('cannot restore readiness or create recovery work after closing during confirmation', async () => {
    await service.connect();
    register('core');
    const flush = deferred<void>();
    mockNc.flush.mockReturnValueOnce(flush.promise);
    const outcome = vi.fn();
    const confirmation = service.confirmRequiredSubscriptions().then(
      () => outcome('resolved'),
      () => outcome('rejected')
    );
    await settle();
    await service.close();
    flush.resolve();
    await settle();
    await confirmation;
    expect(outcome).toHaveBeenCalledWith('rejected');
    expect(service.requiredSubscriptionsReady()).toBe(false);
    await vi.advanceTimersByTimeAsync(6000);
    expect(mockNc.flush).toHaveBeenCalledTimes(1);
    expect(mockNc.subscribe).toHaveBeenCalledTimes(1);
  });

  it('rejects a pending first connect on close and closes its late connection without adopting it', async () => {
    const dial = deferred<typeof mockNc>();
    mockConnect.mockReturnValueOnce(dial.promise);
    const outcome = vi.fn();
    const connecting = service.connect().then(
      () => outcome('resolved'),
      () => outcome('rejected')
    );
    await settle();
    expect(mockConnect).toHaveBeenCalledTimes(1);
    expect(service.isConnected()).toBe(false);

    await service.close();
    await settle();
    await connecting;
    expect(outcome).toHaveBeenCalledExactlyOnceWith('rejected');
    expect(mockNc.close).not.toHaveBeenCalled();

    dial.resolve(mockNc);
    await settle();
    expect(mockNc.close).toHaveBeenCalledTimes(1);
    expect(mockNc.status).not.toHaveBeenCalled();
    expect(mockNc.drain).not.toHaveBeenCalled();
    expect(service.isConnected()).toBe(false);
    expect(service.requiredSubscriptionsReady()).toBe(false);
  });

  it.each(['confirmed', 'recovering'])(
    'stops recovery work from a %s set while preserving live release subscribers',
    async (state) => {
      await confirmedPair();
      if (state === 'confirmed') {
        service.stopRequiredSubscriptionRecovery();
        expect(service.requiredSubscriptionsReady()).toBe(true);
      }
      await status('disconnect');
      latest(coreSubject).end();
      if (state === 'recovering') service.stopRequiredSubscriptionRecovery();
      await status('reconnect');
      await vi.advanceTimersByTimeAsync(6000);

      expect(service.requiredSubscriptionsReady()).toBe(false);
      expect(mockNc.flush).toHaveBeenCalledTimes(1);
      expect(mockNc.subscribe).toHaveBeenCalledTimes(2);
    }
  );

  it('contains observer errors and lets an observer unsubscribe', async () => {
    await service.connect();
    register('core');
    const observer = vi.fn();
    const stopObserving = service.observeRequiredSubscriptions(observer);
    service.observeRequiredSubscriptions(() => {
      throw new Error('observer unavailable');
    });
    await service.confirmRequiredSubscriptions();
    expect(service.requiredSubscriptionsReady()).toBe(true);
    expect(observer).toHaveBeenLastCalledWith(true);
    const observed = observer.mock.calls.length;
    stopObserving();
    await status('disconnect');
    expect(observer).toHaveBeenCalledTimes(observed);
  });
});
