/**
 * Live Stream Audio toggle (R5).
 *
 * The property that makes this worth having: toggling audio must never disturb the
 * VIDEO producer. Viewers keep watching throughout -- an audio choice should not cost
 * them a reconnect, which is what a naive stop-and-restart of the whole share would do.
 *
 * The awkward case is turning audio back ON for a share that started silent. There is
 * no audio track to re-produce, because getUserMedia was called with `audio: false`, so
 * the only honest way to satisfy the request is to re-capture the same source.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useVideoSettingsStore } from '@/renderer/stores/voice/videoSettingsStore';

const track = (id: string, kind: 'video' | 'audio', readyState = 'live') => ({
  id,
  kind,
  readyState,
  muted: false,
  stop: vi.fn(),
  contentHint: '',
});

const streamOf = (tracks: ReturnType<typeof track>[]) => ({
  getTracks: () => tracks,
  getVideoTracks: () => tracks.filter((t) => t.kind === 'video'),
  getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
  removeTrack: vi.fn(),
});

describe('voiceService.setScreenAudioEnabled (R5)', () => {
  let svc: any;
  let videoProducer: any;

  beforeEach(() => {
    resetAllStores();
    svc = voiceService as any;
    svc.producers.clear();
    svc.socket = { emit: vi.fn() };
    svc.sendTransport = { id: 't1' };
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    svc.produceScreenAudioFromStream = vi.fn().mockResolvedValue(undefined);
    // The QUEUED body, not the public entry point: setScreenAudioEnabled now runs
    // inside the per-screen reproduce tail, so calling the public switchScreenSource
    // from in there would chain onto the promise it is already running inside.
    svc.switchScreenSourceQueued = vi.fn().mockResolvedValue(undefined);
    // Every screen mutation is serialized on that tail, and enqueueVideoReproduce
    // returns without running the operation when no media session is live. Without
    // this, all five cases below pass or fail for the wrong reason -- four never
    // execute the body at all, and the fifth asserts nothing happened, which is
    // trivially true when nothing ran.
    svc.videoReproduceSessionActive = true;

    videoProducer = { id: 'video-1', close: vi.fn(), on: vi.fn() };
    svc.producers.set('screen', videoProducer);
  });

  it('turning audio OFF closes only the audio producer, never the video one', async () => {
    const audioProducer = { id: 'audio-1', close: vi.fn(), on: vi.fn() };
    svc.producers.set('screen-audio', audioProducer);
    svc.localScreenStream = streamOf([track('v', 'video'), track('a', 'audio')]);

    await svc.setScreenAudioEnabled(false);

    expect(audioProducer.close).toHaveBeenCalled();
    expect(svc.producers.has('screen-audio')).toBe(false);
    // The whole point: viewers are not interrupted.
    expect(videoProducer.close).not.toHaveBeenCalled();
    expect(svc.producers.get('screen')).toBe(videoProducer);
  });

  it('turning audio ON re-produces from the live track without re-capturing', async () => {
    svc.localScreenStream = streamOf([track('v', 'video'), track('a', 'audio')]);

    await svc.setScreenAudioEnabled(true);

    expect(svc.produceScreenAudioFromStream).toHaveBeenCalledWith(svc.localScreenStream);
    expect(svc.switchScreenSourceQueued).not.toHaveBeenCalled();
  });

  it('turning audio ON for a share captured WITHOUT audio re-captures the same source', async () => {
    // No audio track exists -- getUserMedia was called with audio:false, so there is
    // nothing to produce and re-capture is the only way to honour the request.
    svc.localScreenStream = streamOf([track('v', 'video')]);
    svc.currentScreenSourceId = 'screen:0';

    await svc.setScreenAudioEnabled(true);

    expect(svc.switchScreenSourceQueued).toHaveBeenCalledWith(
      'screen:0',
      expect.objectContaining({ source: 'screen' }),
      expect.objectContaining({ streamAudio: true })
    );
    expect(svc.produceScreenAudioFromStream).not.toHaveBeenCalled();
  });

  it('turning audio ON re-captures when the existing audio track is dead', async () => {
    svc.localScreenStream = streamOf([track('v', 'video'), track('a', 'audio', 'ended')]);
    svc.currentScreenSourceId = 'screen:0';

    await svc.setScreenAudioEnabled(true);

    expect(svc.switchScreenSourceQueued).toHaveBeenCalled();
    expect(svc.produceScreenAudioFromStream).not.toHaveBeenCalled();
  });

  // Three causes, three messages. A boolean here mislabels whichever case it forgot:
  // the first version said "share a whole screen" to a Linux user already sharing one,
  // and the second said it to a share whose source id was unknown.
  it.each([
    ['window:7', 'darwin', /single window/i],
    ['screen:0', 'linux', /Linux/i],
    [null, 'darwin', /cannot tell what this share is showing/i],
  ])('names the real reason audio is refused (%s on %s)', async (sourceId, platform, expected) => {
    svc.localScreenStream = streamOf([track('v', 'video')]);
    svc.currentScreenSourceId = sourceId;
    svc.cachedPlatform = platform;

    await svc.setScreenAudioEnabled(true);

    expect(useVoiceStore.getState().videoSlotError).toMatch(expected);
    expect(svc.produceScreenAudioFromStream).not.toHaveBeenCalled();
    expect(svc.switchScreenSourceQueued).not.toHaveBeenCalled();
  });

  // `closeProducer('screen-audio')` runs `cleanupScreenAudioState`, which STOPS and
  // removes the audio track. Using it here meant a plain On -> Off -> On could not
  // reuse the live capture and fell through to the re-capture branch, replacing the
  // VIDEO track and interrupting every viewer for a change that only touched audio.
  it('turning audio OFF leaves the captured track alive for the next ON', async () => {
    const audio = track('a', 'audio');
    svc.localScreenStream = streamOf([track('v', 'video'), audio]);
    svc.producers.set('screen-audio', { id: 'audio-1', close: vi.fn(), on: vi.fn() });

    await svc.setScreenAudioEnabled(false);

    expect(audio.stop).not.toHaveBeenCalled();
    expect(svc.localScreenStream.getAudioTracks()).toHaveLength(1);
    expect(svc.producers.has('screen-audio')).toBe(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);

    // ...and the next ON reuses it rather than re-capturing.
    await svc.setScreenAudioEnabled(true);
    expect(svc.produceScreenAudioFromStream).toHaveBeenCalledWith(svc.localScreenStream);
    expect(svc.switchScreenSourceQueued).not.toHaveBeenCalled();
  });

  // Enabling audio on a silent share re-captures. Reading the persisted store there
  // turned an audio-only action into a quality change: a transient 720p/15fps/detail
  // share silently became the stored 1080p/60fps/auto.
  it('re-captures with the LIVE share quality, not the persisted defaults', async () => {
    svc.localScreenStream = streamOf([track('v', 'video')]);
    svc.currentScreenSourceId = 'screen:0';
    svc.cachedPlatform = 'darwin';
    svc.currentScreenOptions = {
      resolution: '720p',
      frameRate: 15,
      contentType: 'detail',
      streamAudio: false,
    };
    useVideoSettingsStore.getState().setScreenResolution('1080p');
    useVideoSettingsStore.getState().setScreenFrameRate(60);

    await svc.setScreenAudioEnabled(true);

    expect(svc.switchScreenSourceQueued).toHaveBeenCalledWith(
      'screen:0',
      expect.anything(),
      expect.objectContaining({
        resolution: '720p',
        frameRate: 15,
        contentType: 'detail',
        streamAudio: true,
      })
    );
  });

  it('does nothing when no screen share is active', async () => {
    svc.producers.delete('screen');
    svc.localScreenStream = null;

    await svc.setScreenAudioEnabled(true);

    expect(svc.produceScreenAudioFromStream).not.toHaveBeenCalled();
    expect(svc.switchScreenSourceQueued).not.toHaveBeenCalled();
  });

  it('serializes concurrent toggles so a double-click cannot produce two audio producers', async () => {
    // A user-clickable button: two rapid clicks are ordinary traffic. Unserialized,
    // both calls see no `screen-audio` producer and both produce one.
    svc.localScreenStream = streamOf([track('v', 'video'), track('a', 'audio')]);
    let running = 0;
    let overlapped = false;
    svc.produceScreenAudioFromStream = vi.fn().mockImplementation(async () => {
      running += 1;
      if (running > 1) overlapped = true;
      await Promise.resolve();
      // Registers the producer, as the real implementation does. That is not fixture
      // decoration -- it is the state the idempotence guard reads, so a mock that
      // skipped it would report two productions no matter what the guard did, and the
      // duplicate-producer defect would stay invisible behind a passing test.
      svc.producers.set('screen-audio', { id: 'a1', closed: false, close: vi.fn(), on: vi.fn() });
      running -= 1;
    });

    await Promise.all([svc.setScreenAudioEnabled(true), svc.setScreenAudioEnabled(true)]);

    expect(overlapped).toBe(false);
    // ONE production, not two. Serializing the calls only stops them interleaving --
    // both would still find a live track and produce, and the second `producers.set`
    // would orphan the first, leaving viewers hearing the desktop mix twice.
    expect(svc.produceScreenAudioFromStream).toHaveBeenCalledTimes(1);
  });

  it('a second enable is a no-op while a live audio producer already exists', async () => {
    svc.localScreenStream = streamOf([track('v', 'video'), track('a', 'audio')]);
    svc.producers.set('screen-audio', {
      id: 'audio-1',
      closed: false,
      close: vi.fn(),
      on: vi.fn(),
    });

    await svc.setScreenAudioEnabled(true);

    expect(svc.produceScreenAudioFromStream).not.toHaveBeenCalled();
    expect(svc.switchScreenSourceQueued).not.toHaveBeenCalled();
  });
});
