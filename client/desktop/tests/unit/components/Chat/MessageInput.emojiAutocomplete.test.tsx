import { render, fireEvent } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import { vi, describe, it, expect, beforeEach } from 'vitest';

// jsdom lacks scrollIntoView (used by the autocomplete's selected-item effect).
Element.prototype.scrollIntoView = vi.fn();

vi.mock('@/renderer/components/EmojiPicker/useEmojiData', () => ({
  useEmojiData: () => ({
    search: (q: string) => {
      const code = q.startsWith(':') ? q.slice(1) : q;
      if (!code) return [];
      return [{ e: '😄', n: 'smile', s: false, c: ['smile'] }].filter((x) =>
        x.c.some((cc) => cc.startsWith(code))
      );
    },
    loadAllForSearch: () => Promise.resolve(),
  }),
}));

// MessageInput harness mocks.
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: () => null,
  preloadEmojiPicker: () => {},
}));
vi.mock('@/renderer/components/GifPicker/LazyGifPicker', () => ({
  default: () => null,
  preloadGifPicker: () => {},
}));
vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({ default: () => <div /> }));
vi.mock('@/renderer/stores/ui/layoutStore', () => ({ useLayoutStore: () => false }));
vi.mock('@/renderer/hooks/useFileUpload', () => ({
  useFileUpload: () => ({
    files: [],
    addFiles: vi.fn(),
    removeFile: vi.fn(),
    clearFiles: vi.fn(),
    uploadAll: vi.fn().mockResolvedValue({ ids: [], summaries: [] }),
    isUploading: false,
    hasFiles: false,
  }),
}));
vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({ default: () => null }));

describe('MessageInput :shortcode emoji autocomplete integration (#1754)', () => {
  beforeEach(() => {
    usePermissionStore.setState({
      serverPermissions: {},
      channelPermissions: {},
      channelOverrides: {},
    });
    useSubscriptionStore.getState().reset();
  });

  const type = (c: HTMLElement, value: string) =>
    fireEvent.change(c.querySelector('textarea')!, {
      target: { value, selectionStart: value.length },
    });

  it('shows the emoji autocomplete when typing an active :query', () => {
    const { container } = render(<MessageInput onSendMessage={vi.fn()} channelId="c1" />);
    type(container, ':sm');
    expect(container.querySelector('.emoji-autocomplete')).not.toBeNull();
    expect(container.textContent).toContain(':smile:');
  });

  it('does not show the emoji autocomplete for an @-mention token (no collision)', () => {
    const { container } = render(<MessageInput onSendMessage={vi.fn()} channelId="c1" />);
    type(container, '@al');
    expect(container.querySelector('.emoji-autocomplete')).toBeNull();
  });

  it('does not show for a colon that is not a word-boundary trigger (3:30)', () => {
    const { container } = render(<MessageInput onSendMessage={vi.fn()} channelId="c1" />);
    type(container, '3:30');
    expect(container.querySelector('.emoji-autocomplete')).toBeNull();
  });

  it('replaces the :query token with the emoji glyph on click', () => {
    const { container } = render(<MessageInput onSendMessage={vi.fn()} channelId="c1" />);
    const textarea = container.querySelector('textarea') as HTMLTextAreaElement;
    type(container, 'hi :sm');
    fireEvent.mouseDown(container.querySelector('.emoji-option')!);
    expect(textarea.value).toContain('😄');
    expect(textarea.value).not.toContain(':sm');
  });
});
