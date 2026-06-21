import React from 'react';
import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import EmojiGrid from '@/renderer/components/EmojiPicker/EmojiGrid';
import type { EmojiEntry, SkinTone } from '@/renderer/components/EmojiPicker/types';

const makeEmojis = (count: number): EmojiEntry[] =>
  Array.from({ length: count }, (_, i) => ({
    e: `emoji-${i}`,
    n: `emoji name ${i}`,
    s: i % 2 === 0, // alternating skin-tone support
  }));

const NO_SKIN: SkinTone = '';
const LIGHT_SKIN: SkinTone = '\u{1F3FB}';

describe('EmojiGrid — loading state', () => {
  it('shows "Loading..." when isLoading is true', () => {
    render(<EmojiGrid emojis={[]} onSelect={vi.fn()} skinTone={NO_SKIN} isLoading={true} />);
    expect(screen.getByText('Loading...')).toBeInTheDocument();
  });

  it('does not render emoji buttons while loading', () => {
    const emojis = makeEmojis(5);
    render(<EmojiGrid emojis={emojis} onSelect={vi.fn()} skinTone={NO_SKIN} isLoading={true} />);
    expect(screen.queryAllByRole('button')).toHaveLength(0);
  });
});

describe('EmojiGrid — empty state', () => {
  it('shows "No emoji found" when emojis is empty and not loading', () => {
    render(<EmojiGrid emojis={[]} onSelect={vi.fn()} skinTone={NO_SKIN} isLoading={false} />);
    expect(screen.getByText('No emoji found')).toBeInTheDocument();
  });
});

describe('EmojiGrid — rendering emojis', () => {
  it('renders a button for each emoji with the correct aria-label', () => {
    const emojis = makeEmojis(3);
    render(<EmojiGrid emojis={emojis} onSelect={vi.fn()} skinTone={NO_SKIN} isLoading={false} />);

    for (const emoji of emojis) {
      expect(screen.getByRole('button', { name: emoji.n })).toBeInTheDocument();
    }
  });

  it('calls onSelect with the display emoji when a button is clicked', () => {
    const onSelect = vi.fn();
    const emojis: EmojiEntry[] = [{ e: '😀', n: 'grinning face', s: false }];
    render(<EmojiGrid emojis={emojis} onSelect={onSelect} skinTone={NO_SKIN} isLoading={false} />);

    fireEvent.click(screen.getByRole('button', { name: 'grinning face' }));
    expect(onSelect).toHaveBeenCalledWith('😀');
  });
});

describe('EmojiGrid — skin tone', () => {
  it('applies skin tone modifier for emojis that support it', () => {
    const onSelect = vi.fn();
    const emojis: EmojiEntry[] = [
      { e: '👋', n: 'waving hand', s: true },
      { e: '😀', n: 'grinning face', s: false },
    ];
    render(
      <EmojiGrid emojis={emojis} onSelect={onSelect} skinTone={LIGHT_SKIN} isLoading={false} />
    );

    fireEvent.click(screen.getByRole('button', { name: 'waving hand' }));
    const firstArg = onSelect.mock.calls[0]?.[0] as string;
    expect(firstArg).toContain(LIGHT_SKIN);
  });

  it('does NOT apply skin tone modifier for emojis that do not support it', () => {
    const onSelect = vi.fn();
    const emojis: EmojiEntry[] = [{ e: '😀', n: 'grinning face', s: false }];
    render(
      <EmojiGrid emojis={emojis} onSelect={onSelect} skinTone={LIGHT_SKIN} isLoading={false} />
    );

    fireEvent.click(screen.getByRole('button', { name: 'grinning face' }));
    expect(onSelect).toHaveBeenCalledWith('😀');
  });
});

describe('EmojiGrid — hover callbacks', () => {
  it('calls onHover with the emoji entry on mouseenter', () => {
    const onHover = vi.fn();
    const emojis: EmojiEntry[] = [{ e: '🎉', n: 'party popper', s: false }];
    render(
      <EmojiGrid
        emojis={emojis}
        onSelect={vi.fn()}
        skinTone={NO_SKIN}
        isLoading={false}
        onHover={onHover}
      />
    );

    fireEvent.mouseEnter(screen.getByRole('button', { name: 'party popper' }));
    expect(onHover).toHaveBeenCalledWith(emojis[0]);
  });

  it('calls onHover with null on mouseleave', () => {
    const onHover = vi.fn();
    const emojis: EmojiEntry[] = [{ e: '🎉', n: 'party popper', s: false }];
    render(
      <EmojiGrid
        emojis={emojis}
        onSelect={vi.fn()}
        skinTone={NO_SKIN}
        isLoading={false}
        onHover={onHover}
      />
    );

    const button = screen.getByRole('button', { name: 'party popper' });
    fireEvent.mouseEnter(button);
    fireEvent.mouseLeave(button);
    expect(onHover).toHaveBeenLastCalledWith(null);
  });
});

describe('EmojiGrid — scroll reset', () => {
  it('renders the grid container when emojis are provided', () => {
    const { container } = render(
      <EmojiGrid emojis={makeEmojis(8)} onSelect={vi.fn()} skinTone={NO_SKIN} isLoading={false} />
    );
    expect(container.querySelector('.emoji-picker-grid-viewport')).toBeInTheDocument();
  });
});
