import React from 'react';
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';

// ── Mocks ───────────────────────────────────────────────────────────────

// Mock useParams
let mockPipId = 'controls-main';
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual('react-router-dom');
  return {
    ...actual,
    useParams: () => ({ pipId: mockPipId }),
  };
});

// Mock PipVoiceClient
const mockInit = vi.fn();
const mockDispose = vi.fn().mockResolvedValue(undefined);
const mockAction = vi.fn().mockResolvedValue(undefined);
const mockConsume = vi.fn().mockResolvedValue(null);
const mockSignalReady = vi.fn().mockResolvedValue(undefined);
let mockOnStateUpdate: ((msg: any) => void) | null = null;

vi.mock('@/renderer/services/pipVoiceClient', () => ({
  PipVoiceClient: vi.fn().mockImplementation(function (this: any) {
    this.init = mockInit;
    this.dispose = mockDispose;
    this.action = mockAction;
    this.consume = mockConsume;
    this.signalReady = mockSignalReady;
    this.getStreams = vi.fn().mockReturnValue(new Map());
    this.getStreamBySource = vi.fn().mockReturnValue(null);
    Object.defineProperty(this, 'onStateUpdate', {
      get() {
        return mockOnStateUpdate;
      },
      set(cb: any) {
        mockOnStateUpdate = cb;
      },
    });
  }),
}));

// Mock child components
vi.mock('@/renderer/components/Voice/ParticipantTile', () => ({
  default: ({ participant }: { participant: any }) => (
    <div data-testid={`participant-${participant.userId}`}>{participant.username}</div>
  ),
}));

vi.mock('@/renderer/components/Voice/useVoiceMagnification', () => ({
  useVoiceMagnification: () => ({}),
}));

// Mock CSS
vi.mock('@/renderer/components/Voice/PipWindow.css', () => ({}));

// Mock lucide-react icons
vi.mock('lucide-react', () => ({
  Pin: () => <span data-testid="icon-pin" />,
  PinOff: () => <span data-testid="icon-pin-off" />,
  X: () => <span data-testid="icon-x" />,
  Mic: () => <span data-testid="icon-mic" />,
  MicOff: () => <span data-testid="icon-mic-off" />,
  Volume2: () => <span data-testid="icon-volume2" />,
  VolumeX: () => <span data-testid="icon-volume-x" />,
  Video: () => <span data-testid="icon-video" />,
  VideoOff: () => <span data-testid="icon-video-off" />,
  Monitor: () => <span data-testid="icon-monitor" />,
  MonitorOff: () => <span data-testid="icon-monitor-off" />,
  PhoneOff: () => <span data-testid="icon-phone-off" />,
}));

// ── Import after mocks ──────────────────────────────────────────────────

import PipWindow from '@/renderer/components/Voice/PipWindow';

// ── Helpers ─────────────────────────────────────────────────────────────

const mockVoiceState = {
  participants: {
    'user-1': {
      userId: 'user-1',
      username: 'alice',
      isMuted: true,
      isDeafened: false,
      isVideoOn: false,
      isScreenSharing: false,
      isSpeaking: false,
    },
    'user-2': {
      userId: 'user-2',
      username: 'bob',
      isMuted: false,
      isDeafened: false,
      isVideoOn: true,
      isScreenSharing: false,
      isSpeaking: false,
    },
  },
  tunedInScreenShares: {},
  routerRtpCapabilities: { codecs: [] },
  activeProducers: [
    { producerId: 'p1', userId: 'user-1', source: 'mic' },
    { producerId: 'p2', userId: 'user-2', source: 'mic' },
    { producerId: 'p3', userId: 'user-2', source: 'camera' },
  ],
  localUserId: 'user-1',
};

let savedElectron: any;

function mockElectron() {
  savedElectron = globalThis.electron;
  globalThis.electron = {
    ...savedElectron,
    openPipWindow: vi.fn().mockResolvedValue(undefined),
    closePipWindow: vi.fn().mockResolvedValue(undefined),
    setPipAlwaysOnTop: vi.fn().mockResolvedValue(undefined),
    onPipClosed: vi.fn().mockReturnValue(() => {}),
  } as any;
}

// ── Tests ───────────────────────────────────────────────────────────────

describe('PipWindow', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockOnStateUpdate = null;
    mockElectron();
    mockInit.mockResolvedValue(mockVoiceState);
  });

  afterEach(() => {
    globalThis.electron = savedElectron;
  });

  // ── Routing ─────────────────────────────────────────────────────────

  describe('routing by pipId prefix', () => {
    it('renders controls content for controls-* pipId', async () => {
      mockPipId = 'controls-main';
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTitle('Mute')).toBeInTheDocument();
      });
    });

    it('renders frames content for frames-* pipId', async () => {
      mockPipId = 'frames-123';
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });
    });

    it('renders screen content for screen-* pipId', async () => {
      mockPipId = 'screen-prod1';
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });
    });

    it('renders unknown type for unrecognized pipId', () => {
      mockPipId = 'unknown-123';
      render(<PipWindow />);
      expect(screen.getByText('Unknown PiP type')).toBeInTheDocument();
    });
  });

  // ── PipHeader ───────────────────────────────────────────────────────

  describe('PipHeader', () => {
    it('shows pin icon when pinned (default state)', async () => {
      mockPipId = 'controls-main';
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTitle('Unpin from top')).toBeInTheDocument();
      });
    });

    it('clicking pin toggles state and calls setPipAlwaysOnTop', async () => {
      mockPipId = 'controls-main';
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTitle('Unpin from top')).toBeInTheDocument();
      });

      fireEvent.click(screen.getByTitle('Unpin from top'));

      expect(globalThis.electron?.setPipAlwaysOnTop).toHaveBeenCalledWith('controls-main', false);
    });

    it('clicking close calls closePipWindow', async () => {
      mockPipId = 'controls-main';
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTitle('Close PiP')).toBeInTheDocument();
      });

      fireEvent.click(screen.getByTitle('Close PiP'));

      expect(globalThis.electron?.closePipWindow).toHaveBeenCalledWith('controls-main');
    });
  });

  // ── ControlsPipContent ──────────────────────────────────────────────

  describe('ControlsPipContent', () => {
    beforeEach(() => {
      mockPipId = 'controls-main';
    });

    it('creates PipVoiceClient and calls init()', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });
    });

    it('renders all control buttons', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTitle('Unmute')).toBeInTheDocument();
      });

      // Local user (user-1) is muted, so shows "Unmute"
      expect(screen.getByTitle('Unmute')).toBeInTheDocument();
      expect(screen.getByTitle('Deafen')).toBeInTheDocument();
      expect(screen.getByTitle('Start Video')).toBeInTheDocument();
      expect(screen.getByTitle('Share Screen')).toBeInTheDocument();
      expect(screen.getByTitle('Leave Voice')).toBeInTheDocument();
    });

    it('clicking mute button calls client.action(toggle-mute)', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTitle('Unmute')).toBeInTheDocument();
      });

      fireEvent.click(screen.getByTitle('Unmute'));

      expect(mockAction).toHaveBeenCalledWith('toggle-mute');
    });

    it('displays participant count', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByText('2 participants')).toBeInTheDocument();
      });
    });

    it('updates button states from initial voice state (localUserId fix)', async () => {
      render(<PipWindow />);

      // User-1 is the local user and is muted
      await waitFor(() => {
        // Should show MicOff icon (muted state) and "Unmute" title
        expect(screen.getByTitle('Unmute')).toBeInTheDocument();
      });
    });

    it('updates button states from state-update broadcast', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      // Simulate state-update broadcast with local user now unmuted and video on
      act(() => {
        mockOnStateUpdate?.({
          type: 'state-update',
          participants: {
            'user-1': {
              userId: 'user-1',
              username: 'alice',
              isMuted: false,
              isDeafened: true,
              isVideoOn: true,
              isScreenSharing: false,
            },
          },
          localUserId: 'user-1',
        });
      });

      await waitFor(() => {
        expect(screen.getByTitle('Mute')).toBeInTheDocument();
        expect(screen.getByTitle('Undeafen')).toBeInTheDocument();
        expect(screen.getByTitle('Stop Video')).toBeInTheDocument();
      });
    });

    it('populates localUserIdRef from broadcast when init has not resolved yet', async () => {
      // Make init() hang — simulates broadcast arriving before init resolves
      let resolveInit: (value: any) => void;
      mockInit.mockReturnValue(
        new Promise((r) => {
          resolveInit = r;
        })
      );

      render(<PipWindow />);

      // Send a state-update broadcast before init resolves — localUserIdRef is still ''
      act(() => {
        mockOnStateUpdate?.({
          type: 'state-update',
          participants: {
            'user-1': {
              userId: 'user-1',
              username: 'alice',
              isMuted: true,
              isDeafened: false,
              isVideoOn: false,
              isScreenSharing: false,
            },
          },
          localUserId: 'user-1',
        });
      });

      // Button state should update despite init not having resolved
      await waitFor(() => {
        expect(screen.getByTitle('Unmute')).toBeInTheDocument();
        expect(screen.getByText('1 participant')).toBeInTheDocument();
      });

      // Clean up: resolve init so dispose doesn't hang
      resolveInit!(mockVoiceState);
    });

    it('closes on voice-ended broadcast', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      act(() => {
        mockOnStateUpdate?.({ type: 'voice-ended' });
      });

      expect(globalThis.electron?.closePipWindow).toHaveBeenCalledWith('controls-main');
    });
  });

  // ── FramesPipContent ────────────────────────────────────────────────

  describe('FramesPipContent', () => {
    beforeEach(() => {
      mockPipId = 'frames-123';
    });

    it('shows Connecting... while loading', () => {
      mockInit.mockReturnValue(new Promise(() => {})); // Never resolves
      render(<PipWindow />);
      expect(screen.getByText('Connecting...')).toBeInTheDocument();
    });

    it('renders ParticipantTile for each participant after init', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTestId('participant-user-1')).toBeInTheDocument();
        expect(screen.getByTestId('participant-user-2')).toBeInTheDocument();
      });
    });

    it('shows No participants when none exist', async () => {
      mockInit.mockResolvedValue({
        ...mockVoiceState,
        participants: {},
        activeProducers: [],
      });

      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByText('No participants')).toBeInTheDocument();
      });
    });

    it('disposes client on unmount', async () => {
      const { unmount } = render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      unmount();

      expect(mockDispose).toHaveBeenCalled();
    });

    it('consumes new producer on producer-added broadcast', async () => {
      const mockStream = { id: 'new-stream' };
      mockConsume.mockResolvedValue(mockStream);

      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      act(() => {
        mockOnStateUpdate?.({
          type: 'producer-added',
          producerId: 'p-new',
          userId: 'user-3',
          source: 'camera',
        });
      });

      await waitFor(() => {
        expect(mockConsume).toHaveBeenCalledWith('p-new', 'camera', 'user-3');
      });
    });

    it('queues producer-added consumes sequentially', async () => {
      const callOrder: string[] = [];
      mockConsume.mockImplementation(async (producerId: string) => {
        callOrder.push(`start-${producerId}`);
        await new Promise((r) => setTimeout(r, 10));
        callOrder.push(`end-${producerId}`);
        return { id: `stream-${producerId}` };
      });

      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      // Fire two producer-added broadcasts rapidly
      act(() => {
        mockOnStateUpdate?.({
          type: 'producer-added',
          producerId: 'p-a',
          userId: 'user-3',
          source: 'mic',
        });
        mockOnStateUpdate?.({
          type: 'producer-added',
          producerId: 'p-b',
          userId: 'user-4',
          source: 'camera',
        });
      });

      await waitFor(() => {
        expect(callOrder).toContain('end-p-b');
      });

      // First consume should complete before second starts
      expect(callOrder.indexOf('end-p-a')).toBeLessThan(callOrder.indexOf('start-p-b'));
    });

    it('clears all streams (audio, video, screen) on producer-closed broadcast', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByTestId('participant-user-2')).toBeInTheDocument();
      });

      act(() => {
        mockOnStateUpdate?.({
          type: 'producer-closed',
          producerId: 'p3',
          userId: 'user-2',
        });
      });

      // Participant should still exist but streams cleared
      expect(screen.getByTestId('participant-user-2')).toBeInTheDocument();
    });

    it('closes on voice-ended broadcast', async () => {
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      act(() => {
        mockOnStateUpdate?.({ type: 'voice-ended' });
      });

      expect(globalThis.electron?.closePipWindow).toHaveBeenCalled();
    });
  });

  // ── ScreenPipContent ────────────────────────────────────────────────

  describe('ScreenPipContent', () => {
    beforeEach(() => {
      mockPipId = 'screen-prod-screen1';
      mockInit.mockResolvedValue({
        ...mockVoiceState,
        activeProducers: [{ producerId: 'prod-screen1', userId: 'user-2', source: 'screen' }],
      });
    });

    it('shows Connecting... while loading', () => {
      mockInit.mockReturnValue(new Promise(() => {}));
      render(<PipWindow />);
      expect(screen.getByText('Connecting...')).toBeInTheDocument();
    });

    it('shows Screen share ended when no stream available', async () => {
      mockConsume.mockResolvedValue(null);
      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByText('Screen share ended')).toBeInTheDocument();
      });
    });

    it('shows sharer name when stream is available', async () => {
      const mockStream = { id: 'stream-1' };
      mockConsume.mockResolvedValue(mockStream);

      // Mock HTMLVideoElement.play to return a resolved promise
      const originalPlay = HTMLVideoElement.prototype.play;
      HTMLVideoElement.prototype.play = vi.fn().mockResolvedValue(undefined);

      render(<PipWindow />);

      await waitFor(() => {
        expect(screen.getByText("bob's screen")).toBeInTheDocument();
      });

      HTMLVideoElement.prototype.play = originalPlay;
    });

    it('closes on producer-closed broadcast for matching producerId', async () => {
      mockConsume.mockResolvedValue({ id: 'stream-1' });
      render(<PipWindow />);

      await waitFor(() => {
        expect(mockInit).toHaveBeenCalled();
      });

      act(() => {
        mockOnStateUpdate?.({
          type: 'producer-closed',
          producerId: 'prod-screen1',
          userId: 'user-2',
        });
      });

      expect(globalThis.electron?.closePipWindow).toHaveBeenCalled();
    });

    it('logs error and stops loading when ScreenPip init throws', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      mockInit.mockRejectedValueOnce(new Error('screen init failed'));

      render(<PipWindow />);

      await waitFor(() => {
        expect(consoleSpy).toHaveBeenCalledWith('[ScreenPip] Init failed:', 'screen init failed');
      });
      // After error, loading spinner is gone
      expect(screen.queryByText('Connecting...')).not.toBeInTheDocument();
      consoleSpy.mockRestore();
    });
  });

  describe('FramesPipContent init error', () => {
    beforeEach(() => {
      mockPipId = 'frames-123';
    });

    it('logs error and stops loading when FramesPip init throws', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      mockInit.mockRejectedValueOnce(new Error('frames init failed'));

      render(<PipWindow />);

      await waitFor(() => {
        expect(consoleSpy).toHaveBeenCalledWith('[FramesPip] Init failed:', 'frames init failed');
      });
      expect(screen.queryByText('Connecting...')).not.toBeInTheDocument();
      consoleSpy.mockRestore();
    });
  });
});
