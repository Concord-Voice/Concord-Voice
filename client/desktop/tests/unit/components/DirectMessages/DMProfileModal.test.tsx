import { render, screen, fireEvent, act } from '../../../test-utils';
import DMProfileModal from '@/renderer/components/DirectMessages/DMProfileModal';
import type { DMParticipant, DMConversation } from '@/renderer/stores/dmStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { server } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

const mockPeer: DMParticipant = {
  userId: 'user-2',
  username: 'bob',
  displayName: 'Bob',
};

const mockConversation: DMConversation = {
  id: 'conv-1',
  isGroup: false,
  isPersonal: false,
  name: null,
  participants: [{ userId: 'user-1', username: 'alice' }, mockPeer],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-04-08T00:00:00Z',
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('DMProfileModal', () => {
  const onClose = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('returns null when isOpen is false', () => {
    const { container } = render(
      <DMProfileModal
        isOpen={false}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(container.firstChild).toBeNull();
  });

  it('renders content when isOpen is true', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  it('closes on X button click', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    fireEvent.click(screen.getByLabelText('Close profile'));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('closes on backdrop click (click target is the dialog itself)', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    // Clicking the backdrop fires a click event with target === dialog. The
    // RTL fireEvent helper sets the element passed in as the click target,
    // which mimics the native browser behavior for ::backdrop clicks.
    fireEvent.click(screen.getByRole('dialog'));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('closes on dialog close (Escape key triggers native close event)', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    // The native <dialog> element fires a 'close' event when Escape is
    // pressed (in real browsers) or when dialog.close() is called. In jsdom,
    // we exercise the close-event-relay path by invoking close() directly.
    const dialog = screen.getByRole('dialog') as HTMLDialogElement;
    dialog.close();
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('does NOT close when clicking a child element inside the modal', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    // Clicking the peer's display name (a child of the dialog) should NOT
    // trigger backdrop-dismiss — the target is a child element, not the
    // dialog itself.
    fireEvent.click(screen.getByText('Bob'));
    expect(onClose).not.toHaveBeenCalled();
  });

  it('shows display name and username', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.getByText('@bob')).toBeInTheDocument();
  });

  it('shows live status from friendStore when peer is a friend', () => {
    useFriendStore.setState({
      friends: [
        {
          id: 'f-1',
          userId: 'user-2',
          username: 'bob',
          status: 'dnd',
        },
      ],
    });
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.getByText('Do Not Disturb')).toBeInTheDocument();
  });

  it("defaults to 'Offline' when peer is NOT in friendStore (ignores DMParticipant.status)", () => {
    // DMParticipant.status is a `string | undefined` field — using it as a
    // fallback risks rendering an unknown CSS class for unexpected server
    // values. Per Copilot review fix on PR #1214, we ignore it and default
    // to 'offline' for non-friend DMs.
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={{ ...mockPeer, status: 'online' }}
        conversation={mockConversation}
      />
    );
    expect(screen.getByText('Offline')).toBeInTheDocument();
  });

  it('updates status when friendStore.updateFriendPresence fires', () => {
    useFriendStore.setState({
      friends: [{ id: 'f-1', userId: 'user-2', username: 'bob', status: 'offline' }],
    });
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.getByText('Offline')).toBeInTheDocument();
    act(() => {
      useFriendStore.getState().updateFriendPresence('user-2', 'online');
    });
    expect(screen.getByText('Online')).toBeInTheDocument();
  });

  it('shows "Friends since" when peer is in friendStore with createdAt', () => {
    useFriendStore.setState({
      friends: [
        {
          id: 'f-1',
          userId: 'user-2',
          username: 'bob',
          status: 'online',
          createdAt: '2025-03-14T12:00:00Z',
        },
      ],
    });
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.getByText('Friends since')).toBeInTheDocument();
  });

  it('does NOT show "Friends since" when peer is not in friendStore.friends', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.queryByText('Friends since')).not.toBeInTheDocument();
  });

  it('shows "Member since" after /profile resolves', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({ user: { created_at: '2024-01-02T00:00:00Z' } })
      )
    );
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(await screen.findByText('Member since')).toBeInTheDocument();
  });

  it('shows "Conversation started" always', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.getByText('Conversation started')).toBeInTheDocument();
  });

  it('hides About Me when profile.bio is empty', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({ user: { bio: '', links: [], created_at: '2024-01-02T00:00:00Z' } })
      )
    );
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    await screen.findByText('Member since');
    expect(screen.queryByText('About Me')).not.toBeInTheDocument();
  });

  it('shows About Me when profile.bio is non-empty', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({
          user: { bio: 'Hello world', links: [], created_at: '2024-01-02T00:00:00Z' },
        })
      )
    );
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(await screen.findByText('About Me')).toBeInTheDocument();
    expect(screen.getByText('Hello world')).toBeInTheDocument();
  });

  it('filters out unsafe link protocols (javascript:, data:, vbscript:)', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({
          user: {
            bio: '',
            links: [
              'javascript:alert(1)',
              'data:text/html,<script>alert(1)</script>',
              'vbscript:msgbox("x")',
              'https://example.com/safe',
            ],
            created_at: '2024-01-02T00:00:00Z',
          },
        })
      )
    );

    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );

    // Only the https:// link should appear; the 3 unsafe-scheme entries are
    // filtered out by isSafeLinkUrl.
    const links = await screen.findAllByRole('link');
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAttribute('href', 'https://example.com/safe');
  });

  it('hides Links when all entries have unsafe protocols', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({
          user: {
            bio: '',
            links: ['javascript:alert(1)', 'data:text/html,xxx'],
            created_at: '2024-01-02T00:00:00Z',
          },
        })
      )
    );

    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );

    await screen.findByText('Member since');
    expect(screen.queryByText('Links')).not.toBeInTheDocument();
  });

  it('hides Links when profile.links is empty', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({ user: { bio: '', links: [], created_at: '2024-01-02T00:00:00Z' } })
      )
    );
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    await screen.findByText('Member since');
    expect(screen.queryByText('Links')).not.toBeInTheDocument();
  });

  it('shows Links and routes link click through openExternal IPC', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({
          user: {
            bio: '',
            links: ['https://example.com/blog', '  ', 'https://github.com/peer'],
            created_at: '2024-01-02T00:00:00Z',
          },
        })
      )
    );

    const openExternal = vi.fn().mockResolvedValue(undefined);
    const electronGlobal = (
      globalThis as typeof globalThis & {
        electron?: { openExternal?: (url: string) => Promise<unknown> | void };
      }
    ).electron;
    const originalOpenExternal = electronGlobal?.openExternal;
    if (electronGlobal) electronGlobal.openExternal = openExternal;

    try {
      render(
        <DMProfileModal
          isOpen={true}
          onClose={onClose}
          peer={mockPeer}
          conversation={mockConversation}
        />
      );

      // Whitespace-only links are filtered out — only 2 of 3 entries render.
      const links = await screen.findAllByRole('link');
      expect(links).toHaveLength(2);

      fireEvent.click(links[0]);
      expect(openExternal).toHaveBeenCalledWith('https://example.com/blog');
    } finally {
      // Restore symmetrically — if openExternal was originally undefined (the
      // default in tests/setup.ts), explicitly delete the mock instead of
      // leaving the truthy mock in place for subsequent tests. Per Copilot
      // review on PR #1214.
      if (electronGlobal) {
        if (originalOpenExternal !== undefined) {
          electronGlobal.openExternal = originalOpenExternal;
        } else {
          delete electronGlobal.openExternal;
        }
      }
    }
  });

  it('falls through cleanly when openExternal IPC is unavailable', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/user-2/profile`, () =>
        HttpResponse.json({
          user: { bio: '', links: ['https://example.com'], created_at: '2024-01-02T00:00:00Z' },
        })
      )
    );

    // Remove the openExternal handler from the test-setup mock for this test.
    const electronGlobal = (
      globalThis as typeof globalThis & {
        electron?: { openExternal?: (url: string) => Promise<unknown> | void };
      }
    ).electron;
    const originalOpenExternal = electronGlobal?.openExternal;
    if (electronGlobal) delete electronGlobal.openExternal;

    try {
      render(
        <DMProfileModal
          isOpen={true}
          onClose={onClose}
          peer={mockPeer}
          conversation={mockConversation}
        />
      );

      const link = await screen.findByRole('link');
      // Should not throw — the handler should fall through to native anchor
      // behavior when the IPC bridge is absent.
      fireEvent.click(link);
      expect(link).toBeInTheDocument();
    } finally {
      // Restore symmetrically — if openExternal was originally undefined (the
      // default in tests/setup.ts), explicitly delete the mock instead of
      // leaving the truthy mock in place for subsequent tests. Per Copilot
      // review on PR #1214.
      if (electronGlobal) {
        if (originalOpenExternal !== undefined) {
          electronGlobal.openExternal = originalOpenExternal;
        } else {
          delete electronGlobal.openExternal;
        }
      }
    }
  });

  it('hides Send Message when onSendMessage is undefined', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.queryByRole('button', { name: 'Send Message' })).not.toBeInTheDocument();
  });

  it('hides Voice Call when onVoiceCall is undefined', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.queryByRole('button', { name: 'Voice Call' })).not.toBeInTheDocument();
  });

  it('shows Voice Call when onVoiceCall is provided', () => {
    const onVoiceCall = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onVoiceCall={onVoiceCall}
      />
    );
    expect(screen.getByRole('button', { name: 'Voice Call' })).toBeInTheDocument();
  });

  it('hides Unfriend when peer is not a friend', () => {
    const onUnfriend = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onUnfriend={onUnfriend}
      />
    );
    expect(screen.queryByRole('button', { name: 'Unfriend' })).not.toBeInTheDocument();
  });

  it('shows Unfriend when peer is a friend and onUnfriend is provided', () => {
    useFriendStore.setState({
      friends: [{ id: 'f-1', userId: 'user-2', username: 'bob', status: 'online' }],
    });
    const onUnfriend = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onUnfriend={onUnfriend}
      />
    );
    expect(screen.getByRole('button', { name: 'Unfriend' })).toBeInTheDocument();
  });

  it('hides Block when onBlockUser is undefined', () => {
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
      />
    );
    expect(screen.queryByRole('button', { name: 'Block' })).not.toBeInTheDocument();
  });

  it('invokes onBlockUser(conversation) when Block is clicked', () => {
    const onBlockUser = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onBlockUser={onBlockUser}
      />
    );
    fireEvent.click(screen.getByRole('button', { name: 'Block' }));
    expect(onBlockUser).toHaveBeenCalledWith(mockConversation);
  });

  it('invokes onUnfriend(conversation) when Unfriend is clicked', () => {
    useFriendStore.setState({
      friends: [{ id: 'f-1', userId: 'user-2', username: 'bob', status: 'online' }],
    });
    const onUnfriend = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onUnfriend={onUnfriend}
      />
    );
    fireEvent.click(screen.getByRole('button', { name: 'Unfriend' }));
    expect(onUnfriend).toHaveBeenCalledWith(mockConversation);
  });

  it('invokes onSendMessage(conversation) when Send Message is clicked', () => {
    const onSendMessage = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onSendMessage={onSendMessage}
      />
    );
    fireEvent.click(screen.getByRole('button', { name: 'Send Message' }));
    expect(onSendMessage).toHaveBeenCalledWith(mockConversation);
  });

  it('invokes onVoiceCall(conversation) when Voice Call is clicked', () => {
    const onVoiceCall = vi.fn();
    render(
      <DMProfileModal
        isOpen={true}
        onClose={onClose}
        peer={mockPeer}
        conversation={mockConversation}
        onVoiceCall={onVoiceCall}
      />
    );
    fireEvent.click(screen.getByRole('button', { name: 'Voice Call' }));
    expect(onVoiceCall).toHaveBeenCalledWith(mockConversation);
  });
});
