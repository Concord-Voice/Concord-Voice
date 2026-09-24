import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import DMConversationContextMenu from '@/renderer/components/DirectMessages/DMConversationContextMenu';
import type { DMConversation } from '@/renderer/stores/chat/dmStore';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
}));
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    rotateDMKey: vi.fn(),
    // dmStore reaches these from the menu's other actions (close conversation).
    revokeChannelAccess: vi.fn(),
    getChannelKey: vi.fn(),
    createChannelKeys: vi.fn(),
    isInitialized: true,
  },
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { DMRotationError } from '@/renderer/services/e2ee/e2eeErrors';

const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;
const mockRotateDMKey = vi.mocked(e2eeService.rotateDMKey);

const CURRENT_USER_ID = 'user-1';

function makeConversation(overrides: Partial<DMConversation> = {}): DMConversation {
  return {
    id: 'conv-1',
    isGroup: false,
    isPersonal: false,
    name: null,
    participants: [
      { userId: 'user-1', username: 'alice' },
      { userId: 'user-2', username: 'bob' },
    ],
    lastMessage: null,
    unreadCount: 0,
    createdAt: '2025-01-01T00:00:00Z',
    ...overrides,
  };
}

describe('DMConversationContextMenu', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  const renderMenu = (conversation: DMConversation, currentUserId = CURRENT_USER_ID) => {
    return render(
      <DMConversationContextMenu
        conversation={conversation}
        currentUserId={currentUserId}
        position={{ x: 100, y: 100 }}
        onClose={mockOnClose}
      />
    );
  };

  it('shows Rotate Encryption Key for encrypted 1:1 DM', () => {
    renderMenu(makeConversation());
    expect(screen.getByText('Rotate Encryption Key')).toBeInTheDocument();
  });

  it('always shows Mute Conversation regardless of conversation type', () => {
    // The "No actions available" fallback was removed with issue #84 —
    // the mute action is always available, so the menu can never be empty
    // for any conversation. The unencrypted-DM case that used to live here
    // was removed when main dropped is_encrypted (migration
    // 000062_remove_is_encrypted) — every DM is now encrypted by definition.
    renderMenu(makeConversation());
    expect(screen.getByText('Mute Conversation')).toBeInTheDocument();
  });

  it('shows Rotate Encryption Key for group DM when user is creator', () => {
    renderMenu(
      makeConversation({
        isGroup: true,
        createdBy: CURRENT_USER_ID,
        name: 'Test Group',
      })
    );
    expect(screen.getByText('Rotate Encryption Key')).toBeInTheDocument();
  });

  it('hides Rotate Encryption Key for group DM when user is not creator', () => {
    renderMenu(
      makeConversation({
        isGroup: true,
        createdBy: 'user-other',
        name: 'Test Group',
      })
    );
    expect(screen.queryByText('Rotate Encryption Key')).not.toBeInTheDocument();
    // Mute is always present — see the unencrypted-conversation case above.
    expect(screen.getByText('Mute Conversation')).toBeInTheDocument();
  });

  // The rotation goes through e2eeService.rotateDMKey, which wraps the
  // successor for every participant before it posts. A bare POST to the
  // route revoked the only epoch anyone held (prod, 2026-09-17).
  it('rotates through e2eeService with every participant and shows success', async () => {
    mockRotateDMKey.mockResolvedValue({ ok: true, status: 200 } as Response);
    const conv = makeConversation({ id: 'conv-42' });
    renderMenu(conv);

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(mockRotateDMKey).toHaveBeenCalledWith('conv-42', ['user-1', 'user-2']);
    });
    expect(mockApiFetch).not.toHaveBeenCalledWith(
      '/api/v1/dm/conversations/conv-42/rotate-key',
      expect.anything()
    );

    await waitFor(() => {
      expect(screen.getByText('Key Rotated!')).toBeInTheDocument();
    });
  });

  it('shows rate limit message on 429', async () => {
    mockRotateDMKey.mockResolvedValue({
      ok: false,
      status: 429,
      json: async () => ({ retry_after: 7200 }),
    } as unknown as Response);
    renderMenu(makeConversation());

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('Try again in 2h')).toBeInTheDocument();
    });
  });

  it('shows error message on non-429 failure', async () => {
    mockRotateDMKey.mockResolvedValue({
      ok: false,
      status: 403,
      json: async () => ({ error: 'Forbidden' }),
    } as unknown as Response);
    renderMenu(makeConversation());

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('Forbidden')).toBeInTheDocument();
    });
  });

  it('shows the precondition a DMRotationError names', async () => {
    mockRotateDMKey.mockRejectedValue(
      new DMRotationError('A participant has no encryption key yet')
    );
    renderMenu(makeConversation());

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('A participant has no encryption key yet')).toBeInTheDocument();
    });
  });

  it('shows fallback error message on network failure', async () => {
    mockRotateDMKey.mockRejectedValue(new Error('Network error'));
    renderMenu(makeConversation());

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });
  });

  // ─── #984 menu expansion ──────────────────────────────────────────────

  describe('Mark as Read (#984)', () => {
    it('is hidden when conversation has no unread messages', () => {
      renderMenu(makeConversation({ unreadCount: 0 }));
      expect(screen.queryByText('Mark as Read')).not.toBeInTheDocument();
    });

    it('is visible when conversation has unread messages', () => {
      renderMenu(makeConversation({ unreadCount: 3 }));
      expect(screen.getByText('Mark as Read')).toBeInTheDocument();
    });

    it('clears unread count optimistically + rolls back on API rejection (#984 coverage)', async () => {
      // Pre-populate dmStore with a conversation that has unreads — so we can
      // observe the optimistic clear + rollback round-trip.
      const { useDMStore } = await import('@/renderer/stores/chat/dmStore');
      useDMStore.setState({
        conversations: [{ ...makeConversation({ id: 'conv-rollback', unreadCount: 7 }) }],
      });

      mockApiFetch.mockRejectedValue(new Error('Network down'));
      renderMenu(makeConversation({ id: 'conv-rollback', unreadCount: 7 }));

      fireEvent.click(screen.getByText('Mark as Read'));

      // Optimistic clear happens synchronously (clearUnread in dmStore).
      await waitFor(() => {
        const conv = useDMStore.getState().conversations.find((c) => c.id === 'conv-rollback');
        // After rollback, unreadCount should be restored to the previous value.
        expect(conv?.unreadCount).toBe(7);
      });
    });

    it('clears unread count + rolls back on non-ok HTTP response (#984 coverage)', async () => {
      const { useDMStore } = await import('@/renderer/stores/chat/dmStore');
      useDMStore.setState({
        conversations: [{ ...makeConversation({ id: 'conv-nonok', unreadCount: 4 }) }],
      });

      mockApiFetch.mockResolvedValue({ ok: false, status: 500 });
      renderMenu(makeConversation({ id: 'conv-nonok', unreadCount: 4 }));

      fireEvent.click(screen.getByText('Mark as Read'));

      // Non-ok response throws → caught → rollback restores the unread count.
      await waitFor(() => {
        const conv = useDMStore.getState().conversations.find((c) => c.id === 'conv-nonok');
        expect(conv?.unreadCount).toBe(4);
      });
    });

    // Note: the `if (previousUnread > 0)` guard in handleMarkAsRead is
    // structurally defensive — under React closure semantics, previousUnread
    // is captured at click time from the conversation prop, and the menu
    // item only renders when `conversation.unreadCount > 0`. So
    // previousUnread is always > 0 at the catch branch in practice. We do
    // not test the false branch because there is no realistic scenario that
    // exercises it. The guard stays as defense-in-depth in case the
    // visibility gate ever changes. Gitar review on PR #1180 caught the
    // missing-meaningful-test issue (a vacuous-pass test was removed in
    // favor of this rationale comment).

    it('calls the mark-read API on click', async () => {
      mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
      renderMenu(makeConversation({ id: 'conv-99', unreadCount: 5 }));

      fireEvent.click(screen.getByText('Mark as Read'));

      await waitFor(() => {
        expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/dm/conversations/conv-99/read', {
          method: 'POST',
        });
      });
      expect(mockOnClose).toHaveBeenCalled();
    });
  });

  describe('thread removal actions (#2822)', () => {
    it('shows Hide and Clear for a non-personal 1:1, with Purge separated', () => {
      render(
        <DMConversationContextMenu
          conversation={makeConversation()}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onPurgeMessages={vi.fn()}
          onHideThread={vi.fn()}
          onClearHistory={vi.fn()}
        />
      );

      expect(screen.getByRole('button', { name: 'Hide thread' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Clear history for me' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Close Conversation' })).toBeNull();
    });

    it('keeps removal actions and Purge off personal notes', () => {
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isPersonal: true })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onPurgeMessages={vi.fn()}
          onHideThread={vi.fn()}
          onClearHistory={vi.fn()}
          onLeaveGroup={vi.fn()}
        />
      );

      expect(screen.queryByRole('button', { name: 'Hide thread' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Clear history for me' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Purge Messages' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Leave group' })).toBeNull();
    });

    it('adds a separate Leave group action only for groups', () => {
      const onLeaveGroup = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isGroup: true, name: 'Test Group' })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onPurgeMessages={vi.fn()}
          onHideThread={vi.fn()}
          onClearHistory={vi.fn()}
          onLeaveGroup={onLeaveGroup}
        />
      );

      expect(screen.getByRole('button', { name: 'Hide thread' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Clear history for me' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Leave group' })).toBeInTheDocument();
    });

    it.each([
      ['Hide thread', 'onHideThread'],
      ['Clear history for me', 'onClearHistory'],
      ['Purge Messages', 'onPurgeMessages'],
    ] as const)('calls the parent callback for %s and closes the menu', (label, callbackName) => {
      const callbacks = {
        onHideThread: vi.fn(),
        onClearHistory: vi.fn(),
        onPurgeMessages: vi.fn(),
      };
      const conversation = makeConversation({ id: 'conv-removal' });
      render(
        <DMConversationContextMenu
          conversation={conversation}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          {...callbacks}
        />
      );

      fireEvent.click(screen.getByRole('button', { name: label }));

      expect(callbacks[callbackName]).toHaveBeenCalledWith(conversation);
      expect(mockOnClose).toHaveBeenCalled();
    });

    it('calls the parent callback for Leave group and never mutates the store directly', async () => {
      const { useDMStore } = await import('@/renderer/stores/chat/dmStore');
      const removeSpy = vi.fn();
      useDMStore.setState({ removeConversation: removeSpy });
      const onLeaveGroup = vi.fn();
      const conversation = makeConversation({ id: 'group-to-leave', isGroup: true });

      render(
        <DMConversationContextMenu
          conversation={conversation}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onLeaveGroup={onLeaveGroup}
        />
      );
      fireEvent.click(screen.getByRole('button', { name: 'Leave group' }));

      expect(onLeaveGroup).toHaveBeenCalledWith(conversation);
      expect(removeSpy).not.toHaveBeenCalled();
    });
  });

  describe('Block User (#984)', () => {
    it('is HIDDEN when no onBlockUser callback supplied', () => {
      // Default renderMenu doesn't pass the callback
      renderMenu(makeConversation());
      expect(screen.queryByText('Block User')).not.toBeInTheDocument();
    });

    it('is visible for 1:1 DMs when onBlockUser is supplied', () => {
      const onBlockUser = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation()}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onBlockUser={onBlockUser}
        />
      );
      expect(screen.getByText('Block User')).toBeInTheDocument();
    });

    it('is HIDDEN for group DMs', () => {
      const onBlockUser = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isGroup: true, name: 'Test Group' })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onBlockUser={onBlockUser}
        />
      );
      expect(screen.queryByText('Block User')).not.toBeInTheDocument();
    });

    it('is HIDDEN for personal DMs', () => {
      const onBlockUser = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isPersonal: true })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onBlockUser={onBlockUser}
        />
      );
      expect(screen.queryByText('Block User')).not.toBeInTheDocument();
    });

    it('invokes onBlockUser callback on click (lifted modal pattern)', () => {
      const onBlockUser = vi.fn();
      const conv = makeConversation();
      render(
        <DMConversationContextMenu
          conversation={conv}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onBlockUser={onBlockUser}
        />
      );

      fireEvent.click(screen.getByText('Block User'));

      expect(onBlockUser).toHaveBeenCalledWith(conv);
      expect(mockOnClose).toHaveBeenCalled();
    });
  });

  describe('Unfriend (#984)', () => {
    it('is HIDDEN for group DMs even when peer is friend', () => {
      const onUnfriend = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isGroup: true, name: 'Test Group' })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onUnfriend={onUnfriend}
        />
      );
      expect(screen.queryByText('Unfriend')).not.toBeInTheDocument();
    });

    it('is HIDDEN for 1:1 DMs when peer is not a friend (no friend in store)', () => {
      const onUnfriend = vi.fn();
      // With a clean store, friends list is empty by default -> peer not a friend
      render(
        <DMConversationContextMenu
          conversation={makeConversation()}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onUnfriend={onUnfriend}
        />
      );
      expect(screen.queryByText('Unfriend')).not.toBeInTheDocument();
    });
  });

  describe('Voice Call menu item (#1219)', () => {
    it('shows Voice Call for 1:1 DMs', () => {
      renderMenu(makeConversation());
      expect(screen.getByText('Voice Call')).toBeInTheDocument();
    });

    it('shows Voice Call for group DMs (#1219 flips the 1:1 gate)', () => {
      renderMenu(makeConversation({ isGroup: true, name: 'Test Group' }));
      expect(screen.getByText('Voice Call')).toBeInTheDocument();
    });

    it('hides Voice Call for personal DMs (self-notes)', () => {
      renderMenu(makeConversation({ isPersonal: true }));
      expect(screen.queryByText('Voice Call')).not.toBeInTheDocument();
    });
  });

  describe('View Profile menu item (#1208)', () => {
    it('shows "View Profile" for 1:1 DMs when onViewProfile is provided', () => {
      const onViewProfile = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation()}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onViewProfile={onViewProfile}
        />
      );
      expect(screen.getByText('View Profile')).toBeInTheDocument();
    });

    it('hides "View Profile" for group DMs', () => {
      const onViewProfile = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isGroup: true, name: 'Test Group' })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onViewProfile={onViewProfile}
        />
      );
      expect(screen.queryByText('View Profile')).not.toBeInTheDocument();
    });

    it('hides "View Profile" for personal DMs (self-notes)', () => {
      const onViewProfile = vi.fn();
      render(
        <DMConversationContextMenu
          conversation={makeConversation({ isPersonal: true })}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onViewProfile={onViewProfile}
        />
      );
      expect(screen.queryByText('View Profile')).not.toBeInTheDocument();
    });

    it('clicking "View Profile" invokes onViewProfile(conversation) then closes the menu', () => {
      const onViewProfile = vi.fn();
      const conv = makeConversation({ id: 'conv-42' });
      render(
        <DMConversationContextMenu
          conversation={conv}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
          onViewProfile={onViewProfile}
        />
      );
      fireEvent.click(screen.getByText('View Profile'));
      expect(onViewProfile).toHaveBeenCalledWith(conv);
      expect(mockOnClose).toHaveBeenCalled();
    });

    it('hides "View Profile" entirely when onViewProfile prop is undefined', () => {
      render(
        <DMConversationContextMenu
          conversation={makeConversation()}
          currentUserId={CURRENT_USER_ID}
          position={{ x: 100, y: 100 }}
          onClose={mockOnClose}
        />
      );
      expect(screen.queryByText('View Profile')).not.toBeInTheDocument();
    });
  });
});
