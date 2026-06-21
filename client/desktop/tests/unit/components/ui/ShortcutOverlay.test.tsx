import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import ShortcutOverlay from '@/renderer/components/ui/ShortcutOverlay';
import { useKeyboardShortcutStore } from '@/renderer/stores/keyboardShortcutStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/ui/ShortcutOverlay.css', () => ({}));

vi.mock('@/renderer/services/keyboardShortcutService', () => ({
  keyboardShortcutService: { isMacPlatform: false },
}));

const TEST_SHORTCUTS = [
  {
    id: 'nav-up',
    label: 'Previous Channel',
    category: 'navigation' as const,
    combo: { key: 'ArrowUp', alt: true },
  },
  {
    id: 'nav-down',
    label: 'Next Channel',
    category: 'navigation' as const,
    combo: { key: 'ArrowDown', alt: true },
  },
  {
    id: 'msg-send',
    label: 'Send Message',
    category: 'messaging' as const,
    combo: { key: 'Enter' },
  },
  {
    id: 'msg-newline',
    label: 'New Line',
    category: 'messaging' as const,
    combo: { key: 'Enter', shift: true },
  },
  {
    id: 'app-shortcuts',
    label: 'Show Shortcuts',
    category: 'app' as const,
    combo: { key: '/', ctrl: true },
  },
  {
    id: 'voice-mute',
    label: 'Toggle Mute',
    category: 'voice' as const,
    combo: { key: 'm', ctrl: true },
  },
  {
    id: 'voice-deafen',
    label: 'Toggle Deafen',
    category: 'voice' as const,
    combo: { key: 'd', ctrl: true },
  },
];

function setStoreState(
  overrides: Partial<{ overlayOpen: boolean; shortcuts: typeof TEST_SHORTCUTS }>
) {
  const defaults = { overlayOpen: true, shortcuts: TEST_SHORTCUTS };
  const state = { ...defaults, ...overrides };
  useKeyboardShortcutStore.setState({
    overlayOpen: state.overlayOpen,
    shortcuts: state.shortcuts,
    closeOverlay: vi.fn(() => useKeyboardShortcutStore.setState({ overlayOpen: false })),
  });
}

describe('ShortcutOverlay', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('returns null when overlayOpen is false', () => {
    setStoreState({ overlayOpen: false });
    const { container } = render(<ShortcutOverlay />);
    expect(container.innerHTML).toBe('');
  });

  it('renders modal with title "Keyboard Shortcuts" when open', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);
    expect(screen.getByText('Keyboard Shortcuts')).toBeInTheDocument();
  });

  it('renders all shortcuts grouped by category', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);
    for (const shortcut of TEST_SHORTCUTS) {
      expect(screen.getByText(shortcut.label)).toBeInTheDocument();
    }
  });

  it('shows category headers', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);
    expect(screen.getByText('Navigation')).toBeInTheDocument();
    expect(screen.getByText('Messaging')).toBeInTheDocument();
    expect(screen.getByText('Application')).toBeInTheDocument();
    expect(screen.getByText('Audio & Video')).toBeInTheDocument();
  });

  it('filters shortcuts by search term', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);

    const searchInput = screen.getByPlaceholderText('Search shortcuts...');
    fireEvent.change(searchInput, { target: { value: 'mute' } });

    expect(screen.getByText('Toggle Mute')).toBeInTheDocument();
    expect(screen.queryByText('Send Message')).not.toBeInTheDocument();
    expect(screen.queryByText('Previous Channel')).not.toBeInTheDocument();
  });

  it('shows empty state when search has no results', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);

    const searchInput = screen.getByPlaceholderText('Search shortcuts...');
    fireEvent.change(searchInput, { target: { value: 'zzzznonexistent' } });

    expect(screen.getByText('No shortcuts match your search')).toBeInTheDocument();
  });

  it('renders kbd elements for key combos', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);

    const kbdElements = document.querySelectorAll('.shortcut-kbd');
    expect(kbdElements.length).toBeGreaterThan(0);

    // Check that Ctrl modifier appears (non-Mac mode)
    const kbdTexts = Array.from(kbdElements).map((el) => el.textContent);
    expect(kbdTexts).toContain('Ctrl');
  });

  it('formats arrow keys as symbols', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);

    const kbdElements = document.querySelectorAll('.shortcut-kbd');
    const kbdTexts = Array.from(kbdElements).map((el) => el.textContent);
    expect(kbdTexts).toContain('↑');
    expect(kbdTexts).toContain('↓');
  });

  it('filters by category name', () => {
    setStoreState({ overlayOpen: true });
    render(<ShortcutOverlay />);

    const searchInput = screen.getByPlaceholderText('Search shortcuts...');
    fireEvent.change(searchInput, { target: { value: 'voice' } });

    expect(screen.getByText('Toggle Mute')).toBeInTheDocument();
    expect(screen.getByText('Toggle Deafen')).toBeInTheDocument();
    expect(screen.queryByText('Send Message')).not.toBeInTheDocument();
  });
});
