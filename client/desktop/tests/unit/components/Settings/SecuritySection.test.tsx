import { render, screen, userEvent, within, waitFor } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import SecuritySection from '@/renderer/components/Settings/SecuritySection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';

const API_BASE = 'http://localhost:8080/api/v1';
const USER_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const completedReads = new Set<string>();
server.events.on('response:mocked', ({ request }) =>
  completedReads.add(new URL(request.url).pathname)
);

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  completedReads.clear();
  useAuthStore.setState({ accessToken: 'mock-token', sessionId: 'session-a' });
  useUserStore.setState({
    user: { id: USER_ID, username: 'pilot' },
    isLoading: false,
    error: null,
  });
  useClientConfigStore.getState().setActivityHistoryCapability({ status: 'confirmed-unsupported' });
  server.use(
    http.get(`${API_BASE}/mfa/recovery-key`, () => HttpResponse.json({ enabled: false })),
    http.get(`${API_BASE}/mfa/trusted-devices`, () => HttpResponse.json({ devices: [] })),
    http.get(`${API_BASE}/mfa/recovery-circle`, () => HttpResponse.json({ members: [] })),
    http.get(`${API_BASE}/mfa/recovery-requests`, () => HttpResponse.json({ requests: [] })),
    http.get(`${API_BASE}/friends`, () => HttpResponse.json({ friends: [] })),
    http.get(`${API_BASE}/sessions`, () =>
      HttpResponse.json({ sessions: [], past_sessions: [], revocation_mode: 'secure' })
    ),
    http.get(`${API_BASE}/mfa/status`, () =>
      HttpResponse.json({
        methods: [],
        recovery_only_methods: [],
        recovery_hardened: false,
        backup_codes_remaining: 0,
        backup_email: '',
      })
    ),
    http.get(`${API_BASE}/mfa/webauthn/credentials`, () => HttpResponse.json({ credentials: [] })),
    http.get(`${API_BASE}/users/me/security`, () =>
      HttpResponse.json({ password_login_disabled: false, trust_sso_security: false })
    ),
    http.get(`${API_BASE}/users/me/privacy`, () => HttpResponse.json({ privacy: {} })),
    http.get(`${API_BASE}/users/me/sso-identities`, () => HttpResponse.json({ identities: [] })),
    http.get(`${API_BASE}/users/me/presence-settings`, () =>
      HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
    ),
    http.get(
      `${API_BASE}/users/me/presence-history/settings`,
      () => new HttpResponse(null, { status: 404 })
    ),
    http.get(`${API_BASE}/users/me/presence-history`, () => new HttpResponse(null, { status: 404 }))
  );
});

async function open(id: string, title: string): Promise<HTMLElement> {
  await waitFor(() => {
    expect(completedReads.has('/api/v1/sessions')).toBe(true);
    expect(completedReads.has('/api/v1/mfa/status')).toBe(true);
    expect(completedReads.has('/api/v1/users/me/security')).toBe(true);
  });
  const details = document.getElementById(id);
  expect(details).toBeInstanceOf(HTMLDetailsElement);
  await userEvent.setup().click(within(details as HTMLElement).getByText(title));
  return details as HTMLElement;
}

describe('security settings server-confirmed status', () => {
  it('places credentials, MFA, SSO and session destinations together without privacy or OS permissions', async () => {
    render(<SecuritySection />);
    await open('section-mfa', 'Multi-Factor Authentication');
    for (const id of [
      'section-password',
      'section-mfa',
      'section-sso-security',
      'section-active-sessions',
      'section-past-sessions',
    ]) {
      expect(document.getElementById(id)).toBeInstanceOf(HTMLDetailsElement);
    }
    expect(document.getElementById('section-privacy-settings')).toBeNull();
    expect(document.getElementById('section-presence-settings')).toBeNull();
    expect(document.getElementById('section-system-permissions')).toBeNull();
  });

  it('keeps Past Sessions loading distinct from verified empty history', async () => {
    let finishRead!: () => void;
    const pending = new Promise<void>((resolve) => {
      finishRead = resolve;
    });
    server.use(
      http.get(`${API_BASE}/sessions`, async () => {
        await pending;
        return HttpResponse.json({ sessions: [], past_sessions: [], revocation_mode: 'secure' });
      })
    );
    render(<SecuritySection />);
    const past = document.getElementById('section-past-sessions') as HTMLDetailsElement;
    await userEvent.setup().click(within(past).getByText('Past Sessions'));
    expect(within(past).getByRole('status')).toHaveTextContent(/loading past sessions/i);
    expect(within(past).queryByText(/no past sessions/i)).not.toBeInTheDocument();
    finishRead();
    expect(await within(past).findByText(/no past sessions/i)).toBeInTheDocument();
  });

  it('retains a real Past Sessions destination for confirmed empty history', async () => {
    render(<SecuritySection />);
    const past = await open('section-past-sessions', 'Past Sessions');
    expect(await within(past).findByText(/no past sessions/i)).toBeInTheDocument();
  });

  it('shows a Past Sessions error instead of verified empty history when the read fails', async () => {
    server.use(http.get(`${API_BASE}/sessions`, () => new HttpResponse(null, { status: 503 })));
    render(<SecuritySection />);
    const past = await open('section-past-sessions', 'Past Sessions');
    expect(await within(past).findByText(/failed|couldn't|could not|unable/i)).toBeInTheDocument();
    expect(within(past).queryByText(/no past sessions/i)).not.toBeInTheDocument();
  });

  it('reloads Past Sessions after a failed read and shows the returned revoked device', async () => {
    server.use(http.get(`${API_BASE}/sessions`, () => new HttpResponse(null, { status: 503 })));
    render(<SecuritySection />);
    const past = await open('section-past-sessions', 'Past Sessions');
    expect(await within(past).findByRole('alert')).toHaveTextContent(
      /couldn't load your past sessions/i
    );
    server.use(
      http.get(`${API_BASE}/sessions`, () =>
        HttpResponse.json({
          sessions: [],
          past_sessions: [
            {
              id: 'past-1',
              device_name: 'Old laptop',
              ip_address: '192.0.2.5',
              user_agent: 'Firefox/110',
              created_at: '2026-10-01T00:00:00Z',
              last_used: '2026-10-02T00:00:00Z',
              revoked_at: '2026-10-03T00:00:00Z',
            },
          ],
          revocation_mode: 'secure',
        })
      )
    );
    await userEvent
      .setup()
      .click(within(past).getByRole('button', { name: 'Reload past sessions' }));
    expect(await within(past).findByText('192.0.2.5')).toBeInTheDocument();
    expect(within(past).getByText('Revoked')).toBeInTheDocument();
    expect(within(past).queryByRole('alert')).not.toBeInTheDocument();
    expect(within(past).queryByText(/no past sessions/i)).not.toBeInTheDocument();
  });

  it('hides stale remaining sessions and revocation controls when the post-revoke refresh fails', async () => {
    let readFailed = false;
    const liveSession = (id: string, ip: string) => ({
      id,
      device_name: 'Laptop',
      ip_address: ip,
      user_agent: 'Firefox/110',
      machine_id: id,
      expires_at: '2027-01-01T00:00:00Z',
      created_at: '2026-10-01T00:00:00Z',
      last_used: '2026-10-02T00:00:00Z',
      is_current: false,
    });
    server.use(
      http.get(`${API_BASE}/sessions`, () =>
        readFailed
          ? HttpResponse.json({ error: 'Failed to fetch sessions' }, { status: 503 })
          : HttpResponse.json({
              sessions: [
                liveSession('session-two', '192.0.2.2'),
                liveSession('session-three', '192.0.2.3'),
              ],
              past_sessions: [],
              revocation_mode: 'secure',
            })
      ),
      http.delete(`${API_BASE}/sessions/session-two`, () => {
        readFailed = true;
        return HttpResponse.json({});
      })
    );
    render(<SecuritySection />);
    const active = await open('section-active-sessions', 'Active Sessions');
    const removedCard = (await within(active).findByText('192.0.2.2')).closest(
      '.session-card'
    ) as HTMLElement;
    expect(within(active).getByText('192.0.2.3')).toBeInTheDocument();
    expect(within(active).getByRole('button', { name: 'Revoke All Sessions' })).toBeInTheDocument();
    await userEvent.setup().click(within(removedCard).getByRole('button', { name: 'Revoke' }));
    expect(await within(active).findByRole('alert')).toHaveTextContent(/Failed to fetch sessions/i);
    expect(within(active).getByRole('button', { name: 'Reload sessions' })).toBeInTheDocument();
    expect(within(active).queryByText('192.0.2.3')).not.toBeInTheDocument();
    expect(
      within(active).queryByRole('button', { name: 'Revoke All Sessions' })
    ).not.toBeInTheDocument();
    expect(within(active).queryByRole('button', { name: 'Secure' })).not.toBeInTheDocument();
  });

  it('does not show empty factor/key confirmations after MFA status fails', async () => {
    server.use(http.get(`${API_BASE}/mfa/status`, () => new HttpResponse(null, { status: 503 })));
    render(<SecuritySection />);
    const mfa = await open('section-mfa', 'Multi-Factor Authentication');
    expect(await within(mfa).findByText(/couldn't load your MFA settings/i)).toBeInTheDocument();
    expect(within(mfa).queryByText('Requires MFA')).not.toBeInTheDocument();
    expect(within(mfa).queryByText(/Security Keys:/i)).not.toBeInTheDocument();
  });

  it('does not treat a failed credentials read as confirmed zero security keys', async () => {
    server.use(
      http.get(
        `${API_BASE}/mfa/webauthn/credentials`,
        () => new HttpResponse(null, { status: 503 })
      )
    );
    render(<SecuritySection />);
    const mfa = await open('section-mfa', 'Multi-Factor Authentication');
    expect(await within(mfa).findByText(/couldn't load your MFA settings/i)).toBeInTheDocument();
    expect(within(mfa).queryByText(/Security Keys:/i)).not.toBeInTheDocument();
  });

  it('does not assert SSO security toggle state after a failed read and offers reload', async () => {
    server.use(
      http.get(`${API_BASE}/users/me/security`, () => new HttpResponse(null, { status: 503 }))
    );
    render(<SecuritySection />);
    const sso = await open('section-sso-security', 'SSO Security');
    expect(await within(sso).findByRole('button', { name: /reload.*SSO/i })).toBeInTheDocument();
    expect(
      within(sso).queryByRole('switch', { name: 'Trust SSO provider verification' })
    ).not.toBeInTheDocument();
    expect(within(sso).queryByText(/Password login remains available/i)).not.toBeInTheDocument();
  });
});
