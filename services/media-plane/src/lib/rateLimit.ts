/**
 * Per-socket, per-event token-bucket rate limiting for media-plane Socket.IO
 * handlers (#2032).
 *
 * `withRateLimit` IS the registration — it calls `socket.on` internally, so a
 * handler cannot be registered without declaring a budget. Buckets live on
 * `socket.data`, so they are freed with the socket: there is no module-level
 * registry to prune and no timer to clear (refill is lazy, computed on read).
 *
 * This is the socket-scoped sibling of the participant-scoped limiters
 * (`setTestingStatus.ts`, and the keyframe cooldown on `Participant`). Both
 * exist because their lifetimes differ — a participant is destroyed on
 * `leave-room` while the socket survives to join another room.
 */

export interface RateBudget {
  /** Max tokens the bucket holds — the instantaneous burst size. */
  readonly capacity: number;
  /** Tokens restored per second; fractional allowed (0.5 = one per 2 s). */
  readonly refillPerSec: number;
}

export interface TokenBucket {
  tokens: number;
  lastRefillMs: number;
}

/**
 * Injected clock. REQUIRED rather than optional: `[internal]rules/tests.md`
 * forbids timing-dependent tests, so refill must be drivable without real time.
 */
export type Clock = () => number;

/** Every client-invokable room event. `disconnect` is a lifecycle event and is excluded. */
export type RoomEventName =
  | 'join-room'
  | 'update-rtp-capabilities'
  | 'create-transport'
  | 'connect-transport'
  | 'produce'
  | 'consume'
  | 'request-keyframe'
  | 'resume-consumer'
  | 'set-preferred-layers'
  | 'pause-consumer'
  | 'close-consumer'
  | 'close-recv-transport'
  | 'pause-producer'
  | 'resume-producer'
  | 'set-deafen'
  | 'update-test-status'
  | 'close-producer'
  | 'leave-room';

/**
 * Per-event budgets, sized so the scripted join flow never trips (#2032 AC #2):
 * auth -> join-room -> 3x create-transport -> produce mic -> consume N.
 * Events causing a room-wide broadcast are tighter than local-CPU-only events.
 *
 * `Record<RoomEventName, RateBudget>` is exhaustive: adding a member to the
 * union without a budget here is a compile error.
 */
export const EVENT_BUDGETS: Record<RoomEventName, RateBudget> = {
  'join-room': { capacity: 5, refillPerSec: 0.5 },
  'update-rtp-capabilities': { capacity: 5, refillPerSec: 0.5 },
  // 12, not 8. A stock join spends 3; a picture-in-picture window adds a 4th
  // through the same socket, and at capacity 8 that lands EXACTLY on the 50%
  // headroom floor the burst test asserts — passing by equality, with no slack
  // for a second PiP or for open/close churn across a session. The flood
  // ceiling is not what 8 was buying: MAX_RECV_TRANSPORTS_PER_PARTICIPANT (4)
  // plus one send transport is the hard structural bound, so this budget is a
  // coarse rate ceiling sitting above a cap that already holds. Raising it
  // costs no safety and buys real margin.
  'create-transport': { capacity: 12, refillPerSec: 0.5 },
  'connect-transport': { capacity: 12, refillPerSec: 0.5 },
  produce: { capacity: 10, refillPerSec: 1 },
  consume: { capacity: 250, refillPerSec: 25 },
  'request-keyframe': { capacity: 10, refillPerSec: 0.5 },
  'resume-consumer': { capacity: 250, refillPerSec: 25 },
  'set-preferred-layers': { capacity: 200, refillPerSec: 20 },
  'pause-consumer': { capacity: 250, refillPerSec: 25 },
  'close-consumer': { capacity: 250, refillPerSec: 25 },
  'close-recv-transport': { capacity: 10, refillPerSec: 0.5 },
  'pause-producer': { capacity: 20, refillPerSec: 3 },
  'resume-producer': { capacity: 20, refillPerSec: 3 },
  'set-deafen': { capacity: 10, refillPerSec: 1 },
  'update-test-status': { capacity: 20, refillPerSec: 2 },
  'close-producer': { capacity: 20, refillPerSec: 2 },
  'leave-room': { capacity: 5, refillPerSec: 0.5 },
};

/**
 * Used when a lookup misses. Unreachable through TypeScript (the Record is
 * exhaustive), but a type-erased JS call path must degrade RESTRICTIVELY —
 * never to unlimited.
 */
export const FALLBACK_BUDGET: RateBudget = { capacity: 5, refillPerSec: 0.5 };

/**
 * Socket.IO's own listener shape. `DefaultEventsMap` is
 * `Record<string, (...args: any[]) => void>`, so this MUST mirror it: a
 * narrower rest type (`never[]` / `unknown[]`) makes a real `Socket`
 * unassignable to `RateLimitedSocket` under `strictFunctionTypes`, and — worse
 * — contextually types every wrapped handler's parameters as `never`, so
 * `callback(...)` stops being callable at all 18 registration sites. Mirroring
 * the upstream type keeps each handler body type-checking exactly as it did
 * when it was registered through `socket.on` directly.
 *
 * On [internal]'s "no `any` in security-critical code": this is the narrowest
 * available form of that escape. The `any` lives only in the generic
 * CONSTRAINT and the socket shape — `withRateLimit` infers `H` from the real
 * handler, so each body keeps its own parameter types, and the wrapper's own
 * listener takes `unknown[]`. No security decision reads a value typed `any`:
 * `consumeToken`, the budget lookup, and the overflow branch are unchanged.
 * The alternative — narrowing here and casting at all 18 registration sites —
 * would spread `any` across the call sites instead of confining it to one
 * documented type alias.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- mirrors socket.io's DefaultEventsMap listener type; see above.
export type SocketListener = (...args: any[]) => unknown;

export interface RateLimitedSocket {
  data: { userId: string; rateBuckets?: Map<string, TokenBucket> };
  on: (event: string, listener: SocketListener) => unknown;
  emit: (event: string, payload: unknown) => unknown;
}

export interface RateLimitDeps {
  now?: Clock;
  /** PII-safe rejection hook: event name + userId only, never the payload. */
  onReject?: (event: RoomEventName, userId: string) => void;
  /**
   * PII-safe handler-failure hook. Fires when a wrapped handler throws or
   * rejects (see `invokeHandler`).
   *
   * Deliberately the SAME shape as `onReject` — event name + userId, and no
   * error value. The thrown value is not passed across this boundary at all:
   * `[internal]rules/observability.md` forbids `Error.cause` reaching any log
   * sink, and a reporter that never receives the error cannot leak its cause,
   * its message, or the rejected payload the message may quote. Handlers still
   * log their own failures inside their own try/catch; this hook exists to
   * surface the residue those catches missed.
   */
  onHandlerError?: (event: RoomEventName, userId: string) => void;
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as PromiseLike<unknown>).then === 'function'
  );
}

/**
 * Answer the client after a handler failure, without leaking the cause.
 *
 * Deliberately mirrors the overflow branch: ack when the client supplied one,
 * otherwise a typed `error` emit. A silent drop would leave a stock client
 * waiting forever on an ack that can never arrive.
 */
function reportHandlerFailure(
  socket: RateLimitedSocket,
  event: RoomEventName,
  args: readonly unknown[],
  deps: RateLimitDeps
): void {
  // The reporter is injected plumbing and can throw. It sits in its own guard,
  // separate from the response below, so a broken reporter cannot suppress the
  // client's answer — and cannot escape into the crash path this file closes.
  try {
    deps.onHandlerError?.(event, socket.data.userId);
  } catch {
    // Intentionally swallowed; see above.
  }

  const last = args.at(-1);
  try {
    if (typeof last === 'function') {
      (last as unknown as (response: { error: string }) => void)({ error: 'internal_error' });
      return;
    }
    socket.emit('error', { error: 'internal_error', event });
  } catch {
    // The ack is client-supplied plumbing and `emit` can throw on a torn-down
    // socket; a failure here must not re-enter the crash path this guard
    // exists to close.
  }
}

/**
 * Invoke a wrapped handler so that NOTHING it does can terminate the process.
 *
 * socket.io 4.8.3 dispatches listeners inside `process.nextTick`, so a
 * synchronous throw becomes an `uncaughtException` and an async handler's
 * rejection becomes an unhandled rejection — under Node's default
 * `--unhandled-rejections=throw` both are fatal, and `src/` registers no
 * process-level handler. One malformed packet from any authenticated socket
 * would otherwise drop every call on the node (#2032 red-team).
 *
 * The guard belongs HERE for the same reason the budget does: `withRateLimit`
 * IS the registration, so no handler can exist without it and no future call
 * site can forget it.
 */
function invokeHandler(
  socket: RateLimitedSocket,
  event: RoomEventName,
  handler: SocketListener,
  args: readonly unknown[],
  deps: RateLimitDeps
): unknown {
  let result: unknown;
  try {
    result = handler(...args);
  } catch {
    reportHandlerFailure(socket, event, args, deps);
    return undefined;
  }

  if (isThenable(result)) {
    // Attach the rejection handler to the ORIGINAL thenable's continuation, so
    // the rejection is settled here and never reaches the process.
    return Promise.resolve(result).catch(() => {
      reportHandlerFailure(socket, event, args, deps);
    });
  }

  return result;
}

/**
 * Consume one token, refilling lazily first. Returns true when the call is
 * within budget.
 */
function consumeToken(bucket: TokenBucket, budget: RateBudget, nowMs: number): boolean {
  // Math.max guards a backwards system-clock step from crediting tokens.
  const elapsedMs = Math.max(0, nowMs - bucket.lastRefillMs);
  // Math.min is what makes "budgets self-heal, no permanent lockout" structural.
  bucket.tokens = Math.min(
    budget.capacity,
    bucket.tokens + (elapsedMs / 1000) * budget.refillPerSec
  );
  bucket.lastRefillMs = nowMs;

  if (bucket.tokens < 1) return false;
  bucket.tokens -= 1;
  return true;
}

/**
 * Register `handler` for `event` behind a per-socket token bucket.
 *
 * On overflow the wrapped handler is NEVER invoked, so nothing partial is
 * created — a rejected `join-room` never touches SocketRoomClaim and never
 * increments a pending counter. That is what makes it safe to apply uniformly
 * across heterogeneous handlers.
 *
 * Within budget the handler runs under `invokeHandler`, which is what keeps a
 * throwing or rejecting handler from killing the process.
 */
export function withRateLimit<H extends SocketListener>(
  socket: RateLimitedSocket,
  event: RoomEventName,
  handler: H,
  deps: RateLimitDeps = {}
): void {
  const now = deps.now ?? Date.now;
  const budget = EVENT_BUDGETS[event] ?? FALLBACK_BUDGET;

  socket.on(event, (...args: unknown[]) => {
    const buckets = (socket.data.rateBuckets ??= new Map<string, TokenBucket>());
    let bucket = buckets.get(event);
    if (!bucket) {
      bucket = { tokens: budget.capacity, lastRefillMs: now() };
      buckets.set(event, bucket);
    }

    if (consumeToken(bucket, budget, now())) {
      return invokeHandler(socket, event, handler, args, deps);
    }

    // Both blocks below are guarded for the same reason invokeHandler is, and
    // it matters MORE here: this is the path that fires under a flood. An
    // attacker supplies the ack, socket.io dispatches listeners inside
    // process.nextTick, and the service registers no process-level handler — so
    // an unguarded throw on the REJECTION path would let the abuse control
    // itself become the crash vector. Kept as two separate guards so a throwing
    // reporter cannot suppress the client's rejection response.
    try {
      deps.onReject?.(event, socket.data.userId);
    } catch {
      // Intentionally swallowed; see above.
    }

    // Uniform typed rejection. Only join-room has an existing no-callback error
    // path; the other handlers return SILENTLY, which would make an overflow
    // indistinguishable from a dropped packet. The stock client always supplies
    // an ack, so the emit branch is an attacker-only path.
    const last = args.at(-1);
    try {
      if (typeof last === 'function') {
        (last as unknown as (response: { error: string }) => void)({ error: 'rate_limited' });
        return undefined;
      }
      socket.emit('error', { error: 'rate_limited', event });
    } catch {
      // A client-supplied ack that throws, or an emit on a torn-down socket,
      // must not re-enter the crash path. The token is already spent, so the
      // limiter's accounting is unaffected.
    }
    return undefined;
  });
}
