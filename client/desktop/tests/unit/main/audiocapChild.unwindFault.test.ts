/**
 * The failed-start UNWIND must still report — `audiocapChild.ts` (#3197 PR 1).
 *
 * A SIBLING of `audiocapChild.test.ts` for the same reason the credit-floor file
 * is one: that file is the Task 4 contract and is owned by its author, and this
 * pins a property it does not reach.
 *
 * THE PROPERTY. A failed `start` unwinds through the teardown rather than by
 * hand, and the teardown calls `addon.stop()`. On the ordinary `stop` path that
 * call is deliberately UNWRAPPED — main has already asked for teardown and is
 * about to kill the child, so a catch there would only convert a native defect
 * into silence. On the unwind path the same unwrapped call is a different thing
 * entirely: the seam has already thrown or refused once, so a second throw from
 * `stop()` is plausible, and it escapes the caller's `catch` before `postFault`
 * runs. There is no `process.on('uncaughtException')` handler in this child, so
 * the result is a silent death with the original reason never reported — the
 * exact diagnostic the unwind exists to produce. Found by Gitar on PR #3262.
 *
 * Value passed / value obeyed (`[internal]rules/tests.md`): the assertion is on
 * what reached `parentPort`, which is what main actually sees, not on a spy for
 * `stop()`. A test that only proved `stop()` was called would stay green through
 * the whole defect, because `stop()` IS called — it is the throw afterwards that
 * loses the message.
 *
 * Both failure shapes are covered, because they are separate call sites with
 * separate control flow: `start()` THROWING (inside a try) and `start()`
 * REFUSING with `{ ok: false }` (not inside any try at all — the second is the
 * one an unwrapped call kills with no catch block anywhere in the frame).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

let addonRequireImpl: (specifier: string) => unknown = () => {
  throw new Error('audiocapChild.unwindFault.test.ts: addonRequireImpl not configured');
};

vi.mock('node:module', () => {
  const createRequire = () => (specifier: string) => addonRequireImpl(specifier);
  return { createRequire, default: { createRequire } };
});

let typeDescriptor: PropertyDescriptor | undefined;
let hadType = false;

beforeEach(() => {
  hadType = Object.prototype.hasOwnProperty.call(process, 'type');
  typeDescriptor = Object.getOwnPropertyDescriptor(process, 'type');
});

afterEach(() => {
  if (hadType && typeDescriptor) Object.defineProperty(process, 'type', typeDescriptor);
  else delete (process as { type?: string }).type;
  vi.resetModules();
});

interface Harness {
  /** Everything the child posted on the control channel. */
  posted: unknown[];
  /** Deliver a `start` control message with a PCM port attached. */
  sendStart: () => void;
}

/**
 * Bring the child up with an addon whose `stop()` ALWAYS throws, and whose
 * `start()` fails in the requested shape.
 */
async function childWithFailingStartAndStop(mode: 'throw' | 'refuse'): Promise<Harness> {
  Object.defineProperty(process, 'type', { value: 'utility', configurable: true });

  const posted: unknown[] = [];
  const handlers: Record<string, (m: unknown) => void> = {};
  Object.defineProperty(process, 'parentPort', {
    value: {
      postMessage: (m: unknown) => {
        posted.push(m);
      },
      on: (event: string, h: (m: unknown) => void) => {
        handlers[event] = h;
      },
    },
    configurable: true,
  });

  addonRequireImpl = () => ({
    capability: () => ({
      platform: 'darwin',
      osVersion: '14.4',
      perProcessAudio: true,
      reason: '',
    }),
    start: () => {
      if (mode === 'throw') throw new Error('device open refused');
      return { ok: false, reason: 'Poisoned' };
    },
    drain: () => ({ ok: false }),
    // The second failure. A native module broken enough to fail `start` is
    // broken enough to fail `stop`, which is the whole premise.
    stop: () => {
      throw new Error('native stop exploded');
    },
  });

  vi.resetModules();
  await import('../../../src/main/audiocapChild');

  const { port1: childEnd } = new MessageChannel();
  expect(typeof handlers.message).toBe('function');

  return {
    posted,
    sendStart: () =>
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
      }),
  };
}

describe('audiocap child failed-start unwind (#3197)', () => {
  it.each([
    ['start() throws', 'throw' as const, 'device open refused'],
    // The MAPPED text, not the raw `Poisoned` code. That is deliberate: it pins
    // the production comment's claim that the reason is read through
    // START_FAILURE_REASONS *before* the unwind runs, so what main is told is
    // the addon's own answer and cannot be affected by anything teardown does.
    ['start() refuses', 'refuse' as const, 'this process has already used its one capture'],
  ])(
    'still reports a staged fault when %s and the teardown throws too',
    async (_label, mode, expectedOriginal) => {
      const c = await childWithFailingStartAndStop(mode);

      // The defect itself: an unguarded unwind lets the SECOND throw escape the
      // control-message handler. Nothing downstream catches it in the real child
      // either, which is why this is a silent death rather than a fault.
      expect(() => c.sendStart()).not.toThrow();

      const faults = c.posted.filter((m) => (m as { kind?: string }).kind === 'fault');

      // Exactly one. Reporting twice would be its own defect: main treats a
      // second fault for one start as a protocol violation.
      expect(faults).toHaveLength(1);
      expect(faults[0]).toMatchObject({ kind: 'fault', stage: 'start' });

      const message = (faults[0] as { message: string }).message;

      // The ORIGINAL reason survives -- this is the whole point. A fault that
      // named only the teardown failure would still be a fault, and would still
      // have lost the answer to "why did the share lose its audio".
      expect(message).toContain(expectedOriginal);

      // And the teardown failure is not swallowed either: a native module that
      // failed to stop is a process that must not be reused.
      expect(message).toContain('teardown also failed');
      expect(message).toContain('native stop exploded');
    }
  );
});
