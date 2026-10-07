import React, { StrictMode } from 'react';
import { render as bareRender, act } from '@testing-library/react';
import { BrowserRouter } from 'react-router';
import { vi } from 'vitest';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { mockUser } from '../../../mocks/fixtures';
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

function providers(ui: React.ReactElement) {
  return (
    <BrowserRouter>
      <ModalProvider>{ui}</ModalProvider>
    </BrowserRouter>
  );
}

/**
 * `<StrictMode>` must be the TOPMOST element handed to a bare RTL render for React 19 to
 * replay effects. React's dev double-invoke pass handles the first placed fiber of a
 * commit as a unit and only replays it when StrictMode is that fiber or one of its
 * ancestors, so a StrictMode nested under `tests/test-utils`'s provider wrapper — or
 * under RTL's `wrapper` option — replays nothing while the test stays green. Measured
 * 2026-10-06 with a probe component: topmost StrictMode replayed its effects, nested
 * StrictMode did not. `main.tsx` has StrictMode at the root, so this is what the dev
 * renderer runs. (`MessageList.test.tsx`'s StrictMode case uses the same shape.)
 */
function strictProviders(ui: React.ReactElement) {
  return <StrictMode>{providers(ui)}</StrictMode>;
}

function sectionEl(id: string): HTMLDetailsElement {
  const el = document.getElementById(id);
  if (!(el instanceof HTMLDetailsElement)) throw new Error(`#${id} is not a rendered <details>`);
  return el;
}

// Let the mount's passive effects and any microtask they queued run, then fire the
// focus effect's deferred 50ms timer.
async function settleFocusRequest() {
  await act(async () => {});
  act(() => {
    vi.advanceTimersByTime(60);
  });
}

/**
 * Regression for #2365's visual pass: the user-popover "My Profile" link makes its
 * focus request BEFORE Settings mounts (`openProfilePage`). In the dev renderer, which
 * runs under `<React.StrictMode>`, the pane switched to Account but the Profile section
 * never opened — StrictMode simulates an unmount+remount on mount, and SettingsPage's
 * unmount-time "drop the pending request" cleanup ran inside that simulated unmount,
 * clearing the request before the focus effect could act on it. `defaultOpen` on
 * `section-profile` used to mask this; collapsed-by-default made it visible.
 */
describe('SettingsPage focus request under StrictMode (#2365)', () => {
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
    useSettingsNavStore.getState().clearFocusRequest();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it('control: without StrictMode a request made before mount opens and focuses its section', async () => {
    act(() => openProfilePage());
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-profile',
    });

    bareRender(providers(<SettingsPage />));
    await settleFocusRequest();

    const profile = sectionEl('section-profile');
    expect(profile.open).toBe(true);
    expect(profile.querySelector('summary')).toHaveFocus();
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it("a request made before mount survives StrictMode's simulated unmount and opens its section", async () => {
    act(() => openProfilePage());

    bareRender(strictProviders(<SettingsPage />));
    await settleFocusRequest();

    const profile = sectionEl('section-profile');
    expect(
      profile.open,
      'the deep-linked Profile section must be open after the focus request is handled'
    ).toBe(true);
    expect(profile.querySelector('summary')).toHaveFocus();
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it('a real unmount before the timer fires still drops the pending request under StrictMode', async () => {
    const { unmount } = bareRender(strictProviders(<SettingsPage />));
    await act(async () => {});

    act(() => {
      useSettingsNavStore.getState().requestFocus('accessibility', 'toggle-dyslexic-support');
    });
    // Positive gate: the request is still pending (the 50ms timer has not fired), so the
    // null below can only come from the unmount cleanup.
    expect(useSettingsNavStore.getState().focusRequest).not.toBeNull();
    // close Settings (unmount) BEFORE advancing the 50ms focus timer
    await act(async () => {
      unmount();
    });

    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
});
