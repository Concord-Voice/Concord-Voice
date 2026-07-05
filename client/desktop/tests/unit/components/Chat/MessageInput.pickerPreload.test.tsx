import { render } from '../../../test-utils';
import MessageInput from '@/renderer/components/Chat/MessageInput';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useSubscriptionStore } from '@/renderer/stores/subscriptionStore';
import { vi, describe, it, expect, beforeEach } from 'vitest';

// `mock`-prefixed so vitest permits referencing them inside the hoisted
// vi.mock factories below (same escape hatch used across MessageInput.test.tsx).
const mockPreloadEmoji = vi.fn();
const mockPreloadGif = vi.fn();

// Mock the lazy picker wrappers: stub the default component (never opened here)
// and route the named preload export to a spy so we can assert it fires on mount.
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: () => null,
  preloadEmojiPicker: () => mockPreloadEmoji(),
}));
vi.mock('@/renderer/components/GifPicker/LazyGifPicker', () => ({
  default: () => null,
  preloadGifPicker: () => mockPreloadGif(),
}));

// Minimal MessageInput dependencies (mirrors MessageInput.test.tsx).
vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/layoutStore', () => ({ useLayoutStore: () => false }));
vi.mock('@/renderer/hooks/useFileUpload', () => ({
  useFileUpload: () => ({
    files: [],
    addFiles: vi.fn().mockReturnValue(null),
    removeFile: vi.fn(),
    clearFiles: vi.fn(),
    uploadAll: vi.fn().mockResolvedValue({ ids: [], summaries: [] }),
    isUploading: false,
    hasFiles: false,
  }),
}));
vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({ default: () => null }));

/**
 * #2071: the composer warms the lazy emoji/GIF picker chunks on mount so the
 * FIRST open doesn't incur a chunk download/parse reflow (the reported one-off
 * "expand then snap back" jump). The visual glitch itself isn't assertable in
 * jsdom (no layout engine); this locks the mechanism that removes it — the
 * preload wiring firing exactly once on mount.
 */
describe('MessageInput picker chunk preload (#2071)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usePermissionStore.setState({
      serverPermissions: {},
      channelPermissions: {},
      channelOverrides: {},
    });
    useSubscriptionStore.getState().reset();
  });

  it('warms both lazy picker chunks once on composer mount', () => {
    render(<MessageInput onSendMessage={vi.fn()} serverId="server-1" channelId="channel-1" />);
    expect(mockPreloadEmoji).toHaveBeenCalledTimes(1);
    expect(mockPreloadGif).toHaveBeenCalledTimes(1);
  });
});
