/**
 * `src/main/audiocapChild.ts` — the utilityProcess CHILD (#3195, plan Task 4).
 *
 * Grown from `audiocapSmokeChild.ts`: this file both migrates that module's D5
 * process-boundary security tests (see the migration table at the bottom) and
 * pins the behaviour Task 4 adds on top of it — staged fault reporting instead
 * of a bare exit, and the credit-gated drain loop (spec §4a/§4b/§4d).
 *
 * HARNESS SHAPE, stated up front because it is not obvious from the assertions:
 *
 * - `process.type` is pinned per test (mirrors `audiocapSmokeChild.test.ts`'s
 *   `loadAs` helper) so the D5 guard can be driven both ways.
 * - The native addon is never the real `.node` binary. `audiocapChild.ts` is
 *   expected to carry forward `audiocapSmokeChild.ts`'s `createRequire(__filename)`
 *   loader (plan Task 4 Step 3), which is a genuine Node CJS `require` bound to
 *   the file URL -- it resolves through Node's own CJS loader, not through
 *   Vite/Vitest's module graph, so a plain `vi.mock('../../../native/concord-audiocap')`
 *   cannot intercept it (confirmed empirically before writing this file: the
 *   bare specifier mock is silently never consulted). `vi.mock('node:module', ...)`
 *   intercepts `createRequire` ITSELF instead, which is import-graph-visible and
 *   therefore interceptable, and hands back a fake `require` this file controls
 *   per test via the mutable `addonRequireImpl` ref.
 * - `process.parentPort` is a hand-rolled stand-in exposing `postMessage`/`on`,
 *   matching the shape `audiocapHost.ts` drives from the other end.
 * - The PCM port is a REAL `MessageChannel()` (confirmed available under this
 *   project's jsdom Vitest environment). One end goes to the child via the
 *   `ports` array on a `parentPort` `'message'` event carrying the `start`
 *   control message -- the wire tables in spec §4a/§4b do not nail down HOW the
 *   port crosses from host to child, only that it does (§6b: "capturing: start
 *   sent, port pair transferred"). Bundling it with `start` is this file's
 *   assumption, recorded here and in the handoff report so Task 4's author can
 *   confirm or correct it without archaeology.
 *
 * Every assertion pairs "value passed" with "value obeyed" per
 * `[internal]rules/tests.md`: a fault stage is checked against the EXACT posted
 * message shape (not merely `toMatchObject`'s partial match plus a
 * `posted.length` check elsewhere), and the D5 guard's throw is paired with
 * proof that nothing was posted before it fired.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { settled } from '../../helpers/settled';

// ---------------------------------------------------------------------------
// node:module interception -- see file header. Must be declared at module
// scope; vi.mock is hoisted above imports by the Vitest transform.
// ---------------------------------------------------------------------------

let addonRequireImpl: (specifier: string) => unknown = () => {
  throw new Error('audiocapChild.test.ts: addonRequireImpl not configured for this case');
};

vi.mock('node:module', () => {
  const createRequire = () => (specifier: string) => addonRequireImpl(specifier);
  return { createRequire, default: { createRequire } };
});

// ---------------------------------------------------------------------------
// process.type pinning (mirrors audiocapSmokeChild.test.ts's `loadAs`)
// ---------------------------------------------------------------------------

let typeDescriptor: PropertyDescriptor | undefined;
let hadType = false;

function setProcessType(type: string): void {
  Object.defineProperty(process, 'type', { value: type, configurable: true });
}

function restoreProcessType(): void {
  if (hadType && typeDescriptor) Object.defineProperty(process, 'type', typeDescriptor);
  else delete (process as { type?: string }).type;
}

beforeEach(() => {
  hadType = Object.prototype.hasOwnProperty.call(process, 'type');
  typeDescriptor = Object.getOwnPropertyDescriptor(process, 'type');
});

afterEach(() => {
  restoreProcessType();
  vi.resetModules();
});

// ---------------------------------------------------------------------------
// A minimal working addon -- capability succeeds, start/drain/stop are spies.
// Individual tests override addonRequireImpl to inject a failure.
// ---------------------------------------------------------------------------

interface FakeAddonHandle {
  onQuantumAvailable: (() => void) | null;
}

function installWorkingAddon(): FakeAddonHandle {
  const handle: FakeAddonHandle = { onQuantumAvailable: null };
  addonRequireImpl = () => ({
    capability: () => ({
      platform: 'darwin',
      osVersion: '14.4',
      perProcessAudio: true,
      reason: '',
    }),
    start: (_opts: unknown, cb: () => void) => {
      handle.onQuantumAvailable = cb;
      return { ok: true };
    },
    // Always reports success -- the credit gate, not ring emptiness, is what
    // these tests pin. Ring-empty behaviour is Task 2's concern (native rt/).
    drain: (_into: ArrayBuffer) => ({ ok: true }),
    stop: vi.fn(),
  });
  return handle;
}

// ---------------------------------------------------------------------------
// process.parentPort stand-in
// ---------------------------------------------------------------------------

interface FakeParentPort {
  posted: unknown[];
  handlers: Record<string, (m: unknown) => void>;
}

function installParentPort(): FakeParentPort {
  const posted: unknown[] = [];
  const handlers: Record<string, (m: unknown) => void> = {};
  const fake = {
    postMessage: (m: unknown) => {
      posted.push(m);
    },
    on: (event: string, h: (m: unknown) => void) => {
      handlers[event] = h;
    },
    start: vi.fn(),
  };
  Object.defineProperty(process, 'parentPort', { value: fake, configurable: true });
  return { posted, handlers };
}

// ---------------------------------------------------------------------------
// Task 4 fault-reporting harness
// ---------------------------------------------------------------------------

/**
 * Import `audiocapChild` under a controlled `process.type` and a controlled
 * addon loader, returning every message posted to `parentPort`.
 *
 * For `processType !== 'utility'` the returned promise REJECTS (the D5 guard
 * throws synchronously at import time) rather than resolving with an empty
 * list -- callers use `.rejects` on this function directly.
 */
async function runChildWith(opts: {
  processType?: string;
  throwIn?: 'load' | 'capability';
  cause?: string;
}): Promise<unknown[]> {
  setProcessType(opts.processType ?? 'utility');
  const { posted } = installParentPort();

  addonRequireImpl = () => {
    if (opts.throwIn === 'load') {
      const err = new Error('concord-audiocap native addon is not built or not loadable');
      (err as Error & { cause?: unknown }).cause = opts.cause ?? 'load-failure-detail';
      throw err;
    }
    return {
      capability: () => {
        if (opts.throwIn === 'capability') {
          const err = new Error('capability probe failed');
          (err as Error & { cause?: unknown }).cause = opts.cause ?? 'capability-failure-detail';
          throw err;
        }
        return { platform: 'darwin', osVersion: '14.4', perProcessAudio: true, reason: '' };
      },
      start: vi.fn(() => ({ ok: true })),
      drain: vi.fn(() => ({ ok: true })),
      stop: vi.fn(),
    };
  };

  vi.resetModules();
  await import('../../../src/main/audiocapChild');
  return posted;
}

describe('audiocap child fault reporting', () => {
  // Inherited item (d): a throw from capability() or the loader must surface
  // as a distinguishable STAGED fault, never collapse into a bare process
  // exit the parent can only read as "child exited with code N".
  it.each([
    ['capability throws', 'capability' as const],
    ['loader throws', 'load' as const],
  ])('reports %s as a staged fault rather than exiting', async (_label, stage) => {
    const posted = await runChildWith({ throwIn: stage });
    // Value passed: the stage matches what threw. Value obeyed: it is the
    // ONLY message posted -- no stray 'hello' escaped before the throw, and
    // nothing else papers over it.
    expect(posted).toEqual([{ kind: 'fault', stage, message: expect.any(String) }]);
  });

  it('keeps the D5 guard a HARD throw -- a non-utility process never gets a chance to report', async () => {
    const { posted } = installParentPort();
    setProcessType('browser');
    vi.resetModules();
    await expect(import('../../../src/main/audiocapChild')).rejects.toThrow(/utilityProcess/);
    // Value obeyed: the hard throw means NOTHING reached parentPort -- not a
    // fault, not a hello, not a partial hello. A guard that degraded to a
    // reported fault instead of a throw would still leave posted empty here
    // only by coincidence, so this is checked against the throw, not instead
    // of it.
    expect(posted).toHaveLength(0);
  });

  it('never serialises Error.cause into the fault message', async () => {
    const posted = await runChildWith({ throwIn: 'load', cause: 'SECRET-PATH-DETAIL' });
    expect(JSON.stringify(posted)).not.toContain('SECRET-PATH-DETAIL');
    // Value obeyed: a fault was still posted -- the cause is dropped, not the
    // whole report.
    expect(posted).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// Task 4 credit-gate harness
// ---------------------------------------------------------------------------

interface CreditGateHarness {
  /** Quanta the child forwarded on the PCM port (posted ArrayBuffers). */
  posted: unknown[];
  /** Every message the child posted on parentPort (control channel). */
  parentPosted: unknown[];
  /** Fault messages among `parentPosted`, read live (not a point-in-time snapshot). */
  readonly faults: unknown[];
  /** Simulate the native addon's coalesced "there may be something" signal. */
  signalQuantumAvailable: () => void;
  /** Simulate preload acking one quantum: `{c: 1}` on the PCM port. */
  ack: () => void;
  /** Send an arbitrary message to the child on the PCM port. */
  send: (msg: unknown) => void;
  /** Flush one macrotask so async continuations in the child settle. */
  flush: () => Promise<void>;
  /**
   * Gate on `count()` REACHING `atLeast`, then on it going quiet. Prefer this
   * over `flush()` wherever the assertion is about a count: `flush()` is a bare
   * `setTimeout(0)` queued before the child's posts exist, so it races their
   * delivery (this is what reddened `main` in the sibling creditFloor test).
   * `flush()` survives only for the two assertions below that have no positive
   * signal to gate on.
   */
  settle: (count: () => number, atLeast: number) => Promise<void>;
}

async function startChildHarness(): Promise<CreditGateHarness> {
  setProcessType('utility');
  const addon = installWorkingAddon();
  const { posted: parentPosted, handlers } = installParentPort();

  vi.resetModules();
  await import('../../../src/main/audiocapChild');

  const { port1: childEnd, port2: testEnd } = new MessageChannel();
  const posted: unknown[] = [];
  testEnd.onmessage = (ev: MessageEvent) => {
    posted.push(ev.data);
  };
  testEnd.start();

  // Precondition, not an optional-chained fire: the child must have actually
  // registered a parentPort 'message' listener before we can hand it the PCM
  // port and a `start` control message.
  expect(typeof handlers.message).toBe('function');
  handlers.message({
    data: {
      kind: 'start',
      quantumMs: 10,
      sampleRate: 48000,
      channels: 2,
      frameCount: 480,
      creditBound: 8,
      ringSlots: 8,
    },
    ports: [childEnd],
  });

  // Precondition: the child must have called addon.start(opts, cb) synchronously
  // while handling `start`, or there is nothing for signalQuantumAvailable to
  // drive and every credit-gate assertion below would be vacuous.
  expect(typeof addon.onQuantumAvailable).toBe('function');

  return {
    posted,
    parentPosted,
    get faults() {
      return parentPosted.filter((m) => (m as { kind?: string }).kind === 'fault');
    },
    signalQuantumAvailable: () => {
      const cb = addon.onQuantumAvailable;
      if (typeof cb !== 'function') {
        throw new Error('onQuantumAvailable was never registered');
      }
      cb();
    },
    ack: () => testEnd.postMessage({ c: 1 }),
    send: (msg: unknown) => testEnd.postMessage(msg),
    flush: () => new Promise((resolve) => setTimeout(resolve, 0)),
    settle: (count: () => number, atLeast: number) => settled(count, atLeast),
  };
}

describe('audiocap child credit gate', () => {
  it('stops draining at the credit bound and resumes on ack', async () => {
    const c = await startChildHarness();

    for (let i = 0; i < 20; i++) c.signalQuantumAvailable();
    await c.settle(() => c.posted.length, 8);
    // Value obeyed: CREDIT_BOUND (8) caps the drain regardless of how many
    // times the addon signals availability -- credit exhaustion and ring
    // overflow are the same event (spec §4d), never a queue behind the port.
    expect(c.posted.length).toBe(8);

    c.ack();
    await c.settle(() => c.posted.length, 9);
    // A freed credit slot resumes the ALREADY-SIGNALLED availability; this
    // is not the producer asking for more (spec §4d forbids a timer doing
    // that), it is the child finishing what it already knew was there.
    expect(c.posted.length).toBe(9);
  });

  it('faults on a SECOND protocol violation, not the first', async () => {
    const c = await startChildHarness();

    c.send({ c: 2 });
    // Deliberately still `flush()`: this asserts NO fault, so there is no
    // arrival to gate on and `settle()` would add nothing. It is sound only
    // because the NEXT block gates on the fault that the second violation does
    // produce -- if the child were faulting on the first violation, that gate
    // would see 1 here and the count below would be 2.
    await c.flush();
    expect(c.faults).toHaveLength(0);

    c.send({ c: 2 });
    await c.settle(() => c.faults.length, 1);
    expect(c.faults).toHaveLength(1);
    expect(c.faults[0]).toEqual({
      kind: 'fault',
      stage: 'protocol',
      message: expect.any(String),
    });

    // Value obeyed: the second violation also CLOSES the port (spec §4b) --
    // a signal arriving after the fault must not still reach the far end.
    c.signalQuantumAvailable();
    // Deliberately still `flush()`: asserting the port is CLOSED means asserting
    // nothing arrives, and there is no positive signal for that. The preceding
    // `settle()` on the fault is the gate that makes it non-vacuous -- it proves
    // the child processed the violation before we check that the signal produced
    // nothing.
    await c.flush();
    expect(c.posted.length).toBe(0);
  });
});

/**
 * MIGRATED from `audiocapSmokeChild.test.ts` (plan Task 4: "MIGRATE all three
 * cases ... they are D5 process-boundary security tests and exist nowhere
 * else"). Re-pointed at `audiocapChild` per the migration table in the PR
 * report. The vacuity-control case is the load-bearing one: every case above
 * it asserts a THROW, and this module has more than one way to throw (the
 * guard, and the addon load immediately after it) -- so this case proves the
 * guard specifically does NOT fire under 'utility' and the module reaches the
 * addon-load error instead.
 */
describe('audiocapChild process boundary (#3194, migrated)', () => {
  it('refuses to run when process.type is absent entirely', async () => {
    installParentPort();
    delete (process as { type?: string }).type;
    vi.resetModules();
    await expect(import('../../../src/main/audiocapChild')).rejects.toThrow(/utilityProcess/);
  });

  it('names the offending process type, so the failure is diagnosable', async () => {
    installParentPort();
    setProcessType('browser');
    vi.resetModules();
    await expect(import('../../../src/main/audiocapChild')).rejects.toThrow(/"browser"/);
  });

  it('passes the guard under utility and reaches the addon load', async () => {
    const { posted } = installParentPort();
    setProcessType('utility');
    addonRequireImpl = () => {
      throw new Error('concord-audiocap native addon is not built or not loadable');
    };
    vi.resetModules();
    // Under 'utility' the D5 guard must NOT fire. This differs from the
    // ORIGINAL smoke-child assertion (`.rejects.toThrow(...)`) because Task 4
    // changes what happens next: the loader is now wrapped in try/catch and
    // reports fault{stage:'load'} instead of letting the error escape as an
    // uncaught throw (inherited item (d)) -- so "reached the addon load" is
    // now proven by a POSTED fault carrying the loader's own text, not by a
    // rejection. Asserting only `.resolves` would be vacuous (any
    // non-throwing path satisfies it, guard-skipped-by-bug included); the
    // posted-message shape is what actually distinguishes "reached the
    // loader" from "the guard silently no-opped".
    await expect(import('../../../src/main/audiocapChild')).resolves.toBeDefined();
    expect(posted).toEqual([
      { kind: 'fault', stage: 'load', message: expect.stringContaining('native addon') },
    ]);
  });
});
