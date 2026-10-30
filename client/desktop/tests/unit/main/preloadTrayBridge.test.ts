// @vitest-environment jsdom
//
// Preload bridge for the status-aware tray (spec 2026-10-29-tray-status-design §4,
// plan Task 6): `traySetStatus` and `onTrayAction`.
//
// Lives beside `preloadAudiocapBridge.test.ts` for the same reason that file gives:
// `tests/unit/preload/` matches a protected-file pattern, and the bridge closures are
// inline in the `exposeInMainWorld` literal, so the real `preload.ts` is imported with
// `electron` mocked. No existing test exercised `setBadgeCount` at the preload layer,
// so there was no suite to extend.
import { describe, expect, it, vi } from 'vitest';
import { TRAY_ACTION, TRAY_SET_STATUS, type TrayStatusSnapshot } from '@/shared/trayStatus';

type IpcListener = (event: unknown, data: unknown) => void;

const { mockExposeInMainWorld, mockIpcRenderer } = vi.hoisted(() => ({
  mockExposeInMainWorld: vi.fn(),
  mockIpcRenderer: {
    invoke: vi.fn(),
    on: vi.fn(),
    removeListener: vi.fn(),
    send: vi.fn(),
  },
}));

vi.mock('electron', () => ({
  contextBridge: { exposeInMainWorld: mockExposeInMainWorld },
  ipcRenderer: mockIpcRenderer,
  webFrame: { setZoomFactor: vi.fn() },
}));

interface TrayBridgeShape {
  traySetStatus?: (s: TrayStatusSnapshot) => Promise<void>;
  onTrayAction?: (cb: (a: unknown) => void) => () => void;
}

async function loadBridge(): Promise<TrayBridgeShape> {
  vi.resetModules();
  mockExposeInMainWorld.mockClear();
  mockIpcRenderer.invoke.mockReset();
  mockIpcRenderer.on.mockClear();
  mockIpcRenderer.removeListener.mockClear();

  await import('../../../src/preload/preload');

  const call = mockExposeInMainWorld.mock.calls.find((c) => c[0] === 'electron');
  expect(call, 'contextBridge.exposeInMainWorld("electron", ...) was never called').toBeDefined();
  return call![1] as TrayBridgeShape;
}

function lastHandlerFor(channel: string): IpcListener {
  const calls = mockIpcRenderer.on.mock.calls.filter((c) => c[0] === channel);
  expect(calls.length, `no ipcRenderer.on registration for ${channel}`).toBeGreaterThan(0);
  return calls[calls.length - 1]![1] as IpcListener;
}

const snapshot: TrayStatusSnapshot = {
  v: 1,
  auth: 'signed-in',
  presence: 'online',
  conn: 'ok',
  voice: 'none',
  speaking: false,
  muted: false,
  deafened: false,
  sharing: false,
  micLocked: false,
  deafenLocked: false,
  unread: 0,
  mention: false,
  reducedMotion: false,
  iconStyle: 'auto',
};

describe('preload traySetStatus', () => {
  it('invokes the shared channel constant with the snapshot', async () => {
    const api = await loadBridge();
    expect(api.traySetStatus).toBeTypeOf('function');
    mockIpcRenderer.invoke.mockResolvedValue(undefined);

    await expect(api.traySetStatus!(snapshot)).resolves.toBeUndefined();

    expect(mockIpcRenderer.invoke).toHaveBeenCalledTimes(1);
    expect(mockIpcRenderer.invoke).toHaveBeenCalledWith(TRAY_SET_STATUS, snapshot);
    expect(TRAY_SET_STATUS).toBe('tray:setStatus');
  });

  it('propagates a rejection so the renderer .catch() sees it', async () => {
    const api = await loadBridge();
    mockIpcRenderer.invoke.mockRejectedValue(new Error('No handler registered'));
    await expect(api.traySetStatus!(snapshot)).rejects.toThrow('No handler registered');
  });
});

describe('preload onTrayAction', () => {
  it('delivers a valid action as a new object with the same fields', async () => {
    const api = await loadBridge();
    expect(api.onTrayAction).toBeTypeOf('function');
    const cb = vi.fn();
    api.onTrayAction!(cb);
    const handler = lastHandlerFor(TRAY_ACTION);

    const input = { kind: 'set-muted', value: true };
    handler({}, input);

    expect(cb).toHaveBeenCalledTimes(1);
    const forwarded = cb.mock.calls[0]![0];
    expect(forwarded).toEqual({ kind: 'set-muted', value: true });
    expect(forwarded).not.toBe(input);
  });

  it('delivers the payload-free open-unread action', async () => {
    const api = await loadBridge();
    const cb = vi.fn();
    api.onTrayAction!(cb);
    lastHandlerFor(TRAY_ACTION)({}, { kind: 'open-unread' });
    expect(cb).toHaveBeenCalledWith({ kind: 'open-unread' });
  });

  it('drops {kind:"evil"} (non-vacuity: a valid push lands first)', async () => {
    const api = await loadBridge();
    const cb = vi.fn();
    api.onTrayAction!(cb);
    const handler = lastHandlerFor(TRAY_ACTION);

    handler({}, { kind: 'leave-voice' });
    expect(cb).toHaveBeenCalledTimes(1);

    handler({}, { kind: 'evil' });
    handler({}, { kind: 'set-muted' });
    handler({}, { kind: 'open-unread', targetId: 'x' });
    handler({}, null);
    expect(cb, 'invalid actions must not reach the callback').toHaveBeenCalledTimes(1);
  });

  it('unsubscribe removes the listener and disarms the handler', async () => {
    const api = await loadBridge();
    const cb = vi.fn();
    const unsubscribe = api.onTrayAction!(cb);
    const handler = lastHandlerFor(TRAY_ACTION);

    unsubscribe();

    expect(mockIpcRenderer.removeListener).toHaveBeenCalledWith(TRAY_ACTION, handler);
    // An emit already in flight when unsubscribe ran must not reach the callback.
    handler({}, { kind: 'stop-share' });
    expect(cb).not.toHaveBeenCalled();
  });
});
