import { render, screen, fireEvent, userEvent } from '../../../test-utils';
import { vi } from 'vitest';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

// Mirrors AccessibilitySection.test.tsx — mock the draft hook so the section renders
// in isolation. Variable is `mock`-prefixed so Vitest's hoisted factory may reference it.
const mockSetDraftAppearanceSetting = vi.fn();
let mockAppFont = 'default';
let mockColorScheme = 'concord';
let mockDyslexicSupport = false;

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAppearance: vi.fn(() => ({
    theme: 'dark',
    colorScheme: mockColorScheme,
    fontSize: 'default',
    compactMode: false,
    reduceAnimations: false,
    uiScale: 1,
    highContrast: false,
    customColors: null,
    appFont: mockAppFont,
    dyslexicSupport: mockDyslexicSupport,
  })),
  setDraftAppearanceSetting: (...args: unknown[]) => mockSetDraftAppearanceSetting(...args),
}));

import FontSection from '@/renderer/components/Settings/FontSection';

beforeEach(() => {
  mockAppFont = 'default';
  mockColorScheme = 'concord';
  mockDyslexicSupport = false;
  mockSetDraftAppearanceSetting.mockClear();
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('FontSection (Appearance ▸ Fonts)', () => {
  it('renders the font options', () => {
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /Concord Voice Default/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /System Default/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /OpenDyslexic/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Inter/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Lexend/i })).toBeInTheDocument();
  });

  it('clicking a font writes appFont to the draft store', () => {
    render(<FontSection />);
    fireEvent.click(screen.getByRole('button', { name: /Inter/i }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('appFont', 'inter');
  });

  it('marks the active font with aria-pressed', () => {
    mockAppFont = 'lexend';
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /Lexend/i })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: /Inter/i })).toHaveAttribute('aria-pressed', 'false');
  });

  it('font options are keyboard-activatable (Enter on a focused option)', async () => {
    const user = userEvent.setup();
    render(<FontSection />);
    const inter = screen.getByRole('button', { name: /Inter/i });
    inter.focus();
    await user.keyboard('{Enter}');
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('appFont', 'inter');
  });

  it('reveals the theme-bundled font when a font-bundling scheme is active', () => {
    mockColorScheme = 'agency';
    render(<FontSection />);
    const atkinson = screen.getByRole('button', { name: /Atkinson Hyperlegible Next/i });
    expect(atkinson).toHaveTextContent(/Provided by the active theme/i);
    // No explicit pick → the theme font is the active option (soft lock).
    expect(atkinson).toHaveAttribute('aria-pressed', 'true');
  });

  it('hides the reveal for a non-bundling scheme', () => {
    mockColorScheme = 'concord';
    render(<FontSection />);
    expect(screen.queryByText(/Provided by the active theme/i)).not.toBeInTheDocument();
  });

  it('an explicit pick overrides the theme font (soft lock — pick wins)', () => {
    mockColorScheme = 'agency';
    mockAppFont = 'inter';
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /Inter/i })).toHaveAttribute('aria-pressed', 'true');
    const atkinson = screen.getByRole('button', { name: /Atkinson Hyperlegible Next/i });
    // Badge stays (informational: the scheme still bundles Atkinson) but it is not active.
    expect(atkinson).toHaveTextContent(/Provided by the active theme/i);
    expect(atkinson).toHaveAttribute('aria-pressed', 'false');
  });

  it('options stay clickable under theme-lock (override writes appFont)', () => {
    mockColorScheme = 'agency';
    render(<FontSection />);
    fireEvent.click(screen.getByRole('button', { name: /Lexend/i }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('appFont', 'lexend');
  });

  it('renders the dyslexic HARD lock when dyslexicSupport is on (#1644)', () => {
    mockDyslexicSupport = true;
    render(<FontSection />);
    const inter = screen.getByRole('button', { name: /Inter/i });
    // options are aria-disabled (still native <button>, keeps S6819 + discoverability)
    expect(inter).toHaveAttribute('aria-disabled', 'true');
    // the visible lock note + back-link are present
    expect(screen.getByText(/managed by/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Accessibility ▸ Display/i })).toBeInTheDocument();
  });

  it('a locked option does NOT write appFont (activation guard holds Q2-restore)', () => {
    mockDyslexicSupport = true;
    render(<FontSection />);
    fireEvent.click(screen.getByRole('button', { name: /Inter/i }));
    expect(mockSetDraftAppearanceSetting).not.toHaveBeenCalledWith('appFont', 'inter');
  });

  it('the back-link requests focus on the dyslexic toggle', () => {
    mockDyslexicSupport = true;
    render(<FontSection />);
    fireEvent.click(screen.getByRole('button', { name: /Accessibility ▸ Display/i }));
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'accessibility',
      controlId: 'toggle-dyslexic-support',
    });
  });
});
