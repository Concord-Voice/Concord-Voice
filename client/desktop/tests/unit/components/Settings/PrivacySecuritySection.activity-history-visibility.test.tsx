import { act, render, screen, waitFor } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PrivacySecuritySection from '@/renderer/components/Settings/PrivacySecuritySection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';

vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  default: () => null,
  WebAuthnCredential: {},
}));

const API_BASE = 'http://localhost:8080';
const HISTORY_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-history`;
const SETTINGS_ENDPOINT = `${HISTORY_ENDPOINT}/settings`;
const USER_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';

const settings = {
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
    http.get(`${API_BASE}/api/v1/users/me/privacy`, () => HttpResponse.json({ privacy: {} })),
    http.get(`${API_BASE}/api/v1/users/me/sso-identities`, () =>
      HttpResponse.json({ identities: [] })
    ),
    http.get(`${API_BASE}/api/v1/users/me/presence-settings`, () =>
      HttpResponse.json({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' })
    )
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

describe('PrivacySecuritySection Activity History visibility', () => {
  it('hides the empty container after both real children confirm unsupported routes are absent', async () => {
    let settingsRequests = 0;
    let historyRequests = 0;
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    server.use(
      http.get(SETTINGS_ENDPOINT, () => {
        settingsRequests += 1;
        return new HttpResponse(null, { status: 404 });
      }),
      http.get(HISTORY_ENDPOINT, () => {
        historyRequests += 1;
        return new HttpResponse(null, { status: 404 });
      })
    );

    render(<PrivacySecuritySection />);

    const section = document.getElementById('section-presence-history');
    expect(section).toBeInstanceOf(HTMLDetailsElement);
    await waitFor(() => expect(settingsRequests).toBe(2));
    expect(historyRequests).toBe(1);
    await waitFor(() =>
      expect(section?.querySelector('.settings-collapsible-body')).toBeEmptyDOMElement()
    );
    expect(section?.closest('[hidden]')).not.toBeNull();
  });

  it('keeps the container visible when confirmed-unsupported routes expose drain content', async () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    server.use(
      http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(settings)),
      http.get(HISTORY_ENDPOINT, () => HttpResponse.json({ items: [], next_cursor: null }))
    );

    render(<PrivacySecuritySection />);

    expect(await screen.findByText('Activity History is off')).toBeInTheDocument();
    expect(document.getElementById('section-presence-history')?.closest('[hidden]')).toBeNull();
  });

  it('keeps stale-supported availability visible without waiting for route probes', () => {
    act(() => {
      useClientConfigStore.getState().setActivityHistoryCapability({
        status: 'error',
        lastConfirmedSupported: true,
      });
    });
    server.use(
      http.get(SETTINGS_ENDPOINT, () => new HttpResponse(null, { status: 404 })),
      http.get(HISTORY_ENDPOINT, () => new HttpResponse(null, { status: 404 }))
    );

    render(<PrivacySecuritySection />);

    expect(document.getElementById('section-presence-history')?.closest('[hidden]')).toBeNull();
  });
});
