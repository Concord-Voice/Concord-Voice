import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// ---------------------------------------------------------------------------
// #1878 Task 5 — sender re-base on authoritative CSK rotation.
//
// Goal of this suite: prove the correctness invariant that the media encrypt
// key's stamped version is bound to the channel's AUTHORITATIVE CSK key_version,
// (a) at init (never a stale 0 when the channel is higher), and (b) on a CSK
// rotation the stamped version stays OLD until the by-version fetch resolves,
// then advances to NEW (the rewrap-window seam).
//
// Mocks are declared BEFORE importing voiceService (vi.mock is hoisted).
// ---------------------------------------------------------------------------

// --- Drive USE_SCRIPT_TRANSFORM down the worker path ---
// voiceService selects the Worker path when createEncodedStreams is absent and
// RTCRtpScriptTransform exists. Define a stub transform + a Worker stub so the
// init postMessage path runs and we can assert the keyVersion it carries.
class StubRTCRtpScriptTransform {
  constructor(
    public worker: unknown,
    public options: unknown
  ) {}
}
(globalThis as Record<string, unknown>)['RTCRtpScriptTransform'] = StubRTCRtpScriptTransform;

// Capture every message posted to the worker (the init re-base assertions read this).
const workerPostMessage = vi.fn();
class StubWorker {
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;
  postMessage = workerPostMessage;
  terminate = vi.fn();
}
vi.stubGlobal('Worker', StubWorker as unknown as typeof Worker);

// --- mediasoup-client (minimal; this suite never builds transports) ---
vi.mock('mediasoup-client', () => ({
  Device: class MockDevice {
    load = vi.fn().mockResolvedValue(undefined);
    rtpCapabilities = { codecs: [] };
    createSendTransport = vi.fn();
    createRecvTransport = vi.fn();
    loaded = true;
  },
  types: {},
}));

// --- socket.io-client ---
vi.mock('socket.io-client', () => ({
  io: vi.fn().mockReturnValue({
    connected: false,
    emit: vi.fn(),
    on: vi.fn(),
    once: vi.fn(),
    disconnect: vi.fn(),
    io: { on: vi.fn() },
  }),
}));

// --- apiClient ---
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

// --- e2eeService: controllable version cache + rotation emitter + deferred fetch ---
// Hoisted so the vi.mock factory below (which is itself hoisted) can close over
// these without a TDZ ReferenceError.
const { e2eeMockState, fakeCsk } = vi.hoisted(() => ({
  e2eeMockState: {
    // The authoritative cached version returned by getChannelKeyVersion.
    channelKeyVersion: 0,
    // Registered onKeyRotation listeners (the emitter fan-out).
    rotationListeners: new Set<(e: { channelId: string; keyVersion: number }) => void>(),
    // Pending getChannelKeyByVersion resolvers keyed by `${channelId}:${version}`.
    pendingByVersion: new Map<string, (csk: CryptoKey) => void>(),
  },
  fakeCsk: { __csk: true } as unknown as CryptoKey,
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn().mockResolvedValue(fakeCsk),
    getChannelKeyVersion: vi.fn(() => e2eeMockState.channelKeyVersion),
    invalidateChannelKey: vi.fn(),
    onKeyRotation: vi.fn((listener: (e: { channelId: string; keyVersion: number }) => void) => {
      e2eeMockState.rotationListeners.add(listener);
      return () => e2eeMockState.rotationListeners.delete(listener);
    }),
    getChannelKeyByVersion: vi.fn(
      (channelId: string, version: number) =>
        new Promise<CryptoKey>((resolve) => {
          e2eeMockState.pendingByVersion.set(`${channelId}:${version}`, resolve);
        })
    ),
  },
}));

// --- mediaEncryption: a stub that records setKeyVersion / getKeyVersion ---
const { mediaEncryptionInstances } = vi.hoisted(() => ({
  mediaEncryptionInstances: [] as Array<{
    keyVersion: number;
    setKeyVersion: ReturnType<typeof vi.fn>;
    getKeyVersion: ReturnType<typeof vi.fn>;
    initFromKey: ReturnType<typeof vi.fn>;
  }>,
}));

vi.mock('@/renderer/services/mediaEncryption', () => {
  class MockMediaEncryption {
    keyVersion = 0;
    setKeyVersion = vi.fn((v: number) => {
      this.keyVersion = v;
    });
    getKeyVersion = vi.fn(() => this.keyVersion);
    init = vi.fn().mockResolvedValue(undefined);
    initFromKey = vi.fn();
    destroy = vi.fn();
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    setCurrentKeyId = vi.fn();
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = vi.fn().mockResolvedValue(undefined);
    addDecryptKey = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyAtEpoch = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyAtVersion = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyDirect = vi.fn();
    addDecryptKeyDirectV3 = vi.fn();
    debouncedRotateKeys = vi.fn();
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
    constructor() {
      mediaEncryptionInstances.push(this);
    }
  }
  return {
    MEDIA_E2EE_FRAME_CRYPTO_VERSION: 3,
    MediaEncryption: MockMediaEncryption,
    deriveFrameKey: vi.fn().mockResolvedValue({ __frameKey: true } as unknown as CryptoKey),
    ratchetKey: vi.fn().mockResolvedValue({} as CryptoKey),
  };
});

// --- osPermissionStore ---
vi.mock('@/renderer/stores/osPermissionStore', () => ({
  useOsPermissionStore: {
    getState: vi.fn().mockReturnValue({
      checkOne: vi.fn().mockResolvedValue('granted'),
      openSettings: vi.fn(),
    }),
    subscribe: vi.fn().mockReturnValue(() => {}),
  },
  ensureOsPermission: vi.fn().mockResolvedValue('granted'),
}));

// ---------------------------------------------------------------------------
// Import AFTER mocks
// ---------------------------------------------------------------------------
const { voiceService } = await import('@/renderer/services/voiceService');
const { useUserStore } = await import('@/renderer/stores/userStore');
const { useVoiceStore } = await import('@/renderer/stores/voiceStore');

// Reflection helpers — mirror the existing voiceService tests' `as any` style.
/* eslint-disable @typescript-eslint/no-explicit-any */
const svc = voiceService as any;

function latestEncryption() {
  return mediaEncryptionInstances[mediaEncryptionInstances.length - 1];
}

function emitRotation(channelId: string, keyVersion: number): void {
  for (const l of e2eeMockState.rotationListeners) {
    l({ channelId, keyVersion });
  }
}

function resolveVersionFetch(channelId: string, version: number): void {
  const resolve = e2eeMockState.pendingByVersion.get(`${channelId}:${version}`);
  if (!resolve) throw new Error(`no pending getChannelKeyByVersion for ${channelId}:${version}`);
  resolve(fakeCsk);
  e2eeMockState.pendingByVersion.delete(`${channelId}:${version}`);
}

const CHANNEL = 'channel-1';

describe('voiceService E2EE sender re-base (#1878 Task 5)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    vi.spyOn(console, 'debug').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
    mediaEncryptionInstances.length = 0;
    e2eeMockState.channelKeyVersion = 0;
    e2eeMockState.rotationListeners.clear();
    e2eeMockState.pendingByVersion.clear();
    useUserStore.setState({ user: { id: 'local-user' } as never });
    useVoiceStore.setState({ activeChannelId: CHANNEL });
  });

  afterEach(() => {
    svc.cleanupTimersAndE2EE();
    vi.restoreAllMocks();
  });

  it('binds the encrypt version to the authoritative CSK version at init (not 0)', async () => {
    // The channel is already on version 7 server-side.
    e2eeMockState.channelKeyVersion = 7;

    await svc.initEncryptionCore(CHANNEL, 0);

    const enc = latestEncryption();
    expect(enc.setKeyVersion).toHaveBeenCalledWith(7);
    expect(enc.getKeyVersion()).toBe(7);

    // The worker init message carries the authoritative version, never a stale 0.
    const initMsg = workerPostMessage.mock.calls.map((c) => c[0]).find((m) => m?.type === 'init');
    expect(initMsg).toMatchObject({ keyVersion: 7, currentKeyId: 0 });
  });

  it('on CSK rotation, the sender stays OLD until the fetch resolves, then becomes NEW', async () => {
    // Init at version 5.
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    expect(enc.getKeyVersion()).toBe(5);
    workerPostMessage.mockClear();

    // A rotation to version 6 is announced. The re-base kicks off a by-version
    // fetch but must NOT advance the stamped version until it CONFIRMS.
    emitRotation(CHANNEL, 6);
    // Let the rebaseEncryptKey async body reach its first await (the fetch).
    await Promise.resolve();
    await Promise.resolve();

    // Rewrap window: still stamping the OLD version; no worker re-init yet.
    expect(enc.getKeyVersion()).toBe(5);
    expect(workerPostMessage.mock.calls.map((c) => c[0]).some((m) => m?.type === 'init')).toBe(
      false
    );

    // Confirm the fetch → the sender re-bases onto version 6.
    resolveVersionFetch(CHANNEL, 6);
    // Drain the await chain (getChannelKeyByVersion → deriveFrameKey → install).
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(enc.setKeyVersion).toHaveBeenLastCalledWith(6);
    expect(enc.getKeyVersion()).toBe(6);
    // The worker is re-initialized at the new version.
    const reinit = workerPostMessage.mock.calls.map((c) => c[0]).find((m) => m?.type === 'init');
    expect(reinit).toMatchObject({ keyVersion: 6, currentKeyId: 0 });
  });

  it('ignores a rotation for a non-active channel', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    enc.setKeyVersion.mockClear();

    emitRotation('some-other-channel', 9);
    await Promise.resolve();

    // No fetch attempted, version unchanged.
    expect(e2eeMockState.pendingByVersion.size).toBe(0);
    expect(enc.getKeyVersion()).toBe(5);
  });

  it('on a failed re-base fetch, stays on the old version (fail-closed, no throw)', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();

    // Make the next by-version fetch reject.
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    (e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error('pending-404')
    );

    emitRotation(CHANNEL, 6);
    // Drain the rejected-promise handling.
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    // Stayed on the old version; no crash.
    expect(enc.getKeyVersion()).toBe(5);
  });

  it('drops the rotation subscription on teardown (no re-base after cleanup)', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);

    svc.cleanupTimersAndE2EE();
    expect(e2eeMockState.rotationListeners.size).toBe(0);

    // A late rotation must not trigger another fetch.
    emitRotation(CHANNEL, 6);
    await Promise.resolve();
    expect(e2eeMockState.pendingByVersion.size).toBe(0);
  });
});
