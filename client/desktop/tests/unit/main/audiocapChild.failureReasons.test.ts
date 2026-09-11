/**
 * `START_FAILURE_REASONS` — the child's half of the CLOSED `AudioCapStartFailure`
 * vocabulary (#3197 PR 1, plan Task 6; design §3.2).
 *
 * WHY THIS FILE EXISTS AT ALL. The union is declared in
 * `native/concord-audiocap/index.d.ts` and consumed here, and the two live in
 * different compilation units — `tsconfig.main.json` compiles `src/**` only, so
 * `audiocapChild.ts` RESTATES the union rather than importing it. A restatement
 * that drifts is a native reason string that reaches `startFailureMessage`,
 * misses the map, and is reported as "no recognised reason" — a real outcome
 * degraded into a generic one, silently, at the exact moment a user's share lost
 * its audio. `ALL_FAILURES` below is that drift detector: it is written out by
 * hand, and it is the test.
 *
 * The suite therefore pins three separate things:
 *   1. every member has a mapping (a union that grows without the map breaks here);
 *   2. the map has NO key that is not a member (a mapping that grows without the
 *      union breaks here — the same drift, pointed the other way);
 *   3. `startFailureMessage` names ONLY an own key. `START_FAILURE_REASONS` is an
 *      object literal, so it inherits `constructor`, `toString` and the rest of
 *      Object.prototype; a bare `map[reason]` lookup would find a FUNCTION for a
 *      native string of `'constructor'` and interpolate it into an outbound fault
 *      message. That is the closed set failing open, and case 3 is what proves it
 *      does not.
 *
 * HARNESS. `audiocapChild.ts` runs its D5 guard and `run()` at import time, so
 * the module cannot simply be imported: `process.type` is pinned to `'utility'`,
 * `process.parentPort` is stood in for, and `node:module`'s `createRequire` is
 * intercepted so the loader never reaches a real `.node`. All three mirror
 * `audiocapChild.test.ts`, which documents each one at length.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Must be module scope: vi.mock is hoisted above imports by the Vitest transform.
vi.mock('node:module', () => {
  const createRequire = () => () => ({
    capability: () => ({
      platform: 'darwin',
      osVersion: '14.4',
      perProcessAudio: true,
      reason: '',
    }),
    start: () => ({ ok: true }),
    drain: () => ({ ok: true }),
    stop: () => undefined,
    status: () => ({
      running: false,
      callbackTotal: 0,
      quantaTotal: 0,
      overrunTotal: 0,
      faulted: false,
      faultReason: 'None',
    }),
  });
  return { createRequire, default: { createRequire } };
});

/**
 * EVERY member of `AudioCapStartFailure`, written out. This list is the test —
 * adding a union member without a mapping must break HERE, not at runtime three
 * processes away. Copied from `native/concord-audiocap/index.d.ts` § design 3.2.
 */
const ALL_FAILURES = [
  'NoBackend',
  'BadOptions',
  'BadArguments',
  'AlreadyStarted',
  'ThreadStartFailed',
  'NoTarget',
  'PermissionDenied',
  'UnsupportedFormat',
  'DeviceError',
  'Poisoned',
] as const;

let typeDescriptor: PropertyDescriptor | undefined;
let hadType = false;

async function loadChild(): Promise<typeof import('../../../src/main/audiocapChild')> {
  Object.defineProperty(process, 'type', { value: 'utility', configurable: true });
  Object.defineProperty(process, 'parentPort', {
    value: {
      postMessage: () => undefined,
      on: () => undefined,
      start: () => undefined,
    },
    configurable: true,
  });
  vi.resetModules();
  return import('../../../src/main/audiocapChild');
}

beforeEach(() => {
  hadType = Object.prototype.hasOwnProperty.call(process, 'type');
  typeDescriptor = Object.getOwnPropertyDescriptor(process, 'type');
});

afterEach(() => {
  if (hadType && typeDescriptor) Object.defineProperty(process, 'type', typeDescriptor);
  else delete (process as { type?: string }).type;
  vi.resetModules();
});

describe('START_FAILURE_REASONS', () => {
  it.each(ALL_FAILURES)('maps %s to a phrase this build is willing to name', async (member) => {
    const { START_FAILURE_REASONS } = await loadChild();
    const phrase = (START_FAILURE_REASONS as Record<string, string | undefined>)[member];
    expect(typeof phrase).toBe('string');
    // Value passed, value OBEYED: an empty phrase would satisfy `toBeDefined()`
    // and then produce "capture did not start - " with nothing after the dash.
    expect(phrase).not.toBe('');
  });

  it('has no mapping for anything that is not a union member', async () => {
    const { START_FAILURE_REASONS } = await loadChild();
    expect(Object.keys(START_FAILURE_REASONS).sort()).toEqual([...ALL_FAILURES].sort());
  });

  it('names every member in the message it builds', async () => {
    const { START_FAILURE_REASONS, startFailureMessage } = await loadChild();
    for (const member of ALL_FAILURES) {
      const phrase = (START_FAILURE_REASONS as Record<string, string>)[member];
      expect(startFailureMessage(member)).toBe(`capture did not start - ${phrase}`);
    }
  });

  // The adversarial case. `START_FAILURE_REASONS` is an object literal and so
  // inherits Object.prototype; a native binary is an unvalidated source, and
  // `'constructor'` is a string it may hand over. A bare index lookup returns a
  // FUNCTION here, which is truthy, so a `!== undefined` guard alone would
  // interpolate `function Object() { [native code] }` into an outbound fault.
  it.each(['constructor', 'toString', 'hasOwnProperty', '__proto__', 'valueOf'])(
    'refuses the inherited key %s rather than naming it',
    async (inherited) => {
      const { startFailureMessage } = await loadChild();
      expect(startFailureMessage(inherited)).toBe(
        'capture did not start - the addon gave no recognised reason'
      );
    }
  );

  it.each([undefined, null, 42, {}, ['NoBackend'], 'NotAMember'])(
    'refuses the non-member %s',
    async (reason) => {
      const { startFailureMessage } = await loadChild();
      expect(startFailureMessage(reason)).toBe(
        'capture did not start - the addon gave no recognised reason'
      );
    }
  );
});
