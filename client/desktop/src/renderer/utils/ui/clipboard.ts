/**
 * Clipboard operations with error handling.
 *
 * Uses Electron's IPC-backed clipboard for writes (navigator.clipboard.writeText
 * is blocked in Electron renderers), and falls back to the Web Clipboard API
 * for non-Electron contexts and reads.
 */

export interface ClipboardResult {
  success: boolean;
  /** If denied, a human-readable message the UI can display. */
  error?: string;
}

/**
 * Copy text to the system clipboard.
 * Prefers Electron IPC (writeClipboard) since navigator.clipboard.writeText
 * is blocked in Electron renderers.
 */
export async function copyText(text: string): Promise<ClipboardResult> {
  try {
    if (globalThis.electron?.writeClipboard) {
      await globalThis.electron.writeClipboard(text);
    } else {
      await navigator.clipboard.writeText(text);
    }
    return { success: true };
  } catch {
    return {
      success: false,
      error: 'Clipboard access denied. Check your OS privacy settings.',
    };
  }
}

/**
 * Read text from the system clipboard.
 */
export async function readText(): Promise<{ text: string | null } & ClipboardResult> {
  try {
    const text = await navigator.clipboard.readText();
    return { success: true, text };
  } catch {
    return {
      success: false,
      text: null,
      error: 'Clipboard access denied. Check your OS privacy settings.',
    };
  }
}

/**
 * Cut the current selection in an input/textarea element:
 * copies selected text to clipboard, then deletes the selection.
 */
export async function cutSelection(
  element: HTMLInputElement | HTMLTextAreaElement
): Promise<ClipboardResult> {
  const start = element.selectionStart ?? 0;
  const end = element.selectionEnd ?? 0;
  const selected = element.value.substring(start, end);

  if (!selected) return { success: false, error: 'No text selected' };

  const result = await copyText(selected);
  if (result.success) {
    // Remove the selected text by replacing it with empty string
    element.focus();
    element.setRangeText('', start, end, 'end');
    element.dispatchEvent(new Event('input', { bubbles: true }));
  }
  return result;
}

/**
 * Select all text within an input, textarea, or contentEditable element.
 */
export function selectAll(element: HTMLElement): void {
  if (element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement) {
    element.select();
  } else if (element.isContentEditable || element.getAttribute('contenteditable') === 'true') {
    const range = document.createRange();
    range.selectNodeContents(element);
    const sel = globalThis.getSelection();
    sel?.removeAllRanges();
    sel?.addRange(range);
  }
}
