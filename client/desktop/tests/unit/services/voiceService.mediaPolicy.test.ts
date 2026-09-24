// client/desktop/tests/unit/services/voiceService.mediaPolicy.test.ts
/**
 * #2153 renderer wiring, driven through the REAL voiceService singleton with a stub
 * socket (the voiceService.screenAudioTeardown.test.ts harness). Each case asserts the
 * outermost seam the renderer owns: the store the UI reads, or the socket emit the
 * media plane would receive.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

const LOCAL = 'local-user';
const PEER = 'peer-user';

const producerStub = (id: string, paused = false) => ({
  id,
  closed: false,
  paused,
  close: vi.fn(),
  pause: vi.fn(),
  resume: vi.fn().mockResolvedValue(undefined),
  replaceTrack: vi.fn().mockResolvedValue(undefined),
  on: vi.fn(),
});

const participant = (userId: string, over: Partial<VoiceParticipant> = {}): VoiceParticipant => ({
  userId,
  username: userId,
  isMuted: false,
  isDeafened: false,
  serverMuted: false,
  serverDeafened: false,
  isVideoOn: false,
  isScreenSharing: false,
  isSpeaking: false,
  ...over,
});

describe('voiceService media-policy wiring (#2153)', () => {
  /* eslint-disable @typescript-eslint/no-explicit-any -- the handlers and choke points are
     private; exercising the real singleton is the only way the wiring is under test. */
  let svc: any;
  let socket: {
    emit: ReturnType<typeof vi.fn>;
    on: ReturnType<typeof vi.fn>;
    disconnect: ReturnType<typeof vi.fn>;
    io: { on: ReturnType<typeof vi.fn> };
  };

  const handler = (event: string): ((...args: unknown[]) => unknown) => {
    const call = socket.on.mock.calls.find((c: unknown[]) => c[0] === event);
    if (!call) throw new Error(`no handler registered for ${event}`);
    return call[1] as (...args: unknown[]) => unknown;
  };
  const emitted = (event: string) =>
    socket.emit.mock.calls.filter((c: unknown[]) => c[0] === event);

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    vi.spyOn(Date, 'now').mockReturnValue(1_000_000);
    useUserStore.setState({ user: { id: LOCAL } as any });
    svc = voiceService as any;
    svc.producers.clear();
    svc.consumers = new Map();
    svc.consumerMeta = new Map();
    svc.testSuspendedProducerIds.clear();
    svc.testRestoreEligibleProducerIds.clear();
    socket = { emit: vi.fn(), on: vi.fn(), disconnect: vi.fn(), io: { on: vi.fn() } };
    svc.socket = socket;
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    useVoiceStore.getState().setParticipants([participant(LOCAL), participant(PEER)]);
    svc.setupSocketListeners();
  });

  afterEach(() => vi.restoreAllMocks());

  // ── §1b peers: kind-aware producer-paused ────────────────────────────────
  it.each([
    ['mic', { kind: 'audio', source: 'mic' }, { isMuted: true, isCameraPaused: undefined }],
    ['camera', { kind: 'video', source: 'camera' }, { isMuted: false, isCameraPaused: true }],
    [
      'screen-audio',
      { kind: 'audio', source: 'screen-audio' },
      { isMuted: false, isCameraPaused: undefined },
    ],
    [
      'unknown source (degraded to absent) → legacy mute',
      { source: 'hologram' },
      { isMuted: true, isCameraPaused: undefined },
    ],
    [
      'missing source, unknown locally → legacy mute',
      {},
      { isMuted: true, isCameraPaused: undefined },
    ],
  ])('producer-paused %s', (_name, extra, expected) => {
    handler('producer-paused')({ producerId: 'p-1', userId: PEER, ...extra });
    const peer = useVoiceStore.getState().participants[PEER];
    expect(peer.isMuted).toBe(expected.isMuted);
    expect(peer.isCameraPaused).toBe(expected.isCameraPaused);
  });

  it('producer-paused with no source resolves it from the consumer map (old media plane)', () => {
    svc.consumerMeta.set('c-1', { source: 'camera', producerUserId: PEER, producerId: 'cam-9' });
    handler('producer-paused')({ producerId: 'cam-9', userId: PEER });
    const peer = useVoiceStore.getState().participants[PEER];
    expect(peer.isCameraPaused).toBe(true);
    expect(peer.isMuted).toBe(false);
  });

  it('producer-paused/resumed for a screen marks and unmarks the share entry, not the mute', () => {
    useVoiceStore.getState().registerActiveScreenShare({
      producerId: 'scr-1',
      userId: PEER,
      username: PEER,
      isLocal: false,
    });
    handler('producer-paused')({
      producerId: 'scr-1',
      userId: PEER,
      kind: 'video',
      source: 'screen',
    });
    expect(useVoiceStore.getState().activeScreenShares['scr-1'].paused).toBe(true);
    expect(useVoiceStore.getState().participants[PEER].isMuted).toBe(false);
    handler('producer-resumed')({
      producerId: 'scr-1',
      userId: PEER,
      kind: 'video',
      source: 'screen',
    });
    expect(useVoiceStore.getState().activeScreenShares['scr-1'].paused).toBe(false);
  });

  it('producer-resumed mirrors the camera row', () => {
    handler('producer-paused')({
      producerId: 'cam-1',
      userId: PEER,
      kind: 'video',
      source: 'camera',
    });
    handler('producer-resumed')({
      producerId: 'cam-1',
      userId: PEER,
      kind: 'video',
      source: 'camera',
    });
    expect(useVoiceStore.getState().participants[PEER].isCameraPaused).toBe(false);
  });

  it('a malformed producer-paused changes no state', () => {
    const before = useVoiceStore.getState().participants;
    handler('producer-paused')({ producerId: 'p-1' });
    expect(useVoiceStore.getState().participants).toBe(before);
  });

  it('a peer camera producer-closed clears isCameraPaused so a re-produced camera shows again', () => {
    useVoiceStore.getState().updateParticipant(PEER, { isCameraPaused: true, isVideoOn: true });
    handler('producer-closed')({ producerId: 'cam-1', userId: PEER, source: 'camera' });
    expect(useVoiceStore.getState().participants[PEER].isCameraPaused).toBe(false);
  });

  // ── §1a owner: media-policy-notice ───────────────────────────────────────
  it('a mic notice latches, pauses the local mic, stops VAD, mutes, and emits nothing', () => {
    const mic = producerStub('mic-1');
    svc.producers.set('mic', mic);
    const stopVad = vi.spyOn(svc, 'stopLocalVAD');
    handler('media-policy-notice')({
      producerId: 'mic-1',
      kind: 'audio',
      source: 'mic',
      action: 'paused',
    });
    const s = useVoiceStore.getState();
    expect(s.mediaPolicyPaused).toEqual({ mic: 'mic-1' });
    expect(mic.pause).toHaveBeenCalledTimes(1);
    expect(stopVad).toHaveBeenCalled();
    expect(s.isMuted).toBe(true);
    expect(socket.emit).not.toHaveBeenCalled();
  });

  it('a camera notice latches without pausing the camera locally (Stop is the remedy)', () => {
    const cam = producerStub('cam-1');
    svc.producers.set('camera', cam);
    handler('media-policy-notice')({
      producerId: 'cam-1',
      kind: 'video',
      source: 'camera',
      action: 'paused',
    });
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({ camera: 'cam-1' });
    expect(cam.pause).not.toHaveBeenCalled();
  });

  it('a malformed notice changes no state', () => {
    const mic = producerStub('mic-1');
    svc.producers.set('mic', mic);
    handler('media-policy-notice')({
      producerId: 'mic-1',
      kind: 'audio',
      source: 'mic',
      action: 'closed',
    });
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({});
    expect(mic.pause).not.toHaveBeenCalled();
  });

  it('a notice for a producer this client no longer owns is ignored', () => {
    svc.producers.set('mic', producerStub('mic-2'));
    handler('media-policy-notice')({
      producerId: 'mic-1',
      kind: 'audio',
      source: 'mic',
      action: 'paused',
    });
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({});
  });

  // ── the resume guard ─────────────────────────────────────────────────────
  it('CONTROL: an unlatched muted mic toggle does emit resume-producer (proves the harness reaches the emit)', async () => {
    svc.producers.set('mic', producerStub('mic-1', true));
    useVoiceStore.getState().setMuted(true);
    await svc.toggleMute();
    expect(emitted('resume-producer')).toEqual([['resume-producer', { producerId: 'mic-1' }]]);
  });

  it('a latched mic toggle never emits resume-producer and never resumes locally', async () => {
    const mic = producerStub('mic-1', true);
    svc.producers.set('mic', mic);
    useVoiceStore.getState().setMuted(true);
    useVoiceStore.getState().setMediaPolicyPaused('mic', 'mic-1');
    await svc.toggleMute();
    expect(emitted('resume-producer')).toEqual([]);
    expect(mic.resume).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().isMuted).toBe(true);
  });

  it.each([
    ['CONTROL unlatched', false, 1],
    ['latched', true, 0],
  ])('resumeLocalProducer(camera) %s', (_name, latched, emits) => {
    svc.producers.set('camera', producerStub('cam-1', true));
    if (latched) useVoiceStore.getState().setMediaPolicyPaused('camera', 'cam-1');
    svc.resumeLocalProducer('camera');
    expect(emitted('resume-producer')).toHaveLength(emits);
  });

  // The other two resume-producer sites (resolution 3 covers all four).
  it.each([
    ['CONTROL unlatched', false, 1],
    ['latched', true, 0],
  ])('solo-exit resume(camera) %s', (_name, latched, emits) => {
    svc.producers.set('camera', producerStub('cam-1', true));
    if (latched) useVoiceStore.getState().setMediaPolicyPaused('camera', 'cam-1');
    svc.exitSoloBandwidthSaving();
    expect(emitted('resume-producer')).toHaveLength(emits);
  });

  it.each([
    ['CONTROL unlatched', false, 1],
    ['latched', true, 0],
  ])('test-suspension restore(camera) %s', (_name, latched, emits) => {
    const cam = producerStub('cam-1', true);
    svc.testSuspendedProducerIds.add('cam-1');
    svc.testRestoreEligibleProducerIds.add('cam-1');
    if (latched) useVoiceStore.getState().setMediaPolicyPaused('camera', 'cam-1');
    svc.restoreTestSuspendedProducer('camera', cam, {
      keepAudioOutPaused: false,
      keepProducersPaused: false,
      keepMicPaused: false,
    });
    expect(emitted('resume-producer')).toHaveLength(emits);
  });

  // ── A1 latch clears ──────────────────────────────────────────────────────
  it('the producer-closed self-echo clears the latch; another producerId does not', () => {
    useVoiceStore.getState().setMediaPolicyPaused('camera', 'cam-1');
    handler('producer-closed')({ producerId: 'other', userId: LOCAL, source: 'camera' });
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({ camera: 'cam-1' });
    handler('producer-closed')({ producerId: 'cam-1', userId: LOCAL, source: 'camera' });
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({});
  });

  it('a local close clears the latch with no echo at all (A1 a)', async () => {
    svc.producers.set('camera', producerStub('cam-1'));
    useVoiceStore.getState().setMediaPolicyPaused('camera', 'cam-1');
    await svc.closeProducer('camera');
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({});
  });

  // ── F6: producer-closed for a producer this client still owns ────────────
  // When the server's pause of a policed producer fails, it closes the producer
  // outright instead, and the owner gets only this self-echo. Mirrors the
  // permissions-changed handler's per-source cleanup for mic/camera.
  it('a server-initiated close of the local mic stops capture, mutes, and drops the producer (F6)', async () => {
    const mic = producerStub('mic-1');
    svc.producers.set('mic', mic);
    const closeProducerSpy = vi.spyOn(svc, 'closeProducer');
    handler('producer-closed')({ producerId: 'mic-1', userId: LOCAL, source: 'mic' });
    expect(closeProducerSpy).toHaveBeenCalledWith('mic');
    await closeProducerSpy.mock.results[0].value; // let the fire-and-forget cleanup settle
    expect(mic.close).toHaveBeenCalledTimes(1);
    expect(svc.producers.has('mic')).toBe(false);
    const s = useVoiceStore.getState();
    expect(s.isMuted).toBe(true);
    expect(s.participants[LOCAL].isMuted).toBe(true);
  });

  it('a server-initiated close of the local camera stops capture and drops the producer (F6)', async () => {
    const cam = producerStub('cam-1');
    svc.producers.set('camera', cam);
    const closeProducerSpy = vi.spyOn(svc, 'closeProducer');
    handler('producer-closed')({ producerId: 'cam-1', userId: LOCAL, source: 'camera' });
    expect(closeProducerSpy).toHaveBeenCalledWith('camera');
    await closeProducerSpy.mock.results[0].value;
    expect(cam.close).toHaveBeenCalledTimes(1);
    expect(svc.producers.has('camera')).toBe(false);
  });

  it("a locally-initiated mic close's own producer-closed echo is a no-op (F6)", async () => {
    const mic = producerStub('mic-1');
    svc.producers.set('mic', mic);
    await svc.closeProducer('mic'); // the local close already deleted the map entry
    const closeProducerSpy = vi.spyOn(svc, 'closeProducer');
    handler('producer-closed')({ producerId: 'mic-1', userId: LOCAL, source: 'mic' });
    expect(closeProducerSpy).not.toHaveBeenCalled();
  });

  it('a peer camera close never invokes local cleanup, even when this client owns a different camera producer (F6 control)', () => {
    svc.producers.set('camera', producerStub('my-cam-1'));
    const closeProducerSpy = vi.spyOn(svc, 'closeProducer');
    handler('producer-closed')({ producerId: 'peer-cam-1', userId: PEER, source: 'camera' });
    expect(closeProducerSpy).not.toHaveBeenCalled();
  });

  it('the reconnect teardown retires every latch it orphans (red-team R6c)', () => {
    // resumeAfterReconnect runs cleanupMediaAndTransports, which closes producers by hand
    // (no closeProducer, so no A1 (a) clear) and never calls reset(). The old session's
    // producer-closed reaches the room before the new socket joins it, so no self-echo
    // clears the latch either: without this, the fresh mic stays locked as "Paused".
    svc.producers.set('mic', producerStub('mic-1'));
    svc.stopScreenAudioHost = vi.fn();
    handler('media-policy-notice')({
      producerId: 'mic-1',
      kind: 'audio',
      source: 'mic',
      action: 'paused',
    });
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({ mic: 'mic-1' }); // control
    svc.cleanupMediaAndTransports();
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({});
  });

  // ── §1c eviction ─────────────────────────────────────────────────────────
  it('force-disconnect media_policy then the real emergencyCleanup leaves the interrupt set', () => {
    // Only the media/timer teardown is stubbed; emergencyCleanup's store.reset() stays real,
    // and reset() is the thing that must preserve the interrupt.
    svc.cleanupMediaAndTransports = vi.fn();
    svc.cleanupTimersAndE2EE = vi.fn();
    useVoiceStore.getState().setMediaPolicyPaused('mic', 'mic-1');
    handler('force-disconnect')({ channelId: 'ch-1', reason: 'media_policy', retryAfterSec: 900 });
    handler('disconnect')('io server disconnect');
    const s = useVoiceStore.getState();
    expect(s.mediaPolicyInterrupt).toEqual({ reason: 'evicted', rejoinAt: 1_900_000 });
    expect(s.mediaPolicyPaused).toEqual({});
  });

  it.each([
    ['access_revoked sets nothing', { channelId: 'ch-1', reason: 'access_revoked' }, null],
    [
      'out-of-range retryAfterSec is treated as absent (A2)',
      { channelId: 'ch-1', reason: 'media_policy', retryAfterSec: 86_401 },
      { reason: 'evicted', rejoinAt: null },
    ],
    ['malformed payload sets nothing', { reason: 'media_policy' }, null],
  ])('force-disconnect: %s', (_name, payload, expected) => {
    handler('force-disconnect')(payload);
    expect(useVoiceStore.getState().mediaPolicyInterrupt).toEqual(expected);
  });

  // ── §1d cooldown ─────────────────────────────────────────────────────────
  it('a cooldown join failure ends in the error state WITH the cooldown interrupt set', async () => {
    svc.cleanup = vi.fn().mockResolvedValue(undefined);
    const err = Object.assign(new Error('Media policy cooldown'), {
      code: 'media_policy_cooldown',
      retryAfterSec: 600,
    });
    await svc.handleJoinFailure(err);
    const s = useVoiceStore.getState();
    expect(s.connectionState).toBe('error');
    expect(s.mediaPolicyInterrupt).toEqual({ reason: 'cooldown', rejoinAt: 1_600_000 });
  });

  it('an ordinary join failure sets no interrupt', async () => {
    svc.cleanup = vi.fn().mockResolvedValue(undefined);
    await svc.handleJoinFailure(new Error('boom'));
    expect(useVoiceStore.getState().mediaPolicyInterrupt).toBeNull();
  });

  // ── A3 emitAsync ─────────────────────────────────────────────────────────
  const ackWith = (retryAfterSec: unknown) => {
    socket.emit.mockImplementation((_event: string, _data: unknown, ack: (r: unknown) => void) =>
      ack({ error: 'Media policy cooldown', code: 'media_policy_cooldown', retryAfterSec })
    );
  };

  it('emitAsync copies a valid retryAfterSec onto the rejected error', async () => {
    ackWith(600);
    await expect(svc.emitAsync('join-room', {})).rejects.toMatchObject({
      message: 'Media policy cooldown',
      code: 'media_policy_cooldown',
      retryAfterSec: 600,
    });
  });

  it.each([
    ['string', '600'],
    ['zero', 0],
    ['past the cap', 86_401],
    ['NaN', Number.NaN],
  ])('emitAsync drops an invalid retryAfterSec (%s)', async (_name, value) => {
    ackWith(value);
    const err = await svc.emitAsync('join-room', {}).catch((e: unknown) => e);
    expect(err).toMatchObject({ code: 'media_policy_cooldown' });
    expect(err).not.toHaveProperty('retryAfterSec');
  });
  /* eslint-enable @typescript-eslint/no-explicit-any */
});
