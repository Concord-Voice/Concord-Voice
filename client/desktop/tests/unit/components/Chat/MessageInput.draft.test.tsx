import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';

// --- Mocks ---

const mockSaveDraft = vi.fn();
const mockClearDraft = vi.fn();
let mockInitialDraft:
  | {
      text: string;
      replyToId?: string;
      replyToUserId?: string;
      replyToUsername?: string;
      updatedAt: number;
    }
  | undefined;

vi.mock('@/renderer/hooks/useDraftMessage', () => ({
  useDraftMessage: vi.fn(() => ({
    initialDraft: mockInitialDraft,
    saveDraft: mockSaveDraft,
    clearDraft: mockClearDraft,
  })),
}));

vi.mock('@/renderer/hooks/useFileUpload', () => ({
  useFileUpload: () => ({
    files: [],
    addFiles: vi.fn(),
    removeFile: vi.fn(),
    clearFiles: vi.fn(),
    uploadAll: vi.fn(),
    isUploading: false,
    hasFiles: false,
  }),
}));

vi.mock('@/renderer/stores/layoutStore', () => ({
  useLayoutStore: () => false,
}));

vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: () => null,
}));

vi.mock('@/renderer/components/Chat/MentionAutocomplete', () => ({
  default: () => null,
}));

vi.mock('@/renderer/components/Chat/ReplyPreviewBar', () => ({
  default: () => null,
}));

vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({
  default: () => null,
}));

vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({
  default: () => null,
}));

vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => null,
}));

const mockSetReplyingTo = vi.fn();
vi.mock('@/renderer/stores/chatStore', () => ({
  useChatStore: {
    getState: () => ({
      messagesByChannel: new Map(),
      setReplyingTo: mockSetReplyingTo,
    }),
  },
}));

describe('MessageInput draft integration', () => {
  const onSendMessage = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    mockInitialDraft = undefined;
  });

  it('renders with restored draft content when initialDraft exists', () => {
    mockInitialDraft = {
      text: 'saved draft text',
      updatedAt: Date.now(),
    };

    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-1" />);

    const textarea = screen.getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.value).toBe('saved draft text');
  });

  it('renders empty when no draft exists', () => {
    mockInitialDraft = undefined;

    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-2" />);

    const textarea = screen.getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.value).toBe('');
  });

  it('typing calls saveDraft with current text', () => {
    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-3" />);

    const textarea = screen.getByLabelText('Message input');
    fireEvent.change(textarea, { target: { value: 'hello world', selectionStart: 11 } });

    expect(mockSaveDraft).toHaveBeenCalledWith('hello world', undefined);
  });

  it('sending message calls clearDraft', () => {
    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-4" />);

    const textarea = screen.getByLabelText('Message input');
    fireEvent.change(textarea, { target: { value: 'message to send', selectionStart: 15 } });

    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(mockClearDraft).toHaveBeenCalled();
  });

  it('renders with empty content after sending', () => {
    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-5" />);

    const textarea = screen.getByLabelText('Message input') as HTMLTextAreaElement;
    fireEvent.change(textarea, { target: { value: 'will be cleared', selectionStart: 15 } });
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(textarea.value).toBe('');
  });

  it('saveDraft is called with replyTo context when available', () => {
    const replyMessage = {
      id: 'msg-123',
      user_id: 'user-456',
      username: 'testuser',
      display_name: 'Test User',
      content: 'original message',
      channel_id: 'channel-6',
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      status: 'sent' as const,
    };

    render(
      <MessageInput onSendMessage={onSendMessage} channelId="channel-6" replyingTo={replyMessage} />
    );

    const textarea = screen.getByLabelText('Message input');
    fireEvent.change(textarea, { target: { value: 'replying here', selectionStart: 13 } });

    expect(mockSaveDraft).toHaveBeenCalledWith('replying here', replyMessage);
  });

  it('does not call setReplyingTo when draft has no reply context', () => {
    mockInitialDraft = {
      text: 'plain draft',
      updatedAt: Date.now(),
    };

    render(<MessageInput onSendMessage={onSendMessage} channelId="channel-7" />);

    expect(mockSetReplyingTo).not.toHaveBeenCalled();
  });
});
