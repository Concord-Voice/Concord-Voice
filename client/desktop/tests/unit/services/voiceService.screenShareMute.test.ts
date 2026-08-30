import { describe, it, expect, vi, beforeEach } from 'vitest';
import { voiceService } from '../../../src/renderer/services/voiceService';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

// Screenshare mute rides the ConsumerPauseCoordinator's 'stream-mute' reason
// (scope 'both'): pauseServerForwarding → socket.emit('pause-consumer') AND
// pauseLocalDecode → consumer.pause(). See voiceService.visibility.test.ts for
// the same singleton-seeding pattern (#1541).

vi.mock('../../../src/renderer/services/notificationSoundService', () => ({
  notificationSoundService: { play: vi.fn() },
}));

function seedScreenAudioConsumer(svc: any, consumerId: string, sharerUserId: string) {
  const consumer = {
    id: consumerId,
    kind: 'audio' as const,
    _paused: false,
    get paused() {
      return this._paused;
    },
    pause: vi.fn().mockImplementation(function (this: any) {
      this._paused = true;
    }),
    resume: vi.fn().mockImplementation(function (this: any) {
      this._paused = false;
    }),
  };
  svc.consumers.set(consumerId, consumer);
  svc.consumerMeta.set(consumerId, {
    source: 'screen-audio',
    producerUserId: sharerUserId,
    producerId: 'p-' + consumerId,
  });
  return consumer;
}

describe('voiceService screenshare mute (#2162)', () => {
  let svc: any;
  let emit: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    svc = voiceService as any;
    svc.consumers.clear();
    svc.consumerMeta.clear();
    svc.pauseCoordinator.reset();
    // The service is a leaked singleton — reset visibility state the camera-dispatch
    // path reads, so applyInitialVisibilityReason is a deterministic no-op here.
    svc.tileVisibilityByUser?.clear();
    svc.documentHidden = false;
    useVoiceStore.getState().reset();
    useUserStore.setState({ user: null });
    emit = vi.fn();
    svc.socket = { emit };
  });

  /** Register the local viewer as (self- or server-) deafened. */
  function setLocalDeafen(opts: { self?: boolean; server?: boolean }): void {
    if (opts.self) useVoiceStore.getState().setDeafened(true);
    if (opts.server) {
      useUserStore.setState({ user: { id: 'me', username: 'me' } } as never);
      const local: VoiceParticipant = {
        userId: 'me',
        username: 'me',
        isMuted: false,
        isDeafened: false,
        serverMuted: true,
        serverDeafened: true,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      };
      useVoiceStore.setState({ participants: { me: local } });
    }
  }

  it('mute records intent, pauses the server-side consumer, and pauses local decode', () => {
    const c = seedScreenAudioConsumer(svc, 'sa1', 'sharer-1');
    voiceService.muteScreenShare('sharer-1');
    expect(useVoiceStore.getState().screenShareMuted['sharer-1']).toBe(true);
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'sa1' });
    expect(c.pause).toHaveBeenCalledTimes(1);
  });

  it('mute with no live consumer still records intent (re-applied later)', () => {
    voiceService.muteScreenShare('sharer-2');
    expect(useVoiceStore.getState().screenShareMuted['sharer-2']).toBe(true);
    expect(emit).not.toHaveBeenCalled();
  });

  it('unmute clears intent and resumes server forwarding + local decode', () => {
    const c = seedScreenAudioConsumer(svc, 'sa1', 'sharer-1');
    voiceService.muteScreenShare('sharer-1');
    emit.mockClear();
    c.resume.mockClear();
    voiceService.unmuteScreenShare('sharer-1');
    expect(useVoiceStore.getState().screenShareMuted['sharer-1']).toBe(false);
    expect(emit).toHaveBeenCalledWith('resume-consumer', { consumerId: 'sa1' });
    expect(c.resume).toHaveBeenCalledTimes(1);
  });

  it('re-applies mute intent to a freshly-created screen-audio consumer', () => {
    useVoiceStore.getState().setScreenShareMuted('sharer-1', true);
    const c = seedScreenAudioConsumer(svc, 'sa-new', 'sharer-1');
    svc.applyInitialScreenMuteReason('sa-new', 'sharer-1');
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'sa-new' });
    expect(c.pause).toHaveBeenCalledTimes(1);
  });

  it('does NOT apply a mute reason when there is no intent for the sharer', () => {
    seedScreenAudioConsumer(svc, 'sa-new', 'sharer-1');
    svc.applyInitialScreenMuteReason('sa-new', 'sharer-1');
    expect(emit).not.toHaveBeenCalled();
  });

  it('applyInitialConsumerPauseReasons dispatches screen-audio to the mute path', () => {
    useVoiceStore.getState().setScreenShareMuted('sharer-1', true);
    seedScreenAudioConsumer(svc, 'sa-x', 'sharer-1');
    svc.applyInitialConsumerPauseReasons('sa-x', 'screen-audio', 'sharer-1');
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'sa-x' });
  });

  it('applyInitialConsumerPauseReasons routes camera to visibility and ignores other sources', () => {
    // camera + not-hidden → visibility path is a no-op (no emit); 'mic' → no branch.
    svc.applyInitialConsumerPauseReasons('cam-x', 'camera', 'user-A');
    svc.applyInitialConsumerPauseReasons('mic-x', 'mic', 'user-A');
    expect(emit).not.toHaveBeenCalled();
  });

  it('undeafen does NOT locally resume a stream-muted screenshare consumer', () => {
    const c = seedScreenAudioConsumer(svc, 'sa1', 'sharer-1');
    voiceService.muteScreenShare('sharer-1'); // sets stream-mute reason; c.pause called
    useVoiceStore.getState().setDeafened(true); // enter deafened state
    c.resume.mockClear();
    voiceService.toggleDeafen(); // newDeafened=false → undeafen path
    expect(c.resume).not.toHaveBeenCalled(); // guard keeps the muted stream paused
  });

  // deafen → mute → unmute: clearing the mute intent must NOT let the stream
  // play while the viewer is still deafened (deafen is authoritative). #2162
  it('unmute while self-deafened clears intent but keeps local decode paused', () => {
    const c = seedScreenAudioConsumer(svc, 'sa1', 'sharer-1');
    setLocalDeafen({ self: true });
    voiceService.muteScreenShare('sharer-1');
    emit.mockClear();

    voiceService.unmuteScreenShare('sharer-1');

    expect(useVoiceStore.getState().screenShareMuted['sharer-1']).toBe(false); // intent cleared
    expect(c._paused).toBe(true); // deafen re-asserts the local pause → still silent
  });

  it('unmute while server-deafened keeps local decode paused', () => {
    const c = seedScreenAudioConsumer(svc, 'sa1', 'sharer-1');
    setLocalDeafen({ server: true });
    voiceService.muteScreenShare('sharer-1');

    voiceService.unmuteScreenShare('sharer-1');

    expect(useVoiceStore.getState().screenShareMuted['sharer-1']).toBe(false);
    expect(c._paused).toBe(true);
  });

  it('unmute while NOT deafened resumes local decode (stream plays)', () => {
    const c = seedScreenAudioConsumer(svc, 'sa1', 'sharer-1');
    voiceService.muteScreenShare('sharer-1');

    voiceService.unmuteScreenShare('sharer-1');

    expect(c._paused).toBe(false); // no deafen → coordinator resume stands
  });

  // #2162: a persisted-muted screen-audio consumer recreated on reconnect must
  // stay server-paused. consume() checks startsPausedByScreenMute to SKIP the
  // unconditional resume, so the coordinator's re-applied 'stream-mute' pause is
  // never preceded by a resume → no resume→pause round trip (no audio/bandwidth
  // blip for a stream the viewer explicitly muted).
  describe('skips the consume-time resume for a persisted-muted screen-audio consumer', () => {
    it('startsPausedByScreenMute is true only for screen-audio with a persisted mute intent', () => {
      useVoiceStore.getState().setScreenShareMuted('sharer-1', true);
      expect(svc.startsPausedByScreenMute('screen-audio', 'sharer-1')).toBe(true);
      // No intent for a different sharer, wrong source, or no intent at all → resume normally.
      expect(svc.startsPausedByScreenMute('screen-audio', 'sharer-2')).toBe(false);
      expect(svc.startsPausedByScreenMute('camera', 'sharer-1')).toBe(false);
      expect(svc.startsPausedByScreenMute('mic', 'sharer-1')).toBe(false);
    });

    it('re-applying the mute reason emits pause-consumer and never resume-consumer', () => {
      // Reconnect: persisted mute intent + a freshly-minted consumerId.
      useVoiceStore.getState().setScreenShareMuted('sharer-1', true);
      const c = seedScreenAudioConsumer(svc, 'sa-reconnect', 'sharer-1');

      // consume() skips the resume when startsPausedByScreenMute is true, then
      // re-applies the pause reason. Exercise that second step directly.
      expect(svc.startsPausedByScreenMute('screen-audio', 'sharer-1')).toBe(true);
      svc.applyInitialConsumerPauseReasons('sa-reconnect', 'screen-audio', 'sharer-1');

      expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'sa-reconnect' });
      expect(emit).not.toHaveBeenCalledWith('resume-consumer', { consumerId: 'sa-reconnect' });
      expect(c._paused).toBe(true); // local decode paused too (no resume→pause bounce)
    });
  });
});
