import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { mockServer, mockChannel, mockMember } from '../../../mocks/fixtures';

// Mock apiFetch to avoid timing issues in jsdom
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    createChannelKeys: vi.fn(),
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

describe('CreateChannelModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
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
    useMemberStore.setState({ members: [mockMember] });
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'mock-pub-key' }),
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

  it('shows error on API failure', async () => {
    // Public key fetch succeeds; channel creation fails
    useMemberStore.setState({ members: [mockMember] });
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'mock-pub-key' }),
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
    useMemberStore.setState({ members: [mockMember] });

    const wrappedKeyMap = new Map([['user-1', 'wrapped-key-data']]);
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(wrappedKeyMap);

    // First call: public key fetch; second call: channel creation
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'mock-public-key' }),
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

    useMemberStore.setState({ members: [mockMember] });

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
    useMemberStore.setState({ members: [mockMember] });

    // Public key fetch fails — no wrapped keys can be generated
    mockedApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({}),
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
    useMemberStore.setState({ members: [mockMember] });
    vi.mocked(e2eeService.createChannelKeys).mockResolvedValue(
      new Map([['user-1', 'wrapped-key']])
    );
    const createdChannel = { ...mockChannel, name: 'callback-channel' };
    mockedApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'mock-pub-key' }),
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
    // All channels are E2EE — public key fetch rejects with a string (non-Error)
    // Public key fetch succeeds; createChannelKeys throws a non-Error (string)
    useMemberStore.setState({ members: [mockMember] });
    mockedApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ public_key: 'mock-pub-key' }),
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

      useMemberStore.setState({ members: [mockMember] });

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
