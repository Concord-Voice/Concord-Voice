import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import { useFriendStore, type Friend } from '@/renderer/stores/friendStore';
import { SendToFriendModal } from '@/renderer/components/Channels/SendToFriendModal';

const mockNavigate = vi.fn();
vi.mock('react-router-dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('react-router-dom')>()),
  useNavigate: () => mockNavigate,
}));

const mockSend = vi.fn();
vi.mock('@/renderer/hooks/useSendInviteToFriend', () => ({
  useSendInviteToFriend: () => ({ send: mockSend }),
}));

const alice: Friend = { id: 'fr-1', userId: 'u-alice', username: 'alice', status: 'online' };

function setFriends(friends: Friend[]) {
  useFriendStore.setState({ friends, fetchFriends: vi.fn() });
}

describe('SendToFriendModal', () => {
  const onClose = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    setFriends([alice]);
  });

  it('renders the friend list', () => {
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    expect(screen.getByText('alice')).toBeInTheDocument();
  });

  it('shows an empty state when there are no friends', () => {
    setFriends([]);
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    expect(screen.getByText(/No friends yet/i)).toBeInTheDocument();
  });

  it('on success, navigates to the DMs view and closes', async () => {
    mockSend.mockResolvedValue({ ok: true, conversationId: 'dm-1' });
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    fireEvent.click(screen.getByText('alice'));
    await waitFor(() => {
      expect(mockSend).toHaveBeenCalledWith(alice);
      expect(mockNavigate).toHaveBeenCalledWith('/app/dms');
      expect(onClose).toHaveBeenCalled();
    });
  });

  it('on dm_blocked, shows an inline error and does not navigate', async () => {
    mockSend.mockResolvedValue({ ok: false, reason: 'dm_blocked' });
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    fireEvent.click(screen.getByText('alice'));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/accepting DMs/i));
    expect(mockNavigate).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('disables all rows while a send is in flight (prevents concurrent double-send)', async () => {
    let resolveSend!: (v: { ok: false; reason: 'dm_blocked' }) => void;
    mockSend.mockReturnValue(
      new Promise<{ ok: false; reason: 'dm_blocked' }>((r) => {
        resolveSend = r;
      })
    );
    const bob: Friend = { id: 'fr-2', userId: 'u-bob', username: 'bob', status: 'offline' };
    setFriends([alice, bob]);
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);

    fireEvent.click(screen.getByText('alice'));
    // While alice's send is in flight, every row is disabled — a second click
    // (alice again, or bob) cannot start a concurrent send / mint a 2nd invite.
    await waitFor(() => expect(screen.getByText('bob').closest('button')).toBeDisabled());
    expect(screen.getByText('alice').closest('button')).toBeDisabled();
    expect(mockSend).toHaveBeenCalledTimes(1);

    resolveSend({ ok: false, reason: 'dm_blocked' });
    await waitFor(() => expect(screen.getByText('bob').closest('button')).not.toBeDisabled());
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <SendToFriendModal serverId="s-1" serverName="Acme" open={false} onClose={onClose} />
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders an avatar image when the friend has an avatar URL', () => {
    setFriends([{ ...alice, avatarUrl: '/api/v1/media/avatars/alice.png' }]);
    const { container } = render(
      <SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />
    );
    const img = container.querySelector('img.send-to-friend-modal__avatar');
    expect(img).not.toBeNull();
    expect(img?.getAttribute('src')).toContain('/api/v1/media/avatars/alice.png');
  });

  it('filters the friend list by the search query', () => {
    const bob: Friend = { id: 'fr-2', userId: 'u-bob', username: 'bob', status: 'offline' };
    setFriends([alice, bob]);
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    fireEvent.change(screen.getByLabelText('Search friends'), { target: { value: 'bob' } });
    expect(screen.getByText('bob')).toBeInTheDocument();
    expect(screen.queryByText('alice')).not.toBeInTheDocument();
  });

  it('shows the not_ready message inline', async () => {
    mockSend.mockResolvedValue({ ok: false, reason: 'not_ready' });
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    fireEvent.click(screen.getByText('alice'));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/Still connecting/i));
  });

  it('shows the mint_failed message inline', async () => {
    mockSend.mockResolvedValue({ ok: false, reason: 'mint_failed' });
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    fireEvent.click(screen.getByText('alice'));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/create the invite/i));
  });

  it('closes on Escape', () => {
    render(<SendToFriendModal serverId="s-1" serverName="Acme" open onClose={onClose} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });
});
