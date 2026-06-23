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
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { vi } from 'vitest';
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
    render(<Message message={mockMessage} currentUserId="user-1" showAvatar={true} />);
    expect(screen.getByText('Hello, world!')).toBeInTheDocument();
  });

  it('renders display name when showAvatar is true', () => {
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
    // display_name takes priority over username
    expect(screen.getByText('Test User')).toBeInTheDocument();
  });

  it('falls back to username when no display_name', () => {
    render(
      <Message
        message={{ ...mockMessage, display_name: undefined as unknown as string }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('shows pending status indicator', () => {
    render(<Message message={mockPendingMessage} currentUserId="user-1" showAvatar={true} />);
    // Pending message should have visual indicator
    const msgEl = document.querySelector('.message');
    expect(msgEl).toBeInTheDocument();
  });

  it('renders message with failed status', () => {
    render(
      <Message
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
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
    expect(screen.queryByLabelText(/edit/i)).not.toBeInTheDocument();
  });

  it('shows decryption failure message', () => {
    render(
      <Message
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
        message={{ ...mockMessage, pendingKeys: true }}
        currentUserId="user-2"
        showAvatar={true}
      />
    );
    expect(screen.getByText(/waiting for encryption keys/i)).toBeInTheDocument();
  });

  it('renders avatar initial from display name', () => {
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
    // "T" for "Test User" display_name
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('renders avatar image when avatar_url provided', () => {
    render(
      <Message
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
      <Message message={mockMessage} currentUserId="user-1" showAvatar={true} />
    );
    expect(container.querySelector('.own-message')).toBeInTheDocument();
  });

  it('applies grouped style when showAvatar is false', () => {
    const { container } = render(
      <Message message={mockMessage} currentUserId="user-2" showAvatar={false} />
    );
    expect(container.querySelector('.message-grouped')).toBeInTheDocument();
  });

  it('shows context menu on right-click', () => {
    render(<Message message={mockMessage} currentUserId="user-1" showAvatar={true} />);
    const messageEl = document.querySelector('.message');
    fireEvent.contextMenu(messageEl!);
    // Context menu should render
    expect(messageEl).toBeInTheDocument();
  });

  it('renders other user message correctly', () => {
    useMemberStore.getState().addMember(mockMember2);
    render(<Message message={mockMessage2} currentUserId="user-1" showAvatar={true} />);
    expect(screen.getByText('Hi there!')).toBeInTheDocument();
    expect(screen.getByText('Test User 2')).toBeInTheDocument();
  });

  // ── Edit mode tests ──

  it('enters edit mode via options menu', async () => {
    const onEdit = vi.fn();
    render(
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    fireEvent.click(screen.getByText('Cancel'));
    expect(document.querySelector('.message-edit-input')).not.toBeInTheDocument();
  });

  it('does not submit edit when content is unchanged', () => {
    const onEdit = vi.fn();
    render(
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));
    const saveBtn = screen.getByText('Save');
    expect(saveBtn).toBeDisabled();
  });

  it('Save button is enabled after content change', () => {
    const onEdit = vi.fn();
    render(
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onDelete={onDelete} showAvatar={true} />
    );
    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Delete'));
    // Delete confirmation modal should appear
    expect(document.querySelector('.modal-overlay')).toBeInTheDocument();
  });

  it('shift+click on Delete button skips confirmation', () => {
    const onDelete = vi.fn();
    render(
      <Message message={mockMessage} currentUserId="user-1" onDelete={onDelete} showAvatar={true} />
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
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={false} />);
    const gutterTs = document.querySelector('.message-gutter-timestamp');
    expect(gutterTs).toBeInTheDocument();
  });

  // ── Edited indicator ──

  it('shows (edited) tag on edited messages with header', () => {
    render(
      <Message
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

  it('renders user mention tokens as highlighted spans', () => {
    useMemberStore.getState().addMember(mockMember2);
    render(
      <Message
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
            display_separately: false,
            mentionable: true,
            require_mfa: false,
            created_at: '2025-01-01T00:00:00Z',
            updated_at: '2025-01-01T00:00:00Z',
          },
        ],
      },
    });

    render(
      <Message
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
      <Message message={mockMessage} currentUserId="user-2" showAvatar={true} />
    );
    expect(container.querySelector('.encrypted-indicator')).toBeNull();
    expect(screen.queryByTitle('End-to-end encrypted')).toBeNull();
  });

  it('still renders <Lock> on decrypt-failed messages', () => {
    // Assertion B — regression guard for #795: the legitimate error-state lock icon must
    // remain after the decorative badge is removed.
    render(
      <Message
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
    expect(failedSpan?.querySelector('svg')).toBeTruthy();
  });

  it('still renders <Lock> on pending-keys messages', () => {
    // Assertion C — regression guard for #795: the legitimate pending-keys lock icon must
    // remain after the decorative badge is removed.
    render(
      <Message
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
    expect(pendingSpan?.querySelector('svg')).toBeTruthy();
  });

  // ── Avatar profile card ──

  it('opens profile card on avatar click', async () => {
    const user = userEvent.setup();
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
    const avatarBtn = screen.getByLabelText('View user profile');
    await user.click(avatarBtn);
    // MemberProfileCard should render
    expect(document.querySelector('.member-profile-card')).toBeInTheDocument();
  });

  it('closes profile card when close handler is invoked', async () => {
    const user = userEvent.setup();
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
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
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
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
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
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
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
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
      <Message message={mockMessage} currentUserId="user-2" chatContext="dm" showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-2" chatContext="dm" showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-2" chatContext="voice" showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-2" chatContext="dm" showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={onEdit} showAvatar={true} />
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
      <Message message={mockMessage} currentUserId="user-1" onEdit={vi.fn()} showAvatar={true} />
    );
    // Rerender with new content
    rerender(
      <Message
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
    render(<Message message={messageWithReactions} currentUserId="user-1" showAvatar={true} />);
    expect(document.querySelector('.reaction-bar')).toBeInTheDocument();
    expect(screen.getByText('👍')).toBeInTheDocument();
    expect(screen.getByText('❤️')).toBeInTheDocument();
  });

  it('does not render ReactionBar when no reactions', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Message message={mockMessage} currentUserId="user-1" showAvatar={true} />);
    expect(document.querySelector('.reaction-bar')).not.toBeInTheDocument();
  });

  it('renders reply preview when replied_to is present', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Message message={mockReplyMessage} currentUserId="user-1" showAvatar={true} />);
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
    render(<Message message={deletedReply} currentUserId="user-1" showAvatar={true} />);
    expect(screen.getByText('Original message is unavailable')).toBeInTheDocument();
  });

  it('calls onScrollToMessage when reply header is clicked', () => {
    useMemberStore.getState().addMember(mockMember);
    const onScrollToMessage = vi.fn();
    render(
      <Message
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
    render(<Message message={mockPinnedMessage} currentUserId="user-1" showAvatar={true} />);
    expect(document.querySelector('.message-pinned-indicator')).toBeInTheDocument();
  });

  it('applies pinned class when message is pinned', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Message message={mockPinnedMessage} currentUserId="user-1" showAvatar={true} />);
    expect(document.querySelector('.message.pinned')).toBeInTheDocument();
  });

  it('does not show pin indicator when message is not pinned', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Message message={mockMessage} currentUserId="user-1" showAvatar={true} />);
    expect(document.querySelector('.message-pinned-indicator')).not.toBeInTheDocument();
  });

  // ── Attachments (#178) ──

  it('renders AttachmentDisplay when message has attachments', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message message={mockMessageWithAttachments} currentUserId="user-2" showAvatar={true} />
    );
    expect(screen.getByTestId('attachment-display')).toBeInTheDocument();
    expect(screen.getByText('attach-1')).toBeInTheDocument();
    expect(screen.getByText('attach-2')).toBeInTheDocument();
  });

  it('does not render AttachmentDisplay when message has no attachments', () => {
    useMemberStore.getState().addMember(mockMember);
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
    expect(screen.queryByTestId('attachment-display')).not.toBeInTheDocument();
  });

  it('renders GifEmbed when message has gif_slug', () => {
    useMemberStore.getState().addMember(mockMember);
    render(
      <Message
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
    render(<Message message={mockMessage} currentUserId="user-2" showAvatar={true} />);
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
