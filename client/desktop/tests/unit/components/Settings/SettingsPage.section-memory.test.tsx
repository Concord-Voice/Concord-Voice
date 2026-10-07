import { render, screen, fireEvent, act, waitFor, within } from '../../../test-utils';
import { vi } from 'vitest';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockUser } from '../../../mocks/fixtures';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { openProfilePage } from '@/renderer/utils/ui/openProfilePage';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ sessions: [], past_sessions: [] }),
  }),
  API_BASE: 'http://localhost:8080',
}));

vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: ({ size, inline }: { size?: string; inline?: boolean }) => (
    <div data-testid="loading-spinner" data-size={size} data-inline={inline}>
      Loading...
    </div>
  ),
}));

import SettingsPage from '@/renderer/components/Settings/SettingsPage';

// jsdom lacks scrollIntoView
beforeAll(() => {
  // eslint-disable-next-line @typescript-eslint/no-empty-function
  Element.prototype.scrollIntoView = vi.fn();
});

const storeOpen = (id: string) => useSettingsCollapsibleStore.getState().openSections[id];

function sectionEl(id: string): HTMLDetailsElement {
  const el = document.getElementById(id);
  if (!(el instanceof HTMLDetailsElement)) throw new Error(`#${id} is not a rendered <details>`);
  return el;
}

function paneSectionIds(): string[] {
  return Array.from(document.querySelectorAll('details.settings-collapsible')).map((el) => el.id);
}

// Toggle a section the way a user does: activate its <summary> (jsdom implements the
// activation behaviour, which flips `open` and queues the `toggle` event).
function clickSummary(id: string) {
  const summary = sectionEl(id).querySelector('summary');
  if (!summary) throw new Error(`#${id} has no <summary>`);
  fireEvent.click(summary);
}

function clickNavItem(name: string) {
  const nav = document.querySelector<HTMLElement>('.settings-nav');
  if (!nav) throw new Error('settings nav not rendered');
  fireEvent.click(within(nav).getByRole('button', { name }));
}

function openCollapsibleIds(): string[] {
  return Array.from(document.querySelectorAll('details.settings-collapsible[open]')).map(
    (el) => el.id || '(no id)'
  );
}

describe('SettingsPage section memory (#2365)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser });
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'concord',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
        customColors: null,
      },
    });
  });

  it('I1 default Appearance pane renders no open collapsible on first visit (#2365)', () => {
    render(<SettingsPage />);

    // Positive gate: the pane really rendered collapsibles, including the one the bug
    // leaves expanded, so an empty open-list below cannot mean "nothing rendered".
    expect(document.getElementById('client-behavior')).not.toBeNull();
    expect(document.querySelectorAll('details.settings-collapsible').length).toBeGreaterThan(0);

    const open = openCollapsibleIds();
    expect(
      open,
      `expected no Settings section to be open on first visit, but these are open: ${open.join(', ')}`
    ).toEqual([]);
  });

  it('I2 a section opened on one pane is still open after switching panes and back (#2365)', async () => {
    render(<SettingsPage />);

    // Gate: both sections are rendered and collapsed before the user touches them.
    expect(sectionEl('section-color-scheme').open).toBe(false);
    expect(sectionEl('section-theme').open).toBe(false);

    clickSummary('section-color-scheme');
    clickSummary('section-theme');
    await waitFor(() => {
      expect(storeOpen('section-color-scheme')).toBe(true);
      expect(storeOpen('section-theme')).toBe(true);
    });

    // Switch away: the Appearance sections unmount. Gate on an Account anchor.
    clickNavItem('Account');
    await waitFor(() => expect(document.getElementById('section-profile')).not.toBeNull());
    expect(document.getElementById('section-color-scheme')).toBeNull();

    // Switch back: gate on an Appearance anchor, then assert the remembered state.
    clickNavItem('Appearance');
    await waitFor(() => expect(document.getElementById('section-color-scheme')).not.toBeNull());
    expect(sectionEl('section-color-scheme')).toHaveAttribute('open');
    expect(sectionEl('section-theme')).toHaveAttribute('open');
    // Sections the user never opened stay collapsed.
    expect(sectionEl('client-behavior')).not.toHaveAttribute('open');
  });

  it('I3 section state survives closing and reopening Settings (#2365)', async () => {
    const first = render(<SettingsPage />);
    expect(sectionEl('section-theme').open).toBe(false);

    clickSummary('section-theme');
    await waitFor(() => expect(storeOpen('section-theme')).toBe(true));

    first.unmount();
    expect(document.getElementById('section-theme')).toBeNull();

    render(<SettingsPage />);
    expect(sectionEl('section-theme')).toHaveAttribute('open');
    expect(sectionEl('section-color-scheme')).not.toHaveAttribute('open');
  });

  it('I4 Expand all is remembered, then Collapse all is remembered (#2365)', async () => {
    const first = render(<SettingsPage />);
    const ids = paneSectionIds();
    // Gate: the pane really has several sections, all collapsed, and the label says so.
    expect(ids.length).toBeGreaterThan(1);
    expect(openCollapsibleIds()).toEqual([]);

    fireEvent.click(screen.getByRole('button', { name: 'Expand all' }));
    await waitFor(() => {
      for (const id of ids) expect(storeOpen(id)).toBe(true);
    });

    first.unmount();
    const second = render(<SettingsPage />);
    expect(paneSectionIds()).toEqual(ids);
    expect(openCollapsibleIds()).toHaveLength(ids.length);
    // The aggregate label is derived from the DOM by a MutationObserver after mount.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Collapse all' })).toBeVisible());

    fireEvent.click(screen.getByRole('button', { name: 'Collapse all' }));
    await waitFor(() => {
      for (const id of ids) expect(storeOpen(id)).toBe(false);
    });

    second.unmount();
    render(<SettingsPage />);
    expect(paneSectionIds()).toEqual(ids);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Expand all' })).toBeVisible());
    expect(openCollapsibleIds()).toEqual([]);
  });

  it('I5 a deep-link auto-open is remembered (#2365)', async () => {
    const first = render(<SettingsPage />);

    // Gate: on a plain Account visit the Profile section is collapsed.
    clickNavItem('Account');
    await waitFor(() => expect(document.getElementById('section-profile')).not.toBeNull());
    expect(sectionEl('section-profile').open).toBe(false);
    expect(sectionEl('section-password').open).toBe(false);

    // Leave the pane, then deep-link in: the focus-request effect switches pane, opens
    // the target through `details.open = true`, and focuses its summary.
    clickNavItem('Appearance');
    await waitFor(() => expect(document.getElementById('section-color-scheme')).not.toBeNull());
    act(() => openProfilePage());

    await waitFor(() => expect(document.getElementById('section-profile')).not.toBeNull());
    await waitFor(() => expect(sectionEl('section-profile')).toHaveAttribute('open'));
    await waitFor(() => expect(storeOpen('section-profile')).toBe(true));

    first.unmount();
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();

    // Fresh Settings visit: the nav click itself must open nothing — only the remembered
    // deep-link target is open.
    render(<SettingsPage />);
    clickNavItem('Account');
    await waitFor(() => expect(document.getElementById('section-profile')).not.toBeNull());
    expect(sectionEl('section-profile')).toHaveAttribute('open');
    expect(sectionEl('section-password')).not.toHaveAttribute('open');
    expect(sectionEl('section-nsfw-content')).not.toHaveAttribute('open');
  });

  it('I6 a sub-nav click auto-open is remembered (#2365)', async () => {
    const first = render(<SettingsPage />);
    expect(sectionEl('section-theme').open).toBe(false);

    // Sub-nav item for a collapsed section on the ACTIVE pane: scrollToSection runs
    // `el.open = true` and then scrolls.
    clickNavItem('Theme');
    await waitFor(() => expect(sectionEl('section-theme')).toHaveAttribute('open'));
    await waitFor(() => expect(storeOpen('section-theme')).toBe(true));
    expect(Element.prototype.scrollIntoView).toHaveBeenCalled();

    first.unmount();
    render(<SettingsPage />);
    expect(sectionEl('section-theme')).toHaveAttribute('open');
    expect(sectionEl('section-color-scheme')).not.toHaveAttribute('open');
  });
});
