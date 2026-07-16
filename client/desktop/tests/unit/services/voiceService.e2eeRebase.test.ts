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

// --- Drive the preferred Worker path while both transform APIs are present ---
// Define both APIs so this suite regression-locks modern-path priority as well as
// the Worker init/re-base messages asserted below.
class StubRTCRtpScriptTransform {
  constructor(
    public worker: unknown,
    public options: unknown
  ) {}
}
(globalThis as Record<string, unknown>)['RTCRtpScriptTransform'] = StubRTCRtpScriptTransform;

const prototypeCreateEncodedStreams = vi.fn();
function StubRTCRtpSender() {}
Object.defineProperty(StubRTCRtpSender.prototype, 'createEncodedStreams', {
  value: prototypeCreateEncodedStreams,
});
vi.stubGlobal('RTCRtpSender', StubRTCRtpSender as unknown as typeof RTCRtpSender);

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
    // Highest version observed by any current- or by-version key fetch. This
    // can be ahead of channelKeyVersion when the current-key cache is stale.
    highestSeenVersion: 0,
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
    getChannelKeyMaterial: vi.fn(() => {
      e2eeMockState.highestSeenVersion = Math.max(
        e2eeMockState.highestSeenVersion,
        e2eeMockState.channelKeyVersion
      );
      return Promise.resolve({
        channelKey: fakeCsk,
        keyVersion: e2eeMockState.channelKeyVersion,
      });
    }),
    getChannelKeyVersion: vi.fn(() => e2eeMockState.channelKeyVersion),
    getHighestSeenKeyVersion: vi.fn(() => e2eeMockState.highestSeenVersion),
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
    addDecryptKeyAtEpoch: ReturnType<typeof vi.fn>;
    addDecryptKeyAtVersion: ReturnType<typeof vi.fn>;
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
    MEDIA_E2EE_FRAME_CRYPTO_VERSION: 5,
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
const { e2eeService: mockedE2EEService } = await import('@/renderer/services/e2eeService');
const { deriveFrameKey: mockedDeriveFrameKey } =
  await import('@/renderer/services/mediaEncryption');
const { useUserStore } = await import('@/renderer/stores/userStore');
const { useVoiceStore } = await import('@/renderer/stores/voiceStore');

// Reflection helpers — mirror the existing voiceService tests' `as any` style.
/* eslint-disable @typescript-eslint/no-explicit-any */
const svc = voiceService as any;

function latestEncryption() {
  return mediaEncryptionInstances[mediaEncryptionInstances.length - 1];
}

function emitRotation(channelId: string, keyVersion: number): void {
  e2eeMockState.highestSeenVersion = Math.max(e2eeMockState.highestSeenVersion, keyVersion);
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

async function drainMicrotasks(count = 4): Promise<void> {
  for (let i = 0; i < count; i++) await Promise.resolve();
}

function resetE2EEFetchMocks(): void {
  (mockedE2EEService.getChannelKey as ReturnType<typeof vi.fn>)
    .mockReset()
    .mockResolvedValue(fakeCsk);
  (mockedE2EEService.getChannelKeyMaterial as ReturnType<typeof vi.fn>)
    .mockReset()
    .mockImplementation(() => {
      e2eeMockState.highestSeenVersion = Math.max(
        e2eeMockState.highestSeenVersion,
        e2eeMockState.channelKeyVersion
      );
      return Promise.resolve({
        channelKey: fakeCsk,
        keyVersion: e2eeMockState.channelKeyVersion,
      });
    });
  (mockedE2EEService.getChannelKeyVersion as ReturnType<typeof vi.fn>)
    .mockReset()
    .mockImplementation(() => e2eeMockState.channelKeyVersion);
  (mockedE2EEService.getChannelKeyByVersion as ReturnType<typeof vi.fn>)
    .mockReset()
    .mockImplementation(
      (channelId: string, version: number) =>
        new Promise<CryptoKey>((resolve) => {
          e2eeMockState.pendingByVersion.set(`${channelId}:${version}`, resolve);
        })
    );
}

describe('voiceService E2EE sender re-base (#1878 Task 5)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    vi.spyOn(console, 'debug').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
    mediaEncryptionInstances.length = 0;
    e2eeMockState.channelKeyVersion = 0;
    e2eeMockState.highestSeenVersion = 0;
    e2eeMockState.rotationListeners.clear();
    e2eeMockState.pendingByVersion.clear();
    resetE2EEFetchMocks();
    vi.mocked(mockedDeriveFrameKey)
      .mockReset()
      .mockResolvedValue({ __frameKey: true } as unknown as CryptoKey);
    useUserStore.setState({ user: { id: 'local-user' } as never });
    useVoiceStore.setState({ activeChannelId: CHANNEL });
  });

  afterEach(() => {
    svc.cleanupTimersAndE2EE();
    vi.useRealTimers();
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

  it('binds initial encryption to one atomic CSK and version snapshot', async () => {
    const materialCsk = { __cskVersion7: true } as unknown as CryptoKey;
    const staleCsk = { __staleCsk: true } as unknown as CryptoKey;
    (mockedE2EEService.getChannelKeyMaterial as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      channelKey: materialCsk,
      keyVersion: 7,
    });
    (mockedE2EEService.getChannelKey as ReturnType<typeof vi.fn>).mockResolvedValueOnce(staleCsk);
    (mockedE2EEService.getChannelKeyVersion as ReturnType<typeof vi.fn>).mockReturnValueOnce(9);
    const { deriveFrameKey } = await import('@/renderer/services/mediaEncryption');

    await svc.initEncryptionCore(CHANNEL, 0);

    expect(deriveFrameKey).toHaveBeenCalledWith(materialCsk, 'local-user');
    expect(latestEncryption().getKeyVersion()).toBe(7);
    expect(workerPostMessage).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'init', keyVersion: 7 })
    );
    expect(mockedE2EEService.getChannelKeyVersion).not.toHaveBeenCalled();
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

  it('does not roll back when an older rotation fetch resolves after a newer one', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    workerPostMessage.mockClear();

    emitRotation(CHANNEL, 6);
    emitRotation(CHANNEL, 7);
    await Promise.resolve();
    await Promise.resolve();

    resolveVersionFetch(CHANNEL, 7);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    expect(enc.getKeyVersion()).toBe(7);

    resolveVersionFetch(CHANNEL, 6);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(enc.getKeyVersion()).toBe(7);
    expect(enc.setKeyVersion).not.toHaveBeenLastCalledWith(6);
    expect(
      workerPostMessage.mock.calls
        .map((call) => call[0])
        .filter((message) => message?.type === 'init')
        .map((message) => message.keyVersion)
    ).toEqual([7]);
  });

  it('does not let an old same-channel fetch mutate a replacement encryption session', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const oldEncryption = latestEncryption();

    emitRotation(CHANNEL, 6);
    await Promise.resolve();
    await Promise.resolve();

    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    const replacementEncryption = latestEncryption();
    expect(replacementEncryption).not.toBe(oldEncryption);
    expect(replacementEncryption.getKeyVersion()).toBe(9);
    workerPostMessage.mockClear();

    resolveVersionFetch(CHANNEL, 6);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(replacementEncryption.getKeyVersion()).toBe(9);
    expect(replacementEncryption.setKeyVersion).not.toHaveBeenCalledWith(6);
    expect(workerPostMessage).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'init', keyVersion: 6 })
    );
  });

  it('does not tear down a replacement session when an old fetch rejects late', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const fetchVersion = mockedE2EEService.getChannelKeyByVersion as ReturnType<typeof vi.fn>;
    let rejectOldFetch!: (error: Error) => void;
    fetchVersion.mockReturnValueOnce(
      new Promise<CryptoKey>((_resolve, reject) => {
        rejectOldFetch = reject;
      })
    );

    emitRotation(CHANNEL, 6);
    await drainMicrotasks();
    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    const replacementEncryption = latestEncryption();
    const emergencyCleanup = vi.spyOn(svc, 'emergencyCleanup');

    rejectOldFetch(new Error('late old-session failure'));
    await drainMicrotasks();

    expect(emergencyCleanup).not.toHaveBeenCalled();
    expect(svc.mediaEncryption).toBe(replacementEncryption);
    expect(replacementEncryption.getKeyVersion()).toBe(9);
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

  it('retries the one-shot rotation notification and installs the same version', async () => {
    vi.useFakeTimers();
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const fetchVersion = e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>;
    fetchVersion
      .mockRejectedValueOnce(new Error('transient network failure'))
      .mockResolvedValueOnce(fakeCsk);

    emitRotation(CHANNEL, 6);
    await drainMicrotasks();
    expect(fetchVersion).toHaveBeenCalledTimes(1);
    expect(enc.getKeyVersion()).toBe(5);

    await vi.advanceTimersByTimeAsync(500);
    await drainMicrotasks();

    expect(fetchVersion).toHaveBeenCalledTimes(2);
    expect(enc.getKeyVersion()).toBe(6);
  });

  it('tears down the current session after the bounded re-base retry budget is exhausted', async () => {
    vi.useFakeTimers();
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const fetchVersion = e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>;
    for (let attempt = 0; attempt < 4; attempt++) {
      fetchVersion.mockRejectedValueOnce(new Error('transient network failure'));
    }
    const producer = { close: vi.fn() };
    svc.producers.set('mic', producer);

    emitRotation(CHANNEL, 6);
    await drainMicrotasks();
    await vi.advanceTimersByTimeAsync(500);
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);
    await drainMicrotasks();

    expect(fetchVersion).toHaveBeenCalledTimes(4);
    expect(producer.close).toHaveBeenCalledTimes(1);
    expect(svc.producers.size).toBe(0);
    expect(svc.mediaEncryption).toBeNull();
    expect(svc.e2eeWorker).toBeNull();
    expect(useVoiceStore.getState().activeChannelId).toBeNull();
  });

  it('silently abandons an old retry when a newer rotation supersedes it', async () => {
    vi.useFakeTimers();
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    const fetchVersion = mockedE2EEService.getChannelKeyByVersion as ReturnType<typeof vi.fn>;
    fetchVersion
      .mockRejectedValueOnce(new Error('version 6 transient failure'))
      .mockResolvedValueOnce(fakeCsk);
    const emergencyCleanup = vi.spyOn(svc, 'emergencyCleanup');

    emitRotation(CHANNEL, 6);
    await drainMicrotasks();
    emitRotation(CHANNEL, 7);
    await drainMicrotasks();
    await vi.advanceTimersByTimeAsync(500);
    await drainMicrotasks();

    expect(enc.getKeyVersion()).toBe(7);
    expect(fetchVersion).toHaveBeenCalledTimes(2);
    expect(emergencyCleanup).not.toHaveBeenCalled();
  });

  it('coalesces duplicate notifications for the same in-flight version', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const fetchVersion = mockedE2EEService.getChannelKeyByVersion as ReturnType<typeof vi.fn>;

    emitRotation(CHANNEL, 6);
    emitRotation(CHANNEL, 6);
    await drainMicrotasks();

    expect(fetchVersion).toHaveBeenCalledTimes(1);
    resolveVersionFetch(CHANNEL, 6);
    await drainMicrotasks();
    expect(latestEncryption().getKeyVersion()).toBe(6);
  });

  it('does not retry a terminal membership response before tearing down', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    const { E2EEKeyUnavailableError } = await import('@/renderer/services/e2eeErrors');
    const fetchVersion = e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>;
    fetchVersion.mockRejectedValueOnce(new E2EEKeyUnavailableError('NOT_MEMBER', false));
    const emergencyCleanup = vi.spyOn(svc, 'emergencyCleanup').mockImplementation(() => {});

    emitRotation(CHANNEL, 6);
    await drainMicrotasks();

    expect(fetchVersion).toHaveBeenCalledTimes(1);
    expect(emergencyCleanup).toHaveBeenCalledTimes(1);
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

  it('does not resurrect E2EE when cleanup wins during initial key derivation', async () => {
    e2eeMockState.channelKeyVersion = 5;
    e2eeMockState.highestSeenVersion = 5;
    let resolveDerive!: (key: CryptoKey) => void;
    vi.mocked(mockedDeriveFrameKey).mockImplementationOnce(
      () =>
        new Promise<CryptoKey>((resolve) => {
          resolveDerive = resolve;
        })
    );

    const pending = svc.initEncryption(CHANNEL);
    await drainMicrotasks();
    expect(mockedDeriveFrameKey).toHaveBeenCalledTimes(1);

    svc.cleanupTimersAndE2EE();
    resolveDerive({ __lateInitialKey: true } as unknown as CryptoKey);

    await expect(pending).rejects.toThrow(/superseded|session changed/i);
    expect(svc.mediaEncryption).toBeNull();
    expect(svc.e2eeWorker).toBeNull();
    expect(e2eeMockState.rotationListeners.size).toBe(0);
    expect(workerPostMessage).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'init', keyVersion: 5 })
    );
  });

  it('does not let an older initializer overwrite a newer encryption session', async () => {
    e2eeMockState.channelKeyVersion = 5;
    e2eeMockState.highestSeenVersion = 5;
    let resolveOldMaterial!: (material: { channelKey: CryptoKey; keyVersion: number }) => void;
    (mockedE2EEService.getChannelKeyMaterial as ReturnType<typeof vi.fn>).mockReturnValueOnce(
      new Promise((resolve) => {
        resolveOldMaterial = resolve;
      })
    );

    const older = svc.initEncryptionCore(CHANNEL, 0);
    await drainMicrotasks();
    e2eeMockState.channelKeyVersion = 9;
    e2eeMockState.highestSeenVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    const replacement = latestEncryption();
    expect(replacement.getKeyVersion()).toBe(9);

    resolveOldMaterial({ channelKey: fakeCsk, keyVersion: 5 });
    await expect(older).rejects.toThrow(/superseded|session changed/i);

    expect(svc.mediaEncryption).toBe(replacement);
    expect(replacement.getKeyVersion()).toBe(9);
    expect(workerPostMessage).not.toHaveBeenLastCalledWith(
      expect.objectContaining({ type: 'init', keyVersion: 5 })
    );
  });

  it('does not let stale retry exhaustion clean a successful replacement init', async () => {
    vi.useFakeTimers();
    const fetchMaterial = mockedE2EEService.getChannelKeyMaterial as ReturnType<typeof vi.fn>;
    const transient = new Error('transient init failure');
    fetchMaterial
      .mockReset()
      .mockRejectedValue(transient)
      .mockRejectedValueOnce(transient)
      .mockResolvedValueOnce({ channelKey: fakeCsk, keyVersion: 9 });

    const staleOutcome = svc.initEncryption(CHANNEL).then(
      () => null,
      (error: unknown) => error
    );
    await drainMicrotasks();
    expect(fetchMaterial).toHaveBeenCalledTimes(1);

    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    e2eeMockState.highestSeenVersion = 9;
    await svc.initEncryption(CHANNEL);
    const replacement = latestEncryption();

    await vi.runAllTimersAsync();
    const staleError = await staleOutcome;

    expect(staleError).toBeInstanceOf(Error);
    expect(svc.mediaEncryption).toBe(replacement);
    expect(replacement.getKeyVersion()).toBe(9);
    expect(svc.e2eeWorker).not.toBeNull();
  });

  it('reconciles a rotation observed while the initial frame key is deriving', async () => {
    e2eeMockState.channelKeyVersion = 5;
    e2eeMockState.highestSeenVersion = 5;
    let resolveInitialDerive!: (key: CryptoKey) => void;
    vi.mocked(mockedDeriveFrameKey).mockImplementationOnce(
      () =>
        new Promise<CryptoKey>((resolve) => {
          resolveInitialDerive = resolve;
        })
    );

    const pending = svc.initEncryption(CHANNEL);
    await drainMicrotasks();
    emitRotation(CHANNEL, 6);
    resolveInitialDerive({ __initialFrameKey: true } as unknown as CryptoKey);
    await drainMicrotasks(8);

    expect(mockedE2EEService.getChannelKeyByVersion).toHaveBeenCalledWith(CHANNEL, 6);
    expect(latestEncryption().getKeyVersion()).toBe(5);

    resolveVersionFetch(CHANNEL, 6);
    await pending;
    expect(latestEncryption().getKeyVersion()).toBe(6);
  });

  it('reconciles highestSeen when a versioned fetch advanced beyond the current-key cache', async () => {
    e2eeMockState.channelKeyVersion = 6;
    e2eeMockState.highestSeenVersion = 7;

    const pending = svc.initEncryption(CHANNEL);
    await drainMicrotasks(8);

    expect(mockedE2EEService.getChannelKeyByVersion).toHaveBeenCalledWith(CHANNEL, 7);
    expect(latestEncryption().getKeyVersion()).toBe(6);

    resolveVersionFetch(CHANNEL, 7);
    await pending;
    expect(latestEncryption().getKeyVersion()).toBe(7);
  });

  it('coalesces duplicate lazy initializers for the same channel session', async () => {
    e2eeMockState.channelKeyVersion = 5;
    e2eeMockState.highestSeenVersion = 5;
    let resolveMaterial!: (material: { channelKey: CryptoKey; keyVersion: number }) => void;
    const deferredMaterial = new Promise<{ channelKey: CryptoKey; keyVersion: number }>(
      (resolve) => {
        resolveMaterial = resolve;
      }
    );
    const fetchMaterial = mockedE2EEService.getChannelKeyMaterial as ReturnType<typeof vi.fn>;
    fetchMaterial.mockReturnValue(deferredMaterial);

    const first = svc.initEncryption(CHANNEL);
    const second = svc.initEncryption(CHANNEL);
    await drainMicrotasks();

    expect(fetchMaterial).toHaveBeenCalledTimes(1);
    resolveMaterial({ channelKey: fakeCsk, keyVersion: 5 });
    await Promise.all([first, second]);

    expect(mediaEncryptionInstances).toHaveLength(1);
    expect(
      workerPostMessage.mock.calls.filter(([message]) => message?.type === 'init')
    ).toHaveLength(1);
    expect(e2eeMockState.rotationListeners.size).toBe(1);
  });

  it('invalidates init and unsubscribes rotations at normal cleanup entry', async () => {
    e2eeMockState.channelKeyVersion = 5;
    e2eeMockState.highestSeenVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    expect(e2eeMockState.rotationListeners.size).toBe(1);

    let finishProducerClose!: () => void;
    svc.producers.set('camera', { id: 'producer-camera' });
    vi.spyOn(svc, 'closeProducer').mockReturnValueOnce(
      new Promise<void>((resolve) => {
        finishProducerClose = resolve;
      })
    );

    const cleanup = svc.cleanup();
    expect(e2eeMockState.rotationListeners.size).toBe(0);
    expect(svc.e2eeWorker).toBeNull();
    expect(svc.mediaEncryption).toBeNull();
    expect(workerPostMessage).toHaveBeenCalledWith({ type: 'destroy' });

    finishProducerClose();
    await cleanup;
  });
});

describe('voiceService encoded transform path selection', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mediaEncryptionInstances.length = 0;
    e2eeMockState.channelKeyVersion = 1;
    e2eeMockState.highestSeenVersion = 1;
    useUserStore.setState({ user: { id: 'local-user' } as never });
    useVoiceStore.setState({ activeChannelId: CHANNEL });
  });

  afterEach(() => {
    svc.cleanupTimersAndE2EE();
  });

  it('fails E2EE initialization closed when the selected Worker constructor is unavailable', async () => {
    const workerConstructor = globalThis.Worker;
    vi.stubGlobal('Worker', undefined);
    try {
      await expect(svc.initEncryptionCore(CHANNEL, 0)).rejects.toThrow();
      expect(svc.mediaEncryption).toBeNull();
    } finally {
      vi.stubGlobal('Worker', workerConstructor);
    }
  });

  it('uses RTCRtpScriptTransform for senders and receivers when both APIs are present', async () => {
    await svc.initEncryptionCore(CHANNEL, 0);
    const senderCreateEncodedStreams = vi.fn();
    const sender = { transform: null, createEncodedStreams: senderCreateEncodedStreams };

    svc.applyEncryptTransform(sender, 'vp8', 'camera');

    expect(sender.transform).toBeInstanceOf(StubRTCRtpScriptTransform);
    expect(senderCreateEncodedStreams).not.toHaveBeenCalled();

    const receiverCreateEncodedStreams = vi.fn();
    const receiver = { transform: null, createEncodedStreams: receiverCreateEncodedStreams };
    svc.applyDecryptTransform(
      {
        rtpReceiver: receiver,
        rtpParameters: { codecs: [{ mimeType: 'video/VP8' }] },
      },
      'sender-1'
    );

    expect(receiver.transform).toBeInstanceOf(StubRTCRtpScriptTransform);
    expect(receiverCreateEncodedStreams).not.toHaveBeenCalled();
  });

  it('installs RTCRtpScriptTransform in onRtpSender before transport publication', async () => {
    await svc.initEncryptionCore(CHANNEL, 0);
    const sender = {
      transform: null,
      replaceTrack: vi.fn().mockResolvedValue(undefined),
      createEncodedStreams: vi.fn(),
    };
    const producer = { id: 'producer-1' };
    let published = false;
    const transport = {
      close: vi.fn(),
      produce: vi.fn(async (options: { onRtpSender?: (sender: unknown) => void }) => {
        expect(published).toBe(false);
        expect(sender.transform).toBeNull();
        expect(options.onRtpSender).toEqual(expect.any(Function));
        options.onRtpSender?.(sender);
        expect(sender.transform).toBeInstanceOf(StubRTCRtpScriptTransform);
        published = true;
        return producer;
      }),
    };

    await expect(
      svc.produceEncrypted(transport, {
        track: { kind: 'video' },
        codec: { mimeType: 'video/VP8' },
        appData: { source: 'camera' },
      })
    ).resolves.toBe(producer);

    expect(published).toBe(true);
    expect(sender.createEncodedStreams).not.toHaveBeenCalled();
    expect(transport.close).not.toHaveBeenCalled();
  });

  it('does not enable encodedInsertableStreams on modern send or receive transports', async () => {
    const createSendTransport = vi.fn().mockReturnValue({ id: 'send-1', on: vi.fn() });
    const createRecvTransport = vi.fn().mockReturnValue({ id: 'recv-1', on: vi.fn() });
    svc.device = { createSendTransport, createRecvTransport };
    svc.emitAsync = vi.fn().mockResolvedValue({
      id: 'transport-1',
      iceParameters: {},
      iceCandidates: [],
      dtlsParameters: {},
    });

    await svc.createSendTransport();
    await svc.createRecvTransportForKind('video');

    expect(createSendTransport).toHaveBeenCalledWith(
      expect.not.objectContaining({ additionalSettings: expect.anything() })
    );
    expect(createRecvTransport).toHaveBeenCalledWith(
      expect.not.objectContaining({ additionalSettings: expect.anything() })
    );
  });
});

// ---------------------------------------------------------------------------
// #1895 — legacy createEncodedStreams decrypt-path frame-key provisioning.
//
// The shared provisionFrameKey is reached by BOTH the Worker requestFrameKey IPC
// handler and the legacy applyLegacyDecryptPipeline `requestFrameKey` recovery
// callback. These exercise the version-specific fetch → addDecryptKeyAtVersion →
// (Worker) postMessage path, plus the fail-closed catch.
// ---------------------------------------------------------------------------
describe('voiceService #1895 — legacy-path frame-key provisioning', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    vi.spyOn(console, 'debug').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
    mediaEncryptionInstances.length = 0;
    e2eeMockState.channelKeyVersion = 0;
    e2eeMockState.highestSeenVersion = 0;
    e2eeMockState.rotationListeners.clear();
    e2eeMockState.pendingByVersion.clear();
    resetE2EEFetchMocks();
    vi.mocked(mockedDeriveFrameKey)
      .mockReset()
      .mockResolvedValue({ __frameKey: true } as unknown as CryptoKey);
    useUserStore.setState({ user: { id: 'local-user' } as never });
    useVoiceStore.setState({ activeChannelId: CHANNEL });
  });

  afterEach(() => {
    svc.cleanupTimersAndE2EE();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('does not let a late decrypt derivation mutate a replacement encryption session', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const oldEncryption = latestEncryption();
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    let resolveOldChannelKey!: (material: { channelKey: CryptoKey; keyVersion: number }) => void;
    const oldMaterial = new Promise<{ channelKey: CryptoKey; keyVersion: number }>((resolve) => {
      resolveOldChannelKey = resolve;
    });
    (e2eeService.getChannelKeyMaterial as ReturnType<typeof vi.fn>).mockReturnValueOnce(
      oldMaterial
    );

    const pending = svc.deriveAndInstallDecryptKey(CHANNEL, 'sender-old', 0);
    await drainMicrotasks();
    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    const replacementEncryption = latestEncryption();

    resolveOldChannelKey({ channelKey: fakeCsk, keyVersion: 5 });
    await expect(pending).rejects.toThrow(/E2EE session changed/);

    expect(oldEncryption.addDecryptKeyAtVersion).not.toHaveBeenCalled();
    expect(replacementEncryption.addDecryptKeyAtVersion).not.toHaveBeenCalled();
  });

  it('does not let the outer decrypt retry migrate into a replacement session', async () => {
    vi.useFakeTimers();
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    let resolveOldMaterial!: (material: { channelKey: CryptoKey; keyVersion: number }) => void;
    (e2eeService.getChannelKeyMaterial as ReturnType<typeof vi.fn>).mockReturnValueOnce(
      new Promise<{ channelKey: CryptoKey; keyVersion: number }>((resolve) => {
        resolveOldMaterial = resolve;
      })
    );

    const pending = svc.addDecryptKeyForUser(CHANNEL, 'sender-old');
    await drainMicrotasks();
    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    const replacementEncryption = latestEncryption();

    resolveOldMaterial({ channelKey: fakeCsk, keyVersion: 5 });
    await drainMicrotasks();
    await vi.advanceTimersByTimeAsync(500);
    const result = await pending;

    expect(result).toBe(false);
    expect(replacementEncryption.addDecryptKeyAtVersion).not.toHaveBeenCalled();
  });

  it('does not post an in-flight old-session decrypt key into a replacement worker', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const oldEncryption = latestEncryption();
    let resolveOldDecryptKey!: (key: CryptoKey) => void;
    const oldDecryptKey = new Promise<CryptoKey>((resolve) => {
      resolveOldDecryptKey = resolve;
    });
    oldEncryption.addDecryptKeyAtEpoch.mockReturnValueOnce(oldDecryptKey);
    oldEncryption.addDecryptKeyAtVersion.mockReturnValueOnce(oldDecryptKey);

    const pending = svc.deriveAndInstallDecryptKey(CHANNEL, 'sender-old', 0);
    await drainMicrotasks();

    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    workerPostMessage.mockClear();

    resolveOldDecryptKey({ __oldDecryptKey: true } as unknown as CryptoKey);
    await expect(pending).rejects.toThrow(/E2EE session changed/);
    expect(workerPostMessage).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'addDecryptKey', senderUserId: 'sender-old' })
    );
  });

  it('binds a decrypt key to the version returned with its CSK across a concurrent re-base', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const encryption = latestEncryption();
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    (e2eeService.getChannelKeyMaterial as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      channelKey: fakeCsk,
      keyVersion: 7,
    });
    let resolveDecryptKey!: (key: CryptoKey) => void;
    const decryptKey = new Promise<CryptoKey>((resolve) => {
      resolveDecryptKey = resolve;
    });
    encryption.addDecryptKeyAtEpoch.mockReturnValueOnce(decryptKey);
    encryption.addDecryptKeyAtVersion.mockReturnValueOnce(decryptKey);

    workerPostMessage.mockClear();
    const pending = svc.deriveAndInstallDecryptKey(CHANNEL, 'sender-versioned', 0, 2);
    await drainMicrotasks();
    expect(encryption.addDecryptKeyAtVersion).toHaveBeenCalledWith(
      fakeCsk,
      'sender-versioned',
      7,
      2
    );

    encryption.setKeyVersion(9);
    resolveDecryptKey({ __decryptKeyV7: true } as unknown as CryptoKey);
    await pending;

    expect(workerPostMessage).toHaveBeenCalledWith(
      expect.objectContaining({
        type: 'addDecryptKey',
        senderUserId: 'sender-versioned',
        keyVersion: 7,
        keyId: 2,
      })
    );
  });

  it('does not let late version provisioning mutate or post into a replacement session', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const oldEncryption = latestEncryption();

    svc.provisionFrameKey(CHANNEL, 'sender-old', 7, 2);
    await drainMicrotasks();
    svc.cleanupTimersAndE2EE();
    e2eeMockState.channelKeyVersion = 9;
    await svc.initEncryptionCore(CHANNEL, 0);
    const replacementEncryption = latestEncryption();
    workerPostMessage.mockClear();

    resolveVersionFetch(CHANNEL, 7);
    await drainMicrotasks();

    expect(oldEncryption.addDecryptKeyAtVersion).not.toHaveBeenCalled();
    expect(replacementEncryption.addDecryptKeyAtVersion).not.toHaveBeenCalled();
    expect(workerPostMessage).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'addDecryptKey', senderUserId: 'sender-old' })
    );
  });

  it('provisionFrameKey fetches the exact CSK version, derives the key, and posts it to the worker', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    workerPostMessage.mockClear();
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    (e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>).mockClear();

    svc.provisionFrameKey(CHANNEL, 'sender-9', 7, 2);
    expect(e2eeService.getChannelKeyByVersion).toHaveBeenCalledWith(CHANNEL, 7);

    resolveVersionFetch(CHANNEL, 7);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();

    expect(enc.addDecryptKeyAtVersion).toHaveBeenCalledWith(fakeCsk, 'sender-9', 7, 2);
    const addMsg = workerPostMessage.mock.calls
      .map((c) => c[0])
      .find((m) => m?.type === 'addDecryptKey');
    expect(addMsg).toMatchObject({ senderUserId: 'sender-9', keyVersion: 7, keyId: 2 });
  });

  it('the legacy requestFrameKey recovery callback routes through provisionFrameKey', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    (e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>).mockClear();

    const cbs = svc.decryptRecoveryCallbacks();
    cbs.requestFrameKey('sender-3', 4, 1);
    expect(e2eeService.getChannelKeyByVersion).toHaveBeenCalledWith(CHANNEL, 4);
  });

  it('is fail-closed when the version fetch rejects (no key added, no worker post, no throw)', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const enc = latestEncryption();
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    (e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error('403 not a member')
    );
    workerPostMessage.mockClear();

    svc.provisionFrameKey(CHANNEL, 'sender-x', 9, 0);
    await Promise.resolve();
    await Promise.resolve();

    expect(enc.addDecryptKeyAtVersion).not.toHaveBeenCalled();
    expect(
      workerPostMessage.mock.calls.map((c) => c[0]).some((m) => m?.type === 'addDecryptKey')
    ).toBe(false);
  });

  it('the worker requestFrameKey IPC message provisions via the shared path', async () => {
    e2eeMockState.channelKeyVersion = 5;
    await svc.initEncryptionCore(CHANNEL, 0);
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    (e2eeService.getChannelKeyByVersion as ReturnType<typeof vi.fn>).mockClear();

    svc.handleWorkerMessage({
      type: 'requestFrameKey',
      senderUserId: 'sender-7',
      keyVersion: 8,
      keyId: 3,
    });
    expect(e2eeService.getChannelKeyByVersion).toHaveBeenCalledWith(CHANNEL, 8);
  });
});
