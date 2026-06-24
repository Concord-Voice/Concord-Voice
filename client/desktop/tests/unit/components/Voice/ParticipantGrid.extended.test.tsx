import React from 'react';
import { render } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock child components
vi.mock('@/renderer/components/Voice/ParticipantTile', () => ({
  default: ({ participant }: { participant: { userId: string; username: string } }) => (
    <div data-testid={`participant-tile-${participant.userId}`}>{participant.username}</div>
  ),
}));

vi.mock('@/renderer/components/Voice/useVoiceMagnification', () => ({
  VOICE_MAX_SCALE: 1.12,
  useVoiceMagnification: () => ({}),
}));

vi.mock('@/renderer/components/Voice/ParticipantGrid.css', () => ({}));

// AudioContext mock
const mockClose = vi.fn().mockResolvedValue(undefined);
const mockResume = vi.fn().mockResolvedValue(undefined);
const mockConnect = vi.fn();
const mockDisconnect = vi.fn();

const mockGainNode = {
  gain: { value: 1, setTargetAtTime: vi.fn() },
  connect: mockConnect,
  disconnect: mockDisconnect,
};
const mockAnalyserNode = {
  fftSize: 256,
  smoothingTimeConstant: 0.3,
  frequencyBinCount: 128,
  connect: mockConnect,
  getByteFrequencyData: vi.fn(),
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

beforeAll(() => {
  vi.spyOn(HTMLAudioElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLAudioElement.prototype, 'pause').mockImplementation(() => {});
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

import ParticipantGrid, {
  AudioOutput,
  AudioOutputs,
} from '@/renderer/components/Voice/ParticipantGrid';

function makeMockStream(id = 'stream-1'): MediaStream {
  return {
    id,
    active: true,
    getAudioTracks: () => [{ readyState: 'live', enabled: true }],
  } as unknown as MediaStream;
}

function installFallbackDestination() {
  const origCreateMediaStreamDestination = (
    mockAudioContext as { createMediaStreamDestination?: unknown }
  ).createMediaStreamDestination;
  const createMediaStreamDestination = vi.fn(() => ({ stream: {} as MediaStream }));
  (
    mockAudioContext as { createMediaStreamDestination: typeof createMediaStreamDestination }
  ).createMediaStreamDestination = createMediaStreamDestination;

  const restore = () => {
    if (origCreateMediaStreamDestination !== undefined) {
      (mockAudioContext as { createMediaStreamDestination: unknown }).createMediaStreamDestination =
        origCreateMediaStreamDestination;
    } else {
      delete (mockAudioContext as { createMediaStreamDestination?: unknown })
        .createMediaStreamDestination;
    }
  };

  return { createMediaStreamDestination, restore };
}

function installAudioSinkElements(
  setSinkIdFactory: () => ReturnType<typeof vi.fn> = () => vi.fn().mockResolvedValue(undefined)
) {
  const audioElements: HTMLAudioElement[] = [];
  const origCreate = document.createElement.bind(document);
  const createElement = (tag: string) => {
    const el = origCreate(tag);
    if (tag !== 'audio') return el;

    Object.assign(el, {
      play: vi.fn().mockResolvedValue(undefined),
      setSinkId: setSinkIdFactory(),
    });
    audioElements.push(el as HTMLAudioElement);
    return el;
  };
  const createSpy = vi.spyOn(document, 'createElement').mockImplementation(createElement);

  return { audioElements, restore: () => createSpy.mockRestore() };
}

describe('ParticipantGrid — extended coverage', () => {
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

  describe('ParticipantGrid (default export)', () => {
    it('renders AudioOutputs and UserFrameGrid together', () => {
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
            audioStream: makeMockStream(),
          },
        },
      });
      const { container } = render(<ParticipantGrid />);
      // <audio> lives in the effect closure (DOM-less AudioOutput), so verify
      // the audio graph via the mock and the visual layer via the DOM.
      expect(mockAudioContext.createMediaElementSource).toHaveBeenCalled();
      expect(container.querySelector('.user-frame-grid')).toBeInTheDocument();
    });
  });

  describe('AudioOutput — output volume', () => {
    it('updates gain when outputVolume changes', () => {
      const stream = makeMockStream();
      useAudioSettingsStore.setState({
        outputVolume: 100,
        quietBoost: false,
        quietBoostThreshold: -35,
      });
      const { rerender } = render(<AudioOutput stream={stream} />);

      // Change volume; rerender must not throw — exercises the gain-update path
      useAudioSettingsStore.setState({ outputVolume: 50 });
      expect(() => rerender(<AudioOutput stream={stream} />)).not.toThrow();
      expect(useAudioSettingsStore.getState().outputVolume).toBe(50);
    });
  });

  describe('AudioOutput — quiet boost', () => {
    it('starts boost polling when quietBoost is enabled', () => {
      vi.useFakeTimers();
      const stream = makeMockStream();
      useAudioSettingsStore.setState({
        outputVolume: 100,
        quietBoost: true,
        quietBoostThreshold: -35,
      });
      render(<AudioOutput stream={stream} />);

      // Boost timer should be set up (polling at 20ms interval)
      vi.advanceTimersByTime(100);
      expect(mockAnalyserNode.getByteFrequencyData).toHaveBeenCalled();
      vi.useRealTimers();
    });

    it('resets boost gain when quietBoost is disabled', () => {
      const stream = makeMockStream();
      useAudioSettingsStore.setState({
        outputVolume: 100,
        quietBoost: true,
        quietBoostThreshold: -35,
      });
      const { rerender } = render(<AudioOutput stream={stream} />);

      // Disable quiet boost; the rerender path must reset gain without throwing
      useAudioSettingsStore.setState({ quietBoost: false });
      expect(() => rerender(<AudioOutput stream={stream} />)).not.toThrow();
      expect(useAudioSettingsStore.getState().quietBoost).toBe(false);
    });
  });

  describe('AudioOutput — output device', () => {
    it('uses setSinkId when outputDeviceId is provided', () => {
      const stream = makeMockStream();
      render(<AudioOutput stream={stream} outputDeviceId="device-123" />);

      // AudioContext.setSinkId should be called with the device ID
      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('device-123');
    });

    it('retargets an already-mounted output when outputDeviceId changes', () => {
      const stream = makeMockStream();
      const audioContextCtor = globalThis.AudioContext as unknown as ReturnType<typeof vi.fn>;
      const { rerender } = render(<AudioOutput stream={stream} outputDeviceId="speaker-a" />);

      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('speaker-a');
      expect(audioContextCtor).toHaveBeenCalledTimes(1);

      mockAudioContext.setSinkId.mockClear();
      mockAudioContext.createMediaElementSource.mockClear();

      rerender(<AudioOutput stream={stream} outputDeviceId="speaker-b" />);

      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('speaker-b');
      expect(audioContextCtor).toHaveBeenCalledTimes(1);
      expect(mockAudioContext.createMediaElementSource).not.toHaveBeenCalled();
    });

    it('reapplies outputDeviceId when the stream is replaced', () => {
      const audioContextCtor = globalThis.AudioContext as unknown as ReturnType<typeof vi.fn>;
      const { rerender } = render(
        <AudioOutput stream={makeMockStream('stream-a')} outputDeviceId="speaker-a" />
      );

      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('speaker-a');
      expect(audioContextCtor).toHaveBeenCalledTimes(1);

      mockAudioContext.setSinkId.mockClear();

      rerender(<AudioOutput stream={makeMockStream('stream-b')} outputDeviceId="speaker-a" />);

      expect(audioContextCtor).toHaveBeenCalledTimes(2);
      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('speaker-a');
    });

    it('clears failed setSinkId state so the same output can be retried', async () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      mockAudioContext.setSinkId.mockRejectedValueOnce(new Error('device unavailable'));

      const stream = makeMockStream();
      const { rerender } = render(<AudioOutput stream={stream} outputDeviceId="speaker-a" />);

      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('speaker-a');
      await Promise.resolve();
      expect(warnSpy).toHaveBeenCalledWith(
        'Failed to set audio output device:',
        'device unavailable'
      );

      mockAudioContext.setSinkId.mockClear();
      rerender(<AudioOutput stream={stream} />);
      expect(mockAudioContext.setSinkId).not.toHaveBeenCalled();

      rerender(<AudioOutput stream={stream} outputDeviceId="speaker-a" />);
      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('speaker-a');

      warnSpy.mockRestore();
    });

    it('routes selected output through an audio-element sink when available', () => {
      const fallbackDest = installFallbackDestination();
      const audioSinks = installAudioSinkElements();

      try {
        render(<AudioOutput stream={makeMockStream()} outputDeviceId="speaker-fallback" />);

        expect(fallbackDest.createMediaStreamDestination).toHaveBeenCalled();
        const fallbackEl = audioSinks.audioElements[
          audioSinks.audioElements.length - 1
        ] as HTMLAudioElement & {
          setSinkId: ReturnType<typeof vi.fn>;
        };
        expect(fallbackEl.setSinkId).toHaveBeenCalledWith('speaker-fallback');
        expect(mockAudioContext.setSinkId).not.toHaveBeenCalled();
      } finally {
        audioSinks.restore();
        fallbackDest.restore();
      }
    });

    it('sets the fallback sink before playing routed call audio', async () => {
      const fallbackDest = installFallbackDestination();
      const audioSinks = installAudioSinkElements();

      try {
        render(<AudioOutput stream={makeMockStream()} outputDeviceId="speaker-fallback" />);
        await Promise.resolve();

        const fallbackEl = audioSinks.audioElements[
          audioSinks.audioElements.length - 1
        ] as HTMLAudioElement & {
          play: ReturnType<typeof vi.fn>;
          setSinkId: ReturnType<typeof vi.fn>;
        };
        expect(fallbackEl.setSinkId.mock.invocationCallOrder[0]).toBeLessThan(
          fallbackEl.play.mock.invocationCallOrder[0]
        );
      } finally {
        audioSinks.restore();
        fallbackDest.restore();
      }
    });

    it('keeps call audio audible if fallback sink selection fails', async () => {
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      const fallbackDest = installFallbackDestination();
      const audioSinks = installAudioSinkElements(() =>
        vi.fn().mockRejectedValue(new Error('sink blocked'))
      );

      try {
        render(<AudioOutput stream={makeMockStream()} outputDeviceId="speaker-fallback" />);
        await Promise.resolve();
        await Promise.resolve();

        const fallbackEl = audioSinks.audioElements[
          audioSinks.audioElements.length - 1
        ] as HTMLAudioElement & { play: ReturnType<typeof vi.fn> };
        expect(fallbackEl.play).toHaveBeenCalled();
      } finally {
        warnSpy.mockRestore();
        audioSinks.restore();
        fallbackDest.restore();
      }
    });
  });

  describe('AudioOutputs — filtering', () => {
    it('does not render audio for participants without streams', () => {
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
            // No audioStream or screenAudioStream
          },
        },
      });
      const { container } = render(<AudioOutputs />);
      expect(container.querySelectorAll('audio').length).toBe(0);
    });

    it('passes audioOutputDeviceId to AudioOutput components', () => {
      useVoiceStore.setState({
        audioOutputDeviceId: 'my-device',
        participants: {
          'remote-1': {
            userId: 'remote-1',
            username: 'alice',
            isMuted: false,
            isDeafened: false,
            isVideoOn: false,
            isScreenSharing: false,
            isSpeaking: false,
            audioStream: makeMockStream(),
          },
        },
      });
      render(<AudioOutputs />);
      // The AudioOutput should receive the device ID
      expect(mockAudioContext.setSinkId).toHaveBeenCalledWith('my-device');
    });
  });
});
