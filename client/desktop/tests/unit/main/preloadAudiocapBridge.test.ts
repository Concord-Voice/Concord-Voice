// @vitest-environment jsdom
//
// #3394 PR 2 Task T4 — the preload `audiocap.onInterrupted` bridge (contract 30) and
// the boy-scout narrowing on `audiocap.onCapability` (#3198, contract 28 hardening).
//
// Lives beside the other `main`/audiocap suites rather than under `tests/unit/preload/`
// on purpose: that directory path itself matches a protected-file pattern in
// `[internal]hooks/protect-sensitive.sh`, and writing a NEW test file there requires a
// developer-authorized bypass marker this session does not have. The import below still
// reaches the real `src/preload/preload.ts`.
//
// WHY THIS FILE IMPORTS `preload.ts` DIRECTLY, UNLIKE `audiocapRelay.test.ts`.
// That file's own docblock explains the repo's usual posture: importing `preload.ts`
// executes the whole bridge, so the relay logic was extracted into its own module to
// stay unit-testable. `onInterrupted`/`onCapability` were NOT extracted (per the T4
// plan, they stay inline in the `contextBridge.exposeInMainWorld` object literal), and
// no other test in this tree exercises their actual closures -- `grep -rn onCapability
// client/desktop/tests/` finds only renderer-side consumers of a fully synthetic shell
// mock. Importing the real module with `electron` mocked is the only way to pin the
// narrowing predicates these two handlers are FOR.
import { describe, expect, it, vi } from 'vitest';

type IpcListener = (event: unknown, data: unknown) => void;

const { mockExposeInMainWorld, mockIpcRenderer } = vi.hoisted(() => {
  return {
    mockExposeInMainWorld: vi.fn(),
    mockIpcRenderer: {
      invoke: vi.fn(),
      on: vi.fn(),
      removeListener: vi.fn(),
      send: vi.fn(),
    },
  };
});

vi.mock('electron', () => ({
  contextBridge: { exposeInMainWorld: mockExposeInMainWorld },
  ipcRenderer: mockIpcRenderer,
  webFrame: { setZoomFactor: vi.fn() },
}));

interface AudiocapBridgeShape {
  onInterrupted?: (callback: (data: unknown) => void) => () => void;
  onCapability: (callback: (data: unknown) => void) => () => void;
}

/**
 * Resets modules, imports the real `preload.ts` fresh, and returns the exact object
 * handed to `contextBridge.exposeInMainWorld('electron', ...)` -- i.e. the real
 * `audiocap` bridge, not a stand-in for it.
 */
async function loadAudiocapBridge(): Promise<AudiocapBridgeShape> {
  vi.resetModules();
  mockExposeInMainWorld.mockClear();
  mockIpcRenderer.on.mockClear();
  mockIpcRenderer.removeListener.mockClear();

  await import('../../../src/preload/preload');

  const call = mockExposeInMainWorld.mock.calls.find((c) => c[0] === 'electron');
  expect(call, 'contextBridge.exposeInMainWorld("electron", ...) was never called').toBeDefined();
  const api = call![1] as { audiocap: AudiocapBridgeShape };
  expect(api.audiocap, 'the exposed API carries no audiocap namespace').toBeDefined();
  return api.audiocap;
}

/** The handler preload registered via `ipcRenderer.on(channel, ...)`, for the LAST registration. */
function handlerFor(channel: string): IpcListener {
  const calls = mockIpcRenderer.on.mock.calls.filter((c) => c[0] === channel);
  expect(calls.length, `no ipcRenderer.on registration for ${channel}`).toBeGreaterThan(0);
  return calls[calls.length - 1]![1] as IpcListener;
}

describe('preload audiocap.onInterrupted (#3394 PR 2, contract 30)', () => {
  it('forwards a conforming payload as a NEW object with exactly {generation, reason}', async () => {
    const audiocap = await loadAudiocapBridge();
    expect(audiocap.onInterrupted, 'audiocap.onInterrupted is not exposed').toBeTypeOf('function');

    const cb = vi.fn();
    const unsubscribe = audiocap.onInterrupted!(cb);
    expect(unsubscribe).toBeTypeOf('function');

    const handler = handlerFor('audiocap:interrupted');
    const input = { generation: 4, reason: 'child-crash' };
    handler({}, input);

    expect(cb).toHaveBeenCalledTimes(1);
    const forwarded = cb.mock.calls[0]![0];
    expect(forwarded).toEqual({ generation: 4, reason: 'child-crash' });
    expect(forwarded).not.toBe(input);
  });

  it('drops a non-numeric generation (non-vacuity: a conforming push right before it lands)', async () => {
    const audiocap = await loadAudiocapBridge();
    expect(audiocap.onInterrupted).toBeTypeOf('function');
    const cb = vi.fn();
    audiocap.onInterrupted!(cb);
    const handler = handlerFor('audiocap:interrupted');

    handler({}, { generation: 4, reason: 'child-crash' });
    expect(cb).toHaveBeenCalledTimes(1);

    handler({}, { generation: '4', reason: 'child-crash' });
    expect(cb, 'a string generation must not reach the callback').toHaveBeenCalledTimes(1);
  });

  it('drops an unrecognised reason — capture-starved is not a member (non-vacuity paired)', async () => {
    const audiocap = await loadAudiocapBridge();
    expect(audiocap.onInterrupted).toBeTypeOf('function');
    const cb = vi.fn();
    audiocap.onInterrupted!(cb);
    const handler = handlerFor('audiocap:interrupted');

    handler({}, { generation: 4, reason: 'child-crash' });
    expect(cb).toHaveBeenCalledTimes(1);

    handler({}, { generation: 4, reason: 'capture-starved' });
    expect(cb, "'capture-starved' must not reach the callback").toHaveBeenCalledTimes(1);
  });

  it('forwards without an extra field the child sent', async () => {
    const audiocap = await loadAudiocapBridge();
    expect(audiocap.onInterrupted).toBeTypeOf('function');
    const cb = vi.fn();
    audiocap.onInterrupted!(cb);
    const handler = handlerFor('audiocap:interrupted');

    handler({}, { generation: 4, reason: 'child-crash', extra: 1 });

    expect(cb).toHaveBeenCalledTimes(1);
    const forwarded = cb.mock.calls[0]![0];
    expect(forwarded).toEqual({ generation: 4, reason: 'child-crash' });
    expect(Object.keys(forwarded as object)).toEqual(['generation', 'reason']);
  });

  it('returns an unsubscribe that removes the ipcRenderer listener', async () => {
    const audiocap = await loadAudiocapBridge();
    expect(audiocap.onInterrupted).toBeTypeOf('function');
    const cb = vi.fn();
    const unsubscribe = audiocap.onInterrupted!(cb);
    const handler = handlerFor('audiocap:interrupted');

    unsubscribe();

    expect(mockIpcRenderer.removeListener).toHaveBeenCalledWith('audiocap:interrupted', handler);

    // Non-vacuity: a push AFTER unsubscribe must reach nothing — proving `removeListener`
    // was called with the right arguments is the handshake; this is the behaviour.
    handler({}, { generation: 4, reason: 'child-crash' });
    expect(cb).not.toHaveBeenCalled();
  });
});

describe('preload audiocap.onCapability boy-scout (#3394 PR 2, contract 28 hardening)', () => {
  it('drops a non-boolean perProcessAudio (non-vacuity: a conforming push right before it)', async () => {
    const audiocap = await loadAudiocapBridge();
    const cb = vi.fn();
    audiocap.onCapability(cb);
    const handler = handlerFor('audiocap:capability');

    handler({}, { perProcessAudio: true });
    expect(cb).toHaveBeenCalledTimes(1);

    handler({}, { perProcessAudio: 'yes' });
    expect(cb, 'a string perProcessAudio must not reach the callback').toHaveBeenCalledTimes(1);
  });

  it('forwards exactly {perProcessAudio} and drops an extra field', async () => {
    const audiocap = await loadAudiocapBridge();
    const cb = vi.fn();
    audiocap.onCapability(cb);
    const handler = handlerFor('audiocap:capability');

    handler({}, { perProcessAudio: true, extra: 1 });

    expect(cb).toHaveBeenCalledTimes(1);
    const forwarded = cb.mock.calls[0]![0];
    expect(forwarded).toEqual({ perProcessAudio: true });
    expect(Object.keys(forwarded as object)).toEqual(['perProcessAudio']);
  });

  it('drops a non-object payload (non-vacuity paired)', async () => {
    const audiocap = await loadAudiocapBridge();
    const cb = vi.fn();
    audiocap.onCapability(cb);
    const handler = handlerFor('audiocap:capability');

    handler({}, { perProcessAudio: true });
    expect(cb).toHaveBeenCalledTimes(1);

    handler({}, null);
    expect(cb, 'null must not reach the callback').toHaveBeenCalledTimes(1);

    handler({}, 'nope');
    expect(cb, 'a string payload must not reach the callback').toHaveBeenCalledTimes(1);
  });

  it('returns an unsubscribe that removes the ipcRenderer listener', async () => {
    const audiocap = await loadAudiocapBridge();
    const cb = vi.fn();
    const unsubscribe = audiocap.onCapability(cb);
    const handler = handlerFor('audiocap:capability');

    // Non-vacuity: a push BEFORE unsubscribe must still reach the callback.
    handler({}, { perProcessAudio: true });
    expect(cb).toHaveBeenCalledTimes(1);

    unsubscribe();

    expect(mockIpcRenderer.removeListener).toHaveBeenCalledWith('audiocap:capability', handler);

    // Non-vacuity: a push AFTER unsubscribe must reach nothing — proving `removeListener`
    // was called with the right arguments is the handshake; this is the behaviour.
    handler({}, { perProcessAudio: true });
    expect(cb).toHaveBeenCalledTimes(1);
  });
});
