import { act, fireEvent, render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { resetAllStores } from '../../../helpers/store-helpers';
import { server } from '../../../mocks/server';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';
import PresenceHistorySection from '@/renderer/components/Profile/PresenceHistorySection';
import { deferred } from '../../../helpers/deferred';

const API_BASE = 'http://localhost:8080';
const HISTORY_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-history`;
const SETTINGS_ENDPOINT = `${HISTORY_ENDPOINT}/settings`;
const CAPABILITIES_ENDPOINT = `${API_BASE}/api/v1/server/capabilities`;
const USER_A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const USER_B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const ITEM_A = '11111111-1111-4111-8111-111111111111';
const ITEM_B = '22222222-2222-4222-8222-222222222222';
const ITEM_C = '33333333-3333-4333-8333-333333333333';
const STARTED_AT = '2026-07-12T14:00:00Z';
const ENDED_AT = '2026-07-12T14:45:00Z';
const EXPIRES_AT = '2026-08-11T14:00:00Z';

const CATEGORY_LABELS = {
  server_voice: 'Server Voice',
  private_call: 'Private Call',
  games: 'Games',
  music: 'Music',
  streaming: 'Streaming',
  browser: 'Browser',
  productivity: 'Productivity',
  creator: 'Creator',
  custom_text: 'Custom Status',
} as const;

type HistoryCategory = keyof typeof CATEGORY_LABELS;

function settings(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    available: true,
    enabled: false,
    reconsent_required: false,
    retention_days: 30,
    consent_version: null,
    consent_copy_hash: null,
    consented_at: null,
    required_consent: {
      version: 1,
      copy_hash: 'a'.repeat(64),
      operator_name: 'Concord Voice LLC',
      required_text: 'Persistent activity history is stored on Concord servers.',
      details: ['History starts with your next Custom Status change.'],
      privacy_policy_url: 'https://concordvoice.com/privacy-policy',
      acknowledgement_label:
        'I understand and consent to server-readable Activity History under the terms above.',
    },
    ...overrides,
  };
}

function enabledSettings(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return settings({
    enabled: true,
    consent_version: 1,
    consent_copy_hash: 'a'.repeat(64),
    consented_at: STARTED_AT,
    ...overrides,
  });
}

function supportedItem(
  text: string,
  overrides: Record<string, unknown> = {}
): Record<string, unknown> {
  return {
    status: 'supported',
    id: ITEM_A,
    category: 'custom_text',
    payload_version: 1,
    payload: { text, emoji: '🔍' },
    started_at: STARTED_AT,
    ended_at: ENDED_AT,
    recorded_at: STARTED_AT,
    expires_at: EXPIRES_AT,
    ...overrides,
  };
}

function unsupportedItem(
  category: HistoryCategory,
  index: number,
  overrides: Record<string, unknown> = {}
): Record<string, unknown> {
  const suffix = String(index + 1).padStart(12, '0');
  return {
    status: 'unsupported',
    id: `00000000-0000-4000-8000-${suffix}`,
    category,
    payload_version: 2,
    payload: null,
    started_at: STARTED_AT,
    ended_at: null,
    recorded_at: STARTED_AT,
    expires_at: EXPIRES_AT,
    ...overrides,
  };
}

function historyPage(
  items: Record<string, unknown>[] = [],
  nextCursor: string | null = null
): Record<string, unknown> {
  return { items, next_cursor: nextCursor };
}

function serveHistory(
  settingsBody: Record<string, unknown>,
  pageBody: Record<string, unknown>
): void {
  server.use(
    http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(settingsBody)),
    http.get(HISTORY_ENDPOINT, () => HttpResponse.json(pageBody))
  );
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  clientConfigService.stop();
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' });
});

afterEach(() => {
  clientConfigService.stop();
  server.resetHandlers();
  vi.restoreAllMocks();
  delete document.documentElement.dataset.appfont;
});

describe('PresenceHistorySection capability states', () => {
  it('keeps a labeled loading surface while capability discovery is pending', () => {
    useClientConfigStore.getState().setActivityHistoryCapability({ status: 'loading' });

    render(<PresenceHistorySection userId={USER_A} />);

    expect(screen.getByRole('heading', { name: 'Activity History' })).toBeInTheDocument();
    expect(screen.getByText(/checking activity history availability/i)).toBeInTheDocument();
  });

  it('renders a validated Off feed with activation paused when support is absent', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    serveHistory(settings(), historyPage());

    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Activity History is off')).toBeInTheDocument();
    expect(screen.getByText(/new opt-ins are paused/i)).toBeInTheDocument();
  });

  it('hides when an old server has no self history routes', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    server.use(
      http.get(SETTINGS_ENDPOINT, () => new HttpResponse(null, { status: 404 })),
      http.get(HISTORY_ENDPOINT, () => new HttpResponse(null, { status: 404 }))
    );

    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it('shows retry when a current gate-false history route fails transiently', async () => {
    const user = userEvent.setup();
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(enabledSettings())),
      http.get(HISTORY_ENDPOINT, () => new HttpResponse(null, { status: 500 }))
    );
    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Activity History could not be loaded.')).toBeInTheDocument();
    server.use(
      http.get(HISTORY_ENDPOINT, () =>
        HttpResponse.json(historyPage([supportedItem('Recovered after retry')]))
      )
    );
    await user.click(screen.getByRole('button', { name: 'Retry loading Activity History' }));

    expect(await screen.findByText('Recovered after retry')).toBeInTheDocument();
  });

  it('keeps an existing user history feed available during a gate-false drain', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    serveHistory(enabledSettings(), historyPage([supportedItem('Preserved during the drain')]));

    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Preserved during the drain')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Activity History' })).toBeInTheDocument();
    expect(screen.getByText(/new opt-ins are paused/i)).toBeInTheDocument();
  });

  it('preserves a loaded existing-user feed when a supported instance enters a drain', async () => {
    serveHistory(enabledSettings(), historyPage([supportedItem('Loaded before the drain')]));
    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Loaded before the drain')).toBeInTheDocument();
    act(() => {
      useClientConfigStore
        .getState()
        .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    });

    expect(screen.getByText('Loaded before the drain')).toBeInTheDocument();
    expect(screen.getByText(/new opt-ins are paused/i)).toBeInTheDocument();
  });

  it('hides a confirmed feed when a drain probe proves the routes are absent', async () => {
    let drain = false;
    server.use(
      http.get(SETTINGS_ENDPOINT, () =>
        drain ? new HttpResponse(null, { status: 404 }) : HttpResponse.json(enabledSettings())
      ),
      http.get(HISTORY_ENDPOINT, () =>
        drain
          ? new HttpResponse(null, { status: 404 })
          : HttpResponse.json(historyPage([supportedItem('Loaded before route removal')]))
      )
    );
    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Loaded before route removal')).toBeInTheDocument();
    drain = true;
    act(() => {
      useClientConfigStore
        .getState()
        .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    });

    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it('refetches a drain feed when capability support is activated', async () => {
    let historyRequests = 0;
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(enabledSettings())),
      http.get(HISTORY_ENDPOINT, () => {
        historyRequests += 1;
        const text = historyRequests === 1 ? 'Loaded during drain' : 'Loaded after activation';
        return HttpResponse.json(historyPage([supportedItem(text)]));
      })
    );
    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Loaded during drain')).toBeInTheDocument();
    act(() => {
      useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' });
    });

    expect(await screen.findByText('Loaded after activation')).toBeInTheDocument();
    expect(historyRequests).toBe(2);
  });

  it('shows capability errors as unknown and retries capability discovery', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'error', lastConfirmedSupported: false });
    serveHistory(settings(), historyPage());
    let capabilityRequests = 0;
    server.use(
      http.get(CAPABILITIES_ENDPOINT, () => {
        capabilityRequests += 1;
        return HttpResponse.json({
          auth: { oauthProviders: [] },
          features: { activityHistorySupported: true },
        });
      })
    );

    render(<PresenceHistorySection userId={USER_A} />);

    expect(screen.getByText(/availability could not be confirmed/i)).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(/availability could not be confirmed/i);
    fireEvent.click(screen.getByRole('button', { name: /retry availability check/i }));

    await waitFor(() => expect(capabilityRequests).toBe(1));
    await screen.findByText(/activity history is off/i);
    expect(useClientConfigStore.getState().activityHistoryCapability).toEqual({
      status: 'supported',
    });
  });

  it('loads on remount under stale confirmed support and names the availability warning', async () => {
    useClientConfigStore.getState().setActivityHistoryCapability({
      status: 'error',
      lastConfirmedSupported: true,
    });
    serveHistory(enabledSettings(), historyPage([supportedItem('Loaded from stale support')]));

    render(<PresenceHistorySection userId={USER_A} />);

    const notice = screen.getByRole('complementary', {
      name: 'Activity History availability',
    });
    expect(notice).toHaveAttribute('aria-live', 'polite');
    expect(notice).toHaveAttribute('aria-atomic', 'true');
    expect(within(notice).getByRole('button', { name: /retry availability check/i })).toBeVisible();
    expect(await screen.findByText('Loaded from stale support')).toBeInTheDocument();
  });
});

describe('PresenceHistorySection initial lifecycle and empty states', () => {
  it('starts settings and page one together and renders a labeled loading state', async () => {
    const settingsResponse = deferred<HttpResponse>();
    const pageResponse = deferred<HttpResponse>();
    let settingsRequests = 0;
    let pageRequests = 0;
    server.use(
      http.get(SETTINGS_ENDPOINT, () => {
        settingsRequests += 1;
        return settingsResponse.promise;
      }),
      http.get(HISTORY_ENDPOINT, () => {
        pageRequests += 1;
        return pageResponse.promise;
      })
    );

    render(<PresenceHistorySection userId={USER_A} />);

    expect(screen.getByText(/loading activity history/i)).toBeInTheDocument();
    await waitFor(() => {
      expect(settingsRequests).toBe(1);
      expect(pageRequests).toBe(1);
    });

    act(() => {
      settingsResponse.resolve(HttpResponse.json(enabledSettings()));
      pageResponse.resolve(HttpResponse.json(historyPage()));
    });
    await screen.findByText(/no activity recorded yet/i);
  });

  it.each([
    {
      name: 'disabled',
      settingsBody: settings(),
      expected: /activity history is off/i,
    },
    {
      name: 'paused for re-consent',
      settingsBody: settings({ reconsent_required: true }),
      expected: /recording is paused/i,
    },
    {
      name: 'enabled with no records',
      settingsBody: enabledSettings(),
      expected: /no activity recorded yet/i,
    },
  ])('renders the $name empty state only after both responses validate', async (testCase) => {
    serveHistory(testCase.settingsBody, historyPage());

    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText(testCase.expected)).toBeInTheDocument();
    expect(screen.getByText(/next custom status change/i)).toBeInTheDocument();
  });

  it('does not hide residual rows merely because recording is paused', async () => {
    serveHistory(
      settings({ reconsent_required: true }),
      historyPage([supportedItem('Visible while paused')])
    );

    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Visible while paused')).toBeInTheDocument();
    expect(screen.getByText(/recording is paused/i)).toBeInTheDocument();
  });

  it('does not start self-history transport without an authenticated profile ID', () => {
    let requests = 0;
    server.use(
      http.get(SETTINGS_ENDPOINT, () => {
        requests += 1;
        return HttpResponse.json(settings());
      }),
      http.get(HISTORY_ENDPOINT, () => {
        requests += 1;
        return HttpResponse.json(historyPage());
      })
    );

    render(<PresenceHistorySection userId={null} />);

    expect(requests).toBe(0);
    expect(screen.getByText(/profile is still loading/i)).toBeInTheDocument();
  });
});

describe('PresenceHistorySection safe timeline rendering', () => {
  it('inherits the application font sink without a component-level font override', () => {
    useClientConfigStore.getState().setActivityHistoryCapability({ status: 'loading' });
    document.documentElement.dataset.appfont = 'opendyslexic';
    const css = readFileSync(
      resolve(process.cwd(), 'src/renderer/components/Profile/PresenceHistorySection.css'),
      'utf8'
    );

    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    expect(document.documentElement).toHaveAttribute('data-appfont', 'opendyslexic');
    expect(container.querySelector('.presence-history')).toBeInTheDocument();
    expect(css).not.toMatch(/font-family\s*:/iu);
  });

  it('renders supported Custom Status text as escaped JSX with optional emoji and duration', async () => {
    const unsafeText = '<img src=x onerror="sentinel()">';
    serveHistory(enabledSettings(), historyPage([supportedItem(unsafeText)]));

    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText(unsafeText)).toBeInTheDocument();
    expect(screen.getByRole('img', { name: 'Status emoji: 🔍' })).toHaveTextContent('🔍');
    const timeline = screen.getByRole('list', { name: 'Activity History timeline' });
    expect(within(timeline).getAllByRole('listitem')).toHaveLength(1);
    expect(screen.getByText('45 minutes')).toBeInTheDocument();
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector(`time[datetime="${STARTED_AT}"]`)).toBeInTheDocument();
    expect(container.querySelector(`time[datetime="${ENDED_AT}"]`)).toBeInTheDocument();
    const output = container.querySelector('output[aria-live="polite"]');
    expect(output).toHaveTextContent(/loaded 1 activity/i);
  });

  it('renders a supported status when the optional emoji is absent', async () => {
    serveHistory(
      enabledSettings(),
      historyPage([
        supportedItem('No emoji', {
          payload: { text: 'No emoji' },
        }),
      ])
    );

    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('No emoji')).toBeInTheDocument();
    expect(container.querySelector('.presence-history__emoji')).toBeNull();
  });

  it('formats singular, plural, partial-hour, and multi-day durations', async () => {
    serveHistory(
      enabledSettings(),
      historyPage([
        supportedItem('One hour', {
          id: '00000000-0000-4000-8000-000000000101',
          ended_at: '2026-07-12T15:00:00Z',
        }),
        supportedItem('Partial hours', {
          id: '00000000-0000-4000-8000-000000000102',
          ended_at: '2026-07-12T16:30:00Z',
        }),
        supportedItem('One day', {
          id: '00000000-0000-4000-8000-000000000103',
          ended_at: '2026-07-13T14:00:00Z',
        }),
        supportedItem('Partial days', {
          id: '00000000-0000-4000-8000-000000000104',
          ended_at: '2026-07-13T16:00:00Z',
        }),
        supportedItem('Two days', {
          id: '00000000-0000-4000-8000-000000000105',
          ended_at: '2026-07-14T14:00:00Z',
        }),
      ])
    );

    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('One hour')).toBeInTheDocument();
    expect(screen.getByText('1 hour')).toBeInTheDocument();
    expect(screen.getByText('2 hours 30 minutes')).toBeInTheDocument();
    expect(screen.getByText('1 day')).toBeInTheDocument();
    expect(screen.getByText('1 day 2 hours')).toBeInTheDocument();
    expect(screen.getByText('2 days')).toBeInTheDocument();
  });

  it('labels an open interval Ongoing without inventing an end time or duration', async () => {
    serveHistory(
      enabledSettings(),
      historyPage([
        supportedItem('Still active', {
          ended_at: null,
        }),
      ])
    );

    render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText('Still active')).toBeInTheDocument();
    expect(screen.getByText('Ongoing')).toBeInTheDocument();
    expect(screen.queryByText('45 minutes')).not.toBeInTheDocument();
  });

  it('maps all nine categories to accessible labels and withholds unsupported payloads', async () => {
    const items = (Object.keys(CATEGORY_LABELS) as HistoryCategory[]).map((category, index) =>
      unsupportedItem(category, index)
    );
    serveHistory(enabledSettings(), historyPage(items));

    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    await screen.findByText(CATEGORY_LABELS.server_voice);
    for (const label of Object.values(CATEGORY_LABELS)) {
      expect(screen.getByRole('heading', { name: label })).toBeInTheDocument();
    }
    expect(screen.getAllByText(/details are unavailable in this version/i)).toHaveLength(9);
    expect(container.querySelectorAll('[data-category-icon]')).toHaveLength(9);
    expect(container).not.toHaveTextContent('sentinel-private-payload');
  });
});

describe('PresenceHistorySection errors and pagination', () => {
  it('renders an explicit initial error, retries both requests, and announces recovery', async () => {
    let settingsRequests = 0;
    server.use(
      http.get(SETTINGS_ENDPOINT, () => {
        settingsRequests += 1;
        if (settingsRequests === 1) {
          return HttpResponse.json({ code: 'activity_history_internal_error' }, { status: 500 });
        }
        return HttpResponse.json(enabledSettings());
      }),
      http.get(HISTORY_ENDPOINT, () => HttpResponse.json(historyPage([supportedItem('Recovered')])))
    );

    const { container } = render(<PresenceHistorySection userId={USER_A} />);

    expect(await screen.findByText(/activity history could not be loaded/i)).toBeInTheDocument();
    expect(screen.queryByText('Recovered')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /retry loading activity history/i }));

    expect(await screen.findByText('Recovered')).toBeInTheDocument();
    expect(settingsRequests).toBe(2);
    expect(container.querySelector('output')).toHaveTextContent(/loaded 1 activity after retry/i);
  });

  it('loads an opaque next page, focuses its first card, announces it, and removes the terminal button', async () => {
    const opaqueCursor = 'opaque.cursor/value?keep=yes&still=opaque';
    let requestedCursor: string | null = null;
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(enabledSettings())),
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        const before = new URL(request.url).searchParams.get('before');
        requestedCursor = before;
        if (before === null) {
          return HttpResponse.json(
            historyPage([supportedItem('First page', { id: ITEM_A })], opaqueCursor)
          );
        }
        return HttpResponse.json(historyPage([supportedItem('Second page', { id: ITEM_B })]));
      })
    );

    const { container } = render(<PresenceHistorySection userId={USER_A} />);
    await screen.findByText('First page');

    fireEvent.click(screen.getByRole('button', { name: 'Load more' }));

    expect(await screen.findByText('Second page')).toBeInTheDocument();
    expect(requestedCursor).toBe(opaqueCursor);
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toHaveTextContent('Second page'));
    expect(container.querySelector('output')).toHaveTextContent(
      /loaded 1 more activity.*all history is shown/i
    );
  });

  it('preserves prior rows after a load-more failure and retries from the same cursor', async () => {
    const user = userEvent.setup();
    let nextPageAttempts = 0;
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(enabledSettings())),
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        const before = new URL(request.url).searchParams.get('before');
        if (before === null) {
          return HttpResponse.json(historyPage([supportedItem('Keep me')], 'cursor-two'));
        }
        nextPageAttempts += 1;
        if (nextPageAttempts === 1) {
          return HttpResponse.json({ code: 'activity_history_internal_error' }, { status: 500 });
        }
        return HttpResponse.json(historyPage([supportedItem('Appended', { id: ITEM_C })]));
      })
    );

    render(<PresenceHistorySection userId={USER_A} />);
    await screen.findByText('Keep me');
    await user.click(screen.getByRole('button', { name: 'Load more' }));

    expect(await screen.findByText(/could not load more activity history/i)).toBeInTheDocument();
    expect(screen.getByText('Keep me')).toBeInTheDocument();
    const retry = screen.getByRole('button', { name: /retry load more/i });
    expect(retry).toHaveFocus();
    await user.click(retry);

    expect(await screen.findByText('Appended')).toBeInTheDocument();
    expect(screen.getByText('Keep me')).toBeInTheDocument();
    expect(nextPageAttempts).toBe(2);
  });

  it('returns focus to the last visible item when an empty terminal page removes Load more', async () => {
    const user = userEvent.setup();
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(enabledSettings())),
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        const before = new URL(request.url).searchParams.get('before');
        if (before === null) {
          return HttpResponse.json(historyPage([supportedItem('Only page')], 'terminal-cursor'));
        }
        return HttpResponse.json(historyPage());
      })
    );

    render(<PresenceHistorySection userId={USER_A} />);
    await screen.findByText('Only page');
    await user.click(screen.getByRole('button', { name: 'Load more' }));

    await waitFor(() => expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull());
    expect(screen.getByRole('article', { name: 'Custom Status' })).toHaveFocus();
  });
});

describe('PresenceHistorySection account-scoped request lifetime', () => {
  it('retains rows and in-flight pagination when supported capability becomes stale', async () => {
    const nextPageResponse = deferred<HttpResponse>();
    let loadMoreSignal: AbortSignal | null = null;
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(enabledSettings())),
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        const before = new URL(request.url).searchParams.get('before');
        if (before === null) {
          return HttpResponse.json(historyPage([supportedItem('Still visible')], 'next-page'));
        }
        loadMoreSignal = request.signal;
        return nextPageResponse.promise;
      })
    );

    render(<PresenceHistorySection userId={USER_A} />);
    await screen.findByText('Still visible');
    fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(loadMoreSignal).not.toBeNull());

    act(() => {
      useClientConfigStore.getState().setActivityHistoryCapability({
        status: 'error',
        lastConfirmedSupported: true,
      });
    });

    expect(screen.getByText('Still visible')).toBeInTheDocument();
    expect(
      screen.getByRole('complementary', { name: 'Activity History availability' })
    ).toBeInTheDocument();
    expect(loadMoreSignal?.aborted).toBe(false);

    act(() => {
      nextPageResponse.resolve(
        HttpResponse.json(historyPage([supportedItem('Still completes', { id: ITEM_B })]))
      );
    });
    expect(await screen.findByText('Still completes')).toBeInTheDocument();
  });

  it('aborts account-A initial requests and ignores their late completion after account B loads', async () => {
    const aSettings = deferred<HttpResponse>();
    const aPage = deferred<HttpResponse>();
    const aSignals: AbortSignal[] = [];
    let settingsCalls = 0;
    let pageCalls = 0;
    server.use(
      http.get(SETTINGS_ENDPOINT, ({ request }) => {
        settingsCalls += 1;
        if (settingsCalls === 1) {
          aSignals.push(request.signal);
          return aSettings.promise;
        }
        return HttpResponse.json(enabledSettings());
      }),
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        pageCalls += 1;
        if (pageCalls === 1) {
          aSignals.push(request.signal);
          return aPage.promise;
        }
        return HttpResponse.json(historyPage([supportedItem('Account B')]));
      })
    );

    const { rerender } = render(<PresenceHistorySection userId={USER_A} />);
    await waitFor(() => expect(aSignals).toHaveLength(2));

    rerender(<PresenceHistorySection userId={USER_B} />);

    expect(await screen.findByText('Account B')).toBeInTheDocument();
    await waitFor(() => expect(aSignals.every((signal) => signal.aborted)).toBe(true));
    act(() => {
      aSettings.resolve(HttpResponse.json(enabledSettings()));
      aPage.resolve(HttpResponse.json(historyPage([supportedItem('Stale Account A')])));
    });
    await act(async () => Promise.resolve());
    expect(screen.queryByText('Stale Account A')).not.toBeInTheDocument();
  });

  it('clears rendered rows synchronously and aborts load-more/unmount work on identity change', async () => {
    const loadMoreResponse = deferred<HttpResponse>();
    const bSettingsResponse = deferred<HttpResponse>();
    const bPageResponse = deferred<HttpResponse>();
    let loadMoreSignal: AbortSignal | null = null;
    const bSignals: AbortSignal[] = [];
    let settingsCalls = 0;
    server.use(
      http.get(SETTINGS_ENDPOINT, ({ request }) => {
        settingsCalls += 1;
        if (settingsCalls === 1) return HttpResponse.json(enabledSettings());
        bSignals.push(request.signal);
        return bSettingsResponse.promise;
      }),
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        const before = new URL(request.url).searchParams.get('before');
        if (before === 'more-a') {
          loadMoreSignal = request.signal;
          return loadMoreResponse.promise;
        }
        if (settingsCalls <= 1) {
          return HttpResponse.json(historyPage([supportedItem('Account A row')], 'more-a'));
        }
        bSignals.push(request.signal);
        return bPageResponse.promise;
      })
    );

    const { rerender, unmount } = render(<PresenceHistorySection userId={USER_A} />);
    await screen.findByText('Account A row');
    fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(loadMoreSignal).not.toBeNull());

    rerender(<PresenceHistorySection userId={USER_B} />);

    expect(screen.queryByText('Account A row')).not.toBeInTheDocument();
    expect(screen.getByText(/loading activity history/i)).toBeInTheDocument();
    await waitFor(() => expect(loadMoreSignal?.aborted).toBe(true));
    await waitFor(() => expect(bSignals).toHaveLength(2));

    unmount();
    expect(bSignals.every((signal) => signal.aborted)).toBe(true);
  });
});
