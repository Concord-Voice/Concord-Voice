import React from 'react';
import { act } from 'react';
import { render, screen } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// ── Child component mocks ──────────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/ParticipantTile', () => ({
  default: ({ participant }: { participant: { userId: string; username: string } }) => (
    <div data-testid={`participant-tile-${participant.userId}`}>{participant.username}</div>
  ),
}));

const mockVoiceMagnification = vi.hoisted(() => ({
  scales: {} as Record<string, number>,
}));

vi.mock('@/renderer/components/Voice/useVoiceMagnification', () => ({
  VOICE_MAX_SCALE: 1.12,
  useVoiceMagnification: () => mockVoiceMagnification.scales,
}));

const mockUseGridLayout = vi.fn(() => ({ tileWidth: 300, tileHeight: 169, columns: 3 }));
vi.mock('@/renderer/hooks/useGridLayout', () => ({
  useGridLayout: (...args: unknown[]) => mockUseGridLayout(...args),
}));

vi.mock('@/renderer/components/Voice/ParticipantGrid.css', () => ({}));

// ── AudioContext mock objects ─────────────────────────────────────────────────
// Defined at module scope so individual test assertions can reference them,
// but the globals are only installed / removed inside beforeAll / afterAll
// to avoid leaking into other test files.

const mockClose = vi.fn().mockResolvedValue(undefined);
const mockResume = vi.fn().mockResolvedValue(undefined);
const mockConnect = vi.fn();
const mockDisconnect = vi.fn();

const mockAnalyserNode = {
  fftSize: 256,
  smoothingTimeConstant: 0.3,
  frequencyBinCount: 128,
  connect: mockConnect,
  getByteFrequencyData: vi.fn(),
};
const mockGainNode = {
  gain: { value: 1, setTargetAtTime: vi.fn() },
  connect: mockConnect,
  disconnect: mockDisconnect,
};
const mockSourceNode = { connect: mockConnect };
const mockAudioContext = {
  state: 'running',
  currentTime: 0,
  destination: {},
  sampleRate: 48000,
  createAnalyser: vi.fn(() => mockAnalyserNode),
  createGain: vi.fn(() => ({ ...mockGainNode })),
  createMediaElementSource: vi.fn(() => mockSourceNode),
  resume: mockResume,
  close: mockClose,
  setSinkId: vi.fn().mockResolvedValue(undefined),
};

// Install / restore globals in beforeAll/afterAll so they don't leak into
// other test files when Vitest runs suites concurrently.
beforeAll(() => {
  // jsdom doesn't implement HTMLAudioElement.play / pause
  vi.spyOn(HTMLAudioElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLAudioElement.prototype, 'pause').mockImplementation(() => {});

  // Regular function (not arrow) so `new AudioContext(...)` works.
  // A constructor that returns an object uses that returned object.
  vi.stubGlobal(
    'AudioContext',
    vi.fn(function MockAudioContextCtor() {
      return mockAudioContext;
    })
  );
});

afterAll(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

import {
  AudioOutputs,
  AudioOutput,
  UserFrameGrid,
} from '@/renderer/components/Voice/ParticipantGrid';

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeMockStream(id = 'stream-1'): MediaStream {
  return { id, active: true, getAudioTracks: () => [] } as unknown as MediaStream;
}

describe('AudioOutputs', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useVoiceStore.setState({ participants: {}, audioOutputDeviceId: null });
    useUserStore.setState({
      user: {
        id: 'local-user',
        username: 'me',
        email: '',
        display_name: 'Me',
        bio: null,
        avatar_url: null,
        header_image_url: null,
        links: [],
        email_verified: false,
        age_verified: true,
        created_at: '',
        updated_at: '',
      },
    });
  });

  it('renders nothing when there are no participants', () => {
    useVoiceStore.setState({ participants: {} });
    const { container } = render(<AudioOutputs />);
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing when the only participant is the local user', () => {
    useVoiceStore.setState({
      participants: {
        'local-user': {
          userId: 'local-user',
          username: 'me',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream(),
        },
      },
    });
    const { container } = render(<AudioOutputs />);
    expect(container.firstChild).toBeNull();
  });

  it('instantiates an AudioOutput for a remote participant with audioStream', () => {
    useVoiceStore.setState({
      participants: {
        'remote-1': {
          userId: 'remote-1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream('audio-stream-1'),
        },
      },
    });
    render(<AudioOutputs />);
    expect(mockAudioContext.createMediaElementSource).toHaveBeenCalledTimes(1);
  });

  it('instantiates an AudioOutput for a remote participant with screenAudioStream', () => {
    useVoiceStore.setState({
      participants: {
        'remote-1': {
          userId: 'remote-1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          screenAudioStream: makeMockStream('screen-audio-stream-1'),
        },
      },
    });
    render(<AudioOutputs />);
    expect(mockAudioContext.createMediaElementSource).toHaveBeenCalledTimes(1);
  });

  it('instantiates separate AudioOutputs for audioStream and screenAudioStream', () => {
    useVoiceStore.setState({
      participants: {
        'remote-1': {
          userId: 'remote-1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream('audio-stream-1'),
          screenAudioStream: makeMockStream('screen-audio-stream-1'),
        },
      },
    });
    render(<AudioOutputs />);
    expect(mockAudioContext.createMediaElementSource).toHaveBeenCalledTimes(2);
  });

  it('instantiates AudioOutputs for multiple remote participants', () => {
    useVoiceStore.setState({
      participants: {
        'remote-1': {
          userId: 'remote-1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream('a1'),
        },
        'remote-2': {
          userId: 'remote-2',
          username: 'bob',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream('a2'),
        },
      },
    });
    render(<AudioOutputs />);
    expect(mockAudioContext.createMediaElementSource).toHaveBeenCalledTimes(2);
  });

  it('excludes local user from audio outputs even if they have a stream', () => {
    useVoiceStore.setState({
      participants: {
        'local-user': {
          userId: 'local-user',
          username: 'me',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream('local-audio'),
        },
        'remote-1': {
          userId: 'remote-1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
          audioStream: makeMockStream('remote-audio'),
        },
      },
    });
    render(<AudioOutputs />);
    // Only remote-1 gets an AudioOutput, not the local user.
    expect(mockAudioContext.createMediaElementSource).toHaveBeenCalledTimes(1);
  });
});

describe('AudioOutput', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAudioSettingsStore.setState({
      outputVolume: 100,
      quietBoost: false,
      quietBoostThreshold: -30,
    });
  });

  it('renders no DOM output — audio element lives in the effect closure', () => {
    const stream = makeMockStream();
    const { container } = render(<AudioOutput stream={stream} />);
    expect(container.querySelector('audio')).toBeNull();
    expect(container.firstChild).toBeNull();
  });

  it('sets up AudioContext on mount', () => {
    const stream = makeMockStream();
    render(<AudioOutput stream={stream} />);
    expect(AudioContext).toHaveBeenCalledWith({ sampleRate: 48000 });
  });

  it('creates audio processing chain nodes', () => {
    const stream = makeMockStream();
    render(<AudioOutput stream={stream} />);
    expect(mockAudioContext.createAnalyser).toHaveBeenCalled();
    expect(mockAudioContext.createGain).toHaveBeenCalled();
    expect(mockAudioContext.createMediaElementSource).toHaveBeenCalled();
  });

  it('cleans up AudioContext on unmount', () => {
    const stream = makeMockStream();
    const { unmount } = render(<AudioOutput stream={stream} />);
    unmount();
    expect(mockClose).toHaveBeenCalled();
  });

  it('pauses the fallback <audio> element on unmount when output-device routing falls back', () => {
    // Force the output-device fallback branch: AudioContext WITHOUT setSinkId,
    // but the per-element <audio> WITH setSinkId. Save originals to restore.
    const ctxOrig: { setSinkId?: unknown } = mockAudioContext as { setSinkId?: unknown };
    const origCtxSetSinkId = ctxOrig.setSinkId;
    delete ctxOrig.setSinkId;
    const fallbackDestStream = {} as MediaStream;
    const origCreateMediaStreamDestination = (
      mockAudioContext as { createMediaStreamDestination?: unknown }
    ).createMediaStreamDestination;
    (
      mockAudioContext as { createMediaStreamDestination: () => unknown }
    ).createMediaStreamDestination = vi.fn(() => ({ stream: fallbackDestStream }));

    // Spy on document.createElement so we can capture the fallback <audio> element.
    // The component creates two audio elements: the primary `el` and the fallback.
    const audioElements: HTMLAudioElement[] = [];
    const origCreate = document.createElement.bind(document);
    const createSpy = vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      const el = origCreate(tag);
      if (tag === 'audio') {
        // Make the element eligible for the el-based fallback (has setSinkId).
        Object.assign(el, { setSinkId: vi.fn().mockResolvedValue(undefined) });
        audioElements.push(el as HTMLAudioElement);
      }
      return el;
    });

    const stream = makeMockStream();
    const { unmount } = render(<AudioOutput stream={stream} outputDeviceId="device-fallback" />);

    // The fallback element is created after the primary el and the fallback path runs.
    expect(audioElements.length).toBeGreaterThanOrEqual(2);
    const fallbackEl = audioElements[audioElements.length - 1];
    const pauseSpy = vi.spyOn(fallbackEl, 'pause');

    unmount();

    expect(pauseSpy).toHaveBeenCalled();

    // Restore mocks/state so subsequent tests aren't affected.
    createSpy.mockRestore();
    if (origCtxSetSinkId !== undefined) {
      (mockAudioContext as { setSinkId: unknown }).setSinkId = origCtxSetSinkId;
    }
    if (origCreateMediaStreamDestination !== undefined) {
      (mockAudioContext as { createMediaStreamDestination: unknown }).createMediaStreamDestination =
        origCreateMediaStreamDestination;
    } else {
      delete (mockAudioContext as { createMediaStreamDestination?: unknown })
        .createMediaStreamDestination;
    }
  });

  // Per-participant volume — volumeGain should apply master × perParticipant.
  //
  // Note on the mock: `mockGainNode.gain` is shared across all gain node
  // instances (spread-copy preserves the inner reference). This means the
  // initial-value assertions here reflect the LAST assignment made during
  // AudioOutput setup, which is the final `volumeGain.gain.value` write.
  describe('per-participant volume', () => {
    it('initializes volumeGain to master × per-participant volume', () => {
      useAudioSettingsStore.setState({
        outputVolume: 80,
        perParticipantVolume: { 'user-1': 50 },
      });
      const stream = makeMockStream();
      render(<AudioOutput stream={stream} userId="user-1" />);
      // 0.8 master × 0.5 per-participant = 0.4
      expect(mockGainNode.gain.value).toBeCloseTo(0.4, 5);
    });

    it('falls back to master volume only when userId is omitted', () => {
      useAudioSettingsStore.setState({
        outputVolume: 60,
        perParticipantVolume: { 'user-1': 50 },
      });
      const stream = makeMockStream();
      render(<AudioOutput stream={stream} />);
      expect(mockGainNode.gain.value).toBeCloseTo(0.6, 5);
    });

    it('falls back to master × 100% when no per-participant override exists', () => {
      useAudioSettingsStore.setState({
        outputVolume: 120,
        perParticipantVolume: {},
      });
      const stream = makeMockStream();
      render(<AudioOutput stream={stream} userId="user-1" />);
      expect(mockGainNode.gain.value).toBeCloseTo(1.2, 5);
    });

    it('updates volumeGain via setTargetAtTime when per-participant volume changes', () => {
      useAudioSettingsStore.setState({
        outputVolume: 100,
        perParticipantVolume: { 'user-1': 100 },
      });
      const stream = makeMockStream();
      render(<AudioOutput stream={stream} userId="user-1" />);

      // Clear prior calls from initial render
      mockGainNode.gain.setTargetAtTime.mockClear();

      // Change the participant volume — triggers the reactive effect.
      // Wrap in act() so React flushes the effect after the external update.
      act(() => {
        useAudioSettingsStore.setState({
          perParticipantVolume: { 'user-1': 50 },
        });
      });

      // The effect should call setTargetAtTime with the new combined value (0.5).
      const combinedCalls = mockGainNode.gain.setTargetAtTime.mock.calls.filter(
        (call: unknown[]) => Math.abs((call[0] as number) - 0.5) < 1e-5
      );
      expect(combinedCalls.length).toBeGreaterThanOrEqual(1);
    });
  });

  it('logs redacted warning when HTMLAudioElement.play() rejects', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(HTMLAudioElement.prototype, 'play').mockRejectedValueOnce(new Error('boom'));

    const stream = makeMockStream();
    render(<AudioOutput stream={stream} />);

    await vi.waitFor(() => {
      expect(warnSpy).toHaveBeenCalledWith('Audio element play() rejected:', 'boom');
    });
  });

  it('logs redacted warning when AudioContext.resume() rejects on suspended state', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    // Flip mock ctx to suspended so the resume() branch executes
    const origState = mockAudioContext.state;
    mockAudioContext.state = 'suspended';
    mockAudioContext.resume = vi.fn().mockRejectedValueOnce(new Error('boom'));

    const stream = makeMockStream();
    render(<AudioOutput stream={stream} />);

    await vi.waitFor(() => {
      expect(warnSpy).toHaveBeenCalledWith('AudioContext resume failed:', 'boom');
    });

    mockAudioContext.state = origState;
  });
});

describe('UserFrameGrid', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockUseGridLayout.mockReturnValue({ tileWidth: 300, tileHeight: 169, columns: 3 });
    mockVoiceMagnification.scales = {};
    useVoiceStore.setState({ participants: {} });
    useUserStore.setState({
      user: {
        id: 'local-user',
        username: 'me',
        email: '',
        display_name: 'Me',
        bio: null,
        avatar_url: null,
        header_image_url: null,
        links: [],
        email_verified: false,
        age_verified: true,
        created_at: '',
        updated_at: '',
      },
    });
  });

  it('renders the user frame grid container', () => {
    const { container } = render(<UserFrameGrid />);
    expect(container.querySelector('.user-frame-grid')).toBeInTheDocument();
  });

  it('renders a ParticipantTile for each participant', () => {
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
        u2: {
          userId: 'u2',
          username: 'bob',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    render(<UserFrameGrid />);
    expect(screen.getByTestId('participant-tile-u1')).toBeInTheDocument();
    expect(screen.getByTestId('participant-tile-u2')).toBeInTheDocument();
  });

  it('renders no tiles when participants is empty', () => {
    useVoiceStore.setState({ participants: {} });
    const { container } = render(<UserFrameGrid />);
    expect(container.querySelectorAll('[data-testid^="participant-tile"]').length).toBe(0);
  });

  it('sets --tile-w and --tile-h CSS custom properties on the grid container', () => {
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    const { container } = render(<UserFrameGrid />);
    const grid = container.querySelector('.user-frame-grid') as HTMLElement;
    expect(grid.style.getPropertyValue('--tile-w')).toBe('300px');
    expect(grid.style.getPropertyValue('--tile-h')).toBe('169px');
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-w'))).toBeCloseTo(336, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-h'))).toBeCloseTo(189.28, 3);

    const lastCall = mockUseGridLayout.mock.calls[mockUseGridLayout.mock.calls.length - 1];
    expect(lastCall[2]).toEqual(expect.objectContaining({ scale: 1.12 }));
  });

  it('reserves active-speaker scale inside stable tile slots', () => {
    mockUseGridLayout.mockReturnValue({ tileWidth: 200, tileHeight: 200, columns: 2 });
    mockVoiceMagnification.scales = { u1: 1.12, u2: 1.12 };
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: true,
        },
        u2: {
          userId: 'u2',
          username: 'bob',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: true,
        },
      },
    });
    const { container } = render(<UserFrameGrid />);
    const grid = container.querySelector('.user-frame-grid') as HTMLElement;

    expect(container.querySelectorAll('.user-frame-grid__slot')).toHaveLength(2);
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-w'))).toBeCloseTo(224, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-h'))).toBeCloseTo(224, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-w'))).toBeCloseTo(200, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-h'))).toBeCloseTo(200, 3);

    const lastCall = mockUseGridLayout.mock.calls[mockUseGridLayout.mock.calls.length - 1];
    expect(lastCall[2]).toEqual(expect.objectContaining({ scale: 1.12 }));
  });

  it('reserves full speaking scale before the animation hook catches up', () => {
    mockUseGridLayout.mockReturnValue({ tileWidth: 200, tileHeight: 200, columns: 2 });
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: true,
        },
      },
    });
    const { container } = render(<UserFrameGrid />);
    const grid = container.querySelector('.user-frame-grid') as HTMLElement;

    expect(parseFloat(grid.style.getPropertyValue('--tile-w'))).toBeCloseTo(200, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-h'))).toBeCloseTo(200, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-w'))).toBeCloseTo(224, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-h'))).toBeCloseTo(224, 3);
  });

  it('keeps reserved slots at max scale during release animation', () => {
    mockUseGridLayout.mockReturnValue({ tileWidth: 200, tileHeight: 200, columns: 2 });
    mockVoiceMagnification.scales = { u1: 1.04 };
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    const { container } = render(<UserFrameGrid />);
    const grid = container.querySelector('.user-frame-grid') as HTMLElement;

    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-w'))).toBeCloseTo(224, 3);
    expect(parseFloat(grid.style.getPropertyValue('--tile-slot-h'))).toBeCloseTo(224, 3);

    const lastCall = mockUseGridLayout.mock.calls[mockUseGridLayout.mock.calls.length - 1];
    expect(lastCall[2]).toEqual(expect.objectContaining({ scale: 1.12 }));
  });

  it('attaches a ref to the grid container element', () => {
    const { container } = render(<UserFrameGrid />);
    const grid = container.querySelector('.user-frame-grid');
    expect(grid).toBeInstanceOf(HTMLDivElement);
  });

  it('passes square aspect ratio when no participant has video', () => {
    mockUseGridLayout.mockReturnValue({ tileWidth: 200, tileHeight: 200, columns: 2 });
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
        u2: {
          userId: 'u2',
          username: 'bob',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    const { container } = render(<UserFrameGrid />);

    // Verify useGridLayout was called with aspectRatio: 1 (square)
    const lastCall = mockUseGridLayout.mock.calls[mockUseGridLayout.mock.calls.length - 1];
    expect(lastCall[2]).toEqual(expect.objectContaining({ aspectRatio: 1 }));

    // Verify CSS variables reflect square tiles
    const grid = container.querySelector('.user-frame-grid') as HTMLElement;
    expect(grid.style.getPropertyValue('--tile-w')).toBe('200px');
    expect(grid.style.getPropertyValue('--tile-h')).toBe('200px');
  });

  it('passes 16:9 aspect ratio when any participant has video', () => {
    mockUseGridLayout.mockReturnValue({ tileWidth: 300, tileHeight: 169, columns: 2 });
    useVoiceStore.setState({
      participants: {
        u1: {
          userId: 'u1',
          username: 'alice',
          isMuted: false,
          isDeafened: false,
          isVideoOn: true,
          isScreenSharing: false,
          isSpeaking: false,
        },
        u2: {
          userId: 'u2',
          username: 'bob',
          isMuted: false,
          isDeafened: false,
          isVideoOn: false,
          isScreenSharing: false,
          isSpeaking: false,
        },
      },
    });
    render(<UserFrameGrid />);

    // Verify useGridLayout was called with aspectRatio: 16/9
    const lastCall = mockUseGridLayout.mock.calls[mockUseGridLayout.mock.calls.length - 1];
    expect(lastCall[2]).toEqual(expect.objectContaining({ aspectRatio: 16 / 9 }));
  });
});
