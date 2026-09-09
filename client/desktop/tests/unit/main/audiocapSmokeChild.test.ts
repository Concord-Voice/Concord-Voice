// @vitest-environment node
/**
 * ADR-0043 D5 process-isolation boundary (#3194).
 *
 * `[internal]rules/native-audio.md:89` is categorical: "The addon runs in a
 * utilityProcess, never in main and never in the renderer." Until this PR that
 * boundary was enforced by reviewer attention alone — and this PR is precisely
 * the change that puts a file which loads the addon inside `src/main/`, where a
 * later refactor could import it from the main process with nothing objecting.
 *
 * A header comment saying "this runs in a utility process" is documentation. The
 * assertion under test is the enforcement.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

/** Load the child module with a forced `process.type`, restoring it afterwards. */
async function loadAs(type: string | undefined): Promise<unknown> {
  const had = Object.prototype.hasOwnProperty.call(process, 'type');
  const descriptor = Object.getOwnPropertyDescriptor(process, 'type');
  try {
    if (type === undefined) {
      delete (process as { type?: string }).type;
    } else {
      Object.defineProperty(process, 'type', { value: type, configurable: true });
    }
    vi.resetModules();
    // Literal specifier: vite cannot resolve a dynamic import whose specifier
    // is a variable — it strips the relative prefix and reports
    // "Cannot find module '/src/main/...'".
    return await import('../../../src/main/audiocapSmokeChild');
  } finally {
    if (had && descriptor) Object.defineProperty(process, 'type', descriptor);
    else delete (process as { type?: string }).type;
    vi.resetModules();
  }
}

afterEach(() => {
  vi.resetModules();
});

describe('audiocapSmokeChild process boundary (#3194)', () => {
  it.each(['browser', 'renderer', 'worker'] as const)(
    'refuses to run when process.type is %s',
    async (type) => {
      await expect(loadAs(type)).rejects.toThrow(/utilityProcess/);
    }
  );

  it('refuses to run when process.type is absent entirely', async () => {
    await expect(loadAs(undefined)).rejects.toThrow(/utilityProcess/);
  });

  it('names the offending process type, so the failure is diagnosable', async () => {
    await expect(loadAs('browser')).rejects.toThrow(/"browser"/);
  });

  // VACUITY CONTROL — [internal]rules/tests.md § Vacuity.
  //
  // Every case above asserts a THROW, and this module has more than one way to
  // throw: the guard, and the addon load immediately after it (the binary is
  // gitignored and absent in a dev tree). A guard that fired unconditionally —
  // or one deleted entirely, leaving only the loader's own error — would satisfy
  // all four cases above while pinning nothing.
  //
  // So assert the OTHER side: under 'utility' the guard must NOT fire, and the
  // module must get far enough to execute the addon require and surface the
  // loader's own message. That is what separates "the guard is correct" from
  // "something in this file throws".
  it('passes the guard under utility and reaches the addon load', async () => {
    // Pin the addon path to something that cannot exist. Without this the loader
    // falls back to its dev path, `native/concord-audiocap/build/Release/`, and
    // the case becomes a function of whether anyone has run `npm run build:native`
    // — green on a clean checkout, red the moment the addon is actually built,
    // which is exactly the state a packaging verification leaves behind. Found
    // that way: 6/6 in isolation, then red in the full suite after Task 8 built
    // the binary. `[internal]rules/tests.md` — deterministic, no external dependency.
    // Must be the path the loader DERIVES from resourcesPath: since #3194 F1 an
    // arbitrary value is refused up front, which would exercise the trust branch
    // rather than the addon load this control is about.
    const resDescriptor = Object.getOwnPropertyDescriptor(process, 'resourcesPath');
    Object.defineProperty(process, 'resourcesPath', {
      value: '/nonexistent-resources',
      configurable: true,
    });
    process.env.CONCORD_AUDIOCAP_PATH = '/nonexistent-resources/concord_audiocap.node';
    try {
      await expect(loadAs('utility')).rejects.toThrow(/concord-audiocap native addon/);
      await expect(loadAs('utility')).rejects.not.toThrow(/utilityProcess/);
    } finally {
      delete process.env.CONCORD_AUDIOCAP_PATH;
      if (resDescriptor) Object.defineProperty(process, 'resourcesPath', resDescriptor);
      else delete (process as { resourcesPath?: string }).resourcesPath;
    }
  });
});
