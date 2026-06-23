import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// Mock the lazy emoji picker (same shape the RoleEditorPanel suite uses).
vi.mock('@/renderer/components/EmojiPicker/LazyEmojiPicker', () => ({
  default: ({ onSelect, onClose }: { onSelect: (emoji: string) => void; onClose: () => void }) => (
    <div data-testid="lazy-emoji-picker">
      <button data-testid="pick-emoji" onClick={() => onSelect('\u{1F389}')}>
        Pick
      </button>
      <button data-testid="close-emoji" onClick={onClose}>
        Close
      </button>
    </div>
  ),
}));

import CategoryManagerPanel from '@/renderer/components/DirectMessages/CategoryManagerPanel';

const resetFriendOrg = () =>
  useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });

const categoryManagerCss = readFileSync(
  resolve(process.cwd(), 'src/renderer/components/DirectMessages/CategoryManagerPanel.css'),
  'utf-8'
);

const expectLocalCssRule = (selector: string) => {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  expect(
    categoryManagerCss,
    `CategoryManagerPanel.css must define ${selector} because CategoryManagerPanel renders that class directly`
  ).toMatch(new RegExp(`(?:^|\\n)${escapedSelector}\\s*\\{`));
};

describe('CategoryManagerPanel', () => {
  beforeEach(() => {
    resetAllStores();
    resetFriendOrg();
    vi.clearAllMocks();
  });

  it('renders empty-state message when no category is selected', () => {
    render(<CategoryManagerPanel onClose={() => {}} />);
    expect(screen.getByText('Select a category to edit, or create a new one.')).toBeInTheDocument();
  });

  it('keeps the friend category manager styling contract local to its imported CSS', () => {
    const localSelectors = [
      '.category-manager .roles-layout',
      '.category-manager .roles-list',
      '.category-manager .role-item',
      '.category-manager .role-item.selected',
      '.category-manager .role-color-dot',
      '.category-manager .create-role-btn',
      '.category-manager .role-editor',
      '.category-manager .form-group',
      '.category-manager .form-label',
      '.category-manager .form-input',
      '.category-manager .emoji-input-wrapper',
      '.category-manager .emoji-input-container',
      '.category-manager .emoji-picker-button',
      '.category-manager .emoji-picker-button-placeholder',
      '.category-manager .emoji-clear-btn',
      '.category-manager .emoji-picker-container',
      '.category-manager .channel-form-hint',
      '.category-manager .role-editor-actions',
      '.category-manager .server-settings-cancel-btn',
      '.category-manager .server-settings-submit-btn',
    ];

    for (const selector of localSelectors) {
      expectLocalCssRule(selector);
    }
  });

  it('creates a category from the form', () => {
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByRole('button', { name: /new category/i }));
    fireEvent.change(screen.getByLabelText(/category name/i), { target: { value: 'Gaming' } });
    fireEvent.click(screen.getByRole('button', { name: /save/i }));

    expect(useFriendOrgStore.getState().categories.map((c) => c.name)).toContain('Gaming');
  });

  it('selecting a category shows its editor pre-filled with the name', () => {
    act(() => {
      useFriendOrgStore.getState().createCategory('Close Friends', '\u{1F49C}', '#fa709a');
    });
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByText('Close Friends').closest('button')!);

    expect(screen.getByLabelText(/category name/i)).toHaveValue('Close Friends');
  });

  it('renames a selected category', () => {
    let id = '';
    act(() => {
      id = useFriendOrgStore.getState().createCategory('Old Name', '', null);
    });
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByText('Old Name').closest('button')!);
    fireEvent.change(screen.getByLabelText(/category name/i), { target: { value: 'New Name' } });
    fireEvent.click(screen.getByRole('button', { name: /save/i }));

    expect(useFriendOrgStore.getState().categories.find((c) => c.id === id)?.name).toBe('New Name');
  });

  it('applies a color via the hex input on save', () => {
    let id = '';
    act(() => {
      id = useFriendOrgStore.getState().createCategory('Styled', '', null);
    });
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByText('Styled').closest('button')!);

    const colorTextInput = screen
      .getByLabelText(/category color/i)
      .closest('.form-group')!
      .querySelector('input[type="text"]') as HTMLInputElement;
    fireEvent.change(colorTextInput, { target: { value: '#00ff00' } });
    fireEvent.click(screen.getByRole('button', { name: /save/i }));

    expect(useFriendOrgStore.getState().categories.find((c) => c.id === id)?.color).toBe('#00ff00');
  });

  it('applies an emoji via the picker on save', () => {
    let id = '';
    act(() => {
      id = useFriendOrgStore.getState().createCategory('Emoji Cat', '', null);
    });
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByText('Emoji Cat').closest('button')!);

    // Open the emoji picker, pick the test emoji.
    fireEvent.click(screen.getByTitle(/pick an emoji/i));
    fireEvent.click(screen.getByTestId('pick-emoji'));
    fireEvent.click(screen.getByRole('button', { name: /save/i }));

    expect(useFriendOrgStore.getState().categories.find((c) => c.id === id)?.emoji).toBe(
      '\u{1F389}'
    );
  });

  it('deletes a category after confirming, noting members move back to Online/Offline', () => {
    let id = '';
    act(() => {
      id = useFriendOrgStore.getState().createCategory('Doomed', '', null);
      useFriendOrgStore.getState().assignFriend('u1', id);
    });
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByText('Doomed').closest('button')!);
    fireEvent.click(screen.getByRole('button', { name: /^delete$/i }));

    // Confirm dialog appears, noting members return to Online/Offline.
    expect(screen.getByText(/move back to Online\/Offline/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /^delete category$/i }));

    const s = useFriendOrgStore.getState();
    expect(s.categories.find((c) => c.id === id)).toBeUndefined();
    expect(s.sectionOrder).not.toContain(id);
  });

  it('cancels a delete via the confirm dialog (category survives)', () => {
    let id = '';
    act(() => {
      id = useFriendOrgStore.getState().createCategory('Survivor', '', null);
    });
    render(<CategoryManagerPanel onClose={() => {}} />);

    fireEvent.click(screen.getByText('Survivor').closest('button')!);
    fireEvent.click(screen.getByRole('button', { name: /^delete$/i }));
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }));

    expect(useFriendOrgStore.getState().categories.find((c) => c.id === id)).toBeDefined();
  });

  it('calls onClose when the close button is clicked', () => {
    const onClose = vi.fn();
    render(<CategoryManagerPanel onClose={onClose} />);

    fireEvent.click(screen.getByRole('button', { name: /close/i }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
