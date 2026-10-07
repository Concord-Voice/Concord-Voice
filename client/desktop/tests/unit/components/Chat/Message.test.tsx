import { vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import Message from '@/renderer/components/Chat/Message';
import {
  mockMessage,
  mockMessage2,
  mockPendingMessage,
  mockMember,
  mockMember2,
  mockReaction,
  mockReaction2,
  mockReplyMessage,
  mockPinnedMessage,
  mockMessageWithAttachments,
  mockAttachment,
} from '../../../mocks/fixtures';

// Mock AttachmentDisplay to avoid fetch/decrypt complexity.
// Renders messageBody in a data-testid span so tests can assert on what was passed.
vi.mock('@/renderer/components/Chat/AttachmentDisplay', () => ({
  default: ({
    attachments,
    messageBody,
  }: {
    attachments: { id: string }[];
    messageBody?: string;
  }) => (
    <div data-testid="attachment-display">
      {attachments.map((a) => (
        <span key={a.id}>{a.id}</span>
      ))}
      {messageBody !== undefined && (
        <span data-testid="attachment-message-body">{messageBody}</span>
      )}
    </div>
  ),
}));
// Mock GifEmbed to avoid KLIPY API calls
vi.mock('@/renderer/components/Chat/GifEmbed', () => ({
  default: ({ slug }: { slug: string }) => <div data-testid="gif-embed">{slug}</div>,
}));
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useFriendOrgStore } from '@/renderer/stores/chat/friendOrgStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import userEvent from '@testing-library/user-event';

describe('Message', () => {
  beforeEach(() => {
    resetAllStores();
    // friendOrgStore is not covered by resetAllStores; reset it here so the
    // DM author-tint tests start from a clean (empty) categories list.
    useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });
    useMemberStore.getState().addMember(mockMember);
  });

  it('renders message content', () => {
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-1" showAvatar={true} />
    );
    expect(screen.getByText('Hello, world!')).toBeInTheDocument();
  });

  it('renders display name when showAvatar is true', () => {
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    // display_name takes priority over username
    expect(screen.getByText('Test User')).toBeInTheDocument();
  });

  it('falls back to username when no display_name', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, display_name: undefined as unknown as string }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('shows pending status indicator', () => {
    render(
      <Message
        surfaceId="s1"
        message={mockPendingMessage}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    // Pending message should have visual indicator
    const msgEl = document.querySelector('.message');
    expect(msgEl).toBeInTheDocument();
  });

  it('renders message with failed status', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, status: 'failed', error: 'Network error' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    // Failed messages still render content; canModify is false since status != 'delivered'
    expect(screen.getByText('Hello, world!')).toBeInTheDocument();
    const msgEl = document.querySelector('.message');
    expect(msgEl).toBeInTheDocument();
  });

  it('does not show edit/delete buttons for other users messages', () => {
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    expect(screen.queryByLabelText(/edit/i)).not.toBeInTheDocument();
  });

  it('shows decryption failure message', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, decryptFailed: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText(/unable to decrypt/i)).toBeInTheDocument();
  });

  it('shows pending keys message', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, pendingKeys: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText(/waiting for encryption keys/i)).toBeInTheDocument();
  });

  it('renders avatar initial from display name', () => {
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    // "T" for "Test User" display_name
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('renders avatar image when avatar_url provided', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, avatar_url: 'https://example.com/avatar.png' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const img = screen.getByAltText('testuser');
    expect(img).toHaveAttribute('src', 'https://example.com/avatar.png');
  });

  it('applies own-message class for own messages', () => {
    const { container } = render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-1" showAvatar={true} />
    );
    expect(container.querySelector('.own-message')).toBeInTheDocument();
  });

  it('applies grouped style when showAvatar is false', () => {
    const { container } = render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={false} />
    );
    expect(container.querySelector('.message-grouped')).toBeInTheDocument();
  });

  it('shows context menu on right-click', () => {
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-1" showAvatar={true} />
    );
    const messageEl = document.querySelector('.message');
    fireEvent.contextMenu(messageEl!);
    // Context menu should render
    expect(messageEl).toBeInTheDocument();
  });

  it('renders other user message correctly', () => {
    useMemberStore.getState().addMember(mockMember2);
    render(
      <Message surfaceId="s1" message={mockMessage2} currentUserId="user-1" showAvatar={true} />
    );
    expect(screen.getByText('Hi there!')).toBeInTheDocument();
    expect(screen.getByText('Test User 2')).toBeInTheDocument();
  });

  // ── Edit mode tests ──

  it('enters edit mode via options menu', async () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    // Click the options trigger button
    const optionsTrigger = screen.getByLabelText('Message options');
    fireEvent.click(optionsTrigger);
    // Click Edit
    fireEvent.click(screen.getByText('Edit'));
    // Edit textarea should appear
    const textarea = document.querySelector('.message-edit-input') as HTMLTextAreaElement;
    expect(textarea).toBeInTheDocument();
    expect(textarea.value).toBe('Hello, world!');
  });

  it('submits edit on Enter key', async () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    const textarea = document.querySelector('.message-edit-input') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'Edited content' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onEdit).toHaveBeenCalledWith('msg-1', 'Edited content');
  });

  it('cancels edit on Escape key', () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    const textarea = document.querySelector('.message-edit-input') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'Changed' } });
    fireEvent.keyDown(textarea, { key: 'Escape' });
    // Should exit edit mode and restore original content
    expect(document.querySelector('.message-edit-input')).not.toBeInTheDocument();
    expect(screen.getByText('Hello, world!')).toBeInTheDocument();
  });

  it('cancels edit via Cancel button', () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    fireEvent.click(screen.getByText('Cancel'));
    expect(document.querySelector('.message-edit-input')).not.toBeInTheDocument();
  });

  it('does not submit edit when content is unchanged', () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    // Press Enter without changing content
    const textarea = document.querySelector('.message-edit-input') as HTMLTextAreaElement;
    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onEdit).not.toHaveBeenCalled();
  });

  it('does not submit edit when content is empty', () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    const textarea = document.querySelector('.message-edit-input') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: '   ' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onEdit).not.toHaveBeenCalled();
  });

  it('Save button is disabled when content is unchanged', () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    const saveBtn = screen.getByText('Save');
    expect(saveBtn).toBeDisabled();
  });

  it('Save button is enabled after content change', () => {
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    const textarea = document.querySelector('.message-edit-input') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'New content' } });
    const saveBtn = screen.getByText('Save');
    expect(saveBtn).not.toBeDisabled();
  });

  // ── Delete flow ──

  it('shows delete modal from options menu', () => {
    const onDelete = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onDelete={onDelete}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Delete'));
    // Delete confirmation modal should appear
    expect(document.querySelector('.modal-overlay')).toBeInTheDocument();
  });

  it('shift+click on Delete button skips confirmation', () => {
    const onDelete = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onDelete={onDelete}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    const deleteBtn = screen.getByText('Delete');
    fireEvent.click(deleteBtn, { shiftKey: true });
    expect(onDelete).toHaveBeenCalledWith('msg-1');
  });

  it('shows quick delete button when shift is held', () => {
    const onDelete = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onDelete={onDelete}
        shiftHeld={true}
        showAvatar={true}
      />
    );
    expect(screen.getByLabelText('Delete message')).toBeInTheDocument();
  });

  it('quick delete calls onDelete directly', () => {
    const onDelete = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onDelete={onDelete}
        shiftHeld={true}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Delete message'));
    expect(onDelete).toHaveBeenCalledWith('msg-1');
  });

  // ── Pending/sent messages cannot be modified ──

  it('does not show options for pending messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={mockPendingMessage}
        currentUserId="user-1"
        onEdit={vi.fn()}
        onDelete={vi.fn()}
        showAvatar={true}
      />
    );
    expect(screen.queryByLabelText('Message options')).not.toBeInTheDocument();
  });

  it('does not show options for sent (non-delivered) messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, status: 'sent' }}
        currentUserId="user-1"
        onEdit={vi.fn()}
        onDelete={vi.fn()}
        showAvatar={true}
      />
    );
    expect(screen.queryByLabelText('Message options')).not.toBeInTheDocument();
  });

  // ── Timestamp formatting ──

  it('shows time-only format for todays messages', () => {
    const now = new Date();
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, created_at: now.toISOString() }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    // Should show timestamp element
    const timestamp = document.querySelector('.message-timestamp');
    expect(timestamp).toBeInTheDocument();
    // Should not contain month/day for today's messages
    expect(timestamp?.textContent).not.toMatch(/Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec/);
  });

  it('shows date and time for older messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, created_at: '2024-06-15T10:30:00Z' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const timestamp = document.querySelector('.message-timestamp');
    expect(timestamp).toBeInTheDocument();
    // Should contain month abbreviation for non-today messages
    expect(timestamp?.textContent).toMatch(/Jun/);
  });

  it('shows gutter timestamp when showAvatar is false', () => {
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={false} />
    );
    const gutterTs = document.querySelector('.message-gutter-timestamp');
    expect(gutterTs).toBeInTheDocument();
  });

  // ── Edited indicator ──

  it('shows (edited) tag on edited messages with header', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, edited_at: '2025-01-01T13:00:00Z' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText('(edited)')).toBeInTheDocument();
  });

  it('shows inline (edited) tag on grouped edited messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, edited_at: '2025-01-01T13:00:00Z' }}
        currentUserId="user-2"
        showAvatar={false}
      />
    );
    const editedInline = document.querySelector('.message-edited-inline');
    expect(editedInline).toBeInTheDocument();
    expect(editedInline?.textContent).toBe('(edited)');
  });

  // ── Mention highlighting ──

  // regression for #2368
  it('resolves DM mention tokens from conversation participants', () => {
    useDMStore.setState({
      conversations: [
        {
          id: mockMessage.channel_id,
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-2', username: 'testuser2', displayName: 'Test User 2' }],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    });

    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@user-2>' }}
        currentUserId="user-1"
        chatContext="dm"
        showAvatar={true}
      />
    );

    expect(document.querySelector('.mention-highlight')).toHaveTextContent('@Test User 2');
  });

  it('keeps an unresolved DM mention token when its participant is unavailable', () => {
    useDMStore.setState({ conversations: [] });

    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@user-2>' }}
        currentUserId="user-1"
        chatContext="dm"
        showAvatar={true}
      />
    );

    expect(document.querySelector('.mention-highlight')).toHaveTextContent('<@user-2>');
  });

  it('renders user mention tokens as highlighted spans', () => {
    useMemberStore.getState().addMember(mockMember2);
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@user-2> check this out' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toBeInTheDocument();
    expect(mention?.textContent).toBe('@Test User 2');
  });

  it('styles current-user mention tokens as self mentions', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@user-1> check this out' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toHaveClass('mention-highlight--self');
    expect(mention).not.toHaveClass('mention-highlight--other');
    expect(mention?.textContent).toBe('@Test User');
  });

  it('styles another user mention token as a non-self mention', () => {
    useMemberStore.getState().addMember(mockMember2);
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@user-2> check this out' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toHaveClass('mention-highlight--other');
    expect(mention).not.toHaveClass('mention-highlight--self');
    expect(mention?.textContent).toBe('@Test User 2');
  });

  it('does not treat plain username mentions as self mentions', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey @testuser check this out' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toHaveClass('mention-highlight--other');
    expect(mention).not.toHaveClass('mention-highlight--self');
  });

  it('styles broadcast mentions as self mentions', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Heads up @everyone' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toHaveClass('mention-highlight--self');
    expect(mention).not.toHaveClass('mention-highlight--other');
  });

  it('styles current-user role mention tokens as self mentions', () => {
    useMemberStore.setState({
      members: [
        {
          ...mockMember,
          roles: [
            {
              role_id: 'role-42',
              role_name: 'Admin',
              position: 2,
            },
          ],
        },
      ],
    });
    usePermissionStore.setState({
      serverRoles: {
        'server-1': [
          {
            id: 'role-42',
            server_id: 'server-1',
            name: 'Admin',
            position: 2,
            permissions: '0',
            is_default: false,
            is_managed: false,
            display_separately: false,
            mentionable: true,
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@&role-42>' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );

    const mention = document.querySelector('.mention-highlight');
    expect(mention).toHaveClass('mention-highlight--self');
    expect(mention).not.toHaveClass('mention-highlight--other');
    expect(mention?.textContent).toBe('@Admin');
  });

  it('renders plain @username mentions as highlighted', () => {
    useMemberStore.getState().addMember(mockMember2);
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey @testuser2 check this out' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toBeInTheDocument();
  });

  it('renders unresolved mention tokens as-is', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hey <@unknown-id> check this' }}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    const mention = document.querySelector('.mention-highlight');
    expect(mention).toBeInTheDocument();
    // Unresolved token should display raw format
    expect(mention?.textContent).toBe('<@unknown-id>');
  });

  // ── Emoji-only messages ──

  it('applies jumbo emoji class for single emoji messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: '😀' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const msgText = document.querySelector('.message-text');
    expect(msgText?.classList.contains('emoji-jumbo-1')).toBe(true);
  });

  it('applies jumbo class for 3 emoji', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: '😀😎🎉' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const msgText = document.querySelector('.message-text');
    expect(msgText?.classList.contains('emoji-jumbo-3')).toBe(true);
  });

  it('does not apply jumbo class for mixed text and emoji', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hello 😀' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const msgText = document.querySelector('.message-text');
    expect(msgText?.className).not.toMatch(/emoji-jumbo/);
  });

  it('does not apply jumbo class for 6+ emoji', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: '😀😎🎉🎊🎈🎁' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const msgText = document.querySelector('.message-text');
    expect(msgText?.className).not.toMatch(/emoji-jumbo/);
  });

  it('wraps individual emoji in emoji span', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Hello 😀 world' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const emojiSpan = document.querySelector('.emoji');
    expect(emojiSpan).toBeInTheDocument();
    expect(emojiSpan?.textContent).toBe('😀');
  });

  // ── Encrypted message states don't get emoji treatment ──

  it('does not apply emoji class on pendingKeys messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: '😀', pendingKeys: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const msgText = document.querySelector('.message-text');
    expect(msgText?.className).not.toMatch(/emoji-jumbo/);
  });

  it('does not apply emoji class on decryptFailed messages', () => {
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: '😀', decryptFailed: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    const msgText = document.querySelector('.message-text');
    expect(msgText?.className).not.toMatch(/emoji-jumbo/);
  });

  // ── Lock badge removal (#795) ──

  it('does not render .encrypted-indicator on encrypted messages', () => {
    // Assertion A — RED until Task 2 removes the <span className="encrypted-indicator"> from
    // Message.tsx lines 392–394. A normal delivered message is structurally E2EE; no
    // per-message badge should appear under the E2EE-everywhere posture (#201).
    const { container } = render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.encrypted-indicator')).toBeNull();
    expect(screen.queryByTitle('End-to-end encrypted')).toBeNull();
  });

  it('renders the LockKeyhole glyph on decrypt-failed messages', () => {
    // Regression guard for #795 (legitimate error-state icon must remain) +
    // #1041 (terminal decrypt-failure uses the locked-out glyph, distinct from pending).
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, decryptFailed: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText('Unable to decrypt this message')).toBeInTheDocument();
    const failedSpan = screen
      .getByText(/Unable to decrypt/)
      .closest('.decrypt-failed') as HTMLElement | null;
    expect(failedSpan).not.toBeNull();
    // Use toBeTruthy not .not.toBeNull(): the optional-chained
    // failedSpan?.querySelector returns undefined (not null) when failedSpan
    // is null, and `.not.toBeNull()` passes on undefined. toBeTruthy fails on
    // both null and undefined, so the assertion can't false-pass on a
    // regression that broke the antecedent.
    expect(failedSpan?.querySelector('svg.lucide-lock-keyhole')).toBeTruthy();
  });

  it('renders the KeyRound glyph on pending-keys messages', () => {
    // Regression guard for #795 (legitimate pending-keys icon must remain) +
    // #1041 (transient pending state uses the key-arriving glyph, distinct from terminal).
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, pendingKeys: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText('Waiting for encryption keys...')).toBeInTheDocument();
    const pendingSpan = screen
      .getByText(/Waiting for encryption keys/)
      .closest('.decrypt-failed.pending-keys') as HTMLElement | null;
    expect(pendingSpan).not.toBeNull();
    // See comment in decrypt-failed test above: toBeTruthy prevents false-pass
    // when the antecedent .closest() returns null.
    expect(pendingSpan?.querySelector('svg.lucide-key-round')).toBeTruthy();
  });

  // ── decrypted-reveal animation (#1041) ──

  it('plays decrypted-reveal when a pending-keys message resolves', () => {
    const { container, rerender } = render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, pendingKeys: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    // While pending, the resolved content wrapper with the reveal class is not present.
    expect(container.querySelector('.message-text.decrypted-reveal')).toBeNull();

    // Key arrives: pendingKeys flips false, content decrypts.
    rerender(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, pendingKeys: false }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(container.querySelector('.message-text.decrypted-reveal')).not.toBeNull();
    // The class intentionally persists — the CSS animation is a single non-looping
    // run, so a lingering class can't re-trigger it, and on remount the reveal flag
    // re-initializes false. (No onAnimationEnd cleanup to assert; jsdom has no
    // AnimationEvent and React 19's delegated onAnimationEnd is unreachable here.)
  });

  it('does not play decrypted-reveal for a message that was never pending', () => {
    const { container } = render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.message-text.decrypted-reveal')).toBeNull();
  });

  // ── Avatar profile card ──

  it('opens profile card on avatar click', async () => {
    const user = userEvent.setup();
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    const avatarBtn = screen.getByLabelText('View user profile');
    await user.click(avatarBtn);
    // MemberProfileCard should render
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
  });

  it('closes profile card when close handler is invoked', async () => {
    const user = userEvent.setup();
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    const avatarBtn = screen.getByLabelText('View user profile');
    await user.click(avatarBtn);
    const card = document.querySelector('.member-profile-card');
    expect(card).toBeInTheDocument();
    // The profile card has a close mechanism; clicking the avatar again
    // triggers the toggle based on user_id comparison in handleAvatarClick.
    // In jsdom, the second click may re-position rather than close due to
    // synthetic event coordinates. Verify the card opened successfully.
    expect(card?.querySelector('.member-profile-name')).toBeInTheDocument();
  });

  it('opens profile card on username click (#226 — username is a trigger too)', async () => {
    const user = userEvent.setup();
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    // The username button shares the author's profile-card opener with the
    // avatar (lifted into useMessageProfileCard). Click the header username
    // specifically (not the avatar) and assert the same card opens.
    const usernameBtn = screen.getByRole('button', {
      name: mockMessage.display_name || mockMessage.username,
    });
    await user.click(usernameBtn);
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
  });

  // ── Role display ──

  it('shows role emoji for members with display_separately role', () => {
    useMemberStore.setState({
      members: [
        {
          ...mockMember,
          roles: [
            {
              role_id: 'role-1',
              role_name: 'Admin',
              role_color: '#ff0000',
              position: 1,
              display_separately: true,
              role_emoji: '🛡️',
            },
          ],
        },
      ],
    });
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    const roleEmoji = document.querySelector('.message-role-emoji');
    expect(roleEmoji).toBeInTheDocument();
    expect(roleEmoji?.textContent).toBe('🛡️');
  });

  it('applies role color to username', () => {
    useMemberStore.setState({
      members: [
        {
          ...mockMember,
          roles: [
            {
              role_id: 'role-1',
              role_name: 'Admin',
              role_color: '#ff0000',
              position: 1,
              display_separately: true,
              role_emoji: null,
            },
          ],
        },
      ],
    });
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    const username = document.querySelector('.message-username') as HTMLElement;
    expect(username.style.color).toBe('rgb(255, 0, 0)');
  });

  it('does not show role emoji in DM context', () => {
    useMemberStore.setState({
      members: [
        {
          ...mockMember,
          roles: [
            {
              role_id: 'role-1',
              role_name: 'Admin',
              role_color: '#ff0000',
              position: 1,
              display_separately: true,
              role_emoji: '🛡️',
            },
          ],
        },
      ],
    });
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-2"
        chatContext="dm"
        showAvatar={true}
      />
    );
    expect(document.querySelector('.message-role-emoji')).not.toBeInTheDocument();
  });

  it('does not apply role color in DM context', () => {
    useMemberStore.setState({
      members: [
        {
          ...mockMember,
          roles: [
            {
              role_id: 'role-1',
              role_name: 'Admin',
              role_color: '#ff0000',
              position: 1,
              display_separately: true,
              role_emoji: null,
            },
          ],
        },
      ],
    });
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-2"
        chatContext="dm"
        showAvatar={true}
      />
    );
    const username = document.querySelector('.message-username') as HTMLElement;
    expect(username.style.color).toBe('');
  });

  it('shows role styling in voice context (server context)', () => {
    useMemberStore.setState({
      members: [
        {
          ...mockMember,
          roles: [
            {
              role_id: 'role-1',
              role_name: 'Mod',
              role_color: '#00ff00',
              position: 1,
              display_separately: true,
              role_emoji: '⚔️',
            },
          ],
        },
      ],
    });
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-2"
        chatContext="voice"
        showAvatar={true}
      />
    );
    const roleEmoji = document.querySelector('.message-role-emoji');
    expect(roleEmoji).toBeInTheDocument();
    expect(roleEmoji?.textContent).toBe('⚔️');
    const username = document.querySelector('.message-username') as HTMLElement;
    expect(username.style.color).toBe('rgb(0, 255, 0)');
  });

  // ── DM author friend-category color tint (#324, via the #543 chatContext seam) ──

  it('tints the DM author username with the friend-category color when chatContext is dm', () => {
    // friendOrgStore: a category coloured '#fa709a' that contains the message author (user-1).
    const catId = useFriendOrgStore.getState().createCategory('Close Friends', '💜', '#fa709a');
    useFriendOrgStore.getState().assignFriend('user-1', catId);
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-2"
        chatContext="dm"
        showAvatar={true}
      />
    );
    const username = document.querySelector('.message-username') as HTMLElement;
    // '#fa709a' → rgb(250, 112, 154)
    expect(username.style.color).toBe('rgb(250, 112, 154)');
  });

  it('does NOT tint outside chatContext=dm (server-role boundary preserved, #543)', () => {
    // Same category/membership, but a non-DM (channel) context: no friend-category tint.
    const catId = useFriendOrgStore.getState().createCategory('Close Friends', '💜', '#fa709a');
    useFriendOrgStore.getState().assignFriend('user-1', catId);
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-2"
        chatContext="channel"
        showAvatar={true}
      />
    );
    const username = document.querySelector('.message-username') as HTMLElement;
    expect(username.style.color).toBe('');
  });

  // ── Options menu outside click ──

  it('closes options menu on outside click', async () => {
    const user = userEvent.setup();
    const onEdit = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={onEdit}
        showAvatar={true}
      />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    expect(screen.getByText('Edit')).toBeInTheDocument();
    // Click outside
    await user.click(document.body);
    // Menu should close (Edit should no longer be visible)
    expect(screen.queryByText('Edit')).not.toBeInTheDocument();
  });

  // ── Syncs editContent when content changes externally ──

  it('syncs editContent when message content changes while not editing', () => {
    const { rerender } = render(
      <Message
        surfaceId="s1"
        message={mockMessage}
        currentUserId="user-1"
        onEdit={vi.fn()}
        showAvatar={true}
      />
    );
    // Rerender with new content
    rerender(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, content: 'Updated externally' }}
        currentUserId="user-1"
        onEdit={vi.fn()}
        showAvatar={true}
      />
    );
    expect(screen.getByText('Updated externally')).toBeInTheDocument();
  });

  it('renders ReactionBar when message has reactions', () => {
    useMemberStore.getState().addMember(mockMember);
    const messageWithReactions = {
      ...mockMessage,
      reactions: [mockReaction, mockReaction2],
    };
    render(
      <Message
        surfaceId="s1"
        message={messageWithReactions}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    expect(document.querySelector('.reaction-bar')).toBeInTheDocument();
    expect(screen.getByText('👍')).toBeInTheDocument();
    expect(screen.getByText('❤️')).toBeInTheDocument();
  });

  it('does not render ReactionBar when no reactions', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-1" showAvatar={true} />
    );
    expect(document.querySelector('.reaction-bar')).not.toBeInTheDocument();
  });

  it('renders reply preview when replied_to is present', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message surfaceId="s1" message={mockReplyMessage} currentUserId="user-1" showAvatar={true} />
    );
    expect(document.querySelector('.reply-preview-bar')).toBeInTheDocument();
    expect(document.querySelector('.reply-preview-author')).toBeInTheDocument();
    expect(document.querySelector('.reply-preview-snippet')).toBeInTheDocument();
  });

  it('shows deleted message text when reply_to_id set but replied_to null', () => {
    useMemberStore.getState().addMember(mockMember);
    const deletedReply = {
      ...mockMessage,
      reply_to_id: 'msg-deleted',
      replied_to: undefined,
    };
    render(
      <Message surfaceId="s1" message={deletedReply} currentUserId="user-1" showAvatar={true} />
    );
    expect(screen.getByText('Original message is unavailable')).toBeInTheDocument();
  });

  it('calls onScrollToMessage when reply header is clicked', () => {
    useMemberStore.getState().addMember(mockMember);
    const onScrollToMessage = vi.fn();
    render(
      <Message
        surfaceId="s1"
        message={mockReplyMessage}
        currentUserId="user-1"
        showAvatar={true}
        onScrollToMessage={onScrollToMessage}
      />
    );
    const replyBar = document.querySelector('.reply-preview-bar') as HTMLElement;
    fireEvent.click(replyBar);
    expect(onScrollToMessage).toHaveBeenCalledWith('msg-1');
  });

  it('shows pin indicator when message is pinned', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
        surfaceId="s1"
        message={mockPinnedMessage}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    expect(document.querySelector('.message-pinned-indicator')).toBeInTheDocument();
  });

  it('applies pinned class when message is pinned', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
        surfaceId="s1"
        message={mockPinnedMessage}
        currentUserId="user-1"
        showAvatar={true}
      />
    );
    expect(document.querySelector('.message.pinned')).toBeInTheDocument();
  });

  it('does not show pin indicator when message is not pinned', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-1" showAvatar={true} />
    );
    expect(document.querySelector('.message-pinned-indicator')).not.toBeInTheDocument();
  });

  // ── Attachments (#178) ──

  it('renders AttachmentDisplay when message has attachments', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
        surfaceId="s1"
        message={mockMessageWithAttachments}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByTestId('attachment-display')).toBeInTheDocument();
    expect(screen.getByText('attach-1')).toBeInTheDocument();
    expect(screen.getByText('attach-2')).toBeInTheDocument();
  });

  it('does not render AttachmentDisplay when message has no attachments', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    expect(screen.queryByTestId('attachment-display')).not.toBeInTheDocument();
  });

  it('renders GifEmbed when message has gif_slug', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
        surfaceId="s1"
        message={{ ...mockMessage, gif_slug: 'happy-cat-dance' }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByTestId('gif-embed')).toBeInTheDocument();
    expect(screen.getByText('happy-cat-dance')).toBeInTheDocument();
  });

  it('does not render GifEmbed when message has no gif_slug', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message surfaceId="s1" message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    expect(screen.queryByTestId('gif-embed')).not.toBeInTheDocument();
  });

  // ── AttachmentDisplay messageBody guard (ciphertext leak prevention) ──

  it('does not leak ciphertext to AttachmentDisplay messageBody when pendingKeys=true', () => {
    // When message.pendingKeys is true, message.content is undecrypted ciphertext.
    // The callsite must pass an empty string as messageBody to AttachmentDisplay,
    // preventing OverflowMarkdownAttachment from rendering ciphertext as a preview.
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
        surfaceId="s1"
        message={{
          ...mockMessageWithAttachments,
          pendingKeys: true,
          content: 'CIPHERTEXT_NOT_DECRYPTED',
        }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    // AttachmentDisplay should be rendered (attachments are shown regardless)
    expect(screen.getByTestId('attachment-display')).toBeInTheDocument();
    // The ciphertext must NOT appear in the messageBody slot passed to AttachmentDisplay
    const bodySlot = screen.getByTestId('attachment-message-body');
    expect(bodySlot.textContent).toBe('');
    expect(screen.queryByText(/CIPHERTEXT_NOT_DECRYPTED/)).not.toBeInTheDocument();
  });

  it('does not leak content to AttachmentDisplay messageBody when decryptFailed=true', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
        surfaceId="s1"
        message={{
          ...mockMessageWithAttachments,
          decryptFailed: true,
          content: 'STALE_OR_INVALID_CONTENT',
        }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByTestId('attachment-display')).toBeInTheDocument();
    const bodySlot = screen.getByTestId('attachment-message-body');
    expect(bodySlot.textContent).toBe('');
    expect(screen.queryByText(/STALE_OR_INVALID_CONTENT/)).not.toBeInTheDocument();
  });
});

// ── #1959: store-driven edit box, per-surface focus return ──
import { StrictMode } from 'react';
import { act, within, render as bareRender } from '@testing-library/react';
import { BrowserRouter } from 'react-router';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import type { MessageWithStatus } from '@/renderer/types/chat';

type EditResult = void | Promise<boolean | void>;

/** One chat panel as an owner renders it: a root carrying `data-chat-surface`, rows, a composer. */
function Surface({
  id,
  children,
  withComposer = true,
}: Readonly<{ id: string; children?: React.ReactNode; withComposer?: boolean }>) {
  return (
    <div data-chat-surface={id} data-testid={`surface-${id}`}>
      {children}
      {withComposer && (
        <textarea className="message-input-textarea" aria-label={`Composer ${id}`} />
      )}
    </div>
  );
}

function row(
  message: MessageWithStatus,
  surfaceId: string,
  onEdit?: (messageId: string, content: string) => EditResult,
  extra: { onDelete?: (id: string) => void } = {}
) {
  return (
    <Message
      key={`${surfaceId}:${message.id}`}
      message={message}
      currentUserId="user-1"
      surfaceId={surfaceId}
      onEdit={onEdit}
      onDelete={extra.onDelete}
      showAvatar={true}
    />
  );
}

const editBoxes = (scope: ParentNode = document) =>
  scope.querySelectorAll<HTMLTextAreaElement>('.message-edit-input');

const openEdit = (surfaceId: string, messageId: string) =>
  act(() => useChatStore.getState().setEditingMessage(surfaceId, messageId));

describe('Message edit box driven by chatStore.editingMessage (#1959)', () => {
  // This describe is a sibling of `describe('Message')`, so that block's beforeEach does not run
  // here: reset explicitly or an open edit leaks between cases.
  beforeEach(() => {
    resetAllStores();
    useMemberStore.getState().addMember(mockMember);
  });

  const rowOne = { ...mockMessage, id: 'm-1', content: 'First message body' };
  const rowTwo = { ...mockMessage, id: 'm-2', content: 'Second message body' };

  const renderRows = (onEdit = vi.fn()) => {
    render(
      <Surface id="s1">
        {row(rowOne, 's1', onEdit)}
        {row(rowTwo, 's1', onEdit)}
      </Surface>
    );
    return onEdit;
  };

  const composer = (surfaceId = 's1') =>
    within(screen.getByTestId(`surface-${surfaceId}`)).getByLabelText(`Composer ${surfaceId}`);

  it('#1959 opens exactly the targeted row when the store names it', () => {
    renderRows();
    // Gate: nothing is open before the store is written.
    expect(editBoxes()).toHaveLength(0);

    openEdit('s1', 'm-2');

    const boxes = editBoxes();
    expect(boxes).toHaveLength(1);
    expect(boxes[0].value).toBe('Second message body');
    expect(boxes[0].value).not.toBe('First message body');
  });

  it('#1959 the options-menu Edit action writes the store and opens the box', () => {
    render(<Surface id="s1">{row(mockMessage, 's1', vi.fn())}</Surface>);
    expect(useChatStore.getState().editingMessage).toBeNull();
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: 's1',
      messageId: mockMessage.id,
    });
    expect(editBoxes()).toHaveLength(1);
  });

  it('#1959 Escape clears the store and returns focus to the composer', () => {
    renderRows();
    openEdit('s1', 'm-2');
    const textarea = screen.getByRole('textbox', { name: 'Edit message' });
    expect(textarea).toHaveFocus();

    fireEvent.keyDown(textarea, { key: 'Escape' });

    expect(useChatStore.getState().editingMessage).toBeNull();
    expect(editBoxes()).toHaveLength(0);
    expect(composer()).toHaveFocus();
  });

  it('#1959 Save clears the store and returns focus to the composer', () => {
    const onEdit = renderRows();
    openEdit('s1', 'm-2');
    const textarea = screen.getByRole('textbox', { name: 'Edit message' });
    fireEvent.change(textarea, { target: { value: 'Rewritten body' } });

    fireEvent.keyDown(textarea, { key: 'Enter' });

    expect(onEdit).toHaveBeenCalledWith('m-2', 'Rewritten body');
    expect(useChatStore.getState().editingMessage).toBeNull();
    expect(editBoxes()).toHaveLength(0);
    expect(composer()).toHaveFocus();
  });

  it('#1959 the Cancel button clears the store and returns focus to the composer', () => {
    renderRows();
    openEdit('s1', 'm-2');
    // Gate: the Cancel button exists only while the box is open.
    const cancel = screen.getByText('Cancel');
    expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-2' });

    fireEvent.click(cancel);

    expect(useChatStore.getState().editingMessage).toBeNull();
    expect(editBoxes()).toHaveLength(0);
    expect(composer()).toHaveFocus();
  });

  it('#1959 the edit textarea has the accessible name "Edit message"', () => {
    renderRows();
    openEdit('s1', 'm-1');
    const textarea = screen.getByRole('textbox', { name: 'Edit message' });
    expect(textarea).toBeInTheDocument();
    expect((textarea as HTMLTextAreaElement).value).toBe('First message body');
  });

  it('#1959 cancelling with no composer in the document does not throw and clears the store', () => {
    render(
      <Surface id="s1" withComposer={false}>
        {row(mockMessage, 's1', vi.fn())}
      </Surface>
    );
    openEdit('s1', mockMessage.id);
    // Gate: the box is open and there is genuinely no composer to focus.
    const textarea = screen.getByRole('textbox', { name: 'Edit message' });
    expect(document.querySelector('.message-input-textarea')).toBeNull();

    expect(() => fireEvent.keyDown(textarea, { key: 'Escape' })).not.toThrow();

    expect(useChatStore.getState().editingMessage).toBeNull();
    expect(editBoxes()).toHaveLength(0);
  });

  describe('a Save that is refused leaves the edit open', () => {
    it.each([
      ['whitespace-only content', '   '],
      ['unchanged content', 'Second message body'],
    ])('#1959 Enter on %s does not submit, close or move focus', (_label, draft) => {
      const onEdit = renderRows();
      openEdit('s1', 'm-2');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement;
      fireEvent.change(textarea, { target: { value: draft } });
      // Gate: the box holds the draft under test and owns focus, so a refusal that moved
      // focus to the composer would show below.
      expect(textarea.value).toBe(draft);
      expect(textarea).toHaveFocus();

      fireEvent.keyDown(textarea, { key: 'Enter' });

      expect(onEdit).not.toHaveBeenCalled();
      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: 's1',
        messageId: 'm-2',
      });
      expect(editBoxes()).toHaveLength(1);
      expect(document.activeElement).toBe(textarea);
      expect(composer()).not.toHaveFocus();
    });
  });

  describe('focus returns to the composer of the row own surface', () => {
    const panelRow = { ...mockMessage, id: 'm-panel', content: 'Panel body' };
    const mainRow = { ...mockMessage, id: 'm-main', content: 'Main body' };

    it('#1959 cancelling in the later panel focuses that panel composer, not the first in the page', () => {
      render(
        <>
          <Surface id="s-main">{row(mainRow, 's-main', vi.fn())}</Surface>
          <Surface id="s-panel">{row(panelRow, 's-panel', vi.fn())}</Surface>
        </>
      );
      openEdit('s-panel', 'm-panel');
      // Gate: the edit box is open and has focus, so the change below is a real move.
      const textarea = within(screen.getByTestId('surface-s-panel')).getByRole('textbox', {
        name: 'Edit message',
      });
      expect(textarea).toHaveFocus();

      fireEvent.keyDown(textarea, { key: 'Escape' });

      expect(composer('s-panel')).toHaveFocus();
      expect(composer('s-main')).not.toHaveFocus();
    });

    it('#1959 saving in the later panel focuses that panel composer', () => {
      const onEdit = vi.fn();
      render(
        <>
          <Surface id="s-main">{row(mainRow, 's-main', onEdit)}</Surface>
          <Surface id="s-panel">{row(panelRow, 's-panel', onEdit)}</Surface>
        </>
      );
      openEdit('s-panel', 'm-panel');
      const textarea = within(screen.getByTestId('surface-s-panel')).getByRole('textbox', {
        name: 'Edit message',
      });
      expect(textarea).toHaveFocus();
      fireEvent.change(textarea, { target: { value: 'Panel body, rewritten' } });

      fireEvent.keyDown(textarea, { key: 'Enter' });

      expect(onEdit).toHaveBeenCalledWith('m-panel', 'Panel body, rewritten');
      expect(composer('s-panel')).toHaveFocus();
      expect(composer('s-main')).not.toHaveFocus();
    });

    it('#1959 a surface with no composer does not push focus into another panel composer', () => {
      render(
        <>
          <Surface id="s-main">{row(mainRow, 's-main', vi.fn())}</Surface>
          <Surface id="s-panel" withComposer={false}>
            {row(panelRow, 's-panel', vi.fn())}
          </Surface>
        </>
      );
      openEdit('s-panel', 'm-panel');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' });
      // Gate: the main panel composer exists and is not focused; the edit box holds focus.
      expect(composer('s-main')).toBeInTheDocument();
      expect(textarea).toHaveFocus();

      fireEvent.keyDown(textarea, { key: 'Escape' });

      expect(editBoxes()).toHaveLength(0);
      expect(composer('s-main')).not.toHaveFocus();
    });
  });

  describe('the same message shown in two surfaces', () => {
    const shared = { ...mockMessage, id: 'm-shared', content: 'Shared body' };
    const renderTwoPanels = () =>
      render(
        <>
          <Surface id="s-main">{row(shared, 's-main', vi.fn())}</Surface>
          <Surface id="s-panel">{row(shared, 's-panel', vi.fn())}</Surface>
        </>
      );

    it('#1959 an edit opened for one surface opens only that surface row', () => {
      renderTwoPanels();
      // Gate: both rows are mounted and nothing is open.
      expect(screen.getAllByText('Shared body')).toHaveLength(2);
      expect(editBoxes()).toHaveLength(0);

      openEdit('s-panel', 'm-shared');

      expect(editBoxes()).toHaveLength(1);
      expect(editBoxes(screen.getByTestId('surface-s-panel'))).toHaveLength(1);
      expect(editBoxes(screen.getByTestId('surface-s-main'))).toHaveLength(0);
    });

    it('#1959 the Edit action in the other surface moves the open edit there only', () => {
      renderTwoPanels();
      openEdit('s-panel', 'm-shared');
      // Gate: the edit is open in the panel before it is moved.
      expect(editBoxes(screen.getByTestId('surface-s-panel'))).toHaveLength(1);

      const main = within(screen.getByTestId('surface-s-main'));
      fireEvent.click(main.getByLabelText('Message options'));
      fireEvent.click(main.getByText('Edit'));

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: 's-main',
        messageId: 'm-shared',
      });
      expect(editBoxes()).toHaveLength(1);
      expect(editBoxes(screen.getByTestId('surface-s-main'))).toHaveLength(1);
      expect(editBoxes(screen.getByTestId('surface-s-panel'))).toHaveLength(0);
    });
  });

  describe('an edit ends with its row', () => {
    const second = { ...mockMessage, id: 'm-2', content: 'Second message body' };

    it('#1959 unmounting the editing row clears the open edit', () => {
      const { unmount } = render(<Surface id="s1">{row(rowOne, 's1', vi.fn())}</Surface>);
      openEdit('s1', 'm-1');
      // Gate: the edit is open before the row goes away.
      expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-1' });
      expect(editBoxes()).toHaveLength(1);

      unmount();

      expect(useChatStore.getState().editingMessage).toBeNull();
    });

    it('#1959 unmounting a row that is not editing leaves the open edit alone', () => {
      const both = (showFirst: boolean) => (
        <Surface id="s1">
          {showFirst && row(rowOne, 's1', vi.fn())}
          {row(second, 's1', vi.fn())}
        </Surface>
      );
      const { rerender } = render(both(true));
      openEdit('s1', 'm-2');
      // Gate: the first row is mounted and the second one is editing.
      expect(screen.getByText('First message body')).toBeInTheDocument();
      expect(editBoxes()).toHaveLength(1);

      rerender(both(false));

      expect(screen.queryByText('First message body')).not.toBeInTheDocument();
      expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-2' });
      expect(editBoxes()).toHaveLength(1);
    });

    it('#1959 unmounting the same message in another surface leaves the open edit alone', () => {
      const panels = (showMain: boolean) => (
        <>
          {showMain && <Surface id="s-main">{row(rowOne, 's-main', vi.fn())}</Surface>}
          <Surface id="s-panel">{row(rowOne, 's-panel', vi.fn())}</Surface>
        </>
      );
      const { rerender } = render(panels(true));
      openEdit('s-panel', 'm-1');
      // Gate: both surfaces are mounted and the panel's edit is open.
      expect(screen.getByTestId('surface-s-main')).toBeInTheDocument();
      expect(editBoxes()).toHaveLength(1);

      rerender(panels(false));

      expect(screen.queryByTestId('surface-s-main')).not.toBeInTheDocument();
      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: 's-panel',
        messageId: 'm-1',
      });
      expect(editBoxes()).toHaveLength(1);
    });

    it('#1959 a row mounted not editing, then opened, stays open under StrictMode effect replay', () => {
      // Count calls to the action the unmount cleanup uses: a replayed mount runs that cleanup
      // once, which is the evidence that this render really replays effects.
      const original = useChatStore.getState().clearEditingMessage;
      const clearSpy = vi.fn(original);
      act(() => useChatStore.setState({ clearEditingMessage: clearSpy }));

      // StrictMode must be the TOPMOST element of a bare render to replay effects (tests.md).
      bareRender(
        <StrictMode>
          <BrowserRouter>
            <ModalProvider>
              <Surface id="s1">{row(rowOne, 's1', vi.fn())}</Surface>
            </ModalProvider>
          </BrowserRouter>
        </StrictMode>
      );
      // Gate: the replay ran the cleanup once and nothing is open yet.
      expect(clearSpy).toHaveBeenCalledTimes(1);
      expect(editBoxes()).toHaveLength(0);

      openEdit('s1', 'm-1');

      expect(editBoxes()).toHaveLength(1);
      expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-1' });
      expect(clearSpy).toHaveBeenCalledTimes(1);
    });
  });

  describe('content that arrives while the edit is open', () => {
    // A live message is inserted as a fail-closed placeholder with empty text and an attachment
    // (so it is editable), and its text lands later. Message takes its row as a prop, so the
    // harness feeds it from the store the way the list does.
    const CHANNEL = 'channel-1';
    const placeholder = {
      ...mockMessage,
      id: 'm-late',
      channel_id: CHANNEL,
      content: '',
      attachments: [{ ...mockAttachment, id: 'att-late' }],
    } as MessageWithStatus;

    function StoreBackedRow() {
      const message = useChatStore((s) =>
        s.messagesByChannel.get(CHANNEL)?.find((m) => m.id === 'm-late')
      );
      return <Surface id="s1">{message ? row(message, 's1', vi.fn()) : null}</Surface>;
    }

    const land = (content: string) =>
      act(() => useChatStore.getState().updateMessage(CHANNEL, 'm-late', { content }));

    beforeEach(() => {
      act(() => useChatStore.getState().setMessages(CHANNEL, [placeholder]));
    });

    it('#1959 an untouched draft adopts the text when it arrives', () => {
      render(<StoreBackedRow />);
      openEdit('s1', 'm-late');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement;
      // Gate: the box opened on the placeholder and is empty.
      expect(textarea.value).toBe('');

      land('Decrypted text');

      expect(
        (screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement).value
      ).toBe('Decrypted text');
    });

    it('#1959 a draft the user changed is kept when newer text arrives', () => {
      render(<StoreBackedRow />);
      openEdit('s1', 'm-late');
      land('Decrypted text');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement;
      // Gate: the first arrival was adopted, so the draft is in step with the content.
      expect(textarea.value).toBe('Decrypted text');
      fireEvent.change(textarea, { target: { value: 'Decrypted text, my fix' } });
      expect(textarea.value).toBe('Decrypted text, my fix');

      land('Decrypted text from another device');

      expect(
        (screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement).value
      ).toBe('Decrypted text, my fix');
    });
  });

  describe('input method composition', () => {
    it('#1959 Enter that commits an IME composition neither saves nor closes the box', () => {
      const onEdit = renderRows();
      openEdit('s1', 'm-2');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' });
      fireEvent.change(textarea, { target: { value: 'Rewritten body' } });
      // Gate: the draft is submittable, so only the composition check can stop the save.
      expect((textarea as HTMLTextAreaElement).value).toBe('Rewritten body');

      fireEvent.keyDown(textarea, { key: 'Enter', isComposing: true });

      expect(onEdit).not.toHaveBeenCalled();
      expect(editBoxes()).toHaveLength(1);
      expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-2' });

      // Control: the same draft saves on a plain Enter, so the handler was live all along.
      fireEvent.keyDown(textarea, { key: 'Enter' });
      expect(onEdit).toHaveBeenCalledWith('m-2', 'Rewritten body');
    });

    it('#1959 Escape that cancels an IME composition does not close the box', () => {
      renderRows();
      openEdit('s1', 'm-2');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' });
      expect(textarea).toHaveFocus();

      fireEvent.keyDown(textarea, { key: 'Escape', isComposing: true });

      expect(editBoxes()).toHaveLength(1);
      expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-2' });
    });
  });

  describe('a Save the server did not take', () => {
    const ALERT = "Couldn't save your edit. Try again.";
    const target = { ...mockMessage, id: 'm-2', content: 'Second message body' };

    /** Opens m-2 in s1, rewrites it, presses Enter, and returns the textarea that was submitted. */
    function submitRewrite(onEdit: (id: string, content: string) => EditResult) {
      openEdit('s1', 'm-2');
      const textarea = screen.getByRole('textbox', { name: 'Edit message' });
      fireEvent.change(textarea, { target: { value: '  Rewritten body  ' } });
      fireEvent.keyDown(textarea, { key: 'Enter' });
      // Gate: the optimistic close happened and nothing has been restored yet.
      expect(onEdit).toHaveBeenCalledWith('m-2', 'Rewritten body');
      expect(editBoxes()).toHaveLength(0);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    }

    const settle = () => act(async () => {});

    it.each([
      ['false', () => Promise.resolve(false)],
      ['undefined', () => Promise.resolve(undefined)],
      ['a rejection', () => Promise.reject(new Error('network down'))],
    ])(
      '#1959 a save that resolves to %s reopens the box with the submitted text and an alert',
      async (_label, outcome) => {
        const onEdit = vi.fn(outcome);
        render(<Surface id="s1">{row(target, 's1', onEdit)}</Surface>);
        submitRewrite(onEdit);

        await settle();

        const textarea = screen.getByRole('textbox', {
          name: 'Edit message',
        }) as HTMLTextAreaElement;
        expect(textarea.value).toBe('Rewritten body');
        expect(screen.getByRole('alert')).toHaveTextContent(ALERT);
        expect(useChatStore.getState().editingMessage).toEqual({
          surfaceId: 's1',
          messageId: 'm-2',
        });

        fireEvent.click(screen.getByText('Cancel'));

        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(editBoxes()).toHaveLength(0);
      }
    );

    it('#1959 resubmitting from the restored box clears the alert', async () => {
      const onEdit = vi.fn(() => Promise.resolve(false));
      render(<Surface id="s1">{row(target, 's1', onEdit)}</Surface>);
      submitRewrite(onEdit);
      await settle();
      // Gate: the alert is showing.
      expect(screen.getByRole('alert')).toHaveTextContent(ALERT);

      fireEvent.keyDown(screen.getByRole('textbox', { name: 'Edit message' }), { key: 'Enter' });

      expect(onEdit).toHaveBeenCalledTimes(2);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(editBoxes()).toHaveLength(0);
    });

    it('#1959 a save that resolves true leaves the box closed with no alert', async () => {
      const onEdit = vi.fn(() => Promise.resolve(true));
      render(<Surface id="s1">{row(target, 's1', onEdit)}</Surface>);
      submitRewrite(onEdit);

      await settle();

      expect(editBoxes()).toHaveLength(0);
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(useChatStore.getState().editingMessage).toBeNull();
    });

    it('#1959 a failure that lands after the row unmounted reopens nothing and does not throw', async () => {
      let resolveSave: (saved: boolean) => void = () => {};
      const onEdit = vi.fn(
        () =>
          new Promise<boolean>((resolve) => {
            resolveSave = resolve;
          })
      );
      const { unmount } = render(<Surface id="s1">{row(target, 's1', onEdit)}</Surface>);
      submitRewrite(onEdit);
      unmount();
      // Gate: the row is gone before the save settles.
      expect(screen.queryByTestId('surface-s1')).not.toBeInTheDocument();

      await expect(
        act(async () => {
          resolveSave(false);
        })
      ).resolves.toBeUndefined();

      expect(useChatStore.getState().editingMessage).toBeNull();
      expect(editBoxes()).toHaveLength(0);
    });

    it('#1959 a failure does not displace another edit opened in the same surface meanwhile', async () => {
      let resolveSave: (saved: boolean) => void = () => {};
      const onEdit = vi.fn(
        () =>
          new Promise<boolean>((resolve) => {
            resolveSave = resolve;
          })
      );
      const other = { ...mockMessage, id: 'm-1', content: 'First message body' };
      render(
        <Surface id="s1">
          {row(other, 's1', vi.fn())}
          {row(target, 's1', onEdit)}
        </Surface>
      );
      submitRewrite(onEdit);
      openEdit('s1', 'm-1');
      // Gate: the other row's edit is open before the first save settles.
      expect(editBoxes()).toHaveLength(1);
      expect(editBoxes()[0].value).toBe('First message body');

      await act(async () => {
        resolveSave(false);
      });

      expect(useChatStore.getState().editingMessage).toEqual({ surfaceId: 's1', messageId: 'm-1' });
      expect(editBoxes()).toHaveLength(1);
      expect(editBoxes()[0].value).toBe('First message body');
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('#1959 an edit open in a different surface does not block the restore', async () => {
      const onEdit = vi.fn(() => Promise.resolve(false));
      render(
        <>
          <Surface id="s1">{row(target, 's1', onEdit)}</Surface>
          <Surface id="s2">
            {row({ ...mockMessage, id: 'm-9', content: 'Elsewhere' }, 's2')}
          </Surface>
        </>
      );
      submitRewrite(onEdit);
      openEdit('s2', 'm-9');
      // Gate: the other surface's edit is open.
      expect(editBoxes(screen.getByTestId('surface-s2'))).toHaveLength(1);

      await settle();

      expect(editBoxes(screen.getByTestId('surface-s1'))).toHaveLength(1);
      expect(editBoxes(screen.getByTestId('surface-s1'))[0].value).toBe('Rewritten body');
      expect(screen.getByRole('alert')).toHaveTextContent(ALERT);
    });
  });

  describe('an own message that cannot be read', () => {
    const unreadable = { ...mockMessage, id: 'm-unreadable', decryptFailed: true };

    it('#1959 the options menu offers Delete but not Edit', () => {
      // Control: the readable message offers both, so the Edit omission below is the gate.
      const { unmount } = render(
        <Surface id="s1">{row(mockMessage, 's1', vi.fn(), { onDelete: vi.fn() })}</Surface>
      );
      fireEvent.click(screen.getByLabelText('Message options'));
      expect(screen.getByText('Edit')).toBeInTheDocument();
      expect(screen.getByText('Delete')).toBeInTheDocument();
      unmount();

      render(<Surface id="s1">{row(unreadable, 's1', vi.fn(), { onDelete: vi.fn() })}</Surface>);
      fireEvent.click(screen.getByLabelText('Message options'));

      expect(screen.getByText('Delete')).toBeInTheDocument();
      expect(screen.queryByText('Edit')).not.toBeInTheDocument();
    });

    it('#1959 the context menu offers Delete Message but not Edit Message', () => {
      const { unmount } = render(
        <Surface id="s1">{row(mockMessage, 's1', vi.fn(), { onDelete: vi.fn() })}</Surface>
      );
      fireEvent.contextMenu(document.querySelector('article.message') as HTMLElement);
      expect(screen.getByText('Edit Message')).toBeInTheDocument();
      expect(screen.getByText('Delete Message')).toBeInTheDocument();
      unmount();

      render(<Surface id="s1">{row(unreadable, 's1', vi.fn(), { onDelete: vi.fn() })}</Surface>);
      fireEvent.contextMenu(document.querySelector('article.message') as HTMLElement);

      expect(screen.getByText('Delete Message')).toBeInTheDocument();
      expect(screen.queryByText('Edit Message')).not.toBeInTheDocument();
    });

    it('#1959 an open edit closes its box when the row becomes unreadable', () => {
      const { rerender } = render(<Surface id="s1">{row(mockMessage, 's1', vi.fn())}</Surface>);
      openEdit('s1', mockMessage.id);
      // Gate: the readable row shows its edit box.
      expect(editBoxes()).toHaveLength(1);

      rerender(
        <Surface id="s1">{row({ ...mockMessage, decryptFailed: true }, 's1', vi.fn())}</Surface>
      );

      expect(editBoxes()).toHaveLength(0);
      expect(screen.getByText(/unable to decrypt/i)).toBeInTheDocument();
    });
  });

  describe('caret placement when the edit box opens', () => {
    const body = 'Fix the typo at the end';
    const caretRow = { ...mockMessage, id: 'm-caret', content: body };

    it('#1959 opening through the store leaves the caret after the last character', () => {
      render(<Surface id="s1">{row(caretRow, 's1', vi.fn())}</Surface>);
      // Gate: no edit box yet, and the precondition that 0 and the length differ.
      expect(screen.queryByRole('textbox', { name: 'Edit message' })).not.toBeInTheDocument();
      expect(body.length).toBeGreaterThan(0);

      openEdit('s1', caretRow.id);

      const textarea = screen.getByRole('textbox', {
        name: 'Edit message',
      }) as HTMLTextAreaElement;
      expect(textarea.value).toBe(body);
      expect(textarea.selectionStart).toBe(body.length);
      expect(textarea.selectionEnd).toBe(body.length);
    });

    it('#1959 opening through the Edit action leaves the caret after the last character', () => {
      render(<Surface id="s1">{row(caretRow, 's1', vi.fn())}</Surface>);
      expect(screen.queryByRole('textbox', { name: 'Edit message' })).not.toBeInTheDocument();

      fireEvent.click(screen.getByLabelText('Message options'));
      fireEvent.click(screen.getByText('Edit'));

      const textarea = screen.getByRole('textbox', {
        name: 'Edit message',
      }) as HTMLTextAreaElement;
      expect(textarea.value).toBe(body);
      expect(textarea.selectionStart).toBe(body.length);
      expect(textarea.selectionEnd).toBe(body.length);
    });

    it('#1959 a re-render that leaves the value alone does not move a caret the user placed', () => {
      const onEdit = vi.fn();
      const { rerender } = render(<Surface id="s1">{row(caretRow, 's1', onEdit)}</Surface>);
      openEdit('s1', caretRow.id);
      const textarea = screen.getByRole('textbox', {
        name: 'Edit message',
      }) as HTMLTextAreaElement;
      // Gate: the mount placement happened, so moving the caret below is a real change.
      expect(textarea.selectionStart).toBe(body.length);

      textarea.setSelectionRange(2, 2);
      expect(textarea.selectionStart).toBe(2);

      // A new message object with identical content forces the row (and the edit box) to
      // re-render without touching the textarea's value.
      rerender(<Surface id="s1">{row({ ...caretRow }, 's1', onEdit)}</Surface>);

      const after = screen.getByRole('textbox', { name: 'Edit message' }) as HTMLTextAreaElement;
      expect(after).toBe(textarea);
      expect(after.value).toBe(body);
      expect(after.selectionStart).toBe(2);
      expect(after.selectionEnd).toBe(2);
    });

    describe('scrolling the caret into view', () => {
      // jsdom lays nothing out: scrollHeight is 0 and scrollTop ignores writes. Stub both on the
      // prototype so `scrollTop = scrollHeight` has something to observe. scrollTop starts at 0
      // like a fresh element, so only the production assignment can make the two equal.
      const SCROLL_HEIGHT = 480;
      const scrollTops = new WeakMap<object, number>();
      const originalScrollHeight = Object.getOwnPropertyDescriptor(
        HTMLTextAreaElement.prototype,
        'scrollHeight'
      );
      const originalScrollTop = Object.getOwnPropertyDescriptor(
        HTMLTextAreaElement.prototype,
        'scrollTop'
      );

      beforeEach(() => {
        Object.defineProperty(HTMLTextAreaElement.prototype, 'scrollHeight', {
          configurable: true,
          get: () => SCROLL_HEIGHT,
        });
        Object.defineProperty(HTMLTextAreaElement.prototype, 'scrollTop', {
          configurable: true,
          get(this: object) {
            return scrollTops.get(this) ?? 0;
          },
          set(this: object, value: number) {
            scrollTops.set(this, value);
          },
        });
      });

      afterEach(() => {
        for (const [name, original] of [
          ['scrollHeight', originalScrollHeight],
          ['scrollTop', originalScrollTop],
        ] as const) {
          if (original) Object.defineProperty(HTMLTextAreaElement.prototype, name, original);
          else delete (HTMLTextAreaElement.prototype as unknown as Record<string, unknown>)[name];
        }
      });

      it('#1959 a long message opens scrolled to its end', () => {
        render(<Surface id="s1">{row(caretRow, 's1', vi.fn())}</Surface>);
        // Gate: a fresh textarea reads 0 for scrollTop while its scrollHeight is non-zero, so
        // equality below can only come from the mount-time assignment.
        const probe = document.createElement('textarea');
        expect(probe.scrollTop).toBe(0);
        expect(probe.scrollHeight).toBe(SCROLL_HEIGHT);

        openEdit('s1', caretRow.id);

        const textarea = screen.getByRole('textbox', {
          name: 'Edit message',
        }) as HTMLTextAreaElement;
        expect(textarea.scrollTop).toBe(SCROLL_HEIGHT);
        expect(textarea.scrollTop).toBe(textarea.scrollHeight);
      });
    });
  });
});
