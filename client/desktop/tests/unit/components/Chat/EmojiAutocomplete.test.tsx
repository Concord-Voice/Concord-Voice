import { createRef } from 'react';
import type React from 'react';
import { render, fireEvent, act } from '../../../test-utils';
import EmojiAutocomplete, {
  type EmojiAutocompleteHandle,
} from '@/renderer/components/Chat/EmojiAutocomplete';
import { vi, describe, it, expect } from 'vitest';

// jsdom lacks scrollIntoView (used by the selected-item effect).
Element.prototype.scrollIntoView = vi.fn();

// Deterministic shortcode search over a tiny fixture set.
vi.mock('@/renderer/components/EmojiPicker/useEmojiData', () => ({
  useEmojiData: () => ({
    search: (q: string) => {
      const code = q.startsWith(':') ? q.slice(1) : q;
      if (!code) return [];
      const all = [
        { e: '😄', n: 'smile', s: false, c: ['smile'] },
        { e: '😊', n: 'smiley', s: false, c: ['smiley'] },
        { e: '👍', n: 'thumbsup', s: false, c: ['thumbsup'] },
      ];
      return all.filter((x) => x.c.some((cc) => cc.startsWith(code)));
    },
    loadAllForSearch: () => Promise.resolve(),
  }),
}));

const key = (k: string) => ({ key: k, preventDefault: vi.fn() }) as unknown as React.KeyboardEvent;

describe('EmojiAutocomplete (#1754)', () => {
  it('renders shortcode matches for an active :query token', () => {
    const { container } = render(
      <EmojiAutocomplete text=":sm" cursorPosition={3} onSelect={vi.fn()} onClose={vi.fn()} />
    );
    expect(container.querySelectorAll('.emoji-option')).toHaveLength(2); // smile, smiley
    expect(container.textContent).toContain(':smile:');
    expect(container.textContent).toContain('😄');
  });

  it('renders nothing when there is no active : token', () => {
    const { container } = render(
      <EmojiAutocomplete text="hello" cursorPosition={5} onSelect={vi.fn()} onClose={vi.fn()} />
    );
    expect(container.querySelector('.emoji-autocomplete')).toBeNull();
  });

  it('renders nothing for a colon not at a word boundary (3:30)', () => {
    const { container } = render(
      <EmojiAutocomplete text="3:30" cursorPosition={4} onSelect={vi.fn()} onClose={vi.fn()} />
    );
    expect(container.querySelector('.emoji-autocomplete')).toBeNull();
  });

  it('triggers when the : sits right after a tab (tab is a word boundary, like a space)', () => {
    const { container } = render(
      <EmojiAutocomplete text={'a\t:sm'} cursorPosition={5} onSelect={vi.fn()} onClose={vi.fn()} />
    );
    // '\t' preceding the ':' must count as a boundary, so the token triggers.
    expect(container.querySelectorAll('.emoji-option')).toHaveLength(2); // smile, smiley
  });

  it('renders nothing for a completed :smile: token (closing colon, not re-triggered)', () => {
    const { container } = render(
      <EmojiAutocomplete text=":smile:" cursorPosition={7} onSelect={vi.fn()} onClose={vi.fn()} />
    );
    expect(container.querySelector('.emoji-autocomplete')).toBeNull();
  });

  it('inserts the glyph + space on mouse select (with the token start offset)', () => {
    const onSelect = vi.fn();
    const { container } = render(
      <EmojiAutocomplete
        text="hi :smile"
        cursorPosition={9}
        onSelect={onSelect}
        onClose={vi.fn()}
      />
    );
    fireEvent.mouseDown(container.querySelector('.emoji-option')!);
    expect(onSelect).toHaveBeenCalledWith('😄 ', 3); // ':' is at index 3
  });

  it('Enter selects the highlighted option via the imperative handle', () => {
    const onSelect = vi.fn();
    const ref = createRef<EmojiAutocompleteHandle>();
    render(
      <EmojiAutocomplete
        ref={ref}
        text=":smile"
        cursorPosition={6}
        onSelect={onSelect}
        onClose={vi.fn()}
      />
    );
    expect(ref.current!.handleKeyDown(key('Enter'))).toBe(true);
    expect(onSelect).toHaveBeenCalledWith('😄 ', 0);
  });

  it('Escape closes via the imperative handle', () => {
    const onClose = vi.fn();
    const ref = createRef<EmojiAutocompleteHandle>();
    render(
      <EmojiAutocomplete
        ref={ref}
        text=":sm"
        cursorPosition={3}
        onSelect={vi.fn()}
        onClose={onClose}
      />
    );
    expect(ref.current!.handleKeyDown(key('Escape'))).toBe(true);
    expect(onClose).toHaveBeenCalled();
  });

  it('ArrowDown then Enter selects the second option', () => {
    const onSelect = vi.fn();
    const ref = createRef<EmojiAutocompleteHandle>();
    render(
      <EmojiAutocomplete
        ref={ref}
        text=":sm"
        cursorPosition={3}
        onSelect={onSelect}
        onClose={vi.fn()}
      />
    );
    act(() => {
      ref.current!.handleKeyDown(key('ArrowDown'));
    });
    ref.current!.handleKeyDown(key('Enter'));
    expect(onSelect).toHaveBeenCalledWith('😊 ', 0); // smiley = 2nd option
  });

  it('returns false for keys it does not handle (so send/shortcuts still fire)', () => {
    const ref = createRef<EmojiAutocompleteHandle>();
    render(
      <EmojiAutocomplete
        ref={ref}
        text=":sm"
        cursorPosition={3}
        onSelect={vi.fn()}
        onClose={vi.fn()}
      />
    );
    expect(ref.current!.handleKeyDown(key('a'))).toBe(false);
  });
});
