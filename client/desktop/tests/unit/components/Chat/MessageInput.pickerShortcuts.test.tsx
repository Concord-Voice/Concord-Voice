import { render, fireEvent } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useSubscriptionStore } from '@/renderer/stores/subscriptionStore';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';
import { vi, describe, it, expect, beforeEach } from 'vitest';

// Deterministic picker stubs: render a detectable node when shown, and provide
// the #2071 preload named exports so MessageInput's mount preload resolves.
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: () => <div data-testid="emoji-picker-open" />,
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
vi.mock('@/renderer/stores/layoutStore', () => ({ useLayoutStore: () => false }));
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

describe('MessageInput picker keyboard shortcuts (#1953)', () => {
  beforeEach(() => {
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
});
