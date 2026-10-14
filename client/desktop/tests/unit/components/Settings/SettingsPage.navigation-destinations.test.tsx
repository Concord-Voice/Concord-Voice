import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '../../../test-utils';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { openSubscriptionPage } from '@/renderer/utils/ui/openSubscriptionPage';
import { openVerificationSetup } from '@/renderer/utils/ui/openVerificationSetup';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockUser } from '../../../mocks/fixtures';
import { deferred } from '../../../helpers/deferred';
import { apiFetch } from '@/renderer/services/system/apiClient';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ sessions: [], past_sessions: [], enabled: false }),
  }),
  API_BASE: 'http://localhost:8080',
}));

// Navigation owns the destination and return path; MFA enrollment has its own
// security suites. This fixture supplies the completion event that enables the
// real Security pane's return button, rather than inferring completion from status.
vi.mock('@/renderer/components/Settings/MFASetup', () => ({
  default: ({ onComplete }: { onComplete: () => void }) => (
    <button type="button" onClick={onComplete}>
      Finish verification setup
    </button>
  ),
}));

import SettingsPage from '@/renderer/components/Settings/SettingsPage';

const destinations = [
  ['Appearance', 'Color Scheme', 'section-color-scheme'],
  ['Appearance', 'Theme', 'section-theme'],
  ['Appearance', 'Application Font', 'section-fonts'],
  ['Appearance', 'Layout', 'section-layout'],
  ['Appearance', 'Client Behavior', 'client-behavior'],
  ['Privacy', 'Privacy Controls', 'section-privacy-settings'],
  ['Privacy', 'Rich Presence', 'section-presence-settings'],
  ['Privacy', 'Activity History', 'section-presence-history'],
  ['Account', 'Profile Information', 'section-profile'],
  ['Account', 'Subscription', 'section-subscription'],
  ['Account', 'NSFW Content Access', 'section-nsfw-content'],
  ['Security', 'Change Password', 'section-password'],
  ['Security', 'Multi-Factor Authentication', 'section-mfa'],
  ['Security', 'SSO Security', 'section-sso-security'],
  ['Security', 'Active Sessions', 'section-active-sessions'],
  ['Security', 'Past Sessions', 'section-past-sessions'],
  ['Notifications', 'Desktop Notifications', 'section-desktop-notifications'],
  ['Notifications', 'Sounds', 'section-notification-sounds'],
  ['Notifications', 'Quiet Hours', 'section-quiet-hours'],
  ['Audio & Video', 'Audio Configuration', 'section-audio-config'],
  ['Audio & Video', 'Video Configuration', 'section-video-screen'],
  ['Accessibility', 'Display', 'section-display'],
  ['Accessibility', 'Text-to-Speech', 'section-tts'],
  ['About & Updates', 'Client Info', 'section-client-info'],
  ['About & Updates', 'Update Settings', 'section-update-settings'],
  ['About & Updates', 'System Permissions', 'section-system-permissions'],
  ['About & Updates', 'Developer', 'section-developer'],
] as const;

function navigation() {
  return screen.getByRole('navigation');
}

function clickNavigation(label: string) {
  fireEvent.click(within(navigation()).getByRole('button', { name: label, exact: true }));
}

function disclosure(id: string): HTMLDetailsElement {
  const node = document.getElementById(id);
  if (!(node instanceof HTMLDetailsElement)) throw new Error(`#${id} is not a mounted disclosure`);
  return node;
}

function SettingsSurface() {
  const open = useSettingsOverlayStore((state) => state.open);
  return open === 'app' ? <SettingsPage /> : null;
}

function serveRetainedActivityHistory(waitForRead?: Promise<void>) {
  const settings = {
    available: true,
    enabled: true,
    reconsent_required: false,
    retention_days: 30,
    consent_version: 1,
    consent_copy_hash: 'a'.repeat(64),
    consented_at: '2026-10-13T12:00:00Z',
    required_consent: {
      version: 1,
      copy_hash: 'a'.repeat(64),
      operator_name: 'Concord Voice, Inc.',
      required_text: 'Activity History stores future Custom Status changes on this server.',
      details: ['History starts with your next Custom Status change.'],
      privacy_policy_url: 'https://concordvoice.com/privacy-policy',
      acknowledgement_label: 'I consent to server-readable Activity History.',
    },
  };
  const feed = {
    items: [
      {
        status: 'supported',
        id: '11111111-1111-4111-8111-111111111111',
        category: 'custom_text',
        payload_version: 1,
        payload: { text: 'Retained during the drain', emoji: '🔍' },
        started_at: '2026-10-13T12:00:00Z',
        ended_at: '2026-10-13T13:00:00Z',
        recorded_at: '2026-10-13T12:00:00Z',
        expires_at: '2026-11-12T12:00:00Z',
      },
    ],
    next_cursor: null,
  };
  vi.mocked(apiFetch).mockImplementation(async (path) => {
    if (String(path).startsWith('/api/v1/users/me/presence-history') && waitForRead) {
      await waitForRead;
    }
    const body =
      String(path) === '/api/v1/users/me/presence-history/settings'
        ? settings
        : String(path).startsWith('/api/v1/users/me/presence-history')
          ? feed
          : { sessions: [], past_sessions: [], methods: [], credentials: [] };
    return new Response(JSON.stringify(body));
  });
}

async function revealRetainedActivityHistory() {
  clickNavigation('Privacy');
  await waitFor(() =>
    expect(disclosure('section-presence-history').querySelector('summary')).toBeVisible()
  );
  const summary = disclosure('section-presence-history').querySelector('summary');
  if (!summary) throw new Error('Activity History summary missing');
  fireEvent.click(summary);
  await waitFor(() => expect(screen.getByText('Retained during the drain')).toBeVisible());
  expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
}

describe('Settings navigation reaches its real subsection destinations', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.setState({ sessionId: 'mock-session' });
    useUserStore.setState({ user: mockUser });
    useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' });
    Element.prototype.scrollIntoView = vi.fn();
    vi.mocked(apiFetch)
      .mockReset()
      .mockResolvedValue({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], methods: [], credentials: [] }),
      } as Response);
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: {
        enumerateDevices: vi.fn().mockResolvedValue([]),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      },
    });
  });

  it('places the eight parent destinations and their children in an accessible hierarchy', () => {
    render(<SettingsPage />);
    const nav = navigation();
    const parents = [...new Set(destinations.map(([parent]) => parent))];

    for (const parent of parents) {
      const button = within(nav).getByRole('button', { name: parent, exact: true });
      const parentItem = button.closest('li');
      expect(parentItem, `${parent} must belong to a native list item`).not.toBeNull();
      for (const [, child] of destinations.filter(([pane]) => pane === parent)) {
        const childButton = within(parentItem as HTMLLIElement).getByRole('button', {
          name: child,
          exact: true,
        });
        expect(childButton.closest('ul')?.closest('li')).toBe(parentItem);
      }
    }

    expect(within(nav).queryByRole('button', { name: 'Subscriptions', exact: true })).toBeNull();
    expect(
      within(nav).queryByRole('button', { name: 'Privacy & Security', exact: true })
    ).toBeNull();
    expect(nav).toHaveAccessibleName('Settings');
  });

  it.each(destinations)(
    '%s → %s opens, scrolls, and focuses %s from another pane',
    async (parent, child, id) => {
      render(<SettingsPage />);
      // Every destination is exercised from another pane; Appearance starts active.
      if (parent === 'Appearance') {
        clickNavigation('Account');
        await waitFor(() => expect(document.getElementById('section-profile')).not.toBeNull());
      }
      const childButton = within(navigation()).getByRole('button', { name: child, exact: true });
      expect(childButton).toBeVisible();
      fireEvent.click(childButton);

      await waitFor(() => expect(disclosure(id)).toHaveAttribute('open'));
      const focusTarget = () => disclosure(id).querySelector(':scope > summary');
      await waitFor(() => expect(focusTarget()).toHaveFocus());
      expect(Element.prototype.scrollIntoView).toHaveBeenCalled();
      expect(
        within(navigation()).getByRole('button', { name: parent, exact: true })
      ).toHaveAttribute('aria-current', 'page');
      expect(
        within(navigation()).getByRole('button', { name: child, exact: true })
      ).toHaveAttribute('aria-current', 'location');
      expect(document.querySelectorAll('details.settings-collapsible[open]')).toHaveLength(1);
    }
  );

  it('hides the Activity History child and content after both routes are confirmed unavailable', async () => {
    vi.mocked(apiFetch).mockImplementation(
      async (path) =>
        new Response(
          JSON.stringify({ sessions: [], past_sessions: [], methods: [], credentials: [] }),
          { status: String(path).includes('/presence-history') ? 404 : 200 }
        )
    );
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    render(<SettingsPage />);
    expect(within(navigation()).getByRole('button', { name: 'Privacy Controls' })).toBeVisible();
    await act(async () => clickNavigation('Privacy'));
    expect(disclosure('section-privacy-settings').querySelector('summary')).toBeVisible();
    // Keep unavailable history mounted for ongoing cleanup, but hidden from users.
    expect(disclosure('section-presence-history')).toBeInTheDocument();
    // A closed details body is already hidden; its summary must also be hidden to
    // prove this checks availability rather than ordinary disclosure collapse.
    expect(disclosure('section-presence-history').querySelector('summary')).not.toBeVisible();
    expect(useClientConfigStore.getState().activityHistoryCapability.status).toBe(
      'confirmed-unsupported'
    );
    expect(within(navigation()).queryByRole('button', { name: 'Activity History' })).toBeNull();
  });

  it('keeps retained Activity History reachable from its sidebar and existing View history deep-link during a drain', async () => {
    serveRetainedActivityHistory();
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    render(<SettingsPage />);
    await revealRetainedActivityHistory();

    expect(within(navigation()).getByRole('button', { name: 'Activity History' })).toBeVisible();
    clickNavigation('Appearance');
    await waitFor(() => expect(document.getElementById('section-color-scheme')).not.toBeNull());
    clickNavigation('Activity History');
    await waitFor(() =>
      expect(disclosure('section-presence-history').querySelector(':scope > summary')).toHaveFocus()
    );
    await waitFor(() => expect(screen.getByText('Retained during the drain')).toBeVisible());
    expect(within(navigation()).getByRole('button', { name: 'Activity History' })).toHaveAttribute(
      'aria-current',
      'location'
    );

    // The card's existing View history action uses the same real focus request.
    const viewHistory = screen.getByRole('button', { name: 'View history' });
    viewHistory.focus();
    fireEvent.click(viewHistory);
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'presence-history-heading',
    });
    await waitFor(() => expect(document.getElementById('presence-history-heading')).toHaveFocus());
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
    expect(vi.mocked(Element.prototype.scrollIntoView).mock.contexts).toContain(
      document.getElementById('presence-history-heading')
    );
  });

  it('includes visible retained Activity History in the real scroll observer during a drain', async () => {
    serveRetainedActivityHistory();
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    const observations: Array<{
      observer: IntersectionObserver;
      callback: IntersectionObserverCallback;
      targets: Set<Element>;
    }> = [];
    class ObservedSettings implements IntersectionObserver {
      readonly root = null;
      readonly rootMargin = '';
      readonly thresholds: readonly number[] = [];
      readonly targets = new Set<Element>();
      constructor(callback: IntersectionObserverCallback) {
        observations.push({ observer: this, callback, targets: this.targets });
      }
      observe(target: Element) {
        this.targets.add(target);
      }
      unobserve(target: Element) {
        this.targets.delete(target);
      }
      disconnect() {
        this.targets.clear();
      }
      takeRecords(): IntersectionObserverEntry[] {
        return [];
      }
    }
    const originalObserver = globalThis.IntersectionObserver;
    globalThis.IntersectionObserver = ObservedSettings;
    try {
      render(<SettingsPage />);
      await revealRetainedActivityHistory();
      const history = disclosure('section-presence-history');
      await waitFor(() =>
        expect(observations.some(({ targets }) => targets.has(history))).toBe(true)
      );
      const watching = observations.find(({ targets }) => targets.has(history));
      if (!watching) throw new Error('Visible Activity History has no live scroll observer');
      const visible = (target: Element): IntersectionObserverEntry => ({
        target,
        isIntersecting: true,
        boundingClientRect: new DOMRect(0, 20, 500, 100),
        intersectionRect: new DOMRect(0, 20, 500, 100),
        intersectionRatio: 1,
        rootBounds: null,
        time: 0,
      });
      act(() =>
        watching.callback([visible(disclosure('section-privacy-settings'))], watching.observer)
      );
      expect(
        within(navigation()).getByRole('button', { name: 'Privacy Controls' })
      ).toHaveAttribute('aria-current', 'location');
      act(() =>
        watching.callback(
          [
            { ...visible(disclosure('section-privacy-settings')), isIntersecting: false },
            visible(history),
          ],
          watching.observer
        )
      );
      expect(
        within(navigation()).getByRole('button', { name: 'Activity History' })
      ).toHaveAttribute('aria-current', 'location');
      expect(screen.getByText('Retained during the drain')).toBeVisible();
    } finally {
      globalThis.IntersectionObserver = originalObserver;
    }
  });

  it('opens and focuses a retained-history direct destination after its drain probe exceeds the focus delay', async () => {
    const read = deferred<void>();
    serveRetainedActivityHistory(read.promise);
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      render(<SettingsSurface />);
      await act(async () => useSettingsOverlayStore.getState().openSettings('app'));
      await act(async () =>
        useSettingsNavStore.getState().requestFocus('privacy', 'presence-history-heading')
      );
      expect(disclosure('section-privacy-settings').querySelector('summary')).toBeInTheDocument();
      await act(async () => vi.advanceTimersByTime(60));
      expect(document.getElementById('presence-history-heading')).toBeNull();

      await act(async () => read.resolve());
      await act(async () => vi.advanceTimersByTime(60));
      const heading = document.getElementById('presence-history-heading');
      expect(heading).toBeInTheDocument();
      expect(disclosure('section-presence-history')).toHaveAttribute('open');
      expect(heading).toBeVisible();
      expect(heading).toHaveFocus();
      expect(screen.getByText('Retained during the drain')).toBeVisible();
      expect(
        within(navigation()).getByRole('button', { name: 'Activity History' })
      ).toHaveAttribute('aria-current', 'location');
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
    } finally {
      read.resolve();
      vi.clearAllTimers();
      vi.useRealTimers();
    }
  });

  it('keeps a newer main-pane choice after an abandoned retained-history probe completes', async () => {
    const read = deferred<void>();
    serveRetainedActivityHistory(read.promise);
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      render(<SettingsSurface />);
      await act(async () => useSettingsOverlayStore.getState().openSettings('app'));
      await act(async () =>
        useSettingsNavStore.getState().requestFocus('privacy', 'presence-history-heading')
      );
      expect(disclosure('section-privacy-settings')).toBeInTheDocument();
      await act(async () => vi.advanceTimersByTime(60));
      await act(async () => clickNavigation('Appearance'));
      expect(disclosure('section-color-scheme').querySelector('summary')).toBeInTheDocument();

      await act(async () => read.resolve());
      await act(async () => vi.advanceTimersByTime(60));
      expect(
        within(navigation()).getByRole('button', { name: 'Appearance', exact: true })
      ).toHaveAttribute('aria-current', 'page');
      expect(disclosure('section-color-scheme').querySelector('summary')).toBeVisible();
      expect(document.getElementById('presence-history-heading')).toBeNull();
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
    } finally {
      read.resolve();
      vi.clearAllTimers();
      vi.useRealTimers();
    }
  });

  it('abandons the pending retained-history destination when Settings actually closes', async () => {
    const read = deferred<void>();
    serveRetainedActivityHistory(read.promise);
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      render(<SettingsSurface />);
      await act(async () => useSettingsOverlayStore.getState().openSettings('app'));
      await act(async () =>
        useSettingsNavStore.getState().requestFocus('privacy', 'presence-history-heading')
      );
      expect(disclosure('section-privacy-settings')).toBeInTheDocument();
      expect(useSettingsNavStore.getState().focusRequest).toEqual({
        section: 'privacy',
        controlId: 'presence-history-heading',
      });
      await act(async () => useSettingsOverlayStore.getState().close());
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
      expect(useSettingsCollapsibleStore.getState().visitActive).toBe(false);

      await act(async () => read.resolve());
      await act(async () => vi.advanceTimersByTime(60));
      expect(useSettingsOverlayStore.getState().open).toBeNull();
      expect(screen.queryByRole('navigation', { name: 'Settings' })).toBeNull();
      expect(document.getElementById('presence-history-heading')).toBeNull();
    } finally {
      read.resolve();
      vi.clearAllTimers();
      vi.useRealTimers();
    }
  });

  it('keeps unknown-capability Activity History reachable without claiming it is unavailable', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'error', lastConfirmedSupported: false });
    render(<SettingsPage />);
    clickNavigation('Activity History');
    await waitFor(() =>
      expect(disclosure('section-presence-history').querySelector(':scope > summary')).toHaveFocus()
    );
    expect(disclosure('section-presence-history')).toHaveAttribute('open');
  });

  it('routes an existing subscription lock to Account and the compact Subscription disclosure', async () => {
    render(<SettingsSurface />);
    act(() => openSubscriptionPage('upload-size'));

    await waitFor(() => expect(disclosure('section-subscription')).toHaveAttribute('open'));
    await waitFor(() =>
      expect(disclosure('section-subscription').querySelector('summary')).toHaveFocus()
    );
    expect(
      within(navigation()).getByRole('button', { name: 'Account', exact: true })
    ).toHaveAttribute('aria-current', 'page');
    expect(document.getElementById('section-profile')).not.toHaveAttribute('open');
  });

  it('routes verification setup to Security and preserves its return to the originating chat', async () => {
    render(<SettingsSurface />);
    await act(async () => {
      expect(await openVerificationSetup({ returnTo: { kind: 'chat' } })).toBe(true);
    });

    await waitFor(() => expect(disclosure('section-mfa')).toHaveAttribute('open'));
    await waitFor(() => expect(disclosure('section-mfa').querySelector('summary')).toHaveFocus());
    expect(
      within(navigation()).getByRole('button', { name: 'Security', exact: true })
    ).toHaveAttribute('aria-current', 'page');
    const standardTier = await screen.findByText('Standard — Authenticator App');
    const tier = standardTier.closest('.mfa-tier-card');
    if (!tier) throw new Error('Standard MFA setup tier missing');
    fireEvent.click(within(tier as HTMLElement).getByRole('button', { name: 'Set Up' }));
    const completedSetup = await screen.findByRole('button', { name: 'Finish verification setup' });
    vi.mocked(apiFetch).mockImplementation(
      async () =>
        new Response(
          JSON.stringify({ sessions: [], past_sessions: [], methods: ['totp'], credentials: [] })
        )
    );
    fireEvent.click(completedSetup);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Back to chat' })).toBeVisible());
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(document.getElementById('section-privacy-settings')).toBeNull();
  });

  it('purges expired expansion before a deliberate subscription target opens on return', async () => {
    const clock = vi.spyOn(Date, 'now').mockReturnValue(new Date('2026-10-13T12:00:00Z').getTime());
    try {
      render(<SettingsSurface />);
      act(() => useSettingsOverlayStore.getState().openSettings('app'));
      clickNavigation('Theme');
      await waitFor(() => expect(disclosure('section-theme')).toHaveAttribute('open'));
      await waitFor(() =>
        expect(useSettingsCollapsibleStore.getState().openSections['section-theme']).toBe(true)
      );
      act(() => useSettingsOverlayStore.getState().close());
      expect(screen.queryByRole('navigation')).toBeNull();

      clock.mockReturnValue(new Date('2026-10-13T12:30:00Z').getTime());
      act(() => openSubscriptionPage());
      await waitFor(() =>
        expect(disclosure('section-subscription').querySelector('summary')).toHaveFocus()
      );
      expect(disclosure('section-subscription')).toHaveAttribute('open');
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
      expect(useSettingsCollapsibleStore.getState().openSections['section-theme']).not.toBe(true);
      expect(document.querySelectorAll('details.settings-collapsible[open]')).toHaveLength(1);
    } finally {
      clock.mockRestore();
    }
  });

  it('uses immediate subsection scrolling when the user has reduced animations', async () => {
    useSettingsStore.getState().setReduceAnimations(true);
    render(<SettingsPage />);
    clickNavigation('Client Behavior');
    await waitFor(() =>
      expect(disclosure('client-behavior').querySelector('summary')).toHaveFocus()
    );

    expect(Element.prototype.scrollIntoView).toHaveBeenCalledWith(
      expect.objectContaining({ behavior: 'auto' })
    );
  });
});
