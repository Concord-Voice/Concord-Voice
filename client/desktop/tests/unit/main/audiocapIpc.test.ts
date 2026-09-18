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

vi.mock('electron', () => ({
  ipcMain: { handle: (...a: unknown[]) => handle(...a) },
  desktopCapturer: { getSources: (...a: unknown[]) => getSources(...a) },
}));

vi.mock('../../../src/main/audiocapHost', () => ({
  startAudiocapHost: (...a: unknown[]) => startAudiocapHost(...a),
  stopAudiocapHost: (...a: unknown[]) => stopAudiocapHost(...a),
  killAudiocapHost: (...a: unknown[]) => killAudiocapHost(...a),
  currentAudiocapGeneration: () => 1,
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

/** A sender frame the allowlist accepts. */
const trustedEvent = { senderFrame: { url: 'app://concord/index.html' } } as never;

/**
 * THIRD ARGUMENT, REQUIRED. `handleAudiocapStart(event, request, getRemoteSpaOrigin)`
 * takes the origin getter as its third parameter, matching every sibling registrar's
 * dependency injection (`registerAttestationIpc`, `registerOpenExternalHandler`).
 */
const getRemoteSpaOrigin = () => 'app://concord';

beforeEach(() => {
  vi.clearAllMocks();
  getSources.mockResolvedValue([{ id: 'window:42:0', name: 'Some App' }]);
  startAudiocapHost.mockResolvedValue({ ok: true, generation: 1, perProcessAudio: true });
});

describe('audiocap:start — fence 1, the sender frame (A4, spec test 4)', () => {
  it('refuses an untrusted origin', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const evt = { senderFrame: { url: 'https://evil.example/' } } as never;
    const r = await handleAudiocapStart(evt, { sourceId: 'window:42:0' }, getRemoteSpaOrigin);
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('refuses a null senderFrame', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const evt = { senderFrame: null } as never;
    const r = await handleAudiocapStart(evt, { sourceId: 'window:42:0' }, getRemoteSpaOrigin);
    expect(r).toEqual({ ok: false, reason: 'protocol-fault' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('reads the frame BEFORE the enumeration, so a hostile sender costs no OS work', async () => {
    const { handleAudiocapStart } = await loadIpc();
    const evt = { senderFrame: { url: 'https://evil.example/' } } as never;
    await handleAudiocapStart(evt, { sourceId: 'window:42:0' }, getRemoteSpaOrigin);
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
    const r = await handleAudiocapStart(trustedEvent, payload as never, getRemoteSpaOrigin);
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
      getRemoteSpaOrigin
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
      getRemoteSpaOrigin
    );
    expect(r).toEqual({ ok: false, reason: 'target-unresolved' });
    expect(startAudiocapHost).not.toHaveBeenCalled();
  });

  it('enumerates without thumbnails, so the staleness check is not a screenshot', async () => {
    const { handleAudiocapStart } = await loadIpc();
    await handleAudiocapStart(trustedEvent, { sourceId: 'window:42:0' }, getRemoteSpaOrigin);
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
      getRemoteSpaOrigin
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
    await handleAudiocapStart(trustedEvent, { sourceId: 'window:1337:0' }, getRemoteSpaOrigin);
    // `[internal]rules/tests.md` § "a branch that is OBEYED must be proven REACHED":
    // a single-handle assertion proves the argument EXISTS. Only varying it at the
    // caller proves the value is CARRIED rather than constant-folded. This is the
    // rule #3198 PR 2 wrote after `screenAudioRefusalMessage` gained a `platform`
    // parameter that no call site ever varied.
    expect(startAudiocapHost).toHaveBeenCalledWith(1, 1337);
  });

  it('mints a NEW generation per share, so a stale result is distinguishable', async () => {
    const { handleAudiocapStart } = await loadIpc();
    await handleAudiocapStart(trustedEvent, { sourceId: 'window:42:0' }, getRemoteSpaOrigin);
    await handleAudiocapStart(trustedEvent, { sourceId: 'window:42:0' }, getRemoteSpaOrigin);
    expect(startAudiocapHost.mock.calls.map((c) => c[0])).toEqual([1, 2]);
    // NOT `currentAudiocapGeneration()`, which returns the LIVE child's number --
    // 0 whenever nothing is live, which is every share start. The mock above
    // returns a constant 1 precisely so that a handler wired to it would fail here.
  });
});

describe('registerAudiocapIpc', () => {
  it('registers exactly the two audiocap channels', async () => {
    const { registerAudiocapIpc } = await loadIpc();
    registerAudiocapIpc(getRemoteSpaOrigin);
    // EXACTLY, not "at least". A third channel appearing here is a widened renderer→main
    // surface, which is the thing this assertion exists to make visible in review rather
    // than a count worth keeping accurate for its own sake.
    expect(handle.mock.calls.map((c) => c[0])).toEqual(['audiocap:start', 'audiocap:stop']);
  });
});

describe('audiocap:stop', () => {
  it('reaps the child on a trusted sender', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin);
    expect(stopAudiocapHost).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['an untrusted origin', { senderFrame: { url: 'https://evil.example/' } }],
    ['a null senderFrame', { senderFrame: null }],
  ])('refuses %s', async (_label, evt) => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(evt as never, getRemoteSpaOrigin);
    // This handler reaches a process kill, so an untrusted frame must not be able to end
    // another window's share. Same fence, same position — first statement — as `start`.
    expect(stopAudiocapHost).not.toHaveBeenCalled();
  });

  it('uses the GRACEFUL stop, never the kill', async () => {
    const { handleAudiocapStop } = await loadIpc();
    handleAudiocapStop(trustedEvent, getRemoteSpaOrigin);
    // `killAudiocapHost` destroys the tap just as well and reads none of the child's
    // teardown evidence: `handleStop` is the only caller of the addon's `status()`, so a
    // kill here would silently retire the R9 silence detector, `quiesceProved` and
    // `destroyFailures` — the shipped-but-unread failure this epic keeps producing.
    expect(stopAudiocapHost).toHaveBeenCalled();
    expect(killAudiocapHost).not.toHaveBeenCalled();
  });
});
