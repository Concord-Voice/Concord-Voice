// @vitest-environment node
/**
 * Tests for the concord-audiocap smoke-harness PARENT (#3194).
 *
 * This file exists because `audiocapSmoke.ts` shipped with no test at all — a new
 * source file and a new exported function with neither a happy path nor an error path,
 * against `[internal]rules/tests.md` § Coverage. `src/main/**` is in
 * `sonar.coverage.exclusions`, so the Quality Gate could never have surfaced that; the
 * rule still applies, and the absence was found by review rather than by CI.
 *
 * The env-allowlist cases are the load-bearing ones. They are the regression lock for
 * two review findings that a "does it pass the right path?" test cannot see:
 *   - CWE-497: the child used to receive `{ ...process.env }`, handing a
 *     memory-unsafe addon every secret main holds and giving back part of what
 *     ADR-0043 D5 buys.
 *   - CWE-178: the strip was `delete childEnv[NATIVE_ADDON_ENV]` on a PLAIN object,
 *     whose keys are case-sensitive while Windows env lookup is not — so a
 *     `concord_audiocap_path` set by an attacker survived it.
 * Both are closed structurally by building the child env from an allowlist, and these
 * tests assert the OUTERMOST observable effect (what `utilityProcess.fork` was actually
 * handed) rather than that a value moved between our own functions.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NATIVE_ADDON_ENV } from '../../../src/main/nativeAddonPath';

const forkMock = vi.fn();
const appState = { isPackaged: false };

vi.mock('electron', () => ({
  app: {
    get isPackaged() {
      return appState.isPackaged;
    },
  },
  utilityProcess: {
    fork: (...args: unknown[]) => forkMock(...args),
  },
}));

/** A child stub whose 'message' / 'exit' handlers the test drives by hand. */
function makeChild() {
  const handlers: Record<string, (arg: unknown) => void> = {};
  return {
    handlers,
    kill: vi.fn(),
    on(event: string, cb: (arg: unknown) => void) {
      handlers[event] = cb;
      return this;
    },
    emit(event: string, arg?: unknown) {
      handlers[event]?.(arg);
    },
  };
}

/** Import fresh so the module-level electron mock is applied per case. */
async function load() {
  vi.resetModules();
  return import('../../../src/main/audiocapSmoke');
}

let platformDescriptor: PropertyDescriptor | undefined;
let resourcesDescriptor: PropertyDescriptor | undefined;

function setPlatform(value: NodeJS.Platform) {
  Object.defineProperty(process, 'platform', { value, configurable: true });
}

beforeEach(() => {
  platformDescriptor = Object.getOwnPropertyDescriptor(process, 'platform');
  resourcesDescriptor = Object.getOwnPropertyDescriptor(process, 'resourcesPath');
  forkMock.mockReset();
  appState.isPackaged = false;
});

afterEach(() => {
  if (platformDescriptor) Object.defineProperty(process, 'platform', platformDescriptor);
  if (resourcesDescriptor) Object.defineProperty(process, 'resourcesPath', resourcesDescriptor);
  else delete (process as { resourcesPath?: string }).resourcesPath;
  delete process.env[NATIVE_ADDON_ENV];
  delete process.env.CONCORD_SMOKE_CANARY;
  vi.useRealTimers();
  vi.resetModules();
});

describe('runAudiocapSmoke — unsupported platform (#3194)', () => {
  it('reports the platform and never forks a child', async () => {
    setPlatform('linux');
    const { runAudiocapSmoke } = await load();

    const result = await runAudiocapSmoke();

    expect(result).toEqual({ ok: false, error: 'unsupported platform: linux' });
    // The OUTERMOST effect: ADR-0043 puts Linux/PipeWire out of scope, so there is no
    // addon to probe and no child should ever be spawned.
    expect(forkMock).not.toHaveBeenCalled();
  });
});

describe('runAudiocapSmoke — child environment (#3194 CWE-497 / CWE-178)', () => {
  it('does not forward arbitrary parent variables to the addon child', async () => {
    setPlatform('darwin');
    // Named CANARY, not *_SECRET: detect-secrets flags a `*SECRET* = '<string>'`
    // assignment as a Secret Keyword, and renaming is better than an allowlist
    // pragma — nothing is suppressed and the word is the more accurate one anyway.
    process.env.CONCORD_SMOKE_CANARY = 'canary-value';
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    child.emit('message', { ok: true, capability: {} });
    await pending;

    const env = (forkMock.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    // The whole point of the allowlist: a variable nobody named cannot reach the
    // process ADR-0043 D5 exists to keep secrets away from.
    expect(env.CONCORD_SMOKE_CANARY).toBeUndefined();
  });

  it('refuses to forward a case-variant of the addon variable (Windows CWE-178)', async () => {
    setPlatform('win32');
    // Exactly what `setx concord_audiocap_path ...` leaves behind. A plain-object
    // `delete` of the UPPER-CASE key does not match this; an allowlist never admits it.
    process.env['concord_audiocap_path'] = 'C:\\Users\\victim\\payload.js';
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    child.emit('message', { ok: true, capability: {} });
    await pending;

    const env = (forkMock.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    const leaked = Object.keys(env).filter((k) => k.toUpperCase() === NATIVE_ADDON_ENV);
    // Unpackaged, so NO form of the variable may reach the child — not the canonical
    // casing and not the attacker's.
    expect(leaked).toEqual([]);
    delete process.env['concord_audiocap_path'];
  });

  it('sets the addon path only when packaged', async () => {
    setPlatform('darwin');
    appState.isPackaged = true;
    Object.defineProperty(process, 'resourcesPath', {
      value: '/App/Resources',
      configurable: true,
    });
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    child.emit('message', { ok: true, capability: {} });
    await pending;

    const env = (forkMock.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    expect(env[NATIVE_ADDON_ENV]).toBe('/App/Resources/concord_audiocap.node');
  });
});

describe('runAudiocapSmoke — outcomes (#3194)', () => {
  it('returns what the child reported', async () => {
    setPlatform('darwin');
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    child.emit('message', { ok: true, capability: { perProcessAudio: false } });

    await expect(pending).resolves.toEqual({ ok: true, capability: { perProcessAudio: false } });
    expect(child.kill).toHaveBeenCalled();
  });

  it('reports a child that exited before reporting as a failure, not an absent capability', async () => {
    setPlatform('darwin');
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    child.emit('exit', 1);

    // A child that threw — the D5 guard, or the loader refusing — must read as a
    // PACKAGING DEFECT. Collapsing it into perProcessAudio:false would hide a
    // packaging regression behind a legitimate silent video-only rung.
    await expect(pending).resolves.toEqual({
      ok: false,
      error: 'child exited with code 1 before reporting',
    });
  });

  it('times out rather than hanging when the child neither reports nor exits', async () => {
    vi.useFakeTimers();
    setPlatform('darwin');
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    await vi.advanceTimersByTimeAsync(10_000);

    await expect(pending).resolves.toEqual({ ok: false, error: 'timed out after 10000ms' });
    expect(child.kill).toHaveBeenCalled();
  });

  // VACUITY CONTROL — `[internal]rules/tests.md` § Vacuity.
  //
  // Every case above settles the promise exactly once, so all of them pass for a
  // harness with no `settled` latch at all. This one drives BOTH terminal events and
  // pins that the FIRST one wins: without the latch, `exit` firing after `message`
  // would overwrite a successful probe with "child exited before reporting", turning a
  // real capability answer into a spurious packaging defect. Deleting the latch reds
  // only this case.
  it('keeps the first outcome when message and exit both fire', async () => {
    setPlatform('darwin');
    const child = makeChild();
    forkMock.mockReturnValue(child);

    const { runAudiocapSmoke } = await load();
    const pending = runAudiocapSmoke();
    child.emit('message', { ok: true, capability: { perProcessAudio: true } });
    child.emit('exit', 0);

    await expect(pending).resolves.toEqual({ ok: true, capability: { perProcessAudio: true } });
  });
});
