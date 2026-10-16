import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '../../../test-utils';
import { deferred } from '../../../helpers/deferred';
import { settled } from '../../../helpers/settled';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockUser } from '../../../mocks/fixtures';
import { server } from '../../../mocks/server';
import SettingsPage from '@/renderer/components/Settings/SettingsPage';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';

const HISTORY_ENDPOINT = 'http://localhost:8080/api/v1/users/me/presence-history';
const optimisticCapabilities = [
  ['loading', { status: 'loading' } as const],
  ['unknown', { status: 'error', lastConfirmedSupported: false } as const],
  ['supported', { status: 'supported' } as const],
] as const;

function navigation() {
  return within(screen.getByRole('navigation', { name: 'Settings' }));
}

function clickNavigation(label: string) {
  fireEvent.click(navigation().getByRole('button', { name: label, exact: true }));
}

function serveHistory({
  missing = false,
  missingFeed = false,
  failCardSettings = false,
  hold = Promise.resolve(),
} = {}) {
  const settingsEntered = deferred();
  const feedEntered = deferred();
  let settingsReads = 0;
  const responses: number[] = [];
  const onResponse = ({ request, response }: { request: Request; response: Response }) => {
    if (request.url.startsWith(HISTORY_ENDPOINT)) responses.push(response.status);
  };
  server.events.on('response:mocked', onResponse);
  server.use(
    http.get(`${HISTORY_ENDPOINT}/settings`, async () => {
      const settingsRead = ++settingsReads;
      settingsEntered.resolve();
      await hold;
      // The card starts the first settings read, before the feed's own read.
      // The feed-only test pins the resulting card error and successful feed.
      if (failCardSettings && settingsRead === 1) {
        return HttpResponse.json({ error: 'Temporarily unavailable' }, { status: 503 });
      }
      return missing
        ? HttpResponse.json({ error: 'Not found' }, { status: 404 })
        : HttpResponse.json({
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
          });
    }),
    http.get(HISTORY_ENDPOINT, async () => {
      feedEntered.resolve();
      await hold;
      return missing || missingFeed
        ? HttpResponse.json({ error: 'Not found' }, { status: missingFeed ? 404 : 405 })
        : HttpResponse.json({
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
          });
    })
  );
  return {
    entered: Promise.all([settingsEntered.promise, feedEntered.promise]),
    responses,
    dispose: () => server.events.removeListener('response:mocked', onResponse),
  };
}

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
beforeEach(() => {
  clientConfigService.stop();
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useAuthStore.setState({ sessionId: 'mock-session' });
  useUserStore.setState({ user: mockUser });
  Element.prototype.scrollIntoView = vi.fn();
  server.use(
    http.get('http://localhost:8080/api/v1/users/me/privacy', () =>
      HttpResponse.json({ privacy: {} })
    ),
    http.get('http://localhost:8080/api/v1/users/me/presence-settings', () =>
      HttpResponse.json({
        master_enabled: true,
        server_voice_tier: 1,
        server_voice_show_details: true,
        private_call_tier: 0,
        private_call_show_details: false,
        custom_text_tier: 0,
        custom_text: null,
        custom_text_emoji: null,
      })
    ),
    http.post('http://localhost:8080/api/v1/klipy/customer-id', () =>
      HttpResponse.json({ customer_id: 'mock-klipy-customer-id' })
    ),
    http.get('http://localhost:8080/api/v1/friends', () => HttpResponse.json({ friends: [] }))
  );
});
afterEach(() => {
  cleanup();
  clientConfigService.stop();
  server.resetHandlers();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('Settings Activity History destination availability', () => {
  it('does not offer an unprobed unsupported destination from initial Appearance', () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    render(<SettingsPage />);

    expect(navigation().getByRole('button', { name: 'Appearance', exact: true })).toHaveAttribute(
      'aria-current',
      'page'
    );
    expect(navigation().queryByRole('button', { name: 'Activity History' })).toBeNull();
    expect(navigation().getByRole('button', { name: 'Privacy Controls' })).toBeVisible();
    expect(navigation().getByRole('button', { name: 'Rich Presence' })).toBeVisible();
  });

  it.each(optimisticCapabilities)(
    'keeps the %s destination reachable while availability remains unconfirmed or supported',
    async (_label, capability) => {
      useClientConfigStore.getState().setActivityHistoryCapability(capability);
      const read = deferred();
      const history = serveHistory({ hold: read.promise });
      const page = render(<SettingsPage />);
      try {
        clickNavigation('Activity History');
        await waitFor(() =>
          expect(document.querySelector('#section-presence-history > summary')).toHaveFocus()
        );
        expect(document.getElementById('section-presence-history')).toHaveAttribute('open');
        expect(navigation().getByRole('button', { name: 'Activity History' })).toHaveAttribute(
          'aria-current',
          'location'
        );
        expect(useSettingsNavStore.getState().focusRequest).toBeNull();
      } finally {
        page.unmount();
        read.resolve();
        history.dispose();
      }
    }
  );

  it.each(optimisticCapabilities)(
    'removes an optimistic %s child when Appearance learns activation is unsupported',
    (_label, capability) => {
      useClientConfigStore.getState().setActivityHistoryCapability(capability);
      render(<SettingsPage />);
      expect(navigation().getByRole('button', { name: 'Activity History' })).toBeVisible();

      act(() =>
        useClientConfigStore
          .getState()
          .setActivityHistoryCapability({ status: 'confirmed-unsupported' })
      );

      expect(navigation().getByRole('button', { name: 'Appearance', exact: true })).toHaveAttribute(
        'aria-current',
        'page'
      );
      expect(navigation().queryByRole('button', { name: 'Activity History' })).toBeNull();
    }
  );

  it.each(optimisticCapabilities)(
    'does not preserve a %s Privacy visit as proof of retained history after returning to Appearance',
    async (_label, capability) => {
      useClientConfigStore.getState().setActivityHistoryCapability(capability);
      const read = deferred();
      const history = serveHistory({ hold: read.promise });
      const page = render(<SettingsPage />);
      try {
        clickNavigation('Privacy');
        await waitFor(() =>
          expect(document.querySelector('#section-presence-history > summary')).toBeVisible()
        );
        clickNavigation('Appearance');
        await waitFor(() =>
          expect(document.querySelector('#section-color-scheme > summary')).toBeVisible()
        );

        act(() =>
          useClientConfigStore
            .getState()
            .setActivityHistoryCapability({ status: 'confirmed-unsupported' })
        );

        expect(navigation().queryByRole('button', { name: 'Activity History' })).toBeNull();
      } finally {
        page.unmount();
        read.resolve();
        history.dispose();
      }
    }
  );

  it('does not create a phantom child when both unsupported history probes finish unavailable', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    const history = serveHistory({ missing: true });
    const page = render(<SettingsPage />);
    try {
      act(() => useSettingsNavStore.getState().requestFocus('privacy', 'presence-history-heading'));
      await act(async () => settled(() => history.responses.length, 3));

      expect(history.responses).toContain(404);
      expect(history.responses).toContain(405);
      expect(document.querySelector('#section-privacy-settings > summary')).toBeVisible();
      expect(document.querySelector('#section-presence-history > summary')).not.toBeVisible();
      expect(navigation().queryByRole('button', { name: 'Activity History' })).toBeNull();
      expect(document.getElementById('presence-history-heading')).toBeNull();
    } finally {
      page.unmount();
      history.dispose();
    }
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it.each(['supported', 'confirmed-unsupported'] as const)(
    'offers confirmed retained history from Appearance after a positive %s probe',
    async (status) => {
      useClientConfigStore.getState().setActivityHistoryCapability({ status });
      const read = deferred();
      const history = serveHistory({ hold: read.promise });
      const page = render(<SettingsPage />);
      try {
        clickNavigation('Privacy');
        await act(async () => history.entered);
        if (status === 'confirmed-unsupported') {
          expect(navigation().queryByRole('button', { name: 'Activity History' })).toBeNull();
        }
        await act(async () => read.resolve());
        await waitFor(() =>
          expect(document.querySelector('#section-presence-history > summary')).toBeVisible()
        );
        clickNavigation('Appearance');
        await waitFor(() =>
          expect(document.querySelector('#section-color-scheme > summary')).toBeVisible()
        );
        act(() =>
          useClientConfigStore
            .getState()
            .setActivityHistoryCapability({ status: 'confirmed-unsupported' })
        );
        clickNavigation('Activity History');
        await waitFor(() =>
          expect(document.querySelector('#section-presence-history > summary')).toHaveFocus()
        );

        await waitFor(() => expect(screen.getByText('Retained during the drain')).toBeVisible());
        expect(document.getElementById('section-presence-history')).toHaveAttribute('open');
        expect(useClientConfigStore.getState().activityHistoryCapability.status).toBe(
          'confirmed-unsupported'
        );
        expect(useSettingsNavStore.getState().focusRequest).toBeNull();
      } finally {
        page.unmount();
        read.resolve();
        history.dispose();
      }
    }
  );

  it('opens and focuses card-only History controls when the feed route is unavailable', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    const history = serveHistory({ missingFeed: true });
    const page = render(<SettingsPage />);
    try {
      clickNavigation('Privacy');
      await act(async () => settled(() => history.responses.length, 3));
      expect(navigation().getByRole('button', { name: 'Activity History' })).toBeVisible();
      expect(document.getElementById('presence-history-heading')).toBeNull();
      clickNavigation('Appearance');
      await waitFor(() =>
        expect(document.querySelector('#section-color-scheme > summary')).toBeVisible()
      );

      clickNavigation('Activity History');

      await waitFor(() =>
        expect(document.querySelector('#section-presence-history > summary')).toHaveFocus()
      );
      expect(document.getElementById('section-presence-history')).toHaveAttribute('open');
      expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
      expect(document.getElementById('presence-history-heading')).toBeNull();
      expect(navigation().getByRole('button', { name: 'Activity History' })).toHaveAttribute(
        'aria-current',
        'location'
      );
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
    } finally {
      page.unmount();
      history.dispose();
    }
  });

  it('keeps feed-only History reachable when card settings and a later feed refresh fail', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    const history = serveHistory({ failCardSettings: true });
    const page = render(<SettingsPage />);
    try {
      clickNavigation('Privacy');
      await act(async () => settled(() => history.responses.length, 3));
      const summary = document.querySelector('#section-presence-history > summary');
      if (!summary) throw new Error('Activity History summary missing');
      fireEvent.click(summary);
      await waitFor(() => expect(screen.getByText('Retained during the drain')).toBeVisible());
      expect(screen.getByText('Activity History settings could not be refreshed.')).toBeVisible();
      expect(screen.getByRole('button', { name: 'Retry settings' })).toBeVisible();
      expect(navigation().getByRole('button', { name: 'Activity History' })).toBeVisible();
      fireEvent.click(summary);
      expect(document.getElementById('section-presence-history')).not.toHaveAttribute('open');
      navigation().getByRole('button', { name: 'Privacy', exact: true }).focus();

      clickNavigation('Activity History');

      await waitFor(() =>
        expect(document.querySelector('#section-presence-history > summary')).toHaveFocus()
      );
      expect(document.getElementById('section-presence-history')).toHaveAttribute('open');
      expect(screen.getByText('Retained during the drain')).toBeVisible();
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();

      server.use(
        http.get(`${HISTORY_ENDPOINT}/settings`, () =>
          HttpResponse.json({ error: 'Temporarily unavailable' }, { status: 503 })
        ),
        http.get(HISTORY_ENDPOINT, () =>
          HttpResponse.json({ error: 'Temporarily unavailable' }, { status: 503 })
        )
      );
      act(() =>
        useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' })
      );
      await act(async () => settled(() => history.responses.length, 6));
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Retry loading Activity History' })).toBeVisible()
      );
      act(() =>
        useClientConfigStore
          .getState()
          .setActivityHistoryCapability({ status: 'confirmed-unsupported' })
      );
      await act(async () => settled(() => history.responses.length, 9));
      expect(screen.getByRole('button', { name: 'Retry loading Activity History' })).toBeVisible();
      expect(screen.getByRole('button', { name: 'Retry settings' })).toBeVisible();
      expect(screen.getByText('Retained during the drain')).toBeVisible();
      expect(navigation().getByRole('button', { name: 'Activity History' })).toBeVisible();
      fireEvent.click(summary);
      expect(document.getElementById('section-presence-history')).not.toHaveAttribute('open');
      navigation().getByRole('button', { name: 'Privacy', exact: true }).focus();
      clickNavigation('Activity History');
      await waitFor(() =>
        expect(document.querySelector('#section-presence-history > summary')).toHaveFocus()
      );
      expect(document.getElementById('section-presence-history')).toHaveAttribute('open');
      expect(screen.getByText('Retained during the drain')).toBeVisible();
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
    } finally {
      page.unmount();
      history.dispose();
    }
  });

  it('opens and focuses a direct retained-history link when its probe outlasts the focus timer', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    const read = deferred();
    const history = serveHistory({ hold: read.promise });
    const page = render(<SettingsPage />);
    try {
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      act(() => useSettingsNavStore.getState().requestFocus('privacy', 'presence-history-heading'));
      await act(async () => history.entered);
      await act(async () => vi.advanceTimersByTimeAsync(60));
      expect(document.getElementById('presence-history-heading')).toBeNull();
      expect(navigation().queryByRole('button', { name: 'Activity History' })).toBeNull();
      expect(useSettingsNavStore.getState().focusRequest).toEqual({
        section: 'privacy',
        controlId: 'presence-history-heading',
      });
      vi.useRealTimers();

      await act(async () => read.resolve());
      await waitFor(() =>
        expect(document.getElementById('presence-history-heading')).toHaveFocus()
      );
      expect(document.getElementById('section-presence-history')).toHaveAttribute('open');
      expect(screen.getByText('Retained during the drain')).toBeVisible();
      expect(navigation().getByRole('button', { name: 'Activity History' })).toHaveAttribute(
        'aria-current',
        'location'
      );
      expect(useSettingsNavStore.getState().focusRequest).toBeNull();
    } finally {
      page.unmount();
      read.resolve();
      history.dispose();
      vi.useRealTimers();
    }
  });
});
