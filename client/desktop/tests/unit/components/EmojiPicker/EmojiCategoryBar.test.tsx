import React from 'react';
import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import EmojiCategoryBar from '@/renderer/components/EmojiPicker/EmojiCategoryBar';
import type { EmojiCategory } from '@/renderer/components/EmojiPicker/types';

const mockCategories: EmojiCategory[] = [
  { id: 'smileys', name: 'Smileys & Emotion', icon: '😀', file: 'smileys.json', count: 10 },
  { id: 'people', name: 'People & Body', icon: '👋', file: 'people.json', count: 8 },
  { id: 'animals', name: 'Animals & Nature', icon: '🐶', file: 'animals.json', count: 5 },
];

describe('EmojiCategoryBar', () => {
  it('renders a button for each category', () => {
    render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="smileys"
        hasRecent={false}
        onSelect={vi.fn()}
      />
    );

    for (const cat of mockCategories) {
      expect(screen.getByRole('button', { name: cat.name })).toBeInTheDocument();
    }
  });

  it('does NOT render the recent button when hasRecent is false', () => {
    render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="smileys"
        hasRecent={false}
        onSelect={vi.fn()}
      />
    );

    expect(screen.queryByRole('button', { name: 'Frequently Used' })).not.toBeInTheDocument();
  });

  it('renders the recent button when hasRecent is true', () => {
    render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="smileys"
        hasRecent={true}
        onSelect={vi.fn()}
      />
    );

    expect(screen.getByRole('button', { name: 'Frequently Used' })).toBeInTheDocument();
  });

  it('applies the active class to the matching category', () => {
    const { container } = render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="people"
        hasRecent={false}
        onSelect={vi.fn()}
      />
    );

    const peopleBtn = screen.getByRole('button', { name: 'People & Body' });
    expect(peopleBtn.className).toContain('--active');

    const smileyBtn = screen.getByRole('button', { name: 'Smileys & Emotion' });
    expect(smileyBtn.className).not.toContain('--active');

    // ensure the container itself rendered
    expect(container.querySelector('.emoji-picker-categories')).toBeInTheDocument();
  });

  it('applies the active class to the recent button when activeCategory is "recent"', () => {
    render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="recent"
        hasRecent={true}
        onSelect={vi.fn()}
      />
    );

    const recentBtn = screen.getByRole('button', { name: 'Frequently Used' });
    expect(recentBtn.className).toContain('--active');
  });

  it('calls onSelect with the category id when a category button is clicked', () => {
    const onSelect = vi.fn();
    render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="smileys"
        hasRecent={false}
        onSelect={onSelect}
      />
    );

    fireEvent.click(screen.getByRole('button', { name: 'Animals & Nature' }));
    expect(onSelect).toHaveBeenCalledWith('animals');
  });

  it('calls onSelect with "recent" when the recent button is clicked', () => {
    const onSelect = vi.fn();
    render(
      <EmojiCategoryBar
        categories={mockCategories}
        activeCategory="smileys"
        hasRecent={true}
        onSelect={onSelect}
      />
    );

    fireEvent.click(screen.getByRole('button', { name: 'Frequently Used' }));
    expect(onSelect).toHaveBeenCalledWith('recent');
  });

  it('renders nothing when categories array is empty and hasRecent is false', () => {
    const { container } = render(
      <EmojiCategoryBar categories={[]} activeCategory="" hasRecent={false} onSelect={vi.fn()} />
    );

    const buttons = container.querySelectorAll('button');
    expect(buttons).toHaveLength(0);
  });
});
