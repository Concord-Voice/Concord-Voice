import React from 'react';
import { act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import userEvent from '@testing-library/user-event';
import { render, screen } from '../../../test-utils';
import type { EmojiCategory, EmojiEntry } from '@/renderer/components/EmojiPicker/types';

// ─── Module mocks ─────────────────────────────────────────────────────────────

const mockLoadCategory = vi.fn().mockResolvedValue([]);
const mockGetCategory = vi.fn().mockReturnValue([]);
const mockSearch = vi.fn().mockReturnValue([]);
const mockLoadAllForSearch = vi.fn().mockResolvedValue(undefined);

const mockCategories: EmojiCategory[] = [
  { id: 'smileys', name: 'Smileys', icon: '😀', file: 'smileys.json', count: 2 },
  { id: 'people', name: 'People', icon: '👋', file: 'people.json', count: 1 },
];

vi.mock('@/renderer/components/EmojiPicker/useEmojiData', () => ({
  useEmojiData: () => ({
    categories: mockCategories,
    loadingCategory: null,
    loadCategory: mockLoadCategory,
    getCategory: mockGetCategory,
    search: mockSearch,
    loadAllForSearch: mockLoadAllForSearch,
  }),
}));

const mockGetRecentEmojis = vi.fn().mockReturnValue([]);
const mockAddRecentEmoji = vi.fn();
const mockGetSavedSkinTone = vi.fn().mockReturnValue('');
const mockSaveSkinTone = vi.fn();

vi.mock('@/renderer/components/EmojiPicker/emojiDataCache', () => ({
  getRecentEmojis: () => mockGetRecentEmojis(),
  addRecentEmoji: (e: string) => mockAddRecentEmoji(e),
  getSavedSkinTone: () => mockGetSavedSkinTone(),
  saveSkinTone: (t: string) => mockSaveSkinTone(t),
}));

// ─── Import component after mocks ─────────────────────────────────────────────

const { default: EmojiPicker } = await import('@/renderer/components/EmojiPicker/EmojiPicker');

// ─── Tests ───────────────────────────────────────────────────────────────────

beforeEach(() => {
  vi.clearAllMocks();
  mockGetRecentEmojis.mockReturnValue([]);
  mockGetSavedSkinTone.mockReturnValue('');
  mockGetCategory.mockReturnValue([]);
  mockSearch.mockReturnValue([]);
});

describe('EmojiPicker — basic rendering', () => {
  it('renders the search input', async () => {
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);
    await waitFor(() => expect(screen.getByPlaceholderText('Search emoji...')).toBeInTheDocument());
  });

  it('renders category buttons', async () => {
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Smileys' })).toBeInTheDocument()
    );
    expect(screen.getByRole('button', { name: 'People' })).toBeInTheDocument();
  });

  it('renders the skin tone toggle button', async () => {
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Change skin tone' })).toBeInTheDocument()
    );
  });
});

describe('EmojiPicker — inline vs popover mode', () => {
  it('renders inline mode without a portal', () => {
    const { container } = render(
      <EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />
    );
    expect(container.querySelector('.emoji-picker--inline')).toBeInTheDocument();
  });

  it('renders popover mode inside document.body portal', async () => {
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="popover" />);
    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--popover')).toBeInTheDocument()
    );
  });
});

describe('EmojiPicker — category selection', () => {
  it('calls loadCategory on mount for the initial category', async () => {
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);
    await waitFor(() => expect(mockLoadCategory).toHaveBeenCalled());
  });

  it('calls loadCategory when a different category is selected', async () => {
    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    await waitFor(() => expect(screen.getByRole('button', { name: 'People' })).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'People' }));

    await waitFor(() => expect(mockLoadCategory).toHaveBeenCalledWith('people'));
  });
});

describe('EmojiPicker — search behaviour', () => {
  it('calls search and loadAllForSearch when the user types', async () => {
    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    const input = await screen.findByPlaceholderText('Search emoji...');
    await act(async () => {
      await user.type(input, 'smile');
    });

    await waitFor(() => expect(mockSearch).toHaveBeenCalledWith('smile'));
    expect(mockLoadAllForSearch).toHaveBeenCalled();
  });

  it('hides category bar while a search query is active', async () => {
    mockSearch.mockReturnValue([{ e: '😀', n: 'grinning', s: false }] as EmojiEntry[]);

    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    const input = await screen.findByPlaceholderText('Search emoji...');
    await act(async () => {
      await user.type(input, 'smile');
    });

    // Category bar is hidden during search
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Smileys' })).not.toBeInTheDocument()
    );
  });
});

describe('EmojiPicker — emoji selection', () => {
  it('calls onSelect and addRecentEmoji when an emoji is clicked', async () => {
    const onSelect = vi.fn();
    mockGetCategory.mockReturnValue([{ e: '😀', n: 'grinning face', s: false }]);

    const user = userEvent.setup();
    render(<EmojiPicker onSelect={onSelect} onClose={vi.fn()} mode="inline" />);

    const btn = await screen.findByRole('button', { name: 'grinning face' });
    await user.click(btn);

    expect(onSelect).toHaveBeenCalledWith('😀');
    expect(mockAddRecentEmoji).toHaveBeenCalledWith('😀');
  });
});

describe('EmojiPicker — skin tone', () => {
  it('opens the skin tone picker when the toggle is clicked', async () => {
    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    await user.click(await screen.findByRole('button', { name: 'Change skin tone' }));

    // Skin tone option buttons (Default, Light, Medium-Light, …)
    expect(screen.getByRole('button', { name: 'Default' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Light' })).toBeInTheDocument();
  });

  it('selects a skin tone and closes the picker', async () => {
    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    await user.click(await screen.findByRole('button', { name: 'Change skin tone' }));
    await user.click(screen.getByRole('button', { name: 'Light' }));

    expect(mockSaveSkinTone).toHaveBeenCalledWith('\u{1F3FB}');
    // Picker should be closed after selection
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Default' })).not.toBeInTheDocument()
    );
  });
});

describe('EmojiPicker — close handlers', () => {
  it('calls onClose when Escape is pressed in popover mode', async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={onClose} mode="popover" />);

    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--popover')).toBeInTheDocument()
    );

    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalled();
  });

  it('does NOT call onClose on Escape in inline mode', async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<EmojiPicker onSelect={vi.fn()} onClose={onClose} mode="inline" />);

    await screen.findByPlaceholderText('Search emoji...');
    await user.keyboard('{Escape}');
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe('EmojiPicker — recent emojis', () => {
  it('shows the recent tab when there are recent emojis', async () => {
    mockGetRecentEmojis.mockReturnValue(['😀', '🎉']);

    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Frequently Used' })).toBeInTheDocument()
    );
  });

  it('does not show the recent tab when recents are empty', async () => {
    mockGetRecentEmojis.mockReturnValue([]);

    render(<EmojiPicker onSelect={vi.fn()} onClose={vi.fn()} mode="inline" />);

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Frequently Used' })).not.toBeInTheDocument()
    );
  });
});
