import { render, screen, waitFor, userEvent } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PrivacySecuritySection from '@/renderer/components/Settings/PrivacySecuritySection';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';

vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  default: () => null,
  WebAuthnCredential: {},
}));

const API_BASE = 'http://localhost:8080';
const PRIVACY_ENDPOINT = `${API_BASE}/api/v1/users/me/privacy`;
const HISTORY_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-history`;
const USER_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';

const TOGGLE_LABEL = 'Require authentication before purging';

/** Every field an old control-plane knows about, minus `require_auth_before_purge`. */
const legacyPrivacy = {
  messages_friends_only: true,
  messages_server_members: true,
  dm_privacy_level: 2,
  dm_friends_of_friends: false,
  auto_accept_friend_codes: false,
  searchable_by_username: false,
  searchable_by_email: false,
  searchable_by_phone: false,
  allow_embedded_content: false,
  load_gifs_automatically: true,
  share_personalization_with_gif_provider: true,
};

function serveParentRequests(): void {
  server.use(
    http.get(`${API_BASE}/api/v1/sessions`, () =>
      HttpResponse.json({ sessions: [], past_sessions: [], revocation_mode: 'secure' })
    ),
    http.get(`${API_BASE}/api/v1/mfa/status`, () =>
      HttpResponse.json({
        methods: [],
        recovery_only_methods: [],
        recovery_hardened: false,
        backup_codes_remaining: 0,
        backup_email: '',
      })
    ),
    http.get(`${API_BASE}/api/v1/mfa/webauthn/credentials`, () =>
      HttpResponse.json({ credentials: [] })
    ),
    http.get(`${API_BASE}/api/v1/users/me/security`, () =>
      HttpResponse.json({ password_login_disabled: false, trust_sso_security: false })
    ),
    http.get(`${API_BASE}/api/v1/users/me/sso-identities`, () =>
      HttpResponse.json({ identities: [] })
    ),
    http.get(`${API_BASE}/api/v1/users/me/presence-settings`, () =>
      HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
    ),
    http.get(`${HISTORY_ENDPOINT}/settings`, () => new HttpResponse(null, { status: 404 })),
    http.get(HISTORY_ENDPOINT, () => new HttpResponse(null, { status: 404 })),
    http.get(PRIVACY_ENDPOINT, () => HttpResponse.json({ privacy: legacyPrivacy }))
  );
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  serveParentRequests();
  useAuthStore.setState({ accessToken: 'mock-token', sessionId: 'session-a' });
  useUserStore.setState({
    user: { id: USER_ID, username: 'pilot' },
    isLoading: false,
    error: null,
  });
});

afterEach(() => server.resetHandlers());

describe('PrivacySecuritySection — require authentication before purging (#1354)', () => {
  it('renders the toggle with its helper copy inside the Privacy section', async () => {
    render(<PrivacySecuritySection />);

    const toggle = await screen.findByRole('switch', { name: TOGGLE_LABEL });
    expect(toggle).toBeInTheDocument();
    expect(screen.getByText(TOGGLE_LABEL)).toBeInTheDocument();
    expect(
      screen.getByText(
        'Ask for your password before purging messages in a direct message or group chat.'
      )
    ).toBeInTheDocument();
    expect(document.getElementById('section-privacy-settings')).toContainElement(toggle);
  });

  it('exposes the id Task 4 navigates to', async () => {
    render(<PrivacySecuritySection />);

    const toggle = await screen.findByRole('switch', { name: TOGGLE_LABEL });
    expect(toggle).toHaveAttribute('id', 'requireAuthBeforePurge');
  });

  it('shows the toggle on when the server omits the field (old-server skew)', async () => {
    render(<PrivacySecuritySection />);

    await waitFor(() => expect(screen.getByRole('switch', { name: TOGGLE_LABEL })).toBeChecked());
    expect(screen.queryByText(/anyone with access to your unlocked account/i)).toBeNull();
  });

  it('warns when the toggle is switched off', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        capturedBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({
          privacy: { ...legacyPrivacy, require_auth_before_purge: false },
        });
      })
    );

    render(<PrivacySecuritySection />);
    const toggle = await screen.findByRole('switch', { name: TOGGLE_LABEL });
    await userEvent.click(toggle);

    await waitFor(() =>
      expect(
        screen.getByText(
          'Without this, anyone with access to your unlocked account can permanently purge your message history.'
        )
      ).toBeInTheDocument()
    );
    expect(capturedBody).toEqual({ require_auth_before_purge: false });
    expect(screen.getByRole('switch', { name: TOGGLE_LABEL })).not.toBeChecked();
  });

  it('surfaces the old-server rejection instead of failing silently', async () => {
    server.use(
      http.patch(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ error: 'No fields to update' }, { status: 400 })
      )
    );

    render(<PrivacySecuritySection />);
    const toggle = await screen.findByRole('switch', { name: TOGGLE_LABEL });
    await userEvent.click(toggle);

    await waitFor(() =>
      expect(
        screen.getByText("This version of the server doesn't support this setting yet.")
      ).toBeInTheDocument()
    );
    // The setting did not take, so the control stays on and no OFF warning appears.
    expect(screen.getByRole('switch', { name: TOGGLE_LABEL })).toBeChecked();
    expect(screen.queryByText(/anyone with access to your unlocked account/i)).toBeNull();
  });
});
