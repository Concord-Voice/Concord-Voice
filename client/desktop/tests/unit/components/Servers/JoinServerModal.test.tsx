import { render, screen, fireEvent, act } from '../../../test-utils';
import JoinServerModal from '@/renderer/components/Servers/JoinServerModal';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { apiFetch } from '@/renderer/services/apiClient';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

// Mock LoadingSpinner
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: () => <div data-testid="loading-spinner" />,
}));

describe('JoinServerModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();
  const mockApiFetch = vi.mocked(apiFetch);

  const validPreview = {
    server_name: 'Concord Test',
    server_icon: null,
    server_banner: null,
    member_count: 1,
    valid: true,
  };

  const joinedServer = {
    id: 'server-1',
    name: 'Concord Test',
    owner_id: 'owner-1',
    allow_embedded_content: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  };

  async function advancePreviewTimer() {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });
  }

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    mockApiFetch.mockResolvedValue({ ok: false } as Response);
    useServerStore.getState().clearServers();
    useInviteStore.setState({
      invites: {},
      isLoading: false,
      error: null,
      getInviteInfo: vi.fn().mockResolvedValue(null),
      joinServer: vi.fn().mockResolvedValue(null),
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <JoinServerModal isOpen={false} onClose={mockOnClose} onSuccess={mockOnSuccess} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form when open', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Join a Server')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('AbCd1234')).toBeInTheDocument();
  });

  it('shows character count', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('0/8 characters')).toBeInTheDocument();
  });

  it('prefills an initial deep-link invite code', () => {
    render(
      <JoinServerModal
        isOpen={true}
        initialCode="GHJKMNPQ"
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    const input = screen.getByPlaceholderText('AbCd1234') as HTMLInputElement;
    expect(input.value).toBe('GHJKMNPQ');
    expect(screen.getByText('8/8 characters')).toBeInTheDocument();
  });

  it('updates character count as user types', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234');
    fireEvent.change(input, { target: { value: 'abc' } });
    expect(screen.getByText(/3\/8 characters/)).toBeInTheDocument();
  });

  it('strips non-alphanumeric characters', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ab!@#cd12' } });
    expect(input.value).toBe('abcd12');
  });

  it('limits input to 8 characters', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'AbCd12345678' } });
    expect(input.value).toBe('AbCd1234');
  });

  it('Join Server button is disabled when code is incomplete', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Join Server')).toBeDisabled();
  });

  it('calls onClose when Cancel clicked', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows "keep typing" hint when code is partially entered', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234');
    fireEvent.change(input, { target: { value: 'abc' } });
    expect(screen.getByText(/keep typing/)).toBeInTheDocument();
  });

  it('shows a server preview for a valid invite code', async () => {
    const getInviteInfo = vi.fn().mockResolvedValue(validPreview);
    useInviteStore.setState({ getInviteInfo });

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), {
      target: { value: 'ABCDEFGH' },
    });

    expect(screen.getByTestId('loading-spinner')).toBeInTheDocument();

    await advancePreviewTimer();

    expect(getInviteInfo).toHaveBeenCalledWith('ABCDEFGH');
    expect(screen.getByText('Concord Test')).toBeInTheDocument();
    expect(screen.getByText('1 member')).toBeInTheDocument();
    expect(screen.getByText('C')).toBeInTheDocument();
    expect(screen.getByText('Join Server')).toBeEnabled();
  });

  it('shows an invalid invite message when preview returns invalid', async () => {
    useInviteStore.setState({
      getInviteInfo: vi.fn().mockResolvedValue({ ...validPreview, valid: false }),
    });

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), {
      target: { value: 'ABCDEFGH' },
    });

    await advancePreviewTimer();

    expect(
      screen.getByText('This invite is no longer valid (expired, revoked, or used up)')
    ).toBeInTheDocument();
  });

  it('distinguishes friend codes from server invite codes', async () => {
    useInviteStore.setState({ getInviteInfo: vi.fn().mockResolvedValue(null) });
    mockApiFetch.mockResolvedValue({ ok: true } as Response);

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), {
      target: { value: 'ABCDEFGH' },
    });

    await advancePreviewTimer();

    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/friends/codes/ABCDEFGH');
    expect(screen.getByText(/This looks like a friend code/)).toBeInTheDocument();
  });

  it('shows invalid invite when the friend-code fallback rejects', async () => {
    useInviteStore.setState({ getInviteInfo: vi.fn().mockResolvedValue(null) });
    mockApiFetch.mockRejectedValue(new Error('network down'));

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), {
      target: { value: 'ABCDEFGH' },
    });

    await advancePreviewTimer();

    expect(screen.getByText('Invalid invite code')).toBeInTheDocument();
  });

  it('joins a previewed server and reports success', async () => {
    const joinServer = vi.fn().mockResolvedValue({ server: joinedServer, role: 'member' });
    useInviteStore.setState({
      getInviteInfo: vi.fn().mockResolvedValue(validPreview),
      joinServer,
    });

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), {
      target: { value: 'ABCDEFGH' },
    });
    await advancePreviewTimer();

    await act(async () => {
      fireEvent.click(screen.getByText('Join Server'));
    });

    expect(joinServer).toHaveBeenCalledWith('ABCDEFGH');
    expect(screen.getByText('Joined Concord Test!')).toBeInTheDocument();
    // The serverStore write moved into inviteStore.joinServer (#2363), and this
    // test mocks joinServer out entirely (line 204) — so asserting the write here
    // would be asserting a mocked collaborator's side effect rather than
    // JoinServerModal's own contract. The coverage did not disappear, it moved and
    // got stronger: inviteStore.test.ts T3a/T3b assert the write against the REAL
    // action, and InviteEmbed.test.tsx T3d asserts it through the real UI path.
    // What this test still owns is below: onSuccess receives the shaped server.
    // ...and that the modal does NOT write the store itself. joinServer is mocked
    // (line 204), so the modal is the only possible writer here — an empty store
    // is the exact inverse of the deleted assertion, and it guards the deletion:
    // re-adding useServerStore.getState().addServer(...) to handleJoin reds this.
    // Asserted as store state rather than a spy so it reads as a layering
    // contract, not a call-graph fact. regression for #2363.
    expect(useServerStore.getState().servers).toEqual([]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(800);
    });

    expect(mockOnSuccess).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'server-1', role: 'member' })
    );
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows the invite-store error when join fails', async () => {
    useInviteStore.setState({
      getInviteInfo: vi.fn().mockResolvedValue(validPreview),
      joinServer: vi.fn().mockImplementation(async () => {
        useInviteStore.setState({ error: 'Invite already used' });
        return null;
      }),
    });

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), {
      target: { value: 'ABCDEFGH' },
    });
    await advancePreviewTimer();

    await act(async () => {
      fireEvent.click(screen.getByText('Join Server'));
    });

    expect(screen.getByText('Invite already used')).toBeInTheDocument();
    expect(screen.getByText('Join Server')).toBeEnabled();
    expect(mockOnSuccess).not.toHaveBeenCalled();
  });
});
