import React from 'react';
import { render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { mockUser } from '../../../mocks/fixtures';
import { vi } from 'vitest';

// ── Hooks mocks ──────────────────────────────────────────────────────────────
vi.mock('@/renderer/hooks/useChannelSubscription', () => ({
  useChannelSubscription: vi.fn(),
}));

const mockSendMessage = vi.fn();
vi.mock('@/renderer/hooks/useMessaging', () => ({
  useMessaging: vi.fn(() => ({ sendMessage: mockSendMessage })),
}));

vi.mock('@/renderer/hooks/useMessageFetch', () => ({
  useMessageFetch: vi.fn(() => ({
    messages: [],
    isLoading: false,
    hasMore: false,
    error: null,
    handleLoadMore: vi.fn(),
  })),
}));

// ── Service mocks ────────────────────────────────────────────────────────────
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    encryptForChannel: vi.fn(),
  },
}));

// ── Service mocks ───────────────────────────────────────────────────────────
vi.mock('@/renderer/services/pinService', () => ({
  pinMessage: vi.fn().mockResolvedValue({}),
  unpinMessage: vi.fn().mockResolvedValue({}),
}));

vi.mock('@/renderer/stores/permissionStore', async () => {
  const { create } = await import('zustand');
  const store = create(() => ({
    hasServerPermission: vi.fn().mockReturnValue(true),
    permissions: {},
  }));
  return { usePermissionStore: store };
});

vi.mock('@/renderer/stores/serverStore', async () => {
  const { create } = await import('zustand');
  const store = create(() => ({
    activeServerId: 'server-1',
    servers: [],
    clearServers: vi.fn(),
  }));
  return { useServerStore: store };
});

vi.mock('@/renderer/utils/permissions', () => ({
  PIN_MESSAGES: 'pin_messages',
}));

// ── Child component mocks ────────────────────────────────────────────────────
let capturedMessageListProps: Record<string, unknown> = {};
vi.mock('@/renderer/components/Chat/MessageList', () => ({
  default: (props: Record<string, unknown>) => {
    capturedMessageListProps = props;
    return <div data-testid="message-list">{props.channelName as string}</div>;
  },
}));

let capturedMessageInputProps: Record<string, unknown> = {};
vi.mock('@/renderer/components/Chat/MessageInput', () => ({
  default: (props: {
    onSendMessage: (content: string, mentionMeta?: string, replyToId?: string) => void;
    placeholder: string;
    disabled: boolean;
    replyingTo?: unknown;
    onCancelReply?: () => void;
  }) => {
    capturedMessageInputProps = props;
    return (
      <div
        data-testid="message-input"
        data-placeholder={props.placeholder}
        data-disabled={props.disabled}
      >
        <button onClick={() => props.onSendMessage('test msg')}>Send</button>
      </div>
    );
  },
}));

// ── CSS mock ─────────────────────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/VoiceTextChat.css', () => ({}));

import VoiceTextChat from '@/renderer/components/Voice/VoiceTextChat';

// ── Helpers ──────────────────────────────────────────────────────────────────
const VOICE_CHANNEL_ID = 'voice-1';
const TEXT_CHANNEL_ID = 'text-1';

const linkedTextChannel = {
  id: TEXT_CHANNEL_ID,
  server_id: 's1',
  name: 'voice-chat',
  type: 'text' as const,
  position: 0,
  linked_voice_channel_id: VOICE_CHANNEL_ID,
  created_at: '',
  updated_at: '',
};

describe('VoiceTextChat', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    capturedMessageListProps = {};
    capturedMessageInputProps = {};
    useVoiceStore.setState({
      activeChannelId: null,
      voiceTextChatLayout: 'horizontal',
    });
    useChannelStore.setState({ channels: [] });
    useUserStore.setState({ user: null });
  });

  // ── Empty state ──────────────────────────────────────────────────────────

  it('shows empty state when no voice channel is active', () => {
    useVoiceStore.setState({ activeChannelId: null });
    render(<VoiceTextChat />);
    expect(screen.getByText('No text channel linked')).toBeInTheDocument();
  });

  it('shows empty state when voice channel has no linked text channel', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [] });
    render(<VoiceTextChat />);
    expect(screen.getByText('No text channel linked')).toBeInTheDocument();
  });

  // ── Active state ─────────────────────────────────────────────────────────

  it('renders header with linked channel name', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(screen.getByText('voice-chat Text Chat')).toBeInTheDocument();
  });

  it('renders MessageList with correct channelName', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(screen.getByTestId('message-list')).toHaveTextContent('voice-chat');
  });

  it('renders MessageInput with correct placeholder', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(screen.getByTestId('message-input')).toHaveAttribute(
      'data-placeholder',
      'Message voice-chat text chat...'
    );
  });

  it('disables MessageInput when no user is logged in', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    useUserStore.setState({ user: null });
    render(<VoiceTextChat />);
    expect(screen.getByTestId('message-input')).toHaveAttribute('data-disabled', 'true');
  });

  it('enables MessageInput when user is logged in', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    useUserStore.setState({
      user: {
        id: 'user-1',
        username: 'alice',
        display_name: 'Alice',
        email: 'alice@test.com',
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
    render(<VoiceTextChat />);
    expect(screen.getByTestId('message-input')).toHaveAttribute('data-disabled', 'false');
  });

  // ── Layout toggle ────────────────────────────────────────────────────────

  it('renders layout toggle button', () => {
    useVoiceStore.setState({
      activeChannelId: VOICE_CHANNEL_ID,
      voiceTextChatLayout: 'horizontal',
    });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(screen.getByTitle('Switch to side layout')).toBeInTheDocument();
  });

  it('shows bottom layout title when in vertical mode', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID, voiceTextChatLayout: 'vertical' });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(screen.getByTitle('Switch to bottom layout')).toBeInTheDocument();
  });

  // ── Error display ────────────────────────────────────────────────────────

  it('displays error when useMessageFetch returns error', async () => {
    const { useMessageFetch } = await import('@/renderer/hooks/useMessageFetch');
    (useMessageFetch as ReturnType<typeof vi.fn>).mockReturnValue({
      messages: [],
      isLoading: false,
      hasMore: false,
      error: 'Failed to load messages',
      handleLoadMore: vi.fn(),
    });

    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(screen.getByText('Failed to load messages')).toBeInTheDocument();
  });

  it('calls sendMessage with opts pattern when user sends a message', async () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    useUserStore.setState({ user: mockUser });

    render(<VoiceTextChat />);
    const sendBtn = screen.getByText('Send');
    const user = (await import('@testing-library/user-event')).default.setup();
    await user.click(sendBtn);

    expect(mockSendMessage).toHaveBeenCalledWith(
      TEXT_CHANNEL_ID,
      'test msg',
      mockUser.username,
      expect.objectContaining({
        avatarUrl: mockUser.avatar_url,
        displayName: mockUser.display_name,
      })
    );
  });

  // ── Reply support ─────────────────────────────────────────────────────────

  it('passes onReply to MessageList', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(capturedMessageListProps.onReply).toBeInstanceOf(Function);
  });

  it('passes replyingTo and onCancelReply to MessageInput', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(capturedMessageInputProps.replyingTo).toBeNull();
    expect(capturedMessageInputProps.onCancelReply).toBeInstanceOf(Function);
  });

  // ── Pin support ───────────────────────────────────────────────────────────

  it('passes onPinToggle and canPin to MessageList', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(capturedMessageListProps.onPinToggle).toBeInstanceOf(Function);
    expect(capturedMessageListProps.canPin).toBeDefined();
  });
});
