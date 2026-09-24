import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen, fireEvent, userEvent, within } from '../../../test-utils';
import { vi } from 'vitest';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';

// Mirrors AccessibilitySection.test.tsx — mock the draft hook so the section renders
// in isolation. Variable is `mock`-prefixed so Vitest's hoisted factory may reference it.
const mockSetDraftAppearanceSetting = vi.fn();
let mockAppFont = 'default';
let mockColorScheme = 'concord';
let mockDyslexicSupport = false;
let mockFontSize = 'default';
let mockFontMode = 'one';
let mockFontHeadings = 'default';
let mockFontNavigation = 'default';
let mockFontMessages = 'default';

vi.mock('@/renderer/hooks/ui/useDraftSettings', () => ({
  useDraftAppearance: vi.fn(() => ({
    theme: 'dark',
    colorScheme: mockColorScheme,
    fontSize: mockFontSize,
    compactMode: false,
    reduceAnimations: false,
    uiScale: 1,
    highContrast: false,
    customColors: null,
    appFont: mockAppFont,
    dyslexicSupport: mockDyslexicSupport,
    gifPlayback: 'auto',
    fontMode: mockFontMode,
    fontHeadings: mockFontHeadings,
    fontNavigation: mockFontNavigation,
    fontMessages: mockFontMessages,
  })),
  setDraftAppearanceSetting: (...args: unknown[]) => mockSetDraftAppearanceSetting(...args),
}));

import FontSection from '@/renderer/components/Settings/FontSection';

beforeEach(() => {
  mockAppFont = 'default';
  mockColorScheme = 'concord';
  mockDyslexicSupport = false;
  mockFontSize = 'default';
  mockFontMode = 'one';
  mockFontHeadings = 'default';
  mockFontNavigation = 'default';
  mockFontMessages = 'default';
  mockSetDraftAppearanceSetting.mockClear();
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('FontSection (Appearance ▸ Fonts)', () => {
  it('renders the font options', () => {
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /Theme Default/i })).toBeInTheDocument();
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

  // Codec-style Preferred / In Use: the selection is what is SAVED, and a chip marks the
  // option actually applied while that selection is dynamic.
  it('Theme Default stays selected under Agency; the chip marks Atkinson as applied', () => {
    mockColorScheme = 'agency';
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /^Theme Default/ })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    const atkinson = screen.getByRole('button', { name: /Atkinson Hyperlegible Next/i });
    expect(atkinson).toHaveAttribute('aria-pressed', 'false');
    expect(atkinson).toHaveTextContent('Active with the current theme');
    expect(screen.getAllByText('Active with the current theme')).toHaveLength(1);
  });

  it('under a non-bundling theme the chip marks Concord Voice Default', () => {
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /^Concord Voice Default/ })).toHaveTextContent(
      'Active with the current theme'
    );
  });

  it('Concord Voice Default writes the base face and, once picked, needs no chip', () => {
    const { rerender } = render(<FontSection />);
    fireEvent.click(screen.getByRole('button', { name: /^Concord Voice Default/ }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('appFont', 'sourcesans');
    mockAppFont = 'sourcesans';
    mockColorScheme = 'agency';
    rerender(<FontSection />);
    expect(screen.getByRole('button', { name: /^Concord Voice Default/ })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.queryByText('Active with the current theme')).not.toBeInTheDocument();
  });

  it('"Theme Default" previews the font it applies: the bundled face under Agency', () => {
    const sample = () =>
      screen
        .getByRole('button', { name: /Theme Default/i })
        .querySelector<HTMLElement>('.font-option-sample')?.style.fontFamily;
    const { rerender } = render(<FontSection />);
    expect(sample()).toMatch(/SourceSans/);
    mockColorScheme = 'agency';
    rerender(<FontSection />);
    expect(sample()).toMatch(/Atkinson Hyperlegible Next/);
  });

  // The hint and the chip describe the option; its NAME stays the visible label alone, so
  // voice control and screen readers get "Concord Voice Default", not the chip text too.
  it('the three defaults carry a hint, and hint + chip are the description, not the name', () => {
    render(<FontSection />);
    const concord = screen.getByRole('button', { name: 'Concord Voice Default' });
    expect(concord).toHaveAccessibleDescription(
      "Concord's fonts on every theme Active with the current theme"
    );
    expect(screen.getByRole('button', { name: 'Theme Default' })).toHaveAccessibleDescription(
      'Follows your theme'
    );
    expect(screen.getByRole('button', { name: 'System Default' })).toHaveAccessibleDescription(
      "Your operating system's font"
    );
    expect(screen.getByRole('button', { name: 'Inter' })).not.toHaveAttribute('aria-describedby');
  });

  it('under the Dyslexic lock the description leads with the lock note', () => {
    mockDyslexicSupport = true;
    render(<FontSection />);
    const theme = screen.getByRole('button', { name: 'Theme Default' });
    expect(theme.getAttribute('aria-describedby')?.split(' ')).toHaveLength(2);
    expect(theme).toHaveAccessibleDescription(/^Font selection is managed by/);
    expect(theme).toHaveAccessibleDescription(/Follows your theme$/);
  });

  it('an explicit pick overrides the theme font, and the pick is what applies (no chip)', () => {
    mockColorScheme = 'agency';
    mockAppFont = 'inter';
    render(<FontSection />);
    expect(screen.getByRole('button', { name: /Inter/i })).toHaveAttribute('aria-pressed', 'true');
    const atkinson = screen.getByRole('button', { name: /Atkinson Hyperlegible Next/i });
    expect(atkinson).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByText('Active with the current theme')).not.toBeInTheDocument();
  });

  it('no chip while Dyslexic Support locks the picker', () => {
    mockDyslexicSupport = true;
    render(<FontSection />);
    expect(screen.queryByText('Active with the current theme')).not.toBeInTheDocument();
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

describe('FontSection — Font Size (moved from Accessibility, #2367)', () => {
  it('renders Font Size as a labelled radio group with the current size checked', () => {
    mockFontSize = 'large';
    render(<FontSection />);
    const group = screen.getByRole('group', { name: 'Font Size' });
    const radios = within(group).getAllByRole('radio');
    expect(radios.map((r) => r.getAttribute('value'))).toEqual(['small', 'default', 'large']);
    expect(within(group).getByRole('radio', { name: 'Large' })).toBeChecked();
    expect(within(group).getByRole('radio', { name: 'Small' })).not.toBeChecked();
  });

  it('writes the chosen size to the draft store', () => {
    render(<FontSection />);
    fireEvent.click(screen.getByRole('radio', { name: 'Small' }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('fontSize', 'small');
  });

  it('stays usable while Dyslexic Support locks the typeface', () => {
    // The lock is about WHICH face renders text; size is independent of face. The
    // typeface lock is enforced per option (aria-disabled + an activation guard), not by
    // a disabled fieldset, so this pins the observable property rather than placement:
    // gating the size radios on the lock — native `disabled`, `aria-disabled`, an
    // early-return guard, or a later `<fieldset disabled>` around both groups — fails here.
    mockDyslexicSupport = true;
    render(<FontSection />);

    // Precondition: the typeface options really are locked in this render.
    expect(screen.getByRole('button', { name: /Inter/i })).toHaveAttribute('aria-disabled', 'true');

    const small = screen.getByRole('radio', { name: 'Small' });
    expect(small).toBeEnabled();
    expect(small).not.toHaveAttribute('aria-disabled');
    fireEvent.click(small);
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('fontSize', 'small');
  });
});

describe('FontSection — One Font / Font by Area (#2366)', () => {
  const rows = (container: HTMLElement) => [
    ...container.querySelectorAll<HTMLDetailsElement>('details.font-area-row'),
  ];
  const row = (container: HTMLElement, name: string) => {
    const r = rows(container).find((d) => d.querySelector('.font-area-name')?.textContent === name);
    if (!r) throw new Error(`no ${name} row`);
    return r;
  };

  it('renders the mode as a radio pair with One Font checked, and no area rows', () => {
    const { container } = render(<FontSection />);
    expect(screen.getByRole('radio', { name: 'One Font' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Font by Area' })).not.toBeChecked();
    expect(rows(container)).toHaveLength(0);
    expect(screen.getByRole('button', { name: /Theme Default/i })).toBeInTheDocument();
  });

  // jsdom applies no stylesheet, so assert the hook the shared rule selects on. The
  // toggle once highlighted via its own `.active` class and went dark when #3428 moved
  // SettingsPage.css to `:has(.settings-mode-radio:checked)`; both PRs were green.
  it('the mode radios carry the class the shared checked-pill rule selects', () => {
    const css = readFileSync(
      resolve(__dirname, '../../../../src/renderer/components/Settings/SettingsPage.css'),
      'utf8'
    );
    const hook = /\.settings-mode-pill:has\(\.([\w-]+):checked\)/.exec(css)?.[1];
    expect(hook).toBeDefined();
    render(<FontSection />);
    for (const name of ['One Font', 'Font by Area']) {
      expect(screen.getByRole('radio', { name })).toHaveClass(hook as string);
    }
  });

  it('choosing Font by Area writes fontMode', () => {
    render(<FontSection />);
    fireEvent.click(screen.getByRole('radio', { name: 'Font by Area' }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('fontMode', 'area');
  });

  it('Font by Area renders four rows in one exclusive group, each showing its current font', () => {
    mockFontMode = 'area';
    mockFontHeadings = 'lexend';
    const { container } = render(<FontSection />);
    expect(rows(container).map((d) => d.querySelector('.font-area-name')?.textContent)).toEqual([
      'Messages',
      'Headings',
      'Navigation',
      'Interface',
    ]);
    expect(rows(container).every((d) => d.getAttribute('name') === 'font-areas')).toBe(true);
    expect(
      within(row(container, 'Headings')).getByText('Lexend', { selector: '.font-area-current' })
    ).toBeInTheDocument();
    expect(
      within(row(container, 'Messages')).getByText('Match Interface', {
        selector: '.font-area-current',
      })
    ).toBeInTheDocument();
  });

  it('picking a font inside an area row writes that area key', () => {
    mockFontMode = 'area';
    const { container } = render(<FontSection />);
    fireEvent.click(within(row(container, 'Navigation')).getByRole('button', { name: /^Inter/ }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('fontNavigation', 'inter');
  });

  it('the Interface row writes appFont and area lists offer Source Sans, One Font does not', () => {
    mockFontMode = 'area';
    const { container } = render(<FontSection />);
    fireEvent.click(within(row(container, 'Interface')).getByRole('button', { name: /^Inter/ }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('appFont', 'inter');
    expect(
      within(row(container, 'Headings')).getByRole('button', { name: /^Source Sans/ })
    ).toBeInTheDocument();
    expect(
      within(row(container, 'Interface')).queryByRole('button', { name: /^Source Sans/ })
    ).toBeNull();
  });

  it('every area defaults to "Match Interface"; only Interface carries the In Use chip', () => {
    mockFontMode = 'area';
    mockColorScheme = 'agency';
    const { container } = render(<FontSection />);
    for (const name of ['Messages', 'Headings', 'Navigation']) {
      expect(
        within(row(container, name)).getByText('Match Interface', {
          selector: '.font-area-current',
        })
      ).toBeInTheDocument();
    }
    expect(
      within(row(container, 'Interface')).getByRole('button', { name: /^Theme Default/ })
    ).toBeInTheDocument();
    const badge = (name: string) =>
      within(row(container, name)).queryAllByText('Active with the current theme');
    expect(badge('Headings')).toHaveLength(0);
    expect(badge('Messages')).toHaveLength(0);
    expect(badge('Navigation')).toHaveLength(0);
    expect(badge('Interface')).toHaveLength(1);
  });

  it("Headings' Match Interface previews the Interface pick, else the theme's heading face", () => {
    mockFontMode = 'area';
    const { container, rerender } = render(<FontSection />);
    const family = () =>
      (row(container, 'Headings').querySelector('.font-area-current') as HTMLElement).style
        .fontFamily;
    expect(family()).toBe('var(--font-brand-stack)');
    mockAppFont = 'lexend';
    rerender(<FontSection />);
    expect(family()).toMatch(/Lexend/);
    // Concord Voice Default pins the brand heading face, not its body face.
    mockAppFont = 'sourcesans';
    rerender(<FontSection />);
    expect(family()).toBe('var(--font-stack-concord)');
    // A bundling theme with no pick: the theme lock keeps headings on the theme's face.
    mockColorScheme = 'agency';
    mockAppFont = 'default';
    rerender(<FontSection />);
    expect(family()).toBe('var(--font-brand-stack)');
    // An explicit pick on the same theme lifts the lock and carries into headings.
    mockAppFont = 'inter';
    rerender(<FontSection />);
    expect(family()).toMatch(/Inter/);
  });

  it('the Interface row reads the saved choice, previewed in the font it applies', () => {
    mockFontMode = 'area';
    mockColorScheme = 'agency';
    const { container, rerender } = render(<FontSection />);
    const current = () =>
      row(container, 'Interface').querySelector('.font-area-current') as HTMLElement;
    expect(current()).toHaveTextContent('Theme Default');
    expect(current().style.fontFamily).toMatch(/Atkinson Hyperlegible Next/);
    mockAppFont = 'sourcesans';
    rerender(<FontSection />);
    expect(current()).toHaveTextContent('Concord Voice Default');
    expect(current().style.fontFamily).toMatch(/SourceSans/);
  });

  it('the saved-choices notice appears only in One Font with a non-default area pick', () => {
    const { rerender } = render(<FontSection />);
    // A native <output>: its implicit role is status, so it is a polite live region.
    const status = screen.getByRole('status');
    expect(status.tagName).toBe('OUTPUT');
    expect(status).toBeEmptyDOMElement();
    mockFontMessages = 'lato';
    rerender(<FontSection />);
    expect(status).toHaveTextContent('Your per-area choices are saved');
    mockFontMode = 'area';
    rerender(<FontSection />);
    expect(status).toBeEmptyDOMElement();
  });

  it('Dyslexic Support locks the mode radios and every area option without writing', () => {
    mockDyslexicSupport = true;
    mockFontMode = 'area';
    const { container } = render(<FontSection />);
    const oneFont = screen.getByRole('radio', { name: 'One Font' });
    expect(oneFont).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(oneFont);
    fireEvent.click(within(row(container, 'Headings')).getByRole('button', { name: /^Inter/ }));
    expect(mockSetDraftAppearanceSetting).not.toHaveBeenCalled();
    for (const d of rows(container)) {
      expect(d.querySelector('.font-area-current')).toHaveTextContent('OpenDyslexic · Locked');
    }
  });
});
