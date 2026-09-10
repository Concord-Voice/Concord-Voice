// @vitest-environment node
/**
 * Tests for the audiocap utilityProcess host (#3195, ADR-0043).
 *
 * `src/main/audiocapHost.ts` does not exist yet. This file is written FIRST per
 * superpowers:test-driven-development / the #3195 implementation plan Task 3 Step 1
 * and MUST fail with a module-resolution error only, until a separate agent
 * implements the host against exactly these cases. Do not create the module here.
 *
 * `src/main/**` is in `sonar.coverage.exclusions` (design §1, "New finding neither
 * panellist had") and outside the Istanbul `include`, so this file carries zero
 * Sonar coverage weight. It exists anyway because `[internal]rules/tests.md` §
 * Coverage requires a test file per source file regardless, and because this is
 * the load-bearing surface for three named incidents: #1383 (quit deadlock),
 * #3194 (env-allowlist CWE-497/CWE-178), and ADR-0043 D4b risk 4 (native
 * memory-safety fault → respawn loop → attacker-paced exhaustion primitive).
 *
 * Mocking convention follows `tests/unit/main/audiocapSmoke.test.ts` (the harness
 * this module absorbs, per the plan's Task 3 description) and
 * `tests/unit/main/quitLifecycle.deadlock.test.ts` (the `vi.mock('electron', …)`
 * shape for main-process modules).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { AUDIOCAP_PROTOCOL, HANDSHAKE_TIMEOUT_MS } from '../../../src/shared/audiocapProtocol';
import { NATIVE_ADDON_ENV } from '../../../src/main/nativeAddonPath';

const fork = vi.fn();
/**
 * `app.isPackaged` has to be MUTABLE for the migrated `audiocapSmoke.test.ts`
 * cases: "sets the addon path only when packaged" needs both halves, and the
 * CWE-178 case is only meaningful unpackaged (where NO form of the variable may
 * reach the child). `beforeEach` resets it to `true`, the value the original
 * static mock carried, so the seven Task-3a cases below are unaffected.
 */
const appState = { isPackaged: true };

vi.mock('electron', () => ({
  utilityProcess: { fork: (...args: unknown[]) => fork(...args) },
  // Minimal Option-B' stand-in (design §5 Q6). Task 3 does not exercise the port
  // handoff itself -- that is Task 5/6/7's territory -- but the host module
  // imports the constructor, so the mock must exist for the module to load.
  MessageChannelMain: class {
    port1 = {};
    port2 = {};
  },
  app: {
    on: vi.fn(),
    get isPackaged() {
      return appState.isPackaged;
    },
    exit: vi.fn(),
  },
}));

/** A utilityProcess child stub whose handlers the test drives by hand. */
/**
 * `kill` returns TRUE by default, because that is what Electron returns for a
 * child that has actually spawned -- and the default mock returned `undefined`,
 * which is falsy, so the host's not-yet-spawned recovery path fired on every
 * case. A mock that does not model a return value is not neutral; it silently
 * asserts the least likely branch.
 *
 * `onceHandlers` is separate from `handlers` on purpose: `once('spawn')` and
 * `on('spawn')` are different registrations, and collapsing them would let a
 * test's `on` handler be clobbered by the host's `once` (or the reverse).
 */
function makeChild(options: { killReturns?: boolean } = {}) {
  const { killReturns = true } = options;
  const handlers: Record<string, (...args: unknown[]) => void> = {};
  const onceHandlers: Record<string, (...args: unknown[]) => void> = {};
  return {
    handlers,
    onceHandlers,
    postMessage: vi.fn(),
    kill: vi.fn(() => killReturns),
    pid: 1,
    on(event: string, cb: (...args: unknown[]) => void) {
      handlers[event] = cb;
      return this;
    },
    once(event: string, cb: (...args: unknown[]) => void) {
      onceHandlers[event] = cb;
      return this;
    },
  };
}

/** Import fresh so the module-level electron mock is applied per case. */
async function loadHost() {
  return import('../../../src/main/audiocapHost');
}

let platformDescriptor: PropertyDescriptor | undefined;
let resourcesDescriptor: PropertyDescriptor | undefined;

function setPlatform(value: NodeJS.Platform): void {
  Object.defineProperty(process, 'platform', { value, configurable: true });
}

/** A `hello` that `isAudiocapHello` accepts, so the happy path is reachable. */
function validHello(): unknown {
  return {
    kind: 'hello',
    protocol: AUDIOCAP_PROTOCOL,
    capability: {
      platform: 'darwin',
      osVersion: '15.0',
      perProcessAudio: true,
      reason: 'ok',
    },
    resourcesPathPresent: true,
    envKeys: ['PATH'],
  };
}

beforeEach(() => {
  fork.mockReset();
  vi.resetModules();
  appState.isPackaged = true;
  platformDescriptor = Object.getOwnPropertyDescriptor(process, 'platform');
  resourcesDescriptor = Object.getOwnPropertyDescriptor(process, 'resourcesPath');
  // Pin the platform. `resolveNativeAddonPath` returns null on Linux (ADR-0043
  // scopes the addon to darwin/win32), so on a Linux runner an unpinned case
  // takes the `unsupported-os` path and never forks at all -- every assertion
  // about `fork` in this file would fail there for a reason unrelated to what it
  // tests. Determinism is a precondition for the whole file, not only for the
  // migrated cases (tests.md section General).
  setPlatform('darwin');
});

afterEach(() => {
  delete process.env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP;
  delete process.env.CONCORD_HOST_CANARY;
  delete process.env['concord_audiocap_path'];
  vi.useRealTimers();
  if (platformDescriptor) Object.defineProperty(process, 'platform', platformDescriptor);
  if (resourcesDescriptor) Object.defineProperty(process, 'resourcesPath', resourcesDescriptor);
  else delete (process as { resourcesPath?: string }).resourcesPath;
});

describe('audiocap host env allowlist (#3195, C7 obeyed at the outermost fork() args)', () => {
  it('passes exactly the allowlisted keys, never a spread of process.env', async () => {
    // A real secret-shaped env var, set IN THE TEST, so a reintroduced
    // `...process.env` spread (the #3194 CWE-497 regression) fails this assertion
    // loudly instead of silently. The value is a sentinel, never a credential.
    process.env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP = 'sentinel'; // pragma: allowlist secret

    const { startAudiocapHost, ENV_ALLOWLIST } = await loadHost();
    fork.mockReturnValue(makeChild());

    void startAudiocapHost(1);

    // Outermost observable: what utilityProcess.fork was actually handed, not
    // that a value moved between our own functions (tests.md § "Test the
    // consumer, not the handshake").
    expect(fork).toHaveBeenCalledTimes(1);
    const passedEnv = (fork.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    const passed = Object.keys(passedEnv).sort();
    expect(passed).toEqual([...ENV_ALLOWLIST, 'CONCORD_AUDIOCAP_PATH'].sort());
    expect(passed).not.toContain('GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP');
  });
});

describe('audiocap host crash policy — zero respawn (spec §5 Q3, ADR-0043 D4b risk 4)', () => {
  it('does NOT respawn after a mid-share crash', async () => {
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    void startAudiocapHost(1);

    // Precondition, not incidental: if the host never wires an 'exit' listener at
    // all, firing the crash below is a no-op and the count assertion after it
    // would pass whether or not any respawn-suppression decision exists. Pin
    // that the listener is actually registered before relying on it to prove
    // anything (tests.md § Vacuity).
    expect(typeof child.handlers.exit).toBe('function');

    child.handlers.exit(139);
    await new Promise((resolve) => setTimeout(resolve, 20));

    // The outermost observable: whether a second OS process got spawned. One
    // crash, one fork call total -- no retry loop, asserted on the fork spy's
    // call count, never on a log line.
    expect(fork).toHaveBeenCalledTimes(1);
  });

  it('a SUBSEQUENT share forks a fresh child', async () => {
    const { startAudiocapHost } = await loadHost();
    fork.mockReturnValue(makeChild());

    void startAudiocapHost(1);
    void startAudiocapHost(2);

    expect(fork).toHaveBeenCalledTimes(2);
  });
});

describe('audiocap host kill — #1383 no-await window', () => {
  it('kills synchronously -- no await before kill()', async () => {
    const { startAudiocapHost, killAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    void startAudiocapHost(1);
    killAudiocapHost();

    // Called before any microtask drains: utilityProcess.kill() is synchronous
    // and fire-and-forget, and an await here would re-enter the #1383 veto
    // window (electron.md § Window quit lifecycle).
    expect(child.kill).toHaveBeenCalled();
  });

  it('is idempotent', async () => {
    const { startAudiocapHost, killAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    void startAudiocapHost(1);
    killAudiocapHost();
    killAudiocapHost();
    killAudiocapHost();

    expect(child.kill).toHaveBeenCalledTimes(1);
  });
});

describe('audiocap host handshake validation — trust boundary (spec §7)', () => {
  it('kills the child and degrades when hello fails validation', async () => {
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(1);

    // Same vacuity guard as the crash-policy case above: firing a 'message' the
    // host never listens for would leave `pending` hanging forever rather than
    // passing vacuously, but pin the wiring explicitly so a future refactor that
    // silently drops the listener fails here, not via a suite-wide timeout.
    expect(typeof child.handlers.message).toBe('function');

    // A truthy NON-BOOLEAN perProcessAudio from the child. Per spec §7 this field
    // is a necessary input main ANDs with facts it owns, and is NEVER
    // authorization by itself -- a hostile or buggy child claiming capability
    // must still be refused. This is #2161's defect arriving on a different
    // channel, and the regression lock for it.
    child.handlers.message({ kind: 'hello', protocol: 1, capability: { perProcessAudio: 1 } });

    // Both halves required (C7): the computed decision AND its real-world effect.
    await expect(pending).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(child.kill).toHaveBeenCalled();
  });
});

describe('audiocap host handshake timeout (spec §6b, HANDSHAKE_TIMEOUT_MS)', () => {
  it('kills the child and degrades with handshake-timeout when no hello ever arrives', async () => {
    vi.useFakeTimers();
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(1);
    await vi.advanceTimersByTimeAsync(HANDSHAKE_TIMEOUT_MS);

    await expect(pending).resolves.toEqual({ ok: false, reason: 'handshake-timeout' });
    expect(child.kill).toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// MIGRATED from `tests/unit/main/audiocapSmoke.test.ts` (#3194).
//
// `audiocapHost.ts` ABSORBS `audiocapSmoke.ts` rather than calling it (plan
// Task 3), so the smoke suite is deleted with its module. Seven of its eight
// cases are re-pointed here rather than dropped -- a test is deleted because the
// PROPERTY it pins is gone, never because the module it named was renamed. The
// eighth ("times out rather than hanging when the child neither reports nor
// exits") is subsumed by "kills the child and degrades with handshake-timeout
// when no hello ever arrives" above: same timer, same HANDSHAKE_TIMEOUT_MS, and
// the host case additionally asserts the kill.
// ---------------------------------------------------------------------------

describe('audiocap host unsupported platform (migrated, #3194)', () => {
  it('reports the platform and never forks a child', async () => {
    setPlatform('linux');
    const { startAudiocapHost } = await loadHost();
    fork.mockReturnValue(makeChild());

    await expect(startAudiocapHost(1)).resolves.toEqual({
      ok: false,
      reason: 'unsupported-os',
    });
    // The OUTERMOST effect: ADR-0043 puts Linux/PipeWire out of scope, so there
    // is no addon to host and no child process should ever exist.
    expect(fork).not.toHaveBeenCalled();
  });
});

describe('audiocap host child environment (migrated, #3194 CWE-497 / CWE-178)', () => {
  it('does not forward arbitrary parent variables to the addon child', async () => {
    appState.isPackaged = false;
    // Named CANARY, not *_SECRET: detect-secrets flags a `*SECRET* = '<string>'`
    // assignment as a Secret Keyword, and renaming is better than an allowlist
    // pragma -- nothing is suppressed and the word is the more accurate one anyway.
    process.env.CONCORD_HOST_CANARY = 'canary-value';
    const { startAudiocapHost, ENV_ALLOWLIST } = await loadHost();
    fork.mockReturnValue(makeChild());

    void startAudiocapHost(1);

    const env = (fork.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    // The whole point of the allowlist: a variable nobody named cannot reach the
    // process ADR-0043 D5 exists to keep secrets away from.
    expect(env.CONCORD_HOST_CANARY).toBeUndefined();
    // UNPACKAGED, so the addon path is absent and the allowlist is the entire
    // environment. This is the half that "sets the addon path only when
    // packaged" names in its title and never actually exercises.
    expect(Object.keys(env).sort()).toEqual([...ENV_ALLOWLIST].sort());
  });

  it('refuses to forward a case-variant of the addon variable (Windows CWE-178)', async () => {
    setPlatform('win32');
    appState.isPackaged = false;
    // Exactly what `setx concord_audiocap_path ...` leaves behind. A plain-object
    // `delete` of the UPPER-CASE key does not match this; an allowlist never admits it.
    process.env['concord_audiocap_path'] = 'C:\\Users\\victim\\payload.js';
    const { startAudiocapHost } = await loadHost();
    fork.mockReturnValue(makeChild());

    void startAudiocapHost(1);

    const env = (fork.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    const leaked = Object.keys(env).filter((k) => k.toUpperCase() === NATIVE_ADDON_ENV);
    // Unpackaged, so NO form of the variable may reach the child -- not the canonical
    // casing and not the attacker's.
    expect(leaked).toEqual([]);
  });

  it('sets the addon path only when packaged', async () => {
    appState.isPackaged = true;
    Object.defineProperty(process, 'resourcesPath', {
      value: '/App/Resources',
      configurable: true,
    });
    const { startAudiocapHost } = await loadHost();
    fork.mockReturnValue(makeChild());

    void startAudiocapHost(1);

    const env = (fork.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    expect(env[NATIVE_ADDON_ENV]).toBe('/App/Resources/concord_audiocap.node');
  });
});

describe('audiocap host outcomes (migrated, #3194)', () => {
  it('resolves ok on a validated hello and leaves the child running', async () => {
    const { startAudiocapHost, currentAudiocapGeneration } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(7);
    child.handlers.message(validHello());

    // `perProcessAudio` is the child's CLAIM relayed outward (I5) -- the value the
    // app-start probe records. It is NOT a grant: this PR's ladder has no
    // 'per-process' rung, so nothing downstream can act on a `true`.
    await expect(pending).resolves.toEqual({ ok: true, generation: 7, perProcessAudio: true });
    // The happy path every other case in this file is the negative of. Both
    // halves are required (C7): the computed decision AND its real-world effect
    // -- a validated hello must NOT kill the child, and the generation token
    // must become current so voiceService's sixth currentness term can fence on
    // it (spec section 6a step 3).
    expect(child.kill).not.toHaveBeenCalled();
    expect(currentAudiocapGeneration()).toBe(7);
  });

  it('reports a child that exited before reporting as a failure, not an absent capability', async () => {
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(1);
    child.handlers.exit(1);

    // A child that threw -- the D5 guard, or the loader refusing -- must read as a
    // PACKAGING DEFECT. Collapsing it into perProcessAudio:false would hide a
    // packaging regression behind a legitimate silent video-only rung (C9).
    await expect(pending).resolves.toEqual({ ok: false, reason: 'child-crash' });
  });

  // POST-HELLO CRASH -- and an honest note about what the first assertion proves.
  //
  // The smoke original called this a vacuity control for the settle-once latch
  // and claimed "deleting the latch reds only this case". MEASURED, and that was
  // wrong in both files: a native Promise's `resolve` is ALREADY idempotent, so
  // deleting `settleOnce`'s consumption of `live.settle` leaves this green. The
  // latch is defence for a future non-Promise sink, not something a promise
  // assertion can pin -- see the mutation table in the #3195 Task 3 report.
  //
  // What the case does pin, and what no other case in the file does, is the
  // MID-SHARE crash of spec section 5 Q3. Task 3a's "does NOT respawn after a
  // mid-share crash" never sends a `hello`, so it exercises a child that died
  // during the handshake. Here the child answered first: the capability answer
  // must survive its later exit (a working capture must not read as a packaging
  // defect), the session must be retired so `currentAudiocapGeneration()` stops
  // vouching for a dead child, and there must still be exactly one fork.
  it('keeps the first outcome when message and exit both fire', async () => {
    const { startAudiocapHost, currentAudiocapGeneration } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(1);
    child.handlers.message(validHello());
    child.handlers.exit(0);

    await expect(pending).resolves.toEqual({ ok: true, generation: 1, perProcessAudio: true });
    // I2/I6: the dead child no longer holds the generation token, and nothing
    // forked a replacement for it.
    expect(currentAudiocapGeneration()).toBe(0);
    expect(fork).toHaveBeenCalledTimes(1);
  });
});

describe('audiocap host trust boundary — a rung, once decided, is not re-opened (spec §7)', () => {
  it('kills the child when a SECOND hello arrives after the rung was decided', async () => {
    const { startAudiocapHost, currentAudiocapGeneration } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(3);
    child.handlers.message(validHello());
    await expect(pending).resolves.toEqual({ ok: true, generation: 3, perProcessAudio: true });
    expect(child.kill).not.toHaveBeenCalled();

    // `capability.perProcessAudio` is a necessary INPUT main ANDs with facts it
    // owns; it is never a grant. Rejecting a bad hello at the handshake closes
    // that only for the FIRST one -- a child that answers honestly and then
    // re-announces would otherwise revise a rung main already decided, which is
    // #2161's defect arriving late instead of early. There is no re-negotiation
    // on this channel, so a second hello is a protocol fault like any other
    // out-of-set message.
    child.handlers.message(validHello());

    expect(child.kill).toHaveBeenCalled();
    expect(currentAudiocapGeneration()).toBe(0);
  });
});

// ---------------------------------------------------------------------------
// THE APP-START CAPABILITY PROBE (#3195 §0, plan Task 7 Step 5)
//
// `main.ts` owns the caller; these cases own the four properties the probe
// promises. The caller itself is asserted at its own seam in `main.test.ts`
// ("forks the capture child exactly once during app start") -- a probe with a
// green unit suite and no production caller is precisely the #3194 condition
// this work exists to close, so neither half is sufficient alone.
// ---------------------------------------------------------------------------

describe('audiocap app-start capability probe (#3195 §0)', () => {
  it('records the reported capability and reaps its own child', async () => {
    const { probeAudiocapCapability, audiocapProbeResult, currentAudiocapGeneration } =
      await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    // Nothing has been observed yet, so a later non-null result cannot be the
    // module's initial value read back (tests.md § Vacuity).
    expect(audiocapProbeResult()).toBeNull();

    const pending = probeAudiocapCapability();
    // The fork runs in the promise executor's SYNCHRONOUS pass, so the child's
    // handlers exist by the time control returns here.
    expect(fork).toHaveBeenCalledTimes(1);
    child.handlers.message(validHello());

    await expect(pending).resolves.toEqual({ ok: true, perProcessAudio: true });
    expect(audiocapProbeResult()).toEqual({ ok: true, perProcessAudio: true });
    // P3. A validated hello deliberately does NOT kill the child -- that is the
    // share path's contract -- so this kill is the probe's own, and without it
    // the child outlives the probe with no rail left to reap it.
    expect(child.kill).toHaveBeenCalled();
    expect(currentAudiocapGeneration()).toBe(0);
  });

  it('records a degrade mechanism instead of throwing when the fork cannot be issued', async () => {
    const { probeAudiocapCapability, audiocapProbeResult } = await loadHost();
    // The one failure `startAudiocapHost` does NOT model as a resolution: main
    // could not spawn at all. P2 -- it must still become a recorded mechanism,
    // never a rejection main.ts would have to catch and never a dialog.
    fork.mockImplementation(() => {
      throw new Error('spawn EAGAIN');
    });

    await expect(probeAudiocapCapability()).resolves.toEqual({
      ok: false,
      reason: 'child-crash',
    });
    expect(audiocapProbeResult()).toEqual({ ok: false, reason: 'child-crash' });
  });

  it('records the platform mechanism on an unsupported OS without forking', async () => {
    setPlatform('linux');
    const { probeAudiocapCapability, audiocapProbeResult } = await loadHost();
    fork.mockReturnValue(makeChild());

    await expect(probeAudiocapCapability()).resolves.toEqual({
      ok: false,
      reason: 'unsupported-os',
    });
    expect(audiocapProbeResult()).toEqual({ ok: false, reason: 'unsupported-os' });
    expect(fork).not.toHaveBeenCalled();
  });

  it('runs ONCE -- a second call never forks a second child', async () => {
    const { probeAudiocapCapability } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const first = probeAudiocapCapability();
    const second = probeAudiocapCapability();
    child.handlers.message(validHello());
    await Promise.all([first, second]);

    // P4. `startAudiocapHost` SUPERSEDES (I3): it kills any live child before
    // forking. So a second probe once #3198's shares exist would reap a capture
    // child mid-share, which is why "once" is structural here rather than a
    // convention main.ts is trusted to keep.
    expect(fork).toHaveBeenCalledTimes(1);
  });
});

// ─── #3245: kill() before the OS spawn does not reap ──────────────────────────
//
// MEASURED, not assumed. Electron 44.1.1 / darwin, forking and killing in the
// same tick:
//     A same-tick kill() returned: false
//     A after 2.5s: spawned=true exited=false pid=7659
//     B post-spawn kill() returned: true      <- control
//     B after 1.5s: exited=true
// `kill()` neither reaps nor cancels a pending spawn; the child comes up with
// nothing holding a reference to it. `startAudiocapHost` opens exactly this
// window on itself (I3 supersedes, THEN forks).
describe('audiocap host reaping a child that has not spawned yet (#3245)', () => {
  it('re-kills on spawn when the first kill() reports the child does not exist', async () => {
    const { startAudiocapHost, killAudiocapHost } = await loadHost();
    const child = makeChild({ killReturns: false });
    fork.mockReturnValue(child);

    void startAudiocapHost(1);
    killAudiocapHost();

    // The kill was attempted and reported failure...
    expect(child.kill).toHaveBeenCalledTimes(1);
    // ...so the host must have armed the only signal that says the OS process
    // now exists. Without this the child is live, unreachable and unreaped.
    expect(typeof child.onceHandlers.spawn).toBe('function');

    child.onceHandlers.spawn();
    expect(child.kill).toHaveBeenCalledTimes(2);
  });

  it('does NOT arm a spawn re-kill when the first kill() succeeded', async () => {
    const { startAudiocapHost, killAudiocapHost } = await loadHost();
    const child = makeChild({ killReturns: true });
    fork.mockReturnValue(child);

    void startAudiocapHost(1);
    killAudiocapHost();

    // The control. A child that really was reaped must not carry a listener that
    // would kill its successor's process if the handle were ever reused.
    expect(child.kill).toHaveBeenCalledTimes(1);
    expect(child.onceHandlers.spawn).toBeUndefined();
  });
});
