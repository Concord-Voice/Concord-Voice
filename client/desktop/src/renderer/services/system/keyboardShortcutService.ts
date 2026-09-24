import { useKeyboardShortcutStore, type KeyCombo } from '../../stores/ui/keyboardShortcutStore';
import { useMFAChallengeStore } from '../../stores/auth/mfaChallengeStore';

/**
 * Marks a global overlay's <dialog> (set by useTopLayerDialog). While one is
 * open, only the shortcuts in RUNS_BEHIND_OVERLAY run: see handleKeyDown.
 */
export const GLOBAL_OVERLAY_ATTRIBUTE = 'data-global-overlay';

/**
 * Shortcuts that open no dialog and never use Escape, so they still run behind
 * a pending MFA challenge or an open global overlay.
 */
const RUNS_BEHIND_OVERLAY: ReadonlySet<string> = new Set(['toggle-mute', 'toggle-deafen']);

class KeyboardShortcutService {
  private readonly handlers = new Map<string, () => void>();
  private enabled = true;
  private boundHandler: ((e: KeyboardEvent) => void) | null = null;
  private readonly isMac: boolean;

  constructor() {
    this.isMac =
      typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.userAgent);
  }

  /** Attach document-level keydown listener */
  init(): void {
    if (this.boundHandler) return; // Already initialized
    this.boundHandler = this.handleKeyDown.bind(this);
    document.addEventListener('keydown', this.boundHandler);
  }

  /** Remove document-level keydown listener */
  destroy(): void {
    if (this.boundHandler) {
      document.removeEventListener('keydown', this.boundHandler);
      this.boundHandler = null;
    }
    this.handlers.clear();
  }

  /** Register a handler for a shortcut action */
  registerHandler(shortcutId: string, handler: () => void): void {
    this.handlers.set(shortcutId, handler);
  }

  /** Unregister a handler */
  unregisterHandler(shortcutId: string): void {
    this.handlers.delete(shortcutId);
  }

  /** Temporarily disable all shortcuts */
  disable(): void {
    this.enabled = false;
  }

  /** Re-enable shortcuts */
  enable(): void {
    this.enabled = true;
  }

  /** Check if shortcuts are enabled */
  isEnabled(): boolean {
    return this.enabled;
  }

  /** Core keydown handler */
  private handleKeyDown(event: KeyboardEvent): void {
    if (!this.enabled) return;
    // A pending identity challenge owns the keyboard: a shortcut would open a
    // dialog over it, or cancel the Escape keydown that closes it. An open
    // global overlay owns it for the same two reasons (Ctrl/Cmd+, would show
    // Settings above it in the top layer). Mute and deafen do neither, and a
    // user in a call must still be able to use them.
    const overlayOwnsKeys =
      !!useMFAChallengeStore.getState().challengeToken ||
      document.querySelector(`dialog[open][${GLOBAL_OVERLAY_ATTRIBUTE}]`) !== null;

    const { shortcuts } = useKeyboardShortcutStore.getState();
    const inInput = this.isInTextInput(event);

    for (const shortcut of shortcuts) {
      if (!this.matchesCombo(event, shortcut.combo)) continue;
      if (inInput && !shortcut.allowInInput) continue;
      if (overlayOwnsKeys && !RUNS_BEHIND_OVERLAY.has(shortcut.id)) continue;

      const handler = this.handlers.get(shortcut.id);
      if (handler) {
        event.preventDefault();
        event.stopPropagation();
        handler();
        return;
      }
    }
  }

  /** Match a keyboard event against a key combo */
  matchesCombo(event: KeyboardEvent, combo: KeyCombo): boolean {
    // Key must match (case-insensitive for letter keys)
    if (event.key.toLowerCase() !== combo.key.toLowerCase()) return false;

    // Ctrl: maps to metaKey on macOS, ctrlKey elsewhere
    const ctrlPressed = this.isMac ? event.metaKey : event.ctrlKey;
    if (!!combo.ctrl !== ctrlPressed) return false;

    // Shift
    if (!!combo.shift !== event.shiftKey) return false;

    // Alt/Option
    if (!!combo.alt !== event.altKey) return false;

    // On macOS, don't fire ctrl shortcuts when only Ctrl (not Cmd) is pressed
    // (unless no ctrl modifier is expected)
    if (this.isMac && combo.ctrl && event.ctrlKey && !event.metaKey) return false;

    return true;
  }

  /** Check if the event target is a text input */
  isInTextInput(event: KeyboardEvent): boolean {
    const target = event.target as HTMLElement;
    if (!target?.tagName) return false;

    const tagName = target.tagName.toLowerCase();
    if (tagName === 'textarea') return true;
    if (tagName === 'input') {
      const type = (target as HTMLInputElement).type?.toLowerCase();
      const textTypes = ['text', 'search', 'url', 'email', 'password', 'number', 'tel'];
      return textTypes.includes(type || 'text');
    }
    if (target.isContentEditable || target.contentEditable === 'true') return true;

    return false;
  }

  /** Whether the current platform is macOS */
  get isMacPlatform(): boolean {
    return this.isMac;
  }
}

export const keyboardShortcutService = new KeyboardShortcutService();
