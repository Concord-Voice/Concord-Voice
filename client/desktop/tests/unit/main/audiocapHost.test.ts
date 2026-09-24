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
import { SCREEN_AUDIO_DEGRADE_REASONS } from '../../../src/main/audiocapHost';

const fork = vi.fn();
/**
 * `app.isPackaged` has to be MUTABLE for the migrated `audiocapSmoke.test.ts`
 * cases: "sets the addon path only when packaged" needs both halves, and the
 * CWE-178 case is only meaningful unpackaged (where NO form of the variable may
 * reach the child). `beforeEach` resets it to `true`, the value the original
 * static mock carried, so the seven Task-3a cases below are unaffected.
 */
const appState = { isPackaged: true };

/**
 * `ipcMain.handle` + `desktopCapturer.getSources` (#3394 PR 1, R4/C6). Added so this file
 * can import the REAL `src/main/ipc/audiocap.ts` alongside the REAL host -- a small number
 * of cases below need genuine host state (probe handshaking, a real 'starting' session)
 * that `audiocapIpc.test.ts`'s wholesale `vi.mock('.../audiocapHost', ...)` cannot produce.
 */
const ipcHandle = vi.fn();
const getSources = vi.fn();

vi.mock('electron', () => ({
  ipcMain: { handle: (...args: unknown[]) => ipcHandle(...args) },
  desktopCapturer: { getSources: (...args: unknown[]) => getSources(...args) },
  utilityProcess: { fork: (...args: unknown[]) => fork(...args) },
  // Minimal Option-B' stand-in (design §5 Q6). Task 3 does not exercise the port
  // handoff itself -- that is Task 5/6/7's territory -- but the host module
  // imports the constructor, so the mock must exist for the module to load.
  // Option-B' stand-in (design §5 Q6). #3198 PR 3 gave the ports a real `close`,
  // because `beginCapture`'s failure arm closes BOTH — against `{}` that arm threw a
  // TypeError out of its own catch block and the failure under test was replaced by an
  // unrelated one. A mock that omits a method the code calls is not neutral.
  MessageChannelMain: class {
    port1 = { close: vi.fn() };
    port2 = { close: vi.fn() };
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
  ipcHandle.mockReset();
  getSources.mockReset();
  getSources.mockResolvedValue([{ id: 'window:42:0', name: 'Some App' }]);
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

    void startAudiocapHost(1, null);

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

    void startAudiocapHost(1, null);

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

    void startAudiocapHost(1, null);
    void startAudiocapHost(2, null);

    expect(fork).toHaveBeenCalledTimes(2);
  });
});

describe('audiocap host kill — #1383 no-await window', () => {
  it('kills synchronously -- no await before kill()', async () => {
    const { startAudiocapHost, killAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    void startAudiocapHost(1, null);
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

    void startAudiocapHost(1, null);
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

    const pending = startAudiocapHost(1, null);

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

    const pending = startAudiocapHost(1, null);
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

    await expect(startAudiocapHost(1, null)).resolves.toEqual({
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

    void startAudiocapHost(1, null);

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

    void startAudiocapHost(1, null);

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

    void startAudiocapHost(1, null);

    const env = (fork.mock.calls[0][2] as { env: NodeJS.ProcessEnv }).env;
    expect(env[NATIVE_ADDON_ENV]).toBe('/App/Resources/concord_audiocap.node');
  });
});

describe('audiocap host outcomes (migrated, #3194)', () => {
  it('resolves ok on a validated hello and leaves the child running', async () => {
    const { startAudiocapHost, currentAudiocapGeneration } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(7, null);
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

    const pending = startAudiocapHost(1, null);
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

    const pending = startAudiocapHost(1, null);
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

    const pending = startAudiocapHost(3, null);
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

    void startAudiocapHost(1, null);
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

    void startAudiocapHost(1, null);
    killAudiocapHost();

    // The control. A child that really was reaped must not carry a listener that
    // would kill its successor's process if the handle were ever reused.
    expect(child.kill).toHaveBeenCalledTimes(1);
    expect(child.onceHandlers.spawn).toBeUndefined();
  });
});

/**
 * THE FAULT-STAGE MAPPING, which had no test at all before #3198.
 *
 * `reasonForFaultStage` is a closed mapping from what the child says failed to
 * the mechanism a user is shown, and every arm was unasserted -- so the arm this
 * task adds is pinned together with the five it joins, rather than leaving a
 * table where only the newest entry is checked.
 *
 * Driven through the real fault path (a `fault` message on a live session), not
 * by exporting the mapping: what matters is the reason the host RETIRES with.
 */
describe('audiocap fault stage → degrade mechanism (#3198)', () => {
  it.each([
    ['guard', 'load-fault'],
    ['load', 'load-fault'],
    ['capability', 'capability-fault'],
    ['start', 'no-backend'],
    ['target', 'target-unresolved'],
    ['protocol', 'protocol-fault'],
  ])('maps stage %s to %s', async (stage, reason) => {
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const pending = startAudiocapHost(1, null);
    child.handlers.message({ kind: 'fault', stage, message: 'why' });

    await expect(pending).resolves.toEqual({ ok: false, reason });
  });

  // A prior version of this suite carried a same-named case that asserted only
  // two `SCREEN_AUDIO_DEGRADE_REASONS` memberships -- it could not fail unless
  // the exhaustiveness test below (`has exactly the ten members...`) had
  // already failed, and the actual "target does not collapse into start"
  // behaviour is what the `it.each` table above already pins (`'target'` ->
  // `'target-unresolved'`, distinct from `'start'` -> `'no-backend'`). Removed
  // rather than kept as dead coverage (#3327 review).
});

describe('SCREEN_AUDIO_DEGRADE_REASONS — the #3197 PR 2 addition', () => {
  // ASSERTED AT RUNTIME, AGAINST A SET DECLARED IN src/, and that is the whole
  // point. tsconfig.json includes only src/** — zero files under tests/ are in
  // the type program — so a union enumerated as a typed array HERE would never
  // be type-checked and could never fail. The Readonly<Record<Union, true>> in
  // audiocapHost.ts is what makes a missing member a compile error; this asserts
  // the set that error protects.
  // #3394 PR 2 §4.3: `capture-starved` leaves the union (10 -> 9 members) — a
  // `'run'` fault is now reported through `AudiocapInterrupted`, not a
  // start-time degrade reason. Mutation guard: leaving `capture-starved` in
  // `SCREEN_AUDIO_DEGRADE_REASONS` (audiocapHost.ts) turns this red.
  it('has exactly the nine members, including target-unresolved and no capture-starved', () => {
    expect(Object.keys(SCREEN_AUDIO_DEGRADE_REASONS).sort()).toEqual(
      [
        'capability-fault',
        'child-crash',
        'handshake-timeout',
        'load-fault',
        'no-backend',
        'produce-rejected',
        'protocol-fault',
        'target-unresolved',
        'unsupported-os',
      ].sort()
    );
    expect(Object.keys(SCREEN_AUDIO_DEGRADE_REASONS)).not.toContain('capture-starved');
  });

  it('does not carry consent-denied', () => {
    // Not an omission. R9 measured that TCC denial is byte-identical to a
    // paused app at this seam — every Core Audio call returns noErr and the
    // callbacks arrive on schedule carrying zeros — so nothing in PR 2 can
    // honestly produce it. A mechanism string the mechanism cannot detect is
    // worse than no string at all.
    expect(Object.keys(SCREEN_AUDIO_DEGRADE_REASONS)).not.toContain('consent-denied');
  });
});

/** The child's tap-exists ack (#3394): the capture path's start settles on this, not on hello. */
const STARTED = { kind: 'started' } as const;

/** `validHello()` with the capability bit flipped — everything else stays valid. */
function helloWithCapability(perProcessAudio: boolean): unknown {
  const hello = validHello() as { capability: Record<string, unknown> };
  return { ...hello, capability: { ...hello.capability, perProcessAudio } };
}

describe('machine-capability snapshot (#3198)', () => {
  it('is null before any handshake settles', async () => {
    const { audiocapMachineCapability } = await loadHost();
    // Nothing observed yet, so a later non-null value cannot be the module's
    // initial value read back (tests.md § Vacuity).
    expect(audiocapMachineCapability()).toBeNull();
  });

  it('is set from a validated hello', async () => {
    const { startAudiocapHost, audiocapMachineCapability } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());
    await started;

    expect(audiocapMachineCapability()).toBe(true);
  });

  it('records false when the child claims no per-process backend', async () => {
    const { startAudiocapHost, audiocapMachineCapability } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(helloWithCapability(false));
    await started;

    expect(audiocapMachineCapability()).toBe(false);
  });

  it('is not written by a child that exits without a hello', async () => {
    const { startAudiocapHost, audiocapMachineCapability } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.exit(1);
    await started;

    expect(audiocapMachineCapability()).toBeNull();
  });

  it('notifies the registered listener on every validated hello', async () => {
    const { startAudiocapHost, setAudiocapCapabilityListener } = await loadHost();
    const seen: boolean[] = [];
    setAudiocapCapabilityListener((v: boolean) => seen.push(v));
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());
    await started;

    expect(seen).toEqual([true]);
  });

  // THE COLLISION THIS TASK EXISTS FOR. A share starting while the app-start
  // probe is still handshaking reaps the probe's child (I3 supersedes, then
  // forks). The probe records a FAILURE for a machine that is fully capable, and
  // `probeInFlight ??=` guarantees it never recomputes. The SNAPSHOT must not
  // inherit that: null is "unknown", which the ladder reads as the pre-addon
  // rungs, and the share's own hello then sets it truthfully — self-healing in
  // one share, with no second child and no re-probe path.
  it('stays null when a probe is reaped mid-handshake, and the reaping share sets it', async () => {
    const {
      probeAudiocapCapability,
      startAudiocapHost,
      audiocapMachineCapability,
      audiocapProbeResult,
    } = await loadHost();
    const probeChild = makeChild();
    const shareChild = makeChild();
    fork.mockReturnValueOnce(probeChild).mockReturnValueOnce(shareChild);

    const probe = probeAudiocapCapability();
    expect(audiocapMachineCapability()).toBeNull();

    const share = startAudiocapHost(2, null);
    expect(probeChild.kill).toHaveBeenCalled();
    probeChild.handlers.exit(0);

    shareChild.handlers.message(validHello());
    await share;
    await probe;

    // The probe's own diagnostic still records the false negative — unchanged.
    expect(audiocapProbeResult()?.ok).toBe(false);
    // The snapshot does not inherit it. It was never false; it was null, then true.
    expect(audiocapMachineCapability()).toBe(true);
  });
});

describe('capability listener cannot abort the handshake (#3198 red-team, area 4)', () => {
  // FOUND BY THE PRE-PR ADVERSARIAL PASS, and it is a defect this PR INTRODUCED.
  //
  // `handleChildMessage` clears the handshake timer, writes the snapshot, notifies the
  // listener, sets `ready`, then settles. The notify was added by #3198 and sits BEFORE
  // the settle — so a listener that throws leaves the promise unsettled with its recovery
  // timer already cleared: the host wedges in 'handshaking' forever and never reaps its
  // child. The registered listener calls `webContents.send`, which THROWS on a disposed
  // render frame, and the `mainWindow?.` null check does not cover a disposed frame on a
  // live BrowserWindow.
  //
  // In the fleet as shipped the throw reaches main.ts's uncaughtException handler, which
  // calls app.exit(1) — so the observable failure is an app termination on a narrow race.
  it('settles the handshake even when the listener throws', async () => {
    const { startAudiocapHost, setAudiocapCapabilityListener, audiocapMachineCapability } =
      await loadHost();
    setAudiocapCapabilityListener(() => {
      throw new Error('render frame was disposed');
    });
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());

    await expect(started).resolves.toMatchObject({ ok: true, perProcessAudio: true });
    // The snapshot is still written — only the NOTIFY is allowed to fail.
    expect(audiocapMachineCapability()).toBe(true);
  });

  // CONTROL. Without this, the case above could pass against an implementation that
  // simply stopped calling the listener at all.
  it('notifies a well-behaved listener on a validated hello', async () => {
    const { startAudiocapHost, setAudiocapCapabilityListener } = await loadHost();
    const seen: boolean[] = [];
    setAudiocapCapabilityListener((v: boolean) => seen.push(v));
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());
    await started;

    expect(seen).toEqual([true]);
  });

  // PINS THE ORDERING HALF, WHICH NOTHING PINNED BEFORE (#3198 Phase-8 review).
  //
  // The production comment says "both halves are load-bearing: the ORDERING guarantees the
  // handshake completes; the CATCH guarantees a dead send cannot kill the app after it
  // has." The throw-tolerance case above does not prove the first half. Measured: move the
  // notify back BEFORE `settleOnce` while leaving the try/catch in place, and that case
  // plus its control both stay GREEN -- the listener throws, the catch swallows it, and
  // `settleOnce` runs normally. A false kill, and the PR body's mutation table asserted it
  // as a real one.
  //
  // This case discriminates by making the listener RE-ENTER the host rather than throw.
  // `killAudiocapHost` settles `{ok: false, reason: 'child-crash'}`, and `settleOnce` is
  // first-settle-wins:
  //   notify AFTER settle (correct)  -> {ok:true} already delivered -> green
  //   notify BEFORE settle (mutant)  -> the kill's {ok:false} lands first -> RED
  // The catch cannot mask it, because nothing throws.
  it('settles ok even when the listener re-enters the host synchronously', async () => {
    const { startAudiocapHost, setAudiocapCapabilityListener, killAudiocapHost } = await loadHost();
    setAudiocapCapabilityListener(() => {
      killAudiocapHost();
    });
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());

    await expect(started).resolves.toMatchObject({ ok: true, perProcessAudio: true });
  });

  // The drop is LOGGED. `observability.md` principle 3 constrains the ERROR OBJECT, not the
  // fact that a push was dropped -- a fixed string carries no `cause`, no PII and no
  // privacy discriminator. Without it, a listener throwing on EVERY invocation is
  // indistinguishable from a machine with no per-process backend, permanently.
  it('logs a fixed string when the notify throws, and never the caught value', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { startAudiocapHost, setAudiocapCapabilityListener } = await loadHost();
    setAudiocapCapabilityListener(() => {
      throw new Error('SECRET-CAUSE-SHOULD-NOT-BE-LOGGED');
    });
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());
    await started;

    expect(warn).toHaveBeenCalledWith('[audiocap] capability push dropped');
    const logged = warn.mock.calls.flat().map(String).join(' ');
    expect(logged).not.toContain('SECRET-CAUSE-SHOULD-NOT-BE-LOGGED');
    warn.mockRestore();
  });
});

// ─── The capture leg (#3198 PR 3, plan Task 12a) ───────────────────────────────
//
// The leg this epic never had: #3195 built the host and the handshake, #3197 the
// backends, #3198 PR 1 the ladder and PR 2 the resolver, and nothing anywhere posted
// `{kind:'start'}` — so `AudiocapStart` had no constructor and the child's
// `handleStart` was unreachable in production.
//
// EVERY CASE HERE ASSERTS WHAT REACHED THE CHILD OR THE SINK, never that an internal
// value was passed along (tests.md § "Test the consumer, not the handshake"). The
// handle is VARIED across two cases for the reason § "a branch that is OBEYED must be
// proven REACHED" gives: one handle proves the argument exists, two prove it is
// carried rather than constant-folded.
describe('capture leg — start + port handoff (#3198 PR 3)', () => {
  it.each([
    ['a low handle', 42],
    ['a different handle', 1337],
  ])('posts start carrying %s and hands the renderer its port', async (_label, handle) => {
    const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    const sink = vi.fn();
    setAudiocapPortSink(sink);

    const started = startAudiocapHost(5, handle);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);
    await expect(started).resolves.toEqual({
      ok: true,
      generation: 5,
      perProcessAudio: true,
    });

    expect(child.postMessage).toHaveBeenCalledTimes(1);
    const [message, transfer] = child.postMessage.mock.calls[0] as [
      { kind: string; windowHandle: number },
      unknown[],
    ];
    expect(message).toMatchObject({ kind: 'start', windowHandle: handle });
    // The port must TRANSFER, not ride as a field: a structured clone of a port is not
    // a port, and the child would hold nothing.
    expect(transfer).toHaveLength(1);

    // The renderer gets the OTHER end, under this share's generation. Asserting the two
    // ends DIFFER is the load-bearing half — handing the same end to both would satisfy
    // every count-based check while nothing could ever flow.
    expect(sink).toHaveBeenCalledTimes(1);
    expect(sink.mock.calls[0][0]).toBe(5);
    expect(sink.mock.calls[0][1]).toBeDefined();
    expect(sink.mock.calls[0][1]).not.toBe(transfer[0]);
  });

  it('tells the child FIRST and the renderer second', async () => {
    const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    const order: string[] = [];
    child.postMessage.mockImplementation(() => {
      order.push('child');
    });
    setAudiocapPortSink(() => {
      order.push('renderer');
    });

    const started = startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);
    await started;

    // A renderer holding a port whose peer never reached a child waits forever with no
    // signal; the reverse merely queues quanta the port buffers. One failure mode is
    // permanent and silent, the other self-corrects in a tick.
    expect(order).toEqual(['child', 'renderer']);
  });

  it('THE PROBE TAKES NONE OF IT: a null handle posts no start and calls no sink', async () => {
    const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    const sink = vi.fn();
    setAudiocapPortSink(sink);

    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());
    await expect(started).resolves.toMatchObject({ ok: true });

    // The byte-identical-to-before property that keeps P3 (short-lived probe child) and
    // P4 (it runs once) intact.
    expect(child.postMessage).not.toHaveBeenCalled();
    expect(sink).not.toHaveBeenCalled();
  });

  it('fails closed when the child claims no per-process backend', async () => {
    const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    const sink = vi.fn();
    setAudiocapPortSink(sink);

    const started = startAudiocapHost(1, 42);
    child.handlers.message(helloWithCapability(false));

    // Reachable despite the renderer's ladder, and that is the point: a renderer that
    // lies to itself must not obtain a capture (I5).
    await expect(started).resolves.toEqual({ ok: false, reason: 'no-backend' });
    expect(child.postMessage).not.toHaveBeenCalled();
    expect(sink).not.toHaveBeenCalled();
    expect(child.kill).toHaveBeenCalled();
  });

  it('reaps the child when the port cannot reach the renderer', async () => {
    const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    setAudiocapPortSink(() => {
      throw new Error('no live window');
    });

    const started = startAudiocapHost(1, 42);
    child.handlers.message(validHello());

    // A child told to start whose audio reaches nobody holds an OS tap for no one.
    // Reaping it is the only thing that destroys that tap.
    await expect(started).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(child.kill).toHaveBeenCalled();
  });

  it('fails closed when no sink was ever registered', async () => {
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);

    const started = startAudiocapHost(1, 42);
    child.handlers.message(validHello());

    // A missing sink is a wiring defect, not a machine fact.
    await expect(started).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(child.kill).toHaveBeenCalled();
  });
});

describe('graceful stop (#3198 PR 3)', () => {
  it('posts stop and reaps only after the quiesce window', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink, stopAudiocapHost } = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());

      const started = startAudiocapHost(1, 42);
      child.handlers.message(validHello());
      child.handlers.message(STARTED);
      await started;
      child.postMessage.mockClear();
      child.kill.mockClear();

      stopAudiocapHost();

      // The child's `handleStop` is the ONLY caller of the addon's `status()`, so this
      // message is the only path on which the R9 silence detector, `quiesceProved` and
      // `destroyFailures` are read by anything at all.
      expect(child.postMessage).toHaveBeenCalledWith({ kind: 'stop' });
      expect(child.kill).not.toHaveBeenCalled();

      vi.advanceTimersByTime(200);
      expect(child.kill).toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('cancels the reap when the child exits on its own', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink, stopAudiocapHost } = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());

      const started = startAudiocapHost(1, 42);
      child.handlers.message(validHello());
      child.handlers.message(STARTED);
      await started;
      child.kill.mockClear();

      stopAudiocapHost();
      child.onceHandlers.exit?.(0);

      vi.advanceTimersByTime(200);
      expect(child.kill).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  // One Stop-sharing click sends `audiocap:stop` twice (the renderer's
  // `stopScreenAudioHost` sits on several teardowns). Measured during #3394's T0: the
  // second stop drained the first stop's child 16 ms in, inside `addon.stop()`, so no
  // graceful stop ever wrote its liveness line.
  it('a redundant stop leaves the quiescing child to its own reap timer', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink, stopAudiocapHost } = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());

      const started = startAudiocapHost(1, 42);
      child.handlers.message(validHello());
      child.handlers.message(STARTED);
      await started;
      child.kill.mockClear();

      stopAudiocapHost();
      stopAudiocapHost();
      expect(child.kill).not.toHaveBeenCalled();

      // The timer still owns the reap: redundancy defers nothing past the window.
      vi.advanceTimersByTime(200);
      expect(child.kill).toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('a supersede drains a still-quiescing child rather than letting two taps overlap', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink, stopAudiocapHost } = await loadHost();
      const first = makeChild();
      fork.mockReturnValue(first);
      setAudiocapPortSink(vi.fn());

      const started = startAudiocapHost(1, 42);
      first.handlers.message(validHello());
      first.handlers.message(STARTED);
      await started;
      first.kill.mockClear();

      stopAudiocapHost();
      expect(first.kill).not.toHaveBeenCalled();

      // A new share forks through `killAudiocapHost`, which must drain the pending child
      // SYNCHRONOUSLY. Without the drain the old child holds a live OS tap while the new
      // one opens its own — two taps where the design says one, on the exact privacy
      // surface this epic exists to narrow.
      const second = makeChild();
      fork.mockReturnValue(second);
      void startAudiocapHost(2, 99);

      expect(first.kill).toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('start settles on the started ack, not on hello (#3394 PR 1, spec C1)', () => {
  async function helloWithWindow() {
    const host = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());
    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    return { host, child, started };
  }

  it('does NOT settle at hello for a capture — it waits for started', async () => {
    const { child, started } = await helloWithWindow();
    const settled = vi.fn();
    void started.then(settled);
    await Promise.resolve();
    await Promise.resolve();
    expect(settled).not.toHaveBeenCalled();
    child.handlers.message(STARTED);
    await expect(started).resolves.toEqual({ ok: true, generation: 1, perProcessAudio: true });
  });

  // THE C1 REGRESSION. Must FAIL if settle is moved back to hello.
  it.each([
    ['target', 'target-unresolved'],
    ['start', 'no-backend'],
  ])('a %s fault after hello resolves the start with its real reason', async (stage, reason) => {
    const { child, started } = await helloWithWindow();
    child.handlers.message({ kind: 'fault', stage, message: 'refused' });
    await expect(started).resolves.toEqual({ ok: false, reason });
    expect(child.kill).toHaveBeenCalled();
  });

  it('a child that exits while starting resolves child-crash', async () => {
    const { child, started } = await helloWithWindow();
    child.handlers.exit?.(1);
    await expect(started).resolves.toEqual({ ok: false, reason: 'child-crash' });
  });

  it('times out to handshake-timeout when no started arrives', async () => {
    vi.useFakeTimers();
    try {
      const { child, started } = await helloWithWindow();
      const { START_ACK_TIMEOUT_MS } = await import('../../../src/shared/audiocapProtocol');
      // C11: awaiting the raw promise under fake timers depends on the fake-timer
      // implementation flushing the microtask queue at each `advanceTimersByTime`
      // call. Drive a settled spy instead, so a hang reads as "settled was never
      // called" -- a fast, synchronous assertion -- rather than the whole test
      // timing out with no signal about which half is wrong.
      const settled = vi.fn();
      void started.then(settled);
      vi.advanceTimersByTime(START_ACK_TIMEOUT_MS - 1);
      expect(child.kill).not.toHaveBeenCalled();
      expect(settled).not.toHaveBeenCalled();
      vi.advanceTimersByTime(1);
      // One real microtask tick is enough to let the already-fired timer
      // callback's synchronous `settleOnce` reach the `.then` above -- no
      // further timer advancement, and no dependency on real elapsed time.
      await Promise.resolve();
      expect(settled).toHaveBeenCalledWith({ ok: false, reason: 'handshake-timeout' });
      expect(child.kill).toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  // C8: START_ACK_TIMEOUT_MS and HANDSHAKE_TIMEOUT_MS are declared as the SAME numeric
  // value (10000) today, so `expect(START_ACK_TIMEOUT_MS).toBe(10000)` in
  // audiocapProtocol.test.ts cannot tell "its own constant" from "the handshake timeout
  // reused" -- both readings produce an identical timer. This test drives them to
  // DIFFERENT values via `vi.doMock` and proves the start-ack timer consults
  // START_ACK_TIMEOUT_MS specifically: advancing to HANDSHAKE_TIMEOUT_MS must NOT kill
  // the child, and only reaching the real (mocked) START_ACK_TIMEOUT_MS may.
  it('START_ACK_TIMEOUT_MS is genuinely distinct from HANDSHAKE_TIMEOUT_MS (#3394 PR 1, C8)', async () => {
    vi.doMock('../../../src/shared/audiocapProtocol', async (importOriginal) => {
      const actual = await importOriginal<typeof import('../../../src/shared/audiocapProtocol')>();
      return { ...actual, START_ACK_TIMEOUT_MS: 30_000 };
    });
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
      const { HANDSHAKE_TIMEOUT_MS } = await import('../../../src/shared/audiocapProtocol');
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());
      const started = startAudiocapHost(1, 42);
      child.handlers.message(validHello());
      const settled = vi.fn();
      void started.then(settled);

      vi.advanceTimersByTime(HANDSHAKE_TIMEOUT_MS);
      expect(child.kill).not.toHaveBeenCalled();
      expect(settled).not.toHaveBeenCalled();

      vi.advanceTimersByTime(30_000 - HANDSHAKE_TIMEOUT_MS);
      await Promise.resolve();
      expect(settled).toHaveBeenCalledWith({ ok: false, reason: 'handshake-timeout' });
      expect(child.kill).toHaveBeenCalled();
    } finally {
      vi.doUnmock('../../../src/shared/audiocapProtocol');
      vi.useRealTimers();
    }
  });

  it('a started ack disarms the start-ack timer', async () => {
    vi.useFakeTimers();
    try {
      const { child, started } = await helloWithWindow();
      child.handlers.message(STARTED);
      await started;
      child.kill.mockClear();
      vi.advanceTimersByTime(60_000);
      expect(child.kill).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('a started before hello is a protocol fault (§4a)', async () => {
    const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    setAudiocapPortSink(vi.fn());
    const started = startAudiocapHost(1, 42);
    child.handlers.message(STARTED);
    await expect(started).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(child.kill).toHaveBeenCalled();
  });

  it('a duplicate started kills the child (§4a)', async () => {
    const { child, started } = await helloWithWindow();
    child.handlers.message(STARTED);
    await started;
    child.kill.mockClear();
    child.handlers.message(STARTED);
    expect(child.kill).toHaveBeenCalled();
  });

  it('a started on the probe path is a protocol fault — the probe never starts', async () => {
    const { startAudiocapHost } = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    const started = startAudiocapHost(1, null);
    child.handlers.message(validHello());
    await expect(started).resolves.toMatchObject({ ok: true });
    child.kill.mockClear();
    child.handlers.message(STARTED);
    expect(child.kill).toHaveBeenCalled();
  });

  // C14 for `started`: a detached (stopping) session's message is dropped, NOT reaped.
  it('a started from a child already being stopped is dropped without a reap', async () => {
    vi.useFakeTimers();
    try {
      const { host, child, started } = await helloWithWindow();
      host.stopAudiocapHost();
      await expect(started).resolves.toEqual({ ok: false, reason: 'child-crash' });
      child.kill.mockClear();
      child.handlers.message(STARTED);
      expect(child.kill).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('renderer loss kills the capture, never the probe (#3394 PR 1, spec C15)', () => {
  function fakeContents() {
    const handlers: Record<string, (...args: unknown[]) => void> = {};
    return {
      handlers,
      on: vi.fn((event: string, cb: (...args: unknown[]) => void) => {
        handlers[event] = cb;
      }),
    };
  }

  async function capturing() {
    const host = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());
    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);
    await started;
    child.kill.mockClear();
    const contents = fakeContents();
    host.wireAudiocapRendererLoss(contents as never);
    return { host, child, contents };
  }

  it('kills a capturing child when the renderer process goes away', async () => {
    const { child, contents } = await capturing();
    contents.handlers['render-process-gone']?.({}, { reason: 'crashed', exitCode: 1 });
    expect(child.kill).toHaveBeenCalled();
  });

  it('kills it once a main-frame navigation commits (reload)', async () => {
    const { child, contents } = await capturing();
    contents.handlers['did-navigate']?.({}, 'https://example.invalid/', 200, 'OK');
    expect(child.kill).toHaveBeenCalled();
  });

  it('does NOT kill on did-start-navigation — a navigation will-navigate may still cancel', async () => {
    const { child, contents } = await capturing();
    // The absence is the contract: `did-start-navigation` fires BEFORE
    // `will-navigate`, so wiring a kill on it would end the capture for a link
    // click main.ts's `will-navigate` gate goes on to cancel. Without asserting
    // the key is absent, this optional call is vacuous -- it would pass just as
    // well against a handler that silently no-ops.
    expect(contents.handlers['did-start-navigation']).toBeUndefined();
    contents.handlers['did-start-navigation']?.({
      isMainFrame: true,
      isSameDocument: false,
      url: 'x',
    });
    expect(child.kill).not.toHaveBeenCalled();
  });

  it('spares the windowless app-start probe', async () => {
    const host = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    void host.startAudiocapHost(1, null); // still handshaking: the probe's live window
    const contents = fakeContents();
    host.wireAudiocapRendererLoss(contents as never);
    // C11: `?.()` on a registration this same test just made would silently no-op if the
    // wire ever stopped registering a handler -- an existence assertion first turns that
    // into a loud failure instead of a quietly-vacuous pass.
    expect(contents.handlers['render-process-gone']).toBeTypeOf('function');
    expect(contents.handlers['did-navigate']).toBeTypeOf('function');
    contents.handlers['render-process-gone']!({}, { reason: 'crashed', exitCode: 1 });
    contents.handlers['did-navigate']!({}, 'https://example.invalid/', 200, 'OK');
    expect(child.kill).not.toHaveBeenCalled();
  });

  it('registers all three renderer-loss handlers (did-navigate, render-process-gone, did-fail-load)', async () => {
    const host = await loadHost();
    const contents = fakeContents();
    host.wireAudiocapRendererLoss(contents as never);
    // THREE handlers as of the #3394 PR 1 fix: `did-fail-load` closes the VULN-B gap (a
    // main-frame navigation that FAILS commits an error page and never reaches
    // `did-navigate`, so nothing before this fix ever reaped the capture).
    expect(Object.keys(contents.handlers).sort()).toEqual([
      'did-fail-load',
      'did-navigate',
      'render-process-gone',
    ]);
  });

  /**
   * `did-fail-load` (#3394 PR 1 fix, red-team VULN-B). Electron's signature is
   * `(event, errorCode, errorDescription, validatedURL, isMainFrame)`. The handler must
   * count and kill ONLY when `isMainFrame && errorCode !== -3` -- `-3` is `ERR_ABORTED`,
   * which fires on an ordinary cancelled/superseded load (ordinary link navigation, a
   * `will-navigate` cancel) and must never end a live capture.
   */
  describe('did-fail-load ends a capture whose main-frame navigation never commits (#3394 PR 1 fix, VULN-B)', () => {
    /** A capture still in `starting` -- hello sent, `started` not yet acked. */
    async function startingCapture() {
      const host = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      host.setAudiocapPortSink(vi.fn());
      const started = host.startAudiocapHost(1, 42);
      child.handlers.message(validHello());
      const contents = fakeContents();
      host.wireAudiocapRendererLoss(contents as never);
      return { host, child, contents, started };
    }

    it('B1: a main-frame did-fail-load kills a pending capture and settles child-crash', async () => {
      const { child, contents, started } = await startingCapture();
      expect(contents.handlers['did-fail-load']).toBeTypeOf('function');
      contents.handlers['did-fail-load']!(
        {},
        -6,
        'ERR_FILE_NOT_FOUND',
        'https://example.invalid/',
        true,
        1,
        1
      );
      expect(child.kill).toHaveBeenCalled();
      await expect(started).resolves.toEqual({ ok: false, reason: 'child-crash' });
    });

    it('B2: errorCode -3 (ERR_ABORTED) does not kill', async () => {
      const { child, contents } = await capturing();
      expect(contents.handlers['did-fail-load']).toBeTypeOf('function');
      contents.handlers['did-fail-load']!(
        {},
        -3,
        'ERR_ABORTED',
        'https://example.invalid/',
        true,
        1,
        1
      );
      expect(child.kill).not.toHaveBeenCalled();
    });

    it('B3: a subframe did-fail-load (isMainFrame: false) does not kill', async () => {
      const { child, contents } = await capturing();
      expect(contents.handlers['did-fail-load']).toBeTypeOf('function');
      contents.handlers['did-fail-load']!(
        {},
        -6,
        'ERR_FILE_NOT_FOUND',
        'https://example.invalid/',
        false,
        1,
        1
      );
      expect(child.kill).not.toHaveBeenCalled();
    });

    it('B4: spares the windowless app-start probe on a main-frame did-fail-load', async () => {
      const host = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      void host.startAudiocapHost(1, null); // still handshaking: the probe's live window
      const contents = fakeContents();
      host.wireAudiocapRendererLoss(contents as never);
      expect(contents.handlers['did-fail-load']).toBeTypeOf('function');
      contents.handlers['did-fail-load']!(
        {},
        -6,
        'ERR_FILE_NOT_FOUND',
        'https://example.invalid/',
        true,
        1,
        1
      );
      expect(child.kill).not.toHaveBeenCalled();
    });

    // C11: renamed from "B5: the epoch counts did-navigate/render-process-gone and a real
    // did-fail-load, never -3 or a subframe". The BODY only ever fires `did-fail-load` --
    // it never emits a `did-navigate` or `render-process-gone` event -- so the old title
    // claimed coverage the test did not provide. C6's `it.each` below covers the other two
    // events; this case is renamed to describe exactly what its body exercises.
    it('B5: did-fail-load bumps the epoch only for a real main-frame failure, never -3 or a subframe', async () => {
      const { host, contents } = await capturing();
      expect(contents.handlers['did-fail-load']).toBeTypeOf('function');
      const before = host.audiocapRendererLossEpoch();
      contents.handlers['did-fail-load']!({}, -3, 'ERR_ABORTED', 'x', true, 1, 1);
      contents.handlers['did-fail-load']!({}, -6, 'ERR_FILE_NOT_FOUND', 'x', false, 1, 1);
      expect(host.audiocapRendererLossEpoch()).toBe(before);
      contents.handlers['did-fail-load']!({}, -6, 'ERR_FILE_NOT_FOUND', 'x', true, 1, 1);
      expect(host.audiocapRendererLossEpoch()).toBe(before + 1);
    });
  });
});

// ─────────────────────────────────────────────────────────────────────────
// #3394 PR 1 (this PR): renderer-loss/host admission tests written against the
// interface the accompanying engineer PR implements. C6/C9/C10 below are
// COVERAGE tests and must pass against the CURRENT code. R4 (audiocap:stop
// sparing the probe) is a RED test and is expected to fail until the fix
// lands -- see the file-level report for the exact current failure.
// ─────────────────────────────────────────────────────────────────────────

describe('audiocapRendererLossEpoch always bumps on a recognized loss event, live capture or not (#3394 PR 1, C6)', () => {
  function fakeContentsC6() {
    const handlers: Record<string, (...args: unknown[]) => void> = {};
    return {
      handlers,
      on: vi.fn((event: string, cb: (...args: unknown[]) => void) => {
        handlers[event] = cb;
      }),
    };
  }

  function fireLossEvent(
    handlers: Record<string, (...args: unknown[]) => void>,
    event: 'render-process-gone' | 'did-navigate' | 'did-fail-load'
  ): void {
    if (event === 'did-fail-load') {
      handlers[event]!({}, -6, 'ERR_FILE_NOT_FOUND', 'https://example.invalid/', true, 1, 1);
    } else if (event === 'render-process-gone') {
      handlers[event]!({}, { reason: 'crashed', exitCode: 1 });
    } else {
      handlers[event]!({}, 'https://example.invalid/', 200, 'OK');
    }
  }

  it.each(['render-process-gone', 'did-navigate', 'did-fail-load'] as const)(
    '%s bumps the epoch with no session live at all',
    async (event) => {
      const host = await loadHost();
      const contents = fakeContentsC6();
      host.wireAudiocapRendererLoss(contents as never);
      // Never `?.()` on an existence check -- asserting the value first is what makes a
      // future dropped registration a loud failure here rather than a silent no-op below.
      expect(contents.handlers[event]).toBeTypeOf('function');
      const before = host.audiocapRendererLossEpoch();
      fireLossEvent(contents.handlers, event);
      expect(host.audiocapRendererLossEpoch()).toBe(before + 1);
    }
  );

  it.each(['render-process-gone', 'did-navigate', 'did-fail-load'] as const)(
    '%s bumps the epoch while only the windowless probe is live, and does NOT kill the probe',
    async (event) => {
      const host = await loadHost();
      const probeChild = makeChild();
      fork.mockReturnValue(probeChild);
      void host.startAudiocapHost(1, null); // the probe: windowHandle === null
      const contents = fakeContentsC6();
      host.wireAudiocapRendererLoss(contents as never);
      expect(contents.handlers[event]).toBeTypeOf('function');
      const before = host.audiocapRendererLossEpoch();
      fireLossEvent(contents.handlers, event);
      // The epoch bump is unconditional -- it is NOT gated on a live capture existing.
      // `killAudiocapCapture`'s own probe exemption is what spares the child; the epoch
      // itself must still advance so a concurrently in-flight `audiocap:start` can see it.
      expect(host.audiocapRendererLossEpoch()).toBe(before + 1);
      expect(probeChild.kill).not.toHaveBeenCalled();
    }
  );

  it('integration: a did-navigate during a pending audiocap:start enumeration refuses the start and never forks', async () => {
    const host = await loadHost();
    const { handleAudiocapStart } = await import('../../../src/main/ipc/audiocap');
    let release!: (sources: Array<{ id: string; name: string }>) => void;
    const pending = new Promise<Array<{ id: string; name: string }>>((resolve) => {
      release = resolve;
    });
    getSources.mockReturnValueOnce(pending);

    const mainWebContents = { mainFrame: { frameTreeNodeId: 1 } };
    const getMainWindow = () =>
      ({ isDestroyed: () => false, webContents: mainWebContents }) as never;
    const trusted = {
      senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 1 },
      sender: mainWebContents,
    } as never;

    const contents = { handlers: {} as Record<string, (...a: unknown[]) => void>, on: vi.fn() };
    (contents.on as ReturnType<typeof vi.fn>).mockImplementation(
      (event: string, cb: (...a: unknown[]) => void) => {
        contents.handlers[event] = cb;
      }
    );
    host.wireAudiocapRendererLoss(contents as never);

    const pendingStart = handleAudiocapStart(
      trusted,
      { sourceId: 'window:42:0' },
      () => null,
      getMainWindow
    );
    expect(contents.handlers['did-navigate']).toBeTypeOf('function');
    contents.handlers['did-navigate']!({}, 'https://example.invalid/', 200, 'OK');
    release([{ id: 'window:42:0', name: 'Some App' }]);

    await expect(pendingStart).resolves.toEqual({ ok: false, reason: 'target-unresolved' });
    expect(fork).not.toHaveBeenCalled();
  });
});

/**
 * THE PROBE'S OWN-SESSION FENCE (#3394 PR 1 fix, red-team VULN-C).
 *
 * `runCapabilityProbe`'s `finally` used to call `killAudiocapHost()` UNCONDITIONALLY --
 * so a share that superseded an in-flight probe (I3) was reaped by the probe's own
 * continuation resuming afterward, silently killing a live, correctly-started capture with
 * no signal to its caller. The fix records the probe's own session and kills only when that
 * session is still the live one, or when none was recorded.
 */
describe('the probe reaps only its own session (#3394 PR 1 fix, red-team VULN-C)', () => {
  it('C1: a share started while the probe is mid-handshake is not killed when the probe settles', async () => {
    const host = await loadHost();
    const probeChild = makeChild();
    const shareChild = makeChild();
    fork.mockReturnValueOnce(probeChild).mockReturnValueOnce(shareChild);
    host.setAudiocapPortSink(vi.fn());

    const probe = host.probeAudiocapCapability(); // forks, still handshaking
    const share = host.startAudiocapHost(7, 42); // I3 supersedes the probe's session
    expect(host.currentAudiocapGeneration()).toBe(7);

    // C11: await the REAL probe promise rather than counting microtask ticks by hand.
    // `runCapabilityProbe` is an async function with its own `await` + `finally`, so a
    // fixed tick count is a guess at how many microtasks its continuation needs -- too
    // few silently checks state before the continuation ran (vacuous), too many is inert
    // padding nobody can explain. Awaiting the promise directly waits for EXACTLY as many
    // microtasks as the continuation needs, however many that is.
    await expect(probe).resolves.toEqual({ ok: false, reason: 'child-crash' });

    expect(shareChild.kill).not.toHaveBeenCalled();
    expect(host.currentAudiocapGeneration()).toBe(7);

    shareChild.handlers.message(validHello());
    shareChild.handlers.message(STARTED);
    await expect(share).resolves.toEqual({ ok: true, generation: 7, perProcessAudio: true });
  });

  it('C2 (regression): an undisturbed probe still reaps its own child', async () => {
    const host = await loadHost();
    const probeChild = makeChild();
    fork.mockReturnValue(probeChild);
    const probe = host.probeAudiocapCapability();
    probeChild.handlers.message(validHello());
    await expect(probe).resolves.toEqual({ ok: true, perProcessAudio: true });
    expect(probeChild.kill).toHaveBeenCalled();
    expect(host.currentAudiocapGeneration()).toBe(0);
  });
});

describe('capability notification fires on the capture path too, exactly once (#3394 PR 1, C9)', () => {
  it('notifies before started settles, and does not notify again once it does', async () => {
    const { startAudiocapHost, setAudiocapPortSink, setAudiocapCapabilityListener } =
      await loadHost();
    const spy = vi.fn();
    setAudiocapCapabilityListener(spy);
    const child = makeChild();
    fork.mockReturnValue(child);
    setAudiocapPortSink(vi.fn());

    const started = startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    // BEFORE `started`: the hello arm notifies on every branch, including the one that
    // enters `starting` and returns without settling (#3394's whole point -- a capture no
    // longer settles at hello). A notify dropped from that branch (M21) would leave this
    // assertion unmet while the promise is still pending.
    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith(true);

    child.handlers.message(STARTED);
    await started;
    // UNCHANGED after started: `handleStarted` carries no second notify call, and the
    // snapshot is monotone -- a duplicate push here would not be a correctness bug on
    // its own, but a change in call count is exactly what a mutated call site produces.
    expect(spy).toHaveBeenCalledTimes(1);
  });
});

describe('timer hygiene: no leaked timers after a starting-state exit (#3394 PR 1, C10)', () => {
  it('a fault while starting leaves zero pending timers', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink } = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());
      const started = startAudiocapHost(1, 42);
      child.handlers.message(validHello());
      child.handlers.message({ kind: 'fault', stage: 'start', message: 'refused' });
      await started;
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a renderer-loss kill during starting (B1-style) leaves zero pending timers', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink, wireAudiocapRendererLoss } = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());
      const started = startAudiocapHost(1, 42);
      child.handlers.message(validHello()); // 'starting' -- the ack timer is armed here
      const handlers: Record<string, (...a: unknown[]) => void> = {};
      const contents = {
        on: vi.fn((event: string, cb: (...a: unknown[]) => void) => {
          handlers[event] = cb;
        }),
      };
      wireAudiocapRendererLoss(contents as never);
      handlers['did-fail-load']!({}, -6, 'ERR_FILE_NOT_FOUND', 'https://example.invalid/', true);
      await started;
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a supersede kill while starting leaves zero pending timers (not via stopAudiocapHost)', async () => {
    vi.useFakeTimers();
    try {
      const { startAudiocapHost, setAudiocapPortSink, killAudiocapHost } = await loadHost();
      const child = makeChild();
      fork.mockReturnValue(child);
      setAudiocapPortSink(vi.fn());
      void startAudiocapHost(1, 42);
      child.handlers.message(validHello()); // 'starting' -- the ack timer is armed here
      // `killAudiocapHost` is the EXACT primitive `startAudiocapHost`'s I3 supersede calls
      // first, before it forks a replacement. Driven directly (rather than by starting a
      // second host) so this test is not confounded by a second child's own fresh
      // handshake timer, which would legitimately leave the timer count non-zero.
      killAudiocapHost();
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('audiocap:stop spares the windowless app-start probe (#3394 PR 1, R4)', () => {
  /**
   * RED. `handleAudiocapStop` currently calls `stopAudiocapHost()` unconditionally, which
   * ends WHATEVER session is live -- the app-start probe included. Unlike a share
   * superseding the probe (which self-heals from the SHARE's own `hello`), a stop here
   * never heals: no child ever says `hello`, `audiocapProbeResult()`/
   * `audiocapMachineCapability()` memoize the `child-crash` outcome, and per-process audio
   * is permanently unavailable for the rest of the process's life. See
   * `[internal]worktrees/agent-af5fded6fe6f85402/client/desktop/tests/unit/main/redteam3394/poc2-stop-reaps-probe.test.ts`
   * for the full red-team writeup this test converts into a permanent regression lock.
   */
  it('a renderer audiocap:stop during the probe handshake does not end the probe', async () => {
    const host = await loadHost();
    const { handleAudiocapStop } = await import('../../../src/main/ipc/audiocap');
    const probeChild = makeChild();
    fork.mockReturnValue(probeChild);

    const probe = host.probeAudiocapCapability(); // app start: forks, handshaking
    expect(fork).toHaveBeenCalledTimes(1);

    const mainWebContents = { mainFrame: { frameTreeNodeId: 1 } };
    const getMainWindow = () =>
      ({ isDestroyed: () => false, webContents: mainWebContents }) as never;
    const trusted = {
      senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 1 },
      sender: mainWebContents,
    } as never;
    handleAudiocapStop(trusted, () => null, getMainWindow);

    probeChild.handlers.message(validHello());
    await expect(probe).resolves.toEqual({ ok: true, perProcessAudio: true });
    expect(host.audiocapMachineCapability()).toBe(true);
    // Never re-asked: the machine capability question was answered once, by the probe
    // that just survived.
    expect(fork).toHaveBeenCalledTimes(1);
  });

  it('control: a stop during a REAL capture still stops it', async () => {
    const host = await loadHost();
    const { handleAudiocapStop } = await import('../../../src/main/ipc/audiocap');
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());
    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);
    await started;
    child.postMessage.mockClear();

    const mainWebContents = { mainFrame: { frameTreeNodeId: 1 } };
    const getMainWindow = () =>
      ({ isDestroyed: () => false, webContents: mainWebContents }) as never;
    const trusted = {
      senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 1 },
      sender: mainWebContents,
    } as never;
    handleAudiocapStop(trusted, () => null, getMainWindow);

    // The GRACEFUL stop, not a bare kill (electron.md "IPC contract v28" / #3198 PR 3
    // rationale): a real capture must still be ended by a trusted stop.
    expect(child.postMessage).toHaveBeenCalledWith({ kind: 'stop' });
  });
});

// ─────────────────────────────────────────────────────────────────────────
// #3394 PR 2, Task T3 Step 2: mid-share interrupt publication.
//
// `setAudiocapInterruptListener` does not exist yet -- every case below fails
// with "setAudiocapInterruptListener is not a function" (or an assertion on a
// value that can never be produced) until audiocapHost.ts implements it. That
// is the expected RED per superpowers:test-driven-development.
// ─────────────────────────────────────────────────────────────────────────

describe('mid-share interrupt publication (#3394 PR 2)', () => {
  /**
   * Drives a session from fork through `hello` -> `start` -> `started`, i.e. into
   * `'capturing'`, and registers `onInterrupted` via `setAudiocapInterruptListener`
   * -- mirroring the `capturing()` / `helloWithWindow()` helpers used earlier in
   * this file (renderer-loss describe, "start settles on the started ack" describe).
   */
  async function capturing(onInterrupted: (interrupt: unknown) => void = vi.fn()) {
    const host = await loadHost();
    host.setAudiocapInterruptListener(onInterrupted);
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());
    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);
    await expect(started).resolves.toEqual({ ok: true, generation: 1, perProcessAudio: true });
    child.kill.mockClear();
    return { host, child, onInterrupted };
  }

  // Mutation guard: dropping the `'run'` arm of `interruptReasonFor` (or leaving
  // `reasonForFaultStage` unnarrowed so it still accepts `'run'`) turns the first
  // row red; dropping the `stage === 'protocol'` / start-phase-while-capturing /
  // unknown-message arms of the interrupt mapping in `retire()`'s callers turns
  // the remaining rows red.
  it.each([
    ['run', 'capture-interrupted'],
    ['protocol', 'protocol-fault'],
    ['target', 'protocol-fault'],
  ] as const)(
    "in 'capturing', a fault{%s} publishes %s once and reaps the child",
    async (stage, reason) => {
      const onInterrupted = vi.fn();
      const { child } = await capturing(onInterrupted);

      child.handlers.message({ kind: 'fault', stage, message: 'x' });

      expect(onInterrupted).toHaveBeenCalledTimes(1);
      expect(onInterrupted).toHaveBeenCalledWith({ generation: 1, reason });
      expect(child.kill).toHaveBeenCalled();
    }
  );

  // Mutation guard: dropping the `'child-crash'` arm of the exit-site `retire()`
  // call (audiocapHost.ts, the `child.on('exit', ...)` wiring) turns this red.
  it("in 'capturing', a child exit publishes child-crash once and reaps", async () => {
    const onInterrupted = vi.fn();
    const { child } = await capturing(onInterrupted);

    child.handlers.exit(1);

    expect(onInterrupted).toHaveBeenCalledTimes(1);
    expect(onInterrupted).toHaveBeenCalledWith({ generation: 1, reason: 'child-crash' });
    expect(child.kill).toHaveBeenCalled();
  });

  // Mutation guard: dropping the "unknown message" fallback's interrupt argument
  // in `handleChildMessage` (audiocapHost.ts) turns this red.
  it("in 'capturing', an unknown message publishes protocol-fault once and reaps", async () => {
    const onInterrupted = vi.fn();
    const { child } = await capturing(onInterrupted);

    child.handlers.message({ kind: 'nonsense' });

    expect(onInterrupted).toHaveBeenCalledTimes(1);
    expect(onInterrupted).toHaveBeenCalledWith({ generation: 1, reason: 'protocol-fault' });
    expect(child.kill).toHaveBeenCalled();
  });

  // Mutation guard: dropping the interrupt argument on `handleStarted`'s
  // illegal-state `retire()` call turns this red.
  it("in 'capturing', a duplicate started publishes protocol-fault once and reaps", async () => {
    const onInterrupted = vi.fn();
    const { child } = await capturing(onInterrupted);

    child.handlers.message(STARTED);

    expect(onInterrupted).toHaveBeenCalledTimes(1);
    expect(onInterrupted).toHaveBeenCalledWith({ generation: 1, reason: 'protocol-fault' });
    expect(child.kill).toHaveBeenCalled();
  });

  // Mutation guard: mapping a `'run'` fault arriving in `'starting'` to
  // `capture-interrupted` (rather than refusing it as `protocol-fault`, per
  // spec §4.3 "Stage legality") turns the reason assertion red; publishing it
  // anyway turns the `not.toHaveBeenCalled()` assertion red.
  it("a fault{run} while 'starting' settles protocol-fault and publishes nothing", async () => {
    const host = await loadHost();
    const onInterrupted = vi.fn();
    host.setAudiocapInterruptListener(onInterrupted);
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());

    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message({ kind: 'fault', stage: 'run', message: 'x' });

    await expect(started).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(onInterrupted).not.toHaveBeenCalled();
  });

  // Mutation guard: passing a non-null interrupt reason from `stopAudiocapHost`'s
  // `retire`/`settleOnce` call turns this red.
  it('stopAudiocapHost publishes nothing', async () => {
    vi.useFakeTimers();
    try {
      const onInterrupted = vi.fn();
      const { host, child } = await capturing(onInterrupted);

      host.stopAudiocapHost();
      vi.advanceTimersByTime(200);

      expect(onInterrupted).not.toHaveBeenCalled();
      expect(child.kill).toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  // Mutation guard: passing a non-null interrupt reason from `killAudiocapHost`'s
  // `retire`-shaped teardown turns this red.
  it('killAudiocapHost publishes nothing', async () => {
    const onInterrupted = vi.fn();
    const { host, child } = await capturing(onInterrupted);

    host.killAudiocapHost();

    expect(onInterrupted).not.toHaveBeenCalled();
    expect(child.kill).toHaveBeenCalled();
  });

  // Mutation guard: same as `killAudiocapHost` above -- `startAudiocapHost`'s I3
  // supersede calls `killAudiocapHost` internally, so this pins the same call
  // site from the caller a live share actually uses.
  it('a superseding startAudiocapHost publishes nothing for the superseded session', async () => {
    const onInterrupted = vi.fn();
    const { host, child } = await capturing(onInterrupted);

    const second = makeChild();
    fork.mockReturnValue(second);
    void host.startAudiocapHost(2, 99);

    expect(onInterrupted).not.toHaveBeenCalled();
    expect(child.kill).toHaveBeenCalled();
  });

  // Mutation guard: passing a non-null interrupt reason from `killAudiocapCapture`
  // (or from the `killAudiocapHost` call it wraps) turns this red.
  it('killAudiocapCapture publishes nothing', async () => {
    const onInterrupted = vi.fn();
    const { host, child } = await capturing(onInterrupted);

    host.killAudiocapCapture();

    expect(onInterrupted).not.toHaveBeenCalled();
    expect(child.kill).toHaveBeenCalled();
  });

  // The app-start capability probe (`windowHandle === null`) never reaches
  // `'capturing'` at all -- see the `probeAudiocapCapability` describe block
  // above -- so this is a belt-and-braces pin on the call-site table in the
  // plan (spec §4.3 "Never publish": "the probe"), not a case that currently
  // exercises `retire()`'s publish gate the way the `'capturing'` cases do.
  it('killing the app-start capability probe mid-handshake publishes nothing', async () => {
    const host = await loadHost();
    const onInterrupted = vi.fn();
    host.setAudiocapInterruptListener(onInterrupted);
    const child = makeChild();
    fork.mockReturnValue(child);

    const probe = host.probeAudiocapCapability();
    host.killAudiocapHost();
    await probe;

    expect(onInterrupted).not.toHaveBeenCalled();
  });

  // C14-shaped: mirrors "a started from a child already being stopped is dropped
  // without a reap" earlier in this file. Mutation guard: removing the
  // `session !== live` guard on the child's `'message'` listener (audiocapHost.ts)
  // turns BOTH assertions red -- the stale `live` would reach `handleChildMessage`
  // and `retire` would reap and (post-implementation) publish.
  it('a fault{run} from a detached stopping child publishes nothing and does not reap', async () => {
    vi.useFakeTimers();
    try {
      const onInterrupted = vi.fn();
      const { host, child } = await capturing(onInterrupted);

      host.stopAudiocapHost();
      child.kill.mockClear();
      onInterrupted.mockClear();

      child.handlers.message({ kind: 'fault', stage: 'run', message: 'x' });

      expect(onInterrupted).not.toHaveBeenCalled();
      expect(child.kill).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  // The drop is LOGGED (observability.md principle 3 constrains the ERROR OBJECT,
  // not the fact of a drop), mirroring the existing capability-listener case
  // above in this file. Mutation guard: logging the caught value instead of the
  // fixed string turns the "does not contain" assertion red; removing the
  // try/catch around `onInterrupted?.(...)` in `retire()` turns the whole case
  // red (the throw would propagate out of the fault handler).
  it('a throwing listener does not break retire, and console.warn logs a fixed string with no error object', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const onInterrupted = vi.fn(() => {
      throw new Error('SECRET-CAUSE-SHOULD-NOT-BE-LOGGED');
    });
    const { child } = await capturing(onInterrupted);

    expect(() => child.handlers.exit(1)).not.toThrow();

    expect(child.kill).toHaveBeenCalled();
    expect(warn).toHaveBeenCalledWith('[audiocap] interrupt listener threw');
    const logged = warn.mock.calls.flat().map(String).join(' ');
    expect(logged).not.toContain('SECRET-CAUSE-SHOULD-NOT-BE-LOGGED');
    warn.mockRestore();
  });

  // Mutation guard: if `retire()` reads a captured `live`/`session` variable
  // AFTER the re-entrant `startAudiocapHost(2, ...)` call inside the listener has
  // already replaced `session`, `fork` is called once (the re-entrant start never
  // runs) or the second `startAudiocapHost` throws/double-settles. Correct
  // behaviour: the old session is fully forgotten (settled + reaped) BEFORE the
  // listener runs, so a re-entrant start proceeds normally and forks a second
  // child with no double settle on either promise.
  it('a re-entrant listener calling startAudiocapHost runs after the old session is forgotten, without a double settle', async () => {
    const host = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());

    const second = makeChild();
    let secondStarted: Promise<unknown> | undefined;
    const onInterrupted = vi.fn(() => {
      fork.mockReturnValue(second);
      secondStarted = host.startAudiocapHost(2, 99);
    });
    host.setAudiocapInterruptListener(onInterrupted);

    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);
    await started;
    child.kill.mockClear();

    expect(() =>
      child.handlers.message({ kind: 'fault', stage: 'run', message: 'x' })
    ).not.toThrow();

    expect(onInterrupted).toHaveBeenCalledTimes(1);
    expect(fork).toHaveBeenCalledTimes(2);
    expect(secondStarted).toBeDefined();
    second.handlers.message(validHello());
    second.handlers.message(STARTED);
    await expect(secondStarted).resolves.toEqual({
      ok: true,
      generation: 2,
      perProcessAudio: true,
    });
    // The FIRST start must not settle a second time (no unhandled "already
    // settled" throw, and no interference with the second promise's own value):
    // it still holds the ok:true it resolved with at `started`.
    await expect(started).resolves.toEqual({ ok: true, generation: 1, perProcessAudio: true });
  });

  // PUBLISH DECIDED AT ENTRY (spec §4.3, plan Step 5's named falsification):
  // `retire()` must compute its publish decision from the state `live` arrived
  // in, BEFORE any mutation -- never from `session`/`hostState` re-read after
  // `session = null; hostState = nextState` has already run. A mutant that moves
  // the `publish` computation below that assignment reads `session === live` as
  // false post-mutation and this case's `onInterrupted` call count goes to 0.
  it('publish is decided at entry, not re-derived after session is cleared (mutation guard)', async () => {
    const onInterrupted = vi.fn();
    const { child } = await capturing(onInterrupted);

    child.handlers.message({ kind: 'fault', stage: 'run', message: 'x' });

    expect(onInterrupted).toHaveBeenCalledTimes(1);
    expect(onInterrupted).toHaveBeenCalledWith({ generation: 1, reason: 'capture-interrupted' });
  });
});

// A SIBLING describe, not nested inside the one above: it does not inherit any
// local `beforeEach` the interrupt-publication suite might carry, and it never
// calls `setAudiocapInterruptListener`. It independently drives the same
// fork -> hello -> start -> started sequence the `capturing()` helper above
// uses, and asserts on the START PROMISE's resolved value -- proof, from a
// completely separate setup, that the harness genuinely reaches `'capturing'`
// (rather than merely not throwing) before any interrupt-publish case above
// relies on it. Per tests.md "Vacuity": a fixture that never actually reaches
// the state under test would let every "publishes nothing" case in the suite
// above pass for the wrong reason.
describe('mid-share interrupt publication — the harness genuinely reaches capturing (#3394 PR 2 control)', () => {
  it("hello -> start -> started resolves ok:true, proving hostState really becomes 'capturing'", async () => {
    const host = await loadHost();
    const child = makeChild();
    fork.mockReturnValue(child);
    host.setAudiocapPortSink(vi.fn());

    const started = host.startAudiocapHost(1, 42);
    child.handlers.message(validHello());
    child.handlers.message(STARTED);

    await expect(started).resolves.toEqual({ ok: true, generation: 1, perProcessAudio: true });

    // Independent corroboration of "capturing", not just "settled ok": a fault
    // delivered post-settle still reaches the child (rather than being dropped
    // the way a detached/stopping child's message is -- see the `session !==
    // live` guard tested elsewhere in this file), which is only possible while
    // this session is still the live one.
    child.kill.mockClear();
    child.handlers.message({ kind: 'fault', stage: 'protocol', message: 'x' });
    expect(child.kill).toHaveBeenCalled();
  });
});
