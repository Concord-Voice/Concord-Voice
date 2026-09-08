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

    expect(svc.closeProducer).toHaveBeenCalledWith('screen');
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
