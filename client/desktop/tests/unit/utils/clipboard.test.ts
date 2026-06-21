import { copyText, readText, cutSelection, selectAll } from '@/renderer/utils/clipboard';

describe('clipboard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // --- copyText ---

  describe('copyText', () => {
    it('copies text to clipboard and returns success', async () => {
      const result = await copyText('hello');
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('hello');
      expect(result.success).toBe(true);
    });

    it('returns error when clipboard write is denied', async () => {
      vi.mocked(navigator.clipboard.writeText).mockRejectedValueOnce(
        new DOMException('Write denied')
      );
      const result = await copyText('hello');
      expect(result.success).toBe(false);
      expect(result.error).toMatch(/clipboard access denied/i);
    });

    it('prefers electron.writeClipboard when available', async () => {
      const mockWriteClipboard = vi.fn().mockResolvedValue(undefined);
      const origElectron = globalThis.electron;
      globalThis.electron = { ...origElectron, writeClipboard: mockWriteClipboard };

      const result = await copyText('electron text');
      expect(mockWriteClipboard).toHaveBeenCalledWith('electron text');
      expect(navigator.clipboard.writeText).not.toHaveBeenCalled();
      expect(result.success).toBe(true);

      globalThis.electron = origElectron;
    });

    it('returns error when electron.writeClipboard fails', async () => {
      const mockWriteClipboard = vi.fn().mockRejectedValue(new Error('IPC failed'));
      const origElectron = globalThis.electron;
      globalThis.electron = { ...origElectron, writeClipboard: mockWriteClipboard };

      const result = await copyText('fail');
      expect(result.success).toBe(false);
      expect(result.error).toMatch(/clipboard access denied/i);

      globalThis.electron = origElectron;
    });
  });

  // --- readText ---

  describe('readText', () => {
    it('reads text from clipboard', async () => {
      vi.mocked(navigator.clipboard.readText).mockResolvedValueOnce('pasted');
      const result = await readText();
      expect(result.success).toBe(true);
      expect(result.text).toBe('pasted');
    });

    it('returns null text when clipboard read is denied', async () => {
      vi.mocked(navigator.clipboard.readText).mockRejectedValueOnce(
        new DOMException('Read denied')
      );
      const result = await readText();
      expect(result.success).toBe(false);
      expect(result.text).toBeNull();
      expect(result.error).toMatch(/clipboard access denied/i);
    });
  });

  // --- cutSelection ---

  describe('cutSelection', () => {
    it('cuts selected text from input', async () => {
      const input = document.createElement('input');
      input.value = 'hello world';
      input.selectionStart = 0;
      input.selectionEnd = 5;
      document.body.appendChild(input);

      const result = await cutSelection(input);
      expect(result.success).toBe(true);
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('hello');
      // Selected text should be removed via setRangeText
      expect(input.value).toBe(' world');

      input.remove();
    });

    it('returns error when nothing is selected', async () => {
      const input = document.createElement('input');
      input.value = 'hello';
      input.selectionStart = 3;
      input.selectionEnd = 3;

      const result = await cutSelection(input);
      expect(result.success).toBe(false);
      expect(result.error).toBe('No text selected');
    });

    it('does not delete if copy fails', async () => {
      const input = document.createElement('input');
      input.value = 'hello world';
      input.selectionStart = 0;
      input.selectionEnd = 5;
      document.body.appendChild(input);

      vi.mocked(navigator.clipboard.writeText).mockRejectedValueOnce(new DOMException('denied'));

      const result = await cutSelection(input);
      expect(result.success).toBe(false);
      // Text should not have been removed
      expect(input.value).toBe('hello world');

      input.remove();
    });

    it('works with textarea elements', async () => {
      const textarea = document.createElement('textarea');
      textarea.value = 'line one\nline two';
      textarea.selectionStart = 0;
      textarea.selectionEnd = 8;
      document.body.appendChild(textarea);

      const result = await cutSelection(textarea);
      expect(result.success).toBe(true);
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('line one');

      textarea.remove();
    });
  });

  // --- selectAll ---

  describe('selectAll', () => {
    it('selects all text in an input element', () => {
      const input = document.createElement('input');
      input.value = 'hello world';
      document.body.appendChild(input);

      const selectSpy = vi.spyOn(input, 'select');
      selectAll(input);
      expect(selectSpy).toHaveBeenCalled();

      input.remove();
    });

    it('selects all text in a textarea element', () => {
      const textarea = document.createElement('textarea');
      textarea.value = 'hello world';
      document.body.appendChild(textarea);

      const selectSpy = vi.spyOn(textarea, 'select');
      selectAll(textarea);
      expect(selectSpy).toHaveBeenCalled();

      textarea.remove();
    });

    it('selects all content in a contenteditable element', () => {
      const div = document.createElement('div');
      div.setAttribute('contenteditable', 'true');
      div.textContent = 'editable content';
      document.body.appendChild(div);

      selectAll(div);

      // Verify selection was created around the element's contents
      const sel = globalThis.getSelection();
      expect(sel?.rangeCount).toBeGreaterThan(0);

      div.remove();
    });

    it('does nothing for non-editable elements', () => {
      const div = document.createElement('div');
      div.textContent = 'plain text';
      // Should not throw on a plain non-editable node
      expect(() => selectAll(div)).not.toThrow();
    });
  });
});
