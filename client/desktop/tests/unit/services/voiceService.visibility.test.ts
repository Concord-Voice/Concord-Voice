import { describe, it, expect, vi, beforeEach } from 'vitest';
import { voiceService } from '../../../src/renderer/services/voiceService';

/** Reach into the singleton to seed a fake camera consumer + meta and a socket spy. */
function seedCameraConsumer(svc: any, consumerId: string, userId: string) {
  const consumer = {
    id: consumerId,
    kind: 'video' as const,
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
    source: 'camera',
    producerUserId: userId,
    producerId: 'p-' + consumerId,
  });
  return consumer;
}

describe('voiceService visibility-pause (#1541)', () => {
  let svc: any;
  let emit: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    svc = voiceService as any;
    svc.consumers.clear();
    svc.consumerMeta.clear();
    svc.pauseCoordinator.reset();
    svc.tileVisibilityByUser.clear();
    svc.documentHidden = false;
    emit = vi.fn();
    svc.socket = { emit };
  });

  it('hiding a tile pauses its camera consumer locally and on the SFU', () => {
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.setRemoteVideoVisibility('user-A', false, 'tile-1');
    expect(c.pause).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'cam-1' });
  });

  it('re-showing a hidden tile resumes it', () => {
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.setRemoteVideoVisibility('user-A', false, 'tile-1');
    emit.mockClear();
    svc.setRemoteVideoVisibility('user-A', true, 'tile-1');
    expect(c.resume).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith('resume-consumer', { consumerId: 'cam-1' });
  });

  it('stays running while ANY tile is visible (multi-tile per user)', () => {
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.setRemoteVideoVisibility('user-A', true, 'grid'); // visible in grid
    svc.setRemoteVideoVisibility('user-A', false, 'bar'); // hidden in bar
    expect(c.pause).not.toHaveBeenCalled(); // still visible somewhere
    // now hide the grid tile too -> every tile hidden -> pause
    svc.setRemoteVideoVisibility('user-A', false, 'grid');
    expect(c.pause).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'cam-1' });
  });

  it('removeRemoteVideoTile deregisters a tile without freezing other visible tiles', () => {
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.setRemoteVideoVisibility('user-A', true, 'grid');
    svc.setRemoteVideoVisibility('user-A', false, 'pip'); // a PiP frame, off-screen
    expect(c.pause).not.toHaveBeenCalled();
    // the PiP frame unmounts: deregister (NOT report-hidden) — grid still visible
    svc.removeRemoteVideoTile('user-A', 'pip');
    expect(c.pause).not.toHaveBeenCalled();
    // now the grid tile is the only one, and it is visible -> still running
    expect(svc.tileVisibilityByUser.get('user-A').size).toBe(1);
  });

  it('does NOT pause a consumer before any tile has reported (default visible)', () => {
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.applyInitialVisibilityReason('cam-1', 'user-A'); // no tile reported yet
    expect(c.pause).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalledWith('pause-consumer', { consumerId: 'cam-1' });
  });

  it('a tile reported hidden BEFORE its consumer exists is paused when the consumer is routed', () => {
    svc.setRemoteVideoVisibility('user-A', false, 'tile-1'); // hidden, no consumer yet
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.applyInitialVisibilityReason('cam-1', 'user-A');
    expect(c.pause).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'cam-1' });
  });

  it('document.hidden pauses all remote video consumers; restore resumes them', () => {
    const c = seedCameraConsumer(svc, 'cam-1', 'user-A');
    svc.handleDocumentVisibilityChange(true);
    expect(c.pause).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'cam-1' });
    emit.mockClear();
    svc.handleDocumentVisibilityChange(false);
    expect(c.resume).toHaveBeenCalledTimes(1);
  });

  it('visibility never touches audio (audio consumers get no reason)', () => {
    const audio = { id: 'aud-1', kind: 'audio' as const, pause: vi.fn(), resume: vi.fn() };
    svc.consumers.set('aud-1', audio);
    svc.consumerMeta.set('aud-1', { source: 'mic', producerUserId: 'user-A', producerId: 'pa' });
    svc.handleDocumentVisibilityChange(true);
    expect(audio.pause).not.toHaveBeenCalled();
  });
});
