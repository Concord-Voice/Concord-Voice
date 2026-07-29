import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { mockServer, mockChannel, mockUser } from '../../../mocks/fixtures';

// Mock apiFetch to avoid timing issues in jsdom
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    createChannelKeys: vi.fn(),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

// Mock emoji picker
vi.mock('emoji-picker-react', () => ({
  default: () => <div data-testid="emoji-picker" />,
}));

import { apiFetch } from '@/renderer/services/apiClient';
import { e2eeService } from '@/renderer/services/e2eeService';
import CreateChannelModal from '@/renderer/components/Channels/CreateChannelModal';

const mockedApiFetch = vi.mocked(apiFetch);

function setOverflowMembersAndKeys(count = 501): {
  members: Array<{ user_id: string; public_key: string; key_version: number }>;
  wrappedKeys: Map<string, string>;
} {
  const overflowCount = count - 1;
  const overflowMembers = Array.from({ length: overflowCount }, (_, index) => ({
    user_id: `user-${index + 2}`,
    public_key: `mock-public-key-${index + 2}`,
    key_version: index + 2,
  }));

  const wrappedKeys = new Map<string, string>();
  for (const member of overflowMembers) {
    wrappedKeys.set(member.user_id, `wrapped-${member.user_id}`);
  }
  wrappedKeys.set(mockUser.id, 'wrapped-user-1');
  return {
    members: [
      ...overflowMembers,
      { user_id: mockUser.id, public_key: 'mock-public-key-1', key_version: 1 },
    ],
    wrappedKeys,
  };
}

const currentMemberPublicKeys = [
  { user_id: mockUser.id, public_key: 'mock-public-key-1', key_version: 1 },
];

describe('CreateChannelModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    useUserStore.getState().setUser(mockUser);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <CreateChannelModal isOpen={false} onClose={mockOnClose} onSuccess={mockOnSuccess} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form when open', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByPlaceholderText('general-chat')).toBeInTheDocument();
    expect(screen.getByText('Text')).toBeInTheDocument();
    expect(screen.getByText('Voice')).toBeInTheDocument();
    expect(screen.getByText('Bulletin')).toBeInTheDocument();
  });

  it('shows validation error for empty name', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Channel name is required')).toBeInTheDocument();
  });

  it('shows validation error for short name', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'ab' },
    });
    fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Channel name must be at least 3 characters')).toBeInTheDocument();
  });

  it('submits valid form', async () => {
    vi.useFakeTimers();

    // All channels are always E2EE — set up member + public key + createChannelKeys mocks
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ members: currentMemberPublicKeys }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ channel: { ...mockChannel, name: 'new-channel' } }),
      } as Response);

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'new-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Channel created successfully!')).toBeInTheDocument();
    expect(useChannelStore.getState().channels).toHaveLength(1);

    vi.useRealTimers();
  });

  it('keeps the initial channel creation open until it is recorded locally', async () => {
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    let resolveCreate!: (response: Response) => void;
    let createSignal: AbortSignal | null = null;
    const createResponse = new Promise<Response>((resolve) => {
      resolveCreate = resolve;
    });
    mockedApiFetch.mockImplementation(async (url, init) => {
      if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
        return { ok: true, json: async () => ({ members: currentMemberPublicKeys }) } as Response;
      }
      if (url === '/api/v1/channels') {
        createSignal = init?.signal ?? null;
        return createResponse;
      }
      throw new Error(`Unexpected request: ${url}`);
    });

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'pending-channel' },
    });
    fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

    await waitFor(() => expect(createSignal).toBeInstanceOf(AbortSignal));
    const cancel = screen.getByRole('button', { name: 'Cancel' });
    expect(cancel).toBeDisabled();
    fireEvent.click(cancel);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(createSignal?.aborted).toBe(false);
    expect(mockOnClose).not.toHaveBeenCalled();

    resolveCreate({ ok: true, json: async () => ({ channel: mockChannel }) } as Response);
    await waitFor(() => expect(useChannelStore.getState().channels).toHaveLength(1));
  });

  it('batches overflow keys to both voice channel IDs while retaining the creator', async () => {
    const { members, wrappedKeys } = setOverflowMembersAndKeys();
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);

    const voiceChannel = { ...mockChannel, id: 'voice-channel', type: 'voice' as const };
    const linkedTextChannel = { ...mockChannel, id: 'linked-text-channel', type: 'text' as const };
    mockedApiFetch.mockImplementation(async (url) => {
      if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
        return { ok: true, json: async () => ({ members }) } as Response;
      }
      if (url === '/api/v1/channels') {
        return {
          ok: true,
          json: async () => ({ channel: voiceChannel, linked_text_channel: linkedTextChannel }),
        } as Response;
      }
      return { ok: true, json: async () => ({}) } as Response;
    });

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'voice-overflow' },
    });
    fireEvent.click(screen.getByText('Voice').closest('button')!);
    fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

    await waitFor(() => {
      expect(mockedApiFetch).toHaveBeenCalledWith(
        '/api/v1/channels/voice-channel/keys',
        expect.anything()
      );
    });

    const createCall = mockedApiFetch.mock.calls.find(([url]) => url === '/api/v1/channels');
    const createBody = JSON.parse((createCall?.[1]?.body as string) ?? '{}');
    expect(Object.keys(createBody.wrapped_keys)).toHaveLength(500);
    expect(createBody.wrapped_keys).toHaveProperty(mockUser.id, 'wrapped-user-1');
    expect(createBody.wrapped_key_versions).toHaveProperty(mockUser.id, 1);
    expect(e2eeService.createChannelKeys).toHaveBeenCalledTimes(1);

    const distributionCalls = mockedApiFetch.mock.calls.filter(
      ([url]) => typeof url === 'string' && url.endsWith('/keys')
    );
    expect(distributionCalls).toHaveLength(2);
    expect(distributionCalls.map(([url]) => url)).toEqual([
      '/api/v1/channels/voice-channel/keys',
      '/api/v1/channels/linked-text-channel/keys',
    ]);
    for (const [, request] of distributionCalls) {
      expect(JSON.parse((request?.body as string) ?? '{}')).toEqual({
        wrapped_keys: { 'user-501': 'wrapped-user-501' },
        wrapped_key_versions: { 'user-501': 501 },
        key_version: 1,
      });
    }
  });

  it('reports a failed overflow distribution as a partial channel creation', async () => {
    const { members, wrappedKeys } = setOverflowMembersAndKeys();
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);
    mockedApiFetch.mockImplementation(async (url) => {
      if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
        return { ok: true, json: async () => ({ members }) } as Response;
      }
      if (url === '/api/v1/channels') {
        return { ok: true, json: async () => ({ channel: mockChannel }) } as Response;
      }
      return {
        ok: false,
        json: async () => ({ error: 'Key distribution unavailable' }),
      } as Response;
    });

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'text-overflow' },
    });
    fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

    await waitFor(() => {
      expect(
        screen.getByText('Channel created, but Key distribution unavailable')
      ).toBeInTheDocument();
    });
    expect(screen.queryByText('Channel created successfully!')).not.toBeInTheDocument();
    expect(useChannelStore.getState().channels).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Create Channel' })).toBeDisabled();
  });

  it('retries the 11th overflow batch after rate limiting', async () => {
    vi.useFakeTimers();
    try {
      const { members, wrappedKeys } = setOverflowMembersAndKeys(5501);
      vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);
      let distributionAttempts = 0;
      mockedApiFetch.mockImplementation(async (url) => {
        if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
          return { ok: true, json: async () => ({ members }) } as Response;
        }
        if (url === '/api/v1/channels') {
          return { ok: true, json: async () => ({ channel: mockChannel }) } as Response;
        }
        distributionAttempts++;
        if (distributionAttempts === 11) {
          return {
            ok: false,
            status: 429,
            headers: new Headers({
              'Retry-After': '1',
            }),
            json: async () => ({}),
          } as Response;
        }
        return { ok: true, json: async () => ({}) } as Response;
      });

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'rate-limited-overflow' },
      });
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(distributionAttempts).toBe(11);

      await act(async () => {
        await vi.runAllTimersAsync();
      });
      expect(distributionAttempts).toBe(12);
      expect(useChannelStore.getState().channels).toHaveLength(1);
      expect(screen.queryByText(/Channel created, but/)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('caps an overflow Retry-After delay at one minute', async () => {
    vi.useFakeTimers();
    try {
      const { members, wrappedKeys } = setOverflowMembersAndKeys();
      vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);
      let distributionAttempts = 0;
      mockedApiFetch.mockImplementation(async (url) => {
        if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
          return { ok: true, json: async () => ({ members }) } as Response;
        }
        if (url === '/api/v1/channels') {
          return { ok: true, json: async () => ({ channel: mockChannel }) } as Response;
        }
        distributionAttempts++;
        if (distributionAttempts === 1) {
          return {
            ok: false,
            status: 429,
            headers: new Headers({ 'Retry-After': '3600' }),
            json: async () => ({}),
          } as Response;
        }
        return { ok: true, json: async () => ({}) } as Response;
      });

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'capped-rate-limit-overflow' },
      });
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(distributionAttempts).toBe(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(59_999);
      });
      expect(distributionAttempts).toBe(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(1);
      });
      expect(distributionAttempts).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops after bounded retries when overflow key distribution remains rate limited', async () => {
    vi.useFakeTimers();
    try {
      const { members, wrappedKeys } = setOverflowMembersAndKeys();
      vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);
      let distributionAttempts = 0;
      mockedApiFetch.mockImplementation(async (url) => {
        if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
          return { ok: true, json: async () => ({ members }) } as Response;
        }
        if (url === '/api/v1/channels') {
          return { ok: true, json: async () => ({ channel: mockChannel }) } as Response;
        }
        distributionAttempts++;
        return {
          ok: false,
          status: 429,
          headers: new Headers(),
          json: async () => ({}),
        } as Response;
      });

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'persistently-rate-limited-overflow' },
      });
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

      await act(async () => {
        await vi.runAllTimersAsync();
      });

      expect(distributionAttempts).toBe(4);
      expect(
        screen.getByText(
          'Channel created, but Channel key distribution is rate limited; try again later'
        )
      ).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // Regression for #2532: users must be able to leave a rate-limited overflow retry.
  it('cancels a rate-limited overflow distribution during its backoff', async () => {
    vi.useFakeTimers();
    try {
      const { members, wrappedKeys } = setOverflowMembersAndKeys();
      vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);
      let distributionAttempts = 0;
      let distributionSignal: AbortSignal | null | undefined;
      mockedApiFetch.mockImplementation(async (url, init) => {
        if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
          return { ok: true, json: async () => ({ members }) } as Response;
        }
        if (url === '/api/v1/channels') {
          return { ok: true, json: async () => ({ channel: mockChannel }) } as Response;
        }
        distributionAttempts++;
        distributionSignal = init?.signal;
        return {
          ok: false,
          status: 429,
          headers: new Headers({ 'Retry-After': '60' }),
          json: async () => ({}),
        } as Response;
      });

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'cancel-rate-limited-overflow' },
      });
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(distributionAttempts).toBe(1);

      const cancel = screen.getByRole('button', { name: 'Cancel' });
      expect(cancel).toBeEnabled();
      fireEvent.click(cancel);

      expect(distributionSignal?.aborted).toBe(true);
      expect(mockOnClose).toHaveBeenCalledTimes(1);

      await act(async () => {
        await vi.runAllTimersAsync();
      });
      expect(distributionAttempts).toBe(1);
      expect(useChannelStore.getState().channels).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('cancels a scheduled success close without closing twice', async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
        new Map([['user-1', 'wrapped-key']])
      );
      mockedApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ members: currentMemberPublicKeys }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ channel: mockChannel }),
        } as Response);

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'cancel-scheduled-close' },
      });
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

      expect(mockOnClose).toHaveBeenCalledTimes(1);
      await act(async () => {
        await vi.runAllTimersAsync();
      });
      expect(mockOnClose).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not retry an overflow key distribution after auth supersession', async () => {
    vi.useFakeTimers();
    try {
      const { members, wrappedKeys } = setOverflowMembersAndKeys();
      vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeys);
      let distributionAttempts = 0;
      mockedApiFetch.mockImplementation(async (url) => {
        if (url === `/api/v1/servers/${mockServer.id}/member-public-keys`) {
          return { ok: true, json: async () => ({ members }) } as Response;
        }
        if (url === '/api/v1/channels') {
          return { ok: true, json: async () => ({ channel: mockChannel }) } as Response;
        }
        distributionAttempts++;
        return {
          ok: false,
          status: 429,
          headers: new Headers({
            'X-RateLimit-Reset': String(Math.ceil((Date.now() + 1000) / 1000)),
          }),
          json: async () => ({}),
        } as Response;
      });

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'superseded-overflow' },
      });
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(distributionAttempts).toBe(1);

      act(() => {
        useAuthStore.getState().setAccessToken('successor-token');
      });
      await act(async () => {
        await vi.runAllTimersAsync();
      });

      expect(distributionAttempts).toBe(1);
      expect(screen.queryByText('Channel created successfully!')).not.toBeInTheDocument();
      expect(
        screen.getByText('Create Channel', { selector: 'button[type="submit"]' })
      ).toBeDisabled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows error on API failure', async () => {
    // Public key fetch succeeds; channel creation fails
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ members: currentMemberPublicKeys }),
      } as Response)
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Duplicate name' }),
      } as Response);

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'new-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Duplicate name')).toBeInTheDocument();
    });
  });

  it('shows the generic error when channel creation returns non-JSON', async () => {
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ members: currentMemberPublicKeys }),
      } as Response)
      .mockResolvedValueOnce({
        ok: false,
        json: async () => {
          throw new SyntaxError('Unexpected token');
        },
      } as Response);

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'non-json-error' },
    });
    fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));

    await waitFor(() => {
      expect(screen.getByText('Failed to create channel')).toBeInTheDocument();
    });
  });

  it('always creates E2EE channels (no encryption toggle)', () => {
    // The encryption toggle was removed — all channels are always E2EE
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.queryByText('Encryption Disabled')).not.toBeInTheDocument();
    expect(screen.queryByText('Encryption Enabled')).not.toBeInTheDocument();
  });

  it('selects voice channel type', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const voiceBtn = screen.getByText('Voice').closest('button');
    fireEvent.click(voiceBtn!);
    expect(voiceBtn).toHaveClass('selected');
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows character count', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('0/100 characters')).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'Test' },
    });
    expect(screen.getByText('4/100 characters')).toBeInTheDocument();
  });

  it('submits channel with wrapped E2EE keys (always encrypted)', async () => {
    vi.useFakeTimers();

    // All channels are E2EE — set up member + public key + key gen mocks

    const wrappedKeyMap = new Map([['user-1', 'wrapped-key-data']]);
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeyMap);

    // First call: public key fetch; second call: channel creation
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ members: currentMemberPublicKeys }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          channel: { ...mockChannel, name: 'secret-channel' },
        }),
      } as Response);

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    // Enter channel name
    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'secret-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(e2eeService.createChannelKeys).toHaveBeenCalled();
    expect(screen.getByText('Channel created successfully!')).toBeInTheDocument();

    vi.useRealTimers();
  });

  it('shows error when E2EE is not initialized', async () => {
    // Temporarily set isInitialized to false — submission always tries E2EE key gen
    const original = e2eeService.isInitialized;
    Object.defineProperty(e2eeService, 'isInitialized', { value: false, writable: true });

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'encrypted-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(
        screen.getByText('Setting up secure messaging — try again in a moment.')
      ).toBeInTheDocument();
    });

    // Restore
    Object.defineProperty(e2eeService, 'isInitialized', { value: original, writable: true });
  });

  it('shows error when no member public keys are available', async () => {
    // No current member has a public key.
    mockedApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ members: [] }),
    } as Response);

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'encrypted-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(
        screen.getByText('No member public keys available for E2EE channel creation')
      ).toBeInTheDocument();
    });
  });

  it('renders channel group selector when groups exist', () => {
    useChannelStore.setState({
      channelGroups: [{ id: 'group-1', name: 'General', server_id: 'server-1', position: 0 }],
    });

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Channel Group')).toBeInTheDocument();
  });

  it('calls onSuccess callback after successful creation', async () => {
    vi.useFakeTimers();

    // All channels are E2EE — need member + public key + key gen mocks
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    const createdChannel = { ...mockChannel, name: 'callback-channel' };
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ members: currentMemberPublicKeys }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ channel: createdChannel }),
      } as Response);

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'callback-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(mockOnSuccess).toHaveBeenCalledWith(createdChannel);

    vi.useRealTimers();
  });

  it('handles non-Error thrown exceptions', async () => {
    // Public key fetch succeeds; createChannelKeys throws a non-Error (string)
    mockedApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ members: currentMemberPublicKeys }),
    } as Response);
    vi.mocked(e2eeService.createChannelKeys).mockRejectedValue('string-error');

    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('general-chat'), {
      target: { value: 'new-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Failed to create channel')).toBeInTheDocument();
    });
  });

  it('selects bulletin channel type', () => {
    render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const bulletinBtn = screen.getByText('Bulletin').closest('button');
    fireEvent.click(bulletinBtn!);
    expect(bulletinBtn).toHaveClass('selected');
  });

  describe('defect #2 banner text (#1023)', () => {
    it('shows the new init-not-ready banner instead of the legacy log-out string', async () => {
      // Force e2eeService.isInitialized to false — module mock has it as a writable
      // property, so override via Object.defineProperty matching the existing pattern.
      const original = e2eeService.isInitialized;
      Object.defineProperty(e2eeService, 'isInitialized', { value: false, writable: true });

      render(<CreateChannelModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

      fireEvent.change(screen.getByPlaceholderText('general-chat'), {
        target: { value: 'test-channel' },
      });

      await act(async () => {
        fireEvent.click(screen.getByText('Create Channel', { selector: 'button[type="submit"]' }));
      });

      // Wait for the form-error banner to surface
      const banner = await screen.findByText(/Setting up secure messaging/i);
      expect(banner).toBeInTheDocument();

      // The legacy string MUST NOT appear
      expect(screen.queryByText(/log out and log back in/i)).not.toBeInTheDocument();

      // Restore
      Object.defineProperty(e2eeService, 'isInitialized', { value: original, writable: true });
    });
  });
});
