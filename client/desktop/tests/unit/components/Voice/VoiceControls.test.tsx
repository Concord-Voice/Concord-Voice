import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { vi } from 'vitest';

// ── Service mocks ────────────────────────────────────────────────────────────
const mockToggleMute = vi.fn();
const mockToggleDeafen = vi.fn();
const mockToggleVideo = vi.fn().mockResolvedValue(undefined);
const mockToggleScreenShare = vi.fn().mockResolvedValue(undefined);
const mockLeaveChannel = vi.fn().mockResolvedValue(undefined);

vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    toggleMute: mockToggleMute,
    toggleDeafen: mockToggleDeafen,
    toggleVideo: mockToggleVideo,
    toggleScreenShare: mockToggleScreenShare,
    leaveChannel: mockLeaveChannel,
  },
}));

// ── OS permission store mock ─────────────────────────────────────────────────
const mockCheckOne = vi.fn().mockResolvedValue('granted');
const mockOpenSettings = vi.fn();
vi.mock('@/renderer/stores/osPermissionStore', () => ({
  useOsPermissionStore: Object.assign(vi.fn(), {
    getState: vi.fn(() => ({
      checkOne: mockCheckOne,
      openSettings: mockOpenSettings,
    })),
  }),
}));

// ── ScreenSharePicker mock ───────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/ScreenSharePicker', () => ({
  default: ({ onSelect, onCancel }: { onSelect: (id: string) => void; onCancel: () => void }) => (
    <div data-testid="screen-share-picker">
      <button onClick={() => onSelect('source-1')}>Select Source</button>
      <button onClick={onCancel}>Cancel Picker</button>
    </div>
  ),
}));

// ── CSS mock ─────────────────────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/VoiceControls.css', () => ({}));

import VoiceControls from '@/renderer/components/Voice/VoiceControls';

// ── Helpers ──────────────────────────────────────────────────────────────────
const VOICE_CHANNEL_ID = 'voice-1';

function setVoiceState(overrides: Record<string, unknown> = {}) {
  useVoiceStore.setState({
    activeChannelId: VOICE_CHANNEL_ID,
    connectionState: 'connected',
    isMuted: false,
    isDeafened: false,
    isVideoOn: false,
    isScreenSharing: false,
    showVoiceTextChat: false,
    participants: {},
    videoSlotError: null,
    tunedInScreenShares: {},
    keepActiveWhileUnfocused: false,
    voiceControlsPinned: false,
    ...overrides,
  });
}

describe('VoiceControls', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'disconnected',
    });
    useChannelStore.setState({ channels: [] });
  });

  // ── Rendering ────────────────────────────────────────────────────────────

  it('returns null when connectionState is disconnected', () => {
    useVoiceStore.setState({ connectionState: 'disconnected' });
    const { container } = render(<VoiceControls />);
    expect(container.firstChild).toBeNull();
  });

  it('renders controls when connected', () => {
    setVoiceState();
    render(<VoiceControls />);
    expect(screen.getByTitle('Mute')).toBeInTheDocument();
    expect(screen.getByTitle('Deafen')).toBeInTheDocument();
    expect(screen.getByTitle('Start Video')).toBeInTheDocument();
    expect(screen.getByTitle('Share Screen')).toBeInTheDocument();
    expect(screen.getByTitle('Leave Voice')).toBeInTheDocument();
  });

  // ── Mute/Unmute ──────────────────────────────────────────────────────────

  it('shows Mute label when not muted', () => {
    setVoiceState({ isMuted: false });
    render(<VoiceControls />);
    expect(screen.getByText('Mute')).toBeInTheDocument();
  });

  it('shows Unmute label when muted', () => {
    setVoiceState({ isMuted: true });
    render(<VoiceControls />);
    expect(screen.getByText('Unmute')).toBeInTheDocument();
  });

  it('calls voiceService.toggleMute when mute button clicked', async () => {
    setVoiceState();
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Mute'));
    });
    expect(mockToggleMute).toHaveBeenCalled();
  });

  // ── Deafen/Undeafen ─────────────────────────────────────────────────────

  it('shows Deafen label when not deafened', () => {
    setVoiceState({ isDeafened: false });
    render(<VoiceControls />);
    expect(screen.getByText('Deafen')).toBeInTheDocument();
  });

  it('shows Undeafen label when deafened', () => {
    setVoiceState({ isDeafened: true });
    render(<VoiceControls />);
    expect(screen.getByText('Undeafen')).toBeInTheDocument();
  });

  it('calls voiceService.toggleDeafen when deafen button clicked', async () => {
    setVoiceState();
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Deafen'));
    });
    expect(mockToggleDeafen).toHaveBeenCalled();
  });

  // ── Video toggle ─────────────────────────────────────────────────────────

  it('shows Video label when video is off', () => {
    setVoiceState({ isVideoOn: false });
    render(<VoiceControls />);
    expect(screen.getByText('Video')).toBeInTheDocument();
  });

  it('shows Stop Video label when video is on', () => {
    setVoiceState({ isVideoOn: true });
    render(<VoiceControls />);
    expect(screen.getByText('Stop Video')).toBeInTheDocument();
  });

  it('calls voiceService.toggleVideo when video button clicked (video off)', async () => {
    setVoiceState({ isVideoOn: false });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Start Video'));
    });
    expect(mockToggleVideo).toHaveBeenCalled();
  });

  it('does not toggle video when camera permission is denied', async () => {
    mockCheckOne.mockResolvedValueOnce('denied');
    setVoiceState({ isVideoOn: false });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Start Video'));
    });
    expect(mockToggleVideo).not.toHaveBeenCalled();
    expect(mockOpenSettings).toHaveBeenCalledWith('camera');
  });

  // ── Screen share toggle ──────────────────────────────────────────────────

  it('shows Screen label when not sharing', () => {
    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);
    expect(screen.getByText('Screen')).toBeInTheDocument();
  });

  it('shows Stop label when screen sharing', () => {
    setVoiceState({ isScreenSharing: true });
    render(<VoiceControls />);
    expect(screen.getByText('Stop')).toBeInTheDocument();
  });

  it('calls toggleScreenShare when stopping screen share', async () => {
    setVoiceState({ isScreenSharing: true });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Stop Sharing'));
    });
    expect(mockToggleScreenShare).toHaveBeenCalled();
  });

  it('does not start screen share when permission denied', async () => {
    mockCheckOne.mockResolvedValueOnce('denied');
    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });
    expect(mockToggleScreenShare).not.toHaveBeenCalled();
    expect(mockOpenSettings).toHaveBeenCalledWith('screen');
  });

  // ── Leave voice ──────────────────────────────────────────────────────────

  it('calls voiceService.leaveChannel when leave button clicked', async () => {
    setVoiceState();
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Leave Voice'));
    });
    expect(mockLeaveChannel).toHaveBeenCalled();
  });

  // ── Text chat toggle ─────────────────────────────────────────────────────

  it('does not show chat toggle when no linked text channel', () => {
    setVoiceState();
    useChannelStore.setState({ channels: [] });
    render(<VoiceControls />);
    expect(screen.queryByText('Chat')).not.toBeInTheDocument();
  });

  it('shows chat toggle when linked text channel exists', () => {
    setVoiceState();
    useChannelStore.setState({
      channels: [
        {
          id: 'text-1',
          server_id: 's1',
          name: 'voice-text',
          type: 'text' as const,
          position: 0,
          linked_voice_channel_id: VOICE_CHANNEL_ID,
          created_at: '',
          updated_at: '',
        },
      ],
    });
    render(<VoiceControls />);
    expect(screen.getByText('Chat')).toBeInTheDocument();
  });

  // ── Keep active while unfocused ──────────────────────────────────────────

  it('does not show keep-active button when not screen sharing', () => {
    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);
    expect(screen.queryByText('Always On')).not.toBeInTheDocument();
    expect(screen.queryByText('Auto Pause')).not.toBeInTheDocument();
  });

  it('shows Auto Pause button when screen sharing and keepActiveWhileUnfocused is false', () => {
    setVoiceState({ isScreenSharing: true, keepActiveWhileUnfocused: false });
    render(<VoiceControls />);
    expect(screen.getByText('Auto Pause')).toBeInTheDocument();
  });

  it('shows Always On button when screen sharing and keepActiveWhileUnfocused is true', () => {
    setVoiceState({ isScreenSharing: true, keepActiveWhileUnfocused: true });
    render(<VoiceControls />);
    expect(screen.getByText('Always On')).toBeInTheDocument();
  });

  // ── Video slot error ─────────────────────────────────────────────────────

  it('renders video slot error when present', () => {
    setVoiceState({ videoSlotError: 'Camera access denied.' });
    render(<VoiceControls />);
    expect(screen.getByText('Camera access denied.')).toBeInTheDocument();
  });

  it('does not render video slot error when null', () => {
    setVoiceState({ videoSlotError: null });
    render(<VoiceControls />);
    expect(screen.queryByText('Camera access denied.')).not.toBeInTheDocument();
  });

  // ── Persistent context ───────────────────────────────────────────────────

  it('shows pin button in persistent context', () => {
    setVoiceState();
    render(<VoiceControls context="persistent" />);
    expect(screen.getByTitle('Pin controls')).toBeInTheDocument();
  });

  it('does not show pin button in voiceView context', () => {
    setVoiceState();
    render(<VoiceControls context="voiceView" />);
    expect(screen.queryByTitle('Pin controls')).not.toBeInTheDocument();
    expect(screen.queryByTitle('Unpin controls')).not.toBeInTheDocument();
  });

  it('shows Unpin controls title when voiceControlsPinned is true', () => {
    setVoiceState({ voiceControlsPinned: true });
    render(<VoiceControls context="persistent" />);
    expect(screen.getByTitle('Unpin controls')).toBeInTheDocument();
  });

  it('shows Pop Out button in persistent context with onPopOut', () => {
    setVoiceState();
    const onPopOut = vi.fn();
    render(<VoiceControls context="persistent" onPopOut={onPopOut} />);
    expect(screen.getByTitle('Pop out controls')).toBeInTheDocument();
  });

  it('calls onPopOut when pop out button clicked', async () => {
    setVoiceState();
    const onPopOut = vi.fn();
    render(<VoiceControls context="persistent" onPopOut={onPopOut} />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Pop out controls'));
    });
    expect(onPopOut).toHaveBeenCalled();
  });

  // ── Active class toggling ────────────────────────────────────────────────

  it('applies active class to mute button when muted', () => {
    setVoiceState({ isMuted: true });
    render(<VoiceControls />);
    expect(screen.getByTitle('Unmute').className).toContain('voice-controls__btn--active');
  });

  it('applies active class to deafen button when deafened', () => {
    setVoiceState({ isDeafened: true });
    render(<VoiceControls />);
    expect(screen.getByTitle('Undeafen').className).toContain('voice-controls__btn--active');
  });

  it('applies active class to video button when video on', () => {
    setVoiceState({ isVideoOn: true });
    render(<VoiceControls />);
    expect(screen.getByTitle('Stop Video').className).toContain('voice-controls__btn--active');
  });

  it('applies active class to screen share button when sharing', () => {
    setVoiceState({ isScreenSharing: true });
    render(<VoiceControls />);
    expect(screen.getByTitle('Stop Sharing').className).toContain('voice-controls__btn--active');
  });

  it('applies danger class to leave button', () => {
    setVoiceState();
    render(<VoiceControls />);
    expect(screen.getByTitle('Leave Voice').className).toContain('voice-controls__btn--danger');
  });

  // ── Server-enforced mute/deafen — locked + disabled ────────────────────

  it('renders mute button as disabled when server-muted', () => {
    setVoiceState({
      isMuted: true,
      participants: {
        'user-1': { serverMuted: true, serverDeafened: false },
      },
    });
    // Set local user so the component can look up the local participant
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID, connectionState: 'connected' });
    useUserStore.setState({ user: { id: 'user-1' } });

    render(<VoiceControls />);
    const muteBtn = screen.getByTitle('Server-muted by a moderator');
    expect(muteBtn).toBeDisabled();
    expect(muteBtn.className).toContain('voice-controls__btn--locked');
  });

  it('renders deafen button as disabled when server-deafened', () => {
    setVoiceState({
      isDeafened: true,
      participants: {
        'user-1': { serverMuted: false, serverDeafened: true },
      },
    });
    useUserStore.setState({ user: { id: 'user-1' } });

    render(<VoiceControls />);
    const deafenBtn = screen.getByTitle('Server-deafened by a moderator');
    expect(deafenBtn).toBeDisabled();
    expect(deafenBtn.className).toContain('voice-controls__btn--locked');
  });

  it('renders mute button as enabled when not server-muted', () => {
    setVoiceState({
      isMuted: false,
      participants: {
        'user-1': { serverMuted: false, serverDeafened: false },
      },
    });
    useUserStore.setState({ user: { id: 'user-1' } });

    render(<VoiceControls />);
    const muteBtn = screen.getByTitle('Mute');
    expect(muteBtn).not.toBeDisabled();
    expect(muteBtn.className).not.toContain('voice-controls__btn--locked');
  });

  // ── Screen share with Electron picker ───────────────────────────────────

  it('opens screen share picker when Electron getDesktopSources is available', async () => {
    // Simulate Electron environment with getDesktopSources
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([]),
    } as unknown as typeof globalThis.electron;

    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });

    expect(screen.getByTestId('screen-share-picker')).toBeInTheDocument();
    expect(mockToggleScreenShare).not.toHaveBeenCalled();

    globalThis.electron = origElectron;
  });

  it('portals screen share picker to the document body', async () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([]),
    } as unknown as typeof globalThis.electron;

    setVoiceState({ isScreenSharing: false });
    const { container } = render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });

    const picker = screen.getByTestId('screen-share-picker');
    expect(document.body).toContainElement(picker);
    expect(container).not.toContainElement(picker);

    globalThis.electron = origElectron;
  });

  it('calls toggleScreenShare with sourceId when picker source selected', async () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([]),
    } as unknown as typeof globalThis.electron;

    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);

    // Open picker
    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });

    // Select a source
    await act(async () => {
      fireEvent.click(screen.getByText('Select Source'));
    });

    expect(mockToggleScreenShare).toHaveBeenCalledWith('source-1', undefined);
    expect(screen.queryByTestId('screen-share-picker')).not.toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  it('closes screen share picker on cancel', async () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      getDesktopSources: vi.fn().mockResolvedValue([]),
    } as unknown as typeof globalThis.electron;

    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });
    expect(screen.getByTestId('screen-share-picker')).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByText('Cancel Picker'));
    });
    expect(screen.queryByTestId('screen-share-picker')).not.toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  it('calls toggleScreenShare directly when no Electron picker and not sharing', async () => {
    // Ensure no electron.getDesktopSources
    const origElectron = globalThis.electron;
    globalThis.electron = undefined as unknown as typeof globalThis.electron;

    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });
    expect(mockToggleScreenShare).toHaveBeenCalled();

    globalThis.electron = origElectron;
  });

  // ── Keep active while unfocused toggle ──────────────────────────────────

  it('toggles keepActiveWhileUnfocused when button clicked', async () => {
    const mockSetKeepActive = vi.fn();
    setVoiceState({
      isScreenSharing: true,
      keepActiveWhileUnfocused: false,
      setKeepActiveWhileUnfocused: mockSetKeepActive,
    });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Stream pauses when unfocused'));
    });
    expect(mockSetKeepActive).toHaveBeenCalledWith(true);
  });

  // ── PiP menu ────────────────────────────────────────────────────────────

  it('shows PiP button when electron.openPipWindow is available', () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      openPipWindow: vi.fn(),
    } as unknown as typeof globalThis.electron;

    setVoiceState();
    render(<VoiceControls />);
    expect(screen.getByTitle('Picture-in-Picture')).toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  it('does not show PiP button when electron.openPipWindow is not available', () => {
    const origElectron = globalThis.electron;
    globalThis.electron = undefined as unknown as typeof globalThis.electron;

    setVoiceState();
    render(<VoiceControls />);
    expect(screen.queryByTitle('Picture-in-Picture')).not.toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  it('opens PiP menu when PiP button clicked', async () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      openPipWindow: vi.fn(),
    } as unknown as typeof globalThis.electron;

    setVoiceState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      participants: {
        u1: { isScreenSharing: true, displayName: 'Alice', username: 'alice' },
      },
    });
    render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Picture-in-Picture'));
    });

    expect(screen.getByText('Pop Out User Frames')).toBeInTheDocument();
    expect(screen.getByText(/Pop Out Alice.s Screen/)).toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  it('calls electron.openPipWindow with frames options when Pop Out User Frames clicked', async () => {
    const mockOpenPip = vi.fn().mockResolvedValue(undefined);
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      openPipWindow: mockOpenPip,
    } as unknown as typeof globalThis.electron;

    setVoiceState({ tunedInScreenShares: {} });
    render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Picture-in-Picture'));
    });
    await act(async () => {
      fireEvent.click(screen.getByText('Pop Out User Frames'));
    });

    expect(mockOpenPip).toHaveBeenCalledWith({
      id: 'frames-main',
      width: 320,
      height: 240,
    });

    globalThis.electron = origElectron;
  });

  it('calls electron.openPipWindow with screen options when Pop Out screen clicked', async () => {
    const mockOpenPip = vi.fn().mockResolvedValue(undefined);
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      openPipWindow: mockOpenPip,
    } as unknown as typeof globalThis.electron;

    setVoiceState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      participants: {
        u1: { isScreenSharing: true, displayName: 'Bob', username: 'bob' },
      },
    });
    render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Picture-in-Picture'));
    });
    await act(async () => {
      fireEvent.click(screen.getByText(/Pop Out Bob.s Screen/));
    });

    expect(mockOpenPip).toHaveBeenCalledWith({
      id: 'screen-producer-1',
      width: 400,
      height: 300,
    });

    globalThis.electron = origElectron;
  });

  it('closes PiP menu when Escape is pressed', async () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      openPipWindow: vi.fn(),
    } as unknown as typeof globalThis.electron;

    setVoiceState({ tunedInScreenShares: {} });
    render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Picture-in-Picture'));
    });
    expect(screen.getByText('Pop Out User Frames')).toBeInTheDocument();

    await act(async () => {
      fireEvent.keyDown(document, { key: 'Escape' });
    });
    expect(screen.queryByText('Pop Out User Frames')).not.toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  it('closes PiP menu when clicking outside', async () => {
    const origElectron = globalThis.electron;
    globalThis.electron = {
      ...globalThis.electron,
      openPipWindow: vi.fn(),
    } as unknown as typeof globalThis.electron;

    setVoiceState({ tunedInScreenShares: {} });
    render(<VoiceControls />);

    await act(async () => {
      fireEvent.click(screen.getByTitle('Picture-in-Picture'));
    });
    expect(screen.getByText('Pop Out User Frames')).toBeInTheDocument();

    await act(async () => {
      fireEvent.mouseDown(document.body);
    });
    expect(screen.queryByText('Pop Out User Frames')).not.toBeInTheDocument();

    globalThis.electron = origElectron;
  });

  // ── Video slot error auto-dismiss ───────────────────────────────────────

  it('auto-dismisses video slot error after 5 seconds', async () => {
    vi.useFakeTimers();
    const mockSetError = vi.fn();
    setVoiceState({ videoSlotError: 'Some error', setVideoSlotError: mockSetError });
    render(<VoiceControls />);

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });

    expect(mockSetError).toHaveBeenCalledWith(null);
    vi.useRealTimers();
  });

  // ── Screen share error handling ─────────────────────────────────────────

  it('handles screen share toggle error gracefully', async () => {
    mockToggleScreenShare.mockRejectedValueOnce(new Error('Failed'));
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    setVoiceState({ isScreenSharing: true });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Stop Sharing'));
    });

    expect(consoleSpy).toHaveBeenCalledWith('Failed to toggle screen share:', expect.any(String));
    consoleSpy.mockRestore();
  });

  // ── Screen permission restricted ────────────────────────────────────────

  it('does not start screen share when permission is restricted', async () => {
    mockCheckOne.mockResolvedValueOnce('restricted');
    setVoiceState({ isScreenSharing: false });
    render(<VoiceControls />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Share Screen'));
    });
    expect(mockToggleScreenShare).not.toHaveBeenCalled();
    expect(mockOpenSettings).toHaveBeenCalledWith('screen');
  });
});
