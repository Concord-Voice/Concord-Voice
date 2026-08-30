import { render, fireEvent } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { vi, describe, it, expect, beforeEach } from 'vitest';

// Deterministic picker stubs: render a detectable node when shown, and provide
// the #2071 preload named exports so MessageInput's mount preload resolves.
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: ({ onSelect }: { onSelect: (emoji: string) => void }) => (
    <>
      <div data-testid="emoji-picker-open" />
      <button type="button" data-testid="emoji-picker-select" onClick={() => onSelect('🙂')}>
        Select emoji
      </button>
    </>
  ),
  preloadEmojiPicker: () => {},
}));
vi.mock('@/renderer/components/GifPicker/LazyGifPicker', () => ({
  default: () => <div data-testid="gif-picker-open" />,
  preloadGifPicker: () => {},
}));
vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/ui/layoutStore', () => ({ useLayoutStore: () => false }));
vi.mock('@/renderer/hooks/messaging/useFileUpload', () => ({
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

describe('MessageInput picker keyboard shortcuts (#1953)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    usePermissionStore.setState({
      serverPermissions: {},
      channelPermissions: {},
      channelOverrides: {},
    });
    useSubscriptionStore.getState().reset();
    useClientConfigStore.setState((s) => ({
      featureFlags: { ...s.featureFlags, gifsEnabled: true },
    }));
  });

  function renderInput() {
    return render(
      <MessageInput onSendMessage={vi.fn()} serverId="server-1" channelId="channel-1" />
    );
  }
  const ta = (c: HTMLElement) => c.querySelector('textarea') as HTMLTextAreaElement;

  it('Ctrl+E opens the emoji picker', () => {
    const { container } = renderInput();
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
    fireEvent.keyDown(ta(container), { key: 'e', ctrlKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).not.toBeNull();
  });

  it('Ctrl+G opens the GIF picker when GIFs are enabled', () => {
    const { container } = renderInput();
    expect(container.querySelector('[data-testid="gif-picker-open"]')).toBeNull();
    fireEvent.keyDown(ta(container), { key: 'g', ctrlKey: true });
    expect(container.querySelector('[data-testid="gif-picker-open"]')).not.toBeNull();
  });

  it('Ctrl+E a second time toggles the emoji picker closed', () => {
    const { container } = renderInput();
    fireEvent.keyDown(ta(container), { key: 'e', ctrlKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).not.toBeNull();
    fireEvent.keyDown(ta(container), { key: 'e', ctrlKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
  });

  it('does NOT open the GIF picker when GIFs are disabled', () => {
    useClientConfigStore.setState((s) => ({
      featureFlags: { ...s.featureFlags, gifsEnabled: false },
    }));
    const { container } = renderInput();
    fireEvent.keyDown(ta(container), { key: 'g', ctrlKey: true });
    expect(container.querySelector('[data-testid="gif-picker-open"]')).toBeNull();
  });

  it('plain "e" without Ctrl does not open the emoji picker', () => {
    const { container } = renderInput();
    fireEvent.keyDown(ta(container), { key: 'e' });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
  });

  it('Ctrl+Shift+E does not open the emoji picker (exact-modifier guard)', () => {
    const { container } = renderInput();
    fireEvent.keyDown(ta(container), { key: 'E', ctrlKey: true, shiftKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
  });

  it('Ctrl+Meta+E and Ctrl+Alt+E do not open the emoji picker', () => {
    const { container } = renderInput();
    const textarea = ta(container);
    fireEvent.keyDown(textarea, { key: 'e', ctrlKey: true, metaKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
    fireEvent.keyDown(textarea, { key: 'e', ctrlKey: true, altKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
  });

  it('replaces a middle selection with an emoji and restores the caret and focus', async () => {
    const { container } = renderInput();
    const textarea = ta(container);
    fireEvent.change(textarea, { target: { value: 'hello world' } });
    textarea.selectionStart = 2;
    textarea.selectionEnd = 7;

    fireEvent.keyDown(textarea, { key: 'e', ctrlKey: true });
    fireEvent.click(container.querySelector('[data-testid="emoji-picker-select"]')!);

    await vi.waitFor(() => {
      expect(textarea).toHaveValue('he🙂orld');
      expect(textarea.selectionStart).toBe(4);
      expect(textarea.selectionEnd).toBe(4);
      expect(textarea).toHaveFocus();
    });
  });

  it('Ctrl+an unrelated key leaves pickers closed', () => {
    const { container } = renderInput();
    fireEvent.keyDown(ta(container), { key: 'x', ctrlKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
    expect(container.querySelector('[data-testid="gif-picker-open"]')).toBeNull();
  });
});
