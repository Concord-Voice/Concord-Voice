import { describe, expect, it, vi } from 'vitest';

import {
  EVENT_BUDGETS,
  withRateLimit,
  type RateLimitedSocket,
  type RoomEventName,
  type SocketListener,
  type TokenBucket,
} from '../src/lib/rateLimit.js';

/**
 * Join-flow burst integration test (#2032 AC #2).
 *
 * A scripted LEGITIMATE join must never trip a rate budget. This is the only
 * artifact proving the rate layer does not fight the real client's startup
 * burst, which arrives back-to-back with effectively zero elapsed time.
 *
 * Determinism (`[internal]rules/tests.md`): the clock is injected and FROZEN, so
 * every bucket refills by exactly zero for the whole flow. No real delay is
 * awaited and no timing is observed — the numbers below are arithmetic, not a
 * race.
 */

/** Frozen wall clock. Every `consumeToken` sees `elapsedMs === 0`. */
const FROZEN_NOW_MS = 1_700_000_000_000;
const FROZEN_CLOCK = () => FROZEN_NOW_MS;

/**
 * Remote producers the joiner subscribes to — a realistic mid-size room.
 *
 * Deliberately NOT the 1000-participant admission ceiling: spec §7 notes that
 * ceiling already exceeds what one mediasoup worker can serve, so sizing the
 * burst to it would assert against a room that cannot exist.
 */
const REMOTE_PRODUCERS = 40;

/**
 * `sendTransport` + `recvTransportAudio` + `recvTransportVideo` — what
 * `voiceService.ts` creates on a stock desktop join.
 */
const STOCK_TRANSPORTS = 3;

/**
 * A PiP window opens a FOURTH transport on the same socket
 * (`pipSignalingProxy.ts:231` creates it, `:237` connects it), so it lands in
 * the same per-socket buckets as the main window's three.
 */
const PIP_TRANSPORTS = 4;

/** The minimum fraction of each budget a legitimate join must leave behind. */
const HEADROOM_FLOOR = 0.5;

/** What a wrapped handler acks on success; the wrapper acks `{ error }`. */
interface Ack {
  ok?: true;
  event?: RoomEventName;
  error?: string;
}

type JoinStep = readonly [RoomEventName, number];
type JoinScript = readonly JoinStep[];

/**
 * The scripted flow, in emission order. `update-rtp-capabilities` is included
 * because `voiceService.ts:2950` emits it during the same startup burst.
 */
function joinFlowScript(transportCount: number): JoinScript {
  return [
    ['join-room', 1],
    ['update-rtp-capabilities', 1],
    ['create-transport', transportCount],
    ['connect-transport', transportCount],
    ['produce', 1], // the mic
    ['consume', REMOTE_PRODUCERS],
    ['resume-consumer', REMOTE_PRODUCERS],
  ];
}

function makeSocket(userId = 'joiner-1') {
  const listeners = new Map<string, SocketListener>();
  const emit = vi.fn();
  const socket: RateLimitedSocket = {
    data: { userId },
    on: (event: string, listener: SocketListener) => {
      listeners.set(event, listener);
      return socket;
    },
    emit,
  };
  return { socket, listeners, emit };
}

interface FlowResult {
  socket: RateLimitedSocket;
  emit: ReturnType<typeof vi.fn>;
  acks: Ack[];
  handlerCalls: RoomEventName[];
  script: JoinScript;
  expectedCalls: number;
}

/**
 * Register every event in the script behind `withRateLimit`, then drive the
 * whole flow back-to-back against the frozen clock.
 *
 * The stub handlers ack a success shape, which is what makes assertions (1)
 * and (2) falsifiable: if a budget were tripped, the wrapper would ack
 * `{ error: 'rate_limited' }` INSTEAD and the handler would not run at all,
 * so both the ack shape and the handler-call count would change.
 */
async function runJoinFlow(transportCount: number): Promise<FlowResult> {
  const { socket, listeners, emit } = makeSocket();
  const script = joinFlowScript(transportCount);
  const acks: Ack[] = [];
  const handlerCalls: RoomEventName[] = [];

  for (const [event] of script) {
    withRateLimit(
      socket,
      event,
      (_payload: unknown, ack?: (response: Ack) => void) => {
        handlerCalls.push(event);
        ack?.({ ok: true, event });
      },
      { now: FROZEN_CLOCK }
    );
  }

  let expectedCalls = 0;
  for (const [event, count] of script) {
    const listener = listeners.get(event);
    expect(listener, `withRateLimit registered no listener for ${event}`).toBeDefined();
    for (let i = 0; i < count; i += 1) {
      expectedCalls += 1;
      await listener?.({}, (response: Ack) => acks.push(response));
    }
  }

  return { socket, emit, acks, handlerCalls, script, expectedCalls };
}

function remainingFraction(socket: RateLimitedSocket, event: RoomEventName): number {
  const bucket = socket.data.rateBuckets?.get(event);
  expect(bucket, `no bucket was ever allocated for ${event}`).toBeDefined();
  return (bucket as TokenBucket).tokens / EVENT_BUDGETS[event].capacity;
}

/**
 * Assertion (3), and the one that matters. A burst test asserting only "no
 * rejection" passes with a single token to spare and gives zero warning before
 * the next tuning change breaks production joins. Assert the MARGIN, and name
 * the event plus its remaining percentage so a regression is diagnosable from
 * the failure line alone.
 */
function expectHeadroom(socket: RateLimitedSocket, script: JoinScript): void {
  expect(socket.data.rateBuckets, 'withRateLimit never allocated a bucket map').toBeDefined();

  for (const [event, count] of script) {
    const remaining = remainingFraction(socket, event);
    const { capacity } = EVENT_BUDGETS[event];
    expect(
      remaining,
      `${event}: ${(remaining * 100).toFixed(1)}% of its ${capacity}-token budget remains after ` +
        `${count} legitimate call(s) — below the ${HEADROOM_FLOOR * 100}% floor, so a real join is ` +
        `approaching this budget's ceiling`
    ).toBeGreaterThanOrEqual(HEADROOM_FLOOR);
  }
}

function expectCleanFlow(result: FlowResult): void {
  // Non-vacuity guard: a burst test that never actually exercised a budget is
  // worse than no test. Every scripted call must have reached its handler and
  // produced exactly one ack.
  expect(result.handlerCalls).toHaveLength(result.expectedCalls);
  expect(result.acks).toHaveLength(result.expectedCalls);

  // (1) Nothing was rate-limited.
  expect(result.acks.filter((ack) => ack.error === 'rate_limited')).toEqual([]);

  // (2) Every ack is a success shape.
  expect(result.acks.filter((ack) => ack.ok !== true || ack.error !== undefined)).toEqual([]);

  // The wrapper's no-ack rejection path emits `error` on the socket; a clean
  // flow never reaches it.
  expect(result.emit).not.toHaveBeenCalled();
}

describe('scripted legitimate join flow (#2032 AC #2)', () => {
  it('completes with zero rate-limit rejections and >=50% headroom in every bucket', async () => {
    const result = await runJoinFlow(STOCK_TRANSPORTS);

    expectCleanFlow(result);
    expectHeadroom(result.socket, result.script);
  });

  it('still holds when a PiP window adds a fourth transport on the same socket', async () => {
    // create-transport/connect-transport are capacity 12, so a PiP join spends 4
    // of each and retains ~67%. They were 8 — exactly the 50% floor, passing by
    // equality with no slack for a second PiP or open/close churn — and were
    // raised for that reason. This case is what would fail first if either
    // budget were lowered again, before production felt it.
    const result = await runJoinFlow(PIP_TRANSPORTS);

    expectCleanFlow(result);
    expectHeadroom(result.socket, result.script);
  });

  it('leaves every non-join budget untouched, so the burst cannot mask a leak', async () => {
    const result = await runJoinFlow(STOCK_TRANSPORTS);
    const exercised = new Set<string>(result.script.map(([event]) => event));

    for (const event of Object.keys(EVENT_BUDGETS) as RoomEventName[]) {
      if (exercised.has(event)) continue;
      expect(
        result.socket.data.rateBuckets?.get(event),
        `${event} is not part of the join flow but a bucket was consumed for it`
      ).toBeUndefined();
    }
  });
});
