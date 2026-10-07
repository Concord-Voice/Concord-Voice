import { render, fireEvent } from '../../../test-utils';
import MessageInput, { shouldEditLastMessage } from '@/renderer/components/Chat/MessageInput';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockMessage } from '../../../mocks/fixtures';
import { vi, describe, it, expect, beforeEach } from 'vitest';

// jsdom lacks scrollIntoView (used by the typeahead's selected-item effect).
Element.prototype.scrollIntoView = vi.fn();

// Deterministic picker stubs (same set as MessageInput.pickerShortcuts.test.tsx).
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: () => <div data-testid="emoji-picker-open" />,
  preloadEmojiPicker: () => {},
}));
vi.mock('@/renderer/components/GifPicker/LazyGifPicker', () => ({
  default: () => <div data-testid="gif-picker-open" />,
  preloadGifPicker: () => {},
}));
// Deterministic shortcode search so the ":sm" typeahead has matches.
vi.mock('@/renderer/components/EmojiPicker/useEmojiData', () => ({
  useEmojiData: () => ({
    search: (q: string) => {
      const code = q.startsWith(':') ? q.slice(1) : q;
      if (!code) return [];
      return [
        { e: '😄', n: 'smile', s: false, c: ['smile'] },
        { e: '😊', n: 'smiley', s: false, c: ['smiley'] },
      ].filter((x) => x.c.some((cc) => cc.startsWith(code)));
    },
    loadAllForSearch: () => Promise.resolve(),
  }),
}));
vi.mock('@/renderer/components/Chat/MessageInputContextMenu', () => ({ default: () => null }));
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel" />,
}));
vi.mock('@/renderer/stores/ui/layoutStore', () => ({ useLayoutStore: () => false }));
const uploadMockOverrides: { hasFiles?: boolean } = {};
vi.mock('@/renderer/hooks/messaging/useFileUpload', () => ({
  useFileUpload: () => ({
    files: [],
    addFiles: vi.fn(),
    removeFile: vi.fn(),
    clearFiles: vi.fn(),
    uploadAll: vi.fn().mockResolvedValue({ ids: [], summaries: [] }),
    isUploading: false,
    hasFiles: uploadMockOverrides.hasFiles ?? false,
  }),
}));
vi.mock('@/renderer/components/Chat/AttachmentUploadPreview', () => ({ default: () => null }));

const NO_MODS = { ctrlKey: false, metaKey: false, altKey: false, shiftKey: false };
const OPEN_STATE = {
  enabled: true,
  content: '',
  hasFiles: false,
  hasReplyTarget: false,
  isComposing: false,
};

describe('shouldEditLastMessage (#1959)', () => {
  it.each([
    ['empty composer, plain ArrowUp', 'ArrowUp', NO_MODS, OPEN_STATE, true],
    ['ArrowDown', 'ArrowDown', NO_MODS, OPEN_STATE, false],
    ['Ctrl held', 'ArrowUp', { ...NO_MODS, ctrlKey: true }, OPEN_STATE, false],
    ['Meta held', 'ArrowUp', { ...NO_MODS, metaKey: true }, OPEN_STATE, false],
    ['Alt held', 'ArrowUp', { ...NO_MODS, altKey: true }, OPEN_STATE, false],
    ['Shift held', 'ArrowUp', { ...NO_MODS, shiftKey: true }, OPEN_STATE, false],
    ['handler not provided', 'ArrowUp', NO_MODS, { ...OPEN_STATE, enabled: false }, false],
    ['text present', 'ArrowUp', NO_MODS, { ...OPEN_STATE, content: 'a' }, false],
    ['staged file', 'ArrowUp', NO_MODS, { ...OPEN_STATE, hasFiles: true }, false],
    ['reply target', 'ArrowUp', NO_MODS, { ...OPEN_STATE, hasReplyTarget: true }, false],
    ['IME composing', 'ArrowUp', NO_MODS, { ...OPEN_STATE, isComposing: true }, false],
  ])('%s -> %s', (_label, key, modifiers, state, expected) => {
    expect(shouldEditLastMessage(key, modifiers, state)).toBe(expected);
  });
});

describe('MessageInput Up Arrow edits the last message (#1959)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    uploadMockOverrides.hasFiles = false;
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

  function renderInput(
    props: Partial<React.ComponentProps<typeof MessageInput>> & {
      onEditLastMessage?: () => void;
    } = {}
  ) {
    const result = render(
      <MessageInput onSendMessage={vi.fn()} serverId="server-1" channelId="channel-1" {...props} />
    );
    const textarea = result.container.querySelector(
      '.message-input-textarea'
    ) as HTMLTextAreaElement;
    return { ...result, textarea };
  }

  /** Positive gate: proves the shortcut is live on this fixture before a negative is asserted. */
  function expectShortcutLive(
    textarea: HTMLTextAreaElement,
    onEditLastMessage: ReturnType<typeof vi.fn>
  ) {
    expect(fireEvent.keyDown(textarea, { key: 'ArrowUp' })).toBe(false);
    expect(onEditLastMessage).toHaveBeenCalledTimes(1);
    onEditLastMessage.mockClear();
  }

  it('calls the handler once and prevents default on an empty composer', () => {
    const onEditLastMessage = vi.fn();
    const { textarea } = renderInput({ onEditLastMessage });
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).toHaveBeenCalledTimes(1);
    expect(notPrevented).toBe(false);
  });

  it('does nothing when text is present', () => {
    const onEditLastMessage = vi.fn();
    const { textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    fireEvent.change(textarea, { target: { value: 'hi' } });
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(notPrevented).toBe(true);
  });

  it('does nothing while replying to a message', () => {
    const onEditLastMessage = vi.fn();
    const { textarea, rerender } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    rerender(
      <MessageInput
        onSendMessage={vi.fn()}
        serverId="server-1"
        channelId="channel-1"
        onEditLastMessage={onEditLastMessage}
        replyingTo={mockMessage}
      />
    );
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(notPrevented).toBe(true);
  });

  it('does nothing while a file is staged', () => {
    const onEditLastMessage = vi.fn();
    const { textarea, rerender } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    uploadMockOverrides.hasFiles = true;
    rerender(
      <MessageInput
        onSendMessage={vi.fn()}
        serverId="server-1"
        channelId="channel-1"
        onEditLastMessage={onEditLastMessage}
      />
    );
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(notPrevented).toBe(true);
  });

  it.each([
    ['Shift', { shiftKey: true }],
    ['Ctrl', { ctrlKey: true }],
    ['Alt', { altKey: true }],
    ['Meta', { metaKey: true }],
  ])('does nothing for %s+ArrowUp', (_name, modifier) => {
    const onEditLastMessage = vi.fn();
    const { textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp', ...modifier });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(notPrevented).toBe(true);
  });

  it('does nothing for ArrowDown', () => {
    const onEditLastMessage = vi.fn();
    const { textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowDown' });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(notPrevented).toBe(true);
  });

  it('does nothing when the key event itself reports isComposing', () => {
    const onEditLastMessage = vi.fn();
    const { textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    const notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp', isComposing: true });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(notPrevented).toBe(true);
  });

  it('is suppressed between compositionstart and compositionend, then live again', () => {
    const onEditLastMessage = vi.fn();
    const { textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);

    fireEvent.compositionStart(textarea);
    const duringComposition = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(duringComposition).toBe(true);

    fireEvent.compositionEnd(textarea);
    const afterComposition = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).toHaveBeenCalledTimes(1);
    expect(afterComposition).toBe(false);
  });

  it('does not throw and does not prevent default when no handler is provided', () => {
    const { textarea } = renderInput();
    let notPrevented: boolean | undefined;
    expect(() => {
      notPrevented = fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    }).not.toThrow();
    expect(notPrevented).toBe(true);
  });

  it('leaves an open emoji typeahead in charge of ArrowUp', () => {
    const onEditLastMessage = vi.fn();
    const { container, textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);

    // Positive gate: the ":sm" shortcode popover is open.
    fireEvent.change(textarea, { target: { value: ':sm', selectionStart: 3, selectionEnd: 3 } });
    expect(container.querySelector('.emoji-autocomplete')).not.toBeNull();

    fireEvent.keyDown(textarea, { key: 'ArrowUp' });
    expect(onEditLastMessage).not.toHaveBeenCalled();
    expect(container.querySelector('.emoji-autocomplete')).not.toBeNull();
  });

  it('Ctrl+E still opens the emoji picker with the handler present', () => {
    const onEditLastMessage = vi.fn();
    const { container, textarea } = renderInput({ onEditLastMessage });
    expectShortcutLive(textarea, onEditLastMessage);
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).toBeNull();
    fireEvent.keyDown(textarea, { key: 'e', ctrlKey: true });
    expect(container.querySelector('[data-testid="emoji-picker-open"]')).not.toBeNull();
    expect(onEditLastMessage).not.toHaveBeenCalled();
  });
});
