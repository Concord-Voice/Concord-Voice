/**
 * The CAPTURE seam — what is actually requested from the OS, and what is actually
 * published to the SFU.
 *
 * This file exists because `ScreenShareOptions.streamAudio` shipped with ZERO readers
 * and a full green suite. Every test written for it asserted that the picker EMITTED
 * the value and that the service FORWARDED it; none asserted that capture OBEYED it.
 * A suite shaped that way verifies a handshake and will pass forever while the
 * behaviour is absent.
 *
 * So the assertions here are deliberately made against `getUserMedia`'s constraints and
 * the resulting producer set, not against arguments passed between our own functions.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { voiceService } from '@/renderer/services/voice/voiceService';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { resetAllStores } from '../../helpers/store-helpers';

// The bridge builds a `MediaStreamTrackGenerator`, which jsdom does not implement.
// Stubbed because these cases are about what the capture SEAM records in the store,
// not about the bridge — `screenAudioBridge.test.ts` owns that.
vi.mock('@/renderer/services/voice/screenAudioBridge', () => ({
  createScreenAudioBridge: () => ({
    track: { kind: 'audio', readyState: 'live', muted: false, stop: vi.fn() },
    stop: vi.fn(),
  }),
}));

const track = (kind: 'video' | 'audio', over: Record<string, unknown> = {}) => ({
  kind,
  readyState: 'live',
  muted: false,
  stop: vi.fn(),
  contentHint: '',
  ...over,
});

const streamOf = (tracks: ReturnType<typeof track>[]) => ({
  getTracks: () => tracks,
  getVideoTracks: () => tracks.filter((t) => t.kind === 'video'),
  getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
  removeTrack: vi.fn(),
  // The per-process arm attaches the bridge track to the video-only stream. Without
  // this the call throws and lands in the arm's catch, which reports 'no-backend' —
  // a harness gap that reads exactly like a real degrade.
  addTrack: vi.fn(),
});

describe('screen capture honours the audio opt-out (capture seam)', () => {
  let svc: any;
  let getUserMedia: ReturnType<typeof vi.fn>;
  let origElectron: typeof globalThis.electron;

  beforeEach(() => {
    resetAllStores();
    svc = voiceService as any;
    svc.cachedPlatform = 'darwin';
    getUserMedia = vi.fn().mockResolvedValue(streamOf([track('video'), track('audio')]));
    Object.defineProperty(globalThis.navigator, 'mediaDevices', {
      value: { getUserMedia, getDisplayMedia: vi.fn() },
      configurable: true,
    });
    origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([{ id: 'screen:0', name: 'S' }]),
    } as unknown as typeof globalThis.electron;
  });

  afterEach(() => {
    globalThis.electron = origElectron;
  });

  const audioConstraintOf = (call: unknown[]) => (call[0] as { audio?: unknown }).audio;

  it('requests NO audio for a screen target when the user opted out', async () => {
    await svc.captureScreenElectron('screen:0', { w: 1280, h: 720 }, 30, false);
    expect(getUserMedia).toHaveBeenCalledTimes(1);
    expect(audioConstraintOf(getUserMedia.mock.calls[0])).toBe(false);
  });

  it('requests loopback audio for a screen target when the user opted in', async () => {
    await svc.captureScreenElectron('screen:0', { w: 1280, h: 720 }, 30, true);
    expect(audioConstraintOf(getUserMedia.mock.calls[0])).toMatchObject({
      mandatory: { chromeMediaSource: 'desktop', chromeMediaSourceId: 'screen:0' },
    });
  });

  // #2161 — the invariant that must survive every refactor of this path.
  it('requests NO audio for a window target even when audio is explicitly asked for', async () => {
    await svc.captureScreenElectron('window:7', { w: 1280, h: 720 }, 30, true);
    expect(audioConstraintOf(getUserMedia.mock.calls[0])).toBe(false);
  });

  // -- The live share RECORDS itself (Gitar review, PR #3349) ----------------
  //
  // This file's own header says why these exist: every earlier test asserted the
  // picker EMITTED a value and the service FORWARDED it, and none asserted capture
  // OBEYED. Same shape here one layer on — the per-process arm attached the track
  // and never wrote `screenAudio.mode`, so a live app-scoped share was
  // indistinguishable from an idle one, while `produceScreen`'s sibling comment
  // promised on this arm's behalf that it "sets the mode where it creates the track".

  const perProcessHarness = () => {
    useVoiceStore.getState().setMachineScreenAudioCapable(true);
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([{ id: 'window:42:0', name: 'W' }]),
      audiocap: {
        start: vi.fn().mockResolvedValue({ ok: true, generation: 1, perProcessAudio: true }),
        stop: vi.fn().mockResolvedValue(undefined),
        getPortMessageTag: vi.fn().mockReturnValue('tag'),
        onCapability: vi.fn(),
      },
    } as unknown as typeof globalThis.electron;
  };

  it('records a successful per-process capture as mode per-process', async () => {
    perProcessHarness();
    await svc.captureScreenElectron('window:42:0', { w: 1280, h: 720 }, 30, true);

    const { screenAudio } = useVoiceStore.getState();
    // Asserted on the STORE, never on a call to our own setter: the defect was a
    // missing write, and a spy on the setter would pass the moment any arm called it.
    expect(screenAudio).toEqual({ mode: 'per-process', overrun: 0 });
  });

  it('does not leave a previous system-mix share claiming the new one', async () => {
    // THE CASE WITH TEETH. `setScreenAudioState` is a REPLACE and
    // `stopScreenAudioHost` early-returns when the mode is already off, so an unset
    // mode is not merely absent — it is whatever the last share left. A per-process
    // share started after a whole-desktop one read `'system'`: the app claiming a
    // system mix while an app-scoped tap runs, which is a false statement about what
    // audio leaves this machine.
    useVoiceStore.getState().setScreenAudioState({ mode: 'system', overrun: 4 });
    perProcessHarness();
    await svc.captureScreenElectron('window:42:0', { w: 1280, h: 720 }, 30, true);

    const { screenAudio } = useVoiceStore.getState();
    expect(screenAudio.mode).not.toBe('system');
    expect(screenAudio.mode).toBe('per-process');
    // The counter belongs to the share, not the session — a previous share's drops
    // must not be attributed to this one.
    expect(screenAudio.overrun).toBe(0);
  });

  it('returns the RESOLVED source id so callers need not re-derive it', async () => {
    const out = await svc.captureScreenElectron(undefined, { w: 1280, h: 720 }, 30, true);
    expect(out.sourceId).toBe('screen:0');
  });

  // The platform gate lives at the CAPTURE seam, not in one caller's computation.
  // `switchScreenSourceQueued` does not share `produceScreen`'s inline gate, so a
  // switch confirmed on Linux before the picker's async platform probe settled asked
  // for a loopback that cannot work.
  it('refuses audio on Linux even for a screen: target', async () => {
    svc.cachedPlatform = 'linux';
    await svc.captureScreenElectron('screen:0', { w: 1280, h: 720 }, 30, true);
    expect(audioConstraintOf(getUserMedia.mock.calls[0])).toBe(false);
  });
});

describe('a dead audio track is never published (spec §7.3)', () => {
  let svc: any;

  beforeEach(() => {
    resetAllStores();
    svc = voiceService as any;
    svc.cachedPlatform = 'darwin';
    svc.producers.clear();
    svc.sendTransport = { id: 't1' };
    svc.produceEncrypted = vi.fn();
  });

  // macOS 14.2+ without the capture entitlement returns a LIVE BUT SILENT track: no
  // throw, so the try/catch fallback cannot see it. Publishing it advertises audio
  // nobody can hear.
  it('refuses an ended track and reports it instead of publishing silence', async () => {
    await svc.produceScreenAudioFromStream(streamOf([track('audio', { readyState: 'ended' })]));
    expect(svc.produceEncrypted).not.toHaveBeenCalled();
    expect(svc.producers.has('screen-audio')).toBe(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
    expect(useVoiceStore.getState().videoSlotError).toMatch(/without sound/i);
  });

  it('refuses a muted track for the same reason', async () => {
    await svc.produceScreenAudioFromStream(streamOf([track('audio', { muted: true })]));
    expect(svc.produceEncrypted).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
  });

  it('publishes a live track', async () => {
    svc.produceEncrypted = vi.fn().mockResolvedValue({ id: 'a1', on: vi.fn() });
    svc.localScreenStream = streamOf([track('audio')]);
    svc.producers.set('screen', { id: 'v1' });
    await svc.produceScreenAudioFromStream(svc.localScreenStream);
    expect(svc.produceEncrypted).toHaveBeenCalled();
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(true);
  });

  // `transportclose` never fires when only the AUDIO track dies -- the OS revoking the
  // capture, or the tap failing -- so without an ended handler the toolbar reported
  // sound as shared over silence and the next click cleared a stale flag instead of retrying.
  it('clears the live state when the audio track ends on its own', async () => {
    const audio = track('audio');
    const producer = { id: 'a1', on: vi.fn(), close: vi.fn() };
    svc.produceEncrypted = vi.fn().mockResolvedValue(producer);
    svc.socket = { emit: vi.fn() };
    svc.localScreenStream = streamOf([audio]);
    svc.producers.set('screen', { id: 'v1' });

    await svc.produceScreenAudioFromStream(svc.localScreenStream);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(true);

    (audio as unknown as { onended: () => void }).onended();

    expect(producer.close).toHaveBeenCalled();
    expect(svc.producers.has('screen-audio')).toBe(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
  });

  // The track OUTLIVES its producer: reProduceScreenAudio swaps in a successor for the
  // same track on a codec/layering/settings change. A handler bound to the ORIGINAL
  // producer then fails its own identity check and returns, leaving the successor mapped
  // over a dead track with the flag stuck true.
  it('re-points the ended handler at the successor after an audio re-produce', async () => {
    const audio = track('audio');
    const first = { id: 'a1', on: vi.fn(), close: vi.fn() };
    const second = { id: 'a2', on: vi.fn(), close: vi.fn() };
    svc.socket = { emit: vi.fn() };
    svc.localScreenStream = streamOf([audio]);
    svc.producers.set('screen', { id: 'v1' });

    svc.produceEncrypted = vi.fn().mockResolvedValue(first);
    await svc.produceScreenAudioFromStream(svc.localScreenStream);

    // DRIVE the real re-produce rather than standing in for it. Calling
    // `bindScreenAudioTrackEnded` directly here made this test pass against the
    // unfixed tree: it proved the helper works and said nothing about whether
    // `reProduceScreenAudio` calls it, which was the entire defect.
    svc.produceEncrypted = vi.fn().mockResolvedValue(second);
    svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
    svc.videoReproduceSessionActive = true;
    await svc.reProduceScreenAudio(undefined, svc.sendTransport);
    expect(svc.producers.get('screen-audio')).toBe(second);

    // The re-produce closes the predecessor itself; count from here so the assertion
    // below is about what `onended` did, not about that.
    const firstClosesBefore = first.close.mock.calls.length;

    (audio as unknown as { onended: () => void }).onended();

    expect(second.close).toHaveBeenCalled();
    expect(first.close.mock.calls.length).toBe(firstClosesBefore);
    expect(svc.producers.has('screen-audio')).toBe(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
  });

  // `getDisplayMedia` (dev/web) records no source id, so the id-based capability test
  // says "incapable" for a share that may be sending audio right now -- the toolbar
  // locked the button and the only way to stop was to end the whole share.
  it('reports capability from a live audio track when there is no source id', async () => {
    svc.cachedPlatform = 'darwin';
    svc.currentScreenSourceId = null;
    svc.currentScreenAudioCapable = true;
    expect(await svc.canShareScreenAudio()).toBe(true);

    svc.currentScreenAudioCapable = false;
    expect(await svc.canShareScreenAudio()).toBe(false);
  });

  // Closing a producer by hand does NOT fire `transportclose`, so reconnect teardown is
  // the ONLY thing that can clear this. A stale `true` survived a reconnect, the picker
  // seeded audio as enabled from it, and the next share broadcast system audio against
  // an explicit opt-out.
  it('clears the live screen-audio state during reconnect teardown', () => {
    useVoiceStore.getState().setScreenAudioOn(true);
    useVoiceStore.getState().setScreenAudioVerdict('system-loopback');
    svc.currentScreenSourceId = 'screen:0';
    svc.currentScreenAudioCapable = true;
    svc.producers.set('screen-audio', { id: 'a1', close: vi.fn() });

    svc.cleanupMediaAndTransports();

    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
    expect(useVoiceStore.getState().screenAudioVerdict).toBe('none');
    expect(svc.currentScreenSourceId).toBeNull();
    expect(svc.currentScreenAudioCapable).toBe(false);
  });

  // `produceEncrypted` is a network round-trip and cleanup cannot cancel one already
  // in flight, so a stop landing inside it would otherwise have this continuation
  // register a producer and report sound as shared for a share that no longer exists --
  // system audio still going out after the user stopped sharing.
  it('discards a producer whose share ended while the produce was in flight', async () => {
    const stream = streamOf([track('audio')]);
    svc.localScreenStream = stream;
    svc.producers.set('screen', { id: 'v1' });
    svc.socket = { emit: vi.fn() };
    const close = vi.fn();
    svc.produceEncrypted = vi.fn().mockImplementation(async () => {
      // The stop lands DURING the await, which is the only window this bug has.
      svc.producers.delete('screen');
      svc.localScreenStream = null;
      return { id: 'late-audio', on: vi.fn(), close };
    });

    await svc.produceScreenAudioFromStream(stream);

    expect(close).toHaveBeenCalled();
    expect(svc.socket.emit).toHaveBeenCalledWith('close-producer', { producerId: 'late-audio' });
    expect(svc.producers.has('screen-audio')).toBe(false);
    expect(useVoiceStore.getState().isScreenAudioOn).toBe(false);
  });
});
