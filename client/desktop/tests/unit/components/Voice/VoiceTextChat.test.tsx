import React from 'react';
import { render, screen, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { mockUser } from '../../../mocks/fixtures';
import { vi } from 'vitest';

// ── Hooks mocks ──────────────────────────────────────────────────────────────
vi.mock('@/renderer/hooks/messaging/useChannelSubscription', () => ({
  useChannelSubscription: vi.fn(),
}));

vi.mock('@/renderer/hooks/messaging/useDMSubscription', () => ({
  useDMSubscription: vi.fn(),
}));

const mockSendMessage = vi.fn();
vi.mock('@/renderer/hooks/messaging/useMessaging', () => ({
  useMessaging: vi.fn(() => ({ sendMessage: mockSendMessage })),
}));

vi.mock('@/renderer/hooks/messaging/useMessageFetch', () => ({
  useMessageFetch: vi.fn(() => ({
    messages: [],
    isLoading: false,
    hasMore: false,
    error: null,
    handleLoadMore: vi.fn(),
  })),
}));

// ── Service mocks ────────────────────────────────────────────────────────────
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    encryptForChannel: vi.fn(),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

// ── Service mocks ───────────────────────────────────────────────────────────
vi.mock('@/renderer/services/messaging/pinService', () => ({
  pinMessage: vi.fn().mockResolvedValue({}),
  unpinMessage: vi.fn().mockResolvedValue({}),
}));

vi.mock('@/renderer/stores/chat/permissionStore', async () => {
  const { create } = await import('zustand');
  const store = create(() => ({
    hasServerPermission: vi.fn().mockReturnValue(true),
    permissions: {},
  }));
  return { usePermissionStore: store };
});

vi.mock('@/renderer/stores/chat/serverStore', async () => {
  const { create } = await import('zustand');
  const store = create(() => ({
    activeServerId: 'server-1',
    servers: [],
    clearServers: vi.fn(),
  }));
  return { useServerStore: store };
});

vi.mock('@/renderer/utils/policy/permissions', () => ({
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

  it('advances the read marker for the linked channel when the list reports the latest seen', async () => {
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: true });
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    useUserStore.setState({ user: { id: 'me', username: 'me' } } as never);
    render(<VoiceTextChat />);
    vi.useFakeTimers();
    try {
      act(() => {
        (capturedMessageListProps.onLatestSeen as () => void)();
      });
      expect(apiFetch).not.toHaveBeenCalledWith(
        expect.stringContaining('/read'),
        expect.anything()
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
      expect(apiFetch).toHaveBeenCalledWith(
        '/api/v1/channels/text-1/read',
        expect.objectContaining({ method: 'POST' })
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('passes the target as the persistence key so the panel lands like the other owners', () => {
    useVoiceStore.setState({ activeChannelId: VOICE_CHANNEL_ID });
    useChannelStore.setState({ channels: [linkedTextChannel] });
    render(<VoiceTextChat />);
    expect(capturedMessageListProps.persistenceKey).toBe(TEXT_CHANNEL_ID);
  });

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
    const { useMessageFetch } = await import('@/renderer/hooks/messaging/useMessageFetch');
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

// ── DM call mode (#1873) ──────────────────────────────────────────────────────

describe('VoiceTextChat — DM call', () => {
  const dmConversation = {
    id: 'dm-1',
    isGroup: false,
    isPersonal: false,
    name: '',
    participants: [
      { userId: 'me', username: 'me' },
      { userId: 'u2', username: 'bob', displayName: 'Bob' },
    ],
  };

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    capturedMessageListProps = {};
    capturedMessageInputProps = {};
    useVoiceStore.setState({
      activeChannelId: 'dm-1',
      isDMCall: true,
      dmConversationId: 'dm-1',
      voiceTextChatLayout: 'horizontal',
    });
    useDMStore.setState({ conversations: [dmConversation] } as never);
    useUserStore.setState({ user: { id: 'me', username: 'me' } } as never);
  });

  it('renders the DM conversation (not the server linked-channel empty state)', () => {
    render(<VoiceTextChat />);
    expect(screen.queryByText('No text channel linked')).not.toBeInTheDocument();
    expect(screen.getByText('Bob Text Chat')).toBeInTheDocument();
  });

  it('passes the DM thread name to MessageList', () => {
    render(<VoiceTextChat />);
    expect(screen.getByTestId('message-list')).toHaveTextContent('Bob');
  });

  it('still exposes the layout toggle in DM mode', () => {
    render(<VoiceTextChat />);
    expect(screen.getByTitle(/Switch to (side|bottom) layout/)).toBeInTheDocument();
  });

  it('flushes the DM read marker at once when the list stops following', async () => {
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: true });
    render(<VoiceTextChat />);
    act(() => {
      (capturedMessageListProps.onLatestSeen as () => void)();
    });
    expect(apiFetch).not.toHaveBeenCalledWith(expect.stringContaining('/read'), expect.anything());
    act(() => {
      (capturedMessageListProps.onLatestLeft as () => void)();
    });
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/dm/conversations/dm-1/read',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('posts the DM open-time read once when the drawer fetch completes, clearing the local count', async () => {
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: true });
    const { useMessageFetch } = await import('@/renderer/hooks/messaging/useMessageFetch');
    useDMStore.setState({ conversations: [{ ...dmConversation, unreadCount: 3 }] } as never);
    render(<VoiceTextChat />);
    const calls = (useMessageFetch as ReturnType<typeof vi.fn>).mock.calls;
    const options = calls[calls.length - 1][1] as { onFetchComplete?: () => void };
    await act(async () => {
      options.onFetchComplete?.();
      options.onFetchComplete?.(); // a refetch: not a second open
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    const reads = (apiFetch as ReturnType<typeof vi.fn>).mock.calls.filter(
      ([url, opts]) =>
        String(url).endsWith('/dm/conversations/dm-1/read') && opts?.method === 'POST'
    );
    expect(reads).toHaveLength(1);
  });

  it('restores the local DM count when the drawer open-time read fails, unless a marker was queued since', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: false, status: 429 });
    const { useMessageFetch } = await import('@/renderer/hooks/messaging/useMessageFetch');
    useDMStore.setState({ conversations: [{ ...dmConversation, unreadCount: 3 }] } as never);
    render(<VoiceTextChat />);
    const calls = (useMessageFetch as ReturnType<typeof vi.fn>).mock.calls;
    const options = calls[calls.length - 1][1] as { onFetchComplete?: () => void };
    await act(async () => {
      options.onFetchComplete?.();
    });
    // The 429 rolled the cleared count back, as DMChatArea does.
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(3);

    // A seen event queued before the failure lands: nothing to put back.
    (apiFetch as ReturnType<typeof vi.fn>).mockImplementation(
      () => new Promise((resolve) => setTimeout(() => resolve({ ok: false, status: 500 }), 10))
    );
    await act(async () => {
      options.onFetchComplete?.(); // retry after the failure reset the once-guard
      (capturedMessageListProps.onLatestSeen as () => void)();
      await new Promise((r) => setTimeout(r, 30));
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    consoleSpy.mockRestore();
  });

  it("rolls a previous target's failed open-time read back even after a marker was queued for the next target", async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    const { useMessageFetch } = await import('@/renderer/hooks/messaging/useMessageFetch');
    let finishFirstRead!: (res: { ok: boolean; status: number }) => void;
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: true });
    (apiFetch as ReturnType<typeof vi.fn>).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finishFirstRead = resolve;
        })
    );
    const other = {
      ...dmConversation,
      id: 'dm-2',
      participants: [
        { userId: 'me', username: 'me' },
        { userId: 'u3', username: 'carol', displayName: 'Carol' },
      ],
    };
    useDMStore.setState({
      conversations: [{ ...dmConversation, unreadCount: 3 }, other],
    } as never);
    render(<VoiceTextChat />);
    const calls = (useMessageFetch as ReturnType<typeof vi.fn>).mock.calls;
    const options = calls[calls.length - 1][1] as { onFetchComplete?: () => void };
    await act(async () => {
      options.onFetchComplete?.(); // dm-1's open-time read goes out and hangs
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);

    // The call moves to another conversation: no mount site keys this panel,
    // so the same instance carries on with the new target.
    await act(async () => {
      useVoiceStore.setState({ activeChannelId: 'dm-2', dmConversationId: 'dm-2' });
    });
    expect(screen.getByText('Carol Text Chat')).toBeInTheDocument();
    await act(async () => {
      (capturedMessageListProps.onLatestSeen as () => void)(); // a marker queued for dm-2
    });
    await act(async () => {
      finishFirstRead({ ok: false, status: 500 }); // dm-1's read fails now
      await Promise.resolve();
    });
    // dm-2's marker is not dm-1's: the cleared count comes back.
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(3);
    consoleSpy.mockRestore();
  });

  it('posts the open-time read again when the call returns to a conversation after another target', async () => {
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: true });
    const { useMessageFetch } = await import('@/renderer/hooks/messaging/useMessageFetch');
    const latestOptions = () => {
      const calls = (useMessageFetch as ReturnType<typeof vi.fn>).mock.calls;
      return calls[calls.length - 1][1] as { onFetchComplete?: () => void };
    };
    const reads = () =>
      (apiFetch as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([path]) => path === '/api/v1/dm/conversations/dm-1/read'
      ).length;
    render(<VoiceTextChat />);
    await act(async () => {
      latestOptions().onFetchComplete?.();
    });
    expect(reads()).toBe(1);

    // A server voice call with no linked channel, then back to the DM call:
    // the same instance, so nothing remounts the panel.
    await act(async () => {
      useVoiceStore.setState({ isDMCall: false, activeChannelId: 'ch-1', dmConversationId: null });
    });
    expect(screen.getByText('No text channel linked')).toBeInTheDocument();
    await act(async () => {
      useVoiceStore.setState({ isDMCall: true, activeChannelId: 'dm-1', dmConversationId: 'dm-1' });
    });
    await act(async () => {
      latestOptions().onFetchComplete?.(); // the return's fetch completes
    });
    expect(reads()).toBe(2);
  });

  it("ignores an older open-time read's late failure once a newer attempt for the same target was posted", async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { apiFetch } = await import('@/renderer/services/system/apiClient');
    const { useMessageFetch } = await import('@/renderer/hooks/messaging/useMessageFetch');
    let finishFirstRead!: (res: { ok: boolean; status: number }) => void;
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({ ok: true });
    (apiFetch as ReturnType<typeof vi.fn>).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finishFirstRead = resolve;
        })
    );
    const latestOptions = () => {
      const calls = (useMessageFetch as ReturnType<typeof vi.fn>).mock.calls;
      return calls[calls.length - 1][1] as { onFetchComplete?: () => void };
    };
    useDMStore.setState({ conversations: [{ ...dmConversation, unreadCount: 3 }] } as never);
    render(<VoiceTextChat />);
    await act(async () => {
      latestOptions().onFetchComplete?.(); // the first read hangs
    });
    // Away and back before it returns: the return posts a second read, which
    // succeeds.
    await act(async () => {
      useVoiceStore.setState({ isDMCall: false, activeChannelId: 'ch-1', dmConversationId: null });
    });
    await act(async () => {
      useVoiceStore.setState({ isDMCall: true, activeChannelId: 'dm-1', dmConversationId: 'dm-1' });
    });
    await act(async () => {
      latestOptions().onFetchComplete?.();
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    // The first read fails late: the newer attempt made the server current,
    // so its stale baseline of 3 must not come back.
    await act(async () => {
      finishFirstRead({ ok: false, status: 500 });
      await Promise.resolve();
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    consoleSpy.mockRestore();
  });

  it('clears the local DM unread count before marking the latest seen', () => {
    useDMStore.setState({ conversations: [{ ...dmConversation, unreadCount: 3 }] } as never);
    render(<VoiceTextChat />);
    act(() => {
      (capturedMessageListProps.onLatestSeen as () => void)();
    });
    expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
  });

  it('shows the DM empty state when the conversation id is missing', () => {
    useVoiceStore.setState({ isDMCall: true, dmConversationId: null });
    render(<VoiceTextChat />);
    expect(screen.getByText('No conversation')).toBeInTheDocument();
  });

  it('renders the composer when DMs are not disabled (default privacy)', () => {
    render(<VoiceTextChat />);
    expect(screen.getByTestId('message-input')).toBeInTheDocument();
    expect(screen.queryByText(/All DMs have been disabled/)).not.toBeInTheDocument();
  });

  it('preserves the DM privacy-disabled behavior (no composer when DMs are off)', () => {
    usePrivacyStore.setState((s) => ({ settings: { ...s.settings, dmPrivacyLevel: 0 } }));
    render(<VoiceTextChat />);
    expect(screen.getByText(/All DMs have been disabled/)).toBeInTheDocument();
    expect(screen.queryByTestId('message-input')).not.toBeInTheDocument();
  });
});
