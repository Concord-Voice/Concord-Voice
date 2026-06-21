import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useChannelStore } from '@/renderer/stores/channelStore';

// ── Child component mocks ──────────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/ParticipantGrid', () => ({
  UserFrameGrid: () => <div data-testid="user-frame-grid" />,
  AudioOutputs: () => <div data-testid="audio-outputs" />,
}));
vi.mock('@/renderer/components/Voice/UserFrameBar', () => ({
  default: () => <div data-testid="user-frame-bar" />,
}));
vi.mock('@/renderer/components/Voice/VoiceStage', () => ({
  default: () => <div data-testid="voice-stage" />,
}));
vi.mock('@/renderer/components/Voice/StreamBar', () => ({
  default: () => <div data-testid="stream-bar" />,
}));
vi.mock('@/renderer/components/Voice/TuneInButton', () => ({
  TuneInOverlay: () => <div data-testid="tune-in-overlay" />,
}));
vi.mock('@/renderer/components/Voice/VoiceControls', () => ({
  default: () => <div data-testid="voice-controls" />,
}));
vi.mock('@/renderer/components/Voice/VoiceTextChat', () => ({
  default: () => <div data-testid="voice-text-chat" />,
}));

// ── Service mock (dynamic import inside handleJoin) ────────────────────────────
const mockJoinChannel = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: { joinChannel: mockJoinChannel },
}));

// CSS import — suppressed by vite test config
vi.mock('@/renderer/components/Voice/VoiceView.css', () => ({}));

import VoiceView from '@/renderer/components/Voice/VoiceView';

// ── Default connected store state helper ─────────────────────────────────────
const VOICE_CHANNEL_ID = 'voice-1';

function setConnectedState(overrides: Record<string, unknown> = {}) {
  useVoiceStore.setState({
    activeChannelId: VOICE_CHANNEL_ID,
    connectionState: 'connected',
    participants: {},
    effectiveQualityTier: 'standard',
    decoderHealth: 'green',
    tunedInScreenShares: {},
    availableScreenShares: [],
    showVoiceTextChat: false,
    showUserFrameBar: true,
    showStreamBar: true,
    userFrameBarHeight: 120,
    streamBarHeight: 120,
    stageLayout: 'focus',
    isScreenSharing: false,
    keepActiveWhileUnfocused: false,
    voiceTextChatLayout: 'horizontal',
    voiceTextChatHeight: 300,
    voiceTextChatWidth: 350,
    ...overrides,
  });
}

describe('VoiceView', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Reset all fields touched by these tests so state never leaks between cases
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'disconnected',
      channelVoiceMembers: {},
      participants: {},
      isScreenSharing: false,
      keepActiveWhileUnfocused: false,
      localStreamPaused: false,
      effectiveQualityTier: 'standard',
      decoderHealth: 'green',
      tunedInScreenShares: {},
      availableScreenShares: [],
      showUserFrameBar: true,
      showStreamBar: true,
      userFrameBarHeight: 120,
      streamBarHeight: 120,
      stageLayout: 'focus',
      showVoiceTextChat: false,
      voiceTextChatLayout: 'horizontal',
      voiceTextChatHeight: 300,
      voiceTextChatWidth: 350,
    });
    // No linked text channel by default
    useChannelStore.setState({ channels: [] });
  });

  // ── Join prompt (not connected) ──────────────────────────────────────────────

  it('renders join prompt when not connected', () => {
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('General')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Join Voice' })).toBeInTheDocument();
  });

  it('shows empty subtitle when no members in channel', () => {
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('No one is in this voice channel yet.')).toBeInTheDocument();
  });

  it('shows singular member count when 1 member in channel', () => {
    useVoiceStore.setState({
      channelVoiceMembers: {
        [VOICE_CHANNEL_ID]: [
          {
            userId: 'u1',
            username: 'alice',
            displayName: 'Alice',
            avatarUrl: undefined,
            isMuted: false,
          },
        ],
      },
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('1 user in this channel')).toBeInTheDocument();
  });

  it('shows plural member count when multiple members in channel', () => {
    useVoiceStore.setState({
      channelVoiceMembers: {
        [VOICE_CHANNEL_ID]: [
          {
            userId: 'u1',
            username: 'alice',
            displayName: 'Alice',
            avatarUrl: undefined,
            isMuted: false,
          },
          {
            userId: 'u2',
            username: 'bob',
            displayName: 'Bob',
            avatarUrl: undefined,
            isMuted: false,
          },
        ],
      },
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('2 users in this channel')).toBeInTheDocument();
  });

  it('shows Connecting... state in subtitle and button', () => {
    useVoiceStore.setState({ connectionState: 'connecting' });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    // Both subtitle and button say "Connecting..."
    const els = screen.getAllByText('Connecting...');
    expect(els.length).toBeGreaterThanOrEqual(2);
  });

  it('disables join button while connecting', () => {
    useVoiceStore.setState({ connectionState: 'connecting' });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByRole('button', { name: 'Connecting...' })).toBeDisabled();
  });

  it('calls voiceService.joinChannel with channelId when join button clicked', async () => {
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Join Voice' }));
    });
    expect(mockJoinChannel).toHaveBeenCalledWith(VOICE_CHANNEL_ID);
  });

  it('does not call joinChannel a second time if already joining', async () => {
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Join Voice' }));
      fireEvent.click(screen.getByRole('button', { name: 'Join Voice' }));
    });
    expect(mockJoinChannel).toHaveBeenCalledTimes(1);
  });

  // ── Connected view ───────────────────────────────────────────────────────────

  it('renders channel name in header when connected', () => {
    setConnectedState();
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByRole('heading', { name: 'General' })).toBeInTheDocument();
  });

  it('renders VoiceControls when connected', () => {
    setConnectedState();
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('voice-controls')).toBeInTheDocument();
  });

  it('always shows E2EE badge (all channels are encrypted)', () => {
    setConnectedState();
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('E2EE')).toBeInTheDocument();
  });

  it('shows quality tier badge', () => {
    setConnectedState({ effectiveQualityTier: 'high' });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('high')).toBeInTheDocument();
  });

  it('shows participant count badge', () => {
    setConnectedState({
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
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('2')).toBeInTheDocument();
  });

  it('shows Decode Warning badge when decoderHealth is yellow', () => {
    setConnectedState({ decoderHealth: 'yellow' });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('Decode Warning')).toBeInTheDocument();
  });

  it('shows Decode Overload badge when decoderHealth is red', () => {
    setConnectedState({ decoderHealth: 'red' });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByText('Decode Overload')).toBeInTheDocument();
  });

  it('does not show decoder health badge when decoderHealth is green', () => {
    setConnectedState({ decoderHealth: 'green' });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByText('Decode Warning')).not.toBeInTheDocument();
    expect(screen.queryByText('Decode Overload')).not.toBeInTheDocument();
  });

  // ── Layout modes ─────────────────────────────────────────────────────────────

  it('renders Mode A (UserFrameGrid) when no screen shares are tuned in', () => {
    setConnectedState({ tunedInScreenShares: {} });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('user-frame-grid')).toBeInTheDocument();
  });

  it('renders Mode B (VoiceStage) when screen shares are tuned in', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('voice-stage')).toBeInTheDocument();
    expect(screen.queryByTestId('user-frame-grid')).not.toBeInTheDocument();
  });

  it('shows UserFrameBar in Mode B when showUserFrameBar is true', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      showUserFrameBar: true,
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('user-frame-bar')).toBeInTheDocument();
  });

  it('hides UserFrameBar in Mode B when showUserFrameBar is false', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      showUserFrameBar: false,
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByTestId('user-frame-bar')).not.toBeInTheDocument();
  });

  it('shows TuneInOverlay when availableScreenShares is non-empty', () => {
    setConnectedState({
      availableScreenShares: [
        { producerId: 'p1', userId: 'u1', username: 'alice', displayName: 'Alice' },
      ],
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('tune-in-overlay')).toBeInTheDocument();
  });

  it('does not show TuneInOverlay when availableScreenShares is empty', () => {
    setConnectedState({ availableScreenShares: [] });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByTestId('tune-in-overlay')).not.toBeInTheDocument();
  });

  // ── Text chat ────────────────────────────────────────────────────────────────

  it('does not show VoiceTextChat when showVoiceTextChat is false', () => {
    setConnectedState({ showVoiceTextChat: false });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByTestId('voice-text-chat')).not.toBeInTheDocument();
  });

  it('does not show VoiceTextChat when showVoiceTextChat is true but no linked text channel', () => {
    setConnectedState({ showVoiceTextChat: true });
    // channelStore has no channels with linked_voice_channel_id
    useChannelStore.setState({ channels: [] });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByTestId('voice-text-chat')).not.toBeInTheDocument();
  });

  it('shows VoiceTextChat in horizontal layout when showVoiceTextChat and linked text channel', () => {
    setConnectedState({ showVoiceTextChat: true, voiceTextChatLayout: 'horizontal' });
    useChannelStore.setState({
      channels: [
        {
          id: 'text-1',
          server_id: 's1',
          name: 'voice-text',
          type: 'text',
          position: 0,
          linked_voice_channel_id: VOICE_CHANNEL_ID,
          created_at: '',
          updated_at: '',
        },
      ],
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('voice-text-chat')).toBeInTheDocument();
  });

  it('shows VoiceTextChat in vertical layout when voiceTextChatLayout is vertical', () => {
    setConnectedState({ showVoiceTextChat: true, voiceTextChatLayout: 'vertical' });
    useChannelStore.setState({
      channels: [
        {
          id: 'text-1',
          server_id: 's1',
          name: 'voice-text',
          type: 'text',
          position: 0,
          linked_voice_channel_id: VOICE_CHANNEL_ID,
          created_at: '',
          updated_at: '',
        },
      ],
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('voice-text-chat')).toBeInTheDocument();
  });

  // ── AudioOutputs NOT rendered here (moved to MainView) ───────────────────────

  it('does not render AudioOutputs — it is now in MainView', () => {
    setConnectedState();
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByTestId('audio-outputs')).not.toBeInTheDocument();
  });

  // ── Auto-pause ───────────────────────────────────────────────────────────────

  it('sets localStreamPaused true on window blur when screen sharing', () => {
    setConnectedState({ isScreenSharing: true, keepActiveWhileUnfocused: false });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    act(() => {
      globalThis.dispatchEvent(new Event('blur'));
    });

    expect(useVoiceStore.getState().localStreamPaused).toBe(true);
  });

  it('does not set localStreamPaused on blur when keepActiveWhileUnfocused is true', () => {
    setConnectedState({ isScreenSharing: true, keepActiveWhileUnfocused: true });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    act(() => {
      globalThis.dispatchEvent(new Event('blur'));
    });

    expect(useVoiceStore.getState().localStreamPaused).toBe(false);
  });

  it('does not set localStreamPaused on blur when not screen sharing', () => {
    setConnectedState({ isScreenSharing: false, keepActiveWhileUnfocused: false });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    act(() => {
      globalThis.dispatchEvent(new Event('blur'));
    });

    expect(useVoiceStore.getState().localStreamPaused).toBe(false);
  });

  it('clears localStreamPaused on window focus', () => {
    setConnectedState({ isScreenSharing: true, keepActiveWhileUnfocused: false });
    useVoiceStore.setState({ localStreamPaused: true });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    act(() => {
      globalThis.dispatchEvent(new Event('focus'));
    });

    expect(useVoiceStore.getState().localStreamPaused).toBe(false);
  });

  // ── Focus handler for auto-pause ──

  it('does not clear localStreamPaused on focus when it was not paused', () => {
    setConnectedState({ isScreenSharing: true, keepActiveWhileUnfocused: false });
    useVoiceStore.setState({ localStreamPaused: false });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    act(() => {
      globalThis.dispatchEvent(new Event('focus'));
    });

    // localStreamPaused stays false (the guard `if (!...localStreamPaused) return` short-circuits)
    expect(useVoiceStore.getState().localStreamPaused).toBe(false);
  });

  // ── Join error handling ──

  it('resets hasJoinedRef on join failure so user can retry', async () => {
    mockJoinChannel.mockRejectedValueOnce(new Error('Connection failed'));
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    // First join attempt — fails
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Join Voice' }));
    });
    expect(mockJoinChannel).toHaveBeenCalledTimes(1);

    // Second attempt — should NOT be blocked because ref was reset
    mockJoinChannel.mockResolvedValueOnce(undefined);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Join Voice' }));
    });
    expect(mockJoinChannel).toHaveBeenCalledTimes(2);
  });

  // ── Chat resize (horizontal layout) ──

  it('resizes chat panel height in horizontal layout via mouse drag', () => {
    setConnectedState({
      showVoiceTextChat: true,
      voiceTextChatLayout: 'horizontal',
      voiceTextChatHeight: 300,
    });
    useChannelStore.setState({
      channels: [
        {
          id: 'text-1',
          server_id: 's1',
          name: 'voice-text',
          type: 'text',
          position: 0,
          linked_voice_channel_id: VOICE_CHANNEL_ID,
          created_at: '',
          updated_at: '',
        },
      ],
    });
    const { container } = render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    const resizeHandle = container.querySelector(
      '.voice-text-chat-resize:not(.voice-text-chat-resize--vertical)'
    );
    expect(resizeHandle).toBeTruthy();

    // Start drag
    fireEvent.mouseDown(resizeHandle!, { clientY: 400 });

    // Move mouse up (increases chat height)
    act(() => {
      fireEvent.mouseMove(document, { clientY: 350 });
    });

    // End drag
    act(() => {
      fireEvent.mouseUp(document);
    });

    // Verify cursor was cleaned up
    expect(document.body.style.cursor).toBe('');
    expect(document.body.style.userSelect).toBe('');
  });

  // ── Chat resize (vertical layout) ──

  it('resizes chat panel width in vertical layout via mouse drag', () => {
    setConnectedState({
      showVoiceTextChat: true,
      voiceTextChatLayout: 'vertical',
      voiceTextChatWidth: 350,
    });
    useChannelStore.setState({
      channels: [
        {
          id: 'text-1',
          server_id: 's1',
          name: 'voice-text',
          type: 'text',
          position: 0,
          linked_voice_channel_id: VOICE_CHANNEL_ID,
          created_at: '',
          updated_at: '',
        },
      ],
    });
    const { container } = render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    const resizeHandle = container.querySelector('.voice-text-chat-resize--vertical');
    expect(resizeHandle).toBeTruthy();

    // Start drag
    fireEvent.mouseDown(resizeHandle!, { clientX: 500 });

    // Move mouse left (increases chat width)
    act(() => {
      fireEvent.mouseMove(document, { clientX: 450 });
    });

    // End drag
    act(() => {
      fireEvent.mouseUp(document);
    });

    expect(document.body.style.cursor).toBe('');
    expect(document.body.style.userSelect).toBe('');
  });

  // ── Section resize (UserFrameBar / StreamBar) ──

  it('resizes user frame bar height via mouse drag on section handle', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      showUserFrameBar: true,
      userFrameBarHeight: 120,
    });
    const { container } = render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    const gripHandles = container.querySelectorAll('.voice-view__section-handle-grip');
    expect(gripHandles.length).toBeGreaterThanOrEqual(1);

    // First grip is the user frame bar resize handle
    fireEvent.mouseDown(gripHandles[0], { clientY: 200 });

    act(() => {
      fireEvent.mouseMove(document, { clientY: 250 });
    });

    act(() => {
      fireEvent.mouseUp(document);
    });
  });

  it('resizes stream bar height via mouse drag when stream section visible', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1', 'producer-2': 'consumer-2' },
      showStreamBar: true,
      streamBarHeight: 120,
      stageLayout: 'focus',
    });
    const { container } = render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    const gripHandles = container.querySelectorAll('.voice-view__section-handle-grip');
    // With stream section visible, there should be 2 grips
    expect(gripHandles.length).toBe(2);

    // Second grip is the stream bar resize handle
    fireEvent.mouseDown(gripHandles[1], { clientY: 500 });

    act(() => {
      fireEvent.mouseMove(document, { clientY: 450 });
    });

    act(() => {
      fireEvent.mouseUp(document);
    });
  });

  // ── Stream section conditional rendering ──

  it('shows stream bar section when stageLayout is focus and multiple tuned-in shares', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1', 'producer-2': 'consumer-2' },
      showStreamBar: true,
      stageLayout: 'focus',
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.getByTestId('stream-bar')).toBeInTheDocument();
  });

  it('hides stream bar when showStreamBar is false even with multiple shares', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1', 'producer-2': 'consumer-2' },
      showStreamBar: false,
      stageLayout: 'focus',
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    expect(screen.queryByTestId('stream-bar')).not.toBeInTheDocument();
  });

  it('does not show stream section when stageLayout is not focus', () => {
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1', 'producer-2': 'consumer-2' },
      showStreamBar: true,
      stageLayout: 'grid',
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    // Stream bar toggle button should not appear
    expect(screen.queryByTitle('Hide stream bar')).not.toBeInTheDocument();
  });

  // ── Section toggle buttons ──

  it('toggles user frame bar visibility via toggle button', () => {
    const mockToggle = vi.fn();
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      showUserFrameBar: true,
      toggleUserFrameBar: mockToggle,
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    fireEvent.click(screen.getByTitle('Hide user frames'));
    expect(mockToggle).toHaveBeenCalled();
  });

  it('toggles stream bar visibility via toggle button', () => {
    const mockToggle = vi.fn();
    setConnectedState({
      tunedInScreenShares: { 'producer-1': 'consumer-1', 'producer-2': 'consumer-2' },
      showStreamBar: true,
      stageLayout: 'focus',
      toggleStreamBar: mockToggle,
    });
    render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);

    fireEvent.click(screen.getByTitle('Hide stream bar'));
    expect(mockToggle).toHaveBeenCalled();
  });

  // ── Resize handle accessibility ──

  it('resize handles are keyboard-accessible with aria-labels', () => {
    setConnectedState({
      showUserFrameBar: true,
      showStreamBar: true,
      showVoiceTextChat: false,
      tunedInScreenShares: { 'share-1': true },
      availableScreenShares: [{ id: 'share-1', peerId: 'peer-1', displayName: 'User' }],
      stageLayout: 'focus',
    });
    const { container } = render(<VoiceView channelId={VOICE_CHANNEL_ID} channelName="General" />);
    // Section handle grips appear in the three-section layout (screen share mode)
    const gripHandles = container.querySelectorAll('.voice-view__section-handle-grip');
    expect(gripHandles.length).toBeGreaterThanOrEqual(1);
    for (const handle of gripHandles) {
      expect(handle).toHaveAttribute('tabindex', '0');
      expect(handle).toHaveAttribute('aria-label');
      expect(handle).not.toHaveAttribute('aria-hidden');
    }
  });
});
