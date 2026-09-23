// @vitest-environment node
/**
 * Tests for the `audiocap:start` invoke (#3198 PR 3, plan Task 10).
 *
 * This is the ONE handler the #3198 sender-frame criterion binds (A4, R5). PR 1's
 * `audiocap:capability` is a `webContents.send` PUSH with no sender to validate,
 * so the absence of a check there is design rather than oversight.
 *
 * ONE CASE PER FENCE, and that is the point of the file. The handler's docblock
 * names four fences in a fixed order; a fence with no case is a fence a future
 * edit can delete while the suite stays green -- the exact shape #3198 PR 2's
 * review found four times (dead `:disabled` CSS, the half-applied sanitiser, the
 * Linux copy mirror, the parameter with no call site).
 *
 * Mocking convention follows `tests/unit/main/audiocapHost.test.ts`.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';

const startAudiocapHost = vi.fn();
const stopAudiocapHost = vi.fn();
/**
 * Mocked although `audiocap.ts` never imports it, so the "graceful, never kill" case can
 * assert its ABSENCE. A spy that the module cannot reach proves the negative it is there
 * to prove — and if a future edit does reach for it, that case goes red rather than the
 * module silently swapping a reap for a stop.
 */
const killAudiocapHost = vi.fn();
const getSources = vi.fn();
const handle = vi.fn();
/**
 * The renderer-loss epoch (#3394 PR 1 fix, red-team VULN-A). A fake counter the test
 * drives directly to stand in for `wireAudiocapRendererLoss` firing on the real host --
 * this file mocks `audiocapHost` wholesale, so there is no real handler to trigger. What
 * matters here is only whether `handleAudiocapStart` READS this value after its await,
 * not how it got bumped.
 */
const audiocapRendererLossEpoch = vi.fn();

vi.mock('electron', () => ({
  ipcMain: { handle: (...a: unknown[]) => handle(...a) },
  desktopCapturer: { getSources: (...a: unknown[]) => getSources(...a) },
}));

vi.mock('../../../src/main/audiocapHost', () => ({
  startAudiocapHost: (...a: unknown[]) => startAudiocapHost(...a),
  stopAudiocapHost: (...a: unknown[]) => stopAudiocapHost(...a),
  killAudiocapHost: (...a: unknown[]) => killAudiocapHost(...a),
  currentAudiocapGeneration: () => 1,
  audiocapRendererLossEpoch: (...a: unknown[]) => audiocapRendererLossEpoch(...a),
}));

/**
 * Import fresh per case.
 *
 * NOT decoration: `shareGeneration` is module-local and monotonic, so without a
 * reset the generation a case observes depends on how many EARLIER cases happened
 * to reach the host. Pinning `1` against a shared module would make this file
 * order-coupled -- green today, red the moment a case is inserted above.
 */
async function loadIpc() {
  vi.resetModules();
  return import('../../../src/main/ipc/audiocap');
}

/**
 * THE MAIN WINDOW'S WEB CONTENTS (#3394 PR 1, R5). A plain object identity stand-in for
 * `BrowserWindow.webContents` -- `handleAudiocapStart`/`handleAudiocapStop` admission
 * compares `event.sender` against `getMainWindow()?.webContents` BY REFERENCE, and
 * `event.senderFrame.frameTreeNodeId` against `mainWebContents.mainFrame.frameTreeNodeId`.
 */
const mainWebContents = { mainFrame: { frameTreeNodeId: 1 } };
const getMainWindow = () => ({ isDestroyed: () => false, webContents: mainWebContents }) as never;

/** A sender frame the allowlist accepts, FROM THE MAIN WINDOW'S OWN MAIN FRAME. */
const trustedEvent = {
  senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 1 },
  sender: mainWebContents,
} as never;

/**
 * FOURTH ARGUMENT, REQUIRED (#3394 PR 1, R5). `registerAudiocapIpc(getRemoteSpaOrigin,
 * getMainWindow)`, `handleAudiocapStart(event, request, getRemoteSpaOrigin, getMainWindow)`,
 * `handleAudiocapStop(event, getRemoteSpaOrigin, getMainWindow)`. Admission is now
 * `requireTrustedSender(...)` AND `event.sender === getMainWindow()?.webContents` AND the
 * sender frame is that webContents' own main frame (by `frameTreeNodeId`); a null
 * `senderFrame` is refused, matching every other admission fence in this module.
 */
const getRemoteSpaOrigin = () => 'app://concord';

beforeEach(() => {
  vi.clearAllMocks();
  getSources.mockResolvedValue([{ id: 'window:42:0', name: 'Some App' }]);
  startAudiocapHost.mockResolvedValue({ ok: true, generation: 1, perProcessAudio: true });
  audiocapRendererLossEpoch.mockReset();
  audiocapRendererLossEpoch.mockReturnValue(0);
});

describe('audiocap:start — fence 1, the sender frame (A4, spec test 4)', () => {
  it('refuses an untrusted origin', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const evt = { senderFrame: { url: 'https://evil.example/' } } as never;
    const r = await handleAudiocapStart(
      evt,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('refuses a null senderFrame', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const evt = { senderFrame: null } as never;
    const r = await handleAudiocapStart(
      evt,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('reads the frame BEFORE the enumeration, so a hostile sender costs no OS work', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const evt = { senderFrame: { url: 'https://evil.example/' } } as never;
    await handleAudiocapStart(evt, { sourceId: 'window:42:0' }, getRemoteSpaOrigin, getMainWindow);
    // The ORDER is the design (A4): a validation that runs after other work is a
    // validation a future edit can reorder past. Asserting the enumeration never
    // ran is how that ordering is observable from outside the function.
    expect(getSources).not.toHaveBeenCalled();
  });
});

describe('audiocap:start — fence 2, the payload shape', () => {
  it.each([
    ['a non-string sourceId', { sourceId: 42 }],
    ['a missing sourceId', {}],
    ['a null payload', null],
  ])('refuses %s with protocol-fault, not target-unresolved', async (_label, payload) => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      trustedEvent,
      payload as never,
      getRemoteSpaOrigin,
      getMainWindow
    );
    // `protocol-fault`, NOT `target-unresolved`: a non-record payload or a
    // non-string sourceId is a CONTRACT violation, not a target problem.
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });
});

describe('audiocap:start — fences 3 and 4, D6 main-side (A5, spec test 3)', () => {
  it('refuses a screen: id ON FENCE 3, with fence 4 deliberately satisfied', async () => {
    const { handleAudiocapStart } = await loadIpc();
    // THE ENUMERATION CONTAINS THE SCREEN ID, and that is the whole point of this
    // fixture. With the default enumeration (`window:42:0` only) fence 4 would refuse
    // `screen:0:0` as absent, so the case passed whether or not fence 3 existed — a
    // fixture that can fail for two reasons pins neither (tests.md § Vacuity). Listing
    // the id makes fence 4 admit it, so ONLY the `window:` prefix check can produce the
    // refusal.
    getSources.mockResolvedValue([{ id: 'screen:0:0', name: 'Entire Screen' }]);
    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'screen:0:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'target-unresolved' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
    // And prove fence 4 really was satisfied rather than silently doing the work: a
    // `screen:` id never reaches the enumeration at all, because fence 3 returns first.
    expect(getSources).not.toHaveBeenCalled();
  });

  it('refuses a well-formed id for a window absent from the live enumeration', async () => {
    const { handleAudiocapStart } = await loadIpc();
    getSources.mockResolvedValue([{ id: 'window:99:0', name: 'Other App' }]);
    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'target-unresolved' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('enumerates without thumbnails, so the staleness check is not a screenshot', async () => {
    const { handleAudiocapStart } = await loadIpc();
    await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(getSources).toHaveBeenCalledWith({
      types: ['window'],
      thumbnailSize: { width: 0, height: 0 },
      fetchWindowIcons: false,
    });
  });
});

describe('audiocap:start — what reaches the host', () => {
  it('ignores an extra systemAudio field rather than honouring it', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0', systemAudio: true } as never,
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: true, generation: 1, perProcessAudio: true });
    // OBEYED, NOT MERELY PASSED (spec test 9): assert what reached the host, not
    // that an options object was shaped a certain way.
    expect(startAudiocapHost).toHaveBeenCalledWith(1, 42);
    expect(JSON.stringify(startAudiocapHost.mock.calls[0])).not.toContain('systemAudio');
  });

  it('VARIES the handle: a different window id reaches the host as a different handle', async () => {
    const { handleAudiocapStart } = await loadIpc();
    getSources.mockResolvedValue([{ id: 'window:1337:0', name: 'Another App' }]);
    await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:1337:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    // `[internal]rules/tests.md` § "a branch that is OBEYED must be proven REACHED":
    // a single-handle assertion proves the argument EXISTS. Only varying it at the
    // caller proves the value is CARRIED rather than constant-folded. This is the
    // rule #3198 PR 2 wrote after `screenAudioRefusalMessage` gained a `platform`
    // parameter that no call site ever varied.
    expect(startAudiocapHost).toHaveBeenCalledWith(1, 1337);
  });

  it('mints a NEW generation per share, so a stale result is distinguishable', async () => {
    const { handleAudiocapStart } = await loadIpc();
    await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(startAudiocapHost.mock.calls.map((c) => c[0])).toEqual([1, 2]);
    // NOT `currentAudiocapGeneration()`, which returns the LIVE child's number --
    // 0 whenever nothing is live, which is every share start. The mock above
    // returns a constant 1 precisely so that a handler wired to it would fail here.
  });
});

/**
 * RED-TEAM REGRESSION LOCK (#3394 PR 1 fix). Confirmed findings: a renderer that goes away
 * WHILE `desktopCapturer.getSources` is pending -- via `did-navigate` or `render-process-gone`
 * -- lets the enumeration resolve into a start for a document that no longer exists (VULN-A);
 * a `senderFrame` that goes null during the same window, with no loss event, slips past
 * because fence 1 is read only once, before the await.
 *
 * The fix snapshots `audiocapRendererLossEpoch()` beside the existing `operationEpoch`, and
 * after the enumeration re-checks BOTH epochs (falling to `target-unresolved`, matching the
 * pre-existing stop-during-enumeration branch) and then re-runs the sender fence (falling to
 * `protocol-fault`). This file mocks `audiocapHost` wholesale (see the file header), so a real
 * `wireAudiocapRendererLoss` handler cannot fire here -- `audiocapRendererLossEpoch` is driven
 * directly to stand in for it. What is under test is only whether `handleAudiocapStart` READS
 * the value after its await; `tests/unit/main/audiocapHost.test.ts` covers whether the real
 * host handlers correctly bump it.
 */
describe('audiocap:start — renderer-loss and post-await sender re-check (#3394 PR 1 fix)', () => {
  /** Defers `getSources` so the test can act while the enumeration is still in flight. */
  function deferredSources() {
    let release!: (sources: Array<{ id: string; name: string }>) => void;
    const pending = new Promise<Array<{ id: string; name: string }>>((resolve) => {
      release = resolve;
    });
    getSources.mockImplementationOnce(() => pending);
    return (
      sources: Array<{ id: string; name: string }> = [{ id: 'window:42:0', name: 'Some App' }]
    ) => release(sources);
  }

  /** A trusted `IpcMainInvokeEvent` whose `senderFrame` can be made to go null mid-call. */
  function flippableTrustedEvent() {
    let gone = false;
    return {
      // Carries the main window's identity so A3 passes the FIRST fence and reaches the
      // post-await re-check it exists to exercise; without it the new main-frame check
      // refuses at the top and the test goes green without testing anything.
      event: {
        sender: mainWebContents,
        get senderFrame() {
          return gone ? null : { url: 'app://concord/index.html', frameTreeNodeId: 1 };
        },
      } as never,
      documentGone: () => {
        gone = true;
      },
    };
  }

  it('A1: a did-navigate during the pending enumeration refuses the start (target-unresolved, no fork)', async () => {
    const release = deferredSources();
    const { handleAudiocapStart } = await loadIpc();
    const pending = handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    // Stand-in for `did-navigate` firing on the wired contents while the enumeration is
    // still pending: the real handler would bump `audiocapRendererLossEpoch()` before
    // calling `killAudiocapCapture()`.
    audiocapRendererLossEpoch.mockReturnValue(1);
    release();
    await expect(pending).resolves.toEqual({ ok: false, reason: 'target-unresolved' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  // C11: renamed from "A2: a render-process-gone during the pending enumeration refuses
  // the start the same way". This file mocks `audiocapHost` wholesale (see the header),
  // so `audiocapRendererLossEpoch` is a bare counter this case drives directly -- it
  // cannot involve a REAL `render-process-gone` event, and its body is byte-identical to
  // A1's. What it actually proves is that the FENCE reacts to the epoch moving at all,
  // regardless of which event bumped it -- a duplicate of A1 at the fence level, kept
  // because that duplication is itself the point: the handler cannot distinguish (and
  // must not try to) which loss event produced the bump.
  it('A2 (duplicate of A1 by design): the epoch fence reacts to ANY bump, not to a specific loss event', async () => {
    const release = deferredSources();
    const { handleAudiocapStart } = await loadIpc();
    const pending = handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    // Stand-in for `render-process-gone` -- same counter, same fence.
    audiocapRendererLossEpoch.mockReturnValue(1);
    release();
    await expect(pending).resolves.toEqual({ ok: false, reason: 'target-unresolved' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('A3: senderFrame goes null during the pending enumeration with NO loss event refuses protocol-fault', async () => {
    const release = deferredSources();
    const { handleAudiocapStart } = await loadIpc();
    const { event, documentGone } = flippableTrustedEvent();
    const pending = handleAudiocapStart(
      event,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    // The renderer-loss epoch never moves here -- this is the OTHER way the sender can go
    // away mid-call, which the epoch fence alone cannot see.
    documentGone();
    release();
    await expect(pending).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('A4 (regression): an undisturbed start still resolves ok:true', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: true, generation: 1, perProcessAudio: true });
    expect(startAudiocapHost).toHaveBeenCalledWith(1, 42);
  });
});

describe('registerAudiocapIpc', () => {
  it('registers exactly the two audiocap channels', async () => {
    const { registerAudiocapIpc } = await loadIpc();
    registerAudiocapIpc(getRemoteSpaOrigin, getMainWindow);
    // EXACTLY, not "at least". A third channel appearing here is a widened renderer→main
    // surface, which is the thing this assertion exists to make visible in review rather
    // than a count worth keeping accurate for its own sake.
    expect(handle.mock.calls.map((c) => c[0])).toEqual(['audiocap:start', 'audiocap:stop']);
  });
});

describe('audiocap:stop', () => {
  it('cancels a pending start before it can invoke the native host', async () => {
    let releaseSources!: (sources: Array<{ id: string; name: string }>) => void;
    const sourcesReady = new Promise<Array<{ id: string; name: string }>>((resolve) => {
      releaseSources = resolve;
    });
    getSources.mockImplementationOnce(() => sourcesReady);

    const { handleAudiocapStart, handleAudiocapStop } = await loadIpc();
    const pendingStart = handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(getSources).toHaveBeenCalledTimes(1);

    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, getMainWindow);
    releaseSources([{ id: 'window:42:0', name: 'Some App' }]);
    await pendingStart;

    expect(
      startAudiocapHost,
      'a trusted audiocap:stop must cancel a pending start before it invokes startAudiocapHost'
    ).not.toHaveBeenCalled();
  });

  it('allows an uncancelled pending start to invoke the native host', async () => {
    let releaseSources!: (sources: Array<{ id: string; name: string }>) => void;
    const sourcesReady = new Promise<Array<{ id: string; name: string }>>((resolve) => {
      releaseSources = resolve;
    });
    getSources.mockImplementationOnce(() => sourcesReady);

    const { handleAudiocapStart } = await loadIpc();
    const pendingStart = handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(startAudiocapHost).not.toHaveBeenCalled();

    releaseSources([{ id: 'window:42:0', name: 'Some App' }]);
    await pendingStart;
    expect(startAudiocapHost).toHaveBeenCalledWith(1, 42);
  });

  it('reaps the child on a trusted sender', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, getMainWindow);
    expect(stopAudiocapHost).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['an untrusted origin', { senderFrame: { url: 'https://evil.example/' } }],
    ['a null senderFrame', { senderFrame: null }],
  ])('refuses %s', async (_label, evt) => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(evt as never, getRemoteSpaOrigin, getMainWindow);
    // This handler reaches a process kill, so an untrusted frame must not be able to end
    // another window's share. Same fence, same position — first statement — as `start`.
    expect(stopAudiocapHost).not.toHaveBeenCalled();
  });

  it('uses the GRACEFUL stop, never the kill', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, getMainWindow);
    // `killAudiocapHost` destroys the tap just as well and reads none of the child's
    // teardown evidence: `handleStop` is the only caller of the addon's `status()`, so a
    // kill here would silently retire the R9 silence detector, `quiesceProved` and
    // `destroyFailures` — the shipped-but-unread failure this epic keeps producing.
    expect(stopAudiocapHost).toHaveBeenCalled();
    expect(killAudiocapHost).not.toHaveBeenCalled();
  });
});

/**
 * RED. admission — main window's main frame ONLY (#3394 PR 1, R5).
 *
 * The interface the accompanying engineer PR implements:
 *   `registerAudiocapIpc(getRemoteSpaOrigin, getMainWindow: () => BrowserWindow | null)`
 *   `handleAudiocapStart(event, request, getRemoteSpaOrigin, getMainWindow)`
 *   `handleAudiocapStop(event, getRemoteSpaOrigin, getMainWindow)`
 *
 * Admission = the EXISTING `requireTrustedSender` AND `event.sender ===
 * getMainWindow()?.webContents` AND the sender frame IS that webContents' main frame
 * (compared by `frameTreeNodeId`; a null `senderFrame` is refused).
 *
 * Every case here currently FAILS (RED): production `handleAudiocapStart`/
 * `handleAudiocapStop` check only `requireTrustedSender(event, getRemoteSpaOrigin())` and
 * never read a fourth argument, so a trusted-origin event from ANY webContents — a PiP
 * window, a subframe — is admitted today.
 */
describe('audiocap:start / audiocap:stop admission — main window main frame only (#3394 PR 1, R5)', () => {
  /** A trusted-origin sender that is NOT the main window (e.g. a PiP window). */
  const pipWebContents = { mainFrame: { frameTreeNodeId: 2 } };
  const pipEvent = {
    senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 2 },
    sender: pipWebContents,
  } as never;

  /** A trusted-origin sender whose frame is a SUBFRAME of the main window's webContents. */
  const subframeEvent = {
    senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 999 },
    sender: mainWebContents,
  } as never;

  const noMainWindow = () => null as never;

  it('start: refuses a trusted-origin event from a DIFFERENT webContents (a PiP window)', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      pipEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(getSources).not.toHaveBeenCalled();
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('start: refuses a trusted-origin SUBFRAME of the main window', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      subframeEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('start: refuses when getMainWindow() returns null', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      noMainWindow
    );
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it("start: admits the main window's own main frame (happy path via getMainWindow)", async () => {
    const { handleAudiocapStart } = await loadIpc();
    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    expect(r).toEqual({ ok: true, generation: 1, perProcessAudio: true });
    expect(startAudiocapHost).toHaveBeenCalledWith(1, 42);
  });

  it('start: the post-await re-check also enforces main-window/main-frame — a sender whose frame stops being the main frame is refused', async () => {
    let release!: (sources: Array<{ id: string; name: string }>) => void;
    const pending = new Promise<Array<{ id: string; name: string }>>((resolve) => {
      release = resolve;
    });
    getSources.mockImplementationOnce(() => pending);
    const { handleAudiocapStart } = await loadIpc();

    // A live IPC event whose `.senderFrame` flips to a non-main-frame partway through the
    // call -- e.g. the requesting document is superseded by a PiP taking focus. The FIRST
    // fence (pre-await) sees the main frame; only the RE-CHECK after the await can catch
    // this.
    let flipped = false;
    const flippableEvent = {
      get senderFrame() {
        return flipped
          ? { url: 'app://concord/index.html', frameTreeNodeId: 999 }
          : { url: 'app://concord/index.html', frameTreeNodeId: 1 };
      },
      get sender() {
        return mainWebContents;
      },
    } as never;

    const pendingStart = handleAudiocapStart(
      flippableEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    flipped = true;
    release([{ id: 'window:42:0', name: 'Some App' }]);
    await expect(pendingStart).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('start: the post-await re-check refuses a sender whose senderFrame goes null', async () => {
    let release!: (sources: Array<{ id: string; name: string }>) => void;
    const pending = new Promise<Array<{ id: string; name: string }>>((resolve) => {
      release = resolve;
    });
    getSources.mockImplementationOnce(() => pending);
    const { handleAudiocapStart } = await loadIpc();

    let gone = false;
    const flippableEvent = {
      get senderFrame() {
        return gone ? null : { url: 'app://concord/index.html', frameTreeNodeId: 1 };
      },
      get sender() {
        return mainWebContents;
      },
    } as never;

    const pendingStart = handleAudiocapStart(
      flippableEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      getMainWindow
    );
    gone = true;
    release([{ id: 'window:42:0', name: 'Some App' }]);
    await expect(pendingStart).resolves.toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('stop: refuses (no-op) a trusted-origin event from a DIFFERENT webContents (a PiP window)', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(pipEvent, getRemoteSpaOrigin, getMainWindow);
    expect(stopAudiocapHost).not.toHaveBeenCalled();
  });

  it('stop: refuses (no-op) a trusted-origin SUBFRAME of the main window', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(subframeEvent, getRemoteSpaOrigin, getMainWindow);
    expect(stopAudiocapHost).not.toHaveBeenCalled();
  });

  it('stop: refuses (no-op) when getMainWindow() returns null', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, noMainWindow);
    expect(stopAudiocapHost).not.toHaveBeenCalled();
  });

  it("stop: admits the main window's own main frame (happy path via getMainWindow)", async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, getMainWindow);
    expect(stopAudiocapHost).toHaveBeenCalledTimes(1);
  });

  /**
   * The `isMainWindowMainFrame` catch branch (#3394 PR 1, R5, ~audiocap.ts:83-93). Reading
   * `.webContents` off a destroyed `BrowserWindow`, or `.mainFrame` off a destroyed
   * `webContents`, throws Electron's "Object has been destroyed" error on property access.
   * Both handlers must land on the SAME `false` a missing window already produces, and must
   * never let that throw escape.
   */
  it('start: refuses (protocol-fault) when getMainWindow() returns a window whose webContents getter throws', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const webContentsGetter = vi.fn(() => {
      throw new Error('Object has been destroyed');
    });
    const destroyedWindow = Object.defineProperty({ isDestroyed: () => false }, 'webContents', {
      get: webContentsGetter,
    });

    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      () => destroyedWindow as never
    );

    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(getSources).not.toHaveBeenCalled();
    expect(startAudiocapHost).not.toHaveBeenCalled();
    // Non-vacuity: proves the case reached the throwing `.webContents` getter rather than
    // being refused earlier by `isDestroyed()`.
    expect(webContentsGetter).toHaveBeenCalled();
  });

  it('stop: is a no-op and does not throw when getMainWindow() returns a window whose webContents getter throws', async () => {
    const { handleAudiocapStop } = await loadIpc();
    const webContentsGetter = vi.fn(() => {
      throw new Error('Object has been destroyed');
    });
    const destroyedWindow = Object.defineProperty({ isDestroyed: () => false }, 'webContents', {
      get: webContentsGetter,
    });

    expect(() =>
      handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, () => destroyedWindow as never)
    ).not.toThrow();
    expect(stopAudiocapHost).not.toHaveBeenCalled();
    // Non-vacuity: proves the case reached the throwing `.webContents` getter rather than
    // being refused earlier by `isDestroyed()`.
    expect(webContentsGetter).toHaveBeenCalled();
  });

  it('start: refuses (protocol-fault) when webContents is live but its mainFrame getter throws', async () => {
    const { handleAudiocapStart } = await loadIpc();
    // `event.sender === contents` must hold for the second half of the admission check --
    // the one that reads `.mainFrame` and throws -- to be reached at all (`&&`
    // short-circuits otherwise). So this event's `sender` IS the same object `getMainWindow`
    // hands back as `webContents`.
    const mainFrameGetter = vi.fn(() => {
      throw new Error('Object has been destroyed');
    });
    const contentsWithDestroyedMainFrame = Object.defineProperty({}, 'mainFrame', {
      get: mainFrameGetter,
    });
    const eventWithDestroyedMainFrame = {
      senderFrame: { url: 'app://concord/index.html', frameTreeNodeId: 1 },
      sender: contentsWithDestroyedMainFrame,
    } as never;

    const r = await handleAudiocapStart(
      eventWithDestroyedMainFrame,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      () =>
        ({
          isDestroyed: () => false,
          webContents: contentsWithDestroyedMainFrame,
        }) as never
    );

    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(getSources).not.toHaveBeenCalled();
    expect(startAudiocapHost).not.toHaveBeenCalled();
    // Non-vacuity: proves the case reached the throwing `.mainFrame` getter rather than
    // being refused earlier by `isDestroyed()` or the `.webContents` getter.
    expect(mainFrameGetter).toHaveBeenCalled();
  });

  /**
   * The EXPLICIT `isDestroyed()` guard (#3394 fix), distinct from the catch-branch cases
   * above: a destroyed window refuses via the FIRST statement in the try block, never
   * reaching `.webContents` at all. Proven by a getter spy that must NEVER be invoked --
   * the mirror of the non-vacuity assertions above, which prove the getter WAS reached.
   */
  it('start: refuses (protocol-fault) when getMainWindow() returns a destroyed window, without ever reading .webContents', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const webContentsGetter = vi.fn(() => mainWebContents);
    const destroyedWindow = Object.defineProperty({ isDestroyed: () => true }, 'webContents', {
      get: webContentsGetter,
    });

    const r = await handleAudiocapStart(
      trustedEvent,
      { sourceId: 'window:42:0' },
      getRemoteSpaOrigin,
      () => destroyedWindow as never
    );

    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(getSources).not.toHaveBeenCalled();
    expect(startAudiocapHost).not.toHaveBeenCalled();
    expect(webContentsGetter).not.toHaveBeenCalled();
  });

  it('stop: is a no-op when getMainWindow() returns a destroyed window, without ever reading .webContents', async () => {
    const { handleAudiocapStop } = await loadIpc();
    const webContentsGetter = vi.fn(() => mainWebContents);
    const destroyedWindow = Object.defineProperty({ isDestroyed: () => true }, 'webContents', {
      get: webContentsGetter,
    });

    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin, () => destroyedWindow as never);

    expect(stopAudiocapHost).not.toHaveBeenCalled();
    expect(webContentsGetter).not.toHaveBeenCalled();
  });
});
