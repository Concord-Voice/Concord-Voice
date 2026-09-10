/**
 * The credit FLOOR — `audiocapChild.ts` C5 (#3195, plan Task 4).
 *
 * A SIBLING of `audiocapChild.test.ts` rather than a case inside it: that file is
 * the Task 4 contract and is owned by its author. This one pins a property the
 * contract file does not reach, found by mutating the implementation — deleting
 * the `outstanding > 0` guard leaves all nine of its cases green, because every
 * credit it sends is one the child EARNED.
 *
 * THE PROPERTY. `outstanding` is the number of quanta in flight on the PCM port,
 * and `outstanding < CREDIT_BOUND` is the only thing bounding it. The far end of
 * that port is the preload relay, which a compromised renderer can drive. An
 * UNEARNED credit — one that acknowledges a quantum the child never sent — is a
 * legal `{c: 1}` on the wire, so the shape check cannot refuse it; without the
 * floor it drives the counter negative, and a negative counter buys the peer
 * more than `CREDIT_BOUND` buffers in flight for as long as it keeps sending
 * them. That is the pool-inflation shape spec §4e names for the (unbuilt)
 * free-list fallback, arriving on the path that does exist.
 *
 * Value passed / value obeyed (`[internal]rules/tests.md`): the assertion is on
 * what reached the FAR END of the port, not on the counter. The counter is
 * module-private and asserting it would prove only that our own code moved our
 * own number.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

let addonRequireImpl: (specifier: string) => unknown = () => {
  throw new Error('audiocapChild.creditFloor.test.ts: addonRequireImpl not configured');
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

describe('audiocap child credit floor (#3195 C5)', () => {
  it('refuses to let an UNEARNED credit buy a ninth buffer in flight', async () => {
    Object.defineProperty(process, 'type', { value: 'utility', configurable: true });

    const handlers: Record<string, (m: unknown) => void> = {};
    Object.defineProperty(process, 'parentPort', {
      value: {
        postMessage: () => {},
        on: (event: string, h: (m: unknown) => void) => {
          handlers[event] = h;
        },
      },
      configurable: true,
    });

    let onQuantumAvailable: (() => void) | null = null;
    addonRequireImpl = () => ({
      capability: () => ({
        platform: 'darwin',
        osVersion: '14.4',
        perProcessAudio: true,
        reason: '',
      }),
      start: (_opts: unknown, cb: () => void) => {
        onQuantumAvailable = cb;
        return { ok: true };
      },
      // Never empty: the ring is not what stops this drain, the credit bound is.
      drain: (_into: ArrayBuffer) => ({ ok: true }),
      stop: vi.fn(),
    });

    vi.resetModules();
    await import('../../../src/main/audiocapChild');

    const { port1: childEnd, port2: testEnd } = new MessageChannel();
    const posted: unknown[] = [];
    testEnd.onmessage = (ev: MessageEvent) => {
      posted.push(ev.data);
    };
    testEnd.start();

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
    expect(typeof onQuantumAvailable).toBe('function');

    // The whole case: a credit arrives BEFORE the child has sent anything, so it
    // acknowledges a quantum that does not exist. The child must treat it as a
    // no-op on the counter, not as a free slot.
    testEnd.postMessage({ c: 1 });
    await new Promise((resolve) => setTimeout(resolve, 0));

    // Nothing has been drained yet at this point in the pristine implementation,
    // because the credit's own resume ran with `outstanding` already at zero and
    // filled exactly the bound. Assert the bound, which is the number the peer is
    // trying to move.
    expect(posted.length).toBe(8);

    // And it is still the bound after the producer signals again: the unearned
    // credit bought nothing that a later signal can spend.
    (onQuantumAvailable as unknown as () => void)();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(posted.length).toBe(8);
  });
});
