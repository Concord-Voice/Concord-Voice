import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { ShortcutDefinition } from '../../../src/renderer/stores/keyboardShortcutStore';

// Mock the store module before importing the service
const mockGetState = vi.fn();
vi.mock('../../../src/renderer/stores/keyboardShortcutStore', () => ({
  useKeyboardShortcutStore: {
    getState: mockGetState,
  },
}));

// Import after mocking
const { keyboardShortcutService } =
  await import('../../../src/renderer/services/keyboardShortcutService');

/** Helper to build a shortcut definition for tests */
function makeShortcut(overrides: Partial<ShortcutDefinition> = {}): ShortcutDefinition {
  return {
    id: 'test-shortcut',
    label: 'Test Shortcut',
    category: 'app',
    defaultCombo: { key: 'k', ctrl: true },
    combo: { key: 'k', ctrl: true },
    allowInInput: false,
    ...overrides,
  };
}

/** Helper to create a KeyboardEvent with specified properties */
function createKeyEvent(
  key: string,
  modifiers: { ctrlKey?: boolean; metaKey?: boolean; shiftKey?: boolean; altKey?: boolean } = {},
  target?: HTMLElement
): KeyboardEvent {
  const event = new KeyboardEvent('keydown', {
    key,
    ctrlKey: modifiers.ctrlKey ?? false,
    metaKey: modifiers.metaKey ?? false,
    shiftKey: modifiers.shiftKey ?? false,
    altKey: modifiers.altKey ?? false,
    bubbles: true,
    cancelable: true,
  });
  if (target) {
    Object.defineProperty(event, 'target', { value: target, writable: false });
  }
  return event;
}

describe('KeyboardShortcutService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Ensure service is in a clean state
    keyboardShortcutService.destroy();
    keyboardShortcutService.enable();
    mockGetState.mockReturnValue({ shortcuts: [] });
  });

  afterEach(() => {
    keyboardShortcutService.destroy();
  });

  // ── Lifecycle ──────────────────────────────────────────────────────

  describe('init / destroy', () => {
    it('attaches document keydown listener on init', () => {
      const spy = vi.spyOn(document, 'addEventListener');
      keyboardShortcutService.init();
      expect(spy).toHaveBeenCalledWith('keydown', expect.any(Function));
      spy.mockRestore();
    });

    it('removes listener and clears handlers on destroy', () => {
      keyboardShortcutService.init();
      const handler = vi.fn();
      keyboardShortcutService.registerHandler('some-action', handler);

      const spy = vi.spyOn(document, 'removeEventListener');
      keyboardShortcutService.destroy();

      expect(spy).toHaveBeenCalledWith('keydown', expect.any(Function));
      spy.mockRestore();

      // Handler map should be cleared — re-init and dispatch should not fire
      keyboardShortcutService.init();
      mockGetState.mockReturnValue({
        shortcuts: [makeShortcut({ id: 'some-action' })],
      });
      document.dispatchEvent(createKeyEvent('k', { ctrlKey: true }));
      expect(handler).not.toHaveBeenCalled();
    });
  });

  // ── matchesCombo ───────────────────────────────────────────────────

  describe('matchesCombo', () => {
    it('correctly matches Ctrl+K (non-Mac)', () => {
      const event = createKeyEvent('k', { ctrlKey: true });
      const combo = { key: 'k', ctrl: true };
      expect(keyboardShortcutService.matchesCombo(event, combo)).toBe(true);
    });

    it('correctly matches key with no modifiers', () => {
      const event = createKeyEvent('Escape');
      const combo = { key: 'Escape' };
      expect(keyboardShortcutService.matchesCombo(event, combo)).toBe(true);
    });

    it('rejects wrong key', () => {
      const event = createKeyEvent('j', { ctrlKey: true });
      const combo = { key: 'k', ctrl: true };
      expect(keyboardShortcutService.matchesCombo(event, combo)).toBe(false);
    });

    it('rejects missing modifier', () => {
      // Combo requires ctrl but event has no ctrl
      const event = createKeyEvent('k');
      const combo = { key: 'k', ctrl: true };
      expect(keyboardShortcutService.matchesCombo(event, combo)).toBe(false);
    });

    it('rejects extra modifier', () => {
      // Event has shift but combo does not
      const event = createKeyEvent('k', { ctrlKey: true, shiftKey: true });
      const combo = { key: 'k', ctrl: true };
      expect(keyboardShortcutService.matchesCombo(event, combo)).toBe(false);
    });
  });

  // ── isInTextInput ──────────────────────────────────────────────────

  describe('isInTextInput', () => {
    it('returns true for textarea', () => {
      const textarea = document.createElement('textarea');
      const event = createKeyEvent('k', { ctrlKey: true }, textarea);
      expect(keyboardShortcutService.isInTextInput(event)).toBe(true);
    });

    it('returns true for input[type=text]', () => {
      const input = document.createElement('input');
      input.type = 'text';
      const event = createKeyEvent('k', { ctrlKey: true }, input);
      expect(keyboardShortcutService.isInTextInput(event)).toBe(true);
    });

    it('returns true for contenteditable', () => {
      const div = document.createElement('div');
      div.contentEditable = 'true';
      const event = createKeyEvent('k', { ctrlKey: true }, div);
      expect(keyboardShortcutService.isInTextInput(event)).toBe(true);
    });

    it('returns false for div', () => {
      const div = document.createElement('div');
      const event = createKeyEvent('k', { ctrlKey: true }, div);
      expect(keyboardShortcutService.isInTextInput(event)).toBe(false);
    });

    it('returns false for button', () => {
      const button = document.createElement('button');
      const event = createKeyEvent('k', { ctrlKey: true }, button);
      expect(keyboardShortcutService.isInTextInput(event)).toBe(false);
    });
  });

  // ── Handler dispatch ───────────────────────────────────────────────

  describe('handler dispatch', () => {
    it('fires registered handler when combo matches', () => {
      const handler = vi.fn();
      const shortcut = makeShortcut({ id: 'open-search', combo: { key: 'k', ctrl: true } });
      mockGetState.mockReturnValue({ shortcuts: [shortcut] });

      keyboardShortcutService.init();
      keyboardShortcutService.registerHandler('open-search', handler);

      document.dispatchEvent(createKeyEvent('k', { ctrlKey: true }));
      expect(handler).toHaveBeenCalledOnce();
    });

    it('does NOT fire handler when in text input and allowInInput is false', () => {
      const handler = vi.fn();
      const shortcut = makeShortcut({
        id: 'open-search',
        combo: { key: 'k', ctrl: true },
        allowInInput: false,
      });
      mockGetState.mockReturnValue({ shortcuts: [shortcut] });

      keyboardShortcutService.init();
      keyboardShortcutService.registerHandler('open-search', handler);

      const textarea = document.createElement('textarea');
      document.body.appendChild(textarea);
      textarea.focus();

      const event = new KeyboardEvent('keydown', {
        key: 'k',
        ctrlKey: true,
        bubbles: true,
        cancelable: true,
      });
      textarea.dispatchEvent(event);

      expect(handler).not.toHaveBeenCalled();
      document.body.removeChild(textarea);
    });

    it('DOES fire handler when in text input and allowInInput is true', () => {
      const handler = vi.fn();
      const shortcut = makeShortcut({
        id: 'send-message',
        combo: { key: 'Enter' },
        allowInInput: true,
      });
      mockGetState.mockReturnValue({ shortcuts: [shortcut] });

      keyboardShortcutService.init();
      keyboardShortcutService.registerHandler('send-message', handler);

      const textarea = document.createElement('textarea');
      document.body.appendChild(textarea);
      textarea.focus();

      const event = new KeyboardEvent('keydown', {
        key: 'Enter',
        bubbles: true,
        cancelable: true,
      });
      textarea.dispatchEvent(event);

      expect(handler).toHaveBeenCalledOnce();
      document.body.removeChild(textarea);
    });

    it('disable() suppresses all handlers and enable() resumes them', () => {
      const handler = vi.fn();
      const shortcut = makeShortcut({ id: 'test-action', combo: { key: 'k', ctrl: true } });
      mockGetState.mockReturnValue({ shortcuts: [shortcut] });

      keyboardShortcutService.init();
      keyboardShortcutService.registerHandler('test-action', handler);

      // Disable and verify suppressed
      keyboardShortcutService.disable();
      expect(keyboardShortcutService.isEnabled()).toBe(false);
      document.dispatchEvent(createKeyEvent('k', { ctrlKey: true }));
      expect(handler).not.toHaveBeenCalled();

      // Re-enable and verify fires
      keyboardShortcutService.enable();
      expect(keyboardShortcutService.isEnabled()).toBe(true);
      document.dispatchEvent(createKeyEvent('k', { ctrlKey: true }));
      expect(handler).toHaveBeenCalledOnce();
    });

    it('handler not called after unregisterHandler', () => {
      const handler = vi.fn();
      const shortcut = makeShortcut({ id: 'toggle-mute', combo: { key: 'm', ctrl: true } });
      mockGetState.mockReturnValue({ shortcuts: [shortcut] });

      keyboardShortcutService.init();
      keyboardShortcutService.registerHandler('toggle-mute', handler);

      // Fire once to confirm it works
      document.dispatchEvent(createKeyEvent('m', { ctrlKey: true }));
      expect(handler).toHaveBeenCalledOnce();

      // Unregister and confirm it stops
      keyboardShortcutService.unregisterHandler('toggle-mute');
      document.dispatchEvent(createKeyEvent('m', { ctrlKey: true }));
      expect(handler).toHaveBeenCalledOnce(); // Still just the one call
    });
  });
});
