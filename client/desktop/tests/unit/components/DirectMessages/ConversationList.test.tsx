import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import {
  useDMStore,
  type DMConversation,
  type DMLastMessage,
} from '@/renderer/stores/chat/dmStore';
import { useFriendStore } from '@/renderer/stores/chat/friendStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { useDraftMessageStore } from '@/renderer/stores/chat/draftMessageStore';
import { useLayoutStore } from '@/renderer/stores/ui/layoutStore';
import { API_BASE } from '@/renderer/config';
import { vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock e2eeService
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    createChannelOperationGuard: vi.fn(() => ({ assertCurrent: vi.fn() })),
    decryptForChannel: vi.fn().mockResolvedValue(null),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

import ConversationList from '@/renderer/components/DirectMessages/ConversationList';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2ee/e2eeErrors';
import { DockOverlayProvider, DockShell } from '@/renderer/components/Layout/DockShell';
import ContextMenuProvider from '@/renderer/components/ui/ContextMenuProvider';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';

/**
 * The preview renders the sender name in its own element, so the untrusted half
 * is bounded in the DOM (a display name of exactly `You sent a Photo` otherwise
 * composes into a complete forged attribution). RTL's default text matcher sees
 * only an element's DIRECT text nodes, so `getByText('Alice sent a Photo')`
 * cannot match across that boundary. Assert the composed text on the preview
 * element instead — scoped by class so it can never match an ancestor.
 */
const findPreview = (text: string) =>
  screen.findByText(
    (_content, element) =>
      element?.classList.contains('conversation-preview') === true && element.textContent === text
  );

const makeConversation = (overrides: Partial<DMConversation> = {}): DMConversation => ({
  id: 'conv-1',
  isGroup: false,
  isPersonal: false,
  name: null,
  participants: [
    { userId: 'user-1', username: 'me', displayName: 'Me' },
    { userId: 'user-2', username: 'alice', displayName: 'Alice' },
  ],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-01T00:00:00Z',
  ...overrides,
});

const personalThread: DMConversation = {
  id: 'personal-1',
  isGroup: false,
  isPersonal: true,
  name: 'Personal',
  participants: [{ userId: 'user-1', username: 'me', displayName: 'Me' }],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-01T00:00:00Z',
};

describe('ConversationList', () => {
  const mockOnSelectThread = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    (e2eeService as any).isInitialized = false;
    (e2eeService.createChannelOperationGuard as ReturnType<typeof vi.fn>).mockImplementation(
      () => ({ assertCurrent: vi.fn() })
    );
    // vi.clearAllMocks() clears call history but NOT the mockResolvedValueOnce
    // queue, and a test whose queued value is never consumed (an optimistic
    // send skips the decrypt entirely) hands it to the NEXT test's first
    // decrypt. mockReset drains the queue; the other three mocks already get
    // the equivalent treatment from their mockImplementation calls.
    const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
    decryptMock.mockReset();
    decryptMock.mockResolvedValue(null);
    (e2eeService.invalidateChannelKey as ReturnType<typeof vi.fn>).mockImplementation(
      () => undefined
    );
    (e2eeService.revokeChannelAccess as ReturnType<typeof vi.fn>).mockImplementation(
      () => undefined
    );
    useVoiceStore.getState().reset();
    useUserStore.setState({
      user: {
        id: 'user-1',
        username: 'me',
        display_name: 'Me',
        email: 'me@test.com',
        email_verified: true,
        age_verified: true,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      },
    });
    useDMStore.setState({
      conversations: [],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
      openPersonalThread: vi.fn().mockResolvedValue(personalThread),
    });
    useFriendStore.setState({ friends: [] });
    useDraftMessageStore.getState().clearAllDrafts();
  });

  it('renders conversation list container', () => {
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    expect(container.querySelector('.conversation-list')).toBeInTheDocument();
  });

  it('renders search input', () => {
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByPlaceholderText('Search conversations...')).toBeInTheDocument();
  });

  it('renders Personal Thread button', () => {
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Personal Thread')).toBeInTheDocument();
  });

  // Inverted deliberately (#2363). ServerBar owns the mount fetch now, and it
  // renders in this view as well as the persistent chrome. Two mount sites cost
  // TWO sequential full-list requests per DM-view entry, because an overlapping
  // call queues a deferred refetch rather than collapsing. Restoring the effect
  // here would reintroduce that silently, so assert its ABSENCE.
  it('does not fetch conversations on mount — ServerBar owns that', () => {
    const mockFetch = vi.fn().mockResolvedValue(undefined);
    useDMStore.setState({ fetchConversations: mockFetch });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it('shows empty state when no conversations exist', () => {
    useDMStore.setState({ conversations: [] });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('No conversations yet')).toBeInTheDocument();
  });

  it('renders 1:1 conversation with other users name', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('updates a 1:1 DM row from live friend presence', async () => {
    useFriendStore.getState().addFriend({
      id: 'friendship-1',
      userId: 'user-2',
      username: 'alice',
      displayName: 'Alice',
      status: 'offline',
    });
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'offline' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    expect(
      container.querySelector('.conversation-avatar .member-status-dot.offline')
    ).toBeInTheDocument();

    act(() => {
      useFriendStore.getState().updateFriendPresence('user-2', 'online');
    });

    await waitFor(() =>
      expect(
        container.querySelector('.conversation-avatar .member-status-dot.online')
      ).toBeInTheDocument()
    );
  });

  it('uses live friend presence over a stale DM participant snapshot', () => {
    useFriendStore.getState().addFriend({
      id: 'friendship-1',
      userId: 'user-2',
      username: 'alice',
      displayName: 'Alice',
      status: 'offline',
    });
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice', status: 'online' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    expect(
      container.querySelector('.conversation-avatar .member-status-dot.offline')
    ).toBeInTheDocument();
  });

  it('renders group conversation with group name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Study Group',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Study Group')).toBeInTheDocument();
  });

  it('renders group with participant names when no group name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: null,
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-2', username: 'alice', displayName: 'Alice' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Me, Alice, Bob')).toBeInTheDocument();
  });

  it('renders conversation with username when no display name', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'charlie' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('charlie')).toBeInTheDocument();
  });

  it('shows last message preview after decryption', async () => {
    // All DM messages are E2EE — preview renders after e2eeService decrypts
    (e2eeService as any).isInitialized = true;
    (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('Hey there!');
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-ciphertext',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    await waitFor(() => expect(screen.getByText('Hey there!')).toBeInTheDocument());
    (e2eeService as any).isInitialized = false;
  });

  it('shows call context instead of the encrypted-message fallback', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: '',
            userId: 'user-1',
            username: 'me',
            createdAt: '2026-07-13T12:00:00Z',
            type: 'call_event',
            callEventPayload: {
              caller_user_id: 'user-1',
              started_at: '2026-07-13T12:00:00Z',
              status: 'missed',
              duration_seconds: 0,
            },
          } as DMLastMessage,
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

    expect(screen.getByText('Outbound call — no answer')).toBeInTheDocument();
    expect(screen.queryByText('Encrypted message')).not.toBeInTheDocument();
  });

  it('does not retain an in-flight preview after remote conversation removal', async () => {
    (e2eeService as any).isInitialized = true;
    let generation = 0;
    let resolveDecrypt!: (plaintext: string) => void;
    const decryptPromise = new Promise<string>((resolve) => {
      resolveDecrypt = resolve;
    });
    const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
    decryptMock
      .mockReturnValueOnce(decryptPromise)
      .mockRejectedValueOnce(new Error('new key unavailable'));
    (e2eeService.createChannelOperationGuard as ReturnType<typeof vi.fn>).mockImplementation(() => {
      const capturedGeneration = generation;
      return {
        assertCurrent: () => {
          if (capturedGeneration !== generation) throw new Error('stale preview generation');
        },
      };
    });
    (e2eeService.revokeChannelAccess as ReturnType<typeof vi.fn>).mockImplementation(() => {
      generation += 1;
    });
    const conversation = makeConversation({
      lastMessage: {
        content: 'removed-ciphertext',
        userId: 'user-2',
        username: 'alice',
        createdAt: '2025-01-01T12:00:00Z',
      },
    });
    useDMStore.setState({
      conversations: [conversation],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));

    act(() => useDMStore.getState().removeConversation(conversation.id));
    resolveDecrypt('removed plaintext');
    await act(async () => {
      await decryptPromise;
    });

    act(() => useDMStore.setState({ conversations: [conversation] }));

    await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(2));
    expect(screen.queryByText('removed plaintext')).not.toBeInTheDocument();
    expect(screen.getByText('Encrypted message')).toBeInTheDocument();
    (e2eeService as any).isInitialized = false;
  });

  it('shows a friendly GIF label for a decrypted GIF-only last message preview', async () => {
    // regression for #1991
    (e2eeService as any).isInitialized = true;
    (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce(
      '{"text":"","gif_slug":"night-sleep-18"}'
    );
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-gif-envelope',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

    // A GIF-only message carries no text, so the descriptor's kind is 'gif' and
    // §4.1 rung 5 + §4.2 give every no-text rung sender attribution. The bare
    // 'GIF' label is superseded by 'Alice sent a GIF' — spec, not regression.
    expect(await findPreview('Alice sent a GIF')).toBeInTheDocument();
    expect(screen.queryByText(/gif_slug/)).not.toBeInTheDocument();
    (e2eeService as any).isInitialized = false;
  });

  it('shows "Encrypted message" for encrypted last message', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-ciphertext',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Encrypted message')).toBeInTheDocument();
  });

  it('does not show GIF from undecrypted server gif_slug metadata', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'encrypted-gif-envelope',
            userId: 'user-2',
            username: 'alice',
            createdAt: '2025-01-01T12:00:00Z',
            gifSlug: 'night-sleep-18',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

    expect(screen.getByText('Encrypted message')).toBeInTheDocument();
    expect(screen.queryByText('GIF')).not.toBeInTheDocument();
  });

  it('prefers an optimistic plaintext preview over a stale decrypted cache', async () => {
    (e2eeService as any).isInitialized = true;
    const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
    decryptMock.mockResolvedValueOnce('First sent message');

    useDMStore.setState({
      conversations: [
        makeConversation({
          lastMessage: {
            content: 'first-ciphertext',
            userId: 'user-1',
            username: 'me',
            createdAt: '2025-01-01T12:00:00Z',
          },
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });

    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    await waitFor(() => expect(screen.getByText('First sent message')).toBeInTheDocument());

    decryptMock.mockRejectedValueOnce(new Error('optimistic content is not ciphertext'));
    const optimisticLastMessage: DMLastMessage & { plaintextPreview: string } = {
      content: 'Second sent message',
      plaintextPreview: 'Second sent message',
      userId: 'user-1',
      username: 'me',
      createdAt: '2025-01-01T12:01:00Z',
    };

    await act(async () => {
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: optimisticLastMessage,
          }),
        ],
      });
    });

    await waitFor(() => expect(screen.getByText('Second sent message')).toBeInTheDocument());
    expect(screen.queryByText('First sent message')).not.toBeInTheDocument();
    (e2eeService as any).isInitialized = false;
  });

  describe('preview decrypt tri-state (#2364)', () => {
    const encryptedPhotoConversation = () =>
      makeConversation({
        lastMessage: {
          content: 'encrypted-photo-ciphertext',
          userId: 'user-2',
          username: 'alice',
          createdAt: '2025-01-01T12:00:00Z',
          attachmentType: 'photo',
        },
      });

    // Settling the decrypt inside act() is what makes the "was anything
    // committed?" assertions non-vacuous: without it, decryptingRef may still
    // hold the conversation and a skipped re-decrypt would read as a cache hit.
    const deferred = <T,>() => {
      let resolve!: (value: T) => void;
      let reject!: (reason: Error) => void;
      const promise = new Promise<T>((res, rej) => {
        resolve = res;
        reject = rej;
      });
      return { promise, resolve, reject };
    };

    beforeEach(() => {
      (e2eeService as any).isInitialized = true;
    });

    it('renders the attributed type when decryption proves the text is empty', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [encryptedPhotoConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      expect(await findPreview('Alice sent a Photo')).toBeInTheDocument();
    });

    it('renders the caption with a suffix when decryption yields text', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce(
        'check this out'
      );
      useDMStore.setState({
        conversations: [encryptedPhotoConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      expect(await screen.findByText('check this out · Photo')).toBeInTheDocument();
    });

    it('renders the placeholder when decryption genuinely fails, never a type', async () => {
      const attempt = deferred<string>();
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock.mockReturnValueOnce(attempt.promise);
      useDMStore.setState({
        conversations: [encryptedPhotoConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));
      await act(async () => {
        attempt.reject(new Error('no key'));
        await attempt.promise.catch(() => undefined);
      });

      expect(screen.getByText('Encrypted message')).toBeInTheDocument();
      expect(screen.queryByText(/sent a Photo/)).not.toBeInTheDocument();

      // A GENUINE failure is committed and is terminal for this ciphertext: a
      // fresh pass over the same conversation must not re-attempt the decrypt.
      // This is the positive witness the superseded case below must NOT show.
      await act(async () => {
        useDMStore.setState({ conversations: [encryptedPhotoConversation()] });
      });
      expect(decryptMock).toHaveBeenCalledTimes(1);
    });

    it('commits nothing when the operation is superseded, so a later pass recovers (R9)', async () => {
      // assertCurrent() throws from OUTSIDE the decrypt try. A naive tri-state
      // catches that throw and records a terminal 'failed' for a conversation
      // whose key material has since rotated — re-arming exactly the preview
      // resurrection class the guard exists to prevent. Nothing may be
      // committed, and "nothing" is only distinguishable from "failed" by the
      // re-decrypt that a terminal entry with a matching cipher would suppress.
      let superseded = true;
      (e2eeService.createChannelOperationGuard as ReturnType<typeof vi.fn>).mockImplementation(
        () => ({
          assertCurrent: () => {
            if (superseded) throw new Error('stale preview generation');
          },
        })
      );
      const attempt = deferred<string>();
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock.mockReturnValueOnce(attempt.promise).mockResolvedValueOnce('recovered plaintext');
      useDMStore.setState({
        conversations: [encryptedPhotoConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));
      await act(async () => {
        attempt.resolve('superseded plaintext');
        await attempt.promise;
      });

      expect(screen.getByText('Encrypted message')).toBeInTheDocument();
      expect(screen.queryByText(/superseded plaintext/)).not.toBeInTheDocument();
      expect(screen.queryByText(/sent a Photo/)).not.toBeInTheDocument();

      // Key material settles. A fresh pass over the SAME ciphertext must
      // re-decrypt, which only holds if the superseded pass committed nothing.
      superseded = false;
      await act(async () => {
        useDMStore.setState({ conversations: [encryptedPhotoConversation()] });
      });

      // The conversation carries a photo, so a captioned decrypt renders with
      // the '· Photo' suffix — the same shape the caption case above asserts.
      expect(await screen.findByText('recovered plaintext · Photo')).toBeInTheDocument();
    });
  });

  describe('accessible description carries the preview (#2364 a11y)', () => {
    // aria-label sets the accessible NAME, and NVDA/JAWS re-announce a
    // focused element whenever its name changes. aria-describedby instead
    // points at an accessible DESCRIPTION, which is read once on focus and
    // is NOT monitored for mutation — that is why the preview (which
    // changes on every incoming message) belongs in the description and
    // must never move into the name. See the comment on
    // StandardConversationItem in ConversationList.tsx for the same note.
    beforeEach(() => {
      (e2eeService as any).isInitialized = true;
    });

    it('carries the preview for a plain text message', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce(
        'Hey there!'
      );
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: 'user-2',
              username: 'alice',
              createdAt: '2025-01-01T12:00:00Z',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      await screen.findByText('Hey there!');
      expect(screen.getByLabelText('Alice')).toHaveAccessibleDescription('Hey there!');
    });

    it('carries the preview for an attachment message', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: {
              content: 'encrypted-photo-ciphertext',
              userId: 'user-2',
              username: 'alice',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'photo',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      await findPreview('Alice sent a Photo');
      expect(screen.getByLabelText('Alice')).toHaveAccessibleDescription('Alice sent a Photo');
    });

    it('carries the preview for a captioned attachment', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce(
        'check this out'
      );
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: {
              content: 'encrypted-photo-ciphertext',
              userId: 'user-2',
              username: 'alice',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'photo',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      await screen.findByText('check this out · Photo');
      expect(screen.getByLabelText('Alice')).toHaveAccessibleDescription('check this out · Photo');
    });

    it('exposes only the placeholder for an undecryptable message — no ciphertext, no attachment type', async () => {
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock.mockRejectedValueOnce(new Error('no key'));
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: {
              content: 'encrypted-photo-ciphertext',
              userId: 'user-2',
              username: 'alice',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'photo',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      await screen.findByText('Encrypted message');
      const item = screen.getByLabelText('Alice');
      expect(item).toHaveAccessibleDescription('Encrypted message');
      expect(item).not.toHaveAccessibleDescription(/encrypted-photo-ciphertext/);
      expect(item).not.toHaveAccessibleDescription(/Photo/);
    });

    it('leaves the accessible name unchanged while an incoming message updates the description', async () => {
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock.mockResolvedValueOnce('first message');
      const conversation = makeConversation({
        lastMessage: {
          content: 'encrypted-ciphertext-1',
          userId: 'user-2',
          username: 'alice',
          createdAt: '2025-01-01T12:00:00Z',
        },
      });
      useDMStore.setState({
        conversations: [conversation],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      await screen.findByText('first message');
      expect(screen.getByLabelText('Alice')).toHaveAccessibleDescription('first message');

      decryptMock.mockResolvedValueOnce('second message');
      act(() => {
        useDMStore.setState({
          conversations: [
            {
              ...conversation,
              lastMessage: {
                content: 'encrypted-ciphertext-2',
                userId: 'user-2',
                username: 'alice',
                createdAt: '2025-01-01T12:05:00Z',
              },
            },
          ],
        });
      });

      await waitFor(() =>
        expect(screen.getByLabelText('Alice')).toHaveAccessibleDescription('second message')
      );
      // The NAME is unchanged by the incoming message — only the description
      // moved. This is the assertion that pins the design decision: moving
      // the preview into aria-label would both fail this assertion AND break
      // the getByLabelText('Alice') query above it.
      expect(screen.getByLabelText('Alice')).toHaveAttribute('aria-label', 'Alice');
    });

    it('gets no aria-describedby at all when there is no last message', () => {
      useDMStore.setState({
        conversations: [makeConversation({ lastMessage: null })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      const item = screen.getByLabelText('Alice');
      expect(item).not.toHaveAttribute('aria-describedby');
    });
  });

  describe('preview sender attribution — the name ladder is total', () => {
    beforeEach(() => {
      (e2eeService as any).isInitialized = true;
    });

    it('uses You for the current user', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: 'user-1',
              username: 'me',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'photo',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(await findPreview('You sent a Photo')).toBeInTheDocument();
    });

    it('bounds a hostile display name that reads exactly like the app clause', async () => {
      // 16 graphemes, so truncateSenderName returns it verbatim, and the flat
      // string it used to compose into was "You sent a Photo sent a Photo" --
      // which .conversation-preview's text-overflow clips back to a complete,
      // well-formed attribution of Mallory's attachment to the reader.
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            participants: [
              { userId: 'user-1', username: 'me', displayName: 'Me' },
              { userId: 'user-2', username: 'mallory', displayName: 'You sent a Photo' },
            ],
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: 'user-2',
              username: 'mallory',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'photo',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      const preview = await findPreview('You sent a Photo sent a Photo');
      // The defect was the ABSENCE of any boundary: one text run in which the
      // reader cannot tell the attacker's characters from the app's. The name
      // must occupy its own element, so weight, colour and bidi isolation can
      // mark where the untrusted half ends.
      expect(preview.childElementCount).toBe(1);
      const sender = preview.querySelector('.conversation-preview-sender');
      expect(sender).not.toBeNull();
      expect(sender?.textContent).toBe('You sent a Photo');
      // And the app's own words are NOT inside that element.
      expect(sender?.textContent).not.toContain('sent a Photo sent');
    });

    it('prefers displayName over username', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            participants: [
              { userId: 'user-1', username: 'me', displayName: 'Me' },
              { userId: 'user-2', username: 'alice99', displayName: 'Alice' },
            ],
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: 'user-2',
              username: 'alice99',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'video',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(await findPreview('Alice sent a Video')).toBeInTheDocument();
    });

    it('falls back to the message username when the sender left the group', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            participants: [{ userId: 'user-1', username: 'me', displayName: 'Me' }],
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: 'gone',
              username: 'bob',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'file',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(await findPreview('bob sent a File')).toBeInTheDocument();
    });

    it('renders Someone — never undefined, never a UUID — when nothing resolves', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            participants: [{ userId: 'user-1', username: 'me', displayName: 'Me' }],
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: '33333333-3333-4333-8333-333333333333',
              username: undefined,
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'audio',
            } as DMLastMessage,
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(await findPreview('Someone sent an Audio file')).toBeInTheDocument();
      expect(screen.queryByText(/undefined/)).not.toBeInTheDocument();
      expect(screen.queryByText(/33333333/)).not.toBeInTheDocument();
    });

    it('truncates a long sender name so the type word survives', async () => {
      (e2eeService.decryptForChannel as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
      useDMStore.setState({
        conversations: [
          makeConversation({
            participants: [
              { userId: 'user-1', username: 'me', displayName: 'Me' },
              { userId: 'user-2', username: 'x', displayName: 'Alexandra Bartholomew' },
            ],
            lastMessage: {
              content: 'encrypted-ciphertext',
              userId: 'user-2',
              username: 'x',
              createdAt: '2025-01-01T12:00:00Z',
              attachmentType: 'photo',
            },
          }),
        ],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(await findPreview('Alexandra Bartho… sent a Photo')).toBeInTheDocument();
    });
  });

  it('calls onSelectThread on conversation click', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    fireEvent.click(screen.getByText('Alice'));
    expect(mockOnSelectThread).toHaveBeenCalledWith('conv-1');
  });

  it('applies active class to selected conversation', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId="conv-1" onSelectThread={mockOnSelectThread} />
    );
    const activeItem = container.querySelector('.conversation-item.active');
    expect(activeItem).toBeInTheDocument();
  });

  it('highlights personal thread when selected', () => {
    useDMStore.setState({
      conversations: [personalThread],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId="personal-1" onSelectThread={mockOnSelectThread} />
    );
    const activePersonal = container.querySelector('.personal-thread.active');
    expect(activePersonal).toBeInTheDocument();
  });

  it('shows unread badge on conversation with unreads', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 5 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('5')).toBeInTheDocument();
  });

  it('shows 99+ for large unread counts', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 200 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('99+')).toBeInTheDocument();
  });

  it('does not show unread badge when count is 0', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 0 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    expect(container.querySelector('.conversation-unread-badge')).not.toBeInTheDocument();
  });

  it('applies unread class to conversation with unreads', () => {
    useDMStore.setState({
      conversations: [makeConversation({ unreadCount: 3 })],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    const unreadItem = container.querySelector('.conversation-item.unread');
    expect(unreadItem).toBeInTheDocument();
  });

  it('filters conversations by search query', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({ id: 'conv-1' }),
        makeConversation({
          id: 'conv-2',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-3', username: 'bob', displayName: 'Bob' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const searchInput = screen.getByPlaceholderText('Search conversations...');
    fireEvent.change(searchInput, { target: { value: 'alice' } });
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.queryByText('Bob')).not.toBeInTheDocument();
  });

  it('search is case insensitive', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const searchInput = screen.getByPlaceholderText('Search conversations...');
    fireEvent.change(searchInput, { target: { value: 'ALICE' } });
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('opens personal thread on click', async () => {
    const mockOpen = vi.fn().mockResolvedValue(personalThread);
    useDMStore.setState({ openPersonalThread: mockOpen });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    fireEvent.click(screen.getByText('Personal Thread'));
    await vi.waitFor(() => {
      expect(mockOpen).toHaveBeenCalled();
      expect(mockOnSelectThread).toHaveBeenCalledWith('personal-1');
    });
  });

  it('does not show personal thread in filtered conversation list', () => {
    useDMStore.setState({
      conversations: [personalThread, makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const personalButtons = screen.getAllByText('Personal Thread');
    expect(personalButtons.length).toBe(1);
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('shows avatar initial for 1:1 conversations', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    const initial = container.querySelector('.conversation-avatar-initial');
    expect(initial).toBeInTheDocument();
    expect(initial?.textContent).toBe('A');
  });

  it('shows the other users avatar for 1:1 conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            {
              userId: 'user-2',
              username: 'alice',
              displayName: 'Alice',
              avatarUrl: '/api/v1/media/avatars/alice.png',
            },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    const image = container.querySelector('.conversation-avatar-img');
    expect(image).toHaveAttribute('src', `${API_BASE}/api/v1/media/avatars/alice.png`);
    expect(container.querySelector('.conversation-avatar-initial')).not.toBeInTheDocument();
  });

  it('falls back to initials when a 1:1 avatar image fails', async () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            {
              userId: 'user-2',
              username: 'alice',
              displayName: 'Alice',
              avatarUrl: 'https://example.com/broken.png',
            },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    fireEvent.error(container.querySelector('.conversation-avatar-img') as HTMLImageElement);

    await waitFor(() => {
      expect(container.querySelector('.conversation-avatar-img')).not.toBeInTheDocument();
      expect(container.querySelector('.conversation-avatar-initial')).toHaveTextContent('A');
    });
  });

  it('shows group avatar icon for group conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Team',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );
    const groupAvatar = container.querySelector('.conversation-avatar.group');
    expect(groupAvatar).toBeInTheDocument();
  });

  it('shows group conversation icons when set', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Team',
          iconUrl: '/api/v1/media/dm-icons/group-1.png',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    expect(container.querySelector('.conversation-avatar-img')).toHaveAttribute(
      'src',
      `${API_BASE}/api/v1/media/dm-icons/group-1.png`
    );
  });

  it('falls back to the group icon when a group image fails', async () => {
    useDMStore.setState({
      conversations: [
        makeConversation({
          id: 'group-1',
          isGroup: true,
          name: 'Team',
          iconUrl: 'https://example.com/broken-group.png',
          participants: [
            { userId: 'user-1', username: 'me' },
            { userId: 'user-2', username: 'alice' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    const { container } = render(
      <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
    );

    fireEvent.error(container.querySelector('.conversation-avatar-img') as HTMLImageElement);

    await waitFor(() => {
      expect(container.querySelector('.conversation-avatar-img')).not.toBeInTheDocument();
      expect(container.querySelector('.conversation-avatar.group svg')).toBeInTheDocument();
    });
  });

  it('opens context menu on right-click', () => {
    useDMStore.setState({
      conversations: [makeConversation()],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    const item = screen.getByLabelText('Alice');
    fireEvent.contextMenu(item);
    // Issue #84 added Mute Conversation as the always-present menu item;
    // it replaces the prior "No actions available" fallback. Asserting on
    // Mute is the most robust marker that the context menu opened — it's
    // there regardless of encryption state, while Rotate Encryption Key
    // depends on the conversation being encrypted.
    expect(screen.getByText('Mute Conversation')).toBeInTheDocument();
  });

  it('renders multiple conversations', () => {
    useDMStore.setState({
      conversations: [
        makeConversation({ id: 'conv-1' }),
        makeConversation({
          id: 'conv-2',
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            { userId: 'user-3', username: 'charlie', displayName: 'Charlie' },
          ],
        }),
      ],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('Charlie')).toBeInTheDocument();
  });

  it('logs redacted error when openPersonalThread fails', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useDMStore.setState({
      conversations: [],
      fetchConversations: vi.fn().mockResolvedValue(undefined),
      openPersonalThread: vi.fn().mockRejectedValueOnce(new Error('boom')),
    });
    render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
    fireEvent.click(screen.getByText('Personal Thread'));
    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to open personal thread:', 'boom');
    });
    consoleSpy.mockRestore();
  });

  // ─── #1219 R6: multi-participant "N of M in call" list indicator ──────

  describe('in-call indicator (#1219 R6)', () => {
    const groupConv = makeConversation({
      id: 'grp-1',
      isGroup: true,
      name: 'Squad',
      participants: [
        { userId: 'user-1', username: 'me', displayName: 'Me' },
        { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        { userId: 'user-3', username: 'bob', displayName: 'Bob' },
        { userId: 'user-4', username: 'carol', displayName: 'Carol' },
        { userId: 'user-5', username: 'dan', displayName: 'Dan' },
      ],
    });

    it('shows "N of M in call" for a group with an active call I am not in', () => {
      useDMStore.setState({
        conversations: [groupConv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-2', 'user-3', 'user-4'], 5);
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(screen.getByText('3 of 5 in call')).toBeInTheDocument();
    });

    it('does not show the group indicator when no call is active', () => {
      useDMStore.setState({
        conversations: [groupConv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(screen.queryByText(/in call/)).not.toBeInTheDocument();
    });

    it('keeps the 1:1 🔊 badge when locally in a 1:1 call', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useVoiceStore.getState().setDMCall(true, 'conv-1');
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      const { container } = render(
        <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );
      const badge = container.querySelector('.conversation-in-call-badge');
      expect(badge).toBeInTheDocument();
      expect(badge?.textContent).toBe('🔊');
    });

    it('shows "N of M in call" even on a group call I AM in (group always shows roster)', () => {
      useDMStore.setState({
        conversations: [groupConv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useVoiceStore.getState().seedActiveDMCall('grp-1', ['user-1', 'user-2'], 5);
      useVoiceStore.getState().setDMCall(true, 'grp-1');
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      expect(screen.getByText('2 of 5 in call')).toBeInTheDocument();
    });
  });

  // ─── #984 expansion: Block / Unfriend modal flow coverage ─────────────

  describe('Block / Unfriend modal flows (#984)', () => {
    // Helper: import friendStore lazily so the test file doesn't reach for it
    // unless these tests run.
    async function setupFriendStore(opts: {
      blockUser?: ReturnType<typeof vi.fn>;
      removeFriend?: ReturnType<typeof vi.fn>;
      isFriend?: boolean;
    }) {
      const { useFriendStore } = await import('@/renderer/stores/chat/friendStore');
      useFriendStore.setState({
        friends: opts.isFriend
          ? [
              {
                userId: 'user-2',
                username: 'alice',
                displayName: 'Alice',
                status: 'online',
                avatarUrl: undefined,
                createdAt: '2025-01-01T00:00:00Z',
                accentColor: undefined,
              },
            ]
          : [],
        blockUser: opts.blockUser ?? vi.fn().mockResolvedValue(undefined),
        removeFriend: opts.removeFriend ?? vi.fn().mockResolvedValue(undefined),
      });
    }

    it('shows only the DM row menu when wrapped by the global channel-area context provider (#1712)', async () => {
      await setupFriendStore({ isFriend: true });
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      fireEvent.contextMenu(screen.getByLabelText('Alice'));

      expect(await screen.findByText('Block User')).toBeInTheDocument();
      expect(screen.getByText('Unfriend')).toBeInTheDocument();
      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('does not fall through to the channel menu when right-clicking empty DM list space (#1712)', () => {
      useDMStore.setState({
        conversations: [],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      fireEvent.contextMenu(screen.getByText('No conversations yet'));

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('does not fall through to the channel menu from the DM row keyboard context-menu path (#1712)', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const convButton = screen.getByLabelText('Alice');
      convButton.focus();
      fireEvent.keyDown(convButton, { key: 'ContextMenu' });

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('does not fall through to the channel menu from Shift+F10 on a DM row (#1712)', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const convButton = screen.getByLabelText('Alice');
      convButton.focus();
      fireEvent.keyDown(convButton, { key: 'F10', shiftKey: true });

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('ignores non-context keyboard events inside the DM list boundary (#1712)', () => {
      useDMStore.setState({
        conversations: [makeConversation()],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const convButton = screen.getByLabelText('Alice');
      convButton.focus();
      fireEvent.keyDown(convButton, { key: 'ArrowDown' });

      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
      expect(screen.queryByText('Create Category')).not.toBeInTheDocument();
      expect(screen.queryByText('Channels')).not.toBeInTheDocument();
    });

    it('keeps the global text-input context menu available from the DM search field (#1712)', () => {
      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      fireEvent.contextMenu(screen.getByPlaceholderText('Search conversations...'));

      expect(screen.getByText('Paste')).toBeInTheDocument();
      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
    });

    it('keeps the global keyboard context menu available from the DM search field (#1712)', () => {
      render(
        <ContextMenuProvider>
          <div data-context-area="channels">
            <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
          </div>
        </ContextMenuProvider>
      );

      const searchInput = screen.getByPlaceholderText('Search conversations...');
      searchInput.focus();
      fireEvent.keyDown(searchInput, { key: 'ContextMenu' });

      expect(screen.getByText('Paste')).toBeInTheDocument();
      expect(screen.queryByText('Create Channel')).not.toBeInTheDocument();
    });

    it('Block User flow: right-click → Block User → modal → Confirm → friendStore.blockUser called', async () => {
      const blockSpy = vi.fn().mockResolvedValue(undefined);
      await setupFriendStore({ blockUser: blockSpy });

      const conv = {
        ...makeConversation(),
        id: 'conv-block',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      // Right-click the conversation row to open the context menu.
      const convButton = screen.getByLabelText('Alice');
      fireEvent.contextMenu(convButton);

      // Click "Block User" in the menu → ConversationList opens the modal.
      const blockItem = await screen.findByText('Block User');
      fireEvent.click(blockItem);

      // ConfirmActionModal opens with the peer's display name in the title.
      const modalTitle = await screen.findByText(/Block Alice/);
      expect(modalTitle).toBeInTheDocument();

      // Click the "Block" confirm button inside the modal. The modal has
      // confirmLabel="Block", so look for that specific button.
      const confirmButton = screen.getByRole('button', { name: /^Block$/ });
      fireEvent.click(confirmButton);

      await waitFor(() => {
        expect(blockSpy).toHaveBeenCalledWith('user-2');
      });
    });

    it('Unfriend flow: right-click → Unfriend (only when friend) → modal → Confirm → friendStore.removeFriend called', async () => {
      const removeSpy = vi.fn().mockResolvedValue(undefined);
      // isFriend: true so the Unfriend item is visible.
      await setupFriendStore({ removeFriend: removeSpy, isFriend: true });

      const conv = {
        ...makeConversation(),
        id: 'conv-unfriend',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      const convButton = screen.getByLabelText('Alice');
      fireEvent.contextMenu(convButton);

      const unfriendItem = await screen.findByText('Unfriend');
      fireEvent.click(unfriendItem);

      const modalTitle = await screen.findByText(/Unfriend Alice/);
      expect(modalTitle).toBeInTheDocument();

      const confirmButton = screen.getByRole('button', { name: /^Unfriend$/ });
      fireEvent.click(confirmButton);

      await waitFor(() => {
        expect(removeSpy).toHaveBeenCalledWith('user-2');
      });
    });

    it('Block modal Cancel closes without calling blockUser (#984)', async () => {
      const blockSpy = vi.fn().mockResolvedValue(undefined);
      await setupFriendStore({ blockUser: blockSpy });

      const conv = {
        ...makeConversation(),
        id: 'conv-cancel',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('Block User'));
      await screen.findByText(/Block Alice/);

      const cancelButton = screen.getByRole('button', { name: /Cancel/i });
      fireEvent.click(cancelButton);

      // Modal closes, no API call fires.
      await waitFor(() => {
        expect(screen.queryByText(/Block Alice/)).not.toBeInTheDocument();
      });
      expect(blockSpy).not.toHaveBeenCalled();
    });
  });

  describe('View Profile modal flow (#1208)', () => {
    async function setupFriendStore(opts: { isFriend?: boolean } = {}) {
      const { useFriendStore } = await import('@/renderer/stores/chat/friendStore');
      useFriendStore.setState({
        friends: opts.isFriend
          ? [
              {
                id: 'f-1',
                userId: 'user-2',
                username: 'alice',
                displayName: 'Alice',
                status: 'online',
                createdAt: '2025-01-01T00:00:00Z',
              },
            ]
          : [],
        blockUser: vi.fn().mockResolvedValue(undefined),
        removeFriend: vi.fn().mockResolvedValue(undefined),
      });
    }

    function setupConversation(id = 'conv-vp') {
      const conv = {
        ...makeConversation(),
        id,
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
        ],
      };
      useDMStore.setState({
        conversations: [conv],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
        openPersonalThread: vi.fn().mockResolvedValue(personalThread),
      });
      return conv;
    }

    it('right-click → View Profile opens DMProfileModal with peer identity', async () => {
      await setupFriendStore();
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));

      // Modal renders with peer's @username
      expect(await screen.findByText('@alice')).toBeInTheDocument();
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    });

    it('View Profile → Send Message: invokes onSelectThread and closes modal', async () => {
      await setupFriendStore();
      const conv = setupConversation('conv-send');

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByRole('button', { name: 'Send Message' }));

      expect(mockOnSelectThread).toHaveBeenCalledWith(conv.id);
      await waitFor(() => {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      });
    });

    it('View Profile → Block: closes modal and opens ConfirmActionModal', async () => {
      await setupFriendStore();
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByRole('button', { name: 'Block' }));

      // ConfirmActionModal opens with peer name in title
      expect(await screen.findByText(/Block Alice/)).toBeInTheDocument();
    });

    it('View Profile → Unfriend (when friend): closes modal and opens ConfirmActionModal', async () => {
      await setupFriendStore({ isFriend: true });
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByRole('button', { name: 'Unfriend' }));

      expect(await screen.findByText(/Unfriend Alice/)).toBeInTheDocument();
    });

    it('View Profile → ✕ button: closes modal without side effects', async () => {
      await setupFriendStore();
      setupConversation();

      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);

      fireEvent.contextMenu(screen.getByLabelText('Alice'));
      fireEvent.click(await screen.findByText('View Profile'));
      fireEvent.click(await screen.findByLabelText('Close profile'));

      await waitFor(() => {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      });
      expect(mockOnSelectThread).not.toHaveBeenCalled();
    });
  });

  // ─── mute-aware DM unread (#84 / epic #1029 close audit) ──────────────

  describe('mute-aware DM unread (#84 / epic #1029)', () => {
    beforeEach(() => {
      // The top-level beforeEach doesn't reset notificationPrefsStore, so
      // clear its mute maps before this suite seeds per-target mutes.
      useNotificationPrefsStore.getState().clearAll();
    });

    it('suppresses the unread badge and row class for a muted conversation', () => {
      useDMStore.setState({
        conversations: [makeConversation({ unreadCount: 5 })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      useNotificationPrefsStore.getState().setMute('dm', 'conv-1', true, null);

      const { container } = render(
        <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      expect(container.querySelector('.conversation-unread-badge')).not.toBeInTheDocument();
      expect(container.querySelector('.conversation-item.unread')).not.toBeInTheDocument();
    });

    it('still shows the unread badge and row class for a non-muted conversation', () => {
      useDMStore.setState({
        conversations: [makeConversation({ unreadCount: 5 })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });

      const { container } = render(
        <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      expect(container.querySelector('.conversation-unread-badge')).toBeInTheDocument();
      expect(screen.getByText('5')).toBeInTheDocument();
      expect(container.querySelector('.conversation-item.unread')).toBeInTheDocument();
    });
  });

  describe('thread sidebar presentations', () => {
    it('renders personal, one-to-one, fallback group, and search controls', () => {
      const group = makeConversation({
        id: 'group-1',
        isGroup: true,
        name: null,
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          { userId: 'user-3', username: 'bob', displayName: 'Bob' },
        ],
      });
      useDMStore.setState({
        conversations: [personalThread, makeConversation({ unreadCount: 2 }), group],
      });
      useDraftMessageStore.getState().setDraft('conv-1', {
        text: 'unfinished',
        updatedAt: 1,
      });

      const { container } = render(
        <ConversationList compact selectedThreadId="group-1" onSelectThread={mockOnSelectThread} />
      );

      expect(screen.getByRole('button', { name: /Personal Thread/i })).toBeVisible();
      expect(screen.getByRole('button', { name: /Alice.*2 unread.*draft/i })).toBeVisible();
      expect(screen.getByRole('button', { name: 'Me, Alice, Bob' })).toBeVisible();
      expect(screen.queryByPlaceholderText('Search conversations...')).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Search conversations' })).toBeVisible();
      expect(screen.getByRole('button', { name: 'Me, Alice, Bob' })).toHaveClass('active');
      expect(container.querySelector('.conversation-list--compact')).toBeInTheDocument();

      fireEvent.contextMenu(screen.getByRole('button', { name: /Alice, 2 unread, draft/i }));
      const viewProfileAction = screen.getByRole('button', { name: 'View Profile' });
      expect(viewProfileAction).toBeVisible();
      fireEvent.click(viewProfileAction);
      expect(screen.getByRole('dialog')).toBeVisible();
      expect(screen.getByText('@alice')).toBeVisible();
    });

    it('preserves an active search when the standard list rerenders compact', () => {
      useDMStore.setState({
        conversations: [
          makeConversation(),
          makeConversation({
            id: 'conv-2',
            participants: [
              { userId: 'user-1', username: 'me', displayName: 'Me' },
              { userId: 'user-3', username: 'bob', displayName: 'Bob' },
            ],
          }),
        ],
      });
      const { rerender } = render(
        <ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );
      fireEvent.change(screen.getByPlaceholderText('Search conversations...'), {
        target: { value: 'Alice' },
      });
      expect(screen.getByRole('button', { name: 'Alice' })).toBeVisible();
      expect(screen.queryByRole('button', { name: 'Bob' })).not.toBeInTheDocument();

      rerender(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      expect(screen.queryByPlaceholderText('Search conversations...')).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Alice' })).toBeVisible();
      expect(screen.queryByRole('button', { name: 'Bob' })).not.toBeInTheDocument();
    });

    it('opens search outward from the compact icon and filters conversations', () => {
      useDMStore.setState({
        conversations: [
          makeConversation(),
          makeConversation({
            id: 'conv-2',
            participants: [
              { userId: 'user-1', username: 'me', displayName: 'Me' },
              { userId: 'user-3', username: 'bob', displayName: 'Bob' },
            ],
          }),
        ],
      });
      render(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      const trigger = screen.getByRole('button', { name: 'Search conversations' });
      fireEvent.click(trigger);

      const popover = screen.getByRole('region', { name: 'Search conversations' });
      const input = screen.getByPlaceholderText('Search conversations...');
      expect(popover).toHaveAttribute('data-placement', 'right');
      expect(input).toHaveFocus();

      fireEvent.change(input, { target: { value: 'Bob' } });
      expect(screen.queryByRole('button', { name: 'Alice' })).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Bob' })).toBeVisible();

      fireEvent.keyDown(document, { key: 'Escape' });
      expect(screen.queryByPlaceholderText('Search conversations...')).not.toBeInTheDocument();
      expect(trigger).toHaveFocus();
    });

    it('opens Create Group DM from the compact rail (#1750)', () => {
      useDMStore.setState({ conversations: [makeConversation()] });
      const { container } = render(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      const createGroup = screen.getByRole('button', { name: 'Create Group DM' });
      expect(createGroup.querySelector('.lucide-messages-square')).toBeInTheDocument();
      expect(
        Array.from(container.querySelectorAll('.conversation-list--compact > button')).map(
          (button) => button.getAttribute('title')
        )
      ).toEqual(['Search conversations', 'Create Group DM', 'Personal Thread', 'Alice']);

      fireEvent.click(createGroup);
      expect(screen.getByRole('dialog', { name: 'Create Group DM' })).toBeVisible();
    });

    it.each([
      ['compact', 56],
      ['standard', 240],
    ] as const)(
      'keeps an unpinned %s rail open while Create Group DM owns focus (#1750)',
      (_presentation, width) => {
        const sidebarProfiles = useLayoutStore.getState().sidebarProfiles;
        useLayoutStore.setState({
          sidebarLayoutsDecoupled: true,
          sidebarProfiles: {
            ...sidebarProfiles,
            dm: { ...sidebarProfiles.dm, left: { width, pinned: false } },
          },
        });

        const { container } = render(
          <DockOverlayProvider>
            <DockShell
              context="dm"
              side="left"
              label="Threads"
              header={<span>DMs</span>}
              renderBody={(compact) => (
                <ConversationList
                  compact={compact}
                  selectedThreadId={null}
                  onSelectThread={mockOnSelectThread}
                />
              )}
            />
          </DockOverlayProvider>
        );
        const surface = container.querySelector('.dock-shell__surface') as HTMLElement;
        const lip = screen.getByRole('button', { name: 'Open Threads sidebar' });
        act(() => lip.focus());
        const trigger = screen.getByRole('button', { name: 'Create Group DM' });
        act(() => trigger.focus());

        fireEvent.click(trigger);

        expect(screen.getByRole('dialog', { name: 'Create Group DM' })).toBeVisible();
        expect(screen.getByPlaceholderText('Search friends...')).toHaveFocus();
        expect(surface).toHaveAttribute('data-state', 'open');

        fireEvent.keyDown(document, { key: 'Escape' });

        expect(screen.queryByRole('dialog', { name: 'Create Group DM' })).not.toBeInTheDocument();
        expect(trigger).toHaveFocus();
        expect(surface).toHaveAttribute('data-state', 'open');
      }
    );

    it('opens the personal thread from its compact button', async () => {
      const openPersonalThread = vi.fn().mockResolvedValue(personalThread);
      useDMStore.setState({
        conversations: [personalThread],
        openPersonalThread,
      });

      render(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );
      fireEvent.click(screen.getByRole('button', { name: 'Personal Thread' }));

      await waitFor(() => {
        expect(openPersonalThread).toHaveBeenCalledTimes(1);
        expect(mockOnSelectThread).toHaveBeenCalledWith('personal-1');
      });
    });

    it('keeps one-to-one presence and omits muted unread state', () => {
      useFriendStore.getState().addFriend({
        id: 'friendship-1',
        userId: 'user-2',
        username: 'alice',
        displayName: 'Alice',
        status: 'online',
      });
      useDMStore.setState({ conversations: [makeConversation({ unreadCount: 7 })] });
      useNotificationPrefsStore.getState().setMute('dm', 'conv-1', true, null);

      const { container } = render(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      expect(screen.getByRole('button', { name: 'Alice' })).not.toHaveClass('unread');
      expect(screen.getByRole('button', { name: 'Alice' })).not.toHaveAccessibleName(/unread/i);
      expect(container.querySelector('.conversation-unread-badge')).not.toBeInTheDocument();
      expect(container.querySelector('.member-status-dot.online')).toBeInTheDocument();
    });

    it('bounds unread counts and includes active call state in accessible names', () => {
      const group = makeConversation({
        id: 'group-1',
        isGroup: true,
        name: 'Study Group',
        participants: [
          { userId: 'user-1', username: 'me', displayName: 'Me' },
          { userId: 'user-2', username: 'alice', displayName: 'Alice' },
          { userId: 'user-3', username: 'bob', displayName: 'Bob' },
        ],
      });
      useDMStore.setState({
        conversations: [makeConversation({ unreadCount: 200 }), group],
      });
      useVoiceStore.getState().seedActiveDMCall('group-1', ['user-2', 'user-3'], 3);

      render(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      expect(screen.getByRole('button', { name: /Alice, 99\+ unread/i })).toBeVisible();
      expect(screen.getByRole('button', { name: /Study Group, 2 of 3 in call/i })).toBeVisible();
    });

    it('renders every conversation without an arbitrary compact-mode cap', () => {
      const conversations = Array.from({ length: 18 }, (_, index) =>
        makeConversation({
          id: `conv-${index}`,
          participants: [
            { userId: 'user-1', username: 'me', displayName: 'Me' },
            {
              userId: `user-${index + 2}`,
              username: `friend-${index + 1}`,
              displayName: `Friend ${index + 1}`,
            },
          ],
        })
      );
      useDMStore.setState({ conversations });

      render(
        <ConversationList compact selectedThreadId={null} onSelectThread={mockOnSelectThread} />
      );

      expect(screen.getByRole('button', { name: 'Friend 18' })).toBeVisible();
    });
  });

  describe('preview cache freshness and retry (#2364 review)', () => {
    const lastMessage = (content: string) => ({
      content,
      userId: 'user-2',
      username: 'alice',
      createdAt: '2025-01-01T12:00:00Z',
    });

    it('does not render a cached preview belonging to the previous message', async () => {
      (e2eeService as any).isInitialized = true;
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      // First ciphertext decrypts; the second never settles, so the only entry
      // in the cache is the one describing the message that is no longer last.
      decryptMock
        .mockResolvedValueOnce('first plaintext')
        .mockReturnValueOnce(new Promise(() => {}));

      useDMStore.setState({
        conversations: [makeConversation({ lastMessage: lastMessage('cipher-1') })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(screen.getByText('first plaintext')).toBeInTheDocument());

      act(() =>
        useDMStore.setState({
          conversations: [makeConversation({ lastMessage: lastMessage('cipher-2') })],
        })
      );

      // The stale entry is not data about this message. Rendering it produced
      // the previous message's text under the new message's sender and type.
      await waitFor(() => expect(screen.getByText('Encrypted message')).toBeInTheDocument());
      expect(screen.queryByText('first plaintext')).not.toBeInTheDocument();
      (e2eeService as any).isInitialized = false;
    });

    it('retries after a key that has not arrived yet, instead of latching the placeholder', async () => {
      (e2eeService as any).isInitialized = true;
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock
        .mockRejectedValueOnce(new E2EEKeyUnavailableError('NO_KEY_YET', true))
        .mockResolvedValueOnce('arrived late');

      useDMStore.setState({
        conversations: [makeConversation({ lastMessage: lastMessage('cipher-pending') })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));
      expect(screen.getByText('Encrypted message')).toBeInTheDocument();

      // Caching 'failed' for a pending key satisfied the effect's re-decrypt
      // predicate, so the placeholder stuck for the rest of the connection.
      act(() =>
        useDMStore.setState({
          conversations: [
            makeConversation({ lastMessage: lastMessage('cipher-pending'), unreadCount: 1 }),
          ],
        })
      );

      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(2));
      await waitFor(() => expect(screen.getByText('arrived late')).toBeInTheDocument());
      (e2eeService as any).isInitialized = false;
    });

    it('re-attempts the preview when the missing key is delivered', async () => {
      (e2eeService as any).isInitialized = true;
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock
        .mockRejectedValueOnce(new E2EEKeyUnavailableError('NO_KEY_YET', true))
        .mockResolvedValueOnce('decrypted once the key landed');

      useDMStore.setState({
        conversations: [makeConversation({ lastMessage: lastMessage('cipher-await-key') })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));
      expect(screen.getByText('Encrypted message')).toBeInTheDocument();

      // Nothing else changes: the arriving key touches neither `conversations`
      // nor `decryptedPreviews`, so without a listener the attempt is never
      // re-armed and the row stays on the placeholder indefinitely.
      act(() => {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-delivered', { detail: { channelId: 'conv-1' } })
        );
      });

      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(2));
      await waitFor(() =>
        expect(screen.getByText('decrypted once the key landed')).toBeInTheDocument()
      );
      (e2eeService as any).isInitialized = false;
    });

    it('ignores a key delivered for a different conversation', async () => {
      (e2eeService as any).isInitialized = true;
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock.mockRejectedValueOnce(new E2EEKeyUnavailableError('NO_KEY_YET', true));

      useDMStore.setState({
        conversations: [makeConversation({ lastMessage: lastMessage('cipher-other') })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));

      act(() => {
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-delivered', { detail: { channelId: 'some-other-conv' } })
        );
      });

      await Promise.resolve();
      expect(decryptMock).toHaveBeenCalledTimes(1);
      (e2eeService as any).isInitialized = false;
    });

    it('decrypts a message that arrives while the previous decrypt is still in flight', async () => {
      (e2eeService as any).isInitialized = true;
      const decryptMock = e2eeService.decryptForChannel as ReturnType<typeof vi.fn>;
      decryptMock
        .mockReturnValueOnce(new Promise(() => {}))
        .mockResolvedValueOnce('second plaintext');

      useDMStore.setState({
        conversations: [makeConversation({ lastMessage: lastMessage('cipher-1') })],
        fetchConversations: vi.fn().mockResolvedValue(undefined),
      });
      render(<ConversationList selectedThreadId={null} onSelectThread={mockOnSelectThread} />);
      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(1));

      // Keyed by conversation alone, this second ciphertext was refused by the
      // in-flight guard and then never re-armed.
      act(() =>
        useDMStore.setState({
          conversations: [makeConversation({ lastMessage: lastMessage('cipher-2') })],
        })
      );

      await waitFor(() => expect(decryptMock).toHaveBeenCalledTimes(2));
      expect(decryptMock).toHaveBeenLastCalledWith('conv-1', 'cipher-2');
      await waitFor(() => expect(screen.getByText('second plaintext')).toBeInTheDocument());
      (e2eeService as any).isInitialized = false;
    });
  });
});
