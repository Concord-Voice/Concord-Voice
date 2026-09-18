import { render, screen, fireEvent, act } from '../../../test-utils';
import JoinServerModal from '@/renderer/components/Servers/JoinServerModal';
import { useInviteStore } from '@/renderer/stores/chat/inviteStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { apiFetch } from '@/renderer/services/system/apiClient';

vi.mock('@/renderer/services/system/apiClient', () => ({
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
      joinServer: vi.fn().mockResolvedValue({ status: 'failed', reason: 'Failed to join server' }),
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
    const joinServer = vi
      .fn()
      .mockResolvedValue({ status: 'joined', response: { server: joinedServer, role: 'member' } });
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

  // The reason now travels back on the OUTCOME. The shared `error` field is
  // deliberately set to something DIFFERENT here: if the modal ever goes back to
  // reading the store after its await, this test fails instead of passing on a
  // value that happens to match (Gitar, PR #3353).
  it('shows the reason the join returned, not the shared store field', async () => {
    useInviteStore.setState({
      getInviteInfo: vi.fn().mockResolvedValue(validPreview),
      joinServer: vi.fn().mockImplementation(async () => {
        useInviteStore.setState({ error: 'a DIFFERENT invite failed' });
        return { status: 'failed', reason: 'Invite already used' };
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
  // --- #2372 defect 3: already a member ---
  //
  // This modal is where `concord://invite/<code>` deep links land (App.tsx feeds
  // them in as `initialCode`), so these cases cover the reporter's "shouldn't be
  // allowed to join a server they're already a member of ... via the app:// link"
  // half as well as manual entry.

  const memberPreview = { ...validPreview, server_id: 'server-1' };

  async function openWithPreview(preview: unknown, props = {}) {
    useInviteStore.setState({ getInviteInfo: vi.fn().mockResolvedValue(preview) });
    render(
      <JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} {...props} />
    );
    fireEvent.change(screen.getByPlaceholderText('AbCd1234'), { target: { value: 'ABCDEFGH' } });
    await advancePreviewTimer();
  }

  it('refuses a code for a server the user is already in', async () => {
    useServerStore.setState({ servers: [joinedServer] } as never);
    await openWithPreview(memberPreview);

    expect(screen.getByText(/already a member of Concord Test/i)).toBeInTheDocument();
    expect(screen.getByText('Join Server')).toBeDisabled();
  });

  // The deep-link path, which is the half the reporter described. Same guard,
  // reached by `initialCode` instead of typing.
  it('refuses a deep-linked invite for a server the user is already in', async () => {
    useServerStore.setState({ servers: [joinedServer] } as never);
    useInviteStore.setState({ getInviteInfo: vi.fn().mockResolvedValue(memberPreview) });

    render(
      <JoinServerModal
        isOpen={true}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
        initialCode="ABCDEFGH"
      />
    );
    await advancePreviewTimer();

    expect(screen.getByText(/already a member of Concord Test/i)).toBeInTheDocument();
    expect(screen.getByText('Join Server')).toBeDisabled();
  });

  // The discriminator: identical fixture, membership list empty. Without it the
  // pair above would pass against a component that disabled Join whenever a
  // `server_id` was present at all.
  it('still offers Join for a server the user is not in', async () => {
    useServerStore.getState().clearServers();
    await openWithPreview(memberPreview);

    expect(screen.queryByText(/already a member/i)).not.toBeInTheDocument();
    expect(screen.getByText('Join Server')).toBeEnabled();
  });

  // "Cannot tell" (a control plane predating #2372) must not read as "member" —
  // that would disable Join on every invite against an older self-hosted server.
  // The membership list is deliberately seeded so the case cannot pass merely
  // because there was nothing to match against.
  it('offers Join when the preview carries no server_id', async () => {
    useServerStore.setState({ servers: [joinedServer] } as never);
    await openWithPreview(validPreview);

    expect(screen.queryByText(/already a member/i)).not.toBeInTheDocument();
    expect(screen.getByText('Join Server')).toBeEnabled();
  });
  // A superseded lookup must write NOTHING. `getInviteInfo` is a plain await with
  // no cancellation, so clearing the debounce timer only stops a request that has
  // not started yet — the generation fence is the only thing standing between a
  // slow first lookup and the preview the user is actually looking at. This PR
  // raised the stakes: `alreadyMember` is derived from `preview.server_id`, so a
  // stale row aims the membership guard at the wrong server (CodeRabbit, #3353).
  it('ignores a preview for a code the user has already replaced', async () => {
    const resolvers: Array<(info: unknown) => void> = [];
    const getInviteInfo = vi
      .fn()
      .mockImplementation(() => new Promise((resolve) => resolvers.push(resolve)));
    useInviteStore.setState({ getInviteInfo });

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234');

    fireEvent.change(input, { target: { value: 'AAAAAAAA' } });
    await advancePreviewTimer();
    expect(getInviteInfo).toHaveBeenCalledWith('AAAAAAAA');

    // Edited away, then a different complete code typed.
    fireEvent.change(input, { target: { value: 'AAAAAAA' } });
    fireEvent.change(input, { target: { value: 'BBBBBBBB' } });
    await advancePreviewTimer();
    expect(getInviteInfo).toHaveBeenCalledWith('BBBBBBBB');

    // The SECOND request answers first; the abandoned first one lands after it.
    await act(async () => {
      resolvers[1]({ ...validPreview, server_name: 'Second Server' });
      await vi.advanceTimersByTimeAsync(0);
    });
    await act(async () => {
      resolvers[0]({ ...validPreview, server_name: 'First Server' });
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Second Server')).toBeInTheDocument();
    expect(screen.queryByText('First Server')).not.toBeInTheDocument();
  });

  // The fence removes the only thing that used to stop the spinner on this path —
  // the superseded request's own continuation, which cleared it on its way past.
  // Without the accompanying else-branch clear the modal sits on "Looking up
  // invite..." forever after a single backspace.
  it('stops the lookup spinner when the code is edited back below full length', async () => {
    let resolveLookup: ((info: unknown) => void) | undefined;
    const getInviteInfo = vi
      .fn()
      .mockImplementation(() => new Promise((resolve) => (resolveLookup = resolve)));
    useInviteStore.setState({ getInviteInfo });

    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234');

    fireEvent.change(input, { target: { value: 'ABCDEFGH' } });
    await advancePreviewTimer();
    expect(screen.getByText('Looking up invite...')).toBeInTheDocument();

    fireEvent.change(input, { target: { value: 'ABCDEFG' } });
    expect(screen.queryByText('Looking up invite...')).not.toBeInTheDocument();

    // The abandoned request landing afterwards must revive neither the spinner
    // nor the preview it was fetching.
    await act(async () => {
      resolveLookup?.(validPreview);
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByText('Looking up invite...')).not.toBeInTheDocument();
    expect(screen.queryByText('Concord Test')).not.toBeInTheDocument();
  });
});
