/**
 * #1924 review fix A — client-side auto-re-tune-in across a screen REPRODUCE.
 *
 * When a screensharer reproduces its screen producer (fastReproduceScreen, triggered
 * by the screen-layering-gate flip OR a codec-floor change), the OLD producer closes
 * and a NEW producer (new producerId) is announced. A remote VIEWER who was tuned into
 * that screen otherwise loses it permanently: the `producer-closed` handler runs
 * `tuneOut` and purges all state keyed by the old producerId, and the follow-up
 * `new-producer` announce only auto-consumes when autoTuneInScreenShares is ON
 * (default OFF). So a manually-tuned-in viewer permanently loses the screen on every
 * reproduce.
 *
 * Fix: `producer-closed` stashes a per-SHARER re-tune-in marker (bounded by a ~3s
 * timer) capturing whether the closing producer was dominant; the next `new-producer`
 * announce from that same sharer auto-re-tunes-in to the NEW producerId regardless of
 * the opt-in and restores dominance. A genuine stop (no re-announce within the window)
 * falls back to the normal opt-in path.
 *
 * The wire carries NO reproduce marker — the only lineage across old→new producer is
 * (userId === sharer, source === 'screen'). producer-closed lands BEFORE new-producer,
 * so live tuned-in state is already gone by announce time; the marker is the bridge.
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';

// ---------------------------------------------------------------------------
// Mock external dependencies BEFORE importing voiceService
// ---------------------------------------------------------------------------

const mockDeviceRtpCapabilities = {
  codecs: [
    { mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2, parameters: {} },
    { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
  ],
};

vi.mock('mediasoup-client', () => ({
  Device: class MockDevice {
    load = vi.fn().mockResolvedValue(undefined);
    rtpCapabilities = mockDeviceRtpCapabilities;
    createSendTransport = vi.fn();
    createRecvTransport = vi.fn();
    loaded = true;
  },
  types: {},
}));

const mockSocket = {
  connected: true,
  emit: vi.fn(),
  on: vi.fn(),
  once: vi.fn(),
  disconnect: vi.fn(),
  io: { on: vi.fn() },
};
vi.mock('socket.io-client', () => ({
  io: vi.fn().mockReturnValue(mockSocket),
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn().mockResolvedValue(null),
    invalidateChannelKey: vi.fn(),
    getChannelKeyVersion: vi.fn().mockReturnValue(0),
    getChannelKeyByVersion: vi.fn().mockResolvedValue(null),
    onKeyRotation: vi.fn().mockReturnValue(() => {}),
  },
}));

vi.mock('@/renderer/services/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 3,
  MediaEncryption: class MockMediaEncryption {
    init = vi.fn().mockResolvedValue(undefined);
    destroy = vi.fn();
  },
  deriveFrameKey: vi.fn().mockResolvedValue({} as CryptoKey),
  ratchetKey: vi.fn().mockResolvedValue({} as CryptoKey),
}));

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

// --- browser APIs (voiceService touches MediaStream at import time in helpers) ---
class MockMediaStream {
  private _tracks: any[];
  constructor(tracks?: any[]) {
    this._tracks = tracks || [];
  }
  getTracks() {
    return this._tracks;
  }
  getAudioTracks() {
    return this._tracks.filter((t) => t.kind === 'audio');
  }
  getVideoTracks() {
    return this._tracks.filter((t) => t.kind === 'video');
  }
}
Object.defineProperty(globalThis, 'MediaStream', {
  value: MockMediaStream,
  writable: true,
  configurable: true,
});
if ('RTCRtpScriptTransform' in globalThis) {
  delete (globalThis as Record<string, unknown>)['RTCRtpScriptTransform'];
}

// ---------------------------------------------------------------------------
// Import voiceService AFTER all mocks
// ---------------------------------------------------------------------------
const { voiceService } = await import('@/renderer/services/voiceService');

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

/** Per-test reset of the singleton service to a bare, viewer-side state. */
function resetService(svc: any): void {
  svc.producers = new Map();
  svc.consumers = new Map();
  svc.consumerMeta = new Map();
  svc.lastPreferredLayerKeyByConsumer = new Map();
  svc.pendingScreenAudioProducers = new Map();
  svc.tuneInsInFlight = new Set();
  // Clear any leftover reproduce markers (+ their timers) from a prior test.
  for (const v of svc.screenReproducePending.values()) clearTimeout(v.timer);
  svc.screenReproducePending.clear();
  svc.socket = mockSocket;
  svc.mediaEncryption = null;
  svc.onProducerClosed = undefined;
  svc.onProducerAdded = undefined;
  // tuneInToScreenShare consumes via consumeProducer; register a fake consumer so it
  // finds a consumerId to record in tunedInScreenShares.
  svc.consumeProducer = vi.fn(async (producerId: string) => {
    svc.consumers.set(`consumer-${producerId}`, { producerId, close: vi.fn() });
  });
}

/**
 * Register the real socket handlers on the mock socket and return the callback bound
 * to `event`. setupSocketListeners only registers callbacks (never invokes them).
 */
function getSocketHandler(svc: any, event: string): (payload: unknown) => unknown {
  svc.setupSocketListeners();
  const call = mockSocket.on.mock.calls.find((c: unknown[]) => c[0] === event);
  if (!call) throw new Error(`no handler registered for ${event}`);
  return call[1] as (payload: unknown) => unknown;
}

/** Simulate this viewer being tuned into `producerId` shared by `userId`. */
function tuneViewerInto(svc: any, producerId: string, userId: string, consumerId: string): void {
  const store = useVoiceStore.getState();
  store.registerActiveScreenShare({ producerId, userId, username: 'U', isLocal: false });
  store.tuneIn(producerId, consumerId);
  svc.consumers.set(consumerId, { producerId, close: vi.fn() });
  svc.consumerMeta.set(consumerId, { source: 'screen', producerUserId: userId });
}

describe('voiceService screen-reproduce auto-re-tune-in (#1924 fix A)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Opt-in OFF by default — the whole point is re-tune-in works WITHOUT it.
    useVideoSettingsStore.setState({ autoTuneInScreenShares: false });
  });

  it('re-tunes-in to the NEW producer with autoTuneInScreenShares OFF, restoring dominance', async () => {
    const svc = voiceService as any;
    resetService(svc);

    // Viewer tuned into TWO shares: sharer-x (dominant) and sharer-y. The second
    // tuned-in share means tuneInToScreenShare will NOT auto-restore dominance to the
    // reproduced producer (there is already a dominant), so the dominance assertion
    // proves the marker's explicit restore did the work.
    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');
    tuneViewerInto(svc, 'other-prod', 'sharer-y', 'consumer-other');
    const pre = useVoiceStore.getState();
    expect(pre.tunedInScreenShares['old-prod']).toBe('consumer-old');
    expect(pre.dominantScreenShareId).toBe('old-prod');

    const producerClosed = getSocketHandler(svc, 'producer-closed');
    const newProducer = getSocketHandler(svc, 'new-producer');

    // 1) Reproduce closes the old producer FIRST.
    producerClosed({ producerId: 'old-prod', userId: 'sharer-x', source: 'screen' });

    expect(svc.screenReproducePending.has('sharer-x')).toBe(true);
    expect(svc.screenReproducePending.get('sharer-x').wasDominant).toBe(true);
    const afterClose = useVoiceStore.getState();
    expect('old-prod' in afterClose.tunedInScreenShares).toBe(false);
    expect(svc.consumers.has('consumer-old')).toBe(false); // old consumer torn down
    // dominance fell through to the still-tuned-in other share
    expect(afterClose.dominantScreenShareId).toBe('other-prod');

    // 2) The reproduce re-announces a NEW producer for the same sharer.
    await newProducer({
      producerId: 'new-prod',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });

    const after = useVoiceStore.getState();
    expect('new-prod' in after.tunedInScreenShares).toBe(true); // auto-re-tuned-in
    expect(after.dominantScreenShareId).toBe('new-prod'); // dominance restored
    expect(svc.screenReproducePending.has('sharer-x')).toBe(false); // marker consumed
    expect(svc.consumeProducer).toHaveBeenCalledWith('new-prod', 'sharer-x', 'video');
    // opt-in never got turned on behind our back
    expect(useVideoSettingsStore.getState().autoTuneInScreenShares).toBe(false);
  });

  it('re-tunes-in when new-producer arrives BEFORE producer-closed (order-independent, Gitar review)', async () => {
    const svc = voiceService as any;
    resetService(svc);

    // Reverse-order race: the new-producer announce lands before producer-closed, so no
    // marker is armed yet — but the viewer is STILL tuned into sharer-x's OLD screen. A
    // user has one screen, so a new screen from a sharer we're watching IS a reproduce;
    // Fix A must re-tune WITHOUT depending on the marker (event ordering).
    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');
    const newProducer = getSocketHandler(svc, 'new-producer');
    expect(svc.screenReproducePending.has('sharer-x')).toBe(false); // no marker (reverse order)

    await newProducer({
      producerId: 'new-prod',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });

    expect(svc.consumeProducer).toHaveBeenCalledWith('new-prod', 'sharer-x', 'video');
    expect('new-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(true);
    expect(useVideoSettingsStore.getState().autoTuneInScreenShares).toBe(false); // opt-in stayed off
  });

  it('reverse-order producer-closed does NOT clobber isScreenSharing when a newer screen from the sharer is tuned in (#1924 review)', async () => {
    const svc = voiceService as any;
    resetService(svc);

    // Participant must exist for updateParticipant to take effect.
    useVoiceStore
      .getState()
      .upsertParticipant('sharer-x', { username: 'U', isScreenSharing: true });
    // Viewer tuned into sharer-x's OLD dominant screen and another share. This makes
    // tuneOut's fallback choose the unrelated share unless the new producer explicitly
    // inherits dominance before the delayed old-producer close arrives.
    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');
    tuneViewerInto(svc, 'other-prod', 'sharer-y', 'consumer-other');

    const producerClosed = getSocketHandler(svc, 'producer-closed');
    const newProducer = getSocketHandler(svc, 'new-producer');

    // Reverse order: the sharer's NEW screen producer is announced + re-consumed FIRST.
    await newProducer({
      producerId: 'new-prod',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });
    expect('new-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(true);
    // Simulate what the real consume path records: consumerMeta for the fresh consumer
    // (the test's mocked consumeProducer only records svc.consumers). isUserScreenTunedIn
    // reads consumerMeta, so this is the live-screen signal the clobber guard checks.
    const newConsumerId = useVoiceStore.getState().tunedInScreenShares['new-prod'];
    svc.consumerMeta.set(newConsumerId, { source: 'screen', producerUserId: 'sharer-x' });

    // Now the OLD producer-closed lands (out of order).
    producerClosed({ producerId: 'old-prod', userId: 'sharer-x', source: 'screen' });

    const after = useVoiceStore.getState();
    // The newer screen is still tuned in, so the sharer stays screen-sharing — the old
    // producer-closed must NOT wipe the reverse-order re-consume's participant state.
    expect(after.participants['sharer-x']?.isScreenSharing).toBe(true);
    expect('new-prod' in after.tunedInScreenShares).toBe(true);
    expect('old-prod' in after.tunedInScreenShares).toBe(false);
    expect(after.dominantScreenShareId).toBe('new-prod');
  });

  it('a genuine stop (viewer not re-tuned) DOES clear isScreenSharing (guard only spares a live newer screen)', () => {
    const svc = voiceService as any;
    resetService(svc);

    useVoiceStore
      .getState()
      .upsertParticipant('sharer-x', { username: 'U', isScreenSharing: true });
    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');

    const producerClosed = getSocketHandler(svc, 'producer-closed');
    // No re-announce → the old consumer's meta is deleted by the close loop and nothing
    // newer replaces it → the participant collapses to not-sharing.
    producerClosed({ producerId: 'old-prod', userId: 'sharer-x', source: 'screen' });

    const after = useVoiceStore.getState();
    expect(after.participants['sharer-x']?.isScreenSharing).toBe(false);
    expect(after.participants['sharer-x']?.screenStream).toBeUndefined();
  });

  it('does NOT reverse-order re-tune a viewer not watching that sharer (first-time share stays opt-in)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    // Viewer tuned into a DIFFERENT sharer only.
    tuneViewerInto(svc, 'y-prod', 'sharer-y', 'consumer-y');
    const newProducer = getSocketHandler(svc, 'new-producer');

    await newProducer({
      producerId: 'x-first',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });

    // Not tuned into sharer-x → no reverse-order reproduce → normal opt-in (OFF) → no consume.
    expect(svc.consumeProducer).not.toHaveBeenCalledWith('x-first', 'sharer-x', 'video');
    expect('x-first' in useVoiceStore.getState().tunedInScreenShares).toBe(false);
  });

  it('a genuine stop (no re-announce within the window) does not dangle a consumer, and a later announce uses normal opt-in', async () => {
    vi.useFakeTimers();
    const svc = voiceService as any;
    resetService(svc);

    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');
    expect(useVoiceStore.getState().dominantScreenShareId).toBe('old-prod');

    const producerClosed = getSocketHandler(svc, 'producer-closed');
    const newProducer = getSocketHandler(svc, 'new-producer');

    // Genuine stop: producer closes, no re-announce follows.
    producerClosed({ producerId: 'old-prod', userId: 'sharer-x', source: 'screen' });
    expect(svc.screenReproducePending.has('sharer-x')).toBe(true);
    expect(svc.consumers.has('consumer-old')).toBe(false); // no dangling consumer
    expect('old-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(false);

    // Past the window the marker self-expires.
    vi.advanceTimersByTime(3_000 + 1);
    expect(svc.screenReproducePending.has('sharer-x')).toBe(false);

    // A LATER restart from the same sharer must NOT force a re-consume (opt-in is OFF).
    await newProducer({
      producerId: 'restart-prod',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });

    expect('restart-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(false);
    expect(svc.consumeProducer).not.toHaveBeenCalled();
    vi.useRealTimers();
  });

  it('a viewer only AVAILABLE (not tuned in) gets no marker and no re-consume on reproduce', async () => {
    const svc = voiceService as any;
    resetService(svc);

    // Sharer-x screen is available/active but this viewer never tuned in.
    const store = useVoiceStore.getState();
    store.addAvailableScreenShare({ producerId: 'avail-prod', userId: 'sharer-x', username: 'U' });
    store.registerActiveScreenShare({
      producerId: 'avail-prod',
      userId: 'sharer-x',
      username: 'U',
      isLocal: false,
    });
    expect('avail-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(false);

    const producerClosed = getSocketHandler(svc, 'producer-closed');
    const newProducer = getSocketHandler(svc, 'new-producer');

    producerClosed({ producerId: 'avail-prod', userId: 'sharer-x', source: 'screen' });
    expect(svc.screenReproducePending.has('sharer-x')).toBe(false); // no marker

    await newProducer({
      producerId: 'new-prod',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });

    expect('new-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(false);
    expect(svc.consumeProducer).not.toHaveBeenCalled();
  });

  it('reproduce branch takes precedence over the opt-in branch (single consume, no double-tune-in)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    // Opt-in ON: the reproduce branch must still short-circuit so we consume ONCE.
    useVideoSettingsStore.setState({ autoTuneInScreenShares: true });

    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');
    const producerClosed = getSocketHandler(svc, 'producer-closed');
    const newProducer = getSocketHandler(svc, 'new-producer');

    producerClosed({ producerId: 'old-prod', userId: 'sharer-x', source: 'screen' });
    await newProducer({
      producerId: 'new-prod',
      userId: 'sharer-x',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });

    expect('new-prod' in useVoiceStore.getState().tunedInScreenShares).toBe(true);
    expect(svc.consumeProducer).toHaveBeenCalledTimes(1);
    expect(svc.consumeProducer).toHaveBeenCalledWith('new-prod', 'sharer-x', 'video');
  });

  it('resetRemoteVideoLayeringState clears pending markers and their timers (channel leave)', () => {
    vi.useFakeTimers();
    const svc = voiceService as any;
    resetService(svc);

    tuneViewerInto(svc, 'old-prod', 'sharer-x', 'consumer-old');
    const producerClosed = getSocketHandler(svc, 'producer-closed');
    producerClosed({ producerId: 'old-prod', userId: 'sharer-x', source: 'screen' });
    expect(svc.screenReproducePending.has('sharer-x')).toBe(true);

    // A channel leave / teardown routes through resetRemoteVideoLayeringState.
    svc.resetRemoteVideoLayeringState();
    expect(svc.screenReproducePending.size).toBe(0);

    // The timer was cleared, so advancing time does not throw / re-touch a fresh map.
    expect(() => vi.advanceTimersByTime(3_000 + 1)).not.toThrow();
    vi.useRealTimers();
  });
});
