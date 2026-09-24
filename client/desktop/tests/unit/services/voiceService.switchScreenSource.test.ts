/**
 * Live screen-source switching (R6).
 *
 * Before this, changing what you shared meant stopping the share and starting a new
 * one. Every viewer was dropped: `producer-closed` purges tuned-in state keyed by the
 * old producerId, and the replacement announce only auto-consumes when
 * autoTuneInScreenShares is ON (default OFF) -- the same trap #1924 documented for
 * codec reproduces.
 *
 * switchScreenSource sidesteps it entirely by keeping the PRODUCER and swapping only
 * its track. No close, no new producerId, so there is no tuned-in state to lose and no
 * self-echo race to guard against.
 *
 * The other property under test is failure safety: capture the NEW source before
 * touching the old one, so a denied picker leaves the user still sharing rather than
 * sharing nothing.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { createScreenAudioBridge } from '@/renderer/services/voice/screenAudioBridge';
import { screenAudioDegradeMessage } from '@/renderer/utils/policy/screenAudioDegradeCopy';
import { deferred } from '../../helpers/deferred';

// The bridge builds a `MediaStreamTrackGenerator`, which jsdom does not implement. The
// cases below are about which state survives a switch, not about the bridge itself --
// `screenAudioBridge.test.ts` owns that.
vi.mock('@/renderer/services/voice/screenAudioBridge', () => ({
  createScreenAudioBridge: vi.fn(() => ({
    track: { id: 'bridge-track', kind: 'audio', readyState: 'live', muted: false, stop: vi.fn() },
    stop: vi.fn(),
  })),
}));

const liveTrack = (id: string, kind: 'video' | 'audio' = 'video') => ({
  id,
  kind,
  readyState: 'live' as const,
  enabled: true,
  muted: false,
  stop: vi.fn(),
  contentHint: '',
});

const streamOf = (tracks: ReturnType<typeof liveTrack>[]) => ({
  getTracks: () => tracks,
  getVideoTracks: () => tracks.filter((t) => t.kind === 'video'),
  getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
  removeTrack: vi.fn(),
});

const stubScreenPublication = (svc: any) => {
  const originals = {
    device: svc.device,
    cachedPlatform: svc.cachedPlatform,
    ensurePlatform: svc.ensurePlatform,
    pickScreenCodec: svc.pickScreenCodec,
    requireSelectedVideoCodec: svc.requireSelectedVideoCodec,
    applyDegradationPreference: svc.applyDegradationPreference,
    startPacketLossMonitor: svc.startPacketLossMonitor,
    getProducerCodecMimeType: svc.getProducerCodecMimeType,
    produceScreenAudioFromStream: svc.produceScreenAudioFromStream,
    produceEncrypted: svc.produceEncrypted,
  };
  svc.device = {};
  svc.cachedPlatform = 'darwin';
  svc.ensurePlatform = vi.fn().mockResolvedValue(undefined);
  svc.pickScreenCodec = vi.fn().mockReturnValue({
    codec: { mimeType: 'video/VP8' },
    encodings: [],
    effectiveBitrate: 1_000_000,
  });
  svc.requireSelectedVideoCodec = vi.fn().mockReturnValue({ mimeType: 'video/VP8' });
  svc.applyDegradationPreference = vi.fn();
  svc.startPacketLossMonitor = vi.fn();
  svc.getProducerCodecMimeType = vi.fn().mockReturnValue('video/VP8');
  svc.produceScreenAudioFromStream = vi.fn().mockResolvedValue(undefined);

  return () => {
    Object.assign(svc, originals);
  };
};

describe('voiceService.switchScreenSource (R6)', () => {
  let svc: any;
  let producer: any;
  let oldVideo: ReturnType<typeof liveTrack>;

  beforeEach(() => {
    resetAllStores();
    svc = voiceService as any;
    svc.producers.clear();
    svc.socket = { emit: vi.fn() };
    svc.sendTransport = { id: 'transport-1' };

    oldVideo = liveTrack('old-video');
    svc.localScreenStream = streamOf([oldVideo]);

    producer = {
      id: 'producer-STABLE',
      closed: false,
      paused: false,
      close: vi.fn(),
      pause: vi.fn(),
      resume: vi.fn(),
      replaceTrack: vi.fn().mockResolvedValue(undefined),
      on: vi.fn(),
    };
    svc.producers.set('screen', producer);
    // switchScreenSource is serialized on the per-source reproduce tail, which no-ops
    // unless a reproduce session is active. This is production machinery, not a test
    // shim: without it the switch would be discarded rather than racing.
    svc.videoReproduceSessionActive = true;

    // Nothing here should reach the network or the real capture APIs.
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    svc.produceScreenAudioFromStream = vi.fn().mockResolvedValue(undefined);
    svc.resolveCaptureDims = vi.fn().mockResolvedValue({ w: 1920, h: 1080 });
    svc.clampScreenToEntitlement = vi.fn().mockReturnValue({ width: 1920, height: 1080, fps: 30 });
  });

  it('keeps the SAME producer id, so viewers stay tuned in', async () => {
    const newStream = streamOf([liveTrack('new-video')]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });

    await svc.switchScreenSource('screen:1');

    expect(producer.replaceTrack).toHaveBeenCalledTimes(1);
    expect(producer.close).not.toHaveBeenCalled();
    expect(svc.producers.get('screen').id).toBe('producer-STABLE');
  });

  it('stops the old capture so the previous source is really released', async () => {
    const newStream = streamOf([liveTrack('new-video')]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });

    await svc.switchScreenSource('screen:1');

    expect(oldVideo.stop).toHaveBeenCalled();
    expect(svc.localScreenStream).toBe(newStream);
  });

  // The property that makes this safe to expose on a button: a cancelled or denied
  // picker must not cost the user the share they already had.
  it('leaves the ORIGINAL share running when capturing the new source fails', async () => {
    svc.captureScreen = vi.fn().mockRejectedValue(new DOMException('denied', 'NotAllowedError'));

    await expect(svc.switchScreenSource('screen:1')).resolves.toBeUndefined();

    expect(producer.replaceTrack).not.toHaveBeenCalled();
    expect(producer.close).not.toHaveBeenCalled();
    expect(oldVideo.stop).not.toHaveBeenCalled();
    expect(svc.producers.get('screen')).toBe(producer);
  });

  // The design's one sanctioned exception to producer preservation. `replaceTrack`
  // rejects when the codec cannot accept the new track, and the first implementation
  // kept the old source shared -- which makes the Switch button silently do nothing
  // for the user who pressed it. Close-and-reproduce completes the switch instead,
  // at the cost of the tune-in state keyed by producer.id (#1924), and says so.
  it('closes and re-produces when replaceTrack rejects, rather than doing nothing', async () => {
    const newStream = streamOf([liveTrack('new-video')]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });
    producer.replaceTrack = vi.fn().mockRejectedValue(new Error('codec cannot accept track'));
    svc.closeProducer = vi.fn().mockResolvedValue(undefined);
    svc.produceScreen = vi.fn().mockResolvedValue(undefined);

    await svc.switchScreenSource('screen:1', {
      resolution: '1080p',
      frameRate: 30,
      contentType: 'motion',
      streamAudio: true,
    });

    expect(svc.closeProducer).toHaveBeenCalledWith(
      'screen',
      expect.objectContaining({ preserveVideoReproduceToken: true })
    );
    // The stream we ALREADY hold is handed over, not thrown away and re-acquired.
    // Re-acquiring is fallible, and failing after the old producer is closed leaves
    // the user sharing nothing -- the outcome acquire-first exists to prevent.
    expect(svc.produceScreen).toHaveBeenCalledWith(
      'screen:1',
      expect.objectContaining({ streamAudio: true }),
      expect.objectContaining({ stream: newStream })
    );
    expect(newStream.getTracks()[0].stop).not.toHaveBeenCalled();
    // Viewers lose their tune-in on a new producer id, so the user is told.
    expect(useVoiceStore.getState().videoSlotError).toMatch(/tune back in/i);
  });

  it('discards a fallback producer that resolves after an explicit stop', async () => {
    const newVideo = liveTrack('replacement-video');
    const newStream = streamOf([newVideo]);
    const lateProducer = { id: 'late-screen', close: vi.fn(), on: vi.fn() };
    const publication = deferred<typeof lateProducer>();
    svc.captureScreen = vi.fn().mockResolvedValue({
      stream: newStream,
      sourceId: 'screen:0',
    });
    producer.replaceTrack = vi.fn().mockRejectedValue(new Error('codec cannot accept track'));
    const restorePublication = stubScreenPublication(svc);
    svc.produceEncrypted = vi.fn().mockImplementation(() => publication.promise);
    svc.produceScreen = vi.fn(Object.getPrototypeOf(svc).produceScreen.bind(svc));
    const realCloseProducer = Object.getPrototypeOf(svc).closeProducer.bind(svc);
    svc.closeProducer = Object.getPrototypeOf(svc).closeProducer.bind(svc);

    try {
      const switching = svc.switchScreenSource('screen:1', { streamAudio: false });
      await vi.waitFor(() => expect(svc.produceEncrypted).toHaveBeenCalledOnce());
      expect(svc.produceScreen).toHaveBeenCalledWith(
        'screen:1',
        expect.objectContaining({ streamAudio: false }),
        expect.objectContaining({ stream: newStream, sourceId: 'screen:0' })
      );

      await svc.closeProducer('screen');
      publication.resolve(lateProducer);
      await switching;
    } finally {
      svc.closeProducer = realCloseProducer;
      restorePublication();
    }

    expect(lateProducer.close).toHaveBeenCalledOnce();
    expect(svc.producers.has('screen')).toBe(false);
    expect(svc.localScreenStream).toBeNull();
    expect(newVideo.stop).toHaveBeenCalledOnce();
    expect(useVoiceStore.getState().isScreenSharing).toBe(false);
  });

  it('stops an older capture when its overlapping producer resolves after the newer share', async () => {
    const oldCaptureVideo = liveTrack('overlap-old-video');
    const oldCapture = streamOf([oldCaptureVideo]);
    const newCapture = streamOf([liveTrack('overlap-new-video')]);
    const oldProducer = { id: 'overlap-old-producer', close: vi.fn(), on: vi.fn() };
    const newProducer = { id: 'overlap-new-producer', close: vi.fn(), on: vi.fn() };
    const oldPublication = deferred<typeof oldProducer>();
    const newPublication = deferred<typeof newProducer>();
    const realCaptureScreen = svc.captureScreen;
    const restorePublication = stubScreenPublication(svc);

    svc.producers.delete('screen');
    useVoiceStore.getState().setScreenSharing(false);
    svc.captureScreen = vi
      .fn()
      .mockResolvedValueOnce({ stream: oldCapture, sourceId: 'screen:old' })
      .mockResolvedValueOnce({ stream: newCapture, sourceId: 'screen:new' });
    svc.produceEncrypted = vi
      .fn()
      .mockReturnValueOnce(oldPublication.promise)
      .mockReturnValueOnce(newPublication.promise);

    try {
      const oldStart = svc.produceScreen('screen:old', { streamAudio: false });
      await vi.waitFor(() => expect(svc.produceEncrypted).toHaveBeenCalledOnce());

      const newStart = svc.produceScreen('screen:new', { streamAudio: false });
      await vi.waitFor(() => expect(svc.produceEncrypted).toHaveBeenCalledTimes(2));

      newPublication.resolve(newProducer);
      await newStart;
      expect(svc.localScreenStream).toBe(newCapture);

      oldPublication.resolve(oldProducer);
      await oldStart;
    } finally {
      svc.captureScreen = realCaptureScreen;
      restorePublication();
    }

    expect(oldProducer.close).toHaveBeenCalledOnce();
    expect(oldCaptureVideo.stop).toHaveBeenCalledOnce();
    expect(svc.producers.get('screen')).toBe(newProducer);
    expect(svc.localScreenStream).toBe(newCapture);
  });

  it('lets the newer share win when the older capture resolves last', async () => {
    const oldCaptureVideo = liveTrack('capture-race-old-video');
    const oldCapture = streamOf([oldCaptureVideo]);
    const newCapture = streamOf([liveTrack('capture-race-new-video')]);
    const newProducer = { id: 'capture-race-new-producer', close: vi.fn(), on: vi.fn() };
    const oldCaptureResult = deferred<{ stream: ReturnType<typeof streamOf>; sourceId: string }>();
    const realCaptureScreen = svc.captureScreen;
    const restorePublication = stubScreenPublication(svc);
    const produceEncrypted = vi.fn().mockResolvedValue(newProducer);

    svc.producers.delete('screen');
    useVoiceStore.getState().setScreenSharing(false);
    svc.captureScreen = vi
      .fn()
      .mockReturnValueOnce(oldCaptureResult.promise)
      .mockResolvedValueOnce({ stream: newCapture, sourceId: 'screen:new' });
    svc.produceEncrypted = produceEncrypted;

    try {
      const oldStart = svc.produceScreen('screen:old', {
        streamAudio: false,
        contentType: 'detail',
      });
      await vi.waitFor(() => expect(svc.captureScreen).toHaveBeenCalledOnce());

      const newStart = svc.produceScreen('screen:new', {
        streamAudio: false,
        contentType: 'motion',
      });
      await newStart;
      expect(svc.localScreenStream).toBe(newCapture);
      expect(svc.producers.get('screen')).toBe(newProducer);

      oldCaptureResult.resolve({ stream: oldCapture, sourceId: 'screen:old' });
      await oldStart;
      expect(produceEncrypted).toHaveBeenCalledOnce();
      expect(newProducer.close).not.toHaveBeenCalled();
    } finally {
      svc.captureScreen = realCaptureScreen;
      restorePublication();
    }

    expect(oldCaptureVideo.stop).toHaveBeenCalledOnce();
    expect(svc.localScreenStream).toBe(newCapture);
    expect(svc.producers.get('screen')).toBe(newProducer);
    expect(svc.currentScreenSourceId).toBe('screen:new');
    expect(svc.currentScreenOptions.contentType).toBe('motion');
  });

  it('does not publish when cancellation lands after capture planning but before publication', async () => {
    const candidateVideo = liveTrack('handoff-race-video');
    const candidateStream = streamOf([candidateVideo]);
    const captureResult = deferred<{ stream: ReturnType<typeof streamOf>; sourceId: string }>();
    const restorePublication = stubScreenPublication(svc);
    const realCaptureScreen = svc.captureScreen;
    const realCapturePlan = svc.captureScreenForProduction.bind(svc);
    const staleProducer = { id: 'stale-handoff-producer', close: vi.fn(), on: vi.fn() };
    const produceEncrypted = vi.fn().mockResolvedValue(staleProducer);

    svc.producers.delete('screen');
    svc.captureScreen = vi.fn().mockReturnValue(captureResult.promise);
    svc.produceEncrypted = produceEncrypted;
    svc.captureScreenForProduction = async (...args: any[]) => {
      const plan = await realCapturePlan(...args);
      queueMicrotask(() => svc.cancelVideoReproduce('screen'));
      return plan;
    };

    try {
      const start = svc.produceScreen('screen:handoff-race', { streamAudio: false });
      await vi.waitFor(() => expect(svc.captureScreen).toHaveBeenCalledOnce());

      captureResult.resolve({ stream: candidateStream, sourceId: 'screen:handoff-race' });
      await start;
    } finally {
      svc.captureScreen = realCaptureScreen;
      delete svc.captureScreenForProduction;
      restorePublication();
    }

    expect(produceEncrypted).not.toHaveBeenCalled();
    expect(candidateVideo.stop).toHaveBeenCalledOnce();
  });

  it('runs a queued newer switch after fallback re-publication finishes', async () => {
    const firstCapture = streamOf([liveTrack('fallback-first-video')]);
    const secondCapture = streamOf([liveTrack('fallback-second-video')]);
    const fallbackProducer = {
      id: 'fallback-screen',
      closed: false,
      close: vi.fn(),
      pause: vi.fn(),
      resume: vi.fn(),
      replaceTrack: vi.fn().mockResolvedValue(undefined),
      on: vi.fn(),
    };
    const replaceFailure = deferred<never>();
    const restorePublication = stubScreenPublication(svc);
    const realCaptureScreen = svc.captureScreen;
    svc.screenAudioBridge = null;
    const captureScreen = vi
      .fn()
      .mockResolvedValueOnce({ stream: firstCapture, sourceId: 'screen:first' })
      .mockResolvedValueOnce({ stream: secondCapture, sourceId: 'screen:second' });
    svc.captureScreen = captureScreen;
    svc.produceEncrypted = vi.fn().mockResolvedValue(fallbackProducer);
    producer.replaceTrack = vi.fn().mockReturnValueOnce(replaceFailure.promise);

    try {
      const firstSwitch = svc.switchScreenSource('screen:first', { streamAudio: false });
      await vi.waitFor(() => expect(producer.replaceTrack).toHaveBeenCalledOnce());

      const secondSwitch = svc.switchScreenSource('screen:second', { streamAudio: false });
      replaceFailure.reject(new Error('codec cannot accept track'));
      await Promise.all([firstSwitch, secondSwitch]);
    } finally {
      svc.captureScreen = realCaptureScreen;
      restorePublication();
    }

    expect(captureScreen).toHaveBeenCalledTimes(2);
    expect(fallbackProducer.replaceTrack).toHaveBeenCalledWith({
      track: secondCapture.getVideoTracks()[0],
    });
    expect(svc.localScreenStream).toBe(secondCapture);
  });

  // `replaceTrack` can reject BECAUSE the producer was closed under us. The fallback
  // added for the codec-incompatible case would then close a share that is already gone
  // and re-produce it -- resurrecting the screen AND its system audio after the user
  // explicitly stopped sharing.
  it('does NOT resurrect the share when replaceTrack rejects because it ended', async () => {
    const newStream = streamOf([liveTrack('new-video')]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });
    svc.closeProducer = vi.fn().mockResolvedValue(undefined);
    svc.produceScreen = vi.fn().mockResolvedValue(undefined);
    producer.replaceTrack = vi.fn().mockImplementation(async () => {
      // The stop lands inside the pending replaceTrack, which is what rejects it.
      svc.cancelVideoReproduce('screen');
      throw new Error('producer closed');
    });

    await svc.switchScreenSource('screen:1');

    expect(svc.closeProducer).not.toHaveBeenCalled();
    expect(svc.produceScreen).not.toHaveBeenCalled();
    expect(newStream.getTracks()[0].stop).toHaveBeenCalled();
  });

  // The retire await drains the transport queue. A source ending inside it fires `ended`
  // on a track with no handler attached, and assigning one afterwards does not replay the
  // missed event -- the producer stayed mapped and isScreenSharing true over a dead track.
  it('installs the new track ended handler BEFORE awaiting the audio retire', async () => {
    const newTrack = liveTrack('new-video');
    const newStream = streamOf([newTrack]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });
    let handlerAtDrain: unknown = 'unset';
    // `voiceService` is a singleton and this suite's beforeEach does not restore private
    // methods, so an unrestored stub here leaks into every later test in the file.
    const realRetire = svc.retireScreenAudioProducer;
    svc.retireScreenAudioProducer = vi.fn().mockImplementation(async () => {
      handlerAtDrain = (newTrack as unknown as { onended?: unknown }).onended;
    });
    try {
      await svc.switchScreenSource('screen:1');
    } finally {
      svc.retireScreenAudioProducer = realRetire;
    }

    expect(typeof handlerAtDrain).toBe('function');
  });

  it('re-produces screen audio from the NEW stream, not the old one', async () => {
    // The share is currently carrying audio, so the switch must carry it across.
    useVoiceStore.getState().setScreenAudioOn(true);
    const newStream = streamOf([liveTrack('new-video'), liveTrack('new-audio', 'audio')]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });
    const audioProducer = { id: 'audio-1', close: vi.fn(), on: vi.fn() };
    svc.producers.set('screen-audio', audioProducer);

    await svc.switchScreenSource('screen:1');

    // The old audio producer belongs to the old capture and must not outlive it.
    expect(audioProducer.close).toHaveBeenCalled();
    expect(svc.produceScreenAudioFromStream).toHaveBeenCalledWith(newStream);
  });

  // The defect this guards: switching sources used to call
  // produceScreenAudioFromStream unconditionally, so a user who had deliberately
  // turned audio OFF had it silently switched back ON by an action with no
  // relationship to audio -- and because the swap preserves producer.id, viewers
  // received no new-share event telling them sound had returned.
  it('does NOT re-enable audio the user turned off', async () => {
    useVoiceStore.getState().setScreenAudioOn(false);
    const newStream = streamOf([liveTrack('new-video'), liveTrack('new-audio', 'audio')]);
    svc.captureScreen = vi.fn().mockResolvedValue({ stream: newStream, sourceId: 'screen:0' });

    await svc.switchScreenSource('screen:1');

    expect(svc.produceScreenAudioFromStream).not.toHaveBeenCalled();
    // ...and the capture itself must not have requested audio either.
    expect(svc.captureScreen).toHaveBeenCalledWith(
      'screen:1',
      expect.anything(),
      expect.anything(),
      false
    );
  });

  // Finding 3: the id must name the source actually being shared. It used to be
  // written inside captureScreenElectron BEFORE getUserMedia could fail, so a failed
  // switch left it naming the target that failed -- after which the audio toggle's
  // re-capture path would swap the live share to that other monitor.
  it('leaves currentScreenSourceId naming the still-live source after a failed switch', async () => {
    svc.currentScreenSourceId = 'screen:0';
    svc.captureScreen = vi.fn().mockRejectedValue(new Error('display unplugged'));

    await svc.switchScreenSource('screen:1');

    expect(svc.currentScreenSourceId).toBe('screen:0');
  });

  it('does nothing when there is no active screen producer to switch', async () => {
    svc.producers.delete('screen');
    svc.captureScreen = vi.fn();

    await svc.switchScreenSource('screen:1');

    expect(svc.captureScreen).not.toHaveBeenCalled();
  });
});

/**
 * Who owns the screen-audio outcome when a switch re-captures (#3349).
 *
 * `switchScreenSourceQueued` tears down the PREVIOUS share's audio AFTER the new
 * capture has already run. The per-process arm writes the authoritative new state
 * DURING that capture, so an unconditional teardown destroyed what the capture had
 * just established -- in two opposite directions, neither of which any existing test
 * could see:
 *
 *   SUCCESS -- it stopped the bridge the arm had just installed and reaped the child
 *   `startAudiocapHost` had just started, leaving `produceScreenAudioFromStream` to
 *   read a stream whose audio track had already ended. Per-process audio never
 *   survived a switch at all.
 *
 *   REFUSAL -- it replaced the arm's `mode: 'degraded'` and its reason with a bare
 *   `mode: 'off'`, so a user whose app could not be captured was told nothing rather
 *   than why. The mechanism string was computed and then discarded, which is the
 *   failure class this epic exists to close.
 *
 * Both were found by following a Gitar review finding one layer deeper; its own
 * analysis named neither. The CONTROL case is first on purpose: without it, a failure
 * in either regression case is indistinguishable from a fixture that never reached the
 * per-process rung.
 */
describe('per-process audio ownership across a switch (#3349)', () => {
  /* eslint-disable @typescript-eslint/no-explicit-any -- the capture seam and its state
     are private; these cases drive the real singleton, which is the only way the
     interaction between the capture and the teardown is under test at all. */
  let svc: any;
  let audiocapStop: ReturnType<typeof vi.fn>;

  // `window:<handle>:<n>`, not `window:7` -- `canCarryScreenAudio` requires
  // `parseWindowSourceId` to resolve, and a bare `window:7` never reaches the rung.
  const WINDOW_ID = 'window:9:0';

  const captureStream = () => ({
    getTracks: () => [],
    getVideoTracks: () => [liveTrack('new-video')],
    getAudioTracks: () => [],
    removeTrack: vi.fn(),
    // The arm attaches the bridge track to the video-only stream. Without this the
    // call throws into the arm's catch and reports 'no-backend' -- a harness gap that
    // reads exactly like a real degrade.
    addTrack: vi.fn(),
  });

  const arm = (startResult: unknown) => {
    resetAllStores();
    svc = voiceService as any;

    // A SIBLING describe, so the suite above's `beforeEach` does not run here and this
    // has to stand up the live share itself. Getting that wrong is not a loud failure:
    // `switchScreenSourceQueued` returns at its "no active screen share" guard, the
    // capture never runs, and every assertion below reads untouched state -- which is
    // exactly how this first shipped. The CONTROL case did not catch it, because it
    // drives `captureScreenElectron` directly and needs none of this.
    svc.producers.clear();
    svc.producers.set('screen', {
      id: 'producer-STABLE',
      closed: false,
      paused: false,
      close: vi.fn(),
      pause: vi.fn(),
      resume: vi.fn(),
      replaceTrack: vi.fn().mockResolvedValue(undefined),
      on: vi.fn(),
    });
    svc.socket = { emit: vi.fn() };
    svc.sendTransport = { id: 'transport-1' };
    svc.localScreenStream = streamOf([liveTrack('old-video')]);
    svc.videoReproduceSessionActive = true;
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    svc.produceScreenAudioFromStream = vi.fn().mockResolvedValue(undefined);
    svc.resolveCaptureDims = vi.fn().mockResolvedValue({ w: 1920, h: 1080 });
    svc.clampScreenToEntitlement = vi.fn().mockReturnValue({ width: 1920, height: 1080, fps: 30 });

    svc.cachedPlatform = 'darwin';
    svc.ensurePlatform = vi.fn().mockResolvedValue(undefined);
    svc.publishScreenAudioCapability = vi.fn().mockResolvedValue(undefined);
    svc.screenAudioBridge = null;
    useVoiceStore.setState({ machineScreenAudioCapable: true } as any);

    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: vi.fn().mockResolvedValue(captureStream()), getDisplayMedia: vi.fn() },
      configurable: true,
    });

    audiocapStop = vi.fn().mockResolvedValue(undefined);
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([{ id: WINDOW_ID, name: 'W' }]),
      audiocap: { start: vi.fn().mockResolvedValue(startResult), stop: audiocapStop },
    } as unknown as typeof globalThis.electron;

    // Restore the real seams that the sibling describe may have stubbed. The capture
    // must traverse acquireScreenCapture -> captureScreen -> captureScreenElectron so
    // dropping either currentness handoff makes these tests fail.
    const prototype = Object.getPrototypeOf(svc);
    for (const method of [
      'acquireScreenCapture',
      'captureScreen',
      'closeProducer',
      'produceScreen',
    ]) {
      svc[method] = prototype[method].bind(svc);
    }
  };

  it('CONTROL: a bare per-process capture reaches the rung and installs a bridge', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');
    expect(svc.screenAudioBridge).not.toBeNull();
  });

  // The SECOND control, and the one the first could not stand in for: it proves the
  // switch reaches the capture at all. Without it a missing piece of share state makes
  // `switchScreenSourceQueued` return at its guard, and the three cases below read
  // untouched state and fail as though the fix were absent.
  it('CONTROL: the switch reaches the capture', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });

    await svc.switchScreenSource(WINDOW_ID, { streamAudio: true });

    expect(globalThis.electron.audiocap?.start).toHaveBeenCalledTimes(1);
  });

  it('invalidates a pending per-process start when a newer direct share wins', async () => {
    const pendingStart = deferred<{ ok: true; generation: number; perProcessAudio: true }>();
    arm(pendingStart.promise);
    const restorePublication = stubScreenPublication(svc);
    const oldVideo = liveTrack('pending-per-process-video');
    const newVideo = liveTrack('newer-video-only');
    const oldStream = streamOf([oldVideo]);
    const newStream = streamOf([newVideo]);
    const newProducer = { id: 'newer-screen', close: vi.fn(), on: vi.fn() };
    const getUserMedia = vi.fn().mockResolvedValueOnce(oldStream).mockResolvedValueOnce(newStream);
    const produceEncrypted = vi.fn().mockResolvedValue(newProducer);
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia, getDisplayMedia: vi.fn() },
      configurable: true,
    });
    svc.producers.delete('screen');
    svc.produceEncrypted = produceEncrypted;

    try {
      const oldStart = svc.produceScreen(WINDOW_ID, {
        streamAudio: true,
        contentType: 'detail',
      });
      await vi.waitFor(() => expect(globalThis.electron.audiocap?.start).toHaveBeenCalledOnce());

      const newStart = svc.produceScreen(WINDOW_ID, {
        streamAudio: false,
        contentType: 'motion',
      });
      await newStart;
      expect(svc.localScreenStream).toBe(newStream);
      expect(svc.producers.get('screen')).toBe(newProducer);

      expect(audiocapStop).toHaveBeenCalledTimes(2);
      pendingStart.resolve({ ok: true, generation: 7, perProcessAudio: true });
      await oldStart;
    } finally {
      restorePublication();
    }

    expect(oldVideo.stop).toHaveBeenCalledOnce();
    expect(svc.screenAudioBridge).toBeNull();
    expect(useVoiceStore.getState().screenAudio.mode).not.toBe('per-process');
    expect(svc.localScreenStream).toBe(newStream);
    expect(svc.producers.get('screen')).toBe(newProducer);
    expect(svc.currentScreenOptions.contentType).toBe('motion');
    expect(produceEncrypted).toHaveBeenCalledOnce();
  });

  it('retires an installed per-process bridge when a newer video share wins', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const restorePublication = stubScreenPublication(svc);
    const oldVideo = liveTrack('installed-per-process-video');
    const newVideo = liveTrack('newer-video-only');
    const oldStream = { ...streamOf([oldVideo]), addTrack: vi.fn() };
    const newStream = { ...streamOf([newVideo]), addTrack: vi.fn() };
    const oldProducer = { id: 'stale-screen', close: vi.fn(), on: vi.fn() };
    const newProducer = { id: 'authoritative-screen', close: vi.fn(), on: vi.fn() };
    const oldPublication = deferred<typeof oldProducer>();
    const bridge = {
      track: { id: 'installed-per-process-track', kind: 'audio', readyState: 'live' },
      stop: vi.fn(),
    };
    const getUserMedia = vi.fn().mockResolvedValueOnce(oldStream).mockResolvedValueOnce(newStream);
    const produceEncrypted = vi
      .fn()
      .mockReturnValueOnce(oldPublication.promise)
      .mockResolvedValueOnce(newProducer);
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia, getDisplayMedia: vi.fn() },
      configurable: true,
    });
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => bridge as any);
    svc.producers.delete('screen');
    svc.produceEncrypted = produceEncrypted;

    try {
      const oldStart = svc.produceScreen(WINDOW_ID, {
        streamAudio: true,
        contentType: 'detail',
      });
      await vi.waitFor(() => expect(produceEncrypted).toHaveBeenCalledOnce());
      expect(svc.screenAudioBridge).toBe(bridge);
      expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');

      const newStart = svc.produceScreen(WINDOW_ID, {
        streamAudio: false,
        contentType: 'motion',
      });
      await newStart;
      expect(svc.localScreenStream).toBe(newStream);
      expect(svc.producers.get('screen')).toBe(newProducer);

      expect(bridge.stop).toHaveBeenCalledOnce();
      expect(audiocapStop).toHaveBeenCalledTimes(2);
      oldPublication.resolve(oldProducer);
      await oldStart;
    } finally {
      restorePublication();
    }

    expect(oldProducer.close).toHaveBeenCalledOnce();
    expect(oldVideo.stop).toHaveBeenCalledOnce();
    expect(svc.screenAudioBridge).toBeNull();
    expect(useVoiceStore.getState().screenAudio.mode).not.toBe('per-process');
    expect(svc.localScreenStream).toBe(newStream);
    expect(svc.producers.get('screen')).toBe(newProducer);
    expect(svc.currentScreenOptions.contentType).toBe('motion');
  });

  it('reaps the per-process host and bridge when video publication fails before commit', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const restorePublication = stubScreenPublication(svc);
    const bridge = {
      track: { id: 'failed-publication-track', kind: 'audio', readyState: 'live' },
      stop: vi.fn(),
    };
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => bridge as any);
    svc.producers.delete('screen');
    svc.produceEncrypted = vi.fn().mockRejectedValue(new Error('video publication failed'));

    try {
      await expect(svc.produceScreen(WINDOW_ID, { streamAudio: true })).rejects.toThrow(
        'video publication failed'
      );
    } finally {
      restorePublication();
    }

    expect(bridge.stop).toHaveBeenCalledOnce();
    expect(audiocapStop).toHaveBeenCalledTimes(2);
    expect(svc.screenAudioBridge).toBeNull();
    expect(useVoiceStore.getState().screenAudio.mode).toBe('off');
  });

  it('a SUCCESSFUL per-process re-capture survives the switch', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const oldBridge = { stop: vi.fn(), track: { kind: 'audio' } };
    const newBridge = {
      stop: vi.fn(),
      track: { id: 'new-audiocap-track', kind: 'audio', readyState: 'live', muted: false },
    };
    svc.screenAudioBridge = oldBridge;
    useVoiceStore.getState().setScreenAudioState({ mode: 'per-process', overrun: 1 });
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => newBridge as any);

    await svc.switchScreenSource(WINDOW_ID, { streamAudio: true });

    // The store, not a spy on our own setter: a spy passes the moment any arm calls it.
    expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');
    expect(oldBridge.stop).toHaveBeenCalledTimes(1);
    expect(audiocapStop).toHaveBeenCalledTimes(1);
    expect(newBridge.stop).not.toHaveBeenCalled();
    expect(svc.screenAudioBridge).not.toBeNull();
  });

  it('a REFUSED per-process re-capture keeps its degrade reason across the switch', async () => {
    arm({ ok: false, reason: 'target-unresolved' });

    await svc.switchScreenSource(WINDOW_ID, { streamAudio: true });

    expect(useVoiceStore.getState().screenAudio).toMatchObject({
      mode: 'degraded',
      reason: 'target-unresolved',
    });
  });

  // Gitar's own finding, stated as the property rather than as its suggested patch: a
  // refusal must not leave the inherited host running. `audiocap:stop` is the half that
  // reaps the CHILD -- stopping the bridge alone closes only the renderer's port, and
  // `startAudiocapHost`'s kill-first ordering does not help when a fence refused before
  // main was ever reached.
  it('a REFUSED per-process re-capture still reaps the inherited capture child', async () => {
    arm({ ok: false, reason: 'target-unresolved' });

    await svc.switchScreenSource(WINDOW_ID, { streamAudio: true });

    expect(audiocapStop).toHaveBeenCalled();
  });

  it('keeps the existing video, audio host, and state when replacement video capture rejects', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const existingBridge = { stop: vi.fn(), track: { kind: 'audio' } };
    svc.screenAudioBridge = existingBridge;
    useVoiceStore.getState().setScreenAudioState({ mode: 'per-process', overrun: 3 });
    const replacementCapture = vi.fn().mockRejectedValue(new Error('video capture denied'));
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: replacementCapture, getDisplayMedia: vi.fn() },
      configurable: true,
    });

    await expect(
      svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true)
    ).rejects.toThrow('video capture denied');

    expect(existingBridge.stop).not.toHaveBeenCalled();
    expect(audiocapStop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'per-process', overrun: 3 });
  });

  it('preserves the newly acquired per-process host and publishes live audio after replaceTrack rejects', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });

    const capturedVideo = liveTrack('new-video') as ReturnType<typeof liveTrack> & {
      getSettings: () => { width: number; height: number };
    };
    capturedVideo.getSettings = () => ({ width: 1280, height: 720 });
    const tracks = [capturedVideo];
    const captured = {
      getTracks: () => tracks,
      getVideoTracks: () => tracks.filter((track) => track.kind === 'video'),
      getAudioTracks: () => tracks.filter((track) => track.kind === 'audio'),
      removeTrack: vi.fn((track: (typeof tracks)[number]) => {
        const index = tracks.indexOf(track);
        if (index >= 0) tracks.splice(index, 1);
      }),
      addTrack: vi.fn((track: (typeof tracks)[number]) => tracks.push(track)),
    };
    const getUserMedia = vi.fn().mockResolvedValue(captured);
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia, getDisplayMedia: vi.fn() },
      configurable: true,
    });

    const newBridge = {
      track: {
        id: 'new-audiocap-track',
        kind: 'audio',
        readyState: 'live',
        muted: false,
        stop: vi.fn(),
      },
      stop: vi.fn(),
    };
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => newBridge as any);

    const oldProducer = svc.producers.get('screen');
    oldProducer.replaceTrack = vi.fn().mockRejectedValue(new Error('codec cannot accept track'));
    svc.device = {};
    svc.produceEncrypted = vi.fn().mockImplementation(async (_transport: unknown, params: any) => ({
      id: params.appData.source === 'screen' ? 'new-screen' : 'new-screen-audio',
      closed: false,
      close: vi.fn(),
      on: vi.fn(),
    }));
    svc.pickScreenCodec = vi.fn().mockReturnValue({
      codec: { mimeType: 'video/VP8' },
      encodings: [],
      effectiveBitrate: 1_000_000,
    });
    svc.requireSelectedVideoCodec = vi.fn().mockReturnValue({ mimeType: 'video/VP8' });
    svc.startPacketLossMonitor = vi.fn();
    svc.applyDegradationPreference = vi.fn();
    svc.getProducerCodecMimeType = vi.fn().mockReturnValue('video/VP8');
    svc.produceScreenAudioFromStream =
      Object.getPrototypeOf(svc).produceScreenAudioFromStream.bind(svc);

    await svc.switchScreenSource(WINDOW_ID, { streamAudio: true });

    expect(newBridge.stop, 'preservation of the newly acquired host/bridge').not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio.mode).toBe('per-process');
    // One stop is expected: it supersedes the inherited host before the new host starts.
    // A second stop means fallback cleanup reaped the newly acquired host.
    expect(audiocapStop, 'fallback must not stop the newly acquired host').toHaveBeenCalledTimes(1);
    expect(svc.screenAudioBridge, 'preservation of the newly acquired host/bridge').toBe(newBridge);
    expect(
      getUserMedia,
      'fallback must publish the already acquired capture'
    ).toHaveBeenCalledOnce();
    expect(svc.producers.get('screen-audio')?.id, 'publish a live audio track').toBe(
      'new-screen-audio'
    );
    expect(svc.produceEncrypted).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({ track: newBridge.track, appData: { source: 'screen-audio' } })
    );
  });

  it('does not re-produce after an explicit stop wins during fallback cleanup', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: false });
    const capturedVideo = liveTrack('replacement-video');
    const captured = streamOf([capturedVideo]);
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: vi.fn().mockResolvedValue(captured), getDisplayMedia: vi.fn() },
      configurable: true,
    });

    const oldProducer = svc.producers.get('screen');
    oldProducer.replaceTrack = vi.fn().mockRejectedValue(new Error('codec cannot accept track'));
    svc.produceScreen = vi.fn().mockResolvedValue(undefined);

    let releaseDrain!: () => void;
    const pendingDrain = new Promise<void>((resolve) => {
      releaseDrain = resolve;
    });
    const drainStarted = vi.fn();
    svc.drainSendTransportQueue = vi.fn(() => {
      drainStarted();
      return pendingDrain;
    });

    const switching = svc.switchScreenSource(WINDOW_ID, { streamAudio: false });
    await vi.waitFor(() => expect(drainStarted).toHaveBeenCalledOnce());

    // The user explicitly stops sharing while the fallback close is waiting for the
    // transport queue. Both closes share the same deferred drain so the old fallback
    // continuation resumes before the stop's own cleanup completes.
    const stopping = svc.closeProducer('screen');
    expect(drainStarted).toHaveBeenCalledTimes(2);
    releaseDrain();
    await Promise.all([switching, stopping]);

    expect(svc.produceScreen).not.toHaveBeenCalled();
    expect(capturedVideo.stop).toHaveBeenCalled();
  });

  it('reaps a stale per-process fallback during emergency cleanup', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const capturedVideo = liveTrack('replacement-video') as ReturnType<typeof liveTrack> & {
      getSettings: () => { width: number; height: number };
    };
    capturedVideo.getSettings = () => ({ width: 1280, height: 720 });
    const tracks = [capturedVideo];
    const captured = {
      getTracks: () => tracks,
      getVideoTracks: () => tracks.filter((track) => track.kind === 'video'),
      getAudioTracks: () => tracks.filter((track) => track.kind === 'audio'),
      removeTrack: vi.fn((track: (typeof tracks)[number]) => {
        const index = tracks.indexOf(track);
        if (index >= 0) tracks.splice(index, 1);
      }),
      addTrack: vi.fn((track: (typeof tracks)[number]) => tracks.push(track)),
    };
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: vi.fn().mockResolvedValue(captured), getDisplayMedia: vi.fn() },
      configurable: true,
    });

    const capturedBridge = {
      stop: vi.fn(),
      track: {
        id: 'captured-audio',
        kind: 'audio',
        readyState: 'live',
        muted: false,
        stop: vi.fn(),
      },
    };
    svc.screenAudioBridge = { stop: vi.fn(), track: { kind: 'audio' } };
    useVoiceStore.getState().setScreenAudioState({ mode: 'per-process', overrun: 1 });
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => capturedBridge as any);

    const oldProducer = svc.producers.get('screen');
    oldProducer.replaceTrack = vi.fn().mockRejectedValue(new Error('codec cannot accept track'));
    svc.produceScreen = vi.fn().mockResolvedValue(undefined);

    let releaseDrain!: () => void;
    const drainStarted = vi.fn();
    svc.drainSendTransportQueue = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          drainStarted();
          releaseDrain = resolve;
        })
    );

    const switching = svc.switchScreenSource(WINDOW_ID, { streamAudio: true });
    await vi.waitFor(() => expect(drainStarted).toHaveBeenCalledOnce());
    const inheritedStopCount = audiocapStop.mock.calls.length;
    expect(svc.screenAudioBridge).toBe(capturedBridge);
    expect(capturedBridge.stop).not.toHaveBeenCalled();

    svc.emergencyCleanup();
    expect(capturedBridge.stop).toHaveBeenCalled();
    expect(audiocapStop.mock.calls.length).toBeGreaterThan(inheritedStopCount);
    expect(svc.screenAudioBridge).toBeNull();
    releaseDrain();
    await switching;

    expect(svc.produceScreen).not.toHaveBeenCalled();
    expect(capturedVideo.stop).toHaveBeenCalled();
  });

  it('stops the full existing host when bridge construction fails after audiocap start', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const existingBridge = { stop: vi.fn(), track: { kind: 'audio' } };
    svc.screenAudioBridge = existingBridge;
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => {
      throw new Error('bridge unsupported');
    });

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(audiocapStop).toHaveBeenCalledTimes(2);
    expect(useVoiceStore.getState().screenAudio).toMatchObject({
      mode: 'degraded',
      reason: 'no-backend',
    });
  });

  it('stops the new bridge and full host when stream track attachment fails after audiocap start', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    const existingBridge = { stop: vi.fn(), track: { kind: 'audio' } };
    const bridge = {
      stop: vi.fn(),
      track: {
        id: 'named-bridge-track',
        kind: 'audio',
        readyState: 'live',
        muted: false,
        stop: vi.fn(),
      },
    };
    svc.screenAudioBridge = existingBridge;
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => bridge);
    const stream = captureStream();
    stream.addTrack = vi.fn(() => {
      throw new Error('track attachment failed');
    });
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: vi.fn().mockResolvedValue(stream), getDisplayMedia: vi.fn() },
      configurable: true,
    });

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(bridge.stop).toHaveBeenCalled();
    expect(audiocapStop).toHaveBeenCalledTimes(2);
    expect(useVoiceStore.getState().screenAudio).toMatchObject({
      mode: 'degraded',
      reason: 'no-backend',
    });
  });

  // -- `degradeScreenAudio` writes the toast, not just the state (#3394 PR 2 M3) ---
  //
  // Before `degradeScreenAudio` existed, all three shapes below wrote
  // `voiceStore.screenAudio` alone. No component renders `mode: 'degraded'`, so a
  // refused per-process share (a Safari window, a silent Finder window) went live
  // with no sound and no explanation -- found on hardware. These cases assert the
  // OUTERMOST observable effect (`videoSlotError`, what `VoiceControls` actually
  // renders), not a spy on `degradeScreenAudio` or `setVideoSlotError` itself.

  it('CONTROL: a successful per-process capture leaves videoSlotError untouched', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(useVoiceStore.getState().videoSlotError).toBeNull();
  });

  it('tells the user why when the audiocap start invoke rejects (protocol-fault)', async () => {
    arm({ ok: true, generation: 7, perProcessAudio: true });
    // Shape 1: the invoke itself throws -- main rejected rather than returning an
    // outcome. `degradeScreenAudio` is called with the literal 'protocol-fault'.
    globalThis.electron.audiocap!.start = vi
      .fn()
      .mockRejectedValue(new Error('ipc boundary broke'));

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'degraded',
      reason: 'protocol-fault',
      overrun: 0,
    });
    expect(useVoiceStore.getState().videoSlotError).toBe(
      screenAudioDegradeMessage('protocol-fault')
    );
  });

  it('tells the user why when the audiocap start invoke resolves ok:false', async () => {
    // Shape 2: `start` resolves `{ ok: false, reason }` -- one of main's own fences
    // refused before a child was ever produced.
    arm({ ok: false, reason: 'target-unresolved' });

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'degraded',
      reason: 'target-unresolved',
      overrun: 0,
    });
    expect(useVoiceStore.getState().videoSlotError).toBe(
      screenAudioDegradeMessage('target-unresolved')
    );
  });

  it('tells the user why when bridge construction throws after a successful start (no-backend)', async () => {
    // Shape 3: the start succeeded but `createScreenAudioBridge` throws -- the
    // machine said it could and this renderer cannot.
    arm({ ok: true, generation: 7, perProcessAudio: true });
    vi.mocked(createScreenAudioBridge).mockImplementationOnce(() => {
      throw new Error('bridge unsupported');
    });

    await svc.captureScreenElectron(WINDOW_ID, { w: 1280, h: 720 }, 30, true);

    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'degraded',
      reason: 'no-backend',
      overrun: 0,
    });
    expect(useVoiceStore.getState().videoSlotError).toBe(screenAudioDegradeMessage('no-backend'));
  });

  it('does not attach a late audiocap completion after explicit share stop wins', async () => {
    let resolveStart!: (result: unknown) => void;
    const pendingStart = new Promise((resolve) => {
      resolveStart = resolve;
    });
    arm(pendingStart);
    const stream = captureStream();
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: vi.fn().mockResolvedValue(stream), getDisplayMedia: vi.fn() },
      configurable: true,
    });

    const capture = svc.switchScreenSource(WINDOW_ID, { streamAudio: true });
    await vi.waitFor(() => expect(globalThis.electron.audiocap?.start).toHaveBeenCalled());

    svc.invalidateVideoReproduces();
    svc.stopScreenAudioHost();
    resolveStart({ ok: true, generation: 7, perProcessAudio: true });
    await capture;

    expect(stream.addTrack).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  it('leaves a successor host untouched when invalidated during video-only capture', async () => {
    let resolveVideo!: (stream: unknown) => void;
    const pendingVideo = new Promise((resolve) => {
      resolveVideo = resolve;
    });
    arm({ ok: true, generation: 7, perProcessAudio: true });
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia: vi.fn().mockReturnValue(pendingVideo), getDisplayMedia: vi.fn() },
      configurable: true,
    });

    let current = true;
    const capture = svc.captureScreenElectron(
      WINDOW_ID,
      { w: 1280, h: 720 },
      30,
      true,
      () => current
    );
    await vi.waitFor(() =>
      expect(globalThis.navigator.mediaDevices.getUserMedia).toHaveBeenCalled()
    );

    const successor = { stop: vi.fn(), track: { kind: 'audio' } };
    svc.screenAudioBridge = successor;
    useVoiceStore.getState().setScreenAudioState({ mode: 'per-process', overrun: 4 });
    current = false;
    resolveVideo(captureStream());
    await capture;

    expect(successor.stop).not.toHaveBeenCalled();
    expect(audiocapStop).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'per-process', overrun: 4 });
  });

  it('leaves a successor host untouched when a stale audiocap start rejects', async () => {
    let rejectStart!: (error: Error) => void;
    const pendingStart = new Promise((_, reject) => {
      rejectStart = reject;
    });
    arm(pendingStart);

    let current = true;
    const capture = svc.captureScreenElectron(
      WINDOW_ID,
      { w: 1280, h: 720 },
      30,
      true,
      () => current
    );
    await vi.waitFor(() => expect(globalThis.electron.audiocap?.start).toHaveBeenCalled());

    const successor = { stop: vi.fn(), track: { kind: 'audio' } };
    svc.screenAudioBridge = successor;
    useVoiceStore.getState().setScreenAudioState({ mode: 'per-process', overrun: 5 });
    current = false;
    rejectStart(new Error('stale start rejected'));
    await capture;

    expect(successor.stop).not.toHaveBeenCalled();
    expect(audiocapStop).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'per-process', overrun: 5 });
  });
});
