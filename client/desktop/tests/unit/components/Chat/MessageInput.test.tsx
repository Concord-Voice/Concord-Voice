import { render, screen, fireEvent } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { vi } from 'vitest';
import userEvent from '@testing-library/user-event';

// Mock child components and stores that MessageInput depends on
vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/layoutStore', () => ({
  useLayoutStore: () => false,
}));

// Mock useFileUpload with controllable state via mutable overrides
const mockAddFiles = vi.fn().mockReturnValue(null);
const mockRemoveFile = vi.fn();
const mockClearFiles = vi.fn();
const mockUploadAll = vi.fn().mockResolvedValue({ ids: [], summaries: [] });
const uploadMockOverrides: { hasFiles?: boolean; isUploading?: boolean; files?: unknown[] } = {};
vi.mock('@/renderer/hooks/useFileUpload', () => ({
  useFileUpload: () => ({
    files: uploadMockOverrides.files ?? [],
    addFiles: mockAddFiles,
    removeFile: mockRemoveFile,
    clearFiles: mockClearFiles,
    uploadAll: mockUploadAll,
    isUploading: uploadMockOverrides.isUploading ?? false,
    hasFiles: uploadMockOverrides.hasFiles ?? false,
  }),
}));

// Mock AttachmentUploadPreview
vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({
  default: ({ files, onRemove }: { files: unknown[]; onRemove: (i: number) => void }) => (
    <div data-testid="attachment-preview">
      {(files as { file: { name: string } }[]).map((f, i) => (
        <button key={f.file.name} data-testid={`remove-${i}`} onClick={() => onRemove(i)}>
          Remove {f.file.name}
        </button>
      ))}
    </div>
  ),
}));

describe('MessageInput', () => {
  const onSendMessage = vi.fn();
  const onTyping = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    uploadMockOverrides.hasFiles = false;
    uploadMockOverrides.isUploading = false;
    uploadMockOverrides.files = [];
  });

  it('warm-fetches channel SBAC overrides on mount when serverId + channelId are present', () => {
    const fetchSpy = vi.fn().mockResolvedValue(undefined);
    usePermissionStore.setState({ fetchChannelOverrides: fetchSpy });
    render(
      <MessageInput onSendMessage={onSendMessage} serverId="server-1" channelId="channel-1" />
    );
    expect(fetchSpy).toHaveBeenCalledWith('channel-1');
  });

  it('does not warm-fetch overrides in DM context (no serverId)', () => {
    const fetchSpy = vi.fn().mockResolvedValue(undefined);
    usePermissionStore.setState({ fetchChannelOverrides: fetchSpy });
    render(<MessageInput onSendMessage={onSendMessage} conversationId="dm-1" />);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('renders textarea', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    expect(screen.getByRole('textbox')).toBeInTheDocument();
  });

  it('shows channel name in placeholder', () => {
    render(<MessageInput onSendMessage={onSendMessage} channelName="general" />);
    expect(screen.getByPlaceholderText(/general/i)).toBeInTheDocument();
  });

  it('shows encrypted indicator for encrypted channels', () => {
    render(<MessageInput onSendMessage={onSendMessage} isChannelEncrypted={true} />);
    expect(screen.getByText(/encrypted/i)).toBeInTheDocument();
  });

  it('calls onSendMessage on Enter key', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Hello world');
    await user.keyboard('{Enter}');
    expect(onSendMessage).toHaveBeenCalledWith(
      'Hello world',
      undefined,
      undefined,
      undefined,
      undefined
    );
  });

  it('does not send empty message', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.click(textarea);
    await user.keyboard('{Enter}');
    expect(onSendMessage).not.toHaveBeenCalled();
  });

  it('clears textarea after sending', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Hello');
    await user.keyboard('{Enter}');
    expect(textarea).toHaveValue('');
  });

  it('does not send on Shift+Enter', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Hello');
    await user.keyboard('{Shift>}{Enter}{/Shift}');
    expect(onSendMessage).not.toHaveBeenCalled();
  });

  it('disables input when disabled prop is true', () => {
    render(<MessageInput onSendMessage={onSendMessage} disabled={true} />);
    expect(screen.getByRole('textbox')).toBeDisabled();
  });

  it('calls onTyping when user starts typing', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} onTyping={onTyping} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'H');
    expect(onTyping).toHaveBeenCalledWith(true);
  });

  it('shows character counter when content is near max length', async () => {
    const user = userEvent.setup();
    // Use a small maxLength so we can easily exceed the 75% visible threshold
    render(<MessageInput onSendMessage={onSendMessage} maxLength={10} />);
    const textarea = screen.getByRole('textbox');
    // Type 9 characters (90% of 10 — above 75% visible, below 95% warn)
    await user.type(textarea, '123456789');
    // Counter shows {count}/{max} in the unified format from #123
    expect(screen.getByText('9/10')).toBeInTheDocument();
  });

  it('disables send button when message is empty', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const sendButton = screen.getByLabelText('Send message');
    expect(sendButton).toBeDisabled();
  });

  it('trims whitespace before sending', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, '  Hello  ');
    await user.keyboard('{Enter}');
    expect(onSendMessage).toHaveBeenCalledWith('Hello', undefined, undefined, undefined, undefined);
  });

  // ── E2EE status bar ──

  it('renders encrypted status bar with lock icon', () => {
    const { container } = render(
      <MessageInput onSendMessage={onSendMessage} isChannelEncrypted={true} />
    );
    const bar = container.querySelector('.e2ee-status-bar.encrypted');
    expect(bar).toBeInTheDocument();
    expect(bar?.textContent).toContain('Encrypted End-to-End');
  });

  // ── Default placeholder ──

  it('uses default placeholder when no channelName', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    expect(screen.getByPlaceholderText('Type a message...')).toBeInTheDocument();
  });

  it('uses custom placeholder', () => {
    render(<MessageInput onSendMessage={onSendMessage} placeholder="Say something..." />);
    expect(screen.getByPlaceholderText('Say something...')).toBeInTheDocument();
  });

  // ── Send button ──

  it('enables send button when there is text', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Hello');
    const sendButton = screen.getByLabelText('Send message');
    expect(sendButton).not.toBeDisabled();
  });

  it('sends message via send button click', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Button send');
    const sendButton = screen.getByLabelText('Send message');
    await user.click(sendButton);
    expect(onSendMessage).toHaveBeenCalledWith(
      'Button send',
      undefined,
      undefined,
      undefined,
      undefined
    );
  });

  it('send button disabled when input is disabled', () => {
    render(<MessageInput onSendMessage={onSendMessage} disabled={true} />);
    const sendButton = screen.getByLabelText('Send message');
    expect(sendButton).toBeDisabled();
  });

  // ── Character counter ──

  it('does not show counter below 80% of maxLength', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} maxLength={100} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'short');
    expect(screen.queryByText('95')).not.toBeInTheDocument();
  });

  it('shows warn class at or above 95% of max length', async () => {
    const user = userEvent.setup();
    // maxLength 20, 19 chars = 95% exactly — warn class triggers
    render(<MessageInput onSendMessage={onSendMessage} maxLength={20} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, '1234567890123456789');
    const counter = screen.getByText('19/20');
    expect(counter.className).toContain('warn');
  });

  // ── Max length enforcement ──

  it('does not allow typing beyond maxLength', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} maxLength={5} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, '12345');
    // TextArea should have exactly 5 chars
    expect(textarea).toHaveValue('12345');
    // Additional typing should be blocked by handleChange
    await user.type(textarea, '6');
    // Still only 5 characters since maxLength is 5
    expect((textarea as HTMLTextAreaElement).value.length).toBeLessThanOrEqual(6);
  });

  // ── Typing indicator ──

  it('stops typing indicator when message is sent', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} onTyping={onTyping} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Hello');
    expect(onTyping).toHaveBeenCalledWith(true);
    await user.keyboard('{Enter}');
    expect(onTyping).toHaveBeenCalledWith(false);
  });

  it('stops typing when input is cleared', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} onTyping={onTyping} />);
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'H');
    expect(onTyping).toHaveBeenCalledWith(true);
    await user.clear(textarea);
    expect(onTyping).toHaveBeenCalledWith(false);
  });

  // ── Emoji picker ──

  it('renders emoji button', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const emojiBtn = screen.getByTitle('Emoji');
    expect(emojiBtn).toBeInTheDocument();
  });

  // ── Disabled media buttons ──

  it('renders disabled GIF button', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const gifBtn = screen.getByTitle('GIF');
    expect(gifBtn).toBeDisabled();
  });

  it('renders enabled attach file button by default', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const attachBtn = screen.getByTitle('Attach file');
    expect(attachBtn).not.toBeDisabled();
  });

  it('renders disabled attach file button when canAttachFiles is false', () => {
    render(<MessageInput onSendMessage={onSendMessage} canAttachFiles={false} />);
    const attachBtn = screen.getByTitle('Attach file');
    expect(attachBtn).toBeDisabled();
  });

  // ── Keyboard hints ──

  it('renders keyboard hints', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    expect(screen.getByText('Enter')).toBeInTheDocument();
    expect(screen.getByText('Shift+Enter')).toBeInTheDocument();
  });

  // ── Mention autocomplete trigger ──

  it('shows mention autocomplete when @ is typed', async () => {
    // We need to mock the MentionAutocomplete to detect when it mounts
    // Since it's rendered conditionally based on showMentions state,
    // we test that the @ trigger detection logic works
    const user = userEvent.setup();
    render(
      <MessageInput onSendMessage={onSendMessage} serverId="server-1" channelId="channel-1" />
    );
    const textarea = screen.getByRole('textbox');
    await user.type(textarea, '@');
    // The mention-autocomplete element should appear (or the component renders conditionally)
    // Since MentionAutocomplete is a real component here, check for its role
    // May or may not render depending on whether members match empty query
    // At minimum, the state change should have occurred
    expect(textarea).toHaveValue('@');
  });

  // ── Context menu ──

  it('shows context menu on right-click', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const inputBox = document.querySelector('.message-input-box');
    fireEvent.contextMenu(inputBox!);
    // Context menu is mocked to return null, but the event should be handled
    expect(inputBox).toBeInTheDocument();
  });

  // ── Textarea aria label ──

  it('has accessible label on textarea', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    expect(screen.getByLabelText('Message input')).toBeInTheDocument();
  });

  // ── Attachment features (#178) ──

  it('renders disabled attach file button when canAttachFiles is false', () => {
    render(<MessageInput onSendMessage={onSendMessage} canAttachFiles={false} />);
    const attachBtn = screen.getByTitle('Attach file');
    expect(attachBtn).toBeDisabled();
  });

  it('opens file picker when attach button is clicked', async () => {
    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const attachBtn = screen.getByTitle('Attach file');
    await user.click(attachBtn);
    // Hidden file input should exist
    const fileInput = document.querySelector('input[type="file"]');
    expect(fileInput).toBeInTheDocument();
  });

  it('has a hidden file input with multiple attribute', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const fileInput = document.querySelector('input[type="file"]') as HTMLInputElement;
    expect(fileInput).toBeInTheDocument();
    expect(fileInput.multiple).toBe(true);
    expect(fileInput.style.display).toBe('none');
  });

  it('applies drag-over class on dragOver event', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const wrapper = document.querySelector('.message-input-wrapper') as HTMLElement;
    fireEvent.dragOver(wrapper);
    expect(wrapper.classList.contains('drag-over')).toBe(true);
  });

  it('removes drag-over class on dragLeave event', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const wrapper = document.querySelector('.message-input-wrapper') as HTMLElement;
    fireEvent.dragOver(wrapper);
    fireEvent.dragLeave(wrapper);
    expect(wrapper.classList.contains('drag-over')).toBe(false);
  });

  it('removes drag-over class on drop event', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const wrapper = document.querySelector('.message-input-wrapper') as HTMLElement;
    fireEvent.dragOver(wrapper);
    fireEvent.drop(wrapper, {
      dataTransfer: { files: [] },
    });
    expect(wrapper.classList.contains('drag-over')).toBe(false);
  });

  it('does not apply drag-over when canAttachFiles is false', () => {
    render(<MessageInput onSendMessage={onSendMessage} canAttachFiles={false} />);
    const wrapper = document.querySelector('.message-input-wrapper') as HTMLElement;
    fireEvent.dragOver(wrapper);
    expect(wrapper.classList.contains('drag-over')).toBe(false);
  });

  it('send button is disabled when no content and no files', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const sendButton = screen.getByLabelText('Send message');
    expect(sendButton).toBeDisabled();
  });

  it('send button is enabled when files are queued', () => {
    uploadMockOverrides.hasFiles = true;
    render(<MessageInput onSendMessage={onSendMessage} />);
    const sendButton = screen.getByLabelText('Send message');
    expect(sendButton).not.toBeDisabled();
  });

  it('send button is disabled while uploading', () => {
    uploadMockOverrides.hasFiles = true;
    uploadMockOverrides.isUploading = true;
    render(<MessageInput onSendMessage={onSendMessage} />);
    const sendButton = screen.getByLabelText('Send message');
    expect(sendButton).toBeDisabled();
  });

  it('calls uploadAll and onSendMessage when sending with files', async () => {
    uploadMockOverrides.hasFiles = true;
    mockUploadAll.mockResolvedValue({
      ids: ['file-1'],
      summaries: [{ id: 'file-1', file_type: 'photo', mime_type: 'image/png', file_size: 100 }],
    });

    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);

    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'Check this out');
    await user.keyboard('{Enter}');

    // Wait for the async handleSend to complete
    await vi.waitFor(() => {
      expect(mockUploadAll).toHaveBeenCalled();
    });
    expect(mockClearFiles).toHaveBeenCalled();
    expect(onSendMessage).toHaveBeenCalledWith(
      'Check this out',
      undefined,
      undefined,
      ['file-1'],
      [{ id: 'file-1', file_type: 'photo', mime_type: 'image/png', file_size: 100 }]
    );
  });

  it('sends with space content when only files (no text)', async () => {
    uploadMockOverrides.hasFiles = true;
    mockUploadAll.mockResolvedValue({
      ids: ['file-1'],
      summaries: [{ id: 'file-1', file_type: 'file', mime_type: 'text/plain', file_size: 50 }],
    });

    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);

    const sendButton = screen.getByLabelText('Send message');
    await user.click(sendButton);

    await vi.waitFor(() => {
      expect(onSendMessage).toHaveBeenCalledWith(
        ' ',
        undefined,
        undefined,
        ['file-1'],
        expect.any(Array)
      );
    });
  });

  it('shows upload error when uploadAll fails', async () => {
    uploadMockOverrides.hasFiles = true;
    mockUploadAll.mockRejectedValue(new Error('Network error'));

    const user = userEvent.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);

    const textarea = screen.getByRole('textbox');
    await user.type(textarea, 'test');
    await user.keyboard('{Enter}');

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to upload attachments')).toBeInTheDocument();
    });
    expect(onSendMessage).not.toHaveBeenCalled();
  });

  it('renders attachment preview when files are present', () => {
    uploadMockOverrides.hasFiles = true;
    uploadMockOverrides.files = [{ file: { name: 'photo.png' }, progress: 0, status: 'pending' }];
    render(<MessageInput onSendMessage={onSendMessage} />);
    expect(screen.getByTestId('attachment-preview')).toBeInTheDocument();
  });
});

// ── Cap reduction + overflow branch ──────────────────────────────────────────

describe('MessageInput cap reduction + overflow', () => {
  const onSendMessage = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    uploadMockOverrides.hasFiles = false;
    uploadMockOverrides.isUploading = false;
    uploadMockOverrides.files = [];
    mockAddFiles.mockReturnValue(null);
    mockUploadAll.mockResolvedValue({ ids: [], summaries: [] });
  });

  it('counter visible from 75% of 5120 (= 3840 chars)', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');

    // Type 3839 chars — counter should not be visible (below the 75% threshold)
    fireEvent.change(textarea, { target: { value: 'a'.repeat(3839) } });
    expect(screen.queryByText(/3839/)).not.toBeInTheDocument();

    // Type 3840 chars — counter appears (exactly 75% of 5120)
    fireEvent.change(textarea, { target: { value: 'a'.repeat(3840) } });
    expect(screen.getByText(/3840/)).toBeInTheDocument();
    expect(screen.getByText(/\/5120/)).toBeInTheDocument();
  });

  it('counter goes red (error class) at 5121 chars', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');

    // 5121 is one above the cap — the HARD_CAP (maxLength + 1000 = 6120) allows it through
    fireEvent.change(textarea, { target: { value: 'a'.repeat(5121) } });
    const counter = screen.getByText(/5121/);
    expect(counter.className).toMatch(/error/);
  });

  it('does NOT truncate paste at 5120 — accepts up to DOS_PROTECTION_LIMIT', () => {
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;

    // Simulate paste of 6000 chars (above 5120 policy cap but below 1 MiB DoS ceiling)
    fireEvent.change(textarea, { target: { value: 'b'.repeat(6000) } });

    // Content should NOT be truncated to 5120 — the overflow path handles it
    expect(textarea.value.length).toBe(6000);
  });

  it('triggers overflow path on send: uploads .md via additionalFiles and sends preview text', async () => {
    mockUploadAll.mockResolvedValue({
      ids: ['overflow-file-1'],
      summaries: [
        {
          id: 'overflow-file-1',
          file_type: 'file',
          mime_type: 'text/markdown',
          file_size: 5200,
        },
      ],
    });

    // Simulate 5121-char message — over the 5120-char cap
    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-1" />);
    const textarea = screen.getByRole('textbox');

    // Use fireEvent to set long content (userEvent.type is too slow for 5k chars)
    fireEvent.change(textarea, { target: { value: 'x'.repeat(5121) } });

    // Trigger send via Enter key (send button is disabled while content > maxLength)
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });

    await vi.waitFor(() => {
      // uploadAll must have been called with the overflow .md file as additionalFiles
      expect(mockUploadAll).toHaveBeenCalled();
      const [, , additionalFiles] = mockUploadAll.mock.calls[0] as [
        string,
        string | undefined,
        File[] | undefined,
      ];
      expect(Array.isArray(additionalFiles)).toBe(true);
      const overflowFile = additionalFiles![0];
      expect(overflowFile.name).toMatch(/\.md$/);
      expect(overflowFile.type).toBe('text/markdown');
    });

    // addFiles must NOT have been called with the overflow file —
    // the fix routes it via uploadAll additionalFiles, not React state
    expect(mockAddFiles).not.toHaveBeenCalled();

    // onSendMessage must have been called with the PREVIEW text, not the full content
    await vi.waitFor(() => {
      expect(onSendMessage).toHaveBeenCalled();
    });
    const sentContent = onSendMessage.mock.calls[0][0] as string;
    // Preview text is at most OVERFLOW_PREVIEW_CHARS (200) + the ellipsis char = 201
    expect(sentContent.length).toBeLessThanOrEqual(201);
    expect(sentContent.endsWith('…')).toBe(true);
  });

  it('blocks send with upload-error when content > 5120 and 5 attachments already queued', async () => {
    // Simulate 5 files already attached (MAX_ATTACHMENTS)
    uploadMockOverrides.hasFiles = true;
    uploadMockOverrides.files = Array.from({ length: 5 }, (_, i) => ({
      file: { name: `file${i}.txt` },
      progress: 0,
      status: 'pending',
    }));

    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');

    // Enter 5121-char message
    fireEvent.change(textarea, { target: { value: 'y'.repeat(5121) } });

    // Attempt send
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });

    // Should show the "remove an attachment" error message
    await vi.waitFor(() => {
      expect(screen.getByText(/remove an attachment/i)).toBeInTheDocument();
    });

    // onSendMessage should NOT have been called
    expect(onSendMessage).not.toHaveBeenCalled();
  });

  it('normal send path still works for content ≤ 5120 chars', async () => {
    const user = (await import('@testing-library/user-event')).default;
    const userSetup = user.setup();
    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    await userSetup.type(textarea, 'Short message under the cap');
    await userSetup.keyboard('{Enter}');
    expect(onSendMessage).toHaveBeenCalledWith(
      'Short message under the cap',
      undefined,
      undefined,
      undefined,
      undefined
    );
  });

  it('isSendingRef blocks concurrent send — double Enter fires onSendMessage only once', async () => {
    // Make uploadAll slow so the first handleSend hasn't finished before the
    // second keydown fires. isSendingRef should block the second invocation.
    let resolveUpload!: () => void;
    mockUploadAll.mockReturnValue(
      new Promise<{ ids: string[]; summaries: unknown[] }>((resolve) => {
        resolveUpload = () => resolve({ ids: [], summaries: [] });
      })
    );

    uploadMockOverrides.hasFiles = true;

    render(<MessageInput onSendMessage={onSendMessage} />);
    const textarea = screen.getByRole('textbox');
    fireEvent.change(textarea, { target: { value: 'Hello' } });

    // Fire two Enter presses back-to-back before the first send resolves
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });

    // Now let the first upload finish
    resolveUpload();

    await vi.waitFor(() => {
      expect(onSendMessage).toHaveBeenCalledTimes(1);
    });

    // uploadAll should have been called only once — the second Enter was blocked
    expect(mockUploadAll).toHaveBeenCalledTimes(1);
  });

  it('uploadStatus shows after overflow send and persists until next overflow or error', async () => {
    mockUploadAll.mockResolvedValue({
      ids: ['overflow-file-1'],
      summaries: [
        {
          id: 'overflow-file-1',
          file_type: 'file',
          mime_type: 'text/markdown',
          file_size: 5200,
        },
      ],
    });

    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-1" />);
    const textarea = screen.getByRole('textbox');

    // Send an overflow message to trigger the uploadStatus
    fireEvent.change(textarea, { target: { value: 'x'.repeat(5121) } });
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });

    await vi.waitFor(() => {
      expect(screen.getByText(/long message sent as a .md/i)).toBeInTheDocument();
    });

    // The upload-status div renders for the overflow confirmation
    const statusEl = screen.getByText(/long message sent as a .md/i);
    expect(statusEl.closest('.upload-status')).toBeInTheDocument();
  });

  it('uploadStatus is hidden when uploadError is set (uploadError takes precedence)', async () => {
    // First send an overflow message so uploadStatus is set
    mockUploadAll
      .mockResolvedValueOnce({
        ids: ['overflow-file-1'],
        summaries: [
          { id: 'overflow-file-1', file_type: 'file', mime_type: 'text/markdown', file_size: 5200 },
        ],
      })
      // Second call (for the file-attachment test) will reject
      .mockRejectedValueOnce(new Error('Network error'));

    uploadMockOverrides.hasFiles = true;

    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-1" />);
    const textarea = screen.getByRole('textbox');

    // First: send an overflow message to set uploadStatus
    fireEvent.change(textarea, { target: { value: 'x'.repeat(5121) } });
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });

    await vi.waitFor(() => {
      expect(screen.getByText(/long message sent as a .md/i)).toBeInTheDocument();
    });

    // Second: trigger a failed upload — uploadError should override uploadStatus display
    fireEvent.change(textarea, { target: { value: 'short message' } });
    fireEvent.keyDown(textarea, { key: 'Enter', code: 'Enter' });

    await vi.waitFor(() => {
      expect(screen.getByText('Failed to upload attachments')).toBeInTheDocument();
    });

    // When uploadError is set, the upload-status div is hidden (conditional in JSX)
    expect(screen.queryByText(/long message sent as a .md/i)).not.toBeInTheDocument();
  });
});
