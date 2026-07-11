/**
 * #1924 Fix #7 — equal-layout single-screen focus role.
 *
 * Screen simulcast consumers seed at spatial layer 0 and ramp UP on reported render-size
 * demand (set-preferred-layers). The 'grid' role caps demand at layer 1, which is right
 * for multiple equal-weight cells but wrong when a SINGLE share fills the equal-layout
 * stage — that lone cell should demand the 'focus' layer. VoiceStage's equal branch now
 * passes renderRole={hasMultiple ? 'grid' : 'focus'}; this locks that mapping.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, waitFor, cleanup } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voiceStore';

// VoiceStage → StageVideo → ScreenShareAudioControls statically imports voiceService, so
// the mock factory runs during the import chain (before module-body consts initialize) —
// vi.hoisted lifts the mock fns above that so the factory can reference them.
const { setRemoteVideoRenderState, removeRemoteVideoTile } = vi.hoisted(() => ({
  setRemoteVideoRenderState: vi.fn(),
  removeRemoteVideoTile: vi.fn(),
}));
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { setRemoteVideoRenderState, removeRemoteVideoTile },
}));

vi.mock('@/renderer/components/Voice/VoiceStage.css', () => ({}));

// Capture EVERY IntersectionObserver callback (one per StageVideo cell) so the test can
// drive intersection for all cells — a single shared handle would only hold the last one.
let ioCallbacks: Array<(entries: Array<{ isIntersecting: boolean }>) => void> = [];
class CaptureIO {
  constructor(cb: (e: Array<{ isIntersecting: boolean }>) => void) {
    ioCallbacks.push(cb);
  }
  observe = vi.fn();
  unobserve = vi.fn();
  disconnect = vi.fn();
}
/** Drive an "intersecting" tick for all captured observers. */
const fireIntersect = () => ioCallbacks.forEach((cb) => cb([{ isIntersecting: true }]));

// jsdom does not provide MediaStream.
class MockMediaStream {
  id = 'mock-stream';
  active = true;
  getTracks() {
    return [];
  }
  getAudioTracks() {
    return [];
  }
  getVideoTracks() {
    return [];
  }
}
globalThis.MediaStream = MockMediaStream as unknown as typeof MediaStream;

import VoiceStage from '@/renderer/components/Voice/VoiceStage';

const mockParticipant = (overrides: Partial<VoiceParticipant> = {}): VoiceParticipant =>
  ({
    userId: 'user-1',
    username: 'alice',
    displayName: 'Alice',
    isMuted: false,
    isDeafened: false,
    isVideoOn: false,
    isScreenSharing: true,
    isSpeaking: false,
    screenStream: new MockMediaStream() as unknown as MediaStream,
    ...overrides,
  }) as VoiceParticipant;

describe('VoiceStage equal-layout render role (#1924 Fix #7)', () => {
  let originalIO: typeof IntersectionObserver;
  let gbcrSpy: ReturnType<typeof vi.spyOn> | null = null;

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    ioCallbacks = [];
    originalIO = window.IntersectionObserver;
    (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver =
      CaptureIO as unknown as typeof IntersectionObserver;
    HTMLVideoElement.prototype.play = vi.fn().mockResolvedValue(undefined);
    gbcrSpy = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      width: 1280,
      height: 720,
      top: 0,
      right: 1280,
      bottom: 720,
      left: 0,
      toJSON: () => ({}),
    } as DOMRect);
  });

  afterEach(() => {
    cleanup();
    window.IntersectionObserver = originalIO;
    gbcrSpy?.mockRestore();
    gbcrSpy = null;
  });

  it("a SINGLE equal-layout remote share reports role 'focus'", async () => {
    useVoiceStore.setState({
      stageLayout: 'equal',
      tunedInScreenShares: { p1: 'c1' },
      participants: { 'user-1': mockParticipant() },
      localStreamPaused: false,
    });
    useVoiceStore.getState().registerActiveScreenShare({
      producerId: 'p1',
      userId: 'user-1',
      username: 'alice',
      displayName: 'Alice',
      isLocal: false,
    });

    render(<VoiceStage />);
    // Drive intersection on every poll so the report fires once the lazy voiceService
    // import has resolved (svc set), independent of import timing.
    await waitFor(() => {
      fireIntersect();
      expect(setRemoteVideoRenderState).toHaveBeenCalledWith(
        'user-1',
        expect.any(String),
        expect.objectContaining({ visible: true, role: 'focus' }),
        'screen'
      );
    });
    // The single-cell demand must never be reported as 'grid' (which caps at layer 1).
    for (const call of setRemoteVideoRenderState.mock.calls) {
      expect(call[2].role).not.toBe('grid');
    }
  });

  it("MULTIPLE equal-layout remote shares report role 'grid'", async () => {
    useVoiceStore.setState({
      stageLayout: 'equal',
      tunedInScreenShares: { pa: 'ca', pb: 'cb' },
      participants: {
        'user-1': mockParticipant(),
        'user-2': mockParticipant({ userId: 'user-2', username: 'bob', displayName: 'Bob' }),
      },
      localStreamPaused: false,
    });
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare({
      producerId: 'pa',
      userId: 'user-1',
      username: 'alice',
      displayName: 'Alice',
      isLocal: false,
    });
    store.registerActiveScreenShare({
      producerId: 'pb',
      userId: 'user-2',
      username: 'bob',
      displayName: 'Bob',
      isLocal: false,
    });

    render(<VoiceStage />);
    await waitFor(() => {
      fireIntersect();
      expect(setRemoteVideoRenderState).toHaveBeenCalledWith(
        expect.any(String),
        expect.any(String),
        expect.objectContaining({ visible: true, role: 'grid' }),
        'screen'
      );
    });
    // With more than one equal-weight cell, none should demand the 'focus' layer.
    for (const call of setRemoteVideoRenderState.mock.calls) {
      expect(call[2].role).not.toBe('focus');
    }
  });
});
